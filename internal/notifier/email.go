package notifier

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// EmailSender delivers via SMTP. Config shape:
//
//	{
//	  "host": "smtp.example.com",
//	  "port": 587,
//	  "username": "alerts@example.com",
//	  "password": "...",          // plaintext (operator-managed)
//	  "from": "alerts@example.com",
//	  "to": "ops@example.com",
//	  "starttls": true            // optional, default true on port 587
//	}
//
// STARTTLS is the default for port 587; plain port 25 or implicit
// TLS on 465 are also supported. Username/password are passed via
// PLAIN auth so any standards-compliant SMTP server works.
//
// Future enhancement: pull the password from the vault by
// password_credential_id rather than a plaintext field. Wired the
// shape but kept the plaintext path so a fresh install does not need
// the vault.
type EmailSender struct {
	// dial is the test seam; nil means use the default net dialer.
	dial func(network, addr string, timeout time.Duration) (net.Conn, error)
}

// NewEmailSender returns a sender backed by the stdlib net/smtp client.
func NewEmailSender() *EmailSender {
	return &EmailSender{}
}

// Kind implements Sender.
func (s *EmailSender) Kind() string { return "email_smtp" }

// Send delivers one message. Returns the SMTP server's error message
// verbatim on auth or recipient failure so the operator can debug
// from the dashboard log line.
func (s *EmailSender) Send(ctx context.Context, cfg json.RawMessage, ev Event) error {
	var c struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		From     string `json:"from"`
		To       string `json:"to"`
		StartTLS *bool  `json:"starttls"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return fmt.Errorf("email: parse config: %w", err)
	}
	if c.Host == "" || c.From == "" || c.To == "" {
		return fmt.Errorf("email: host, from, to are required")
	}
	if c.Port == 0 {
		c.Port = 587
	}
	addr := net.JoinHostPort(c.Host, fmt.Sprintf("%d", c.Port))

	dialer := s.dial
	if dialer == nil {
		dialer = func(network, addr string, timeout time.Duration) (net.Conn, error) {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, network, addr)
		}
	}

	deadline, ok := ctx.Deadline()
	timeout := 10 * time.Second
	if ok {
		if rem := time.Until(deadline); rem > 0 {
			timeout = rem
		}
	}

	conn, err := dialer("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("email: dial %s: %w", addr, err)
	}
	defer conn.Close()
	// Bound the WHOLE SMTP exchange, not just the dial. Without a
	// connection deadline a server that accepts TCP then stalls (e.g. a
	// slowloris) would block NewClient/STARTTLS/Auth/Data forever and
	// hang the dispatcher's graceful Drain. SetDeadline covers every
	// subsequent read+write on this conn.
	_ = conn.SetDeadline(time.Now().Add(timeout))

	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return fmt.Errorf("email: smtp.NewClient: %w", err)
	}
	defer client.Quit()

	wantSTARTTLS := c.Port == 587
	if c.StartTLS != nil {
		wantSTARTTLS = *c.StartTLS
	}
	if wantSTARTTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			tlsCfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
			if err := client.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("email: STARTTLS: %w", err)
			}
		}
	}
	if c.Username != "" {
		auth := smtp.PlainAuth("", c.Username, c.Password, c.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("email: auth: %w", err)
		}
	}
	if err := client.Mail(c.From); err != nil {
		return fmt.Errorf("email: MAIL FROM: %w", err)
	}
	for _, rcpt := range splitRecipients(c.To) {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("email: RCPT TO %s: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("email: DATA: %w", err)
	}
	if _, err := w.Write([]byte(emailMessage(c.From, c.To, ev))); err != nil {
		w.Close()
		return fmt.Errorf("email: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: close body: %w", err)
	}
	return nil
}

func splitRecipients(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func emailMessage(from, to string, ev Event) string {
	subject := alertHeadline(ev)
	body := alertPlainText(ev)
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}
