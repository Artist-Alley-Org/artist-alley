// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Helpers of the #1577 sender gate (metadata_sender_gate_test.go).
//
// Everything the gate sends is produced by the source's own handlers and
// Server.Run, and kept or refused by the destination's own workers. These
// helpers observe (read-only SQL and API reads), hold or count inbox
// requests without altering them, schedule ONLY next_attempt_at of named
// queued source outbox rows, and drive the real admin renegotiation
// route. TEST-ONLY ADVERTISEMENT: the shared in-process
// peer.KnownCapabilities list is changed between completed handshakes;
// both servers share it, so this is not an independent upgrade.

package twostack_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
)

// senderGate is a flow gate with one genuinely imported, settled asset.
type senderGate struct {
	*flowGate
	o        original
	ann      string
	recordID string
	baseline string // the destination's local digest
	actor    string
	full     peer.CapabilitySet // the test advertisement the gate began with
	mu       sync.Mutex
	restores []func() // run after the workers stopped
}

// newSenderGate registers the late restores (before the workers start,
// so they run after the workers stop), starts both instances, imports
// one asset described as given and settles it.
func newSenderGate(t *testing.T, g *flowGate, title, desc, rights, statement string) *senderGate {
	t.Helper()
	s := &senderGate{flowGate: g, full: append(peer.CapabilitySet(nil), peer.KnownCapabilities...)}
	t.Cleanup(func() {
		s.mu.Lock()
		fns := s.restores
		s.mu.Unlock()
		for i := len(fns) - 1; i >= 0; i-- {
			fns[i]()
		}
	})
	g.srcRun, g.dstRun = g.start(g.src), g.start(g.dst)
	g.localFixture()
	s.baseline = localDigest(t, g.dst)
	s.actor = g.src.url + "/users/" + g.owner
	s.o, s.ann, s.recordID = g.importOne(title, desc, rights, statement)
	var r, st *string
	if rights != "" {
		r = sp(rights)
	}
	if statement != "" {
		st = sp(statement)
	}
	var d *string
	if desc != "" {
		d = sp(desc)
	}
	g.waitCurrent(s.recordID, want(sp(title), d, r, st), nil)
	g.settle(s.o)
	s.servedOriginal("after the import")
	return s
}

// restoreDefLater snapshots source definition id and puts its code,
// status and read capability back after the workers stopped.
func (s *senderGate) restoreDefLater(id uuid.UUID) {
	s.t.Helper()
	var row []byte
	if err := s.src.pool.QueryRow(context.Background(), `SELECT to_jsonb(d) FROM field_definition d WHERE id = $1`, id).Scan(&row); err != nil {
		s.t.Fatalf("snapshot definition %s: %v", id, err)
	}
	pool, t := s.src.pool, s.t
	s.mu.Lock()
	s.restores = append(s.restores, func() {
		if _, err := pool.Exec(context.Background(), `
			UPDATE field_definition d SET code = r.code, status = r.status, read_capability = r.read_capability
			  FROM jsonb_populate_record(NULL::field_definition, $2::jsonb) r WHERE d.id = $1`, id, row); err != nil {
			t.Errorf("restore definition %s: %v", id, err)
		}
	})
	s.mu.Unlock()
}

// heldRows is the destination's full stored rows of the original record,
// its files, the original's pins, variants and storage object.
func (s *senderGate) heldRows() string {
	s.t.Helper()
	var out string
	if err := s.dst.pool.QueryRow(context.Background(), `SELECT concat_ws(' || ',
		(SELECT to_jsonb(r)::text FROM federation_received_assets r WHERE r.peer_id = $1 AND r.activity_uri = $2),
		(SELECT string_agg(to_jsonb(f)::text, ' | ' ORDER BY f."position") FROM federation_received_asset_files f
		  JOIN federation_received_assets r ON r.id = f.received_asset_id WHERE r.peer_id = $1 AND r.activity_uri = $2),
		(SELECT coalesce(string_agg(to_jsonb(p)::text, ' | ' ORDER BY p.pin_subject_type, p.pin_subject_id), '') FROM storage_pins p WHERE p.object_hash = $3),
		(SELECT string_agg(to_jsonb(v)::text, ' | ' ORDER BY v.variant_key) FROM storage_variants v WHERE v.object_hash = $3),
		(SELECT to_jsonb(o)::text FROM storage_objects o WHERE o.hash = $3),
		(SELECT coalesce(string_agg(to_jsonb(t)::text, ' | ' ORDER BY t.id), '') FROM federation_received_tombstones t
		  WHERE t.peer_id = $1 AND t.origin_asset_uri = $4))`, s.peerAonB, s.ann, s.o.hash, s.assetURI(s.o)).Scan(&out); err != nil {
		s.t.Fatalf("%s: read the held rows: %v", s.dir, err)
	}
	if !strings.Contains(out, s.o.hash) {
		s.t.Fatalf("%s: the held rows name no original", s.dir)
	}
	return out
}

