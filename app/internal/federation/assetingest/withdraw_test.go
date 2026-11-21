// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1575 withdrawal (spec v1 §17.3) at the receiver: an origin's
// aa:tombstone on a Delete of an asset it announced. Same harness as
// ingest_test.go (live database, fs storage, production casfetch client
// and inbound grant record, origin stub). Skips without AA_DB_PASSWORD.

package assetingest_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
)

// tombstone builds the aa:tombstone of a's version 1, listing a's
// originals.
func (f *fx) tombstone(a *asset, reason string) map[string]any {
	files := make([]map[string]any, 0, len(a.hash))
	for _, h := range a.hash {
		files = append(files, map[string]any{"role": "original", "contentUri": "cas://" + h,
			"checksums": []map[string]string{{"algorithm": "sha256", "value": h}}})
	}
	t := map[string]any{"id": a.uri, "formerType": "aa:Asset", "deleted": "2026-11-21T09:00:00Z",
		"lastVersion": f.version(a), "files": files}
	if reason != "" {
		t["reason"] = reason
	}
	return t
}

// deleteOf is the origin's Delete of a carrying member as aa:tombstone.
func (f *fx) deleteOf(a *asset, member map[string]any) *federation.Envelope {
	f.t.Helper()
	raw, err := json.Marshal(member)
	if err != nil {
		f.t.Fatal(err)
	}
	return &federation.Envelope{
		Context: federation.ContextV1, Type: federation.ActivityDelete,
		ID: f.origin.srv.URL + "/activities/" + uuid.NewString(), Actor: f.origin.srv.URL + "/users/alice",
		Published: time.Now().UTC(), Object: a.uri,
		Extra: map[string]json.RawMessage{federation.MemberArchivalTombstone: raw},
	}
}

func (f *fx) withdraw(e *federation.Envelope) (assetingest.Withdrawal, error) {
	return f.ingest.Withdraw(context.Background(), f.peerID, e)
}

func (f *fx) tombstones(assetURI string) int {
	return f.count(`SELECT count(*) FROM federation_received_tombstones WHERE origin_asset_uri = $1`, assetURI)
}

// deaccessions are the record's deaccession ledger rows.
func (f *fx) deaccessions(id uuid.UUID) []ledgerRow {
	var out []ledgerRow
	for _, r := range f.ledger(id) {
		if r.eventType == assetingest.EventDeaccession {
			out = append(out, r)
		}
	}
	return out
}

