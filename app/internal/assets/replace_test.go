// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1596: replacing an asset's original file (POST /assets/{id}/versions),
// on a real database and the fs backend.
//
//   - a replacement makes exactly one gapless version (origin
//     replacement, predecessor the current version), moves the asset to
//     the new bytes, keeps the old bytes pinned to their version, moves
//     the asset pin, uses up the receipt, records the receipt's base name
//     as the version's provenance, queues the new bytes' processing in the
//     same transaction, and leaves the old bytes' fixity history alone;
//   - a bad, foreign, expired or mismatched receipt and an unknown object
//     are 400 and use nothing up; a replacement that fails before its
//     commit rolls everything back and leaves the receipt reusable;
//   - an unauthenticated caller is 401, a reader 403, a hidden, missing or
//     deleted asset 404; a team editor may replace;
//   - a stale expected version, the same bytes, bytes the owner already
//     has elsewhere and a received asset are 409;
//   - of concurrent replacements on one version exactly one wins;
//   - every version's original stays pinned across replacements and soft
//     delete, and a purge releases them all;
//   - a type with nothing to process is sealed by the reconcile its ready
//     state requests; a processed type is sealed once processing ends,
//     successfully or not;
//   - a metadata edit makes no version;
//   - the extension is required, normalized, and must stay in the
//     current file's class;
//   - a job for the old bytes, a real one run after the replacement or a
//     write racing it on the row lock, never changes the new version:
//     status, error, thumbhash, dimensions, metadata, page count, the
//     visual embedding; the new version stays a draft until its own job;
//   - the per-bytes follow-up work (metadata extraction, transcription,
//     visual embedding) is queued for the new bytes in the replacement's
//     transaction, uncoalesced with work still running for the old ones,
//     and the old bytes' workers write nothing.

package assets_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/pgvector/pgvector-go"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	aitranscribe "github.com/mscrnt/artist-alley/app/internal/ai/transcribe"
	assetmetadata "github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/preview"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualembed"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualprovider"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualstore"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/subtitles"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

func ptr[T any](v T) *T { return &v }

// receipt issues an upload receipt to user for hash, as POST
// /storage/objects does.
func (rr *reconcileRig) receipt(user int64, hash string, basename *string, ttl time.Duration) string {
	rr.t.Helper()
	source := "unknown"
	if basename != nil {
		source = "upload_request"
	}
	var id uuid.UUID
	// created_at is set an hour before the expiry when ttl is negative
	// (an expired receipt): the table requires expires_at > created_at.
	if err := rr.pool.QueryRow(rr.ctx, `INSERT INTO upload_receipts (user_ref, file_hash, original_basename, original_basename_source, created_at, expires_at)
		VALUES ($1, $2, $3, $4, LEAST(now(), now() + $5::interval - interval '1 hour'), now() + $5::interval) RETURNING id`,
		user, hash, basename, source, ttl.String()).Scan(&id); err != nil {
		rr.t.Fatal(err)
	}
	return id.String()
}

func (rr *reconcileRig) receiptExists(id string) bool {
	rr.t.Helper()
	var n int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM upload_receipts WHERE id = $1`, id).Scan(&n); err != nil {
		rr.t.Fatal(err)
	}
	return n == 1
}

func (rr *reconcileRig) replace(ctx context.Context, asset uuid.UUID, hash, receipt string, expected int64, ext string) openapi.CreateAssetVersionResponseObject {
	rr.t.Helper()
	resp, err := rr.h.CreateAssetVersion(ctx, openapi.CreateAssetVersionRequestObject{
		Id:   openapi_types.UUID(asset),
		Body: &openapi.AssetVersionCreate{FileHash: hash, UploadReceipt: receipt, ExpectedVersion: expected, FileExtension: ext},
	})
	if err != nil {
		rr.t.Fatalf("CreateAssetVersion: %v", err)
	}
	return resp
}

// freshUpload stores new bytes as owner and returns their hash.
func (rr *reconcileRig) freshUpload(owner int64, label string) string {
	rr.t.Helper()
	h, _ := rr.upload(owner, label)
	rr.track(h)
	return h
}

func (rr *reconcileRig) assetRow(a uuid.UUID) (hash string, version uuid.UUID, status string) {
	rr.t.Helper()
	if err := rr.pool.QueryRow(rr.ctx, `SELECT file_hash, current_version_id, processing_status FROM assets WHERE id = $1`, a).Scan(&hash, &version, &status); err != nil {
		rr.t.Fatal(err)
	}
	return
}

func (rr *reconcileRig) pinCount(hash, subjectType, subjectID string) int {
	rr.t.Helper()
	var n int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = $2 AND pin_subject_id = $3`,
		hash, subjectType, subjectID).Scan(&n); err != nil {
		rr.t.Fatal(err)
	}
	return n
}

func (rr *reconcileRig) previewJobs(hash string) int {
	rr.t.Helper()
	var n int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM jobs WHERE type LIKE 'preview.%' AND status = 'pending' AND payload->>'file_hash' = $1`, hash).Scan(&n); err != nil {
		rr.t.Fatal(err)
	}
	rr.t.Cleanup(func() {
		_, _ = rr.pool.Exec(context.Background(), `DELETE FROM jobs WHERE type LIKE 'preview.%' AND payload->>'file_hash' = $1`, hash)
	})
	return n
}

func (rr *reconcileRig) currentVersionVia(ctx context.Context, a uuid.UUID) *int64 {
	rr.t.Helper()
	resp, err := rr.h.GetAsset(ctx, openapi.GetAssetRequestObject{Id: openapi_types.UUID(a)})
	if err != nil {
		rr.t.Fatal(err)
	}
	ok, isOK := resp.(openapi.GetAsset200JSONResponse)
	if !isOK {
		rr.t.Fatalf("GetAsset: %T", resp)
	}
	return ok.CurrentVersion
}

func is201(t *testing.T, what string, resp openapi.CreateAssetVersionResponseObject) openapi.AssetVersion {
	t.Helper()
	ok, isOK := resp.(openapi.CreateAssetVersion201JSONResponse)
	if !isOK {
		t.Fatalf("%s: %T %+v, want 201", what, resp, resp)
	}
	return openapi.AssetVersion(ok)
}

func TestReplace_MakesOneGaplessVersionAndKeepsTheOldBytes(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-basic")
	a, h1 := rr.readyImage(owner)
	v1 := rr.versions(a)[0]
	ctx := rr.identity(owner)
	if n := rr.currentVersionVia(ctx, a); n == nil || *n != 1 {
		t.Fatalf("GET current_version %v, want 1", n)
	}
	var fixBefore string
	if err := rr.pool.QueryRow(rr.ctx, `SELECT row_to_json(v)::text FROM storage_variants v WHERE object_hash = $1 AND variant_key = 'original'`, h1).Scan(&fixBefore); err != nil {
		t.Fatal(err)
	}
	var eventsBefore int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM asset_preservation_events WHERE asset_id = $1`, a).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}

	h2 := rr.freshUpload(owner, "replacement")
	rid := rr.receipt(owner, h2, ptr("new-cover.png"), time.Hour)
	v := is201(t, "replace", rr.replace(ctx, a, h2, rid, 1, "png"))
	if v.Number != 2 || v.Origin != openapi.Replacement || v.PredecessorId == nil || uuid.UUID(*v.PredecessorId) != v1.ID || v.OriginalHash != h2 {
		t.Fatalf("version %+v, want number 2, origin replacement, predecessor v1, the new bytes", v)
	}
	hash, cur, status := rr.assetRow(a)
	if hash != h2 || cur != uuid.UUID(v.Id) || status != "pending" {
		t.Fatalf("asset = %s / %s / %s, want the new bytes, the new version, pending", hash[:8], cur, status)
	}
	var basename, source string
	var sealed bool
	if err := rr.pool.QueryRow(rr.ctx, `SELECT original_basename, original_basename_source, sealed_at IS NOT NULL FROM asset_versions WHERE id = $1`, v.Id).Scan(&basename, &source, &sealed); err != nil {
		t.Fatal(err)
	}
	if basename != "new-cover.png" || source != "upload_request" || sealed {
		t.Fatalf("v2 provenance %q/%s sealed=%v, want the receipt's base name, a draft until processing ends", basename, source, sealed)
	}
	if rr.receiptExists(rid) {
		t.Fatal("the receipt was not used up")
	}
	if rr.previewJobs(h2) != 1 {
		t.Fatal("the new bytes' processing was not queued")
	}
	// Pins: the old bytes stay held by their version; the asset pin moved;
	// the uploader's pin is gone.
	if !rr.versionPinned(h1, v1.ID) || !rr.versionPinned(h2, uuid.UUID(v.Id)) {
		t.Fatal("a version's original lost its pin")
	}
	if rr.pinCount(h2, assets.PinSubjectTypeAsset, a.String()) != 1 || rr.pinCount(h1, assets.PinSubjectTypeAsset, a.String()) != 0 {
		t.Fatal("the asset pin did not move to the new bytes")
	}
	if rr.pinCount(h2, storage.PinSubjectTypeUser, itoaRef(owner)) != 0 {
		t.Fatal("the uploader's pin survived the replacement")
	}
	if rr.gcEligibleAt(h1) != nil || rr.gcEligibleAt(h2) != nil {
		t.Fatal("bytes became GC-eligible")
	}
	// Both originals are still readable, and the old bytes' history is
	// exactly as it was.
	if len(rr.storedOriginal(h1)) == 0 || len(rr.storedOriginal(h2)) == 0 {
		t.Fatal("an original is unreadable")
	}
	var fixAfter string
	if err := rr.pool.QueryRow(rr.ctx, `SELECT row_to_json(v)::text FROM storage_variants v WHERE object_hash = $1 AND variant_key = 'original'`, h1).Scan(&fixAfter); err != nil {
		t.Fatal(err)
	}
	var eventsAfter int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM asset_preservation_events WHERE asset_id = $1 AND object_hash = $2`, a, h1).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if fixAfter != fixBefore || eventsAfter > eventsBefore {
		t.Fatal("the replacement touched the old bytes' fixity history")
	}
	if n := rr.currentVersionVia(ctx, a); n == nil || *n != 2 {
		t.Fatalf("GET current_version %v, want 2", n)
	}
}

func itoaRef(n int64) string {
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestReplace_ReceiptScopeAndRollback(t *testing.T) {
	rr := newReconcileRig(t)
	owner, other := rr.user("rp-receipt"), rr.user("rp-receipt-other")
	a, h1 := rr.readyImage(owner)
	ctx := rr.identity(owner)
	h2 := rr.freshUpload(owner, "receipt-new")
	h3 := rr.freshUpload(owner, "receipt-elsewhere")
	unchanged := func(why string) {
		t.Helper()
		if hash, _, _ := rr.assetRow(a); hash != h1 || len(rr.versions(a)) != 1 {
			t.Fatalf("%s changed the asset", why)
		}
	}
	bad := func(why string, resp openapi.CreateAssetVersionResponseObject) {
		t.Helper()
		if _, ok := resp.(openapi.CreateAssetVersion400JSONResponse); !ok {
			t.Fatalf("%s: %T, want 400", why, resp)
		}
		unchanged(why)
	}
	foreign := rr.receipt(other, h2, nil, time.Hour)
	bad("another user's receipt", rr.replace(ctx, a, h2, foreign, 1, "png"))
	wrong := rr.receipt(owner, h3, nil, time.Hour)
	bad("a receipt for other bytes", rr.replace(ctx, a, h2, wrong, 1, "png"))
	expired := rr.receipt(owner, h2, nil, -time.Minute)
	bad("an expired receipt", rr.replace(ctx, a, h2, expired, 1, "png"))
	bad("a malformed receipt", rr.replace(ctx, a, h2, "not-a-uuid", 1, "png"))
	bad("an object that was never uploaded", rr.replace(ctx, a, strings.Repeat("d", 64), wrong, 1, "png"))
	if !rr.receiptExists(foreign) || !rr.receiptExists(wrong) {
		t.Fatal("a refused replacement used up a receipt")
	}

	// A failure after every write, before the commit: all of it rolls back.
	rid := rr.receipt(owner, h2, nil, time.Hour)
	injected := errors.New("injected before commit")
	rr.h.SetReplaceBeforeCommitHook(func(context.Context, pgx.Tx) error { return injected })
	_, err := rr.h.CreateAssetVersion(ctx, openapi.CreateAssetVersionRequestObject{Id: openapi_types.UUID(a),
		Body: &openapi.AssetVersionCreate{FileHash: h2, UploadReceipt: rid, ExpectedVersion: 1, FileExtension: "png"}})
	rr.h.SetReplaceBeforeCommitHook(nil)
	if !errors.Is(err, injected) {
		t.Fatalf("CreateAssetVersion: %v, want the injected failure", err)
	}
	unchanged("a rolled-back replacement")
	if !rr.receiptExists(rid) || rr.previewJobs(h2) != 0 || rr.pinCount(h2, assets.PinSubjectTypeAsset, a.String()) != 0 {
		t.Fatal("a rolled-back replacement used the receipt, queued work or pinned the bytes")
	}
	var versionPins int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'asset_version'`, h2).Scan(&versionPins); err != nil || versionPins != 0 {
		t.Fatalf("%d version pins on the rolled-back bytes (%v)", versionPins, err)
	}
	// The same receipt then succeeds.
	is201(t, "retry with the same receipt", rr.replace(ctx, a, h2, rid, 1, "png"))
}

