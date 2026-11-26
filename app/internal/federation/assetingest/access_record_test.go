// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: recording a received access copy's outcome (access_record.go,
// migration 00104), against a live database, the fs storage backend, the
// production casfetch client (class access, version-bound route) and the
// production inbound grant record. Pinned here:
//
//   - access rows of a completed record start unattempted; originals and
//     preservation entries are untouched by recording;
//   - a copy is recorded only for the current complete version, under a
//     live access-copy grant (an original grant is not one) and all four
//     profiles, checked again in the recording transaction;
//   - only the result's own ingest pin is consumed, for the exact entry;
//     a refusal changes nothing (atomic) and leaves the pin to the caller;
//   - a second success is a duplicate; failures are per file and never
//     overwrite a verified copy;
//   - after supersession a version keeps its copies and records nothing
//     new; withdrawal releases the record's pins (once per hash), keeps
//     verified_at and never touches another owner's pin.
//
// Skips without AA_DB_PASSWORD.

package assetingest_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/assetingest"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

const accessProfiles = `["share-lifecycle-1", "archival-1", "original-transfer-1", "derived-transfer-1"]`

// Positions of the access fixture's file set.
const (
	posOriginal     = 0
	posAccessB      = 1
	posAccessC      = 2
	posPreservation = 3
	posAccessDup    = 4 // an access entry of the original's own bytes
)

// accessFx is a received version with one original, two access copies,
// a preservation entry and an access entry of the original's bytes.
type accessFx struct {
	*fx
	a      *asset
	id     uuid.UUID
	hB, hC string
	bodyB  []byte
	share  string
}

func (f *fx) addFile(role string, size int) (string, []byte, map[string]any) {
	body := make([]byte, size)
	_, _ = rand.Read(body)
	h := f.origin.add(body)
	return h, body, fileEntry(role, h, size, "")
}

// grantFor records a live peer-wide grant of a with the given byte grants.
func (f *fx) grantFor(objectURI string, original, access bool) string {
	f.t.Helper()
	act := f.origin.srv.URL + "/activities/share-" + uuid.NewString()
	if _, err := f.grants.ApplyShare(context.Background(), inboundgrants.Share{
		PeerID: f.peerID, ActivityURI: act, GrantorActorURI: f.origin.srv.URL + "/users/alice",
		ObjectKind: federation.ShareObjectKindAsset, ObjectURI: objectURI, Scope: federation.ShareScopeView,
		AllowOriginal: original, AllowAccessCopies: access,
	}); err != nil {
		f.t.Fatal(err)
	}
	return act
}