func TestWithdraw_CompleteRecord(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset(2)
	f.grant(f.peerID, a.uri, nil)
	in, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if err != nil || in.State != assetingest.StateComplete {
		t.Fatalf("import: %+v %v", in, err)
	}
	// A pin of the same bytes by another owner (here a local asset) is
	// not the record's and is kept.
	other := uuid.NewString()
	if _, err := f.pool.Exec(ctx, `INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($1, 'asset', $2)`,
		a.hash[0], other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM storage_pins WHERE pin_subject_id = $1`, other)
	})

	del := f.deleteOf(a, f.tombstone(a, "withdrawn by the artist"))
	w, err := f.withdraw(del)
	if err != nil || w.Duplicate || len(w.Records) != 1 || w.Records[0] != in.RecordID {
		t.Fatalf("withdraw: %+v %v", w, err)
	}
	if st, reason := f.state(in.RecordID); st != assetingest.StateWithdrawn || reason != assetingest.ReasonWithdrawn {
		t.Fatalf("record %s/%s, want withdrawn/withdrawn", st, reason)
	}
	var withdrawnAt, completedAt *time.Time
	var grant *uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT withdrawn_at, completed_at, authorizing_grant_id FROM federation_received_assets WHERE id = $1`,
		in.RecordID).Scan(&withdrawnAt, &completedAt, &grant); err != nil || withdrawnAt == nil || completedAt == nil || grant == nil {
		t.Fatalf("withdrawn_at %v completed_at %v grant %v (%v): the record keeps when it was completed and under what", withdrawnAt, completedAt, grant, err)
	}
	// The originals are withdrawn and keep when they were verified and
	// what they were identified as.
	if n := f.count(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1 AND role = 'original'
		AND state = 'withdrawn' AND verified_at IS NOT NULL AND detected_media_type IS NOT NULL`, in.RecordID); n != 2 {
		t.Fatalf("%d withdrawn originals keeping their verification, want 2", n)
	}
	// Only the record's own pins went.
	for _, h := range a.hash {
		if n := f.pins(h, assetingest.PinSubjectType); n != 0 {
			t.Fatalf("hash %s: %d record pins after withdrawal", h[:12], n)
		}
	}
	if n := f.pins(a.hash[0], "asset"); n != 1 {
		t.Fatalf("the other owner's pin: %d, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NULL`, a.hash[0]); n != 1 {
		t.Fatal("bytes still pinned by another owner were scheduled for GC")
	}
	if n := f.count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NOT NULL`, a.hash[1]); n != 1 {
		t.Fatal("unpinned bytes of the withdrawn record were not scheduled for GC")
	}
	// One ledger event.
	d := f.deaccessions(in.RecordID)
	if len(d) != 1 || d[0].outcome != assetingest.OutcomeSuccess || d[0].stateAfter != assetingest.StateWithdrawn ||
		d[0].reason != nil || d[0].pos != nil {
		t.Fatalf("deaccession events %+v", d)
	}
	// The tombstone is kept as received.
	var kept []byte
	var reason, last string
	if err := f.pool.QueryRow(ctx, `SELECT tombstone, reason, last_version FROM federation_received_tombstones
		WHERE peer_id = $1 AND origin_asset_uri = $2`, f.peerID, a.uri).Scan(&kept, &reason, &last); err != nil ||
		reason != "withdrawn by the artist" || last != f.version(a) {
		t.Fatalf("kept tombstone reason %q last %q (%v)", reason, last, err)
	}
	if _, err := federation.ParseArchivalTombstone(kept); err != nil {
		t.Fatalf("kept tombstone: %v", err)
	}

	// Replays change nothing: the same activity, and a second tombstone.
	for _, e := range []*federation.Envelope{del, f.deleteOf(a, f.tombstone(a, "another reason"))} {
		again, err := f.withdraw(e)
		if err != nil || !again.Duplicate || len(again.Records) != 0 {
			t.Fatalf("replay: %+v %v", again, err)
		}
	}
	if n := len(f.deaccessions(in.RecordID)); n != 1 {
		t.Fatalf("%d deaccession events after replays, want 1", n)
	}
	if n := f.tombstones(a.uri); n != 1 {
		t.Fatalf("%d tombstones after replays, want 1", n)
	}
	// The announcement again, redelivered: the record is withdrawn and
	// nothing is fetched.
	before := f.origin.getCount(a.hash[1])
	out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	wantReason(t, err, assetingest.ReasonWithdrawn)
	if assetingest.Retryable(err) || out.State == assetingest.StateComplete {
		t.Fatalf("a later announcement after withdrawal: %+v %v", out, err)
	}
	if f.origin.getCount(a.hash[1]) != before {
		t.Fatal("a later announcement fetched bytes of a withdrawn asset")
	}
	if n := f.count(`SELECT count(*) FROM storage_pins WHERE object_hash = $1`, a.hash[1]); n != 0 {
		t.Fatal("a later announcement pinned bytes of a withdrawn asset")
	}
}

func TestWithdraw_TombstoneBeforeAnnouncement(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	w, err := f.withdraw(f.deleteOf(a, f.tombstone(a, "")))
	if err != nil || w.Duplicate || len(w.Records) != 0 {
		t.Fatalf("withdraw first: %+v %v", w, err)
	}
	if n := f.tombstones(a.uri); n != 1 {
		t.Fatalf("%d tombstones, want 1", n)
	}
	// The delayed announcement is refused before anything is fetched and
	// is never resurrected by a redelivery.
	e := f.envelope(a, a.member(f.version(a), 1, "public"))
	for i := 0; i < 2; i++ {
		out, err := f.receive(e)
		wantReason(t, err, assetingest.ReasonWithdrawn)
		if out.State != assetingest.StateRejected {
			t.Fatalf("delivery %d: %+v %v", i, out, err)
		}
	}
	if f.origin.getCount(a.hash[0]) != 0 {
		t.Fatal("the announcement of a withdrawn asset fetched bytes")
	}
	f.nothingUsable(a.hash...)
	if n := f.count(`SELECT count(*) FROM federation_received_assets WHERE origin_asset_uri = $1`, a.uri); n != 1 {
		t.Fatalf("%d records, want the one refused record", n)
	}
}

func TestWithdraw_WaitingRecordIsNotResurrected(t *testing.T) {
	f := newFx(t).withWake()
	a := f.newAsset(1)
	// No grant yet: the record waits for one.
	out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if out.State != assetingest.StatePending || !assetingest.Retryable(err) {
		t.Fatalf("before the grant: %+v %v", out, err)
	}
	w, err := f.withdraw(f.deleteOf(a, f.tombstone(a, "")))
	if err != nil || len(w.Records) != 1 || w.Records[0] != out.RecordID {
		t.Fatalf("withdraw: %+v %v", w, err)
	}
	// The grant arrives after the withdrawal: it wakes nothing.
	f.grant(f.peerID, a.uri, nil)
	if st, _ := f.state(out.RecordID); st != assetingest.StateWithdrawn {
		t.Fatalf("record %s after the grant, want withdrawn", st)
	}
	if f.due(out.RecordID) {
		t.Fatal("the withdrawn record is due again")
	}
	if _, err := f.ingest.Process(context.Background(), out.RecordID, f.signerFor()); assetingest.ReasonOf(err) != assetingest.ReasonWithdrawn {
		t.Fatalf("processing the withdrawn record: %v", err)
	}
	if f.origin.getCount(a.hash[0]) != 0 {
		t.Fatal("bytes of a withdrawn asset were fetched")
	}
	f.nothingUsable(a.hash...)
}

func TestWithdraw_ImportInFlightKeepsNothing(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(2)
	f.grant(f.peerID, a.uri, nil)
	var w assetingest.Withdrawal
	var werr error
	// The bytes are fetched and verified; the withdrawal commits before
	// the import does.
	assetingest.SetAfterFetch(f.ingest, func() {
		w, werr = f.withdraw(f.deleteOf(a, f.tombstone(a, "")))
	})
	out, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if werr != nil || len(w.Records) != 1 {
		t.Fatalf("withdraw during the import: %+v %v", w, werr)
	}
	wantReason(t, err, assetingest.ReasonWithdrawn)
	if assetingest.Retryable(err) || out.State != assetingest.StateWithdrawn {
		t.Fatalf("the import after the withdrawal: %+v %v", out, err)
	}
	f.nothingUsable(a.hash...)
	if n := f.count(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1 AND role = 'original' AND state = 'withdrawn' AND verified_at IS NULL`,
		out.RecordID); n != 2 {
		t.Fatalf("%d never-verified withdrawn originals, want 2", n)
	}
}

