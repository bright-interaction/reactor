// Package email is the workflow-side email connector. AI-generated workflows
// call email.Send (or the provider-specific SendGmail / SendOutlook) from
// inside a Step closure to send through a connected Google or Microsoft
// account. The access token comes from the vault at run time:
//
//	tok := vault.MustGet("oauth:" + connectionID) // resolved + auto-refreshed by the host
//	id, err := email.SendGmail(ctx, string(tok.Reveal()), email.Message{
//		From: "me@example.com", To: []string{"customer@acme.com"},
//		Subject: "Welcome", Text: "Thanks for signing up.",
//	})
//
// The token never reaches the AI builder: codegen only sees the credential
// metadata (the connection name), and the real value is exposed only while the
// workflow runs. Sending is a side effect, so wrap these calls in a Step with a
// non-empty IdempotencyKey.
package email

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"strings"

	ahttp "github.com/bright-interaction/reactor/sdk/http"
)

// Endpoints are package vars so tests can point them at a fake server. They
// default to the real Gmail + Microsoft Graph send endpoints.
var (
	gmailSendURL = "https://gmail.googleapis.com/gmail/v1/users/me/messages/send"
	graphSendURL = "https://graph.microsoft.com/v1.0/me/sendMail"
)

// Provider identifies which mail backend a connection talks to. It matches the
// OAuth provider id of the connection.
type Provider string

const (
	Google    Provider = "google"
	Microsoft Provider = "microsoft"
)

// Message is a plain-text and/or HTML email. Set Text, HTML, or both (both
// sends a multipart/alternative so clients pick the richest they render).
type Message struct {
	From    string   // sender address, usually the connected account
	To      []string // at least one recipient
	Cc      []string
	Subject string
	Text    string // plain-text body
	HTML    string // optional HTML body
}

// Send dispatches to the right provider. It is the convenience entry point when
// the workflow already knows the connection's provider; otherwise call
// SendGmail / SendOutlook directly. Returns the provider message id when one is
// available (Gmail returns an id; Microsoft Graph does not).
func Send(ctx context.Context, provider Provider, accessToken string, msg Message) (string, error) {
	switch provider {
	case Google:
		return SendGmail(ctx, accessToken, msg)
	case Microsoft:
		return "", SendOutlook(ctx, accessToken, msg)
	default:
		return "", fmt.Errorf("email: unsupported provider %q (want google or microsoft)", provider)
	}
}

// SendGmail sends msg via the Gmail API using a Google OAuth access token
// (scope gmail.send). Returns the sent message id.
func SendGmail(ctx context.Context, accessToken string, msg Message) (string, error) {
	if accessToken == "" {
		return "", errors.New("email: empty access token")
	}
	raw, err := msg.rfc822()
	if err != nil {
		return "", err
	}
	c := &ahttp.Client{Bearer: accessToken}
	body := map[string]string{"raw": base64.URLEncoding.EncodeToString(raw)}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.PostJSON(ctx, gmailSendURL, body, &out); err != nil {
		return "", fmt.Errorf("email: gmail send: %w", err)
	}
	return out.ID, nil
}

// SendOutlook sends msg via Microsoft Graph using a Microsoft OAuth access
// token (scope Mail.Send). Graph returns 202 with no body, so there is no id.
func SendOutlook(ctx context.Context, accessToken string, msg Message) error {
	if accessToken == "" {
		return errors.New("email: empty access token")
	}
	if msg.From == "" || len(msg.To) == 0 {
		return errors.New("email: From and at least one To are required")
	}
	c := &ahttp.Client{Bearer: accessToken}
	if err := c.PostJSON(ctx, graphSendURL, graphPayload(msg), nil); err != nil {
		return fmt.Errorf("email: outlook send: %w", err)
	}
	return nil
}

// graphPayload builds the Microsoft Graph sendMail JSON body.
func graphPayload(m Message) map[string]any {
	ct, content := "Text", m.Text
	if m.HTML != "" {
		ct, content = "HTML", m.HTML
	}
	msg := map[string]any{
		"subject":      m.Subject,
		"body":         map[string]string{"contentType": ct, "content": content},
		"toRecipients": recipients(m.To),
	}
	if len(m.Cc) > 0 {
		msg["ccRecipients"] = recipients(m.Cc)
	}
	if m.From != "" {
		msg["from"] = map[string]any{"emailAddress": map[string]string{"address": m.From}}
	}
	return map[string]any{"message": msg, "saveToSentItems": true}
}

func recipients(addrs []string) []map[string]any {
	out := make([]map[string]any, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, map[string]any{"emailAddress": map[string]string{"address": a}})
	}
	return out
}

// rfc822 builds an RFC 2822 message (CRLF line endings, UTF-8 quoted-printable
// bodies) suitable for Gmail's raw send.
func (m Message) rfc822() ([]byte, error) {
	if m.From == "" {
		return nil, errors.New("email: From is required")
	}
	if len(m.To) == 0 {
		return nil, errors.New("email: at least one To recipient is required")
	}
	if m.Text == "" && m.HTML == "" {
		return nil, errors.New("email: Text or HTML body is required")
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", m.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(m.To, ", "))
	if len(m.Cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(m.Cc, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	b.WriteString("MIME-Version: 1.0\r\n")

	switch {
	case m.Text != "" && m.HTML != "":
		boundary := m.boundary()
		fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
		writePart(&b, boundary, "text/plain", m.Text)
		writePart(&b, boundary, "text/html", m.HTML)
		fmt.Fprintf(&b, "--%s--\r\n", boundary)
	case m.HTML != "":
		b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		writeQP(&b, m.HTML)
	default:
		b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		writeQP(&b, m.Text)
	}
	return b.Bytes(), nil
}

func writePart(b *bytes.Buffer, boundary, contentType, content string) {
	fmt.Fprintf(b, "--%s\r\n", boundary)
	fmt.Fprintf(b, "Content-Type: %s; charset=\"UTF-8\"\r\n", contentType)
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	writeQP(b, content)
	b.WriteString("\r\n")
}

func writeQP(b *bytes.Buffer, s string) {
	w := quotedprintable.NewWriter(b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
}

// boundary derives a deterministic MIME boundary from the content so the same
// message always serialises identically (helpful for replay) while never
// colliding with the body text in practice.
func (m Message) boundary() string {
	sum := sha256.Sum256([]byte(m.Text + "\x00" + m.HTML))
	return "reactor_" + hex.EncodeToString(sum[:])[:32]
}
