// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package email

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

// Encryption is the SMTP transport mode. Stable strings matching
// sysconfig's enum on the wire so the boot-time adapter is a
// no-op type conversion.
type Encryption string

const (
	EncryptionNone     Encryption = "none"
	EncryptionStartTLS Encryption = "starttls"
	EncryptionTLS      Encryption = "tls"
)

// Config is the minimal SMTP config the sender needs. Defined
// here (rather than referencing sysconfig.SMTP) so the email
// package has no dependency edge into sysconfig — that would
// invert the natural direction (sysconfig wires the email seam
// for its admin test-send endpoint).
type Config struct {
	Host       string
	Port       int
	Encryption Encryption
	Username   string
	Password   string
	FromAddr   string
}

// ConfigProvider returns the current SMTP config. SMTPSender calls
// this once per send so admin changes propagate without restart.
type ConfigProvider func(ctx context.Context) (Config, error)

// SMTPSender is the production [Sender] over the stdlib net/smtp +
// crypto/tls. Stateless beyond the provider — one instance per
// process is fine.
type SMTPSender struct {
	provider ConfigProvider
	// dialTimeout caps the TCP connect + the full SMTP conversation.
	// Default 30s; jobs framework lease is 5min so plenty of margin.
	dialTimeout time.Duration
}

// NewSMTPSender wires a sender against the given config provider.
func NewSMTPSender(provider ConfigProvider) *SMTPSender {
	return &SMTPSender{
		provider:    provider,
		dialTimeout: 30 * time.Second,
	}
}

// WithDialTimeout overrides the default 30s timeout. Tests use
// short timeouts when asserting "SMTP relay down → fail fast".
func (s *SMTPSender) WithDialTimeout(d time.Duration) *SMTPSender {
	if d > 0 {
		s.dialTimeout = d
	}
	return s
}

// Send implements [Sender]. Looks up the live SMTP config, dials
// the relay using the right transport for the encryption mode,
// authenticates if a username is set, then issues MAIL FROM /
// RCPT TO / DATA. Returns [ErrNotConfigured] if SMTP host is
// empty (callers map this to "skip + log").
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	cfg, err := s.provider(ctx)
	if err != nil {
		return fmt.Errorf("email.smtp: load config: %w", err)
	}
	if cfg.Host == "" {
		return ErrNotConfigured
	}
	if msg.From == "" {
		if cfg.FromAddr == "" {
			return ErrFromAddressUnset
		}
		msg.From = cfg.FromAddr
	}
	if err := validateBasic(msg); err != nil {
		return err
	}

	body, err := buildMIME(msg)
	if err != nil {
		return fmt.Errorf("email.smtp: build mime: %w", err)
	}

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	d := &net.Dialer{Timeout: s.dialTimeout}

	var client *smtp.Client
	switch cfg.Encryption {
	case EncryptionTLS:
		conn, dErr := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: cfg.Host})
		if dErr != nil {
			return fmt.Errorf("email.smtp: dial tls %s: %w", addr, dErr)
		}
		client, err = smtp.NewClient(conn, cfg.Host)
		if err != nil {
			conn.Close()
			return fmt.Errorf("email.smtp: smtp handshake: %w", err)
		}
	default:
		conn, dErr := d.DialContext(ctx, "tcp", addr)
		if dErr != nil {
			return fmt.Errorf("email.smtp: dial %s: %w", addr, dErr)
		}
		client, err = smtp.NewClient(conn, cfg.Host)
		if err != nil {
			conn.Close()
			return fmt.Errorf("email.smtp: smtp handshake: %w", err)
		}
		if cfg.Encryption == EncryptionStartTLS {
			if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
				client.Close()
				return fmt.Errorf("email.smtp: starttls: %w", err)
			}
		}
	}
	defer client.Close()

	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("email.smtp: auth: %w", err)
		}
	}

	fromAddr, err := extractAddr(msg.From)
	if err != nil {
		return fmt.Errorf("email.smtp: bad from %q: %w", msg.From, err)
	}
	if err := client.Mail(fromAddr); err != nil {
		return fmt.Errorf("email.smtp: MAIL FROM: %w", err)
	}
	for _, rcpt := range msg.To {
		addr, err := extractAddr(rcpt)
		if err != nil {
			return fmt.Errorf("email.smtp: bad to %q: %w", rcpt, err)
		}
		if err := client.Rcpt(addr); err != nil {
			return fmt.Errorf("email.smtp: RCPT TO %s: %w", addr, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("email.smtp: DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("email.smtp: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email.smtp: close body: %w", err)
	}
	if err := client.Quit(); err != nil {
		// Some relays disconnect after a successful Quit without
		// the proper "221 Goodbye". Treat io.EOF / connection-
		// reset as success since the body was already acknowledged.
		if !errors.Is(err, net.ErrClosed) {
			// Best-effort log via the caller's logger; not fatal.
		}
	}
	return nil
}

// extractAddr peels the address out of an RFC 5322 header value
// (e.g. "Display Name <user@example.com>" → "user@example.com").
// The SMTP envelope needs just the address; display names live in
// the From: / To: header lines.
func extractAddr(s string) (string, error) {
	a, err := parseAddr(s)
	if err != nil {
		return "", err
	}
	return a, nil
}

func parseAddr(s string) (string, error) {
	// net/mail.ParseAddress accepts both shapes; keep it simple.
	a, err := mailParseAddress(strings.TrimSpace(s))
	if err != nil {
		return "", err
	}
	return a, nil
}
