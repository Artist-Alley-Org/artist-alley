package auth

import (
	"testing"
	"time"
)

// TestLoginLimiter_BurstThenDeny exercises the bucket-emptying path:
// the first `burst` attempts succeed, then attempts are rejected until
// at least one refill window has elapsed.
func TestLoginLimiter_BurstThenDeny(t *testing.T) {
	t.Parallel()
	l := NewLoginLimiter()
	const key = "ip:198.51.100.1"
	for i := 0; i < l.burst; i++ {
		if !l.Allow(key) {
			t.Fatalf("attempt %d should be allowed within burst (%d)", i, l.burst)
		}
	}
	if l.Allow(key) {
		t.Fatal("attempt past burst should be denied")
	}
}

// TestLoginLimiter_Forget clears the bucket so a legitimate user
// recovering from a fat-finger isn't penalised.
func TestLoginLimiter_Forget(t *testing.T) {
	t.Parallel()
	l := NewLoginLimiter()
	const key = "user:alice"
	for i := 0; i < l.burst; i++ {
		l.Allow(key)
	}
	if l.Allow(key) {
		t.Fatal("bucket should be empty before Forget")
	}
	l.Forget(key)
	if !l.Allow(key) {
		t.Fatal("Forget should reset the bucket to full")
	}
}

// TestLoginLimiter_Refill simulates time passing by manipulating the
// entry's lastFill directly. Avoids real sleeps so the test stays fast.
func TestLoginLimiter_Refill(t *testing.T) {
	t.Parallel()
	l := NewLoginLimiter()
	const key = "ip:203.0.113.5"
	for i := 0; i < l.burst; i++ {
		l.Allow(key)
	}
	if l.Allow(key) {
		t.Fatal("bucket should be empty")
	}

	// Rewind lastFill by enough that two tokens have refilled.
	l.mu.Lock()
	entry := l.buckets[key].Value.(*limiterEntry)
	entry.lastFill = time.Now().Add(-2 * l.refill)
	l.mu.Unlock()

	if !l.Allow(key) {
		t.Fatal("after refill, at least one token should be available")
	}
}

// TestLoginLimiter_KeysAreIndependent confirms an exhausted IP key
// doesn't leak into a separate username key.
func TestLoginLimiter_KeysAreIndependent(t *testing.T) {
	t.Parallel()
	l := NewLoginLimiter()
	for i := 0; i < l.burst; i++ {
		l.Allow("ip:192.0.2.99")
	}
	if !l.Allow("user:bob") {
		t.Fatal("a different key should not be affected by ip:192.0.2.99 exhaustion")
	}
}

// TestLoginLimiter_LRUEviction confirms the limiter's capacity bound:
// when capacity unique keys are pushed in, the oldest is evicted, so
// it gets a fresh bucket on next use.
func TestLoginLimiter_LRUEviction(t *testing.T) {
	t.Parallel()
	l := NewLoginLimiter()
	l.capacity = 4 // shrink for the test

	for i := 0; i < l.capacity; i++ {
		l.Allow(keyN(i))
	}
	// Pressure the oldest entry (keyN(0)) out by inserting two
	// brand-new keys.
	l.Allow(keyN(100))
	l.Allow(keyN(101))

	l.mu.Lock()
	_, stillThere := l.buckets[keyN(0)]
	count := l.lru.Len()
	l.mu.Unlock()
	if stillThere {
		t.Errorf("oldest key should have been evicted at capacity")
	}
	if count > l.capacity {
		t.Errorf("lru length=%d should be <= capacity=%d", count, l.capacity)
	}
}

func keyN(n int) string {
	return "k:" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}
