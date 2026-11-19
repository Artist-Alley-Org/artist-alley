// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1595: POST /storage/objects records the file name the request carried
// (X-Original-Filename) on a per-upload receipt, never on the shared
// content-addressed object. Pinned here through the production strict
// handler chain:
//
//   - each upload of the same bytes gets its own receipt with its own
//     name and user, while storage_objects stays one shared row;
//   - an absent header is a receipt with source unknown;
//   - a supplied value that is not a safe base name answers 400 before
//     any byte is stored, and is not echoed;
//   - expired receipts are swept by later uploads.

package storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

// receiptRig is the production handler chain with the caller taken from
// a test header, so one test can upload as two users.
type receiptRig struct {
	pool   *pgxpool.Pool
	router http.Handler
}

func newReceiptRig(t *testing.T) *receiptRig {
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
	h := storage.NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ref, _ := strconv.ParseInt(r.Header.Get("X-Test-User"), 10, 64)
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), &auth.Identity{UserRef: ref, AuthMethod: "session"})))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return &receiptRig{pool: pool, router: router}
}

// upload POSTs payload as user; name nil sends no X-Original-Filename.
func (r *receiptRig) upload(t *testing.T, user int64, payload []byte, name *string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/storage/objects", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Test-User", strconv.FormatInt(user, 10))
	if name != nil {
		req.Header.Set("X-Original-Filename", *name)
	}
	rr := httptest.NewRecorder()
	r.router.ServeHTTP(rr, req)
	return rr
}