func TestReplace_AuthorityAndHiding(t *testing.T) {
	rr := newReconcileRig(t)
	owner, reader, editor := rr.user("rp-owner"), rr.user("rp-reader"), rr.user("rp-editor")
	team := rr.team("rp-team", nil)
	rr.grant(editor, assets.CapAssetsAdmin, &team)
	a, h1 := rr.readyImage(owner)
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE assets SET status = 'active', team_id = $2 WHERE id = $1`, a, team); err != nil {
		t.Fatal(err)
	}
	h2 := rr.freshUpload(owner, "auth-new")

	if _, ok := rr.replace(rr.anonCtx(), a, h2, rr.receipt(owner, h2, nil, time.Hour), 1, "png").(openapi.CreateAssetVersion401JSONResponse); !ok {
		t.Fatal("an anonymous caller was not refused with 401")
	}
	if _, ok := rr.replace(rr.identity(reader), a, h2, rr.receipt(reader, h2, nil, time.Hour), 1, "png").(openapi.CreateAssetVersion403JSONResponse); !ok {
		t.Fatal("a reader was not refused with 403")
	}
	if hash, _, _ := rr.assetRow(a); hash != h1 {
		t.Fatal("a refused reader changed the asset")
	}
	// A team editor (team-scoped assets.admin) may replace.
	is201(t, "team editor", rr.replace(rr.identity(editor), a, h2, rr.receipt(editor, h2, nil, time.Hour), 1, "png"))

	// Hidden, missing and deleted assets are all 404.
	hidden, _ := rr.readyImage(owner)
	// Restricted content: its row is visible to signed-in users (ADR 0063),
	// its content is not (ADR 0064), so a stranger must get 404.
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE assets SET sensitivity = 'restricted' WHERE id = $1`, hidden); err != nil {
		t.Fatal(err)
	}
	h3 := rr.freshUpload(reader, "auth-hidden")
	if _, ok := rr.replace(rr.identity(reader), hidden, h3, rr.receipt(reader, h3, nil, time.Hour), 1, "png").(openapi.CreateAssetVersion404JSONResponse); !ok {
		t.Fatal("an asset the caller cannot read was not 404")
	}
	if _, ok := rr.replace(rr.identity(owner), uuid.New(), h3, rr.receipt(owner, h3, nil, time.Hour), 1, "png").(openapi.CreateAssetVersion404JSONResponse); !ok {
		t.Fatal("a missing asset was not 404")
	}
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE assets SET deleted_at = now() WHERE id = $1`, hidden); err != nil {
		t.Fatal(err)
	}
	h4 := rr.freshUpload(owner, "auth-deleted")
	if _, ok := rr.replace(rr.identity(owner), hidden, h4, rr.receipt(owner, h4, nil, time.Hour), 1, "png").(openapi.CreateAssetVersion404JSONResponse); !ok {
		t.Fatal("a deleted asset was not 404")
	}
}

func TestReplace_ConflictsAreRefusedAndUseNothing(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-conflict")
	a, h1 := rr.readyImage(owner)
	ctx := rr.identity(owner)
	conflict := func(why string, resp openapi.CreateAssetVersionResponseObject, rid string) {
		t.Helper()
		if _, ok := resp.(openapi.CreateAssetVersion409JSONResponse); !ok {
			t.Fatalf("%s: %T, want 409", why, resp)
		}
		if !rr.receiptExists(rid) {
			t.Fatalf("%s used up the receipt", why)
		}
		if hash, _, _ := rr.assetRow(a); hash != h1 || len(rr.versions(a)) != 1 {
			t.Fatalf("%s changed the asset", why)
		}
	}
	h2 := rr.freshUpload(owner, "conflict-new")
	rid := rr.receipt(owner, h2, nil, time.Hour)
	conflict("a stale expected version", rr.replace(ctx, a, h2, rid, 7, "png"), rid)
	same := rr.receipt(owner, h1, nil, time.Hour)
	conflict("the same bytes", rr.replace(ctx, a, h1, same, 1, "png"), same)
	_, hTaken := rr.readyImage(owner) // the owner's other asset holds these bytes
	taken := rr.receipt(owner, hTaken, nil, time.Hour)
	conflict("bytes the owner already has elsewhere", rr.replace(ctx, a, hTaken, taken, 1, "png"), taken)

	// A received asset is versioned by its origin.
	hr := rr.freshUpload(owner, "conflict-remote")
	remote := uuid.New()
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, origin_server_id, status)
		VALUES ($1, 'received', (SELECT MIN(ref) FROM asset_types), $2, $3, $4, 'active')`, remote, owner, hr, uuid.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = rr.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, remote) })
	h3 := rr.freshUpload(owner, "conflict-remote-new")
	rrid := rr.receipt(owner, h3, nil, time.Hour)
	if _, ok := rr.replace(ctx, remote, h3, rrid, 1, "png").(openapi.CreateAssetVersion409JSONResponse); !ok || !rr.receiptExists(rrid) {
		t.Fatal("a received asset was not refused with 409, receipt intact")
	}
}

func TestReplace_ConcurrentReplacementsHaveOneWinner(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-race")
	a, _ := rr.readyImage(owner)
	ctx := rr.identity(owner)
	const n = 5
	type attempt struct{ hash, receipt string }
	tries := make([]attempt, n)
	for i := range tries {
		h := rr.freshUpload(owner, "race-"+itoaRef(int64(i+1)))
		tries[i] = attempt{h, rr.receipt(owner, h, nil, time.Hour)}
	}
	var wg sync.WaitGroup
	results := make([]openapi.CreateAssetVersionResponseObject, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range tries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = rr.h.CreateAssetVersion(ctx, openapi.CreateAssetVersionRequestObject{Id: openapi_types.UUID(a),
				Body: &openapi.AssetVersionCreate{FileHash: tries[i].hash, UploadReceipt: tries[i].receipt, ExpectedVersion: 1, FileExtension: "png"}})
		}(i)
	}
	close(start)
	wg.Wait()
	won, lost := 0, 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		switch r.(type) {
		case openapi.CreateAssetVersion201JSONResponse:
			won++
		case openapi.CreateAssetVersion409JSONResponse:
			lost++
			if !rr.receiptExists(tries[i].receipt) {
				t.Errorf("losing attempt %d used up its receipt", i)
			}
		default:
			t.Fatalf("attempt %d: %T", i, r)
		}
	}
	vs := rr.versions(a)
	if won != 1 || lost != n-1 || len(vs) != 2 || vs[1].Number != 2 || vs[1].Predecessor == nil || *vs[1].Predecessor != vs[0].ID {
		t.Fatalf("%d won, %d lost, %d versions; want exactly one winner and a gapless chain", won, lost, len(vs))
	}
}

