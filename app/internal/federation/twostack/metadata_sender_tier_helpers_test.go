// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Helpers of the #1577 source-tier sender gate
// (metadata_sender_tier_gate_test.go).
//
// TEST-ONLY SOURCE-TIER FIXTURE: the asset API has no sensitivity setter,
// so setSourceTier changes ONLY the sensitivity of the gate's own uploaded
// source asset with one parameterized UPDATE (as cas_gate_test.go's
// restricted case does). It is not a sensitivity API, a scheduled action
// or an embargo expiry, and nothing else is written: every metadata edit
// goes through the source's own handlers, every Update is recorded, sent,
// refused or kept by the instances' own workers. The other helpers only
// read, or forward inbox requests unaltered.

package twostack_test

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/google/uuid"
)

// setSourceTier is the TEST-ONLY SOURCE-TIER FIXTURE: exactly the gate's
// own live local source asset, exactly one row, read back.
func (s *senderGate) setSourceTier(tier string) {
	s.t.Helper()
	ctx := context.Background()
	tag, err := s.src.pool.Exec(ctx, `UPDATE assets SET sensitivity = $2
		WHERE id = $1 AND origin_server_id IS NULL AND deleted_at IS NULL`, s.o.asset, tier)
	if err != nil || tag.RowsAffected() != 1 {
		s.t.Fatalf("%s: TEST-ONLY SOURCE-TIER FIXTURE to %s: %v (%d rows)", s.dir, tier, err, tag.RowsAffected())
	}
	var now string
	if err := s.src.pool.QueryRow(ctx, `SELECT sensitivity FROM assets WHERE id = $1`, s.o.asset).Scan(&now); err != nil || now != tier {
		s.t.Fatalf("%s: the source tier reads %q, want %q: %v", s.dir, now, tier, err)
	}
}

// restoreTierLater registers, among the gate's late restores (after the
// workers stopped, before the pools close), putting the gate's own
// source asset back to public if a failed test left it otherwise.
func (s *senderGate) restoreTierLater() {
	s.t.Helper()
	pool, t, id, dir := s.src.pool, s.t, s.o.asset, s.dir
	s.mu.Lock()
	s.restores = append(s.restores, func() {
		tag, err := pool.Exec(context.Background(), `UPDATE assets SET sensitivity = 'public'
			WHERE id = $1 AND sensitivity <> 'public'`, id)
		if err != nil {
			t.Errorf("%s: restore the source tier: %v", dir, err)
			return
		}
		if tag.RowsAffected() != 0 {
			t.Logf("%s: cleanup restored the source tier of %s to public", dir, id)
		}
	})
	s.mu.Unlock()
}

// sourceValues are the source asset's title, description and tier and its
// seeded rights and required statement, as stored.
func (s *senderGate) sourceValues() (title, desc, tier, rights, statement string) {
	s.t.Helper()
	if err := s.src.pool.QueryRow(context.Background(), `SELECT a.title, coalesce(a.description, ''), a.sensitivity,
		       coalesce((SELECT v.value_text FROM asset_field_value v WHERE v.asset_id = a.id AND v.field_id = $2), ''),
		       coalesce((SELECT v.value_text FROM asset_field_value v WHERE v.asset_id = a.id AND v.field_id = $3), '')
		  FROM assets a WHERE a.id = $1`, s.o.asset, seedRightsDef, seedStatementDef).
		Scan(&title, &desc, &tier, &rights, &statement); err != nil {
		s.t.Fatalf("%s: read the source values: %v", s.dir, err)
	}
	return title, desc, tier, rights, statement
}

// outboxRows are the full source outbox rows of the held activities to
// the destination, as text; every one must exist.
func (s *senderGate) outboxRows(hs []held) string {
	s.t.Helper()
	ids := make([]uuid.UUID, 0, len(hs))
	for _, h := range hs {
		ids = append(ids, h.id)
	}
	var (
		out string
		n   int
	)
	if err := s.src.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(to_jsonb(ob)::text, ' | ' ORDER BY ob.activity_id), ''), count(*)
		FROM federation_outbox ob WHERE ob.activity_id = ANY ($1) AND ob.peer_id = $2 AND ob.target_user_url IS NULL`,
		ids, s.peerBonA).Scan(&out, &n); err != nil {
		s.t.Fatalf("%s: read the held outbox rows: %v", s.dir, err)
	}
	if n != len(hs) || n == 0 {
		s.t.Fatalf("%s: %d outbox rows for %d held activities", s.dir, n, len(hs))
	}
	return out
}

// peerState is both instances' peer rows of each other: status, enabled
// and the stored capability intersection.
func (s *senderGate) peerState() string {
	s.t.Helper()
	var a, b string
	if err := s.src.pool.QueryRow(context.Background(), `SELECT p.status || ' ' || p.enabled::text || ' ' || p.capabilities::text
		FROM federation_peers p WHERE p.id = $1`, s.peerBonA).Scan(&a); err != nil {
		s.t.Fatalf("%s: read the source's peer row: %v", s.dir, err)
	}
	if err := s.dst.pool.QueryRow(context.Background(), `SELECT p.status || ' ' || p.enabled::text || ' ' || p.capabilities::text
		FROM federation_peers p WHERE p.id = $1`, s.peerAonB).Scan(&b); err != nil {
		s.t.Fatalf("%s: read the destination's peer row: %v", s.dir, err)
	}
	return a + " || " + b
}

// anonymousHidden requires the destination to answer 404 to an anonymous
// read of the record and of its original.
func (s *senderGate) anonymousHidden(what string) {
	s.t.Helper()
	for _, p := range []string{"/federation/received-assets/" + s.recordID, "/federation/received-assets/" + s.recordID + "/original"} {
		if st, _ := s.getOnB("", p); st != http.StatusNotFound {
			s.t.Fatalf("%s: %s anonymous %s: %d", s.dir, what, p, st)
		}
	}
}

// assetFront forwards every inbox request with its exact bytes and
// records each envelope whose object is object. It never fails the test
// itself; errors are kept.
type assetFront struct {
	mu     sync.Mutex
	object string
	seen   []observed
	errs   []string
}

func (f *assetFront) serve(w http.ResponseWriter, r *http.Request, h http.Handler) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.mu.Lock()
		f.errs = append(f.errs, "read: "+err.Error())
		f.mu.Unlock()
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	envs, derr := observeInbox(body)
	f.mu.Lock()
	if derr != nil {
		f.errs = append(f.errs, "decode: "+derr.Error())
	}
	for _, e := range envs {
		if e.Object == f.object {
			f.seen = append(f.seen, e)
		}
	}
	f.mu.Unlock()
	forward(w, r, h, body)
}

func (f *assetFront) snapshot() ([]observed, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]observed(nil), f.seen...), append([]string(nil), f.errs...)
}