func TestWithdraw_Refusals(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	in, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if err != nil || in.State != assetingest.StateComplete {
		t.Fatalf("import: %+v %v", in, err)
	}
	other := f.addPeer("https://other-"+uuid.NewString()[:8]+".example", bothProfiles)
	noProfile := f.addPeer("https://plain-"+uuid.NewString()[:8]+".example", `["share-lifecycle-1"]`)

	cases := []struct {
		name   string
		peer   uuid.UUID
		edit   func(e *federation.Envelope)
		reason string
	}{
		{"another peer's tombstone of this asset", other, nil, casfetch.ReasonForeignOrigin},
		{"actor on another origin", f.peerID, func(e *federation.Envelope) { e.Actor = "https://elsewhere.example/users/alice" }, casfetch.ReasonForeignOrigin},
		{"not negotiated", noProfile, nil, casfetch.ReasonProfile},
		{"tombstone id is not the object", f.peerID, func(e *federation.Envelope) {
			m := f.tombstone(a, "")
			m["id"] = a.uri + "x"
			raw, _ := json.Marshal(m)
			e.Extra[federation.MemberArchivalTombstone] = raw
		}, assetingest.ReasonArchivalInvalid},
		{"checksum not the content hash", f.peerID, func(e *federation.Envelope) {
			m := f.tombstone(a, "")
			last := "0"
			if strings.HasSuffix(a.hash[0], "0") {
				last = "1"
			}
			m["files"].([]map[string]any)[0]["checksums"] = []map[string]string{{"algorithm": "sha256", "value": a.hash[0][:63] + last}}
			raw, _ := json.Marshal(m)
			e.Extra[federation.MemberArchivalTombstone] = raw
		}, assetingest.ReasonArchivalInvalid},
		{"no tombstone", f.peerID, func(e *federation.Envelope) { delete(e.Extra, federation.MemberArchivalTombstone) }, assetingest.ReasonArchivalInvalid},
		{"not a Delete", f.peerID, func(e *federation.Envelope) { e.Type = federation.ActivityUpdate }, assetingest.ReasonUnsupportedActivity},
	}
	for _, c := range cases {
		e := f.deleteOf(a, f.tombstone(a, ""))
		if c.edit != nil {
			c.edit(e)
		}
		_, err := f.ingest.Withdraw(ctx, c.peer, e)
		if err == nil || assetingest.ReasonOf(err) != c.reason || assetingest.Retryable(err) {
			t.Fatalf("%s: %v (reason %q), want %q", c.name, err, assetingest.ReasonOf(err), c.reason)
		}
	}
	if n := f.tombstones(a.uri); n != 0 {
		t.Fatalf("%d tombstones kept from refused withdrawals", n)
	}
	if st, _ := f.state(in.RecordID); st != assetingest.StateComplete {
		t.Fatalf("record %s after refused withdrawals, want complete", st)
	}
	if n := f.pins(a.hash[0], assetingest.PinSubjectType); n != 1 {
		t.Fatalf("record pins %d after refused withdrawals, want 1", n)
	}
}