func TestReplace_EveryOriginalIsRetainedUntilPurge(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-retain")
	a, h1 := rr.readyImage(owner)
	ctx := rr.identity(owner)
	hashes := []string{h1}
	for i := 1; i <= 3; i++ {
		h := rr.freshUpload(owner, "retain-"+itoaRef(int64(i)))
		is201(t, "replacement", rr.replace(ctx, a, h, rr.receipt(owner, h, nil, time.Hour), int64(i), "png"))
		hashes = append(hashes, h)
	}
	vs := rr.versions(a)
	if len(vs) != 4 {
		t.Fatalf("%d versions, want 4", len(vs))
	}
	for i, v := range vs {
		if v.Number != int64(i+1) || v.OriginalHash != hashes[i] || !rr.versionPinned(hashes[i], v.ID) || rr.gcEligibleAt(hashes[i]) != nil {
			t.Fatalf("version %d: %+v, want number %d on its bytes, pinned, not GC-eligible", i+1, v, i+1)
		}
	}
	// Soft delete keeps them all.
	if _, err := rr.h.DeleteAsset(ctx, openapi.DeleteAssetRequestObject{Id: openapi_types.UUID(a)}); err != nil {
		t.Fatal(err)
	}
	for i, h := range hashes {
		if rr.gcEligibleAt(h) != nil || !rr.versionPinned(h, vs[i].ID) {
			t.Fatalf("soft delete released version %d's bytes", i+1)
		}
	}
	// A purge releases them all.
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE assets SET deleted_at = now() - interval '40 days' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := softdelete.NewService(rr.pool, nil).HardDeletePastAssets(rr.ctx, 30, 1000); err != nil {
		t.Fatal(err)
	}
	for i, h := range hashes {
		if rr.gcEligibleAt(h) == nil {
			t.Fatalf("the purge left version %d's bytes unscheduled", i+1)
		}
	}
}

func TestReplace_NothingToProcessIsSealedByItsReconcile(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-opaque")
	h1 := rr.freshUpload(owner, "opaque-v1")
	a := uuid.New()
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_extension, status)
		VALUES ($1, 'opaque file', (SELECT MIN(ref) FROM asset_types), $2, $3, 'bin', 'active')`, a, owner, h1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = rr.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, a) })
	h2 := rr.freshUpload(owner, "opaque-v2")
	v := is201(t, "replace", rr.replace(rr.identity(owner), a, h2, rr.receipt(owner, h2, nil, time.Hour), 1, "bin"))
	if _, _, status := rr.assetRow(a); status != "ready" || rr.previewJobs(h2) != 0 {
		t.Fatalf("status %s: a type with nothing to process must be ready with no preview job", status)
	}
	if rr.reconcileJobs(h2) != 1 {
		t.Fatal("the ready replacement requested no reconcile")
	}
	if res := rr.reconcile(h2); res.Sealed != 1 {
		t.Fatalf("result %+v, want the new version sealed", res)
	}
	var sealed bool
	if err := rr.pool.QueryRow(rr.ctx, `SELECT sealed_at IS NOT NULL FROM asset_versions WHERE id = $1`, v.Id).Scan(&sealed); err != nil || !sealed {
		t.Fatalf("v2 sealed=%v (%v)", sealed, err)
	}
}

func TestReplace_ProcessingEndSealsTheNewVersion(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-process")
	ctx := rr.identity(owner)
	for _, ok := range []bool{true, false} {
		a, _ := rr.readyImage(owner)
		h2 := rr.freshUpload(owner, "process")
		v := is201(t, "replace", rr.replace(ctx, a, h2, rr.receipt(owner, h2, nil, time.Hour), 1, "png"))
		if rr.previewJobs(h2) != 1 || rr.reconcileJobs(h2) != 0 {
			t.Fatal("a processed type must queue its preview job and request no reconcile yet")
		}
		b := rr.ladder(h2, "preview", "hires") // what the preview job writes
		pg := pgtype.UUID{Bytes: a, Valid: true}
		if ok {
			if err := assets.New(rr.pool).MarkAssetReady(rr.ctx, assets.MarkAssetReadyParams{ID: pg, FileHash: h2}); err != nil {
				t.Fatal(err)
			}
		} else if err := assets.New(rr.pool).MarkAssetFailed(rr.ctx, assets.MarkAssetFailedParams{ID: pg, FileHash: h2, ProcessingError: "boom"}); err != nil {
			t.Fatal(err)
		}
		if rr.reconcileJobs(h2) < 1 {
			t.Fatal("the end of processing requested no reconcile")
		}
		if res := rr.reconcile(h2); res.Sealed != 1 {
			t.Fatalf("ok=%v: result %+v, want v2 sealed", ok, res)
		}
		vs := rr.versions(a)
		if len(vs) != 2 || vs[1].ID != uuid.UUID(v.Id) {
			t.Fatalf("ok=%v: %d versions", ok, len(vs))
		}
		rr.requireSealedWith("v2", vs[1], map[string]string{"preview": digest(b["preview"]), "access": digest(b["hires"])})
	}
}

func TestReplace_MetadataEditMakesNoVersion(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-meta")
	a, _ := rr.readyImage(owner)
	ctx := rr.identity(owner)
	h2 := rr.freshUpload(owner, "meta")
	is201(t, "replace", rr.replace(ctx, a, h2, rr.receipt(owner, h2, nil, time.Hour), 1, "png"))
	title := "renamed after the replacement"
	if _, err := rr.h.UpdateAsset(ctx, openapi.UpdateAssetRequestObject{Id: openapi_types.UUID(a), Body: &openapi.AssetUpdate{Title: &title}}); err != nil {
		t.Fatal(err)
	}
	if vs := rr.versions(a); len(vs) != 2 {
		t.Fatalf("%d versions after a metadata edit, want 2", len(vs))
	}
	if n := rr.currentVersionVia(ctx, a); n == nil || *n != 2 {
		t.Fatalf("current_version %v after a metadata edit, want 2", n)
	}
}

// ---------------------------------------------------------------------------
// File class (#1596 rev533)
// ---------------------------------------------------------------------------

func TestReplace_ExtensionIsRequiredNormalizedAndKeepsItsClass(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-class")
	a, h1 := rr.readyImage(owner) // png
	ctx := rr.identity(owner)
	h2 := rr.freshUpload(owner, "class-new")
	rid := rr.receipt(owner, h2, nil, time.Hour)
	untouched := func(why string, resp openapi.CreateAssetVersionResponseObject, want string) {
		t.Helper()
		switch want {
		case "400":
			if _, ok := resp.(openapi.CreateAssetVersion400JSONResponse); !ok {
				t.Fatalf("%s: %T, want 400", why, resp)
			}
		case "409":
			if _, ok := resp.(openapi.CreateAssetVersion409JSONResponse); !ok {
				t.Fatalf("%s: %T, want 409", why, resp)
			}
		}
		if !rr.receiptExists(rid) {
			t.Fatalf("%s used up the receipt", why)
		}
		if hash, _, _ := rr.assetRow(a); hash != h1 || len(rr.versions(a)) != 1 {
			t.Fatalf("%s changed the asset", why)
		}
	}
	untouched("an omitted extension", rr.replace(ctx, a, h2, rid, 1, ""), "400")
	untouched("a dot alone", rr.replace(ctx, a, h2, rid, 1, "."), "400")
	untouched("a path in the extension", rr.replace(ctx, a, h2, rid, 1, "../png"), "400")
	untouched("an extension of 33 characters", rr.replace(ctx, a, h2, rid, 1, strings.Repeat("a", 33)), "400")
	untouched("an image replaced by a 3D model", rr.replace(ctx, a, h2, rid, 1, "glb"), "409")
	untouched("an image replaced by a video", rr.replace(ctx, a, h2, rid, 1, "mp4"), "409")
	untouched("an image replaced by a document", rr.replace(ctx, a, h2, rid, 1, "pdf"), "409")

	// Same class, spelled loosely: accepted and stored normalized.
	v := is201(t, "png replaced by .JPG", rr.replace(ctx, a, h2, rid, 1, ".JPG"))
	var ext string
	if err := rr.pool.QueryRow(rr.ctx, `SELECT file_extension FROM assets WHERE id = $1`, a).Scan(&ext); err != nil {
		t.Fatal(err)
	}
	if ext != "jpg" || v.OriginalExtension == nil || *v.OriginalExtension != "jpg" {
		t.Fatalf("asset extension %q, version extension %v; want both normalized to jpg", ext, v.OriginalExtension)
	}
}

// ---------------------------------------------------------------------------
// Jobs for the old bytes (#1596 rev533)
// ---------------------------------------------------------------------------

// uploadPNG stores a real, decodable w x h PNG as owner.
func (rr *reconcileRig) uploadPNG(owner int64, w, h int) string {
	rr.t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	seed := make([]byte, 1)
	if _, err := crand.Read(seed); err != nil {
		rr.t.Fatal(err)
	}
	for i := range img.Pix {
		img.Pix[i] = byte(i*7) + seed[0]
		if i%4 == 3 {
			img.Pix[i] = 0xff
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		rr.t.Fatal(err)
	}
	res, err := rr.h.Storage.UploadOriginal(rr.ctx, bytes.NewReader(buf.Bytes()), "image/png",
		storage.PinRef{SubjectType: storage.PinSubjectTypeUser, SubjectID: itoaRef(owner)})
	if err != nil {
		rr.t.Fatalf("upload png: %v", err)
	}
	rr.track(res.Hash)
	rr.t.Cleanup(func() {
		c := context.Background()
		_, _ = rr.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1 AND pin_subject_type <> 'asset_version'`, res.Hash)
	})
	return res.Hash
}

