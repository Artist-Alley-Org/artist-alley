package lockout_test

import (
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/auth/lockout"
)

// TestCache_NilReceiver — every method must survive a nil receiver
// so the login-handler's caller can compose without a nil-check.
func TestCache_NilReceiver(t *testing.T) {
	var c *lockout.Cache
	if _, ok := c.Get(1); ok {
		t.Fatalf("nil Get should miss")
	}
	c.Put(1, lockout.CachedState{})
	c.Invalidate(1)
	if n := c.Len(); n != 0 {
		t.Fatalf("nil Len should be 0, got %d", n)
	}
}

// TestCache_NilRegistry — NewCache(nil) returns nil (single-process
// tests skip caching cleanly).
func TestCache_NilRegistry(t *testing.T) {
	c := lockout.NewCache(nil)
	if c != nil {
		t.Fatalf("NewCache(nil) should return nil; got %v", c)
	}
}

// TestCachedState_ExposesCounter — the CachedState struct exposes
// the count + deadline the admin surface needs.
func TestCachedState_ExposesCounter(t *testing.T) {
	deadline := time.Now().Add(10 * time.Minute)
	s := lockout.CachedState{
		FailedCount:  7,
		LockoutUntil: deadline,
	}
	if s.FailedCount != 7 {
		t.Fatalf("FailedCount not exposed")
	}
	if !s.LockoutUntil.Equal(deadline) {
		t.Fatalf("LockoutUntil not exposed")
	}
}
