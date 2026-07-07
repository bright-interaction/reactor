package server

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html/template"
	"image/png"
	"strings"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"
)

// Flash message codes passed via ?msg= after a security mutation.
const (
	msgPasskeyAdded   = "passkey_added"
	msgPasskeyRemoved = "passkey_removed"
	msgTOTPEnabled    = "totp_enabled"
	msgTOTPDisabled   = "totp_disabled"
	msgStepUpFailed   = "stepup_failed"
)

func msgText(code string) string {
	switch code {
	case msgPasskeyAdded:
		return "Passkey added."
	case msgPasskeyRemoved:
		return "Passkey removed."
	case msgTOTPEnabled:
		return "Authenticator app enabled."
	case msgTOTPDisabled:
		return "Authenticator app removed."
	case msgStepUpFailed:
		return "That verification did not match. Try again."
	default:
		return ""
	}
}

func esc(s string) string { return template.HTMLEscapeString(s) }

// mfaChallengeBody renders the second-factor prompt shown at login for a
// pending session. Passkey uses the external webauthn.js (CSP: no inline JS).
func mfaChallengeBody(next string, hasPasskey, hasTOTP bool, errMsg string) string {
	var b strings.Builder
	if errMsg != "" {
		fmt.Fprintf(&b, `<div class="err" style="background:#fff;border:1px solid var(--err);padding:12px 16px;margin:0 0 16px;border-radius:3px;">%s</div>`, esc(errMsg))
	}
	b.WriteString(`<p class="muted" style="max-width:440px;">You have two-factor authentication on. Confirm one of your factors to finish signing in.</p>`)
	b.WriteString(`<div style="max-width:440px;display:flex;flex-direction:column;gap:20px;">`)

	if hasPasskey {
		finish := "/login/mfa/webauthn/finish?next=" + template.URLQueryEscaper(next)
		fmt.Fprintf(&b, `<div>
  <button type="button" class="btn-primary" data-webauthn="assert" data-begin="/login/mfa/webauthn/begin" data-finish="%s">Use a passkey</button>
</div>`, esc(finish))
	}
	if hasTOTP {
		fmt.Fprintf(&b, `<form method="POST" action="/login/mfa/totp" class="form">
  <input type="hidden" name="next" value="%s">
  <label>Authenticator code <input type="text" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9 ]*" required autofocus></label>
  <button type="submit" class="btn-primary">Verify code</button>
</form>`, esc(next))
	}
	fmt.Fprintf(&b, `<details><summary class="muted">Use a recovery code</summary>
  <form method="POST" action="/login/mfa/recovery" class="form" style="margin-top:10px;">
    <input type="hidden" name="next" value="%s">
    <label>Recovery code <input type="text" name="code" autocomplete="off" required></label>
    <button type="submit" class="btn">Use recovery code</button>
  </form>
</details>`, esc(next))
	b.WriteString(`</div>`)
	b.WriteString(`<p id="webauthn-error" class="err" hidden style="margin-top:14px;color:var(--err);"></p>`)
	b.WriteString(`<script src="/assets/webauthn.js"></script>`)
	return b.String()
}