// history is every kept revision of the asset with its metadata row.
func (s *senderGate) history() string {
	s.t.Helper()
	var out string
	if err := s.dst.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(
		    (to_jsonb(r) || jsonb_build_object('metadata', to_jsonb(m)))::text, ' | ' ORDER BY r.id), '')
		  FROM federation_received_asset_revisions r LEFT JOIN federation_received_asset_metadata m ON m.revision_id = r.id
		 WHERE r.peer_id = $1 AND r.origin_asset_uri = $2`, s.peerAonB, s.assetURI(s.o)).Scan(&out); err != nil {
		s.t.Fatalf("%s: read the history: %v", s.dir, err)
	}
	return out
}

// currentJSON is the API current_metadata (detail must answer 200).
func (s *senderGate) currentJSON() string {
	s.t.Helper()
	st, d := s.detailOnDst(s.recordID)
	if st != http.StatusOK || d.CurrentMetadata == nil {
		s.t.Fatalf("%s: detail %d, current %v", s.dir, st, d.CurrentMetadata)
	}
	b, _ := json.Marshal(d.CurrentMetadata)
	return string(b)
}

func (s *senderGate) servedOriginal(what string) {
	s.t.Helper()
	st, body := s.getOnB(s.member, "/federation/received-assets/"+s.recordID+"/original")
	if st != http.StatusOK || string(body) != string(s.o.body) {
		s.t.Fatalf("%s: %s the destination served %d (%d bytes, uploaded %d)", s.dir, what, st, len(body), len(s.o.body))
	}
}

// metadataActivities are the source's metadata Update activity URIs of
// the asset, sorted.
func (s *senderGate) metadataActivities() []string {
	s.t.Helper()
	rows, err := s.src.pool.Query(context.Background(), `SELECT activity_uri FROM activities
		WHERE activity_type = 'Update' AND object_local_id = $1 AND payload ? 'metadata_capture' ORDER BY activity_uri`, s.o.asset.String())
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// flagsOnlyAfter counts the source's Updates of the asset without a
// metadata capture published after t.
func (s *senderGate) flagsOnlyAfter(t0 time.Time) int {
	return s.countOnSrc(`SELECT count(*) FROM activities WHERE activity_type = 'Update' AND object_local_id = $1
		AND NOT payload ? 'metadata_capture' AND published_at > $2`, s.o.asset.String(), t0)
}

// activityRow is a source activity's full row, as text.
func (s *senderGate) activityRow(uri string) string {
	s.t.Helper()
	var out string
	if err := s.src.pool.QueryRow(context.Background(), `SELECT to_jsonb(a)::text FROM activities a WHERE activity_uri = $1`, uri).Scan(&out); err != nil {
		s.t.Fatalf("%s: read activity %s: %v", s.dir, uri, err)
	}
	return out
}

// authorityRows are the source's share rows of the asset and the
// destination's inbound grant rows for it, as text.
func (s *senderGate) authorityRows() string {
	s.t.Helper()
	var a, b string
	if err := s.src.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(to_jsonb(x)::text, ' | ' ORDER BY x.id), '')
		FROM federation_shares x WHERE x.object_kind = 'asset' AND x.object_id = $1`, s.o.asset).Scan(&a); err != nil {
		s.t.Fatal(err)
	}
	if err := s.dst.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(to_jsonb(x)::text, ' | ' ORDER BY x.id), '')
		FROM federation_inbound_grants x WHERE x.peer_id = $1 AND x.object_uri = $2`, s.peerAonB, s.assetURI(s.o)).Scan(&b); err != nil {
		s.t.Fatal(err)
	}
	if a == "" || b == "" {
		s.t.Fatalf("%s: no share (%d) or grant (%d) rows", s.dir, len(a), len(b))
	}
	return a + " || " + b
}

// renegotiate sets the TEST-ONLY shared advertisement to adv and runs the
// source's real admin renegotiation of the destination, requiring the
// complete response and both stored rows: the same rows, keys, URLs,
// connected and enabled, holding exactly the new intersection at a later
// negotiation time; and the share and grant rows unchanged.
func (s *senderGate) renegotiate(adv peer.CapabilitySet) {
	s.t.Helper()
	srcRow, dstRow := readPeerRow(s.t, s.src, s.dst.url), readPeerRow(s.t, s.dst, s.src.url)
	auth := s.authorityRows()
	peer.KnownCapabilities = append(peer.CapabilitySet(nil), adv...)
	want := strs(peer.Intersect(adv, adv))
	var out renegotiation
	st := s.api(s.src, http.MethodPost, "/admin/federation/peers/"+s.peerBonA.String()+"/renegotiate", nil, &out)
	if st != http.StatusOK || out.Error != nil || out.Peer.ID != s.peerBonA.String() || !slices.Equal(out.Previous, srcRow.caps) ||
		!slices.Equal(out.Current, want) {
		s.t.Fatalf("%s: renegotiate: %d %+v, want %v -> %v", s.dir, st, out, srcRow.caps, want)
	}
	for _, side := range []struct {
		in       *instance
		otherURL string
		before   peerRow
	}{{s.src, s.dst.url, srcRow}, {s.dst, s.src.url, dstRow}} {
		now := readPeerRow(s.t, side.in, side.otherURL)
		if now.id != side.before.id || now.key != side.before.key || now.status != "connected" || !now.enabled ||
			!now.created.Equal(side.before.created) {
			s.t.Fatalf("%s: %s's peer row is not the same row: %+v, was %+v", s.dir, side.in.name, now, side.before)
		}
		if !slices.Equal(now.caps, want) || !now.negotiatedAt.After(side.before.negotiatedAt) {
			s.t.Fatalf("%s: %s stored %v at %v, want %v after %v", s.dir, side.in.name, now.caps, now.negotiatedAt, want, side.before.negotiatedAt)
		}
	}
	if s.peerAonB != s.peerID(s.dst, s.src) || s.peerBonA != s.peerID(s.src, s.dst) {
		s.t.Fatalf("%s: the peer ids changed", s.dir)
	}
	if s.authorityRows() != auth {
		s.t.Fatalf("%s: renegotiation changed a share or grant row", s.dir)
	}
}

// without is adv minus token.
func without(adv peer.CapabilitySet, token string) peer.CapabilitySet {
	var out peer.CapabilitySet
	for _, c := range adv {
		if string(c) != token {
			out = append(out, c)
		}
	}
	return out
}

// allNullMetadata selects the ids of the Updates by actor about object
// whose aa:metadata is exactly four explicit nulls.
func allNullMetadata(envs []observed, actor, object string) []string {
	var out []string
	for _, e := range envs {
		if e.Type != "Update" || e.Actor != actor || e.Object != object || len(e.Metadata) == 0 {
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(e.Metadata, &m) != nil || len(m) != 4 {
			continue
		}
		ok := true
		for _, k := range []string{"title", "description", "rights", "requiredStatement"} {
			if v, present := m[k]; !present || string(v) != "null" {
				ok = false
			}
		}
		if ok {
			out = append(out, e.ID)
		}
	}
	return out
}

// countFront forwards every request with its exact bytes and counts the
// requests carrying any of the given activity ids, whatever members they
// still carry. It never fails the test; errors are kept.
type countFront struct {
	mu   sync.Mutex
	ids  map[string]bool
	hits int
	errs []string
}

func newCountFront(ids []string) *countFront {
	f := &countFront{ids: map[string]bool{}}
	for _, id := range ids {
		f.ids[id] = true
	}
	return f
}

func (f *countFront) serve(w http.ResponseWriter, r *http.Request, h http.Handler) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.mu.Lock()
		f.errs = append(f.errs, "read: "+err.Error())
		f.mu.Unlock()
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	envs, derr := observeInbox(body)
	hit := false
	for _, e := range envs {
		if f.ids[e.ID] {
			hit = true
		}
	}
	f.mu.Lock()
	if derr != nil {
		f.errs = append(f.errs, "decode: "+derr.Error())
	}
	if hit {
		f.hits++
	}
	f.mu.Unlock()
	forward(w, r, h, body)
}

func (f *countFront) result() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, append([]string(nil), f.errs...)
}

// held is one held source activity: its URI, id and full row when held.
type held struct {
	uri string
	id  uuid.UUID
	row string
}

// holdAll waits until the front holds every queued metadata Update of the
// asset to the destination (each attempted at least once), no metadata
// work is pending and every Update is routed, then moves ONLY those held
// queued rows' next_attempt_at into the future. It returns them.
func (s *senderGate) holdAll(front *holdFront, notBefore []string) []held {
	s.t.Helper()
	ctx := context.Background()
	var queued []string
	s.waitFor("every matching Update held", 120*time.Second, func() bool {
		h, _, _ := front.snapshot()
		if len(h) == 0 {
			return false
		}
		rows, err := s.src.pool.Query(ctx, `SELECT a.activity_uri, ob.attempts FROM federation_outbox ob JOIN activities a ON a.id = ob.activity_id
			WHERE ob.peer_id = $1 AND ob.status = 'queued' AND a.object_local_id = $2 AND a.activity_type = 'Update'
			  AND a.payload ? 'metadata_capture'`, s.peerBonA, s.o.asset.String())
		if err != nil {
			s.t.Fatal(err)
		}
		queued = queued[:0]
		all := true
		for rows.Next() {
			var uri string
			var attempts int
			if err := rows.Scan(&uri, &attempts); err != nil {
				rows.Close()
				s.t.Fatal(err)
			}
			queued = append(queued, uri)
			if !slices.Contains(h, uri) || attempts < 1 {
				all = false
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			s.t.Fatal(err)
		}
		work := s.countOnSrc(`SELECT count(*) FROM federation_metadata_work WHERE asset_id = $1 OR kind = 'all'`, s.o.asset)
		unrouted := s.countOnSrc(`SELECT count(*) FROM activities a WHERE a.object_local_id = $1 AND a.activity_type = 'Update'
			AND a.payload ? 'metadata_capture' AND NOT EXISTS (SELECT 1 FROM federation_outbox ob WHERE ob.activity_id = a.id AND ob.peer_id = $2)`,
			s.o.asset.String(), s.peerBonA)
		return all && len(queued) > 0 && work == 0 && unrouted == 0
	})
	h, _, _ := front.snapshot()
	var out []held
	seen := map[string]bool{}
	for _, uri := range h {
		if seen[uri] {
			continue
		}
		seen[uri] = true
		if slices.Contains(notBefore, uri) {
			s.t.Fatalf("%s: the front held %s, which existed before the edit", s.dir, uri)
		}
		x := held{uri: uri, id: s.activityID(uri)}
		if st, _, _ := s.outboxRow(x.id); st == "queued" {
			s.schedule(x.id, "now() + interval '1 hour'")
		} else {
			s.t.Fatalf("%s: held %s is %q, not queued", s.dir, uri, st)
		}
		x.row = s.activityRow(uri)
		if n := s.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, uri); n != 0 {
			s.t.Fatalf("%s: the held %s reached the destination's inbox", s.dir, uri)
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].uri < out[j].uri })
	if len(out) != len(queued) {
		s.t.Fatalf("%s: %d held, %d queued", s.dir, len(out), len(queued))
	}
	return out
}

// refuseHeld makes ONLY the held rows due, behind a counting front, and
// requires each to end failed with a last_error containing reason, no
// request carrying any of them, and every source row unchanged.
func (s *senderGate) refuseHeld(hs []held, reason string) {
	s.t.Helper()
	var uris []string
	for _, h := range hs {
		uris = append(uris, h.uri)
	}
	counter := newCountFront(uris)
	s.dst.setInboxFront(counter.serve)
	for _, h := range hs {
		s.schedule(h.id, "now()")
	}
	for _, h := range hs {
		s.waitFor("held "+h.uri+" refused", 120*time.Second, func() bool { st, _, _ := s.outboxRow(h.id); return st == "failed" })
		var lastError string
		err := s.src.pool.QueryRow(context.Background(), `SELECT last_error FROM federation_outbox WHERE activity_id = $1 AND peer_id = $2
			AND target_user_url IS NULL`, h.id, s.peerBonA).Scan(&lastError)
		if errors.Is(err, pgx.ErrNoRows) || err != nil {
			s.t.Fatalf("%s: read the failed row of %s: %v", s.dir, h.uri, err)
		}
		if !strings.Contains(lastError, reason) {
			s.t.Fatalf("%s: %s failed with %q, want %q", s.dir, h.uri, lastError, reason)
		}
		if s.activityRow(h.uri) != h.row {
			s.t.Fatalf("%s: the held activity %s changed", s.dir, h.uri)
		}
		if n := s.countOnB(`SELECT count(*) FROM federation_inbox WHERE activity_uri = $1`, h.uri); n != 0 {
			s.t.Fatalf("%s: the refused %s reached the destination's inbox", s.dir, h.uri)
		}
		if n := s.countOnB(`SELECT count(*) FROM federation_received_asset_revisions WHERE peer_id = $1 AND activity_uri = $2`,
			s.peerAonB, h.uri); n != 0 {
			s.t.Fatalf("%s: the destination kept a revision of the refused %s", s.dir, h.uri)
		}
	}
	if hits, errs := counter.result(); hits != 0 || len(errs) != 0 {
		s.t.Fatalf("%s: %d requests carried a refused activity (errors %v)", s.dir, hits, errs)
	}
}
