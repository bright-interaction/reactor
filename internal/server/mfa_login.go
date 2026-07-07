package server

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"strings"

	"github.com/bright-interaction/reactor/internal/auth"
)

// ceremonyBegin is the JSON a *_begin endpoint returns: the opaque challenge id
// to echo back, and the raw WebAuthn options for navigator.credentials.
type ceremonyBegin struct {
	ChallengeID string          `json:"challengeId"`
	Options     json.RawMessage `json:"options"`
}

// ceremonyFinish is the JSON a *_finish endpoint receives from webauthn.js.
type ceremonyFinish struct {
	ChallengeID string          `json:"challengeId"`
	Name        string          `json:"name"`
	Response    json.RawMessage `json:"response"`
}

// maxCeremonyBody caps a ceremony finish body. Attestation objects are a few KB.
const maxCeremonyBody = 64 << 10

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readCeremonyFinish(w http.ResponseWriter, r *http.Request) (ceremonyFinish, bool) {
	var f ceremonyFinish
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCeremonyBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
		return f, false
	}
	if err := json.Unmarshal(body, &f); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return f, false
	}
	return f, true
}

// mfaChallenge renders the second-factor prompt for a pending session. A full
// (already-cleared) session that lands here is just sent on to its target.
func (s *Server) mfaChallenge(w http.ResponseWriter, r *http.Request) {
	me, ok := UserFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	next := safeNextPath(r.URL.Query().Get("next"))
	if st, ok := SessionStateFromContext(r.Context()); ok && !st.MFAPending {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	factors, err := s.Auth.ListMFAFactors(r.Context(), me.ID)
	if err != nil {
		s.errorPage(w, "load factors", err)
		return
	}
	s.renderPage(w, r, page{
		Title:   "Verify it's you",
		Heading: "Verify it's you",
		Body:    template.HTML(mfaChallengeBody(next, len(factors.Passkeys) > 0, factors.TOTPEnabled, "")),
	})
}

func (s *Server) mfaLoginWebAuthnBegin(w http.ResponseWriter, r *http.Request) {
	me, ok := UserFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	opts, id, err := s.Auth.BeginWebAuthnAssertion(r.Context(), me, auth.PurposeLogin)
	if err != nil {
		s.mfaBeginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ceremonyBegin{ChallengeID: id, Options: opts})
}

func (s *Server) mfaLoginWebAuthnFinish(w http.ResponseWriter, r *http.Request) {
	me, ok := UserFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	f, ok := readCeremonyFinish(w, r)
	if !ok {
		return
	}
	if err := s.Auth.FinishWebAuthnAssertion(r.Context(), me, auth.PurposeLogin, f.ChallengeID, f.Response); err != nil {
		s.mfaFinishError(w, err)
		return
	}
	if err := s.Auth.ClearSessionMFAPending(r.Context(), sessionCookieFromContext(r.Context())); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not complete sign-in"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": safeNextPath(r.URL.Query().Get("next"))})
}

func (s *Server) mfaLoginTOTP(w http.ResponseWriter, r *http.Request) {
	me, ok := UserFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	next := safeNextPath(r.PostFormValue("next"))
	throttleKey := clientIP(r) + "|mfa|" + me.ID
	if s.loginThrottle().locked(throttleKey) {
		s.renderMFAError(w, r, next, "Too many attempts. Wait 15 minutes and try again.")
		return
	}
	code := strings.TrimSpace(r.PostFormValue("code"))
	ok, err := s.Auth.VerifyTOTP(r.Context(), me.ID, code)
	if err != nil || !ok {
		s.loginThrottle().recordFailure(throttleKey)
		s.renderMFAError(w, r, next, "That code did not match. Try again.")
		return
	}
	s.loginThrottle().reset(throttleKey)
	if err := s.Auth.ClearSessionMFAPending(r.Context(), sessionCookieFromContext(r.Context())); err != nil {
		s.errorPage(w, "complete sign-in", err)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) mfaLoginRecovery(w http.ResponseWriter, r *http.Request) {
	me, ok := UserFromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = r.ParseForm()
	next := safeNextPath(r.PostFormValue("next"))
	throttleKey := clientIP(r) + "|mfa|" + me.ID
	if s.loginThrottle().locked(throttleKey) {
		s.renderMFAError(w, r, next, "Too many attempts. Wait 15 minutes and try again.")
		return
	}
	code := r.PostFormValue("code")
	ok, err := s.Auth.ConsumeRecoveryCode(r.Context(), me.ID, code)
	if err != nil || !ok {
		s.loginThrottle().recordFailure(throttleKey)
		s.renderMFAError(w, r, next, "That recovery code is not valid or has already been used.")
		return
	}
	s.loginThrottle().reset(throttleKey)
	if err := s.Auth.ClearSessionMFAPending(r.Context(), sessionCookieFromContext(r.Context())); err != nil {
		s.errorPage(w, "complete sign-in", err)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) renderMFAError(w http.ResponseWriter, r *http.Request, next, msg string) {
	me, _ := UserFromContext(r.Context())
	hasPasskey, totp := false, false
	if me.ID != "" {
		if f, err := s.Auth.ListMFAFactors(r.Context(), me.ID); err == nil {
			hasPasskey = len(f.Passkeys) > 0
			totp = f.TOTPEnabled
		}
	}
	w.WriteHeader(http.StatusUnauthorized)
	s.renderPage(w, r, page{
		Title:   "Verify it's you",
		Heading: "Verify it's you",
		Body:    template.HTML(mfaChallengeBody(next, hasPasskey, totp, msg)),
	})
}

// mfaBeginError maps a begin error to a status without leaking internals.
func (s *Server) mfaBeginError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrWebAuthnMissing), errors.Is(err, auth.ErrNoMFA):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "passkey sign-in is not available"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start passkey check"})
	}
}

// mfaFinishError maps a finish error to a status. A clone warning is surfaced
// distinctly so the operator can act on it.
func (s *Server) mfaFinishError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrCloneWarning):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this authenticator may be cloned; sign-in refused. Remove and re-register it."})
	case errors.Is(err, auth.ErrChallengeExpired):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the check expired; try again"})
	default:
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "passkey check failed"})
	}
}
