// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetingest_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
)

// #1575 (spec v1 §17.2.1): later versions of a received asset. Each
// version is its own immutable record; a later one is accepted only as
// the successor of the version its predecessor names (one or more
// numbers below it, with nothing held between them), is fetched while
// the current version stays complete, and replaces it in one transaction.

// versionURI is the origin's id of a's version n.
func (f *fx) versionURI(a *asset, n int) string {
	return f.origin.srv.URL + "/assets/" + a.id.String() + "/versions/" + fmt.Sprint(n)
}

// originals adds n new original files to the origin and returns their
// file entries and hashes.
func (f *fx) originals(n int) ([]map[string]any, []string) {
	var entries []map[string]any
	var hashes []string
	for i := 0; i < n; i++ {
		body := make([]byte, 24<<10+i)
		_, _ = rand.Read(body)
		h := f.origin.add(body)
		hashes = append(hashes, h)
		entries = append(entries, fileEntry("original", h, len(body), fmt.Sprintf("v-part-%d.png", i)))
	}
	return entries, hashes
}

// memberAt is a's aa:asset member for version (id, number) with the given
// predecessor ("" for none) and file set.
func (a *asset) memberAt(id string, number int, predecessor string, files []map[string]any) map[string]any {
	v := map[string]any{"id": id, "number": number, "created": "2026-11-22T10:00:00Z", "predecessor": nil}
	if predecessor != "" {
		v["predecessor"] = predecessor
	}
	return map[string]any{"id": a.uri, "version": v, "files": files, "sensitivity": "public", "mature": false}
}

// announceV is the envelope announcing a's version n with files,
// following version n-1 (none for n = 1).
func (f *fx) announceV(a *asset, n int, files []map[string]any) *federation.Envelope {
	pred := ""
	if n > 1 {
		pred = f.versionURI(a, n-1)
	}
	return f.envelope(a, a.memberAt(f.versionURI(a, n), n, pred, files))
}

func (f *fx) schedule(e *federation.Envelope) (assetingest.Outcome, error) {
	return f.ingest.Schedule(context.Background(), assetingest.Announcement{PeerID: f.peerID, Envelope: e, Signer: f.signer})
}

func (f *fx) process(id uuid.UUID) (assetingest.Outcome, error) {
	return f.ingest.Process(context.Background(), id, f.signerFor())
}

// recordPins counts the pins the record holds.
func (f *fx) recordPins(id uuid.UUID) int {
	return f.count(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = $1 AND pin_subject_id = $2`,
		assetingest.PinSubjectType, id.String())
}

// chain lists the asset's records that are not rejected, oldest version
// first, as "number:state".
func (f *fx) chain(a *asset) string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT version_number, state FROM federation_received_assets
		WHERE peer_id = $1 AND origin_asset_uri = $2 AND state <> 'rejected' ORDER BY version_number`, f.peerID, a.uri)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n int
		var st string
		if err := rows.Scan(&n, &st); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d:%s", n, st))
	}
	return strings.Join(out, " ")
}

