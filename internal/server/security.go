package server

import (
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/bright-interaction/reactor/internal/auth"
)

// securityView is the /security page model.
type securityView struct {
	Factors     auth.MFAFactors
	Elevated    bool // session is inside a step-up window
	NeedsStepUp bool // has a factor but not elevated -> mutations gated
	PasskeysOK  bool // relying party configured server-side
	TOTPOK      bool // encryption key configured server-side
	Msg         string
}

// currentUserFor returns the signed-in user or writes 401 and reports false.
func (s *Server) currentUserFor(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	me, ok := UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return auth.User{}, false
	}
	return me, true
}

// requireStepUp enforces a fresh step-up ("sudo") for security mutations once
// the user already holds a factor. Bootstrapping the first factor is exempt (a
// user with nothing to step up with cannot be asked to). On failure it writes
// the response (JSON for ceremony endpoints, plain text for form posts) and
// returns false.
func (s *Server) requireStepUp(w http.ResponseWriter, r *http.Request, u auth.User, jsonResp bool) bool {
	has, err := s.Auth.HasMFA(r.Context(), u.ID)
	if err != nil {
		if jsonResp {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		} else {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return false
	}
	if !has {
		return true
	}
	if st, ok := SessionStateFromContext(r.Context()); ok && st.IsElevated() {
		return true
	}
	if jsonResp {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "step-up required", "stepUp": true})
	} else {
		http.Error(w, "step-up required: verify your identity on the security page, then retry", http.StatusForbidden)
	}
	return false
}

func (s *Server) securityViewFor(r *http.Request, me auth.User, msg string) (securityView, error) {
	factors, err := s.Auth.ListMFAFactors(r.Context(), me.ID)
	if err != nil {
		return securityView{}, err
	}
	elevated := false
	if st, ok := SessionStateFromContext(r.Context()); ok {
		elevated = st.IsElevated()
	}
	hasMFA := len(factors.Passkeys) > 0 || factors.TOTPEnabled
	return securityView{
		Factors:     factors,
		Elevated:    elevated,
		NeedsStepUp: hasMFA && !elevated,
		PasskeysOK:  s.Auth.WebAuthnAvailable(),
		TOTPOK:      s.Auth.TOTPAvailable(),
		Msg:         msg,
	}, nil
}

// security renders the self-service factor management page.
func (s *Server) security(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	view, err := s.securityViewFor(r, me, r.URL.Query().Get("msg"))
	if err != nil {
		s.errorPage(w, "load security", err)
		return
	}
	s.renderPage(w, r, page{Title: "Security", Heading: "Security", Body: template.HTML(securityBody(view))})
}

// --- passkey registration (JSON ceremony) ---

func (s *Server) securityWebAuthnRegisterBegin(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	if !s.requireStepUp(w, r, me, true) {
		return
	}
	opts, id, err := s.Auth.BeginWebAuthnRegistration(r.Context(), me)
	if err != nil {
		if errors.Is(err, auth.ErrWebAuthnMissing) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "passkeys are not configured on this server"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start registration"})
		return
	}
	writeJSON(w, http.StatusOK, ceremonyBegin{ChallengeID: id, Options: opts})
}

func (s *Server) securityWebAuthnRegisterFinish(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	if !s.requireStepUp(w, r, me, true) {
		return
	}
	f, ok := readCeremonyFinish(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(f.Name)
	if len(name) > 64 {
		name = name[:64]
	}
	if err := s.Auth.FinishWebAuthnRegistration(r.Context(), me, f.ChallengeID, f.Response, name); err != nil {
		if errors.Is(err, auth.ErrPasskeyExists) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "that passkey is already registered"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "registration failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": "/security?msg=" + msgPasskeyAdded})
}

