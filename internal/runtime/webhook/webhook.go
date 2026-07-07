// Package webhook is the inbound HTTP receiver for trigger payloads. The
// chi handler at POST /webhook/{tokenID} resolves the token to a trigger,
// verifies HMAC against the vault-stored shared secret, deduplicates by
// (provider, delivery_id), records the delivery, and dispatches a run via
// the registered Dispatcher.
//
// HMAC verification is provider-aware:
//
//	stripe   - Stripe-Signature header: t=<unix>,v1=<hex>; signed payload
//	           is "<t>.<body>"; ts skew window 5 minutes.
//	github   - X-Hub-Signature-256 header: "sha256=<hex>"; signed payload
//	           is the raw body.
//	generic  - X-Webhook-Signature header: "sha256=<hex>"; signed payload
//	           is the raw body. The dedup ID comes from X-Webhook-Delivery
//	           or, when absent, a SHA-256 fingerprint of the body.
//
// All HMAC compares use crypto/subtle.ConstantTimeCompare; bad sigs return
// 401. Replays inside the dedup window return 200 with no run dispatched.
// Anything else that fails returns 500 (with a generic body) and the
// trigger gets last_error stamped for dashboard surfacing.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"encoding/json"

	"github.com/bright-interaction/reactor/internal/dispatcher"
	"github.com/bright-interaction/reactor/internal/runtime/journal"
	"github.com/bright-interaction/reactor/internal/vault"
)

// MaxBodyBytes caps the inbound body so a misconfigured upstream cannot
// exhaust host memory. Larger payloads should ride object storage with a
// reference; webhooks themselves stay small.
const MaxBodyBytes = 1 << 20 // 1 MiB

// stripeSkew is the maximum acceptable clock drift between the upstream
// and the host for Stripe signature timestamps. Stripe's own docs use 5
// minutes as the recommended ceiling.
const stripeSkew = 5 * time.Minute

// VaultReader is the slice of the vault interface this package needs.
// Defined locally to keep imports tight.
type VaultReader interface {
	Get(ctx context.Context, id string) (*vault.Secret, error)
}

// syncConfig is the subset of a webhook trigger's config JSON that controls
// synchronous (request/response) behaviour.
type syncConfig struct {
	Sync           bool `json:"sync"`
	TimeoutSeconds int  `json:"timeout_seconds"`
}

// parseSyncConfig reads the sync flag + timeout from a trigger's config. A sync
// trigger with no timeout defaults to 30s; the timeout is clamped to 120s so a
// hung run can't pin an HTTP connection indefinitely.
func parseSyncConfig(cfg []byte) syncConfig {
	var sc syncConfig
	if len(cfg) > 0 {
		_ = json.Unmarshal(cfg, &sc)
	}
	if sc.Sync {
		if sc.TimeoutSeconds <= 0 {
			sc.TimeoutSeconds = 30
		}
		if sc.TimeoutSeconds > 120 {
			sc.TimeoutSeconds = 120
		}
	}
	return sc
}

// Dispatcher is the contract that fan-outs a verified webhook payload into
// a workflow run. The supervisor lives behind this interface so the webhook
// package can be tested with a fake.
type Dispatcher interface {
	Dispatch(ctx context.Context, t journal.Trigger, payload []byte) error
	// DispatchSync starts the run and waits for its result (for synchronous
	// webhook triggers that return the workflow's output to the caller).
	DispatchSync(ctx context.Context, t journal.Trigger, payload []byte, timeout time.Duration) (runID, status string, output json.RawMessage, err error)
}

// Receiver wires the components.
type Receiver struct {
	Journal *journal.Journal
	Vault   VaultReader
	Disp    Dispatcher
	Log     *slog.Logger

	// Now overrides the clock for deterministic tests.
	Now func() time.Time
}

// Mount registers the receiver's routes:
//
//	POST /webhook/{token_id}    HMAC-verified trigger payload
//	POST /signal/{token}        AwaitSignal external delivery
func (r *Receiver) Mount(router chi.Router) {
	router.Post("/webhook/{token_id}", r.handle)
	router.Post("/signal/{token}", r.handleSignal)
}

// Handler returns the bare chi handler for callers that compose their own
// router (e.g. tests).
func (r *Receiver) Handler() http.HandlerFunc { return r.handle }

// SignalHandler returns the bare signal-delivery handler for tests that
// compose their own router.
func (r *Receiver) SignalHandler() http.HandlerFunc { return r.handleSignal }