type assetState struct {
	status   string
	attempts int32
	thumb    []byte
	errText  *string
	meta     string
	pages    *int32
}

func (rr *reconcileRig) state(a uuid.UUID) assetState {
	rr.t.Helper()
	var s assetState
	if err := rr.pool.QueryRow(rr.ctx, `SELECT processing_status, processing_attempts, thumbhash, processing_error,
		COALESCE(metadata, '{}'::jsonb)::text, page_count FROM assets WHERE id = $1`, a).
		Scan(&s.status, &s.attempts, &s.thumb, &s.errText, &s.meta, &s.pages); err != nil {
		rr.t.Fatal(err)
	}
	return s
}

func sameState(x, y assetState) bool {
	return x.status == y.status && x.attempts == y.attempts && bytes.Equal(x.thumb, y.thumb) &&
		(x.errText == nil) == (y.errText == nil) && x.meta == y.meta && (x.pages == nil) == (y.pages == nil)
}

// dims is the asset's recorded pixel dimensions, 0x0 when none.
func (rr *reconcileRig) dims(a uuid.UUID) (w, h int) {
	rr.t.Helper()
	if err := rr.pool.QueryRow(rr.ctx, `SELECT
		COALESCE((SELECT value_num::INT FROM asset_field_value v JOIN field_definition d ON d.id = v.field_id WHERE v.asset_id = $1 AND d.code = $2), 0),
		COALESCE((SELECT value_num::INT FROM asset_field_value v JOIN field_definition d ON d.id = v.field_id WHERE v.asset_id = $1 AND d.code = $3), 0)`,
		a, pixeldims.Width, pixeldims.Height).Scan(&w, &h); err != nil {
		rr.t.Fatal(err)
	}
	return
}

func (rr *reconcileRig) sealed(version uuid.UUID) bool {
	rr.t.Helper()
	var sealed bool
	if err := rr.pool.QueryRow(rr.ctx, `SELECT sealed_at IS NOT NULL FROM asset_versions WHERE id = $1`, version).Scan(&sealed); err != nil {
		rr.t.Fatal(err)
	}
	return sealed
}

func (rr *reconcileRig) clearJobsFor(a uuid.UUID) {
	rr.t.Cleanup(func() {
		_, _ = rr.pool.Exec(context.Background(), `DELETE FROM jobs WHERE payload->>'asset_id' = $1`, a.String())
		_, _ = rr.pool.Exec(context.Background(), `DELETE FROM asset_field_value WHERE asset_id = $1`, a)
		_, _ = rr.pool.Exec(context.Background(), `DELETE FROM asset_visual_embedding WHERE asset_id = $1`, a)
	})
}

// runRaster runs the real preview.raster handler on one payload.
func (rr *reconcileRig) runRaster(a uuid.UUID, hash string) error {
	rr.t.Helper()
	h := preview.NewRasterHandler(rr.pool, rr.h.Storage, sysconfig.NewStore(rr.pool), slog.New(slog.DiscardHandler))
	b, err := json.Marshal(dispatch.NewPayload(a, hash, ptr("png"), false))
	if err != nil {
		rr.t.Fatal(err)
	}
	_, err = h.Handle(rr.ctx, &jobs.Claim{Type: jobs.TypePreviewRaster, Payload: b})
	return err
}

func vector(seed float32) *pgvector.Vector {
	v := make([]float32, 768)
	for i := range v {
		v[i] = seed + float32(i%7)/100
	}
	pv := pgvector.NewVector(v)
	return &pv
}

