// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package email is the outgoing-mail substrate for transactional
// notifications (password reset, comment alerts, license expiry,
// admin test-sends, etc.). Three pieces:
//
//   - [Message] + [Sender] — the typed envelope + the I/O seam every
//     caller programs against. Sender is small on purpose so test
//     stubs are one-line: a recorder, a faulty one, a slow one.
//
//   - [SMTPSender] — production impl. Reads SMTP config from
//     sysconfig on every send (so admin changes take effect without
//     a restart). Supports plain / STARTTLS / TLS via stdlib
//     net/smtp + crypto/tls — zero non-stdlib deps.
//
//   - [Capture] — in-memory recorder. Tests use it as a fake
//     Sender; ops use it via boot mode (AA_EMAIL_MODE=capture) to
//     freeze outbound mail in dev/staging without configuring an
//     SMTP relay. Mode selection lives in [SenderFromEnv].
//
// Templates ride alongside via the [Render] entry point — see
// templates.go for the registry + the embedded .tmpl files.
package email

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// Message is one outbound email. Construct via [Render] for
// template-backed sends; construct directly for the admin-test
// path that crafts its own subject + body.
type Message struct {
	// From — defaults to the SMTP config's from_address when
	// empty. Production callers leave it empty; the test path
	// can override for "send as admin" investigations.
	From string

	// To — at least one recipient required.
	To []string

	// Subject — single line; the SMTP sender wraps it in a UTF-8
	// MIME-word if it carries non-ASCII characters.
	Subject string

	// TextBody — required (plaintext alternative). RFC 2046
	// + the operator-mail convention: every HTML mail carries a
	// text/plain twin so terminal mail clients still read it.
	TextBody string

	// HTMLBody — optional. When set the message is multipart/
	// alternative; when blank, plain-text only.
	HTMLBody string

	// Headers — extra headers (e.g. List-Unsubscribe, In-Reply-To).
	// The sender always sets From / To / Subject / Date / MIME-Version
	// itself, so don't duplicate them here.
	Headers map[string]string
}

// Sender is the I/O contract every email caller programs against.
// Implementations:
//
//   - [SMTPSender] — production over plain / STARTTLS / TLS.
//   - [Capture]    — in-memory recorder; tests + dev mode.
//   - [DisabledSender] — no-op + WARN log; the safe fallback when
//     boot can't pick a real impl.
type Sender interface {
	// Send delivers msg. Returns nil on success. Errors should be
	// classified by caller: transient network errors are retry-
	// candidates; permanent (bad recipient, auth fail) should be
	// surfaced to the operator via the job framework's
	// [jobs.TerminalError].
	Send(ctx context.Context, msg Message) error
}

// ErrNoRecipients is returned when Send receives a Message with an
// empty To slice. Callers should validate upstream; this is the
// defensive guard at the boundary.
var ErrNoRecipients = errors.New("email: message has no recipients")

// ErrFromAddressUnset is returned when a Message arrives without a
// From and the SMTP config also has no FromAddr. The send path
// refuses rather than guess.
var ErrFromAddressUnset = errors.New("email: no From address (message + SMTP config both empty)")

// ErrNotConfigured is returned by [SMTPSender] when SMTP host is
// empty (the admin hasn't set up email yet). Callers map this to
// "skip + log" rather than a retry — no number of attempts will
// configure the relay.
var ErrNotConfigured = errors.New("email: SMTP host not configured")

// validateBasic returns an error for the most common
// caller-side mistakes before the sender touches network code.
func validateBasic(msg Message) error {
	if len(msg.To) == 0 {
		return ErrNoRecipients
	}
	for _, addr := range msg.To {
		if _, err := mail.ParseAddress(addr); err != nil {
			return fmt.Errorf("email: bad recipient %q: %w", addr, err)
		}
	}
	if strings.TrimSpace(msg.Subject) == "" {
		return errors.New("email: subject is required")
	}
	if strings.TrimSpace(msg.TextBody) == "" {
		return errors.New("email: text body is required (RFC 2046 alternative)")
	}
	return nil
}

// DisabledSender is the no-op fallback the boot wire picks when
// SMTP isn't configured AND capture mode isn't requested. Every
// Send call logs a structured WARN + returns nil so the upstream
// job marks complete (no point retrying a never-going-to-send).
type DisabledSender struct {
	Logger interface {
		Warn(msg string, args ...any)
	}
}

// Send logs + returns nil.
func (d DisabledSender) Send(_ context.Context, msg Message) error {
	if d.Logger != nil {
		d.Logger.Warn("email.disabled.send_skipped",
			"to", strings.Join(msg.To, ","),
			"subject", msg.Subject,
		)
	}
	return nil
}
