// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package inbox

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// PeerRateLimiter caps inbound request rate per peer per the
// 1.22.D design proposal §5.5 addition A. A misbehaving (or
// compromised) peer that floods /federation/inbox would burn
// CPU on signature verification before the dedup catches the
// replay; the rate limiter front-loads the rejection.
//
// Placement in the pipeline: AFTER HTTP-Signature verification
// (so unauthenticated junk still rejects at sig_invalid) but
// BEFORE envelope verification + DB write.
//
// Implementation: token bucket per peer. burstSize tokens
// available; refillRate tokens per second; tokens cap at burstSize.
// Exceeded → 429 Too Many Requests with a Retry-After header
// computed from the time-to-next-token.
//
// Defaults: 100 req/sec per peer, burst 100. Configurable via
// sysconfig later when 1.22.D-c admin surface lands.
//
// Concurrency: protected by a single sync.Mutex. The map +
// per-bucket math are O(1); the mutex is not a bottleneck at
// the configured rates.
type PeerRateLimiter struct {
	mu         sync.Mutex
	buckets    map[uuid.UUID]*peerBucket
	burstSize  float64
	refillRate float64 // tokens per second
}

type peerBucket struct {
	tokens   float64
	lastSeen time.Time
}

// NewPeerRateLimiter constructs a limiter. burstSize is the
// max in-flight allowance; refillRate is tokens per second.
// 100/100 matches the §5.5 addition A default; tests override.
func NewPeerRateLimiter(burstSize, refillRate float64) *PeerRateLimiter {
	if burstSize <= 0 {
		burstSize = 100
	}
	if refillRate <= 0 {
		refillRate = 100
	}
	return &PeerRateLimiter{
		buckets:    make(map[uuid.UUID]*peerBucket),
		burstSize:  burstSize,
		refillRate: refillRate,
	}
}

// Allow checks whether a request from `peerID` is permitted.
// Returns (true, 0) when allowed; (false, retryAfter) when
// rate-limited. retryAfter is the duration until at least one
// token becomes available — caller renders it as the Retry-After
// HTTP header.
func (l *PeerRateLimiter) Allow(peerID uuid.UUID) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[peerID]
	if !ok {
		// First request from this peer — start with a full
		// bucket minus one (the request we're approving now).
		l.buckets[peerID] = &peerBucket{
			tokens:   l.burstSize - 1,
			lastSeen: now,
		}
		return true, 0
	}

	// Refill since lastSeen, cap at burstSize.
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens = min(l.burstSize, b.tokens+elapsed*l.refillRate)
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Not enough — compute time-to-next-token.
	needed := 1 - b.tokens
	wait := time.Duration(needed / l.refillRate * float64(time.Second))
	// Round up to whole seconds for the HTTP Retry-After header
	// (which is integer seconds per RFC 7231 §7.1.3).
	if wait%time.Second != 0 {
		wait = wait.Truncate(time.Second) + time.Second
	}
	return false, wait
}

// PeerCount returns the number of peers currently tracked.
// Exposed for observability + tests.
func (l *PeerRateLimiter) PeerCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// EvictOlderThan drops bucket entries last seen before cutoff.
// Bounds memory if many one-shot peers churn through. Caller
// invokes periodically (e.g. every 5 min) from a janitor
// goroutine; absent that, the map grows monotonically.
func (l *PeerRateLimiter) EvictOlderThan(cutoff time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	dropped := 0
	for id, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, id)
			dropped++
		}
	}
	return dropped
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
