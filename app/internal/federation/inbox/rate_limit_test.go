// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package inbox_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

func TestRateLimiter_FirstRequestAllowed(t *testing.T) {
	l := inbox.NewPeerRateLimiter(10, 10)
	ok, wait := l.Allow(uuid.New())
	if !ok || wait != 0 {
		t.Errorf("first request: ok=%v wait=%v want true/0", ok, wait)
	}
}

func TestRateLimiter_BurstThenLimit(t *testing.T) {
	l := inbox.NewPeerRateLimiter(5, 5) // 5 burst, 5 rps
	peer := uuid.New()
	allowed := 0
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow(peer); ok {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("burst: allowed=%d want 5", allowed)
	}
	// 6th request — bucket empty, refill rate is 5/sec → wait ~1s.
	ok, wait := l.Allow(peer)
	if ok {
		t.Errorf("6th request after burst should be limited")
	}
	if wait < 100*time.Millisecond || wait > 2*time.Second {
		t.Errorf("retry-after suspiciously off: %v", wait)
	}
}

func TestRateLimiter_RefillRestoresCapacity(t *testing.T) {
	l := inbox.NewPeerRateLimiter(2, 100) // 2 burst, 100 rps refill (fast)
	peer := uuid.New()
	l.Allow(peer)
	l.Allow(peer)
	// 3rd → limited
	if ok, _ := l.Allow(peer); ok {
		t.Fatal("3rd should be limited")
	}
	// Wait long enough to refill at least 1 token (10ms at 100rps = 1 token)
	time.Sleep(30 * time.Millisecond)
	if ok, _ := l.Allow(peer); !ok {
		t.Error("after refill window, request should be allowed again")
	}
}

func TestRateLimiter_PerPeerIndependent(t *testing.T) {
	l := inbox.NewPeerRateLimiter(2, 2)
	a := uuid.New()
	b := uuid.New()
	// Exhaust peer A.
	l.Allow(a)
	l.Allow(a)
	if ok, _ := l.Allow(a); ok {
		t.Fatal("A 3rd should be limited")
	}
	// Peer B should still have its own full bucket.
	if ok, _ := l.Allow(b); !ok {
		t.Error("peer B's bucket should be independent of A's")
	}
}

func TestRateLimiter_ConcurrentSafe(t *testing.T) {
	l := inbox.NewPeerRateLimiter(1000, 1000)
	peer := uuid.New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.Allow(peer)
			}
		}()
	}
	wg.Wait()
	// 400 total requests, 1000-token bucket — all allowed; no panic
	// is the assertion. Bucket should have ~600 tokens left.
}

func TestRateLimiter_EvictOlderThan(t *testing.T) {
	l := inbox.NewPeerRateLimiter(10, 10)
	a := uuid.New()
	l.Allow(a)
	if got := l.PeerCount(); got != 1 {
		t.Fatalf("PeerCount: %d want 1", got)
	}
	// Cutoff in the future drops everything.
	dropped := l.EvictOlderThan(time.Now().Add(time.Hour))
	if dropped != 1 {
		t.Errorf("EvictOlderThan: dropped=%d want 1", dropped)
	}
	if l.PeerCount() != 0 {
		t.Errorf("bucket should be empty after evict")
	}
}