func (r *Receiver) handle(w http.ResponseWriter, req *http.Request) {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.Now == nil {
		r.Now = time.Now
	}

	tokenID := chi.URLParam(req, "token_id")
	if tokenID == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, MaxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			r.Log.Warn("webhook: body too large", "limit", maxErr.Limit)
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Log.Warn("webhook: body read", "op", "webhook", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx := req.Context()
	trig, err := r.Journal.FindWebhookByToken(ctx, tokenID)
	if err != nil {
		// Don't leak whether the token was bad vs. inactive; both 404.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	provider := trig.Provider
	if provider == "" {
		provider = "generic"
	}

	if err := r.verifyHMAC(ctx, trig, provider, req.Header, body); err != nil {
		r.Log.Warn("webhook: hmac verify failed", "trigger_id", trig.ID, "provider", provider, "err", err)
		_ = r.Journal.MarkTriggerError(ctx, trig.ID, "hmac: "+err.Error())
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	deliveryID := pickDeliveryID(provider, req.Header, body)
	fresh, err := r.Journal.RecordWebhookDelivery(ctx, provider, deliveryID)
	if err != nil {
		r.Log.Error("webhook: dedup record", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !fresh {
		// Replay: idempotent 200, no dispatch. Stripe + GitHub explicitly
		// retry on non-2xx; replying 200 lets them stop quickly.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"deduped":true}`))
		return
	}

	// Synchronous trigger: run the workflow and return its result to the
	// caller (request/response API), instead of fire-and-forget.
	if sc := parseSyncConfig(trig.Config); sc.Sync {
		runID, status, output, derr := r.Disp.DispatchSync(ctx, trig, body, time.Duration(sc.TimeoutSeconds)*time.Second)
		if derr != nil {
			if errors.Is(derr, dispatcher.ErrRateLimited) || errors.Is(derr, dispatcher.ErrCapacity) {
				_ = r.Journal.DeleteWebhookDelivery(ctx, provider, deliveryID) // let the sender retry
				http.Error(w, "rate limited; retry later", http.StatusTooManyRequests)
				return
			}
			if errors.Is(derr, dispatcher.ErrSyncTimeout) {
				// The run keeps executing; hand back 202 + the run id to poll.
				_ = r.Journal.MarkTriggerFired(ctx, trig.ID)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true, "run_id": runID, "timed_out": true})
				return
			}
			r.Log.Error("webhook: sync dispatch", "trigger_id", trig.ID, "err", derr)
			_ = r.Journal.MarkTriggerError(ctx, trig.ID, "dispatch: "+derr.Error())
			_ = r.Journal.DeleteWebhookDelivery(ctx, provider, deliveryID)
			http.Error(w, "dispatch failed", http.StatusInternalServerError)
			return
		}
		_ = r.Journal.MarkTriggerFired(ctx, trig.ID)
		code := http.StatusOK
		if status != "succeeded" {
			code = http.StatusBadGateway // the workflow ran but did not succeed
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": runID, "status": status, "output": json.RawMessage(output)})
		return
	}

	if err := r.Disp.Dispatch(ctx, trig, body); err != nil {
		r.Log.Error("webhook: dispatch", "trigger_id", trig.ID, "err", err)
		_ = r.Journal.MarkTriggerError(ctx, trig.ID, "dispatch: "+err.Error())
		// Roll back the dedup claim so the provider's retry of this exact
		// delivery is processed instead of silently deduped. Otherwise a
		// transient dispatch failure permanently eats a Stripe/GitHub
		// webhook.
		if delErr := r.Journal.DeleteWebhookDelivery(ctx, provider, deliveryID); delErr != nil {
			r.Log.Warn("webhook: dedup rollback failed", "err", delErr, "trigger_id", trig.ID)
		}
		// Rate-limit / capacity are backpressure, not server faults: 429 tells
		// the sender to retry later rather than alarming on a 500.
		if errors.Is(err, dispatcher.ErrRateLimited) || errors.Is(err, dispatcher.ErrCapacity) {
			http.Error(w, "rate limited; retry later", http.StatusTooManyRequests)
			return
		}
		http.Error(w, "dispatch failed", http.StatusInternalServerError)
		return
	}
	if err := r.Journal.MarkTriggerFired(ctx, trig.ID); err != nil {
		r.Log.Warn("webhook: mark fired", "err", err)
	}

	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"accepted":true}`))
}

// handleSignal accepts an external delivery for a workflow's AwaitSignal.
// The token is the per-await capability the supervisor minted when the run
// suspended; the SDK exposes the same value via DeriveSignalToken so
// workflows can embed the URL in approval emails before suspending.
//
// HMAC is not enforced here. The token is the auth gate; brute-forcing
// 128 bits of randomness via 1 MiB-bounded HTTP requests is infeasible
// inside the sub-day windows typical for human-in-the-loop signals.
//
// Status codes:
//
//	202 Accepted  payload recorded; scheduler will resume the run on next tick
//	404 Not Found token doesn't match any active signal schedule
//	410 Gone      a prior delivery already won; idempotent retry
//	413 Payload Too Large body exceeded MaxBodyBytes
//	500           journal write failed; client may retry
func (r *Receiver) handleSignal(w http.ResponseWriter, req *http.Request) {
	if r.Log == nil {
		r.Log = slog.Default()
	}

	token := chi.URLParam(req, "token")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, MaxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			r.Log.Warn("signal: body too large", "limit", maxErr.Limit)
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Log.Warn("signal: body read", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	runID, signalName, err := r.Journal.FireSignal(req.Context(), token, body)
	switch {
	case errors.Is(err, journal.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
		return
	case errors.Is(err, journal.ErrAlreadyFired):
		http.Error(w, "already delivered", http.StatusGone)
		return
	case err != nil:
		r.Log.Error("signal: fire", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Log only a fingerprint of the token, never the token itself: it is
	// a bearer capability that grants POST /signal/{token} access without
	// HMAC, so anyone who reads the logs could otherwise replay the
	// signal with an attacker-controlled payload.
	r.Log.Info("signal delivered",
		"run_id", runID, "signal", signalName, "token_fp", tokenFP(token), "bytes", len(body))
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"accepted":true}`))
}

// tokenFP returns a short, non-reversible fingerprint of a capability
// token so it can be correlated in logs without leaking the token.
func tokenFP(token string) string {
	if len(token) <= 8 {
		return "********"
	}
	return token[:8] + "..."
}

// verifyHMAC dispatches per-provider HMAC validation.
func (r *Receiver) verifyHMAC(ctx context.Context, trig journal.Trigger, provider string, h http.Header, body []byte) error {
	if trig.SecretID == "" {
		return errors.New("no secret bound to trigger")
	}
	sec, err := r.Vault.Get(ctx, trig.SecretID)
	if err != nil {
		return fmt.Errorf("vault get: %w", err)
	}
	key := sec.Reveal()

	switch provider {
	case "stripe":
		return verifyStripe(h.Get("Stripe-Signature"), body, key, r.Now())
	case "github":
		return verifyGitHub(h.Get("X-Hub-Signature-256"), body, key)
	default: // generic
		return verifyGeneric(h.Get("X-Webhook-Signature"), body, key)
	}
}

// verifyStripe parses "t=<unix>,v1=<hex>[,v0=<hex>]" headers, recomputes
// HMAC-SHA256 over "<t>.<body>", and compares constant-time. Rejects when
// the timestamp is older than the skew window.
func verifyStripe(header string, body, key []byte, now time.Time) error {
	if header == "" {
		return errors.New("missing Stripe-Signature")
	}
	var (
		ts         string
		signatures []string
	)
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			signatures = append(signatures, kv[1])
		}
	}
	if ts == "" || len(signatures) == 0 {
		return errors.New("malformed Stripe-Signature")
	}
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("bad timestamp: %w", err)
	}
	skew := now.Sub(time.Unix(tsInt, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > stripeSkew {
		return fmt.Errorf("timestamp outside skew window: %s", skew)
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write(body)
	want := mac.Sum(nil)
	for _, s := range signatures {
		got, err := hex.DecodeString(s)
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare(got, want) == 1 {
			return nil
		}
	}
	return errors.New("no v1 signature matched")
}

// verifyGitHub validates "sha256=<hex>" against HMAC-SHA256(body, key).
func verifyGitHub(header string, body, key []byte) error {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return errors.New("missing or malformed X-Hub-Signature-256")
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return fmt.Errorf("bad hex: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	want := mac.Sum(nil)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return errors.New("signature mismatch")
	}
	return nil
}

// verifyGeneric validates "sha256=<hex>" or bare "<hex>" against
// HMAC-SHA256(body, key). The bare form is convenient for clients that
// can't set a custom prefix.
func verifyGeneric(header string, body, key []byte) error {
	header = strings.TrimSpace(header)
	if header == "" {
		return errors.New("missing X-Webhook-Signature")
	}
	header = strings.TrimPrefix(header, "sha256=")
	got, err := hex.DecodeString(header)
	if err != nil {
		return fmt.Errorf("bad hex: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	want := mac.Sum(nil)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return errors.New("signature mismatch")
	}
	return nil
}

// pickDeliveryID resolves a stable delivery identifier for dedup. Providers
// that supply one in headers win; otherwise the SHA-256 of the body is used.
func pickDeliveryID(provider string, h http.Header, body []byte) string {
	switch provider {
	case "stripe":
		// Stripe doesn't ship a delivery header on every event type; fall
		// back to the body hash. Real Stripe payloads include "id" in JSON
		// which we could parse, but parsing JSON in dedup adds attack
		// surface; the body hash is uniform across event shapes.
		return sha256Hex(body)
	case "github":
		if id := h.Get("X-GitHub-Delivery"); id != "" {
			return id
		}
		return sha256Hex(body)
	default:
		if id := h.Get("X-Webhook-Delivery"); id != "" {
			return id
		}
		return sha256Hex(body)
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