// securityBody renders the self-service factor management page.
func securityBody(v securityView) string {
	var b strings.Builder
	if t := msgText(v.Msg); t != "" {
		fmt.Fprintf(&b, `<div class="ok" style="background:#fff;border:1px solid var(--ok,#2a7);padding:10px 14px;margin:0 0 16px;border-radius:3px;">%s</div>`, esc(t))
	}

	// Server-capability notices.
	if !v.PasskeysOK {
		b.WriteString(`<p class="muted">Passkeys are not enabled on this server. Set <code>REACTOR_WEBAUTHN_RP_ID</code> and <code>REACTOR_WEBAUTHN_RP_ORIGIN</code> (or <code>REACTOR_DASHBOARD_URL</code>) to enable them.</p>`)
	}
	if !v.TOTPOK {
		b.WriteString(`<p class="muted">Authenticator-app (TOTP) enrollment needs a master key. Set <code>REACTOR_MASTER_KEY</code> to enable it.</p>`)
	}

	canManage := !v.NeedsStepUp

	// Step-up banner when the user has a factor but the session is not elevated.
	if v.NeedsStepUp {
		b.WriteString(`<div style="background:#fff;border:1px solid var(--accent,#c93);padding:14px 16px;margin:0 0 20px;border-radius:3px;max-width:560px;">`)
		b.WriteString(`<strong>Verify your identity to change security settings.</strong>`)
		b.WriteString(`<div style="display:flex;flex-direction:column;gap:12px;margin-top:12px;">`)
		if len(v.Factors.Passkeys) > 0 && v.PasskeysOK {
			b.WriteString(`<button type="button" class="btn-primary" data-webauthn="assert" data-begin="/security/stepup/begin" data-finish="/security/stepup/finish">Verify with a passkey</button>`)
		}
		if v.Factors.TOTPEnabled {
			b.WriteString(`<form method="POST" action="/security/stepup/totp" class="form" style="margin:0;">
  <label>Authenticator code <input type="text" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9 ]*" required></label>
  <button type="submit" class="btn">Verify with code</button>
</form>`)
		}
		b.WriteString(`</div></div>`)
	}

	// --- Passkeys ---
	b.WriteString(`<section style="margin-bottom:28px;"><h2 style="font-size:1.05rem;margin:0 0 8px;">Passkeys</h2>`)
	b.WriteString(`<p class="muted" style="margin-top:0;">Phishing-resistant sign-in with Touch ID, Windows Hello, or a security key.</p>`)
	if len(v.Factors.Passkeys) == 0 {
		b.WriteString(`<p class="muted">No passkeys registered.</p>`)
	} else {
		b.WriteString(`<table class="tbl"><thead><tr><th>Name</th><th>Added</th><th>Last used</th><th></th></tr></thead><tbody>`)
		for _, p := range v.Factors.Passkeys {
			last := "never"
			if p.LastUsedAt != nil {
				last = p.LastUsedAt.Format("2006-01-02")
			}
			badges := ""
			if p.BackedUp {
				badges += ` <span class="muted" title="Synced/backed up passkey">(synced)</span>`
			}
			if p.CloneWarning {
				badges += ` <span style="color:var(--err);" title="Signature counter regressed; possible clone. Remove and re-register.">(clone warning)</span>`
			}
			fmt.Fprintf(&b, `<tr><td>%s%s</td><td>%s</td><td>%s</td><td style="text-align:right;">`,
				esc(p.Name), badges, p.CreatedAt.Format("2006-01-02"), esc(last))
			if canManage {
				fmt.Fprintf(&b, `<form method="POST" action="/security/webauthn/%s/delete" style="display:inline;"><button type="submit" class="btn-danger">Remove</button></form>`, esc(p.ID))
			}
			b.WriteString(`</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	}
	if canManage && v.PasskeysOK {
		b.WriteString(`<div style="margin-top:12px;display:flex;gap:8px;align-items:end;">
  <label style="margin:0;">Name <input type="text" id="passkey-name" placeholder="e.g. MacBook Touch ID" maxlength="64"></label>
  <button type="button" class="btn-primary" data-webauthn="register" data-begin="/security/webauthn/register/begin" data-finish="/security/webauthn/register/finish" data-name-source="passkey-name">Add passkey</button>
</div>`)
	}
	b.WriteString(`</section>`)

	// --- Authenticator app (TOTP) ---
	b.WriteString(`<section style="margin-bottom:28px;"><h2 style="font-size:1.05rem;margin:0 0 8px;">Authenticator app</h2>`)
	if v.Factors.TOTPEnabled {
		b.WriteString(`<p>Enabled.</p>`)
		if canManage {
			b.WriteString(`<form method="POST" action="/security/totp/disable"><button type="submit" class="btn-danger">Remove authenticator app</button></form>`)
		}
	} else if v.TOTPOK {
		b.WriteString(`<p class="muted" style="margin-top:0;">Time-based codes from Google Authenticator, 1Password, and similar.</p>`)
		if canManage {
			b.WriteString(`<form method="POST" action="/security/totp/begin"><button type="submit" class="btn-primary">Set up authenticator app</button></form>`)
		}
	}
	b.WriteString(`</section>`)

	// --- Recovery codes ---
	b.WriteString(`<section><h2 style="font-size:1.05rem;margin:0 0 8px;">Recovery codes</h2>`)
	fmt.Fprintf(&b, `<p class="muted" style="margin-top:0;">One-time codes to sign in if you lose your other factors. %d remaining.</p>`, v.Factors.RecoveryLeft)
	if canManage {
		b.WriteString(`<form method="POST" action="/security/recovery/regenerate"><button type="submit" class="btn">Generate new recovery codes</button></form>`)
		b.WriteString(`<p class="muted">Generating a new set invalidates the old one.</p>`)
	}
	b.WriteString(`</section>`)

	b.WriteString(`<p id="webauthn-error" class="err" hidden style="margin-top:14px;color:var(--err);"></p>`)
	b.WriteString(`<script src="/assets/webauthn.js"></script>`)
	return b.String()
}

// totpEnrollBody shows the QR + secret + confirm form for a new TOTP enrollment.
func totpEnrollBody(secret, qrURI, errMsg string) string {
	var b strings.Builder
	if errMsg != "" {
		fmt.Fprintf(&b, `<div class="err" style="background:#fff;border:1px solid var(--err);padding:12px 16px;margin:0 0 16px;border-radius:3px;">%s</div>`, esc(errMsg))
	}
	b.WriteString(`<ol class="muted" style="max-width:520px;"><li>Scan the QR code with your authenticator app, or enter the key manually.</li><li>Enter the 6-digit code it shows to confirm.</li></ol>`)
	fmt.Fprintf(&b, `<div style="margin:16px 0;"><img src="%s" alt="TOTP QR code" width="220" height="220" style="border:1px solid var(--border,#ddd);border-radius:4px;"></div>`, esc(qrURI))
	fmt.Fprintf(&b, `<p>Manual key: <code style="user-select:all;">%s</code></p>`, esc(secret))
	b.WriteString(`<form method="POST" action="/security/totp/confirm" class="form" style="max-width:320px;">
  <label>6-digit code <input type="text" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9 ]*" required autofocus></label>
  <button type="submit" class="btn-primary">Confirm and enable</button>
</form>
<p><a href="/security" class="muted">Cancel</a></p>`)
	return b.String()
}

// totpRetryBody is shown when a confirm code is wrong; the pending (unconfirmed)
// secret is still stored, but rather than reprint it we send the operator back
// to restart cleanly (a fresh QR), which avoids leaving a half-entered state.
func totpRetryBody(msg string) string {
	return fmt.Sprintf(`<div class="err" style="background:#fff;border:1px solid var(--err);padding:12px 16px;margin:0 0 16px;border-radius:3px;">%s</div>
<form method="POST" action="/security/totp/begin"><button type="submit" class="btn-primary">Start over</button></form>
<p><a href="/security" class="muted">Cancel</a></p>`, esc(msg))
}

// recoveryCodesBody shows a freshly generated batch exactly once.
func recoveryCodesBody(codes []string) string {
	var b strings.Builder
	b.WriteString(`<p class="muted" style="max-width:520px;">Save these somewhere safe. Each code works once. This is the only time they are shown; regenerate to get a new set.</p>`)
	b.WriteString(`<pre style="background:var(--code-bg,#f5f5f5);border:1px solid var(--border,#ddd);border-radius:4px;padding:16px;max-width:320px;user-select:all;line-height:1.9;">`)
	for _, c := range codes {
		b.WriteString(esc(c) + "\n")
	}
	b.WriteString(`</pre>`)
	b.WriteString(`<p><a href="/security" class="btn-primary" style="display:inline-block;">I've saved them</a></p>`)
	return b.String()
}

// qrDataURI renders an otpauth:// URL as a PNG data URI (server-side, so no
// client-side QR library or CDN is needed; CSP-clean).
func qrDataURI(content string) (string, error) {
	code, err := qr.Encode(content, qr.M, qr.Auto)
	if err != nil {
		return "", err
	}
	code, err = barcode.Scale(code, 220, 220)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, code); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