func randomPayload(t *testing.T) ([]byte, string) {
	t.Helper()
	b := make([]byte, 2048)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

func (r *receiptRig) cleanup(t *testing.T, hash string, users ...int64) {
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = r.pool.Exec(ctx, `DELETE FROM upload_receipts WHERE file_hash = $1`, hash)
		for _, u := range users {
			_, _ = r.pool.Exec(ctx, `DELETE FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'user' AND pin_subject_id = $2`, hash, strconv.FormatInt(u, 10))
		}
		_, _ = r.pool.Exec(ctx, `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
		_, _ = r.pool.Exec(ctx, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
}

type storedReceipt struct {
	user     int64
	hash     string
	basename *string
	source   string
}

func (r *receiptRig) receipt(t *testing.T, id string) storedReceipt {
	t.Helper()
	var s storedReceipt
	var ttlOK bool
	if err := r.pool.QueryRow(context.Background(), `
		SELECT user_ref, file_hash, original_basename, original_basename_source,
		       expires_at BETWEEN now() + interval '23 hours' AND now() + interval '25 hours'
		FROM upload_receipts WHERE id = $1::uuid`, id).Scan(&s.user, &s.hash, &s.basename, &s.source, &ttlOK); err != nil {
		t.Fatalf("receipt %s: %v", id, err)
	}
	if !ttlOK {
		t.Errorf("receipt %s does not expire about a day from now", id)
	}
	return s
}

func decodeUpload(t *testing.T, rr *httptest.ResponseRecorder) openapi.UploadResult {
	t.Helper()
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: status %d body %s", rr.Code, rr.Body.String())
	}
	var ur openapi.UploadResult
	if err := json.Unmarshal(rr.Body.Bytes(), &ur); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return ur
}

func TestUpload_OriginalFilename_OneReceiptPerUpload(t *testing.T) {
	r := newReceiptRig(t)
	const alice, bob int64 = 9_159_501, 9_159_502
	payload, hash := randomPayload(t)
	r.cleanup(t, hash, alice, bob)

	// Alice: a percent-encoded path; only its base name is kept.
	aliceName := "art%2Fsketch%20one.psd"
	a := decodeUpload(t, r.upload(t, alice, payload, &aliceName))
	if a.Hash != hash || a.Deduped {
		t.Fatalf("first upload: hash %s deduped %v", a.Hash, a.Deduped)
	}
	if a.OriginalBasename == nil || *a.OriginalBasename != "sketch one.psd" || string(a.OriginalBasenameSource) != "upload_request" {
		t.Fatalf("alice result: basename %v source %q", a.OriginalBasename, a.OriginalBasenameSource)
	}

	// Bob: the same bytes under a Windows path: deduped object, own receipt.
	bobName := `C:\Users\bob\other.psd`
	b := decodeUpload(t, r.upload(t, bob, payload, &bobName))
	if !b.Deduped || b.Hash != hash {
		t.Fatalf("second upload of the same bytes: deduped %v hash %s", b.Deduped, b.Hash)
	}
	if b.OriginalBasename == nil || *b.OriginalBasename != "other.psd" {
		t.Fatalf("bob result: basename %v", b.OriginalBasename)
	}

	// Alice again, no header: unknown.
	c := decodeUpload(t, r.upload(t, alice, payload, nil))
	if c.OriginalBasename != nil || string(c.OriginalBasenameSource) != "unknown" {
		t.Fatalf("headerless result: basename %v source %q", c.OriginalBasename, c.OriginalBasenameSource)
	}

	ids := map[string]bool{a.UploadReceipt: true, b.UploadReceipt: true, c.UploadReceipt: true}
	if len(ids) != 3 {
		t.Fatalf("three uploads, %d distinct receipts: %v", len(ids), ids)
	}
	want := map[string]storedReceipt{
		a.UploadReceipt: {alice, hash, ptr("sketch one.psd"), "upload_request"},
		b.UploadReceipt: {bob, hash, ptr("other.psd"), "upload_request"},
		c.UploadReceipt: {alice, hash, nil, "unknown"},
	}
	for id, w := range want {
		got := r.receipt(t, id)
		if got.user != w.user || got.hash != w.hash || got.source != w.source || fmtPtr(got.basename) != fmtPtr(w.basename) {
			t.Errorf("receipt %s = %+v (basename %s), want %+v (basename %s)", id, got, fmtPtr(got.basename), w, fmtPtr(w.basename))
		}
	}

	// The shared object is still one row, and nothing about it names a file.
	var objects int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM storage_objects WHERE hash = $1`, hash).Scan(&objects); err != nil || objects != 1 {
		t.Fatalf("storage_objects rows for the hash = %d (%v), want 1", objects, err)
	}
	var nameCols int
	if err := r.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name IN ('storage_objects', 'storage_pins')
		  AND column_name ILIKE '%name%'`).Scan(&nameCols); err != nil || nameCols != 0 {
		t.Fatalf("storage_objects/storage_pins name-like columns = %d (%v), want 0", nameCols, err)
	}
}

func TestUpload_MalformedOriginalFilename_400BeforeStoring(t *testing.T) {
	r := newReceiptRig(t)
	const user int64 = 9_159_503
	for _, bad := range []string{
		"",
		".",
		"..",
		"dir/",
		"a%00b.jpg",
		"a%0Ab.jpg",
		"100%.jpg", // a literal % must be %25
		"a%FF.jpg",
		"photo%E2%80%AEgpj.exe",
		strings.Repeat("x", 256),
	} {
		t.Run(strconv.Quote(bad), func(t *testing.T) {
			payload, hash := randomPayload(t)
			r.cleanup(t, hash, user)
			name := bad
			rr := r.upload(t, user, payload, &name)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status %d body %s, want 400", rr.Code, rr.Body.String())
			}
			if bad != "" && strings.Contains(rr.Body.String(), bad) {
				t.Fatalf("400 body echoes the supplied value: %s", rr.Body.String())
			}
			var objects, receipts int
			_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM storage_objects WHERE hash = $1`, hash).Scan(&objects)
			_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM upload_receipts WHERE file_hash = $1`, hash).Scan(&receipts)
			if objects != 0 || receipts != 0 {
				t.Fatalf("a refused upload stored %d object(s) and %d receipt(s)", objects, receipts)
			}
		})
	}
}

func TestUploadReceipt_ExpiredSweptByLaterUpload(t *testing.T) {
	r := newReceiptRig(t)
	const user int64 = 9_159_504
	old, oldHash := randomPayload(t)
	r.cleanup(t, oldHash, user)
	rec := decodeUpload(t, r.upload(t, user, old, nil))
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `UPDATE upload_receipts SET created_at = now() - interval '3 days', expires_at = now() - interval '2 days' WHERE id = $1::uuid`, rec.UploadReceipt); err != nil {
		t.Fatal(err)
	}
	fresh, freshHash := randomPayload(t)
	r.cleanup(t, freshHash, user)
	decodeUpload(t, r.upload(t, user, fresh, nil))
	var n int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM upload_receipts WHERE id = $1::uuid`, rec.UploadReceipt).Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired receipt still present after a later upload (%d, %v)", n, err)
	}
}

func ptr[T any](v T) *T { return &v }

func fmtPtr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return strconv.Quote(*s)
}
