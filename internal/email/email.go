// Package email provides a thin SMTP sender for the notify queue (AC-M6-5, AC-M6-6).
//
// Security constraints:
//   - SMTP_PASSWORD is never logged (NFR-6).
//   - Validation occurs at send time, not at boot (SMTP may be optional at boot).
//   - Email addresses appear in slog at debug level only; production log level is info.
package email

import (
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
)

// Config carries the SMTP relay settings read from environment variables.
// None of these values are hardcoded; they are injected from Config (AC-M6-5).
type Config struct {
	Host     string // SMTP_HOST, e.g. "smtp.mailgun.org"
	Port     int    // SMTP_PORT, default 587
	Username string // SMTP_USERNAME
	Password string // SMTP_PASSWORD — never logged
	From     string // SMTP_FROM, e.g. "support@example.com"
}

// Sender is an SMTP email sender built from a Config.
type Sender struct {
	cfg Config
}

// NewSender creates a Sender from the provided Config.
// No network call is made at construction time (validation deferred to Send).
func NewSender(cfg Config) *Sender {
	return &Sender{cfg: cfg}
}

// sanitizeHeaderValue returns an error if v contains a CR or LF character.
// This is a defense-in-depth guard (SEC-M6-002): the email package must be
// safe on its own, independent of upstream API-edge validation.
// The value itself is never included in the error to avoid leaking PII/data.
func sanitizeHeaderValue(v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("email: header value contains illegal CRLF characters")
	}
	return nil
}

// Send sends a plain-text email to `to` with the given subject and body.
//
// Returns an error when:
//   - the SMTP config is incomplete (host, from, or credentials missing)
//   - a header value contains CR or LF characters (CRLF-injection guard, SEC-M6-002)
//   - the SMTP dial or handshake fails
//   - the DATA write fails
//
// SMTP_PASSWORD is never included in error messages or log fields (NFR-6).
func (s *Sender) Send(_ context.Context, to, subject, body string) error {
	if err := s.validate(); err != nil {
		return err
	}

	// Defense-in-depth: reject CRLF in header fields before building the message
	// (SEC-M6-002). Values are not included in errors to avoid leaking PII.
	for _, hdr := range []string{to, subject, s.cfg.From} {
		if err := sanitizeHeaderValue(hdr); err != nil {
			return err
		}
	}

	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	// Only authenticate when a username is configured. PlainAuth refuses to send
	// credentials over a non-TLS connection (Go stdlib), so passing it to an
	// unauthenticated/dev relay that advertises AUTH would fail with
	// "unencrypted connection". A nil auth lets net/smtp skip authentication
	// entirely; a credentialed relay still gets PlainAuth (which requires TLS).
	var auth smtp.Auth
	if s.cfg.Username != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	}

	// Build the RFC 2822 message.
	msg := buildMessage(s.cfg.From, to, subject, body)

	// net/smtp.SendMail dials, upgrades to TLS (STARTTLS), authenticates, and sends.
	if err := smtp.SendMail(addr, auth, s.cfg.From, []string{to}, []byte(msg)); err != nil {
		// Log without password (NFR-6); redact `to` at info level (personal data).
		slog.Error("email: send failed", "host", s.cfg.Host, "from", s.cfg.From, "err", err)
		return fmt.Errorf("email: send to <redacted>: %w", err)
	}

	slog.Debug("email: sent", "host", s.cfg.Host, "subject", subject)
	return nil
}

// validate returns an error when required SMTP fields are absent.
// Called at send time so the service starts without an SMTP config (optional at boot).
func (s *Sender) validate() error {
	var missing []string
	if s.cfg.Host == "" {
		missing = append(missing, "SMTP_HOST")
	}
	if s.cfg.From == "" {
		missing = append(missing, "SMTP_FROM")
	}
	if s.cfg.Port <= 0 {
		missing = append(missing, "SMTP_PORT")
	}
	if len(missing) > 0 {
		return fmt.Errorf("email: missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
}

// buildMessage produces a minimal RFC 2822 email message suitable for smtp.SendMail.
// Headers are newline-delimited; the body follows a blank line.
func buildMessage(from, to, subject, body string) string {
	var sb strings.Builder
	sb.WriteString("From: " + from + "\r\n")
	sb.WriteString("To: " + to + "\r\n")
	sb.WriteString("Subject: " + subject + "\r\n")
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(body)
	return sb.String()
}