// current is the asset's complete record.
func (f *fx) current(a *asset) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM federation_received_assets
		WHERE peer_id = $1 AND origin_asset_uri = $2 AND state = 'complete'`, f.peerID, a.uri).Scan(&id); err != nil {
		f.t.Fatalf("no complete version: %v", err)
	}
	return id
}

func TestVersions_FirstSuccessorReplacesTheCurrentVersionOnceVerified(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	v1, err := f.receive(f.announceV(a, 1, a.files))
	if err != nil || v1.State != assetingest.StateComplete {
		t.Fatalf("version 1: %+v %v", v1, err)
	}
	files2, hash2 := f.originals(2)
	e2 := f.announceV(a, 2, files2)

	// Bound as the successor of version 1: pending, fetching nothing yet,
	// while version 1 stays complete and holds its pins.
	s2, err := f.schedule(e2)
	if err != nil || s2.State != assetingest.StatePending || s2.Reason != "" {
		t.Fatalf("version 2 scheduled: %+v %v", s2, err)
	}
	var predURI string
	if err := f.pool.QueryRow(ctx, `SELECT predecessor_version_uri FROM federation_received_assets WHERE id = $1`, s2.RecordID).Scan(&predURI); err != nil || predURI != f.versionURI(a, 1) {
		t.Fatalf("version 2 follows %q (%v)", predURI, err)
	}
	// While version 2's originals are fetched, version 1 is still the
	// asset's complete, pinned version.
	var during string
	assetingest.SetAfterFetch(f.ingest, func() {
		st, _ := f.state(v1.RecordID)
		during = fmt.Sprintf("%s/%d/%s", st, f.recordPins(v1.RecordID), f.chain(a))
	})
	out2, err := f.process(s2.RecordID)
	assetingest.SetAfterFetch(f.ingest, nil)
	if err != nil || out2.State != assetingest.StateComplete {
		t.Fatalf("version 2: %+v %v", out2, err)
	}
	if during != "complete/1/1:complete 2:pending" {
		t.Fatalf("while version 2 was fetched: %s", during)
	}
	// One transaction replaced it: version 1 superseded, keeping its
	// verified originals and pins; version 2 complete and linked to it.
	if got := f.chain(a); got != "1:superseded 2:complete" {
		t.Fatalf("chain %s", got)
	}
	var supersededAt, completedAt *time.Time
	var predID *uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT p.superseded_at, p.completed_at, s.predecessor_id FROM federation_received_assets p, federation_received_assets s
		WHERE p.id = $1 AND s.id = $2`, v1.RecordID, out2.RecordID).Scan(&supersededAt, &completedAt, &predID); err != nil ||
		supersededAt == nil || completedAt == nil || predID == nil || *predID != v1.RecordID {
		t.Fatalf("superseded_at %v completed_at %v predecessor %v (%v)", supersededAt, completedAt, predID, err)
	}
	if f.recordPins(v1.RecordID) != 1 || f.recordPins(out2.RecordID) != 2 {
		t.Fatalf("pins: version 1 %d, version 2 %d", f.recordPins(v1.RecordID), f.recordPins(out2.RecordID))
	}
	if n := f.count(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1 AND state = 'verified'`, v1.RecordID); n != 1 {
		t.Fatalf("version 1 keeps %d verified originals", n)
	}
	for _, h := range hash2 {
		if f.pins(h, casfetch.IngestPinType) != 0 {
			t.Fatal("an ingest pin was left")
		}
	}
	if n := f.count(`SELECT count(*) FROM audit_events WHERE event_type = $1 AND metadata->>'record_id' = $2 AND metadata->>'supersedes' = $3`,
		audit.EventFederationAssetIngestCompleted, out2.RecordID.String(), v1.RecordID.String()); n != 1 {
		t.Fatalf("%d completion audits naming the superseded version", n)
	}
	got := f.ledgerReasons(out2.RecordID)
	if len(got) != 3 || got[2] != "ingestion/success/" {
		t.Fatalf("version 2's ledger: %v", got)
	}

	t.Run("a repeated announcement of either version changes nothing", func(t *testing.T) {
		gets := f.origin.getCount(hash2[0])
		for _, tc := range []struct {
			name  string
			env   *federation.Envelope
			id    uuid.UUID
			state string
		}{
			{"version 2's activity again", e2, out2.RecordID, assetingest.StateComplete},
			{"version 2 in another activity", f.announceV(a, 2, files2), out2.RecordID, assetingest.StateComplete},
			{"version 1 in another activity", f.announceV(a, 1, a.files), v1.RecordID, assetingest.StateSuperseded},
		} {
			res, err := f.receive(tc.env)
			if err != nil || !res.Duplicate || res.RecordID != tc.id || res.State != tc.state {
				t.Fatalf("%s: %+v %v", tc.name, res, err)
			}
		}
		if f.origin.getCount(hash2[0]) != gets || f.chain(a) != "1:superseded 2:complete" {
			t.Fatalf("a duplicate fetched again or changed the chain: %s", f.chain(a))
		}
	})
	t.Run("a version id or number reused with other bytes fails closed", func(t *testing.T) {
		other, _ := f.originals(1)
		for _, tc := range []struct {
			name string
			env  *federation.Envelope
		}{
			{"version 2's id with another file set", f.envelope(a, a.memberAt(f.versionURI(a, 2), 2, f.versionURI(a, 1), other))},
			{"version 1's id with another file set", f.envelope(a, a.memberAt(f.versionURI(a, 1), 1, "", other))},
			{"another id at number 2", f.envelope(a, a.memberAt(f.versionURI(a, 2)+"-b", 2, f.versionURI(a, 1), other))},
			{"version 3 naming a predecessor that is not version 2", f.envelope(a, a.memberAt(f.versionURI(a, 3), 3, f.versionURI(a, 2)+"-b", other))},
		} {
			res, err := f.receive(tc.env)
			wantReason(t, err, assetingest.ReasonVersionConflict)
			if res.State != assetingest.StateRejected {
				t.Fatalf("%s: %+v", tc.name, res)
			}
		}
		if got := f.chain(a); got != "1:superseded 2:complete" || f.current(a) != out2.RecordID {
			t.Fatalf("a refused version changed the chain: %s", got)
		}
	})
	t.Run("the next successor supersedes version 2", func(t *testing.T) {
		files3, _ := f.originals(1)
		out3, err := f.receive(f.announceV(a, 3, files3))
		if err != nil || out3.State != assetingest.StateComplete {
			t.Fatalf("version 3: %+v %v", out3, err)
		}
		if got := f.chain(a); got != "1:superseded 2:superseded 3:complete" {
			t.Fatalf("chain %s", got)
		}
		if f.recordPins(v1.RecordID) != 1 || f.recordPins(out2.RecordID) != 2 || f.recordPins(out3.RecordID) != 1 {
			t.Fatal("an accepted version lost its pins")
		}
	})
}

func TestVersions_TheFirstVersionReceivedMayHaveAnyNumberAndNothingGoesBack(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	// A share that starts at version 3: received as the first version.
	v3, err := f.receive(f.announceV(a, 3, a.files))
	if err != nil || v3.State != assetingest.StateComplete {
		t.Fatalf("version 3 first: %+v %v", v3, err)
	}
	var pred *string
	if err := f.pool.QueryRow(context.Background(), `SELECT predecessor_version_uri FROM federation_received_assets WHERE id = $1`, v3.RecordID).Scan(&pred); err != nil || pred != nil {
		t.Fatalf("the first version received follows %v (%v)", pred, err)
	}
	// An older version, delayed or replayed: refused, never received.
	files2, hash2 := f.originals(1)
	res, err := f.receive(f.announceV(a, 2, files2))
	wantReason(t, err, assetingest.ReasonVersionStale)
	if res.State != assetingest.StateRejected || f.origin.getCount(hash2[0]) != 0 || f.current(a) != v3.RecordID {
		t.Fatalf("stale version: %+v, chain %s", res, f.chain(a))
	}
	files4, _ := f.originals(1)
	v4, err := f.receive(f.announceV(a, 4, files4))
	if err != nil || v4.State != assetingest.StateComplete || f.chain(a) != "3:superseded 4:complete" {
		t.Fatalf("version 4: %+v %v, chain %s", v4, err, f.chain(a))
	}
}

func TestVersions_ASuccessorBeforeItsPredecessorWaitsForIt(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	v1, err := f.receive(f.announceV(a, 1, a.files))
	if err != nil || v1.State != assetingest.StateComplete {
		t.Fatalf("version 1: %+v %v", v1, err)
	}
	// Version 3 arrives before version 2.
	files3, hash3 := f.originals(1)
	s3, err := f.schedule(f.announceV(a, 3, files3))
	if err != nil || s3.State != assetingest.StatePending {
		t.Fatalf("version 3 scheduled: %+v %v", s3, err)
	}
	if st, reason := f.state(s3.RecordID); st != assetingest.StatePending || reason != assetingest.ReasonAwaitingPredecessor || f.due(s3.RecordID) {
		t.Fatalf("version 3 is %s/%s, due %v", st, reason, f.due(s3.RecordID))
	}
	// Checked again: still waiting, no attempt, nothing fetched, one
	// ledger row for the wait.
	res, err := f.process(s3.RecordID)
	wantReason(t, err, assetingest.ReasonAwaitingPredecessor)
	if res.State != assetingest.StatePending || f.origin.getCount(hash3[0]) != 0 {
		t.Fatalf("version 3 checked: %+v", res)
	}
	if n := f.count(`SELECT attempts FROM federation_received_assets WHERE id = $1`, s3.RecordID); n != 0 {
		t.Fatalf("%d attempts counted while waiting", n)
	}
	if got := f.ledgerReasons(s3.RecordID); len(got) != 1 || got[0] != "ingestion/warning/awaiting_predecessor" {
		t.Fatalf("version 3's ledger: %v", got)
	}
	// Version 2 arrives and completes; that wakes version 3, which then
	// completes after it.
	files2, _ := f.originals(1)
	v2, err := f.receive(f.announceV(a, 2, files2))
	if err != nil || v2.State != assetingest.StateComplete {
		t.Fatalf("version 2: %+v %v", v2, err)
	}
	if !f.due(s3.RecordID) {
		t.Fatal("version 2's completion did not wake version 3")
	}
	out3, err := f.process(s3.RecordID)
	if err != nil || out3.State != assetingest.StateComplete {
		t.Fatalf("version 3: %+v %v", out3, err)
	}
	if got := f.chain(a); got != "1:superseded 2:superseded 3:complete" {
		t.Fatalf("chain %s", got)
	}

	t.Run("a successor of a version still being received waits for it", func(t *testing.T) {
		b := f.newAsset(1)
		f.grant(f.peerID, b.uri, nil)
		s1, err := f.schedule(f.announceV(b, 1, b.files))
		if err != nil || s1.State != assetingest.StatePending {
			t.Fatalf("version 1 scheduled: %+v %v", s1, err)
		}
		files2, _ := f.originals(1)
		s2, err := f.schedule(f.announceV(b, 2, files2))
		if st, reason := f.state(s2.RecordID); err != nil || st != assetingest.StatePending || reason != assetingest.ReasonAwaitingPredecessor {
			t.Fatalf("version 2 while version 1 arrives: %s/%s %v", st, reason, err)
		}
		if out, err := f.process(s1.RecordID); err != nil || out.State != assetingest.StateComplete || !f.due(s2.RecordID) {
			t.Fatalf("version 1: %+v %v, version 2 due %v", out, err, f.due(s2.RecordID))
		}
		if out, err := f.process(s2.RecordID); err != nil || out.State != assetingest.StateComplete {
			t.Fatalf("version 2: %+v %v", out, err)
		}
		if got := f.chain(b); got != "1:superseded 2:complete" {
			t.Fatalf("chain %s", got)
		}
	})
}

func TestVersions_TheWaitForAPredecessorIsBounded(t *testing.T) {
	f := newFx(t, assetingest.Config{PredecessorWait: time.Second})
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	v1, err := f.receive(f.announceV(a, 1, a.files))
	if err != nil || v1.State != assetingest.StateComplete {
		t.Fatalf("version 1: %+v %v", v1, err)
	}
	files3, hash3 := f.originals(1)
	s3, err := f.schedule(f.announceV(a, 3, files3))
	if err != nil {
		t.Fatalf("version 3: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM federation_received_assets
		WHERE id = $1 AND next_attempt_at <= received_at + interval '1 second'`, s3.RecordID); n != 1 {
		t.Fatal("the next check is scheduled past the wait")
	}
	time.Sleep(1200 * time.Millisecond)
	res, err := f.process(s3.RecordID)
	wantReason(t, err, assetingest.ReasonPredecessorMissing)
	if res.State != assetingest.StateRejected || f.origin.getCount(hash3[0]) != 0 {
		t.Fatalf("after the wait: %+v", res)
	}
	f.nothingUsable(hash3...)
	if got := f.chain(a); got != "1:complete" {
		t.Fatalf("chain %s", got)
	}
	if got := f.ledgerReasons(s3.RecordID); len(got) != 2 || got[1] != "ingestion/failure/predecessor_missing" {
		t.Fatalf("ledger: %v", got)
	}
}