func (f *fx) setCaps(caps string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE federation_peers SET capabilities = $2::jsonb WHERE id = $1`, f.peerID, caps); err != nil {
		f.t.Fatal(err)
	}
}

// newAccessFx receives the fixture's version 1 under a grant with
// originals and access copies: complete, or, with original=false, still
// pending because the origin answers the original's GET with a transient
// 503 (the access copies are served).
func newAccessFx(t *testing.T, original bool) *accessFx {
	t.Helper()
	f := newFx(t)
	f.setCaps(accessProfiles)
	a := f.newAsset(1)
	x := &accessFx{fx: f, a: a}
	var eb, ec, ep map[string]any
	x.hB, x.bodyB, eb = f.addFile("access", 20<<10)
	x.hC, _, ec = f.addFile("access", 21<<10)
	_, _, ep = f.addFile("preservation", 22<<10)
	a.files = append(a.files, eb, ec, ep, fileEntry("access", a.hash[0], 32<<10, ""))
	x.share = f.grantFor(a.uri, true, true)
	if !original {
		f.origin.setMode(a.hash[0], "unavailable")
	}
	out, err := f.receive(f.announceV(a, 1, a.files))
	if original && (err != nil || out.State != assetingest.StateComplete) {
		t.Fatalf("receive: %+v %v", out, err)
	}
	if !original && out.State != assetingest.StatePending {
		t.Fatalf("receive with the original unavailable: %+v %v", out, err)
	}
	x.id = out.RecordID
	return x
}

// work is the persisted identity of the entry at pos, built from the
// fixture's known values (LoadAccessWork refuses a record that is not
// the current version).
func (x *accessFx) work(pos int) assetingest.AccessWork {
	x.t.Helper()
	hash, size := x.a.hash[0], int64(32<<10)
	switch pos {
	case posAccessB:
		hash, size = x.hB, int64(len(x.bodyB))
	case posAccessC:
		hash, size = x.hC, 21<<10
	}
	return assetingest.AccessWork{
		RecordID: x.id, PeerID: x.peerID, OriginAssetURI: x.a.uri, OriginAssetID: x.a.id,
		VersionURI: x.versionURI(x.a, 1), VersionNumber: 1, Sensitivity: "public", Position: pos,
		SHA256: hash, Size: size, Checksums: []federation.ArchivalChecksum{{Algorithm: "sha256", Value: hash}},
		MediaType: "image/png", SignerKeyID: x.signer.KeyID,
	}
}

// fetch receives w's access copy with the production client.
func (x *accessFx) fetch(w assetingest.AccessWork) *casfetch.Result {
	x.t.Helper()
	req, err := w.Request(x.signer)
	if err != nil {
		x.t.Fatal(err)
	}
	res, err := x.client.Fetch(context.Background(), req)
	if err != nil {
		x.t.Fatalf("fetch access copy: %v", err)
	}
	return res
}

// release is the caller's contract for a result not consumed.
func (x *accessFx) release(res *casfetch.Result) {
	x.t.Helper()
	if err := x.svc.RemovePin(context.WithoutCancel(context.Background()), res.Pin, res.Hash); err != nil {
		x.t.Fatal(err)
	}
}

type fileRow struct {
	state      string
	reason     *string
	verifiedAt *time.Time
	detected   *string
}

func (x *accessFx) row(id uuid.UUID, pos int) fileRow {
	x.t.Helper()
	var r fileRow
	if err := x.pool.QueryRow(context.Background(), `SELECT state, access_failure_reason, verified_at, detected_media_type
		FROM federation_received_asset_files WHERE received_asset_id = $1 AND position = $2`, id, pos).
		Scan(&r.state, &r.reason, &r.verifiedAt, &r.detected); err != nil {
		x.t.Fatal(err)
	}
	return r
}

// snapshot is everything recording must not change: the record row and
// every other file row.
func (x *accessFx) snapshot(id uuid.UUID, except int) string {
	x.t.Helper()
	var s string
	if err := x.pool.QueryRow(context.Background(), `SELECT row_to_json(r)::text || (
		    SELECT json_agg(f ORDER BY f.position)::text FROM federation_received_asset_files f
		     WHERE f.received_asset_id = r.id AND f.position <> $2)
		  FROM federation_received_assets r WHERE r.id = $1`, id, except).Scan(&s); err != nil {
		x.t.Fatal(err)
	}
	return s
}

func wantNoAccessRecorded(t *testing.T, x *accessFx, res *casfetch.Result, pos int) {
	t.Helper()
	if r := x.row(x.id, pos); r.state != assetingest.FileStateNotTransferred || r.verifiedAt != nil {
		t.Fatalf("position %d recorded: %+v", pos, r)
	}
	if n := x.pins(res.Hash, casfetch.IngestPinType); n != 1 {
		t.Fatalf("the caller's ingest pin: %d, want still held", n)
	}
	if pos != posAccessDup && x.recordPins(x.id) != 1 { // only the original's
		t.Fatalf("record pins %d, want the original's only", x.recordPins(x.id))
	}
}

// Rows recorded by the original transfer start unattempted; recording a
// copy changes only its own row, and the DB refuses a verified copy with
// no record pin.
func TestAccessRecord_OldClaimsAndInvariants(t *testing.T) {
	x := newAccessFx(t, true)
	ctx := context.Background()
	for pos, want := range map[int]string{posOriginal: "verified", posAccessB: "not_transferred", posAccessC: "not_transferred",
		posPreservation: "not_transferred", posAccessDup: "not_transferred"} {
		if r := x.row(x.id, pos); r.state != want || r.reason != nil {
			t.Fatalf("position %d: %+v, want %s and no reason", pos, r, want)
		}
	}
	// The database alone refuses a verified copy the record does not pin.
	_, err := x.pool.Exec(ctx, `UPDATE federation_received_asset_files SET state = 'verified', verified_at = now()
		WHERE received_asset_id = $1 AND position = $2`, x.id, posAccessB)
	if err == nil {
		t.Fatal("a verified access copy without its record pin was committed")
	}
	before := x.snapshot(x.id, posAccessB)
	w, err := x.ingest.LoadAccessWork(ctx, x.id, posAccessB)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.Checksums, x.work(posAccessB).Checksums) || w.SHA256 != x.hB || w.VersionNumber != 1 {
		t.Fatalf("loaded work %+v", w)
	}
	res := x.fetch(w)
	out, err := x.ingest.RecordAccessCopy(ctx, w, res)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != assetingest.FileStateVerified || !out.Consumed || out.Duplicate || out.VerifiedAt.IsZero() {
		t.Fatalf("outcome %+v", out)
	}
	if after := x.snapshot(x.id, posAccessB); after != before {
		t.Fatalf("recording changed the record or another file:\n%s\n%s", before, after)
	}
	if r := x.row(x.id, posAccessB); r.state != "verified" || r.verifiedAt == nil || !r.verifiedAt.Equal(out.VerifiedAt) || r.detected != nil {
		t.Fatalf("row %+v", r)
	}
	if x.pins(x.hB, casfetch.IngestPinType) != 0 || x.pins(x.hB, assetingest.PinSubjectType) != 1 ||
		x.count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NULL`, x.hB) != 1 {
		t.Fatal("the ingest pin was not moved to the record")
	}
	// Preservation and original entries are not access work.
	for _, pos := range []int{posOriginal, posPreservation} {
		if _, err := x.ingest.LoadAccessWork(ctx, x.id, pos); assetingest.ReasonOf(err) != assetingest.ReasonAccessWorkChanged {
			t.Fatalf("position %d as access work: %v", pos, err)
		}
	}
}