func (rr *reconcileRig) hasEmbedding(a uuid.UUID) bool {
	rr.t.Helper()
	var n int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM asset_visual_embedding WHERE asset_id = $1`, a).Scan(&n); err != nil {
		rr.t.Fatal(err)
	}
	return n == 1
}

// A real preview job for the old bytes, run after the replacement
// committed, renders its own renditions and changes nothing on the asset;
// one that fails changes nothing either. The new version stays a draft
// until its own job ends.
func TestReplace_AnOldPreviewJobAfterTheReplacementChangesNothing(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-oldjob")
	ctx := rr.identity(owner)

	h1 := rr.uploadPNG(owner, 64, 48)
	a := rr.createWithFile(owner, h1)
	rr.clearJobsFor(a)
	rr.setProcessing(a, "processing") // its job has been claimed
	h2 := rr.uploadPNG(owner, 40, 30)
	v := is201(t, "replace", rr.replace(ctx, a, h2, rr.receipt(owner, h2, nil, time.Hour), 1, "png"))
	before := rr.state(a)
	if before.status != "pending" || len(before.thumb) == 0 {
		t.Fatalf("after the replacement: %+v, want pending with the new bytes' thumbhash", before)
	}

	// The old job completes.
	if err := rr.runRaster(a, h1); err != nil {
		t.Fatalf("old job: %v", err)
	}
	var renditions int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM storage_variants WHERE object_hash = $1 AND variant_key <> 'original'`, h1).Scan(&renditions); err != nil {
		t.Fatal(err)
	}
	if renditions == 0 {
		t.Fatal("the old job wrote no renditions of the old bytes (they are still a version)")
	}
	if after := rr.state(a); !sameState(before, after) {
		t.Fatalf("the old job changed the asset: %+v -> %+v", before, after)
	}
	if w, h := rr.dims(a); w != 0 || h != 0 {
		t.Fatalf("the old job recorded %dx%d on the new version", w, h)
	}
	// Every other stamp a preview or extraction job makes, for the old bytes.
	q := assets.New(rr.pool)
	pg := pgtype.UUID{Bytes: a, Valid: true}
	if err := q.MergeAssetMetadata(rr.ctx, assets.MergeAssetMetadataParams{ID: pg, FileHash: h1, Metadata: []byte(`{"pdf":{"pages":3}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := q.SetAssetThumbhash(rr.ctx, assets.SetAssetThumbhashParams{ID: pg, FileHash: h1, Thumbhash: []byte{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	if err := assetmetadata.NewPoolAssetAttributeWriter(rr.pool).SetAssetPageCount(rr.ctx, a, h1, 9); err != nil {
		t.Fatal(err)
	}
	if err := pixeldims.Record(rr.ctx, rr.pool, a, h1, 64, 48); err != nil {
		t.Fatal(err)
	}
	if after := rr.state(a); !sameState(before, after) {
		t.Fatalf("an old-bytes stamp changed the asset: %+v -> %+v", before, after)
	}
	if w, h := rr.dims(a); w != 0 || h != 0 {
		t.Fatalf("old dimensions %dx%d landed on the new version", w, h)
	}
	if rr.sealed(uuid.UUID(v.Id)) || rr.reconcileJobs(h2) != 0 {
		t.Fatal("the old job sealed, or requested the seal of, the new version")
	}

	// An old job that fails, on another asset whose old bytes do not decode.
	b, g1 := rr.readyImage(owner)
	rr.clearJobsFor(b)
	rr.setProcessing(b, "processing")
	g2 := rr.freshUpload(owner, "oldjob-fail-v2")
	vb := is201(t, "replace", rr.replace(ctx, b, g2, rr.receipt(owner, g2, nil, time.Hour), 1, "png"))
	bBefore := rr.state(b)
	if err := rr.runRaster(b, g1); err == nil {
		t.Fatal("the old job decoded bytes that are not an image")
	}
	if after := rr.state(b); !sameState(bBefore, after) || after.status != "pending" || after.errText != nil {
		t.Fatalf("the old job's failure reached the new version: %+v -> %+v", bBefore, after)
	}
	if rr.sealed(uuid.UUID(vb.Id)) || rr.reconcileJobs(g2) != 0 {
		t.Fatal("the old job's failure sealed, or requested the seal of, the new version")
	}

	// The new version's own job ends it: ready, its own dimensions, sealed.
	if err := rr.runRaster(a, h2); err != nil {
		t.Fatalf("new job: %v", err)
	}
	if s := rr.state(a); s.status != "ready" {
		t.Fatalf("after its own job: %+v, want ready", s)
	}
	if w, h := rr.dims(a); w != 40 || h != 30 {
		t.Fatalf("dimensions %dx%d, want the new bytes' 40x30", w, h)
	}
	if rr.reconcileJobs(h2) < 1 {
		t.Fatal("the new version's own job requested no reconcile")
	}
	if res := rr.reconcile(h2); res.Sealed != 1 || !rr.sealed(uuid.UUID(v.Id)) {
		t.Fatalf("result %+v, want the new version sealed by its own job's end", res)
	}
}

// Old-bytes writes that reach the row while the replacement holds its lock
// wait for it, then match nothing: the compare is in the write's own
// statement, re-evaluated on the replaced row.
func TestReplace_OldBytesWritesRacingTheReplacementMatchNothing(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-lockrace")
	ctx := rr.identity(owner)
	h1 := rr.uploadPNG(owner, 64, 48)
	a := rr.createWithFile(owner, h1)
	rr.clearJobsFor(a)
	rr.setProcessing(a, "processing")
	vs := visualstore.New(rr.pool)
	pg := pgtype.UUID{Bytes: a, Valid: true}
	// The old bytes' embedding, written while they were current.
	if n, err := vs.UpsertAssetVisualEmbedding(rr.ctx, visualstore.UpsertAssetVisualEmbeddingParams{
		AssetID: pg, FileHash: h1, Embedding: vector(0.1), Model: "m", Checkpoint: "c", Provider: "p",
	}); err != nil || n != 1 {
		t.Fatalf("embedding for the current bytes: %d %v", n, err)
	}
	h2 := rr.uploadPNG(owner, 40, 30)
	rid := rr.receipt(owner, h2, nil, time.Hour)

	reached, release := make(chan struct{}), make(chan struct{})
	rr.h.SetReplaceBeforeCommitHook(func(context.Context, pgx.Tx) error {
		close(reached)
		<-release
		return nil
	})
	t.Cleanup(func() { rr.h.SetReplaceBeforeCommitHook(nil) })
	replaced := make(chan openapi.CreateAssetVersionResponseObject, 1)
	go func() {
		resp, err := rr.h.CreateAssetVersion(ctx, openapi.CreateAssetVersionRequestObject{Id: openapi_types.UUID(a),
			Body: &openapi.AssetVersionCreate{FileHash: h2, UploadReceipt: rid, ExpectedVersion: 1, FileExtension: "png"}})
		if err != nil {
			t.Errorf("replace: %v", err)
		}
		replaced <- resp
	}()
	<-reached
	before := rr.state(a) // still the old row: the replacement has not committed

	bg := context.Background()
	q := assets.New(rr.pool)
	var embedRows int64
	writes := []func() error{
		func() error { return q.MarkAssetReady(bg, assets.MarkAssetReadyParams{ID: pg, FileHash: h1}) },
		func() error {
			return q.MarkAssetFailed(bg, assets.MarkAssetFailedParams{ID: pg, FileHash: h1, ProcessingError: "late"})
		},
		func() error {
			return q.MergeAssetMetadata(bg, assets.MergeAssetMetadataParams{ID: pg, FileHash: h1, Metadata: []byte(`{"late":true}`)})
		},
		func() error { return pixeldims.Record(bg, rr.pool, a, h1, 64, 48) },
		func() error {
			n, err := vs.UpsertAssetVisualEmbedding(bg, visualstore.UpsertAssetVisualEmbeddingParams{
				AssetID: pg, FileHash: h1, Embedding: vector(0.2), Model: "m", Checkpoint: "c", Provider: "p",
			})
			embedRows = n
			return err
		},
	}
	var wg sync.WaitGroup
	errs := make([]error, len(writes))
	for i, w := range writes {
		wg.Add(1)
		go func(i int, w func() error) { defer wg.Done(); errs[i] = w() }(i, w)
	}
	rr.waitForLockWaiters(len(writes))
	close(release)
	resp := <-replaced
	wg.Wait()
	v := is201(t, "replace", resp)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racing write %d: %v", i, err)
		}
	}
	after := rr.state(a)
	if after.status != "pending" || after.errText != nil || after.attempts != before.attempts || after.meta != before.meta {
		t.Fatalf("a racing old-bytes write reached the new version: %+v", after)
	}
	if w, h := rr.dims(a); w != 0 || h != 0 {
		t.Fatalf("racing dimensions %dx%d landed", w, h)
	}
	if embedRows != 0 || rr.hasEmbedding(a) {
		t.Fatalf("the old picture's embedding survived or was rewritten (%d rows)", embedRows)
	}
	if rr.sealed(uuid.UUID(v.Id)) || rr.reconcileJobs(h2) != 0 {
		t.Fatal("a racing old-bytes write sealed, or requested the seal of, the new version")
	}
	// Its own end still works.
	if err := q.MarkAssetReady(rr.ctx, assets.MarkAssetReadyParams{ID: pg, FileHash: h2}); err != nil {
		t.Fatal(err)
	}
	if rr.state(a).status != "ready" || rr.reconcileJobs(h2) < 1 {
		t.Fatal("the new bytes' own completion did not land")
	}
}

// waitForLockWaiters returns once n backends on this database wait on a
// lock. Bounded: a write that never blocks on the replacement fails here.
func (rr *reconcileRig) waitForLockWaiters(n int) {
	rr.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&got); err != nil {
			rr.t.Fatal(err)
		}
		if got >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	rr.t.Fatalf("fewer than %d writes waited on the replacement's row lock", n)
}

// ---------------------------------------------------------------------------
// Per-bytes follow-up work (#1596 rev533)
// ---------------------------------------------------------------------------

type visualAdapter struct{ d *visualembed.Dispatcher }

func (v visualAdapter) Dispatch(ctx context.Context, in assets.VisualEmbedInput) error {
	return v.d.Dispatch(ctx, visualembed.DispatchInput{
		AssetID: in.AssetID, FileExtension: in.FileExtension, FileHash: in.FileHash, Version: in.Version, DB: in.DB,
	})
}

// gatedProvider embeds once its gate opens (nil gate: at once), telling
// started when it is called.
type gatedProvider struct {
	started chan struct{}
	gate    chan struct{}
}

func (p gatedProvider) EmbedImage(ctx context.Context, _ []byte) (visualprovider.Embedding, error) {
	if p.started != nil {
		close(p.started)
	}
	if p.gate != nil {
		<-p.gate
	}
	return visualprovider.Embedding{Vector: vector(0.3).Slice(), Dim: 768, Model: "m", Checkpoint: "c"}, nil
}
func (gatedProvider) Health(context.Context) (visualprovider.Health, error) {
	return visualprovider.Health{}, nil
}
func (gatedProvider) Info() visualprovider.Info { return visualprovider.Info{} }

type visualLookup struct{ rr *reconcileRig }

func (l visualLookup) Get(ctx context.Context, id uuid.UUID) (visualembed.AssetRecord, error) {
	var r visualembed.AssetRecord
	var deleted *time.Time
	err := l.rr.pool.QueryRow(ctx, `SELECT file_hash, file_extension, deleted_at FROM assets WHERE id = $1`, id).Scan(&r.FileHash, &r.FileExtension, &deleted)
	r.Deleted = deleted != nil
	return r, err
}

type visualStorage struct{ s *storage.Service }

func (v visualStorage) Download(ctx context.Context, hash, variant string) (io.ReadCloser, visualembed.StorageObjectInfo, error) {
	rc, _, err := v.s.Download(ctx, hash, variant)
	return rc, visualembed.StorageObjectInfo{}, err
}

type extractLookup struct{ rr *reconcileRig }

func (l extractLookup) GetAssetRef(ctx context.Context, id uuid.UUID) (assetmetadata.AssetRef, bool, error) {
	var h string
	if err := l.rr.pool.QueryRow(ctx, `SELECT file_hash FROM assets WHERE id = $1`, id).Scan(&h); err != nil {
		return assetmetadata.AssetRef{}, false, err
	}
	return assetmetadata.AssetRef{ID: id, FileHash: h}, true, nil
}

type mustNotLoad struct{ t *testing.T }

func (m mustNotLoad) LoadSource(context.Context, assetmetadata.AssetRef) (io.ReadCloser, string, error) {
	m.t.Error("a superseded extraction read the bytes")
	return nil, "", errors.New("superseded")
}

func (rr *reconcileRig) jobsFor(a uuid.UUID, jobType string) []struct{ status, hash, key string } {
	rr.t.Helper()
	rows, err := rr.pool.Query(rr.ctx, `SELECT status, COALESCE(payload->>'file_hash', ''), COALESCE(idempotency_key, '')
		FROM jobs WHERE type = $1 AND payload->>'asset_id' = $2 ORDER BY enqueued_at`, jobType, a.String())
	if err != nil {
		rr.t.Fatal(err)
	}
	defer rows.Close()
	var out []struct{ status, hash, key string }
	for rows.Next() {
		var r struct{ status, hash, key string }
		if err := rows.Scan(&r.status, &r.hash, &r.key); err != nil {
			rr.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestReplace_QueuesTheNewBytesWorkAndTheOldBytesWorkersWriteNothing(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-followup")
	ctx := rr.identity(owner)
	h1 := rr.uploadPNG(owner, 64, 48)
	a := rr.createWithFile(owner, h1)
	rr.clearJobsFor(a)
	rr.setProcessing(a, "ready")
	rr.h.SetVisualEmbedDispatcher(visualAdapter{&visualembed.Dispatcher{
		Provider: gatedProvider{}, EnabledGetter: func(context.Context) (bool, error) { return true, nil },
	}})
	t.Cleanup(func() { rr.h.SetVisualEmbedDispatcher(nil) })

	// The old bytes' embedding job, as CreateAsset queues it (per-asset key,
	// no hash), is running: the provider is embedding the old picture.
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO jobs (type, payload, status, idempotency_key, claimed_by, claimed_at, lease_expires_at, started_at)
		VALUES ('search.visual_embed', jsonb_build_object('asset_id', $1::text), 'running', 'search.visual_embed|' || $1::text, 'test', now(), now() + interval '1 hour', now())`, a.String()); err != nil {
		t.Fatal(err)
	}
	started, gate := make(chan struct{}), make(chan struct{})
	oldJob := &visualembed.Job{Assets: visualLookup{rr}, VisualStore: visualstore.New(rr.pool), Storage: visualStorage{rr.h.Storage},
		Provider: gatedProvider{started: started, gate: gate}}
	type outcome struct {
		res json.RawMessage
		err error
	}
	oldDone := make(chan outcome, 1)
	go func() {
		res, err := oldJob.Handle(context.Background(), &jobs.Claim{Type: visualembed.JobTypeVisualEmbed, Payload: []byte(`{"asset_id":"` + a.String() + `"}`)})
		oldDone <- outcome{res, err}
	}()
	<-started

	h2 := rr.uploadPNG(owner, 40, 30)
	is201(t, "replace", rr.replace(ctx, a, h2, rr.receipt(owner, h2, nil, time.Hour), 1, "png"))

	// Queued in the replacement's transaction, for the new bytes.
	extract := rr.jobsFor(a, "metadata.extract")
	var newExtract int
	for _, j := range extract {
		if j.status == "pending" && j.hash == h2 {
			newExtract++
		}
	}
	if newExtract != 1 {
		t.Fatalf("metadata.extract jobs %+v, want one pending for the new bytes", extract)
	}
	visual := rr.jobsFor(a, "search.visual_embed")
	if len(visual) != 2 || visual[0].status != "running" || visual[1].status != "pending" || visual[1].hash != h2 ||
		!strings.HasSuffix(visual[1].key, "|v2") {
		t.Fatalf("visual jobs %+v, want the old one still running and a separate pending one for v2's bytes", visual)
	}
	if rr.hasEmbedding(a) {
		t.Fatal("the old picture's embedding outlived the replacement")
	}

	// The old job finishes embedding the old picture: it writes nothing.
	close(gate)
	o := <-oldDone
	if o.err != nil || !bytes.Contains(o.res, []byte(`"superseded":true`)) {
		t.Fatalf("old embedding job: %s %v, want a superseded no-op", o.res, o.err)
	}
	if rr.hasEmbedding(a) {
		t.Fatal("the old job's embedding landed on the new version")
	}
	// The new bytes' own job embeds them.
	var payload []byte
	if err := rr.pool.QueryRow(rr.ctx, `SELECT payload FROM jobs WHERE type = 'search.visual_embed' AND status = 'pending' AND payload->>'asset_id' = $1`, a.String()).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	newJob := &visualembed.Job{Assets: visualLookup{rr}, VisualStore: visualstore.New(rr.pool), Storage: visualStorage{rr.h.Storage}, Provider: gatedProvider{}}
	if res, err := newJob.Handle(rr.ctx, &jobs.Claim{Type: visualembed.JobTypeVisualEmbed, Payload: payload}); err != nil || bytes.Contains(res, []byte("superseded")) {
		t.Fatalf("new embedding job: %s %v", res, err)
	}
	if !rr.hasEmbedding(a) {
		t.Fatal("the new bytes' embedding job wrote nothing")
	}

	// An extraction queued for the old bytes finishes without reading them,
	// and an old page count does not land; the new bytes' does.
	ex := assetmetadata.NewExtractJobHandler(mustNotLoad{t}, extractLookup{rr}, nil, nil, nil, nil)
	res, err := ex.Handle(rr.ctx, &jobs.Claim{Type: "metadata.extract", Payload: []byte(`{"asset_id":"` + a.String() + `","file_hash":"` + h1 + `"}`)})
	if err != nil || !bytes.Contains(res, []byte(`"superseded":true`)) {
		t.Fatalf("old extraction: %s %v, want a superseded no-op", res, err)
	}
	pw := assetmetadata.NewPoolAssetAttributeWriter(rr.pool)
	if err := pw.SetAssetPageCount(rr.ctx, a, h1, 9); err != nil {
		t.Fatal(err)
	}
	if s := rr.state(a); s.pages != nil {
		t.Fatalf("the old bytes' page count %d landed", *s.pages)
	}
	if err := pw.SetAssetPageCount(rr.ctx, a, h2, 4); err != nil {
		t.Fatal(err)
	}
	if s := rr.state(a); s.pages == nil || *s.pages != 4 {
		t.Fatal("the new bytes' page count did not land")
	}
}

func TestReplace_AVideoQueuesItsTranscriptionAndOldTranscriptsAreRefused(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-video")
	ctx := rr.identity(owner)
	hv1 := rr.freshUpload(owner, "video-v1")
	a := uuid.New()
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_extension, status, processing_status)
		VALUES ($1, 'a video', 3, $2, $3, 'mp4', 'active', 'ready')`, a, owner, hv1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = rr.pool.Exec(c, `DELETE FROM asset_subtitle_tracks WHERE asset_id = $1`, a)
		_, _ = rr.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, a)
	})
	rr.clearJobsFor(a)
	hv2 := rr.freshUpload(owner, "video-v2")
	is201(t, "mp4 replaced by mov", rr.replace(ctx, a, hv2, rr.receipt(owner, hv2, nil, time.Hour), 1, "mov"))
	tj := rr.jobsFor(a, "ai.transcribe")
	if len(tj) != 1 || tj[0].status != "pending" || tj[0].hash != hv2 || tj[0].key == "" {
		t.Fatalf("transcription jobs %+v, want one pending for the new bytes with a version-scoped key", tj)
	}
	if rr.previewJobs(hv2) == 0 {
		t.Fatal("the new video's preview work was not queued")
	}
	// A transcript of the old recording is refused; the new one lands.
	sq := subtitles.New(rr.pool)
	track := subtitles.UpsertSubtitleTrackParams{AssetID: pgtype.UUID{Bytes: a, Valid: true}, Lang: "en", Label: "AI",
		FileHash: hv2, SourceFormat: "whisper", Confidence: 1, AssetFileHash: hv1}
	if _, err := sq.UpsertSubtitleTrack(rr.ctx, track); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old recording's transcript: %v, want no row written", err)
	}
	track.AssetFileHash = hv2
	if _, err := sq.UpsertSubtitleTrack(rr.ctx, track); err != nil {
		t.Fatalf("new recording's transcript: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The old file's facts retire with it (#1596 rev534)
// ---------------------------------------------------------------------------

func (rr *reconcileRig) field(code, typ string) uuid.UUID {
	rr.t.Helper()
	var id uuid.UUID
	code = code + "_" + uuid.NewString()[:8]
	if err := rr.pool.QueryRow(rr.ctx, `INSERT INTO field_definition (code, label, type, subject_kind) VALUES ($1, $1, $2, 'asset') RETURNING id`, code, typ).Scan(&id); err != nil {
		rr.t.Fatal(err)
	}
	rr.t.Cleanup(func() {
		c := context.Background()
		_, _ = rr.pool.Exec(c, `DELETE FROM asset_field_value_history WHERE field_id = $1`, id)
		_, _ = rr.pool.Exec(c, `DELETE FROM asset_field_value WHERE field_id = $1`, id)
		_, _ = rr.pool.Exec(c, `DELETE FROM field_definition WHERE id = $1`, id)
	})
	return id
}

func (rr *reconcileRig) setValue(a, field uuid.UUID, text, setBy string) {
	rr.t.Helper()
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (asset_id, field_id) DO UPDATE SET value_text = EXCLUDED.value_text, set_by = EXCLUDED.set_by, set_at = now()`, a, field, text, setBy); err != nil {
		rr.t.Fatal(err)
	}
}

