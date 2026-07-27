// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package email_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/email"
)

// fakeSMTPRelay is a minimal in-process SMTP-over-plaintext server
// just smart enough to drive the [email.SMTPSender] path end-to-end.
// Captures the raw conversation so tests can assert MAIL FROM /
// RCPT TO + the DATA payload landed correctly.
type fakeSMTPRelay struct {
	listener net.Listener
	done     chan struct{}

	mu       sync.Mutex
	captured []capturedSession
}

type capturedSession struct {
	mailFrom  string
	rcptTo    []string
	body      string
	authPlain string
}

func newFakeSMTPRelay(t *testing.T) *fakeSMTPRelay {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	r := &fakeSMTPRelay{listener: l, done: make(chan struct{})}
	go r.acceptLoop()
	t.Cleanup(func() {
		l.Close()
		<-r.done
	})
	return r
}

func (r *fakeSMTPRelay) Addr() string { return r.listener.Addr().String() }

func (r *fakeSMTPRelay) Captured() []capturedSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedSession, len(r.captured))
	copy(out, r.captured)
	return out
}

func (r *fakeSMTPRelay) acceptLoop() {
	defer close(r.done)
	for {
		c, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.handle(c)
	}
}

// handle drives ONE SMTP session. Minimalist: only the verbs the
// SMTPSender uses, replied with 2xx. AUTH PLAIN is captured for
// assertions but not validated.
func (r *fakeSMTPRelay) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	bw := bufio.NewWriter(c)
	write := func(s string) {
		_, _ = bw.WriteString(s + "\r\n")
		_ = bw.Flush()
	}
	write("220 fake.smtp.test ESMTP")

	var sess capturedSession
	inData := false
	var bodyBuf strings.Builder
	for {
		line, err := br.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				write("250 OK queued")
				inData = false
				sess.body = bodyBuf.String()
				r.mu.Lock()
				r.captured = append(r.captured, sess)
				r.mu.Unlock()
				sess = capturedSession{}
				bodyBuf.Reset()
				continue
			}
			bodyBuf.WriteString(line)
			bodyBuf.WriteString("\r\n")
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO") || strings.HasPrefix(upper, "HELO"):
			write("250-fake.smtp.test\r\n250 AUTH PLAIN")
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			sess.authPlain = strings.TrimSpace(strings.TrimPrefix(line, "AUTH PLAIN"))
			if sess.authPlain == "" {
				// Two-step form — read next line for credentials.
				cred, _ := br.ReadString('\n')
				sess.authPlain = strings.TrimRight(cred, "\r\n")
			}
			write("235 OK auth")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			sess.mailFrom = extractAngled(line[len("MAIL FROM:"):])
			write("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			sess.rcptTo = append(sess.rcptTo, extractAngled(line[len("RCPT TO:"):]))
			write("250 OK")
		case upper == "DATA":
			write("354 send body")
			inData = true
		case upper == "QUIT":
			write("221 bye")
			return
		case upper == "RSET":
			sess = capturedSession{}
			bodyBuf.Reset()
			write("250 OK")
		case upper == "NOOP":
			write("250 OK")
		default:
			write("250 OK")
		}
	}
}

func extractAngled(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '<'); i >= 0 {
		if j := strings.IndexByte(s[i:], '>'); j > 0 {
			return s[i+1 : i+j]
		}
	}
	return s
}

func TestSMTPSender_DeliversPlainNoAuth(t *testing.T) {
	relay := newFakeSMTPRelay(t)
	host, port := splitHostPort(t, relay.Addr())

	provider := func(_ context.Context) (email.Config, error) {
		return email.Config{
			Host: host, Port: port,
			Encryption: email.EncryptionNone,
			FromAddr:   "ops@example.com",
		}, nil
	}
	sender := email.NewSMTPSender(provider).WithDialTimeout(2 * time.Second)
	ctx := t.Context()

	err := sender.Send(ctx, email.Message{
		To:       []string{"alice@example.com", "bob@example.com"},
		Subject:  "hi",
		TextBody: "hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	caps := relay.Captured()
	if len(caps) != 1 {
		t.Fatalf("captured %d sessions, want 1", len(caps))
	}
	if caps[0].mailFrom != "ops@example.com" {
		t.Errorf("MAIL FROM = %q, want ops@example.com", caps[0].mailFrom)
	}
	if len(caps[0].rcptTo) != 2 {
		t.Errorf("RCPT TO count = %d, want 2 (%+v)", len(caps[0].rcptTo), caps[0].rcptTo)
	}
	if !strings.Contains(caps[0].body, "Subject: hi") {
		t.Errorf("body missing Subject header:\n%s", caps[0].body)
	}
	if !strings.Contains(caps[0].body, "hello") {
		t.Errorf("body missing text content:\n%s", caps[0].body)
	}
	if caps[0].authPlain != "" {
		t.Errorf("did not expect AUTH PLAIN when username is empty, got %q", caps[0].authPlain)
	}
}

func TestSMTPSender_AuthPlainWhenUsernameSet(t *testing.T) {
	relay := newFakeSMTPRelay(t)
	host, port := splitHostPort(t, relay.Addr())

	provider := func(_ context.Context) (email.Config, error) {
		return email.Config{
			Host: host, Port: port,
			Encryption: email.EncryptionNone,
			Username:   "alice", Password: "s3cret",
			FromAddr: "ops@example.com",
		}, nil
	}
	sender := email.NewSMTPSender(provider).WithDialTimeout(2 * time.Second)
	ctx := context.Background()
	err := sender.Send(ctx, email.Message{
		To: []string{"r@e.c"}, Subject: "x", TextBody: "y",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	caps := relay.Captured()
	if len(caps) != 1 || caps[0].authPlain == "" {
		t.Fatalf("expected AUTH PLAIN payload, got %+v", caps)
	}
}

func TestSMTPSender_ErrNotConfigured(t *testing.T) {
	provider := func(_ context.Context) (email.Config, error) {
		return email.Config{}, nil
	}
	sender := email.NewSMTPSender(provider)
	err := sender.Send(context.Background(), email.Message{
		To: []string{"r@e.c"}, Subject: "x", TextBody: "y",
	})
	if !errors.Is(err, email.ErrNotConfigured) {
		t.Errorf("Send w/ empty host = %v, want ErrNotConfigured", err)
	}
}

func TestSMTPSender_DefaultsFromAddrFromConfig(t *testing.T) {
	relay := newFakeSMTPRelay(t)
	host, port := splitHostPort(t, relay.Addr())

	provider := func(_ context.Context) (email.Config, error) {
		return email.Config{
			Host: host, Port: port,
			Encryption: email.EncryptionNone,
			FromAddr:   "default-from@example.com",
		}, nil
	}
	sender := email.NewSMTPSender(provider).WithDialTimeout(2 * time.Second)

	// Send a Message with empty From — config FromAddr should be used.
	if err := sender.Send(context.Background(), email.Message{
		To: []string{"r@e.c"}, Subject: "x", TextBody: "y",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	caps := relay.Captured()
	if len(caps) != 1 || caps[0].mailFrom != "default-from@example.com" {
		t.Fatalf("MAIL FROM did not default to config; captured %+v", caps)
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return host, port
}
