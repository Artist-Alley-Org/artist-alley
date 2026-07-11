// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package email

import (
	"strings"
	"testing"
	"time"
)

const testKey = "test-scramble-key-0123456789abcdef"

func TestUnsubscribe_ValidToken_RoundTrips(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok := SignUnsubscribe(testKey, 42, "mention_of_me", now)
	ref, topic, err := VerifyUnsubscribe(testKey, tok, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ref != 42 || topic != "mention_of_me" {
		t.Fatalf("got (%d, %q), want (42, mention_of_me)", ref, topic)
	}
}

func TestUnsubscribe_AllTopicToken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok := SignUnsubscribe(testKey, 7, "__all__", now)
	ref, topic, err := VerifyUnsubscribe(testKey, tok, now)
	if err != nil || ref != 7 || topic != "__all__" {
		t.Fatalf("got (%d, %q, %v), want (7, __all__, nil)", ref, topic, err)
	}
}

func TestUnsubscribe_ExpiredToken_Rejected(t *testing.T) {
	minted := time.Unix(1_700_000_000, 0)
	tok := SignUnsubscribe(testKey, 42, "mention_of_me", minted)
	// Verify well past the TTL.
	later := minted.Add(UnsubscribeTTL + time.Hour)
	if _, _, err := VerifyUnsubscribe(testKey, tok, later); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestUnsubscribe_TamperedToken_Rejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok := SignUnsubscribe(testKey, 42, "mention_of_me", now)

	// Flip a byte in the payload segment.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape %q", tok)
	}
	tampered := parts[0] + "." + mangle(parts[1]) + "." + parts[2]
	if _, _, err := VerifyUnsubscribe(testKey, tampered, now); err == nil {
		t.Fatal("expected tampered payload to be rejected")
	}

	// Wrong key rejects too.
	if _, _, err := VerifyUnsubscribe("different-key", tok, now); err == nil {
		t.Fatal("expected wrong-key verification to fail")
	}
}

func TestUnsubscribe_Malformed_Rejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, bad := range []string{"", "garbage", "v1.only-two", "v2.a.b", "v1..", "v1.a.b.c"} {
		if _, _, err := VerifyUnsubscribe(testKey, bad, now); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}

func TestUnsubscribeHeaders_RFC8058Shape(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok := SignUnsubscribe(testKey, 1, "__all__", now)
	h := UnsubscribeHeaders("https://art.example.com/", tok)
	lu := h["List-Unsubscribe"]
	if !strings.HasPrefix(lu, "<https://art.example.com/api/v1/unsubscribe?token=") || !strings.HasSuffix(lu, ">") {
		t.Fatalf("List-Unsubscribe malformed: %q", lu)
	}
	if h["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Fatalf("List-Unsubscribe-Post = %q", h["List-Unsubscribe-Post"])
	}
}

func mangle(s string) string {
	b := []byte(s)
	if len(b) == 0 {
		return "x"
	}
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}