func TestVersions_AFailedSuccessorLeavesTheCurrentVersion(t *testing.T) {
	f := newFx(t, assetingest.Config{MaxAttempts: 3, RetryBase: time.Millisecond, RetryMax: time.Millisecond})
	a := f.newAsset(1)
	share := f.grant(f.peerID, a.uri, nil)
	v1, err := f.receive(f.announceV(a, 1, a.files))
	if err != nil || v1.State != assetingest.StateComplete {
		t.Fatalf("version 1: %+v %v", v1, err)
	}

	t.Run("a delayed fetch is retried while version 1 stays current", func(t *testing.T) {
		files2, hash2 := f.originals(1)
		f.origin.setMode(hash2[0], "unavailable")
		res, err := f.receive(f.announceV(a, 2, files2))
		if !assetingest.Retryable(err) || res.State != assetingest.StatePending {
			t.Fatalf("version 2 during the outage: %+v %v", res, err)
		}
		if f.current(a) != v1.RecordID || f.recordPins(v1.RecordID) != 1 {
			t.Fatal("version 1 is no longer the current version during the outage")
		}
		f.nothingUsable(hash2...)
		f.origin.setMode(hash2[0], "")
		out, err := f.process(res.RecordID)
		if err != nil || out.State != assetingest.StateComplete || f.chain(a) != "1:superseded 2:complete" {
			t.Fatalf("version 2 after the outage: %+v %v, chain %s", out, err, f.chain(a))
		}
	})
	t.Run("a grant revoked during the transfer leaves the current version", func(t *testing.T) {
		v2 := f.current(a)
		files3, hash3 := f.originals(1)
		s3, err := f.schedule(f.announceV(a, 3, files3))
		if err != nil {
			t.Fatal(err)
		}
		assetingest.SetAfterFetch(f.ingest, func() { f.revoke(f.peerID, a.uri, share) })
		res, err := f.process(s3.RecordID)
		assetingest.SetAfterFetch(f.ingest, nil)
		wantReason(t, err, casfetch.ReasonRevokedDuringFetch)
		if res.State != assetingest.StateRejected {
			t.Fatalf("version 3: %+v", res)
		}
		f.nothingUsable(hash3...)
		if f.current(a) != v2 || f.recordPins(v2) != 1 || f.chain(a) != "1:superseded 2:complete" {
			t.Fatalf("the revoke changed the held versions: %s", f.chain(a))
		}
	})
}

