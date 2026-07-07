// Package http is the workflow-side HTTP client helper. AI-generated
// workflows import this instead of building net/http calls from scratch
// so retry, timeout, JSON encoding, and bearer auth all converge on
// one tested implementation.
//
// All requests must carry context.Context with a timeout (the
// codegen lint rule h_timeout enforces this) so this package never
// dials without a deadline.
package http

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"
)

// DefaultClient is the singleton used by Get/Post/Do unless the caller
// constructs their own Client. Bounded timeout + connection pooling
// match the codegen prompt's guidance.
var DefaultClient = &Client{
	HTTPClient: &http.Client{Timeout: 30 * time.Second},
	Retry: Retry{
		Max:       3,
		BaseDelay: 250 * time.Millisecond,
		MaxDelay:  10 * time.Second,
		Jitter:    true,
	},
}

// Client wraps net/http.Client with retry + jitter + bearer auth
// helpers. Construct one per upstream + reuse so connection pooling
// works.
type Client struct {
	HTTPClient *http.Client
	Retry      Retry

	// Bearer, when non-empty, is sent as Authorization: Bearer <value>
	// on every request. Workflows fetch this via vault.MustGet so the
	// value is encrypted at rest + redacted on accidental log.
	Bearer string

	// UserAgent override; defaults to "reactor-sdk/0.1".
	UserAgent string

	// Headers are static headers sent on every request. Use this for auth
	// shapes Bearer does not cover: a raw token (ClickUp "Authorization: <t>"),
	// an API-key header (Storyblok, Shortcut), a version pin (Notion-Version),
	// or HTTP Basic ("Authorization: Basic <base64>"). Set per upstream and
	// reuse the client. Values here win over the Bearer default.
	Headers map[string]string
}

// Retry configures the exponential-backoff-with-jitter retry loop. The
// codegen prompt's h_retry-jitter entry teaches this shape; mirroring
// it in the SDK reduces what the AI has to emit per workflow.
//
// Mirrors the field semantics of sdk.ExpBackoff (Max, Base, Cap) so
// authors learn one shape: BaseDelay aliases Base, MaxDelay aliases
// Cap. RetryFromExpBackoff is the one-liner adapter when an author
// already constructed an reactor.ExpBackoff for a Step retry policy.
type Retry struct {
	Max       int           // total attempts (1 = no retry)
	BaseDelay time.Duration // delay before attempt 2 (alias of reactor.ExpBackoff.Base)
	MaxDelay  time.Duration // upper bound on per-attempt delay (alias of reactor.ExpBackoff.Cap)
	Jitter    bool          // when true, sleep is uniform[0, computed]
}

// RetryFromExpBackoff converts the reactor SDK's RetryPolicy shape to
// the http.Client Retry shape. Jitter defaults to true so http
// retries pick up the full-jitter behaviour ExpBackoff already
// provides for Step retries.
//
// Usage:
//
//	c := &http.Client{Retry: http.RetryFromExpBackoff(reactor.ExpBackoff{Max: 5, Base: 100*time.Millisecond})}
func RetryFromExpBackoff(max int, base, ceiling time.Duration) Retry {
	return Retry{
		Max:       max,
		BaseDelay: base,
		MaxDelay:  ceiling,
		Jitter:    true,
	}
}

// Get fetches url + decodes the JSON response into out. Returns an
// error wrapping the upstream body on non-2xx so the caller can
// surface a meaningful step failure message.
func (c *Client) Get(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	return c.doDecode(req, out)
}

// PostJSON sends a JSON body + decodes the JSON response into out.
func (c *Client) PostJSON(ctx context.Context, url string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doDecode(req, out)
}

// Do executes req under the retry policy. Returns the response body
// + status. The caller is responsible for closing the body when not
// using the JSON helpers.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	c.applyHeaders(req)
	attempt := 0
	for {
		attempt++
		resp, err := c.client().Do(req)
		if err == nil && resp.StatusCode < 500 {
			return resp, nil
		}
		if attempt >= c.maxAttempts() {
			if err != nil {
				return nil, fmt.Errorf("attempt %d/%d: %w", attempt, c.maxAttempts(), err)
			}
			return resp, nil
		}
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		select {
		case <-time.After(c.delay(attempt)):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
}

func (c *Client) doDecode(req *http.Request, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode response: %w (body=%s)", err, trim(body))
		}
		return nil
	}
	return &Error{Status: resp.StatusCode, Body: string(body)}
}

func (c *Client) applyHeaders(req *http.Request) {
	if c.Bearer != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	}
	// Static headers (custom auth, version pins). Set after Bearer so they win.
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "reactor-sdk/0.1"
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", ua)
	}
}

func (c *Client) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) maxAttempts() int {
	if c.Retry.Max < 1 {
		return 1
	}
	return c.Retry.Max
}

// delay returns the per-attempt sleep. Exponential backoff with full
// jitter: delay = uniform(0, min(maxDelay, base * 2^(attempt-1))).
func (c *Client) delay(attempt int) time.Duration {
	base := c.Retry.BaseDelay
	if base <= 0 {
		base = 250 * time.Millisecond
	}
	max := c.Retry.MaxDelay
	if max <= 0 {
		max = 10 * time.Second
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > max {
			d = max
			break
		}
	}
	if !c.Retry.Jitter {
		return d
	}
	// crypto/rand to avoid math/rand which is banned by reactor lint.
	n, err := rand.Int(rand.Reader, big.NewInt(int64(d)))
	if err != nil {
		return d
	}
	return time.Duration(n.Int64())
}

// Error is the typed non-2xx error returned by Get / PostJSON. Callers
// can errors.As to read the status + body without parsing the message.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, trim([]byte(e.Body)))
}

// IsRetryable reports whether err looks transient enough that the
// caller's outer Step retry should reschedule. 5xx, timeouts, and
// connection failures qualify; 4xx (except 408 and 429) does not.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		if apiErr.Status >= 500 || apiErr.Status == 408 || apiErr.Status == 429 {
			return true
		}
		return false
	}
	// Bare net.Error / connection-reset / DNS -- treat as retryable.
	return true
}

func trim(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