// value is the field's (text, set_by), "" "" when it has none.
func (rr *reconcileRig) value(a, field uuid.UUID) (text, setBy string) {
	rr.t.Helper()
	err := rr.pool.QueryRow(rr.ctx, `SELECT COALESCE(value_text, ''), set_by FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`, a, field).Scan(&text, &setBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ""
	}
	if err != nil {
		rr.t.Fatal(err)
	}
	return
}

func TestReplace_RetiresTheOldFilesFactsAndKeepsPeoples(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-facts")
	ctx := rr.identity(owner)
	h1 := rr.uploadPNG(owner, 64, 48)
	a := rr.createWithFile(owner, h1)
	rr.clearJobsFor(a)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = rr.pool.Exec(c, `DELETE FROM extraction_failure WHERE asset_id = $1`, a)
		_, _ = rr.pool.Exec(c, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, a)
	})
	rr.setProcessing(a, "ready")

	// What the old file's workers wrote, beside what people wrote.
	fExif, fIptc, fXmp := rr.field("rp_exif", "text"), rr.field("rp_iptc", "text"), rr.field("rp_xmp", "text")
	fManual, fDefault, fAPI, fComputed := rr.field("rp_manual", "text"), rr.field("rp_default", "text"), rr.field("rp_api", "text"), rr.field("rp_computed", "text")
	rr.setValue(a, fExif, "Canon", "exif")
	rr.setValue(a, fIptc, "credit line", "iptc")
	rr.setValue(a, fXmp, "rights", "xmp")
	rr.setValue(a, fManual, "a person typed this", "manual")
	rr.setValue(a, fDefault, "team default", "default")
	rr.setValue(a, fAPI, "an integration", "api")
	rr.setValue(a, fComputed, "derived from the title", "computed")
	if err := pixeldims.Record(rr.ctx, rr.pool, a, h1, 64, 48); err != nil {
		t.Fatal(err)
	}
	q := assets.New(rr.pool)
	pg := pgtype.UUID{Bytes: a, Valid: true}
	if err := q.MergeAssetMetadata(rr.ctx, assets.MergeAssetMetadataParams{ID: pg, FileHash: h1,
		Metadata: []byte(`{"pdf":{"pages":3},"text":{"lines":9},"audio":{"duration":1}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.pool.Exec(rr.ctx, `UPDATE assets SET metadata = COALESCE(metadata, '{}') || '{"client":{"k":1}}' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if err := assetmetadata.NewPoolAssetAttributeWriter(rr.pool).SetAssetPageCount(rr.ctx, a, h1, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO extraction_failure (asset_id, format, error_kind, message) VALUES ($1, 'image/png', 'validation', 'the old file')`, a); err != nil {
		t.Fatal(err)
	}
	if w, h := rr.dims(a); w != 64 || h != 48 {
		t.Fatalf("setup: dims %dx%d", w, h)
	}

	h2 := rr.uploadPNG(owner, 40, 30)
	is201(t, "replace", rr.replace(ctx, a, h2, rr.receipt(owner, h2, nil, time.Hour), 1, "png"))

	// Before the new file is processed, none of the old file's facts show.
	s := rr.state(a)
	if s.pages != nil {
		t.Fatalf("the old file's page count %d is still on the asset", *s.pages)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(s.meta), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta) != 1 || meta["client"] == nil {
		t.Fatalf("metadata %s, want only the client's own key (the preview namespaces retired)", s.meta)
	}
	if w, h := rr.dims(a); w != 0 || h != 0 {
		t.Fatalf("the old file's dimensions %dx%d are still on the asset", w, h)
	}
	for name, f := range map[string]uuid.UUID{"exif": fExif, "iptc": fIptc, "xmp": fXmp} {
		if v, _ := rr.value(a, f); v != "" {
			t.Fatalf("the old file's %s value %q is still on the asset", name, v)
		}
	}
	for name, f := range map[string]uuid.UUID{"manual": fManual, "default": fDefault, "api": fAPI, "computed": fComputed} {
		if v, by := rr.value(a, f); v == "" || by != name {
			t.Fatalf("the %s value was not kept (%q, %q)", name, v, by)
		}
	}
	// Every retired value has its history entry: old value -> null, by
	// the caller, as a replacement.
	var tomb int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM asset_field_value_history WHERE asset_id = $1
		AND set_by = 'replacement' AND old_value IS NOT NULL AND new_value IS NULL AND changed_by_user_ref = $2`, a, owner).Scan(&tomb); err != nil {
		t.Fatal(err)
	}
	var canon string
	if err := rr.pool.QueryRow(rr.ctx, `SELECT old_value->>'value' FROM asset_field_value_history WHERE asset_id = $1 AND field_id = $2 AND set_by = 'replacement'`, a, fExif).Scan(&canon); err != nil {
		t.Fatal(err)
	}
	if tomb != 5 || canon != "Canon" {
		t.Fatalf("%d replacement history entries (want 5: three extracted, two dimensions); exif old value %q", tomb, canon)
	}
	// The old file's failure is history, not a current warning.
	var pending, total int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FILTER (WHERE dismissed_at IS NULL), count(*) FROM extraction_failure WHERE asset_id = $1`, a).Scan(&pending, &total); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || total != 1 {
		t.Fatalf("extraction failures: %d pending of %d, want the old one kept and dismissed", pending, total)
	}

	// The new file's processing fills its own facts: different dimensions,
	// and the fields it does not carry stay empty.
	if err := rr.runRaster(a, h2); err != nil {
		t.Fatal(err)
	}
	if w, h := rr.dims(a); w != 40 || h != 30 {
		t.Fatalf("dims %dx%d after the new file's processing, want 40x30", w, h)
	}
	if v, _ := rr.value(a, fExif); v != "" {
		t.Fatalf("a field the new file lacks shows %q", v)
	}
}

// skip_if_set reads an extracted value as occupied, so without the
// retirement a replacement's extraction would never converge.
type oneFieldConfig struct{ f uuid.UUID }

func (c oneFieldConfig) ListExtractionConfig(context.Context) ([]assetmetadata.FieldExtractionConfig, error) {
	return []assetmetadata.FieldExtractionConfig{{FieldID: c.f, Source: assetmetadata.FieldCameraMake, Mode: assetmetadata.ExtractionModeSkipIfSet, FieldType: "text"}}, nil
}

type rowReader struct{ rr *reconcileRig }

func (r rowReader) GetAssetFieldValue(ctx context.Context, a, f uuid.UUID) (assetmetadata.FieldValueSnapshot, bool, error) {
	v, by := r.rr.value(a, f)
	if by == "" {
		return assetmetadata.FieldValueSnapshot{}, false, nil
	}
	return assetmetadata.FieldValueSnapshot{ValueText: &v, SetBy: by}, true, nil
}

type rowWriter struct{ rr *reconcileRig }

func (w rowWriter) WriteAssetFieldValue(ctx context.Context, p assetmetadata.WriteAssetFieldValueParams) error {
	w.rr.setValue(p.AssetID, p.FieldID, p.Value.Text, p.SetBy)
	return nil
}

func TestReplace_SkipIfSetExtractionConvergesOnTheNewFile(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-skipifset")
	h1 := rr.uploadPNG(owner, 64, 48)
	a := rr.createWithFile(owner, h1)
	rr.clearJobsFor(a)
	t.Cleanup(func() {
		_, _ = rr.pool.Exec(context.Background(), `DELETE FROM asset_field_value_history WHERE asset_id = $1`, a)
	})
	f := rr.field("rp_make", "text")
	ap := assetmetadata.NewApplier(oneFieldConfig{f}, rowReader{rr}, rowWriter{rr}, nil)
	newMake := assetmetadata.Result{Format: "image/png",
		Fields:       map[assetmetadata.CanonicalField]assetmetadata.Value{assetmetadata.FieldCameraMake: {Kind: assetmetadata.ValueKindText, Text: "Nikon"}},
		FieldSources: map[assetmetadata.CanonicalField]string{assetmetadata.FieldCameraMake: "exif"}}

	rr.setValue(a, f, "Canon", "exif") // the old file's extraction
	sum, err := ap.Apply(rr.ctx, assetmetadata.AssetRef{ID: a, FileHash: h1}, newMake)
	if err != nil || len(sum.FieldsSkippedMode) != 1 {
		t.Fatalf("control: %+v %v, want skip_if_set to skip an occupied field", sum, err)
	}
	h2 := rr.uploadPNG(owner, 40, 30)
	is201(t, "replace", rr.replace(rr.identity(owner), a, h2, rr.receipt(owner, h2, nil, time.Hour), 1, "png"))
	sum, err = ap.Apply(rr.ctx, assetmetadata.AssetRef{ID: a, FileHash: h2}, newMake)
	if err != nil || len(sum.FieldsSet) != 1 {
		t.Fatalf("after the replacement: %+v %v, want the new file's value written", sum, err)
	}
	if v, by := rr.value(a, f); v != "Nikon" || by != "exif" {
		t.Fatalf("value %q (%s), want the new file's Nikon", v, by)
	}
}

// ---------------------------------------------------------------------------
// The old recording's AI tracks retire with it (#1596 rev534)
// ---------------------------------------------------------------------------

func (rr *reconcileRig) trackPins(hash string) int {
	rr.t.Helper()
	var n int
	if err := rr.pool.QueryRow(rr.ctx, `SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'subtitle_track'`, hash).Scan(&n); err != nil {
		rr.t.Fatal(err)
	}
	return n
}

func (rr *reconcileRig) trackHash(a uuid.UUID, lang string) string {
	rr.t.Helper()
	var h string
	if err := rr.pool.QueryRow(rr.ctx, `SELECT file_hash FROM asset_subtitle_tracks WHERE asset_id = $1 AND lang = $2`, a, lang).Scan(&h); err != nil {
		rr.t.Fatalf("track %s: %v", lang, err)
	}
	rr.track(h)
	return h
}

func langs(ts []subtitles.Track) string {
	out := []string{}
	for _, t := range ts {
		out = append(out, t.Lang+":"+t.SourceFormat)
	}
	return strings.Join(out, ",")
}

func TestReplace_RetiresTheOldRecordingsAITracks(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-tracks")
	ctx := rr.identity(owner)
	hv1 := rr.freshUpload(owner, "tracks-v1")
	a := uuid.New()
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_extension, status, processing_status)
		VALUES ($1, 'a talk', 3, $2, $3, 'mp4', 'active', 'ready')`, a, owner, hv1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = rr.pool.Exec(c, `DELETE FROM asset_subtitle_tracks WHERE asset_id = $1`, a)
		_, _ = rr.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, a)
		_, _ = rr.pool.Exec(c, `DELETE FROM storage_pins WHERE pin_subject_type = 'subtitle_track' AND pin_subject_id LIKE $1`, a.String()+"-%")
	})
	rr.clearJobsFor(a)

	// Two processes: this one (shares the replacing handler's registry)
	// and a peer that only hears the NOTIFY.
	regA, regB := cache.NewRegistry(rr.pool, nil), cache.NewRegistry(rr.pool, nil)
	listenCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	if err := regB.Start(listenCtx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(regB.Stop)
	shA, shB := subtitles.NewHandler(rr.pool, regA, nil), subtitles.NewHandler(rr.pool, regB, nil)
	rr.h.SetCacheRegistryForTest(regA)
	t.Cleanup(func() { rr.h.SetCacheRegistryForTest(nil) })
	writer := aitranscribe.NewWriter(aitranscribe.NewStorageAdapter(rr.h.Storage), shA, nil)
	transcribe := func(lang, words, from string) error {
		return writer.SetAITranscriptForAsset(rr.ctx, ai.TranscriptInput{AssetID: a, Language: lang,
			VTTContent: []byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\n" + words + "\n"), Confidence: 0.9, ExpectedFileHash: from})
	}

	// The old recording: AI tracks in en and fr, a person's track in de.
	if err := transcribe("en", "old english "+a.String(), hv1); err != nil {
		t.Fatal(err)
	}
	if err := transcribe("fr", "vieux "+a.String(), hv1); err != nil {
		t.Fatal(err)
	}
	oldEN, oldFR := rr.trackHash(a, "en"), rr.trackHash(a, "fr")
	deRes, err := rr.h.Storage.UploadOriginal(rr.ctx, strings.NewReader("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nperson "+a.String()+"\n"), "text/vtt",
		storage.PinRef{SubjectType: subtitles.PinSubjectTypeTrack, SubjectID: subtitles.TrackPinSubjectID(a, "de")})
	if err != nil {
		t.Fatal(err)
	}
	rr.track(deRes.Hash)
	if _, err := shA.Upsert(rr.ctx, subtitles.Track{AssetID: a, Lang: "de", Label: "Deutsch", FileHash: deRes.Hash, SourceFormat: "vtt", Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	// Both processes have the list cached.
	for _, sh := range []*subtitles.Handler{shA, shB} {
		if ts, err := sh.GetForAsset(rr.ctx, a); err != nil || langs(ts) != "de:vtt,en:whisper,fr:whisper" {
			t.Fatalf("warm cache: %s %v", langs(ts), err)
		}
	}

	hv2 := rr.freshUpload(owner, "tracks-v2")
	is201(t, "replace", rr.replace(ctx, a, hv2, rr.receipt(owner, hv2, nil, time.Hour), 1, "mov"))

	// Only the person's track is left, in the rows and in both caches.
	if ts, err := shA.GetForAsset(rr.ctx, a); err != nil || langs(ts) != "de:vtt" {
		t.Fatalf("this process serves %s (%v) after the replacement, want only the person's track", langs(ts), err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ts, err := shB.GetForAsset(rr.ctx, a)
		if err == nil && langs(ts) == "de:vtt" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the peer process still serves %s (%v)", langs(ts), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The retired tracks' bytes lost their pin and are scheduled for GC;
	// the person's keep theirs.
	for name, h := range map[string]string{"en": oldEN, "fr": oldFR} {
		if rr.trackPins(h) != 0 || rr.gcEligibleAt(h) == nil {
			t.Fatalf("the retired %s track's bytes are still pinned or unscheduled", name)
		}
	}
	if rr.trackPins(deRes.Hash) != 1 || rr.gcEligibleAt(deRes.Hash) != nil {
		t.Fatal("the person's track lost its pin")
	}

	// The new recording's transcription: the same language again, and a
	// different detected language.
	if err := transcribe("en", "new english "+a.String(), hv2); err != nil {
		t.Fatal(err)
	}
	if err := transcribe("es", "nuevo "+a.String(), hv2); err != nil {
		t.Fatal(err)
	}
	newEN := rr.trackHash(a, "en")
	if ts, _ := shA.GetForAsset(rr.ctx, a); langs(ts) != "de:vtt,en:whisper,es:whisper" || newEN == oldEN {
		t.Fatalf("tracks %s after the new transcription, want de, a new en and es (no fr)", langs(ts))
	}

	// A transcriber still listening to the old recording finishes late: its
	// write is refused and its stored bytes released...
	late := "late english " + a.String()
	if err := transcribe("en", late, hv1); !errors.Is(err, ai.ErrAssetSuperseded) {
		t.Fatalf("late old transcript: %v, want ErrAssetSuperseded", err)
	}
	lateHash := digest([]byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\n" + late + "\n"))
	rr.track(lateHash)
	if rr.trackPins(lateHash) != 0 || rr.gcEligibleAt(lateHash) == nil {
		t.Fatal("the refused transcript's bytes kept a permanent pin")
	}
	// ...but a late one whose bytes equal the live track's leaves that pin.
	if err := transcribe("en", "new english "+a.String(), hv1); !errors.Is(err, ai.ErrAssetSuperseded) {
		t.Fatalf("late identical transcript: %v, want ErrAssetSuperseded", err)
	}
	if rr.trackPins(newEN) != 1 || rr.gcEligibleAt(newEN) != nil {
		t.Fatal("releasing a refused transcript unpinned the live track with the same bytes")
	}
	if ts, _ := shA.GetForAsset(rr.ctx, a); langs(ts) != "de:vtt,en:whisper,es:whisper" || rr.trackHash(a, "en") != newEN {
		t.Fatalf("tracks %s after the late writes, want them unchanged", langs(ts))
	}

	// A release can also land between a live writer storing its bytes and
	// writing its row (a refused late transcript with identical bytes):
	// the row's own transaction asserts the pin again.
	racing := "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nracing " + a.String() + "\n"
	res, err := rr.h.Storage.UploadOriginal(rr.ctx, strings.NewReader(racing), "text/vtt",
		storage.PinRef{SubjectType: subtitles.PinSubjectTypeTrack, SubjectID: subtitles.TrackPinSubjectID(a, "es")})
	if err != nil {
		t.Fatal(err)
	}
	rr.track(res.Hash)
	if err := shA.ReleaseUnusedTrackBytes(rr.ctx, a, "es", res.Hash); err != nil {
		t.Fatal(err)
	}
	if rr.trackPins(res.Hash) != 0 {
		t.Fatal("setup: the release kept a pin no track used")
	}
	if _, err := shA.Upsert(rr.ctx, subtitles.Track{AssetID: a, Lang: "es", Label: "AI", FileHash: res.Hash,
		SourceFormat: subtitles.SourceFormatAI, Confidence: 1, ExpectedAssetFileHash: hv2}); err != nil {
		t.Fatal(err)
	}
	if rr.trackPins(res.Hash) != 1 || rr.gcEligibleAt(res.Hash) != nil {
		t.Fatal("a track row committed without its pin")
	}
}

// ---------------------------------------------------------------------------
// A track's bytes are held exactly while a track uses them (#1596 rev535)
// ---------------------------------------------------------------------------

// Overwriting a track, deleting it, or failing to write it after its
// bytes were stored releases the bytes no track uses any more; a
// replacement then leaves nothing of the old recording pinned.
func TestSubtitles_SupersededTrackBytesLoseTheirPin(t *testing.T) {
	rr := newReconcileRig(t)
	owner := rr.user("rp-trackpins")
	ctx := rr.identity(owner)
	hv1 := rr.freshUpload(owner, "trackpins-v1")
	a := uuid.New()
	if _, err := rr.pool.Exec(rr.ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_extension, status, processing_status)
		VALUES ($1, 'a lecture', 3, $2, $3, 'mp4', 'active', 'ready')`, a, owner, hv1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = rr.pool.Exec(c, `DELETE FROM asset_subtitle_tracks WHERE asset_id = $1`, a)
		_, _ = rr.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, a)
		_, _ = rr.pool.Exec(c, `DELETE FROM storage_pins WHERE pin_subject_type = 'subtitle_track' AND pin_subject_id LIKE $1`, a.String()+"-%")
	})
	rr.clearJobsFor(a)
	sh := subtitles.NewHandler(rr.pool, nil, nil)
	st := aitranscribe.NewStorageAdapter(rr.h.Storage)
	writer := aitranscribe.NewWriter(st, sh, nil)
	vtt := func(words string) string {
		return "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\n" + words + " " + a.String() + "\n"
	}
	transcribe := func(lang, words, from string) error {
		return writer.SetAITranscriptForAsset(rr.ctx, ai.TranscriptInput{AssetID: a, Language: lang, VTTContent: []byte(vtt(words)), Confidence: 0.9, ExpectedFileHash: from})
	}
	held := func(why, hash string) {
		t.Helper()
		if rr.trackPins(hash) != 1 || rr.gcEligibleAt(hash) != nil {
			t.Fatalf("%s: bytes in use are not held (%d pins)", why, rr.trackPins(hash))
		}
	}
	released := func(why, hash string) {
		t.Helper()
		if rr.trackPins(hash) != 0 || rr.gcEligibleAt(hash) == nil {
			t.Fatalf("%s: bytes no track uses keep %d pins (GC scheduled: %v)", why, rr.trackPins(hash), rr.gcEligibleAt(hash) != nil)
		}
	}

	// The AI transcribes the same language twice: only the current
	// transcript's bytes stay held. Re-writing identical bytes keeps them.
	if err := transcribe("en", "first take", hv1); err != nil {
		t.Fatal(err)
	}
	x1 := rr.trackHash(a, "en")
	if err := transcribe("en", "second take", hv1); err != nil {
		t.Fatal(err)
	}
	x2 := rr.trackHash(a, "en")
	released("an overwritten AI transcript", x1)
	held("the current AI transcript", x2)
	if err := transcribe("en", "second take", hv1); err != nil {
		t.Fatal(err)
	}
	held("an AI transcript rewritten with identical bytes", x2)

	// A write that fails after its bytes were stored (an invalid language
	// here) releases them.
	bad := vtt("bad language")
	if err := writer.SetAITranscriptForAsset(rr.ctx, ai.TranscriptInput{AssetID: a, Language: "not a language", VTTContent: []byte(bad), Confidence: 0.9, ExpectedFileHash: hv1}); err == nil || errors.Is(err, ai.ErrAssetSuperseded) {
		t.Fatalf("an invalid language: %v, want a plain failure", err)
	}
	badHash := digest([]byte(bad))
	rr.track(badHash)
	released("a failed AI write", badHash)

	// A person uploads, overwrites and deletes a track through the API.
	up := subtitles.NewHTTPHandler(sh, subtitles.NewStorageAdapter(rr.h.Storage), nil, nil)
	upload := func(lang, words string) string {
		t.Helper()
		resp, err := up.UploadSubtitleTrack(ctx, openapi.UploadSubtitleTrackRequestObject{Id: openapi_types.UUID(a),
			Body: &openapi.UploadSubtitleTrackJSONRequestBody{Lang: lang, Content: vtt(words), SourceFormat: openapi.UploadSubtitleTrackJSONBodySourceFormatVtt}})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := resp.(openapi.UploadSubtitleTrack202JSONResponse); !ok {
			t.Fatalf("upload %s: %T %+v", lang, resp, resp)
		}
		return rr.trackHash(a, lang)
	}
	d1 := upload("de", "erste Fassung")
	d2 := upload("de", "zweite Fassung")
	released("an overwritten person's track", d1)
	held("the person's current track", d2)
	if resp, err := up.DeleteSubtitleTrack(ctx, openapi.DeleteSubtitleTrackRequestObject{Id: openapi_types.UUID(a), Lang: "de"}); err != nil {
		t.Fatal(err)
	} else if _, ok := resp.(openapi.DeleteSubtitleTrack204Response); !ok {
		t.Fatalf("delete: %T", resp)
	}
	released("a deleted person's track", d2)
	f1 := upload("fr", "version humaine")

	// The replacement retires the AI track: nothing of the old recording's
	// transcripts stays pinned; the person's track keeps its pin.
	hv2 := rr.freshUpload(owner, "trackpins-v2")
	is201(t, "replace", rr.replace(ctx, a, hv2, rr.receipt(owner, hv2, nil, time.Hour), 1, "mov"))
	released("the first AI transcript after the replacement", x1)
	released("the current AI transcript after the replacement", x2)
	held("the person's track after the replacement", f1)
}