// The copy is recorded only after the original completed: a pending
// record (its original not yet received) records nothing, even with a
// live access-copy grant and a verified fetch; once the original
// completes, the same copy records.
func TestAccessRecord_OnlyAfterOriginalCompletion(t *testing.T) {
	x := newAccessFx(t, false)
	ctx := context.Background()
	if _, err := x.ingest.LoadAccessWork(ctx, x.id, posAccessB); assetingest.ReasonOf(err) != assetingest.ReasonNotCurrentVersion {
		t.Fatalf("load of a pending version: %v", err)
	}
	res := x.fetch(x.work(posAccessB))
	_, err := x.ingest.RecordAccessCopy(ctx, x.work(posAccessB), res)
	wantReason(t, err, assetingest.ReasonNotCurrentVersion)
	_, err = x.ingest.RecordAccessFailure(ctx, x.work(posAccessC), casfetch.ReasonTimeout)
	wantReason(t, err, assetingest.ReasonNotCurrentVersion)
	if r := x.row(x.id, posAccessB); r.state != "not_transferred" || x.pins(x.hB, casfetch.IngestPinType) != 1 || x.recordPins(x.id) != 0 {
		t.Fatalf("recorded for a pending version: %+v", r)
	}
	x.release(res)
	x.nothingUsable(x.hB)
	x.origin.setMode(x.a.hash[0], "")
	if out, err := x.process(x.id); err != nil || out.State != assetingest.StateComplete {
		t.Fatalf("the original completes: %+v %v", out, err)
	}
	w, err := x.ingest.LoadAccessWork(ctx, x.id, posAccessB)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := x.ingest.RecordAccessCopy(ctx, w, x.fetch(w)); err != nil || !out.Consumed {
		t.Fatalf("after completion: %+v %v", out, err)
	}
}

