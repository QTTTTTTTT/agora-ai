// Package mailer renders + delivers transactional emails for the
// platform. Sprint 2A introduces three flavours (verification code,
// password reset link, security notice); the production wiring uses
// a plain SMTP submission relay (works with Aliyun DM / Tencent SES
// / SendGrid / MailHog dev) so we don't pull in a third-party SaaS
// SDK.
//
// All templates are bilingual (zh-CN + en-US) so a user who toggled
// language preference still receives a message they can read; the
// dominant locale (zh first) is configurable per-deployment.
//
// The interface is intentionally small (one Send method) so tests can
// substitute an in-memory recorder without touching the SMTP plumbing.
package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Message is the high-level outbound envelope. The mailer renders the
// final MIME body (multipart/alternative with plain + HTML) before
// handing the bytes to the transport.
type Message struct {
	To       string
	Subject  string
	HTMLBody string
	TextBody string
}

// Mailer is the dependency the rest of the codebase pins to. Tests
// inject a recorder; production wires SMTPSender.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// Config captures everything we need to dial out to an SMTP relay.
// Sprint 2A's deployment doc points operators at Aliyun DM / Tencent
// SES (high deliverability in mainland China) or MailHog for dev.
type Config struct {
	Host      string
	Port      int
	Username  string
	Password  string
	From      string
	FromName  string
	UseTLS    bool
	StartTLS  bool
	Timeout   time.Duration
	AppURL    string
	BrandName string
}

// Enabled reports whether the mailer has the minimum information
// (host + from address) to actually try delivery. The handler layer
// checks this so it can fall back gracefully (e.g. log the reset link
// in dev) when SMTP is unconfigured.
func (c Config) Enabled() bool {
	return strings.TrimSpace(c.Host) != "" && strings.TrimSpace(c.From) != ""
}

// SMTPSender talks to a relay via stdlib net/smtp. It deliberately
// stays minimal: most relays accept PLAIN auth on submission ports
// (587 STARTTLS or 465 implicit TLS) so we only support those two
// modes plus an explicit unauthenticated path for MailHog.
type SMTPSender struct {
	cfg Config
}

// NewSMTPSender returns a sender; nil if SMTP is not configured.
func NewSMTPSender(cfg Config) *SMTPSender {
	if !cfg.Enabled() {
		return nil
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Port == 0 {
		if cfg.UseTLS {
			cfg.Port = 465
		} else {
			cfg.Port = 587
		}
	}
	if strings.TrimSpace(cfg.FromName) == "" {
		cfg.FromName = "FundAI"
	}
	if strings.TrimSpace(cfg.BrandName) == "" {
		cfg.BrandName = "FundAI"
	}
	return &SMTPSender{cfg: cfg}
}

// Send renders the MIME envelope and pushes it through the relay.
// We honour ctx by sealing the dial in a goroutine and aborting if
// the caller cancels (smtp.Dial doesn't accept a Context).
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	if s == nil {
		return errors.New("mailer: not configured")
	}
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	from := s.cfg.From
	body := buildMIME(s.cfg, msg)

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		done <- result{err: s.sendDirect(addr, from, msg.To, body)}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-done:
		return r.err
	}
}

func (s *SMTPSender) sendDirect(addr, from, to, body string) error {
	dialer := &net.Dialer{Timeout: s.cfg.Timeout}
	var conn net.Conn
	var err error
	if s.cfg.UseTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.cfg.Host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mailer dial %s: %w", addr, err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("mailer client %s: %w", addr, err)
	}
	defer client.Quit()

	if s.cfg.StartTLS && !s.cfg.UseTLS {
		if err := client.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
			return fmt.Errorf("mailer starttls: %w", err)
		}
	}
	if strings.TrimSpace(s.cfg.Username) != "" || strings.TrimSpace(s.cfg.Password) != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("mailer auth: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("mailer mail from %s: %w", from, err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("mailer rcpt %s: %w", to, err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mailer data: %w", err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		return fmt.Errorf("mailer write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer close: %w", err)
	}
	return nil
}

// RecordedMessage is used by tests and the dev fallback to capture
// what would have been sent without actually contacting a relay.
type RecordedMessage struct {
	Message
	SentAt time.Time
}

// Recorder is a Mailer that just records calls in memory. Useful for
// tests and for the dev mode where SMTP isn't configured (the reset
// link is logged so a developer can click it from the console).
type Recorder struct {
	Messages []RecordedMessage
}

// Send appends the message to the recorder.
func (r *Recorder) Send(_ context.Context, msg Message) error {
	r.Messages = append(r.Messages, RecordedMessage{Message: msg, SentAt: time.Now().UTC()})
	return nil
}

// Last returns the most recently captured message or false.
func (r *Recorder) Last() (RecordedMessage, bool) {
	if len(r.Messages) == 0 {
		return RecordedMessage{}, false
	}
	return r.Messages[len(r.Messages)-1], true
}

// Reset clears the recorded buffer.
func (r *Recorder) Reset() {
	r.Messages = nil
}