func (s *Server) securityWebAuthnDelete(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	if !s.requireStepUp(w, r, me, false) {
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.Auth.DeleteWebAuthnCredential(r.Context(), me.ID, id); err != nil && !errors.Is(err, auth.ErrNotFound) {
		s.errorPage(w, "delete passkey", err)
		return
	}
	http.Redirect(w, r, "/security?msg="+msgPasskeyRemoved, http.StatusSeeOther)
}

func (s *Server) securityWebAuthnRename(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	if !s.requireStepUp(w, r, me, false) {
		return
	}
	_ = r.ParseForm()
	id := chi.URLParam(r, "id")
	name := strings.TrimSpace(r.PostFormValue("name"))
	if err := s.Auth.RenameWebAuthnCredential(r.Context(), me.ID, id, name); err != nil && !errors.Is(err, auth.ErrNotFound) {
		s.errorPage(w, "rename passkey", err)
		return
	}
	http.Redirect(w, r, "/security", http.StatusSeeOther)
}

// --- TOTP enrollment (form flow) ---

func (s *Server) securityTOTPBegin(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	if !s.requireStepUp(w, r, me, false) {
		return
	}
	secret, otpauthURL, err := s.Auth.BeginTOTPEnrollment(r.Context(), me)
	if err != nil {
		if errors.Is(err, auth.ErrMFAKeyMissing) {
			http.Error(w, "TOTP is not configured on this server", http.StatusConflict)
			return
		}
		if errors.Is(err, auth.ErrTOTPAlreadyEnabled) {
			http.Redirect(w, r, "/security", http.StatusSeeOther)
			return
		}
		s.errorPage(w, "start totp", err)
		return
	}
	qr, err := qrDataURI(otpauthURL)
	if err != nil {
		s.errorPage(w, "totp qr", err)
		return
	}
	s.renderPage(w, r, page{Title: "Set up authenticator app", Heading: "Set up authenticator app", Body: template.HTML(totpEnrollBody(secret, qr, ""))})
}

func (s *Server) securityTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	if !s.requireStepUp(w, r, me, false) {
		return
	}
	_ = r.ParseForm()
	code := strings.TrimSpace(r.PostFormValue("code"))
	if err := s.Auth.ConfirmTOTP(r.Context(), me.ID, code); err != nil {
		// Re-render the enrollment page with the error, keeping the pending secret.
		if errors.Is(err, auth.ErrCodeInvalid) {
			s.reRenderTOTPEnroll(w, r, me, "That code did not match. Check your device clock and try again.")
			return
		}
		if errors.Is(err, auth.ErrNoMFA) {
			http.Redirect(w, r, "/security", http.StatusSeeOther)
			return
		}
		s.errorPage(w, "confirm totp", err)
		return
	}
	http.Redirect(w, r, "/security?msg="+msgTOTPEnabled, http.StatusSeeOther)
}

// reRenderTOTPEnroll rebuilds the enroll page from the still-pending secret so a
// wrong code does not force the operator to restart enrollment.
func (s *Server) reRenderTOTPEnroll(w http.ResponseWriter, r *http.Request, me auth.User, msg string) {
	// The unconfirmed secret still exists; re-begin returns the SAME row only if
	// unconfirmed, but BeginTOTPEnrollment regenerates. To avoid rotating the
	// secret under the user, just show the error on a minimal page pointing back.
	w.WriteHeader(http.StatusBadRequest)
	s.renderPage(w, r, page{Title: "Set up authenticator app", Heading: "Set up authenticator app", Body: template.HTML(totpRetryBody(msg))})
}

func (s *Server) securityTOTPDisable(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	if !s.requireStepUp(w, r, me, false) {
		return
	}
	if err := s.Auth.DisableTOTP(r.Context(), me.ID); err != nil {
		s.errorPage(w, "disable totp", err)
		return
	}
	http.Redirect(w, r, "/security?msg="+msgTOTPDisabled, http.StatusSeeOther)
}

// --- recovery codes ---

func (s *Server) securityRecoveryRegenerate(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	if !s.requireStepUp(w, r, me, false) {
		return
	}
	codes, err := s.Auth.GenerateRecoveryCodes(r.Context(), me.ID)
	if err != nil {
		s.errorPage(w, "recovery codes", err)
		return
	}
	s.renderPage(w, r, page{Title: "Recovery codes", Heading: "Recovery codes", Body: template.HTML(recoveryCodesBody(codes))})
}

// --- step-up ("sudo") assertion ---

func (s *Server) securityStepUpBegin(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	opts, id, err := s.Auth.BeginWebAuthnAssertion(r.Context(), me, auth.PurposeStepUp)
	if err != nil {
		if errors.Is(err, auth.ErrNoMFA) || errors.Is(err, auth.ErrWebAuthnMissing) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "no passkey available for step-up"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start verification"})
		return
	}
	writeJSON(w, http.StatusOK, ceremonyBegin{ChallengeID: id, Options: opts})
}

func (s *Server) securityStepUpFinish(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	f, ok := readCeremonyFinish(w, r)
	if !ok {
		return
	}
	if err := s.Auth.FinishWebAuthnAssertion(r.Context(), me, auth.PurposeStepUp, f.ChallengeID, f.Response); err != nil {
		s.mfaFinishError(w, err)
		return
	}
	if err := s.Auth.MarkSessionElevated(r.Context(), sessionCookieFromContext(r.Context()), auth.StepUpWindow); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not elevate session"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": "/security"})
}

// securityStepUpTOTP is the non-passkey step-up path: a TOTP code elevates the
// session so a user whose only factor is TOTP can still authorize mutations.
func (s *Server) securityStepUpTOTP(w http.ResponseWriter, r *http.Request) {
	me, ok := s.currentUserFor(w, r)
	if !ok {
		return
	}
	_ = r.ParseForm()
	code := strings.TrimSpace(r.PostFormValue("code"))
	verified, err := s.Auth.VerifyTOTP(r.Context(), me.ID, code)
	if err != nil || !verified {
		http.Redirect(w, r, "/security?msg="+msgStepUpFailed, http.StatusSeeOther)
		return
	}
	if err := s.Auth.MarkSessionElevated(r.Context(), sessionCookieFromContext(r.Context()), auth.StepUpWindow); err != nil {
		s.errorPage(w, "elevate session", err)
		return
	}
	http.Redirect(w, r, "/security", http.StatusSeeOther)
}
