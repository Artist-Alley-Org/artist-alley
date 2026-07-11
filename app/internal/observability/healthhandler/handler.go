// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package healthhandler is the generic shared shim behind every
// /admin/{subsystem}/health endpoint. Per the Phase 1.18.A-2
// follow-up B brief (decision 17): factor the JSON-rendering +
// admin-capability-gating into a tiny shared helper so future
// subsystems implement their /admin/{subsystem}/health in one
// line.
//
// Usage from a subsystem's boot wire:
//
//	router.Method("GET", "/admin/metadata-extraction/health",
//	    healthhandler.HandlerFor("metadata-extraction", counter, "system.admin"))
//
// The Counter is whatever the subsystem already keeps for its
// operator-visible event stream (success / failure / format
// breakdown). The handler doesn't own the counter — it just
// snapshots + renders on every request.
//
// First two users (~commit 1 below + ~commit 4 of Phase 1.19.A):
//
//   - /admin/metadata-extraction/health  (this PR)
//   - /admin/email/health                (next PR)
//
// Future subsystems (1.54 iiif, 1.16 search, 1.19.B/C auth) plug
// in one-line per the same shape; the per-subsystem counter type
// stays local to each subsystem's package.
package healthhandler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// SubsystemHealth is the wire shape every /admin/{subsystem}/health
// endpoint returns. Per decision 17 in the brief — keep this
// stable so admin UI clients can render any subsystem's health
// with the same component.
type SubsystemHealth struct {
	Subsystem    string           `json:"subsystem"`
	CounterTotal int64            `json:"counter_total"`
	ByFormat     map[string]int64 `json:"by_format,omitempty"`
	ByResult     map[string]int64 `json:"by_result"`
	LastSuccess  *time.Time       `json:"last_success_at,omitempty"`
	LastFailure  *time.Time       `json:"last_failure_at,omitempty"`
	Notes        []string         `json:"notes,omitempty"`
}

// Counter is the narrow surface every subsystem's per-event
// counter implements. The shim takes a snapshot on each request
// and renders it. Implementations are concurrency-safe (sync/
// atomic per-pair counters; a per-subsystem package owns the
// concrete struct).
type Counter interface {
	// Snapshot returns the current health state. Called on every
	// /admin/{subsystem}/health request; implementations should
	// be cheap (read-only over atomic counters).
	Snapshot() SubsystemHealth
}

// HandlerFor returns a net/http.Handler that:
//
//  1. Resolves the caller's [auth.Identity] from request context.
//  2. Returns 401 if absent, 403 if the identity lacks
//     requiredCap (use "system.admin" for the standard admin
//     check; future custom caps for narrower roles).
//  3. Calls counter.Snapshot() + JSON-renders the
//     [SubsystemHealth] with the [subsystem] field populated
//     (overrides whatever the snapshot returned).
//
// Returns 500 with a small error body if JSON encoding fails;
// the snapshot itself shouldn't fail.
func HandlerFor(subsystem string, counter Counter, requiredCap string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := auth.IdentityFromContext(r.Context())
		if id == nil {
			writeJSONError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if requiredCap != "" && !id.Can(requiredCap) && !id.Can(auth.SuperAdminCapability) {
			writeJSONError(w, http.StatusForbidden, "missing capability: "+requiredCap)
			return
		}

		snap := counter.Snapshot()
		snap.Subsystem = subsystem // always render the canonical name

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(snap)
	})
}

// SnapshotFromContext is a convenience for tests that want to
// hit the shim without spinning up an http.Server. Threads a
// minimal [auth.Identity] through context + invokes the handler
// against a synthetic ResponseWriter.
//
// Returned snapshot is nil when the handler returned non-200.
func SnapshotFromContext(ctx context.Context, h http.Handler) (*SubsystemHealth, int) {
	rec := newRecorder()
	req, _ := http.NewRequestWithContext(ctx, "GET", "/", nil)
	h.ServeHTTP(rec, req)
	if rec.status != http.StatusOK {
		return nil, rec.status
	}
	var out SubsystemHealth
	if err := json.Unmarshal(rec.body, &out); err != nil {
		return nil, http.StatusInternalServerError
	}
	return &out, rec.status
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// recorder is a tiny test-only http.ResponseWriter substitute.
// Kept local so SnapshotFromContext can be used from any
// subsystem's test without dragging in httptest.
type recorder struct {
	status int
	body   []byte
	hdr    http.Header
}

func newRecorder() *recorder { return &recorder{hdr: http.Header{}} }

func (r *recorder) Header() http.Header         { return r.hdr }
func (r *recorder) Write(p []byte) (int, error) { r.body = append(r.body, p...); return len(p), nil }
func (r *recorder) WriteHeader(s int)           { r.status = s }
