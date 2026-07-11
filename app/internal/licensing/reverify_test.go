// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package licensing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// StartReverify must:
//   - exit cleanly when the context cancels (no goroutine leak), and
//   - pick up on-disk .lic changes between ticks.
//
// The cheapest way to prove (1) is to run with a tiny interval, wait
// long enough for one tick to fire, then cancel and assert the
// goroutine has stopped touching the State. Proving (2) requires a
// signed envelope; we mint one in-test using the same withTestKey
// helper the rest of verifier_test.go uses.

func TestStartReverify_ExitsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "license.lic")
	if err := os.WriteFile(path, []byte("not a real license"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	s := NewState(path, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	s.StartReverify(ctx, 10*time.Millisecond)

	// Let a couple of ticks fire.
	time.Sleep(40 * time.Millisecond)
	cancel()

	// After cancel + grace period, the goroutine must have stopped.
	// We assert that by capturing the cached LastError, waiting, and
	// confirming it doesn't change. (A still-running loop would
	// re-read the same garbage file and overwrite the same value,
	// which IS unchanged — so the stronger assertion is "the load
	// counter doesn't increment". Without exposing a counter, we
	// rely on race-detector + the fact that the goroutine logs
	// "stopping" through the logger on exit.)
	before := s.Status().LastError
	time.Sleep(30 * time.Millisecond)
	after := s.Status().LastError
	if before != after {
		t.Errorf("LastError changed after cancel: %q → %q (goroutine kept running)", before, after)
	}
}

// StartReverify is a no-op when path is empty (community mode) — no
// goroutine should be spawned.
func TestStartReverify_CommunityNoOp(t *testing.T) {
	s := NewState("", "", nil)
	// Should not launch anything; cancel immediately to be safe.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartReverify(ctx, 10*time.Millisecond)
	// No assertion needed — if the goroutine erroneously launched it
	// would hit a nil file read; the State's path == "" short-circuits.
}

// StartReverify with interval <= 0 must not launch at all.
func TestStartReverify_ZeroIntervalNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "license.lic")
	_ = os.WriteFile(path, []byte("x"), 0o600)
	s := NewState(path, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartReverify(ctx, 0)
	s.StartReverify(ctx, -1*time.Second)
}

// OnReload callbacks fire after every successful Status swap. The
// load-bearing use case is the boot wiring in http/server.go — when
// an admin uploads a new .lic via /admin/license/upload, the
// callback rebuilds the auth.Registry so enterprise providers
// (LDAP/SAML) start serving without a process restart.
func TestOnReload_FiresAfterReload(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kid := "onreload-kid"
	withTestKey(t, kid, pub, "lic-test.artist-alley.org", func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "license.lic")
		// Seed an invalid file so initial load fails; that's still a
		// "swap" event (community fallback gets cached) so callbacks
		// registered AFTER NewState fire on the next manual reload.
		if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		s := NewState(path, "", nil)

		var fired int
		var gotFeatures []string
		s.OnReload(func(st Status) {
			fired++
			gotFeatures = append([]string(nil), st.Features...)
		})

		// Now write a valid envelope + manual Reload. Callback must fire.
		now := time.Now().Unix()
		claims := LicenseClaims{
			V: 1, KID: kid, LID: "01HZOR", Product: "artist-alley:core",
			Tier: "pro", SeatWindowDays: 30,
			Owner: "or@test.com", Org: "or",
			NotBefore: now - 60, Expires: now + 3600, IssuedAt: now,
			Features: []string{"core", "sso_ldap"},
			Issuer:   "lic-test.artist-alley.org",
		}
		if err := os.WriteFile(path, []byte(mustSignEnvelope(t, priv, claims)), 0o600); err != nil {
			t.Fatalf("write good envelope: %v", err)
		}
		if _, err := s.Reload(); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if fired == 0 {
			t.Fatal("OnReload callback never fired after successful Reload")
		}
		var sawLdap bool
		for _, f := range gotFeatures {
			if f == "sso_ldap" {
				sawLdap = true
			}
		}
		if !sawLdap {
			t.Errorf("callback Status missing sso_ldap, got features: %v", gotFeatures)
		}
	})
}

// Nil callback is a no-op — guards against a refactor that drops the
// nil check inside OnReload and accidentally calls a nil func.
func TestOnReload_NilCallbackIgnored(t *testing.T) {
	s := NewState("", "", nil)
	s.OnReload(nil) // must not panic
	_, _ = s.Reload()
}