func TestVersions_AWithdrawalStopsASuccessorAndWithdrawsEveryVersion(t *testing.T) {
	f := newFx(t)

	t.Run("during the successor's transfer", func(t *testing.T) {
		a := f.newAsset(1)
		f.grant(f.peerID, a.uri, nil)
		v1, err := f.receive(f.announceV(a, 1, a.files))
		if err != nil || v1.State != assetingest.StateComplete {
			t.Fatalf("version 1: %+v %v", v1, err)
		}
		files2, hash2 := f.originals(1)
		e2 := f.announceV(a, 2, files2)
		s2, err := f.schedule(e2)
		if err != nil {
			t.Fatal(err)
		}
		assetingest.SetAfterFetch(f.ingest, func() {
			if _, err := f.withdraw(f.deleteOf(a, f.tombstone(a, ""))); err != nil {
				t.Errorf("withdraw: %v", err)
			}
		})
		res, err := f.process(s2.RecordID)
		assetingest.SetAfterFetch(f.ingest, nil)
		wantReason(t, err, assetingest.ReasonWithdrawn)
		if res.State != assetingest.StateWithdrawn {
			t.Fatalf("version 2: %+v", res)
		}
		if got := f.chain(a); got != "1:withdrawn 2:withdrawn" {
			t.Fatalf("chain %s", got)
		}
		f.nothingUsable(append(append([]string{}, a.hash...), hash2...)...)
		// Neither a replay nor a later version brings it back.
		if res, err := f.receive(e2); assetingest.ReasonOf(err) != assetingest.ReasonWithdrawn || res.State != assetingest.StateWithdrawn || !res.Duplicate {
			t.Fatalf("replay of version 2: %+v %v", res, err)
		}
		files3, _ := f.originals(1)
		if res, err := f.receive(f.announceV(a, 3, files3)); assetingest.ReasonOf(err) != assetingest.ReasonWithdrawn || res.State != assetingest.StateRejected {
			t.Fatalf("version 3 after the withdrawal: %+v %v", res, err)
		}
		if got := f.chain(a); got != "1:withdrawn 2:withdrawn" {
			t.Fatalf("chain after replays %s", got)
		}
	})
	t.Run("after the successor replaced version 1", func(t *testing.T) {
		a := f.newAsset(1)
		f.grant(f.peerID, a.uri, nil)
		v1, err := f.receive(f.announceV(a, 1, a.files))
		if err != nil {
			t.Fatal(err)
		}
		files2, hash2 := f.originals(1)
		v2, err := f.receive(f.announceV(a, 2, files2))
		if err != nil || v2.State != assetingest.StateComplete {
			t.Fatalf("version 2: %+v %v", v2, err)
		}
		w, err := f.withdraw(f.deleteOf(a, f.tombstone(a, "")))
		if err != nil || len(w.Records) != 2 {
			t.Fatalf("withdraw: %+v %v", w, err)
		}
		if got := f.chain(a); got != "1:withdrawn 2:withdrawn" {
			t.Fatalf("chain %s", got)
		}
		f.nothingUsable(append(append([]string{}, a.hash...), hash2...)...)
		if len(f.deaccessions(v1.RecordID)) != 1 || len(f.deaccessions(v2.RecordID)) != 1 {
			t.Fatal("a version has no deaccession")
		}
	})
}

