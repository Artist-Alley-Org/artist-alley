package brushpacks

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// The strict-server handlers run their auth gate before touching the
// service, so a Handler with Service=nil exercises just the early-
// return paths. That's the most valuable contract to pin: the gates
// must NEVER leak existence (every protected endpoint returns the
// same 404 regardless of whether the resource exists for another
// owner).

func TestListBrushPacks_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.ListBrushPacks(context.Background(), openapi.ListBrushPacksRequestObject{})
	if err != nil {
		t.Fatalf("ListBrushPacks: %v", err)
	}
	if _, ok := resp.(openapi.ListBrushPacks401JSONResponse); !ok {
		t.Errorf("expected 401, got %T", resp)
	}
}

func TestImportBrushPack_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.ImportBrushPack(context.Background(), openapi.ImportBrushPackRequestObject{})
	if err != nil {
		t.Fatalf("ImportBrushPack: %v", err)
	}
	// Multipart endpoint surfaces unauth as 400 with a clear error
	// rather than 401 (oapi-codegen multipart strict-server doesn't
	// declare 401 for upload endpoints).
	if _, ok := resp.(openapi.ImportBrushPack400JSONResponse); !ok {
		t.Errorf("expected 400, got %T", resp)
	}
}

// 404-leak guard: an unauthenticated GET on a pack must NOT look
// different from a GET on a missing pack — same 404 both ways so
// pack existence can't be probed by enumerating UUIDs.
func TestGetBrushPack_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.GetBrushPack(context.Background(), openapi.GetBrushPackRequestObject{
		PackId: openapi_types.UUID(uuid.New()),
	})
	if err != nil {
		t.Fatalf("GetBrushPack: %v", err)
	}
	if _, ok := resp.(openapi.GetBrushPack404JSONResponse); !ok {
		t.Errorf("expected 404, got %T", resp)
	}
}

func TestDeleteBrushPack_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.DeleteBrushPack(context.Background(), openapi.DeleteBrushPackRequestObject{
		PackId: openapi_types.UUID(uuid.New()),
	})
	if err != nil {
		t.Fatalf("DeleteBrushPack: %v", err)
	}
	if _, ok := resp.(openapi.DeleteBrushPack404JSONResponse); !ok {
		t.Errorf("expected 404, got %T", resp)
	}
}

func TestGetBrushPackStamp_Unauthenticated(t *testing.T) {
	h := &Handler{}
	resp, err := h.GetBrushPackStamp(context.Background(), openapi.GetBrushPackStampRequestObject{
		StampId: openapi_types.UUID(uuid.New()),
	})
	if err != nil {
		t.Fatalf("GetBrushPackStamp: %v", err)
	}
	if _, ok := resp.(openapi.GetBrushPackStamp404JSONResponse); !ok {
		t.Errorf("expected 404, got %T", resp)
	}
}

// withAuth is a helper for tests that need a logged-in context. We
// don't actually exercise the service in this file (no DB) but the
// presence of an identity proves the gate passes.
func withAuth(t *testing.T) context.Context {
	t.Helper()
	return auth.WithIdentity(context.Background(), &auth.Identity{UserRef: 42})
}

func TestImportBrushPack_AuthedButNoBody(t *testing.T) {
	h := &Handler{}
	resp, err := h.ImportBrushPack(withAuth(t), openapi.ImportBrushPackRequestObject{Body: nil})
	if err != nil {
		t.Fatalf("ImportBrushPack: %v", err)
	}
	r, ok := resp.(openapi.ImportBrushPack400JSONResponse)
	if !ok {
		t.Fatalf("expected 400, got %T", resp)
	}
	if r.Error != "missing body" {
		t.Errorf("error = %q, want 'missing body'", r.Error)
	}
}

// packToAPI / stampToAPI are the wire-shape converters that go to
// every API consumer. A missed-field bug here ships silently — no
// runtime crash, just data the frontend can't render.
func TestPackToAPI_RoundtripsFields(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	sourceFile := "my-pack.abr"
	packID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	stampID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	pack := BrushPack{
		ID:        pgtype.UUID{Bytes: packID, Valid: true},
		OwnerRef:  10,
		Name:      "My Pack",
		SourceFile: &sourceFile,
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
	label := "stamp-1"
	sj := 0.25
	stamps := []BrushPackStamp{{
		ID:          pgtype.UUID{Bytes: stampID, Valid: true},
		PackID:      pack.ID,
		Label:       &label,
		Width:       64,
		Height:      32,
		StorageKey:  "abc123",
		Spacing:     0.1,
		AlignToPath: true,
		SizeJitter:  &sj,
	}}

	got := packToAPI(pack, stamps)
	if got.Id != openapi_types.UUID(packID) {
		t.Errorf("pack Id mismatch: %v vs %v", got.Id, packID)
	}
	if got.Name != "My Pack" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.SourceFile == nil || *got.SourceFile != "my-pack.abr" {
		t.Errorf("SourceFile = %v", got.SourceFile)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, now)
	}
	if len(got.Stamps) != 1 {
		t.Fatalf("Stamps = %d, want 1", len(got.Stamps))
	}
	s := got.Stamps[0]
	if s.Id != openapi_types.UUID(stampID) {
		t.Errorf("stamp Id mismatch")
	}
	if s.Width != 64 || s.Height != 32 {
		t.Errorf("dims = %dx%d", s.Width, s.Height)
	}
	if s.Label == nil || *s.Label != "stamp-1" {
		t.Errorf("Label = %v", s.Label)
	}
	if s.SizeJitter == nil || *s.SizeJitter != 0.25 {
		t.Errorf("SizeJitter = %v", s.SizeJitter)
	}
	if !s.AlignToPath {
		t.Error("AlignToPath = false")
	}
}

// nil pointer fields should NOT show up as zero-value strings on the
// wire — the converter must keep them nil so JSON omits them.
func TestPackToAPI_OmitsNilOptionalFields(t *testing.T) {
	pack := BrushPack{
		ID:        pgtype.UUID{Bytes: uuid.New(), Valid: true},
		Name:      "No source",
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	got := packToAPI(pack, nil)
	if got.SourceFile != nil {
		t.Errorf("SourceFile = %v, want nil", got.SourceFile)
	}
}

func TestUUIDPgRoundtrip(t *testing.T) {
	for i := 0; i < 5; i++ {
		raw := uuid.New()
		pg := uuidToPg(openapi_types.UUID(raw))
		if !pg.Valid {
			t.Error("pg.Valid = false")
		}
		back := pgToUUID(pg)
		if back != openapi_types.UUID(raw) {
			t.Errorf("roundtrip failed: %v vs %v", back, raw)
		}
	}
}

// bytesReader is a tiny shim around []byte → io.Reader. The contract
// matters: it must signal EOF exactly once at end-of-buffer (a buggy
// io.Reader that loops forever locks the parser).
func TestBytesReader(t *testing.T) {
	r := bytesReader([]byte("hello world"))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != "hello world" {
		t.Errorf("got %q, want %q", string(out), "hello world")
	}
	// Subsequent read returns EOF.
	buf := make([]byte, 1)
	n, err := r.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("post-EOF read: n=%d err=%v, want 0 + EOF", n, err)
	}
}

func TestBytesReader_Empty(t *testing.T) {
	r := bytesReader(nil)
	buf := make([]byte, 4)
	n, err := r.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("empty read: n=%d err=%v, want 0 + EOF", n, err)
	}
}