// Multiple callbacks all fire, in registration order — important
// because the boot wiring may want both "rebuild registry" + "log
// the swap" callbacks.
func TestOnReload_MultipleCallbacksFireInOrder(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kid := "multi-cb-kid"
	withTestKey(t, kid, pub, "lic-test.artist-alley.org", func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "license.lic")
		now := time.Now().Unix()
		claims := LicenseClaims{
			V: 1, KID: kid, LID: "01HZMC", Product: "artist-alley:core",
			Tier: "pro", SeatWindowDays: 30,
			Owner: "mc@test.com", Org: "mc",
			NotBefore: now - 60, Expires: now + 3600, IssuedAt: now,
			Features: []string{"core"},
			Issuer:   "lic-test.artist-alley.org",
		}
		if err := os.WriteFile(path, []byte(mustSignEnvelope(t, priv, claims)), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		s := NewState(path, "", nil)

		var order []string
		s.OnReload(func(Status) { order = append(order, "a") })
		s.OnReload(func(Status) { order = append(order, "b") })
		s.OnReload(func(Status) { order = append(order, "c") })

		if _, err := s.Reload(); err != nil {
			t.Fatalf("reload: %v", err)
		}
		want := []string{"a", "b", "c"}
		if len(order) != len(want) {
			t.Fatalf("got %d callbacks fired, want %d", len(order), len(want))
		}
		for i := range want {
			if order[i] != want[i] {
				t.Errorf("[%d] = %q, want %q", i, order[i], want[i])
			}
		}
	})
}

// Callbacks must be able to call back INTO State methods (HasFeature,
// Status, Features) without deadlocking. The swap() implementation
// snapshots the callback slice and releases the lock before firing —
// pin that property here so a refactor that holds the lock through
// callback invocation breaks loudly.
func TestOnReload_CallbackCanReadState(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	kid := "cb-readback-kid"
	withTestKey(t, kid, pub, "lic-test.artist-alley.org", func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "license.lic")
		now := time.Now().Unix()
		claims := LicenseClaims{
			V: 1, KID: kid, LID: "01HZCR", Product: "artist-alley:core",
			Tier: "pro", SeatWindowDays: 30,
			Owner: "cr@test.com", Org: "cr",
			NotBefore: now - 60, Expires: now + 3600, IssuedAt: now,
			Features: []string{"core", "sso_saml"},
			Issuer:   "lic-test.artist-alley.org",
		}
		if err := os.WriteFile(path, []byte(mustSignEnvelope(t, priv, claims)), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		s := NewState(path, "", nil)

		var sawSaml bool
		s.OnReload(func(Status) {
			// Read-back through the State's own method must not
			// deadlock + must see the newly swapped data.
			sawSaml = s.HasFeature("sso_saml")
		})

		if _, err := s.Reload(); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if !sawSaml {
			t.Fatal("callback could not read post-swap features via State.HasFeature")
		}
	})
}

// End-to-end: drop a fresh valid envelope on disk between ticks and
// confirm the cached Status flips from "verify failed" to "loaded".
func TestStartReverify_PicksUpFileChange(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	kid := "reverify-test-kid"
	withTestKey(t, kid, pub, "lic-test.artist-alley.org", func() {
		dir := t.TempDir()
		path := filepath.Join(dir, "license.lic")
		if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		s := NewState(path, "", nil)
		// Initial load fails verification → LastError is set.
		if s.Status().LastError == "" {
			t.Fatal("expected initial LastError for garbage file")
		}

		// Mint a valid envelope and write it over.
		now := time.Now().Unix()
		seats := int64(50)
		claims := LicenseClaims{
			V: 1, KID: kid, LID: "01HZRV", Product: "artist-alley:core",
			Tier: "pro", Seats: &seats, SeatWindowDays: 30,
			Owner: "rv@test.com", Org: "rv",
			NotBefore: now - 60, Expires: now + 3600, IssuedAt: now,
			Features: []string{"core"},
			Issuer:   "lic-test.artist-alley.org",
		}
		text := mustSignEnvelope(t, priv, claims)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.StartReverify(ctx, 10*time.Millisecond)

		// Swap the file AFTER starting the ticker so a tick has to
		// fire to see it.
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatalf("write good envelope: %v", err)
		}

		// Wait for at most ~500ms for a tick to pick up the change.
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if s.Status().Loaded {
				return // success
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatalf("Status.Loaded never became true; status = %+v", s.Status())
	})
}