func TestVersions_AnotherPeerCannotAnnounceAVersion(t *testing.T) {
	f := newFx(t)
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	v1, err := f.receive(f.announceV(a, 1, a.files))
	if err != nil || v1.State != assetingest.StateComplete {
		t.Fatalf("version 1: %+v %v", v1, err)
	}
	other := newOrigin(t)
	otherPeer := f.addPeer(other.srv.URL, bothProfiles)
	files2, hash2 := f.originals(1)
	for _, actor := range []string{other.srv.URL + "/users/mallory", f.origin.srv.URL + "/users/alice"} {
		e := f.announceV(a, 2, files2)
		e.Actor = actor
		_, err := f.ingest.Receive(context.Background(), assetingest.Announcement{PeerID: otherPeer, Envelope: e, Signer: f.signer})
		wantReason(t, err, casfetch.ReasonForeignOrigin)
	}
	if n := f.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1`, otherPeer); n != 0 {
		t.Fatalf("%d records for the other peer", n)
	}
	if f.origin.getCount(hash2[0]) != 0 || f.chain(a) != "1:complete" {
		t.Fatalf("the other peer's announcement changed the chain: %s", f.chain(a))
	}
}

func TestVersions_TheDatabaseHoldsTheChain(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	v1, err := f.receive(f.announceV(a, 1, a.files))
	if err != nil {
		t.Fatal(err)
	}
	b := f.newAsset(1)
	f.grant(f.peerID, b.uri, nil)
	other, err := f.receive(f.announceV(b, 1, b.files))
	if err != nil {
		t.Fatal(err)
	}
	files2, _ := f.originals(1)
	s2, err := f.schedule(f.announceV(a, 2, files2))
	if err != nil {
		t.Fatal(err)
	}
	refused := func(name, sql string, args ...any) {
		t.Helper()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, sql, args...); err == nil {
			if err := tx.Commit(ctx); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		}
	}
	refused("a superseded version without a successor",
		`UPDATE federation_received_assets SET state = 'superseded', superseded_at = now() WHERE id = $1`, v1.RecordID)
	refused("a predecessor link to another asset's record",
		`UPDATE federation_received_assets SET predecessor_id = $2 WHERE id = $1`, s2.RecordID, other.RecordID)
	refused("a second complete version",
		`UPDATE federation_received_assets SET state = 'complete', reason = NULL, completed_at = now(), authorizing_grant_id = $2, predecessor_id = $3 WHERE id = $1`,
		s2.RecordID, v1.GrantID, v1.RecordID)
	refused("a second record of version 1",
		`INSERT INTO federation_received_assets (peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
		   file_set_sha256, record, sensitivity, signer_key_id, state)
		 SELECT peer_id, activity_uri || '-copy', actor_uri, origin_asset_uri, origin_asset_id, version_uri || '-copy', version_number,
		   file_set_sha256, record, sensitivity, signer_key_id, 'pending' FROM federation_received_assets WHERE id = $1`, v1.RecordID)
	if out, err := f.process(s2.RecordID); err != nil || out.State != assetingest.StateComplete {
		t.Fatalf("version 2: %+v %v", out, err)
	}
	refused("a superseded version made complete again",
		`UPDATE federation_received_assets SET state = 'complete', superseded_at = NULL WHERE id = $1`, v1.RecordID)
	refused("a complete version relinked",
		`UPDATE federation_received_assets SET predecessor_id = NULL WHERE id = $1`, s2.RecordID)
	if got := f.chain(a); got != "1:superseded 2:complete" {
		t.Fatalf("chain %s", got)
	}
}

// withVersion is a copy of member with version.key set to value (nil
// removes nothing: it sets JSON null).
func withVersion(member map[string]any, key string, value any) map[string]any {
	out := map[string]any{}
	for k, v := range member {
		out[k] = v
	}
	ver := map[string]any{}
	for k, v := range member["version"].(map[string]any) {
		ver[k] = v
	}
	ver[key] = value
	out["version"] = ver
	return out
}

// A version is immutable once announced (§17.2.1): a new announcement of
// a recorded version id with the same bytes but another predecessor or
// creation time is a different version claim and fails closed, leaving
// the recorded version, its pins and the chain as they were. The same
// claims again, the same time written another way, or a difference in
// descriptive fields only, are the same version: a duplicate, with the
// stored record kept as it was.
func TestVersions_AVersionIsImmutableOnceAnnounced(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	v1, err := f.receive(f.announceV(a, 1, a.files))
	if err != nil {
		t.Fatal(err)
	}
	files2, hash2 := f.originals(1)
	v2member := a.memberAt(f.versionURI(a, 2), 2, f.versionURI(a, 1), files2)
	v2, err := f.receive(f.envelope(a, v2member))
	if err != nil || v2.State != assetingest.StateComplete {
		t.Fatalf("version 2: %+v %v", v2, err)
	}
	stored := func(id uuid.UUID) string {
		var raw string
		if err := f.pool.QueryRow(ctx, `SELECT record::text FROM federation_received_assets WHERE id = $1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before1, before2 := stored(v1.RecordID), stored(v2.RecordID)
	records := f.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND origin_asset_uri = $2`, f.peerID, a.uri)
	gets := f.origin.getCount(hash2[0])
	unchanged := func(name string) {
		t.Helper()
		if got := f.chain(a); got != "1:superseded 2:complete" || f.current(a) != v2.RecordID {
			t.Fatalf("%s: chain %s", name, got)
		}
		if f.recordPins(v1.RecordID) != 1 || f.recordPins(v2.RecordID) != 1 {
			t.Fatalf("%s: pins %d %d", name, f.recordPins(v1.RecordID), f.recordPins(v2.RecordID))
		}
		if stored(v1.RecordID) != before1 || stored(v2.RecordID) != before2 {
			t.Fatalf("%s: a stored version's record was rewritten", name)
		}
		if f.origin.getCount(hash2[0]) != gets {
			t.Fatalf("%s: fetched again", name)
		}
	}

	for _, tc := range []struct {
		name   string
		member map[string]any
	}{
		{"version 2 with another predecessor", withVersion(v2member, "predecessor", f.versionURI(a, 1)+"-elsewhere")},
		{"version 2 with another creation time", withVersion(v2member, "created", "2026-11-23T10:00:00Z")},
		{"version 1 (superseded) with another creation time", withVersion(a.memberAt(f.versionURI(a, 1), 1, "", a.files), "created", "2026-11-21T10:00:00Z")},
	} {
		res, err := f.receive(f.envelope(a, tc.member))
		wantReason(t, err, assetingest.ReasonVersionConflict)
		if res.State != assetingest.StateRejected || res.RecordID == v1.RecordID || res.RecordID == v2.RecordID {
			t.Fatalf("%s: %+v", tc.name, res)
		}
		unchanged(tc.name)
		records++
	}
	if n := f.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND origin_asset_uri = $2 AND state = 'rejected' AND reason = 'version_conflict'`,
		f.peerID, a.uri); n != 3 {
		t.Fatalf("%d version_conflict refusals recorded, want 3", n)
	}

	described := withVersion(v2member, "created", "2026-11-22T10:00:00Z")
	described["description"] = map[string]any{"title": "A new title"}
	for _, tc := range []struct {
		name   string
		member map[string]any
		want   uuid.UUID
		state  string
	}{
		{"version 2 again in another activity", v2member, v2.RecordID, assetingest.StateComplete},
		{"version 2 with the same creation time written another way", withVersion(v2member, "created", "2026-11-22T11:00:00+01:00"), v2.RecordID, assetingest.StateComplete},
		{"version 2 differing only in descriptive fields", described, v2.RecordID, assetingest.StateComplete},
		{"version 1 again in another activity", a.memberAt(f.versionURI(a, 1), 1, "", a.files), v1.RecordID, assetingest.StateSuperseded},
	} {
		res, err := f.receive(f.envelope(a, tc.member))
		if err != nil || !res.Duplicate || res.RecordID != tc.want || res.State != tc.state {
			t.Fatalf("%s: %+v %v", tc.name, res, err)
		}
		unchanged(tc.name)
	}
	if n := f.count(`SELECT count(*) FROM federation_received_assets WHERE peer_id = $1 AND origin_asset_uri = $2`, f.peerID, a.uri); n != records {
		t.Fatalf("%d records, want %d: a duplicate wrote a record", n, records)
	}
}

