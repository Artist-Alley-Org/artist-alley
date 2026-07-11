// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package email_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/email"
)

func TestCapture_RecordsMessages(t *testing.T) {
	c := &email.Capture{}
	ctx := context.Background()

	msg := email.Message{
		From: "from@example.com", To: []string{"a@example.com"},
		Subject: "hi", TextBody: "body",
	}
	if err := c.Send(ctx, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := c.Len(); got != 1 {
		t.Errorf("Len = %d, want 1", got)
	}

	last, ok := c.Last()
	if !ok {
		t.Fatal("Last() returned ok=false after Send")
	}
	if last.Subject != "hi" {
		t.Errorf("Last.Subject = %q, want hi", last.Subject)
	}

	// Mutating the original message's slice header after Send must
	// NOT affect the captured copy.
	msg.To[0] = "mutated@example.com"
	if got, _ := c.Last(); got.To[0] == "mutated@example.com" {
		t.Errorf("capture didn't deep-copy To slice; got %v", got.To)
	}

	c.Reset()
	if got := c.Len(); got != 0 {
		t.Errorf("after Reset Len = %d, want 0", got)
	}
}

func TestCapture_RejectsBadInput(t *testing.T) {
	c := &email.Capture{}
	ctx := context.Background()

	cases := []struct {
		name string
		msg  email.Message
		want string
	}{
		{
			name: "no recipients",
			msg:  email.Message{Subject: "x", TextBody: "y"},
			want: "no recipients",
		},
		{
			name: "missing subject",
			msg:  email.Message{To: []string{"a@b.c"}, TextBody: "y"},
			want: "subject",
		},
		{
			name: "missing text body",
			msg:  email.Message{To: []string{"a@b.c"}, Subject: "x"},
			want: "text body",
		},
		{
			name: "bad recipient",
			msg:  email.Message{To: []string{"not-an-email"}, Subject: "x", TextBody: "y"},
			want: "bad recipient",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Send(ctx, tc.msg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestRender_AdminTestTemplate(t *testing.T) {
	msg, err := email.Render(email.TemplateAdminTest, []string{"ops@example.com"}, map[string]any{
		"site_name":      "Studio Alpha",
		"site_url":       "https://art.example.com",
		"recipient_name": "Pat",
		"triggered_by":   "admin@example.com",
		"triggered_at":   "2026-06-24T08:00:00Z",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(msg.Subject, "Studio Alpha") {
		t.Errorf("subject = %q, want contains 'Studio Alpha'", msg.Subject)
	}
	if !strings.Contains(msg.TextBody, "Pat") {
		t.Errorf("text body missing recipient_name: %q", msg.TextBody)
	}
	if !strings.Contains(msg.HTMLBody, "Studio Alpha") {
		t.Errorf("html body missing site_name: %q", msg.HTMLBody)
	}
	if !strings.HasPrefix(strings.TrimSpace(msg.HTMLBody), "<!doctype html>") {
		t.Errorf("html body missing doctype")
	}
}

func TestRender_UnknownTemplate(t *testing.T) {
	_, err := email.Render("does_not_exist", []string{"a@b.c"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown template") {
		t.Errorf("expected unknown-template error, got %v", err)
	}
}

func TestDisabledSender_NoOps(t *testing.T) {
	d := email.DisabledSender{}
	err := d.Send(context.Background(), email.Message{
		To: []string{"a@b.c"}, Subject: "x", TextBody: "y",
	})
	if err != nil {
		t.Errorf("DisabledSender should return nil, got %v", err)
	}
}
