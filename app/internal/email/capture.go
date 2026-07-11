// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package email

import (
	"context"
	"sync"
)

// Capture is an in-memory [Sender]. Tests use it as a fake;
// production boot wires it when AA_EMAIL_MODE=capture so dev /
// staging stacks can exercise notification flows without
// configuring a real SMTP relay. The mode-pick path logs a WARN
// outside of test environments — see [SenderFromEnv] for the
// boot-time selection logic.
type Capture struct {
	mu   sync.Mutex
	sent []Message
}

// Send records the message + returns nil. Returns
// [ErrNoRecipients] when To is empty so tests catch upstream bugs
// the same way SMTPSender would.
func (c *Capture) Send(_ context.Context, msg Message) error {
	if err := validateBasic(msg); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Defensive copy so caller-side mutation of the slice headers
	// after Send doesn't retroactively edit the recording.
	cp := msg
	cp.To = append([]string(nil), msg.To...)
	c.sent = append(c.sent, cp)
	return nil
}

// All returns a copy of every captured message. Safe to mutate.
func (c *Capture) All() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, len(c.sent))
	copy(out, c.sent)
	return out
}

// Last returns the most-recently captured message and ok=false if
// nothing's been sent yet. Common test assertion.
func (c *Capture) Last() (Message, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return Message{}, false
	}
	return c.sent[len(c.sent)-1], true
}

// Len returns the number of captured messages.
func (c *Capture) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// Reset drops every captured message. Tests call this between
// cases; production capture mode doesn't.
func (c *Capture) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = nil
}
