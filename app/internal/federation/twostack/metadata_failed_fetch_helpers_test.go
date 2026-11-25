// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Helpers of the #1577 failed-fetch activation gate, inside
// TestTwoStack_MetadataFlow_BeforeVersion (metadata_flow_gate_test.go).
//
// Test-only controls, labelled where used:
//
//   - TEST-ONLY SOURCE CAS FRONT (casFailFront): the source answers 503 to
//     a GET of exactly one object; every other request, its HEAD
//     included, reaches the real handler unaltered.
//   - TEST-ONLY RECEIVER RETRY SCHEDULING (parkRetry, dueRetry): only the
//     next_attempt_at of the one named pending record, under a predicate
//     of everything observed about it, exactly one row. Never its state,
//     reason, attempts, files, storage, pins, grants or metadata. This
//     controls when the worker's normal scan finds the retry; it does not
//     prove the default backoff's wall time.
//
// Everything else here only reads.

package twostack_test

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// casFailFront is the TEST-ONLY SOURCE CAS FRONT. It never fails the
// test itself; what it saw is read under its lock.
type casFailFront struct {
	hash  string
	mu    sync.Mutex
	gets  int
	heads []int // the real handler's status for each HEAD of hash
}

func (f *casFailFront) serve(w http.ResponseWriter, r *http.Request, h http.Handler) {
	if strings.HasSuffix(r.URL.Path, "/"+f.hash) {
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			f.gets++
			f.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case http.MethodHead:
			rec := &failFrontStatus{ResponseWriter: w, status: http.StatusOK}
			h.ServeHTTP(rec, r)
			f.mu.Lock()
			f.heads = append(f.heads, rec.status)
			f.mu.Unlock()
			return
		}
	}
	h.ServeHTTP(w, r)
}

func (f *casFailFront) seen() (gets int, heads []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, append([]int(nil), f.heads...)
}

// failFrontStatus records the status the real handler wrote.
type failFrontStatus struct {
	http.ResponseWriter
	status int
}

func (w *failFrontStatus) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// failedAttempt is a received record after its first failed fetch, as
// observed: committed pending/http_status with its transfer warning.
type failedAttempt struct {
	id       uuid.UUID
	attempts int32
	next     time.Time
}

// waitFailedFetch waits until the front answered a GET of hash with 503
// and the destination's record of ann is pending/http_status, not
// completed, its next attempt a near-term retry (not the ten-minute
// lease of a running attempt), with exactly one matching transfer
// warning in the ledger for that attempt.
func (g *flowGate) waitFailedFetch(f *casFailFront, ann, versionURI, hash string) failedAttempt {
	g.t.Helper()
	ctx := context.Background()
	var fa failedAttempt
	g.waitFor("the first fetch of "+versionURI+" failed and was recorded", 120*time.Second, func() bool {
		if gets, _ := f.seen(); gets == 0 {
			return false
		}
		var (
			state, reason     string
			notDone, nearTerm bool
		)
		err := g.dst.pool.QueryRow(ctx, `SELECT id, state, coalesce(reason, ''), attempts, next_attempt_at, completed_at IS NULL,
			       next_attempt_at <= now() + interval '1 minute'
			  FROM federation_received_assets WHERE peer_id = $1 AND activity_uri = $2`, g.peerAonB, ann).
			Scan(&fa.id, &state, &reason, &fa.attempts, &fa.next, &notDone, &nearTerm)
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		if err != nil {
			g.t.Fatalf("%s: read the record of %s: %v", g.dir, ann, err)
		}
		if state != "pending" || reason != "http_status" || fa.attempts < 1 || !notDone || !nearTerm {
			return false
		}
		return g.countOnB(`SELECT count(*) FROM federation_transfer_events WHERE received_asset_id = $1 AND event_type = 'transfer'
			AND outcome = 'warning' AND reason = 'http_status' AND record_state_after = 'pending' AND attempt = $2
			AND activity_uri = $3 AND version_uri = $4 AND object_hash = $5`, fa.id, fa.attempts, ann, versionURI, hash) == 1
	})
	return fa
}

// parkRetry is TEST-ONLY RECEIVER RETRY SCHEDULING: the first failed
// attempt's near-term retry, exactly as observed, moves an hour ahead.
// It returns the parked time.
func (g *flowGate) parkRetry(fa failedAttempt, originURI, versionURI string) time.Time {
	g.t.Helper()
	var parked time.Time
	err := g.dst.pool.QueryRow(context.Background(), `UPDATE federation_received_assets SET next_attempt_at = now() + interval '1 hour'
		WHERE id = $1 AND peer_id = $2 AND origin_asset_uri = $3 AND version_uri = $4 AND state = 'pending' AND reason = 'http_status'
		  AND attempts = 1 AND attempts = $5 AND completed_at IS NULL AND next_attempt_at = $6
		  AND next_attempt_at <= now() + interval '1 minute'
		RETURNING next_attempt_at`, fa.id, g.peerAonB, originURI, versionURI, fa.attempts, fa.next).Scan(&parked)
	if err != nil {
		g.t.Fatalf("%s: TEST-ONLY RECEIVER RETRY SCHEDULING could not park %s (attempts %d, next %s): %v",
			g.dir, fa.id, fa.attempts, fa.next, err)
	}
	return parked
}

