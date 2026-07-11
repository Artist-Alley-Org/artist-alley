// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package auth

import (
	"container/list"
	"sync"
	"time"
)

// LoginLimiter is an in-process rate limiter for /auth/login.
//
// It keeps a token bucket per key (e.g., "ip:1.2.3.4" or "user:alice")
// with LRU eviction so a flood of unique keys can't blow the process
// heap. Two buckets are consulted per attempt: one for the client IP
// and one for the attempted username. Either one tripping rejects the
// request.
//
// This isn't a distributed limiter — multi-instance deployments will
// want Redis or similar in front. Solo self-hosted (the artist-alley
// default) needs nothing more than this.
type LoginLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*list.Element // key -> entry in lru
	lru      *list.List
	capacity int           // max distinct keys retained
	refill   time.Duration // time per token added back
	burst    int           // bucket size
}

type limiterEntry struct {
	key       string
	tokens    float64
	lastFill  time.Time
}

// NewLoginLimiter returns a limiter sized for solo-deploy traffic.
//
// burst is the number of attempts allowed in a quick burst; refill is
// the steady-state interval at which one token comes back. Defaults
// match a "5 attempts then one per 12 seconds" budget — enough to
// recover from a fat-fingered password, painful enough to deter
// online brute force.
func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{
		buckets:  make(map[string]*list.Element),
		lru:      list.New(),
		capacity: 10000,
		refill:   12 * time.Second,
		burst:    5,
	}
}

// Allow consumes one token from key's bucket. Returns true if the
// caller may proceed, false if the bucket is empty.
func (l *LoginLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elem, ok := l.buckets[key]
	if !ok {
		// Evict the oldest entry if at capacity.
		if l.lru.Len() >= l.capacity {
			victim := l.lru.Back()
			if victim != nil {
				delete(l.buckets, victim.Value.(*limiterEntry).key)
				l.lru.Remove(victim)
			}
		}
		entry := &limiterEntry{
			key:      key,
			tokens:   float64(l.burst) - 1, // consume one immediately
			lastFill: now,
		}
		elem = l.lru.PushFront(entry)
		l.buckets[key] = elem
		return true
	}
	l.lru.MoveToFront(elem)
	entry := elem.Value.(*limiterEntry)

	elapsed := now.Sub(entry.lastFill).Seconds()
	if elapsed > 0 {
		entry.tokens += elapsed / l.refill.Seconds()
		if entry.tokens > float64(l.burst) {
			entry.tokens = float64(l.burst)
		}
		entry.lastFill = now
	}

	if entry.tokens < 1 {
		return false
	}
	entry.tokens--
	return true
}

// Forget removes a key from the limiter. Called on successful login so
// a legitimate user's history doesn't penalise them later.
func (l *LoginLimiter) Forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if elem, ok := l.buckets[key]; ok {
		l.lru.Remove(elem)
		delete(l.buckets, key)
	}
}