// Authorisation is checked again in the recording transaction: an
// original-only grant, a downgrade, a disabled peer or a moved peer
// records nothing, and the caller keeps its pin.
func TestAccessRecord_AuthorisationRecheckedBeforeRecording(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(x *accessFx)
		want   string
	}{
		"access grant replaced by an original-only grant": {func(x *accessFx) {
			x.revoke(x.peerID, x.a.uri, x.share)
			x.grantFor(x.a.uri, true, false)
		}, casfetch.ReasonRevokedDuringFetch},
		"access grant revoked": {func(x *accessFx) { x.revoke(x.peerID, x.a.uri, x.share) }, casfetch.ReasonRevokedDuringFetch},
		"derived-transfer-1 dropped": {func(x *accessFx) {
			x.setCaps(`["share-lifecycle-1", "archival-1", "original-transfer-1"]`)
		}, casfetch.ReasonRevokedDuringFetch},
		"peer disabled": {func(x *accessFx) {
			if _, err := x.pool.Exec(context.Background(), `UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, x.peerID); err != nil {
				x.t.Fatal(err)
			}
		}, casfetch.ReasonRevokedDuringFetch},
		"peer moved": {func(x *accessFx) {
			if _, err := x.pool.Exec(context.Background(), `UPDATE federation_peers SET instance_url = 'https://moved.example' WHERE id = $1`, x.peerID); err != nil {
				x.t.Fatal(err)
			}
		}, casfetch.ReasonRevokedDuringFetch},
	} {
		t.Run(name, func(t *testing.T) {
			x := newAccessFx(t, true)
			w := x.work(posAccessB)
			res := x.fetch(w)
			tc.change(x)
			out, err := x.ingest.RecordAccessCopy(context.Background(), w, res)
			wantReason(t, err, tc.want)
			if out.Consumed {
				t.Fatal("consumed on a refusal")
			}
			wantNoAccessRecorded(t, x, res, posAccessB)
			x.release(res)
			if x.pins(x.hB, casfetch.IngestPinType) != 0 {
				t.Fatal("the release left the ingest pin")
			}
		})
	}
	t.Run("profile message", func(t *testing.T) {
		x := newAccessFx(t, true)
		w := x.work(posAccessB)
		res := x.fetch(w)
		x.setCaps(`["share-lifecycle-1", "archival-1", "original-transfer-1"]`)
		_, err := x.ingest.RecordAccessCopy(context.Background(), w, res)
		if !errors.Is(err, federation.ErrProfileNotNegotiated) {
			t.Fatalf("%v, want the profile cause", err)
		}
		x.release(res)
	})
}

// Only the exact entry and only the result's own, still held ingest pin;
// a refusal inside the transaction leaves nothing (atomic).
func TestAccessRecord_ExactPinAndFileAndAtomicRollback(t *testing.T) {
	x := newAccessFx(t, true)
	ctx := context.Background()
	w := x.work(posAccessB)
	res := x.fetch(w)
	refused := func(name string, w assetingest.AccessWork, res *casfetch.Result, want string) {
		t.Helper()
		out, err := x.ingest.RecordAccessCopy(ctx, w, res)
		if assetingest.ReasonOf(err) != want || out.Consumed {
			t.Fatalf("%s: %+v %v, want %s", name, out, err, want)
		}
	}
	other := *res
	other.Hash = x.hC
	refused("another entry's hash", w, &other, casfetch.ReasonInvalidRequest)
	foreignPin := *res
	foreignPin.Pin = storage.PinRef{SubjectType: "user", SubjectID: res.IngestID.String()}
	refused("not an ingest pin", w, &foreignPin, casfetch.ReasonInvalidRequest)
	otherIngest := *res
	otherIngest.IngestID = uuid.New()
	refused("an ingest id the pin does not name", w, &otherIngest, casfetch.ReasonInvalidRequest)
	for name, mut := range map[string]func(*assetingest.AccessWork){
		"position": func(w *assetingest.AccessWork) { w.Position = posAccessC },
		"checksums": func(w *assetingest.AccessWork) {
			w.Checksums = append(w.Checksums, federation.ArchivalChecksum{Algorithm: "md5", Value: "00"})
		},
		"version": func(w *assetingest.AccessWork) { w.VersionNumber = 2 },
		"signer":  func(w *assetingest.AccessWork) { w.SignerActorURI = localBase + "/users/eve" },
		// Another tier a record may hold (team, not the stored public).
		"sensitivity": func(w *assetingest.AccessWork) { w.Sensitivity = "team" },
		"record":      func(w *assetingest.AccessWork) { w.RecordID = uuid.New() },
	} {
		ww := w
		mut(&ww)
		refused("work with another "+name, ww, res, assetingest.ReasonAccessWorkChanged)
	}
	// Another size: the result disagrees with the work first; with both
	// changed alike, the persisted row is what refuses.
	ws := w
	ws.Size++
	refused("work with another size", ws, res, casfetch.ReasonInvalidRequest)
	rs := *res
	rs.Size = ws.Size
	refused("work and result with another size", ws, &rs, assetingest.ReasonAccessWorkChanged)
	wantNoAccessRecorded(t, x, res, posAccessB)

	// The pin is gone (released, or another caller's): the record pin
	// added earlier in the same transaction is rolled back with it.
	stale := x.fetch(w)
	x.release(stale)
	refused("a released ingest pin", w, stale, casfetch.ReasonStorage)
	wantNoAccessRecorded(t, x, res, posAccessB)

	// A cancelled caller records nothing and still owns its pin.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if out, err := x.ingest.RecordAccessCopy(cctx, w, res); err == nil || out.Consumed {
		t.Fatalf("cancelled: %+v %v", out, err)
	}
	wantNoAccessRecorded(t, x, res, posAccessB)

	// The exact result still records.
	if out, err := x.ingest.RecordAccessCopy(ctx, w, res); err != nil || !out.Consumed {
		t.Fatalf("the exact result: %+v %v", out, err)
	}
}

// A second success is a duplicate: one caller's pin is consumed, the
// other keeps (and releases) its own, and verified_at stands.
func TestAccessRecord_DuplicateSuccess(t *testing.T) {
	x := newAccessFx(t, true)
	ctx := context.Background()
	w := x.work(posAccessB)
	first, second := x.fetch(w), x.fetch(w)
	if first.IngestID == second.IngestID || x.pins(x.hB, casfetch.IngestPinType) != 2 {
		t.Fatal("the two fetches do not hold their own pins")
	}
	var wg sync.WaitGroup
	outs := make([]assetingest.AccessOutcome, 2)
	errs := make([]error, 2)
	for i, res := range []*casfetch.Result{first, second} {
		wg.Add(1)
		go func() { defer wg.Done(); outs[i], errs[i] = x.ingest.RecordAccessCopy(ctx, w, res) }()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatal(errs)
	}
	if outs[0].Consumed == outs[1].Consumed || outs[0].Duplicate == outs[1].Duplicate {
		t.Fatalf("outcomes %+v %+v, want one consumed and one duplicate", outs[0], outs[1])
	}
	if !outs[0].VerifiedAt.Equal(outs[1].VerifiedAt) {
		t.Fatalf("verified_at %v and %v", outs[0].VerifiedAt, outs[1].VerifiedAt)
	}
	for i, res := range []*casfetch.Result{first, second} {
		if !outs[i].Consumed {
			x.release(res)
		}
	}
	if x.pins(x.hB, casfetch.IngestPinType) != 0 || x.pins(x.hB, assetingest.PinSubjectType) != 1 {
		t.Fatal("pins after a duplicate")
	}
	// A later replay, and a failure, leave the verified copy as it is.
	at := *x.row(x.id, posAccessB).verifiedAt
	third := x.fetch(w)
	if out, err := x.ingest.RecordAccessCopy(ctx, w, third); err != nil || !out.Duplicate || out.Consumed {
		t.Fatalf("replay %+v %v", out, err)
	}
	x.release(third)
	if out, err := x.ingest.RecordAccessFailure(ctx, w, casfetch.ReasonTimeout); err != nil || !out.Duplicate {
		t.Fatalf("failure on a verified copy %+v %v", out, err)
	}
	if r := x.row(x.id, posAccessB); r.state != "verified" || !r.verifiedAt.Equal(at) || r.reason != nil {
		t.Fatalf("verified copy rewritten: %+v", r)
	}
}

// Failures are recorded per file, bounded, and change nothing else.
func TestAccessRecord_IndependentFailureReason(t *testing.T) {
	x := newAccessFx(t, true)
	ctx := context.Background()
	before := x.snapshot(x.id, posAccessC)
	out, err := x.ingest.RecordAccessFailure(ctx, x.work(posAccessC), casfetch.ReasonFixityMismatch)
	if err != nil || out.State != assetingest.FileStateNotTransferred || out.Reason != casfetch.ReasonFixityMismatch {
		t.Fatalf("%+v %v", out, err)
	}
	if after := x.snapshot(x.id, posAccessC); after != before {
		t.Fatalf("a failure changed the record or another file:\n%s\n%s", before, after)
	}
	if r := x.row(x.id, posAccessC); r.reason == nil || *r.reason != casfetch.ReasonFixityMismatch {
		t.Fatalf("row %+v", r)
	}
	// A later failure replaces it; another file records its own.
	if _, err := x.ingest.RecordAccessFailure(ctx, x.work(posAccessC), casfetch.ReasonTimeout); err != nil {
		t.Fatal(err)
	}
	if _, err := x.ingest.RecordAccessFailure(ctx, x.work(posAccessB), casfetch.ReasonOriginRefused); err != nil {
		t.Fatal(err)
	}
	if b, c := x.row(x.id, posAccessB), x.row(x.id, posAccessC); *b.reason != casfetch.ReasonOriginRefused || *c.reason != casfetch.ReasonTimeout {
		t.Fatalf("reasons %v %v", *b.reason, *c.reason)
	}
	// A failed copy can still be received later.
	w := x.work(posAccessB)
	if out, err := x.ingest.RecordAccessCopy(ctx, w, x.fetch(w)); err != nil || !out.Consumed {
		t.Fatalf("success after a failure: %+v %v", out, err)
	}
	if r := x.row(x.id, posAccessB); r.state != "verified" || r.reason != nil {
		t.Fatalf("row %+v", r)
	}
	for _, reason := range []string{"", "withdrawn", "awaiting_grant", "made_up"} {
		if _, err := x.ingest.RecordAccessFailure(ctx, x.work(posAccessC), reason); assetingest.ReasonOf(err) != casfetch.ReasonInvalidRequest {
			t.Fatalf("reason %q: %v", reason, err)
		}
	}
	for _, reason := range assetingest.AccessFailureReasons {
		if _, err := x.ingest.RecordAccessFailure(ctx, x.work(posAccessC), reason); err != nil {
			t.Fatalf("reason %q refused by the database: %v", reason, err)
		}
	}
}

// After supersession the version keeps its verified copy and records
// nothing new; the successor's copies are its own. Withdrawing both then
// keeps the history and releases only their pins.
func TestAccessRecord_SupersededKeepsCopyAndRefusesWrites(t *testing.T) {
	x := newAccessFx(t, true)
	ctx := context.Background()
	w := x.work(posAccessB)
	v1Out, err := x.ingest.RecordAccessCopy(ctx, w, x.fetch(w))
	if err != nil {
		t.Fatal(err)
	}
	files, _ := x.originals(1)
	hD, _, ed := x.addFile("access", 23<<10)
	files = append(files, ed)
	v2, err := x.receive(x.announceV(x.a, 2, files))
	if err != nil || v2.State != assetingest.StateComplete {
		t.Fatalf("v2: %+v %v", v2, err)
	}
	if st, _ := x.state(x.id); st != assetingest.StateSuperseded {
		t.Fatalf("v1 is %s", st)
	}
	// v1 keeps its copy, pinned.
	if r := x.row(x.id, posAccessB); r.state != "verified" || !r.verifiedAt.Equal(v1Out.VerifiedAt) || x.pins(x.hB, assetingest.PinSubjectType) != 1 {
		t.Fatalf("superseded copy %+v", r)
	}
	// Nothing new for v1.
	if _, err := x.ingest.LoadAccessWork(ctx, x.id, posAccessC); assetingest.ReasonOf(err) != assetingest.ReasonNotCurrentVersion {
		t.Fatalf("load superseded: %v", err)
	}
	wc := x.work(posAccessC)
	res := x.fetch(wc)
	_, err = x.ingest.RecordAccessCopy(ctx, wc, res)
	wantReason(t, err, assetingest.ReasonNotCurrentVersion)
	_, err = x.ingest.RecordAccessFailure(ctx, wc, casfetch.ReasonTimeout)
	wantReason(t, err, assetingest.ReasonNotCurrentVersion)
	if r := x.row(x.id, posAccessC); r.state != "not_transferred" || r.reason != nil {
		t.Fatalf("superseded row written: %+v", r)
	}
	x.release(res)
	// v2's own access copy.
	w2, err := x.ingest.LoadAccessWork(ctx, v2.RecordID, 1)
	if err != nil || w2.SHA256 != hD || w2.VersionNumber != 2 {
		t.Fatalf("v2 work %+v %v", w2, err)
	}
	if out, err := x.ingest.RecordAccessCopy(ctx, w2, x.fetch(w2)); err != nil || !out.Consumed {
		t.Fatalf("v2 copy: %+v %v", out, err)
	}

	// Withdrawal: both versions; verified copies keep verified_at.
	if _, err := x.withdraw(x.deleteOf(x.a, x.tombstone(x.a, ""))); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id  uuid.UUID
		pos int
	}{{x.id, posAccessB}, {v2.RecordID, 1}} {
		if r := x.row(c.id, c.pos); r.state != assetingest.FileStateWithdrawn || r.verifiedAt == nil {
			t.Fatalf("withdrawn copy %+v", r)
		}
	}
	if r := x.row(x.id, posAccessC); r.state != "not_transferred" {
		t.Fatalf("an unattempted claim after withdrawal: %+v", r)
	}
	if x.recordPins(x.id)+x.recordPins(v2.RecordID) != 0 {
		t.Fatal("record pins left after withdrawal")
	}
	x.nothingUsable(x.hB, hD)
	if _, err := x.ingest.RecordAccessFailure(ctx, x.work(posAccessC), casfetch.ReasonTimeout); assetingest.ReasonOf(err) != assetingest.ReasonWithdrawn {
		t.Fatalf("failure after withdrawal: %v", err)
	}
}

// Withdrawal of a version holding copies whose bytes are also the
// record's original and another owner's upload: the record's pins go,
// once per hash, the other owner's stays, and only unpinned bytes are
// scheduled for GC.
func TestAccessRecord_WithdrawalCleanupWithSharedHashes(t *testing.T) {
	x := newAccessFx(t, true)
	ctx := context.Background()
	// Another owner holds B's bytes.
	if _, err := x.svc.UploadOriginal(ctx, bytes.NewReader(x.bodyB), "image/png",
		storage.PinRef{SubjectType: "user", SubjectID: "unrelated-upload"}); err != nil {
		t.Fatal(err)
	}
	for _, pos := range []int{posAccessB, posAccessDup} {
		w := x.work(pos)
		res := x.fetch(w)
		out, err := x.ingest.RecordAccessCopy(ctx, w, res)
		if err != nil || !out.Consumed {
			t.Fatalf("position %d: %+v %v", pos, out, err)
		}
	}
	// The original and its access entry share one record pin.
	if n := x.pins(x.a.hash[0], assetingest.PinSubjectType); n != 1 {
		t.Fatalf("record pins of the shared hash: %d", n)
	}
	if x.recordPins(x.id) != 2 {
		t.Fatalf("record pins %d, want 2 (the original and B)", x.recordPins(x.id))
	}
	if _, err := x.withdraw(x.deleteOf(x.a, x.tombstone(x.a, "rights"))); err != nil {
		t.Fatal(err)
	}
	for _, pos := range []int{posOriginal, posAccessB, posAccessDup} {
		if r := x.row(x.id, pos); r.state != assetingest.FileStateWithdrawn || r.verifiedAt == nil {
			t.Fatalf("position %d after withdrawal: %+v", pos, r)
		}
	}
	if x.recordPins(x.id) != 0 {
		t.Fatal("record pins left")
	}
	if x.pins(x.hB, "user") != 1 || x.count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NULL`, x.hB) != 1 {
		t.Fatal("the other owner's pin of B was touched, or B was scheduled for GC")
	}
	x.nothingUsable(x.a.hash[0])
	if n := x.count(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1 AND state = 'withdrawn'`, x.id); n != 3 {
		t.Fatalf("withdrawn rows %d, want 3", n)
	}
}
