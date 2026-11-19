// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1595: an asset's original base name is write-once ingest provenance,
// carried from the upload request by that upload's receipt. Pinned here
// through the production handlers on a real database:
//
//   - a receipt gives the new asset its upload's name as upload_request,
//     on the create response, the detail response and the row;
//   - it is single use, bound to its user and its exact hash, and
//     expires; a malformed one, or one without a file_hash, is refused;
//     every miss is the same 400;
//   - no receipt is unknown, even beside metadata.original_filename;
//   - a duplicate upload answered from the dedup check keeps the
//     existing asset's provenance and leaves its own receipt unused;
//   - editing metadata.original_filename changes nothing typed, and the
//     database refuses any UPDATE of the pair.

package assets_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

type basenameRig struct {
	pool    *pgxpool.Pool
	storage *storage.Handler
	assets  *assets.Handler
}

func newBasenameRig(t *testing.T) *basenameRig {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	svc.TempDir = t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &basenameRig{
		pool:    pool,
		storage: storage.NewHandler(svc, logger),
		assets:  assets.NewHandler(pool, svc, logger, nil, nil, nil),
	}
}

func basenameCtx(user int64) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{UserRef: user, AuthMethod: "session"})
}

// upload stores random bytes as user, sending name (nil: no header), and
// returns the hash and receipt. Rows are removed when the test ends.
func (r *basenameRig) upload(t *testing.T, user int64, name *string) (hash, receipt string) {
	t.Helper()
	payload := make([]byte, 1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	return r.uploadBytes(t, user, payload, name)
}

func (r *basenameRig) uploadBytes(t *testing.T, user int64, payload []byte, name *string) (hash, receipt string) {
	t.Helper()
	resp, err := r.storage.UploadStorageObject(basenameCtx(user), openapi.UploadStorageObjectRequestObject{
		Params: openapi.UploadStorageObjectParams{XOriginalFilename: name},
		Body:   bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	ok, isOK := resp.(openapi.UploadStorageObject201JSONResponse)
	if !isOK {
		t.Fatalf("upload answered %T", resp)
	}
	hash = ok.Hash
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = r.pool.Exec(ctx, `DELETE FROM assets WHERE file_hash = $1`, hash)
		_, _ = r.pool.Exec(ctx, `DELETE FROM upload_receipts WHERE file_hash = $1`, hash)
		_, _ = r.pool.Exec(ctx, `DELETE FROM storage_pins WHERE object_hash = $1`, hash)
		_, _ = r.pool.Exec(ctx, `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
		_, _ = r.pool.Exec(ctx, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
	return hash, ok.UploadReceipt
}

func (r *basenameRig) create(t *testing.T, user int64, hash string, receipt *string, meta map[string]any) openapi.CreateAssetResponseObject {
	t.Helper()
	ext := "png"
	body := &openapi.AssetCreate{Title: "ob-1595", AssetType: 1, FileHash: &hash, FileExtension: &ext, UploadReceipt: receipt}
	if meta != nil {
		body.Metadata = &meta
	}
	resp, err := r.assets.CreateAsset(basenameCtx(user), openapi.CreateAssetRequestObject{Body: body})
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	return resp
}

func (r *basenameRig) receiptExists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM upload_receipts WHERE id = $1::uuid`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func (r *basenameRig) stored(t *testing.T, id openapi_types.UUID) (string, string) {
	t.Helper()
	var name *string
	var source string
	if err := r.pool.QueryRow(context.Background(), `SELECT original_basename, original_basename_source FROM assets WHERE id = $1`, uuid.UUID(id)).Scan(&name, &source); err != nil {
		t.Fatal(err)
	}
	if name == nil {
		return "<nil>", source
	}
	return *name, source
}

func want201(t *testing.T, resp openapi.CreateAssetResponseObject) openapi.Asset {
	t.Helper()
	ok, isOK := resp.(openapi.CreateAsset201JSONResponse)
	if !isOK {
		t.Fatalf("create answered %T %+v, want 201", resp, resp)
	}
	return openapi.Asset(ok)
}

func want400Receipt(t *testing.T, resp openapi.CreateAssetResponseObject) {
	t.Helper()
	bad, isBad := resp.(openapi.CreateAsset400JSONResponse)
	if !isBad || bad.Error != "upload_receipt is not valid for this upload" {
		t.Fatalf("create answered %T %+v, want the upload_receipt 400", resp, resp)
	}
}

func apiName(a openapi.Asset) (string, string) {
	name, source := "<nil>", "<nil>"
	if a.OriginalBasename != nil {
		name = *a.OriginalBasename
	}
	if a.OriginalBasenameSource != nil {
		source = string(*a.OriginalBasenameSource)
	}
	return name, source
}

func TestCreateAsset_UploadReceiptCarriesTheUploadRequestName(t *testing.T) {
	r := newBasenameRig(t)
	const alice int64 = 9_159_601
	name := "work%2Fhero%20shot.png"
	hash, receipt := r.upload(t, alice, &name)

	a := want201(t, r.create(t, alice, hash, &receipt, map[string]any{"original_filename": "client says.png"}))
	if n, s := apiName(a); n != "hero shot.png" || s != "upload_request" {
		t.Fatalf("create response: %q %q", n, s)
	}
	if n, s := r.stored(t, a.Id); n != "hero shot.png" || s != "upload_request" {
		t.Fatalf("row: %q %q", n, s)
	}
	if r.receiptExists(t, receipt) {
		t.Fatal("receipt still present after the create used it")
	}
	got, err := r.assets.GetAsset(basenameCtx(alice), openapi.GetAssetRequestObject{Id: a.Id})
	if err != nil {
		t.Fatal(err)
	}
	detail, ok := got.(openapi.GetAsset200JSONResponse)
	if !ok {
		t.Fatalf("GetAsset answered %T", got)
	}
	if n, s := apiName(openapi.Asset(detail)); n != "hero shot.png" || s != "upload_request" {
		t.Fatalf("detail: %q %q", n, s)
	}
	// The editable claim is still there, beside (not instead of) the typed value.
	if detail.Metadata == nil || (*detail.Metadata)["original_filename"] != "client says.png" {
		t.Fatalf("metadata.original_filename lost: %v", detail.Metadata)
	}

	// Edit the claim: the typed provenance does not move.
	meta := map[string]any{"original_filename": "renamed.png"}
	up, err := r.assets.UpdateAsset(basenameCtx(alice), openapi.UpdateAssetRequestObject{
		Id: a.Id, Body: &openapi.AssetUpdate{Metadata: &meta},
	})
	if err != nil {
		t.Fatal(err)
	}
	edited, ok := up.(openapi.UpdateAsset200JSONResponse)
	if !ok {
		t.Fatalf("UpdateAsset answered %T", up)
	}
	if n, s := apiName(openapi.Asset(edited)); n != "hero shot.png" || s != "upload_request" {
		t.Fatalf("edit response: %q %q", n, s)
	}
	if n, s := r.stored(t, a.Id); n != "hero shot.png" || s != "upload_request" {
		t.Fatalf("row after edit: %q %q", n, s)
	}

	// Nothing else can change it either: the database refuses.
	for _, q := range []string{
		`UPDATE assets SET original_basename = 'other.png' WHERE id = $1`,
		`UPDATE assets SET original_basename_source = 'client_reported' WHERE id = $1`,
		`UPDATE assets SET original_basename = NULL, original_basename_source = 'unknown' WHERE id = $1`,
	} {
		if _, err := r.pool.Exec(context.Background(), q, uuid.UUID(a.Id)); err == nil {
			t.Fatalf("%s: accepted", q)
		}
	}
	// A same-value write and an unrelated update still work.
	if _, err := r.pool.Exec(context.Background(),
		`UPDATE assets SET original_basename = original_basename, title = 'ob-1595 retitled' WHERE id = $1`, uuid.UUID(a.Id)); err != nil {
		t.Fatalf("unrelated update refused: %v", err)
	}
}

func TestCreateAsset_UploadReceiptIsBoundAndSingleUse(t *testing.T) {
	r := newBasenameRig(t)
	const alice, bob int64 = 9_159_602, 9_159_603
	name := "a.png"

	// Another user's receipt: refused, and still Alice's to use.
	hash, receipt := r.upload(t, alice, &name)
	want400Receipt(t, r.create(t, bob, hash, &receipt, nil))
	if !r.receiptExists(t, receipt) {
		t.Fatal("a refused create consumed the receipt")
	}

	// A receipt for other bytes: refused, untouched.
	otherHash, otherReceipt := r.upload(t, alice, &name)
	want400Receipt(t, r.create(t, alice, hash, &otherReceipt, nil))
	if !r.receiptExists(t, otherReceipt) {
		t.Fatal("a wrong-hash create consumed the receipt")
	}

	// Its own: used once.
	a := want201(t, r.create(t, alice, hash, &receipt, nil))
	if n, s := apiName(a); n != "a.png" || s != "upload_request" {
		t.Fatalf("create: %q %q", n, s)
	}
	// Again, once the first asset is gone (so the duplicate check does
	// not answer first): the receipt is spent.
	if _, err := r.pool.Exec(context.Background(), `UPDATE assets SET deleted_at = now() WHERE id = $1`, uuid.UUID(a.Id)); err != nil {
		t.Fatal(err)
	}
	want400Receipt(t, r.create(t, alice, hash, &receipt, nil))

	// Expired: refused.
	if _, err := r.pool.Exec(context.Background(),
		`UPDATE upload_receipts SET created_at = now() - interval '3 days', expires_at = now() - interval '1 day' WHERE id = $1::uuid`, otherReceipt); err != nil {
		t.Fatal(err)
	}
	want400Receipt(t, r.create(t, alice, otherHash, &otherReceipt, nil))

	// Malformed, and a receipt with no file_hash.
	junk := "not-a-receipt"
	want400Receipt(t, r.create(t, alice, otherHash, &junk, nil))
	resp, err := r.assets.CreateAsset(basenameCtx(alice), openapi.CreateAssetRequestObject{Body: &openapi.AssetCreate{
		Title: "ob-1595 no file", AssetType: 1, UploadReceipt: &otherReceipt,
	}})
	if err != nil {
		t.Fatal(err)
	}
	want400Receipt(t, resp)
}

func TestCreateAsset_NoReceiptIsUnknown_EvenWithAClaim(t *testing.T) {
	r := newBasenameRig(t)
	const carol int64 = 9_159_604
	name := "sent.png"
	hash, receipt := r.upload(t, carol, &name)
	a := want201(t, r.create(t, carol, hash, nil, map[string]any{"original_filename": "claimed.png"}))
	if n, s := apiName(a); n != "<nil>" || s != "unknown" {
		t.Fatalf("an old client's create: %q %q, want unknown", n, s)
	}
	if n, s := r.stored(t, a.Id); n != "<nil>" || s != "unknown" {
		t.Fatalf("row: %q %q", n, s)
	}
	if !r.receiptExists(t, receipt) {
		t.Fatal("a create without the receipt consumed it")
	}
}

func TestCreateAsset_DuplicateUploadKeepsTheExistingProvenance(t *testing.T) {
	r := newBasenameRig(t)
	const dave int64 = 9_159_605
	payload := make([]byte, 1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	first := "first.png"
	hash, r1 := r.uploadBytes(t, dave, payload, &first)
	a := want201(t, r.create(t, dave, hash, &r1, nil))

	second := "second.png"
	_, r2 := r.uploadBytes(t, dave, payload, &second)
	resp := r.create(t, dave, hash, &r2, nil)
	dup, ok := resp.(openapi.CreateAsset200JSONResponse)
	if !ok {
		t.Fatalf("duplicate create answered %T, want the 200 duplicate warning", resp)
	}
	if dup.Id != a.Id {
		t.Fatalf("duplicate names %v, want %v", dup.Id, a.Id)
	}
	if dup.OriginalBasename == nil || *dup.OriginalBasename != "first.png" ||
		dup.OriginalBasenameSource == nil || string(*dup.OriginalBasenameSource) != "upload_request" {
		t.Fatalf("duplicate response provenance %v %v, want the existing first.png", dup.OriginalBasename, dup.OriginalBasenameSource)
	}
	if n, s := r.stored(t, a.Id); n != "first.png" || s != "upload_request" {
		t.Fatalf("existing row rewritten: %q %q", n, s)
	}
	if !r.receiptExists(t, r2) {
		t.Fatal("the duplicate upload's receipt was consumed")
	}
	var live int
	if err := r.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM assets WHERE file_hash = $1 AND owner_user_ref = $2 AND deleted_at IS NULL`, hash, dave).Scan(&live); err != nil || live != 1 {
		t.Fatalf("live rows %d (%v), want 1", live, err)
	}
}