// announceAfter is the envelope announcing a's version n with files,
// following version p (0 for none). A sender passes over a version that
// stayed a draft, so p may be more than one below n (migration 00093).
func (f *fx) announceAfter(a *asset, n, p int, files []map[string]any) *federation.Envelope {
	pred := ""
	if p > 0 {
		pred = f.versionURI(a, p)
	}
	return f.envelope(a, a.memberAt(f.versionURI(a, n), n, pred, files))
}

// predecessorOf is the record a complete record is linked to.
func (f *fx) predecessorOf(id uuid.UUID) uuid.UUID {
	f.t.Helper()
	var p *uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT predecessor_id FROM federation_received_assets WHERE id = $1`, id).Scan(&p); err != nil || p == nil {
		f.t.Fatalf("record %s has no predecessor link (%v)", id, err)
	}
	return *p
}

// A sender never announces a version that stayed a draft, so the next
// version it announces names the version below the draft and skips its
// number (#1575, migration 00093). The receiver accepts it as the
// successor of the version it names, and only while the chain stays
// consistent with what it holds.
func TestVersions_ASuccessorMayPassOverANumberThatWasNeverAnnounced(t *testing.T) {
	f := newFx(t)
	received := func(a *asset, n int) uuid.UUID {
		t.Helper()
		out, err := f.receive(f.announceV(a, n, a.files))
		if err != nil || out.State != assetingest.StateComplete {
			t.Fatalf("version %d: %+v %v", n, out, err)
		}
		return out.RecordID
	}
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	v1 := received(a, 1)
	files3, _ := f.originals(1)
	// Bound as version 1's successor, due at once: it waits for nothing.
	s3, err := f.schedule(f.announceAfter(a, 3, 1, files3))
	if err != nil || s3.State != assetingest.StatePending || s3.Reason != "" || !f.due(s3.RecordID) {
		t.Fatalf("version 3 after version 1 scheduled: %+v %v, due %v", s3, err, f.due(s3.RecordID))
	}
	out3, err := f.process(s3.RecordID)
	if err != nil || out3.State != assetingest.StateComplete {
		t.Fatalf("version 3 after version 1: %+v %v", out3, err)
	}
	if got := f.chain(a); got != "1:superseded 3:complete" || f.predecessorOf(out3.RecordID) != v1 {
		t.Fatalf("chain %s", got)
	}
	files4, _ := f.originals(1)
	if out4, err := f.receive(f.announceAfter(a, 4, 3, files4)); err != nil || out4.State != assetingest.StateComplete {
		t.Fatalf("version 4 after version 3: %+v %v", out4, err)
	}
	if res, err := f.receive(f.announceAfter(a, 3, 1, files3)); err != nil || !res.Duplicate || res.RecordID != out3.RecordID {
		t.Fatalf("version 3 again: %+v %v", res, err)
	}
	if got := f.chain(a); got != "1:superseded 3:superseded 4:complete" {
		t.Fatalf("chain %s", got)
	}

	t.Run("a successor before the version it names waits and is woken", func(t *testing.T) {
		b := f.newAsset(1)
		f.grant(f.peerID, b.uri, nil)
		received(b, 1)
		files5, hash5 := f.originals(1)
		s5, err := f.schedule(f.announceAfter(b, 5, 3, files5))
		if st, reason := f.state(s5.RecordID); err != nil || st != assetingest.StatePending || reason != assetingest.ReasonAwaitingPredecessor {
			t.Fatalf("version 5 before version 3: %s/%s %v", st, reason, err)
		}
		if res, err := f.process(s5.RecordID); res.State != assetingest.StatePending || f.origin.getCount(hash5[0]) != 0 {
			t.Fatalf("version 5 checked: %+v %v", res, err)
		}
		files3, _ := f.originals(1)
		out3, err := f.receive(f.announceAfter(b, 3, 1, files3))
		if err != nil || out3.State != assetingest.StateComplete {
			t.Fatalf("version 3: %+v %v", out3, err)
		}
		if !f.due(s5.RecordID) {
			t.Fatal("version 3's completion did not wake version 5")
		}
		if out5, err := f.process(s5.RecordID); err != nil || out5.State != assetingest.StateComplete || f.predecessorOf(out5.RecordID) != out3.RecordID {
			t.Fatalf("version 5: %+v %v", out5, err)
		}
		if got := f.chain(b); got != "1:superseded 3:superseded 5:complete" {
			t.Fatalf("chain %s", got)
		}
	})

	t.Run("a chain that contradicts what is held fails closed", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			setup func(b *asset) // versions held before the announcement
			n, p  int
			chain string // the chain after the refusal
		}{
			{"a version held between it and the version it names", func(b *asset) { received(b, 1); received(b, 2) }, 3, 1, "1:superseded 2:complete"},
			{"a version still being received between them", func(b *asset) {
				received(b, 1)
				files, _ := f.originals(1)
				if _, err := f.schedule(f.announceV(b, 2, files)); err != nil {
					t.Fatal(err)
				}
			}, 3, 1, "1:complete 2:pending"},
			{"a predecessor numbered above it", func(b *asset) {
				received(b, 1)
				files, _ := f.originals(1)
				if _, err := f.receive(f.announceAfter(b, 3, 1, files)); err != nil {
					t.Fatal(err)
				}
			}, 2, 3, "1:superseded 3:complete"},
			{"a later version that passes over it", func(b *asset) {
				received(b, 1)
				files, _ := f.originals(1)
				if _, err := f.schedule(f.announceAfter(b, 4, 1, files)); err != nil {
					t.Fatal(err)
				}
			}, 3, 2, "1:complete 4:pending"},
			{"a later version that follows it with a version held between them", func(b *asset) {
				received(b, 1)
				files, _ := f.originals(1)
				if _, err := f.schedule(f.announceAfter(b, 5, 3, files)); err != nil {
					t.Fatal(err)
				}
				if _, err := f.schedule(f.announceAfter(b, 4, 2, files)); err != nil {
					t.Fatal(err)
				}
			}, 3, 1, "1:complete 4:pending 5:pending"},
			{"a later version that already follows the same version", func(b *asset) {
				received(b, 1)
				files, _ := f.originals(1)
				if _, err := f.schedule(f.announceAfter(b, 5, 3, files)); err != nil {
					t.Fatal(err)
				}
				if _, err := f.schedule(f.announceAfter(b, 4, 1, files)); err != nil {
					t.Fatal(err)
				}
			}, 3, 1, "1:complete 4:pending 5:pending"},
			{"a version waiting on a predecessor not held, one number below", func(b *asset) { received(b, 1); received(b, 2) }, 3, 0, "1:superseded 2:complete"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				b := f.newAsset(1)
				f.grant(f.peerID, b.uri, nil)
				tc.setup(b)
				files, hashes := f.originals(1)
				env := f.announceAfter(b, tc.n, tc.p, files)
				if tc.p == 0 {
					// A predecessor the receiver does not hold.
					env = f.envelope(b, b.memberAt(f.versionURI(b, tc.n), tc.n, f.versionURI(b, tc.n)+"-elsewhere", files))
				}
				res, err := f.receive(env)
				wantReason(t, err, assetingest.ReasonVersionConflict)
				if res.State != assetingest.StateRejected || f.origin.getCount(hashes[0]) != 0 {
					t.Fatalf("%+v", res)
				}
				if got := f.chain(b); got != tc.chain {
					t.Fatalf("a refused version changed the chain: %s", got)
				}
			})
		}
	})
}

func TestVersions_TheWaitForAPredecessorAcrossAGapIsBounded(t *testing.T) {
	f := newFx(t, assetingest.Config{PredecessorWait: time.Second})
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	if v1, err := f.receive(f.announceV(a, 1, a.files)); err != nil || v1.State != assetingest.StateComplete {
		t.Fatalf("version 1: %+v %v", v1, err)
	}
	files5, hash5 := f.originals(1)
	s5, err := f.schedule(f.announceAfter(a, 5, 3, files5))
	if st, reason := f.state(s5.RecordID); err != nil || st != assetingest.StatePending || reason != assetingest.ReasonAwaitingPredecessor {
		t.Fatalf("version 5 naming an unknown version 3: %s/%s %v", st, reason, err)
	}
	time.Sleep(1200 * time.Millisecond)
	res, err := f.process(s5.RecordID)
	wantReason(t, err, assetingest.ReasonPredecessorMissing)
	if res.State != assetingest.StateRejected || f.origin.getCount(hash5[0]) != 0 {
		t.Fatalf("after the wait: %+v", res)
	}
	f.nothingUsable(hash5...)
	if got := f.chain(a); got != "1:complete" {
		t.Fatalf("chain %s", got)
	}
}

// The chain trigger (migration 00093) accepts a link to a lower version
// with the version id the record names, more than one number below it,
// and refuses a link to a version that is not lower, or one that passes
// over a version held between the two.
func TestVersions_TheDatabaseHoldsAChainWithGaps(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	a := f.newAsset(1)
	f.grant(f.peerID, a.uri, nil)
	v1, err := f.receive(f.announceV(a, 1, a.files))
	if err != nil {
		t.Fatal(err)
	}
	files2, _ := f.originals(1)
	s2, err := f.schedule(f.announceV(a, 2, files2))
	if err != nil {
		t.Fatal(err)
	}
	// insertLinked writes, and rolls back, a pending record of version n
	// that names the record pred, and returns the error.
	insertLinked := func(n int, pred uuid.UUID) error {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		_, err = tx.Exec(ctx, `INSERT INTO federation_received_assets (peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id,
			   version_uri, version_number, file_set_sha256, record, sensitivity, signer_key_id, state, predecessor_version_uri, predecessor_id)
			 SELECT r.peer_id, r.activity_uri || '-gap', r.actor_uri, r.origin_asset_uri, r.origin_asset_id, $3, $2,
			   r.file_set_sha256, r.record, r.sensitivity, r.signer_key_id, 'pending', p.version_uri, p.id
			  FROM federation_received_assets r, federation_received_assets p WHERE r.id = $1 AND p.id = $4`,
			v1.RecordID, n, f.versionURI(a, n), pred)
		return err
	}
	const lower, passed = "is not an earlier version", "is passed over"
	for _, tc := range []struct {
		name string
		n    int
		pred uuid.UUID
		want string // "" accepted by the trigger
	}{
		{"a link two numbers down", 4, s2.RecordID, ""},
		{"a link to a later version", 1, s2.RecordID, lower},
		{"a link to the same number", 2, s2.RecordID, lower},
		{"a link passing over a held version", 3, v1.RecordID, passed},
	} {
		err := insertLinked(tc.n, tc.pred)
		switch {
		case tc.want == "" && err != nil && (strings.Contains(err.Error(), lower) || strings.Contains(err.Error(), passed)):
			t.Fatalf("%s: refused by the chain trigger: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Fatalf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}