// The schema holds a withdrawn record final and its originals withdrawn
// only with it.
func TestWithdraw_SchemaHoldsTheTransitions(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	in, err := f.receive(f.envelope(a, a.member(f.version(a), 1, "public")))
	if err != nil || in.State != assetingest.StateComplete {
		t.Fatalf("import: %+v %v", in, err)
	}
	// An original is not withdrawn while its record is complete.
	if _, err := f.pool.Exec(ctx, `UPDATE federation_received_asset_files SET state = 'withdrawn' WHERE received_asset_id = $1`, in.RecordID); err == nil {
		t.Fatal("an original of a complete record was withdrawn")
	}
	// A complete record goes nowhere but withdrawn.
	if _, err := f.pool.Exec(ctx, `UPDATE federation_received_assets SET state = 'rejected', reason = 'fixity_mismatch', completed_at = NULL, authorizing_grant_id = NULL WHERE id = $1`, in.RecordID); err == nil {
		t.Fatal("a complete record was rejected")
	}
	if _, err := f.withdraw(f.deleteOf(a, f.tombstone(a, ""))); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE federation_received_assets SET state = 'complete', reason = NULL, withdrawn_at = NULL WHERE id = $1`,
		`UPDATE federation_received_assets SET attempts = attempts + 1 WHERE id = $1`,
		`UPDATE federation_received_asset_files SET state = 'verified' WHERE received_asset_id = $1`,
	} {
		if _, err := f.pool.Exec(ctx, q, in.RecordID); err == nil {
			t.Fatalf("a withdrawn record changed: %s", q)
		}
	}
	// Pinning the record again is refused at commit.
	if _, err := f.pool.Exec(ctx, `INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($1, $2, $3)`,
		a.hash[0], assetingest.PinSubjectType, in.RecordID.String()); err == nil {
		t.Fatal("a withdrawn record was pinned again")
	}
}