// dueRetry is TEST-ONLY RECEIVER RETRY SCHEDULING: the parked record,
// still exactly as parked, becomes due now.
func (g *flowGate) dueRetry(fa failedAttempt, originURI, versionURI string, parked time.Time) {
	g.t.Helper()
	var id uuid.UUID
	err := g.dst.pool.QueryRow(context.Background(), `UPDATE federation_received_assets SET next_attempt_at = now()
		WHERE id = $1 AND peer_id = $2 AND origin_asset_uri = $3 AND version_uri = $4 AND state = 'pending' AND reason = 'http_status'
		  AND attempts = $5 AND completed_at IS NULL AND next_attempt_at = $6
		RETURNING id`, fa.id, g.peerAonB, originURI, versionURI, fa.attempts, parked).Scan(&id)
	if err != nil {
		g.t.Fatalf("%s: TEST-ONLY RECEIVER RETRY SCHEDULING could not make %s due: %v", g.dir, fa.id, err)
	}
}

// recordStable is the destination's record of ann without the columns a
// legitimate supersession changes (state, superseded_at, updated_at),
// with its files and its record pins.
func (g *flowGate) recordStable(ann string) string {
	g.t.Helper()
	var s string
	if err := g.dst.pool.QueryRow(context.Background(), `SELECT concat_ws(' || ',
		    (to_jsonb(r) - 'state' - 'superseded_at' - 'updated_at')::text,
		    (SELECT string_agg(to_jsonb(f)::text, ' | ' ORDER BY f."position") FROM federation_received_asset_files f WHERE f.received_asset_id = r.id),
		    (SELECT coalesce(string_agg(to_jsonb(p)::text, ' | ' ORDER BY p.object_hash), '') FROM storage_pins p
		      WHERE p.pin_subject_type = 'federation_received_asset' AND p.pin_subject_id = r.id::text))
		  FROM federation_received_assets r WHERE r.peer_id = $1 AND r.activity_uri = $2`, g.peerAonB, ann).Scan(&s); err != nil {
		g.t.Fatalf("%s: read the record of %s: %v", g.dir, ann, err)
	}
	if !strings.Contains(s, `"pin_subject_type": "federation_received_asset"`) {
		g.t.Fatalf("%s: the record of %s holds no pin", g.dir, ann)
	}
	return s
}

// revisionRow is one kept revision with its metadata row, as text.
func (g *flowGate) revisionRow(id uuid.UUID) string {
	g.t.Helper()
	var s string
	if err := g.dst.pool.QueryRow(context.Background(), `SELECT (to_jsonb(r) || jsonb_build_object('metadata', to_jsonb(m)))::text
		  FROM federation_received_asset_revisions r JOIN federation_received_asset_metadata m ON m.revision_id = r.id
		 WHERE r.id = $1 AND r.peer_id = $2`, id, g.peerAonB).Scan(&s); err != nil {
		g.t.Fatalf("%s: read revision %s: %v", g.dir, id, err)
	}
	return s
}

// fullHistory is every kept revision of the asset, keyed by its activity
// URI, each value its complete revision row with its complete metadata
// row (null for a flags-only revision), as text. It fails on a query
// error, a duplicate key or an empty history.
func (g *flowGate) fullHistory(o original) map[string]string {
	g.t.Helper()
	rows, err := g.dst.pool.Query(context.Background(), `SELECT r.activity_uri,
		       (to_jsonb(r) || jsonb_build_object('metadata', to_jsonb(m)))::text
		  FROM federation_received_asset_revisions r LEFT JOIN federation_received_asset_metadata m ON m.revision_id = r.id
		 WHERE r.peer_id = $1 AND r.origin_asset_uri = $2 ORDER BY r.activity_uri`, g.peerAonB, g.assetURI(o))
	if err != nil {
		g.t.Fatalf("%s: read the history: %v", g.dir, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uri, row string
		if err := rows.Scan(&uri, &row); err != nil {
			g.t.Fatalf("%s: read the history: %v", g.dir, err)
		}
		if _, dup := out[uri]; dup {
			g.t.Fatalf("%s: two kept revisions of %s", g.dir, uri)
		}
		out[uri] = row
	}
	if err := rows.Err(); err != nil {
		g.t.Fatalf("%s: read the history: %v", g.dir, err)
	}
	if len(out) == 0 {
		g.t.Fatalf("%s: the asset has no kept revision", g.dir)
	}
	return out
}

// sameHistory fails, naming every difference, unless got holds exactly
// want's revisions with identical rows.
func (g *flowGate) sameHistory(what string, want, got map[string]string) {
	g.t.Helper()
	var diffs []string
	for uri, row := range want {
		switch now, ok := got[uri]; {
		case !ok:
			diffs = append(diffs, "removed "+uri)
		case now != row:
			diffs = append(diffs, "rewritten "+uri+": "+row+" -> "+now)
		}
	}
	for uri := range got {
		if _, ok := want[uri]; !ok {
			diffs = append(diffs, "added "+uri)
		}
	}
	if len(diffs) != 0 {
		sort.Strings(diffs)
		g.t.Fatalf("%s: %s the kept history changed: %s", g.dir, what, strings.Join(diffs, "; "))
	}
}

// inboxEnvelope is the destination's single stored inbox envelope of an
// activity, as text.
func (g *flowGate) inboxEnvelope(activityURI string) string {
	g.t.Helper()
	var (
		s string
		n int
	)
	if err := g.dst.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(envelope_json::text, ' | '), ''), count(*)
		FROM federation_inbox WHERE peer_id = $1 AND activity_uri = $2`, g.peerAonB, activityURI).Scan(&s, &n); err != nil {
		g.t.Fatalf("%s: read the inbox row of %s: %v", g.dir, activityURI, err)
	}
	if n != 1 || s == "" {
		g.t.Fatalf("%s: %d inbox rows of %s", g.dir, n, activityURI)
	}
	return s
}
