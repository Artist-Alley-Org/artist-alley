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
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

// End-to-end integration: real DB + fs backend, wired through the
// strict-handler chain just like the production server.
func TestUploadDownload_RoundTrip(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}

	pool := openPool(t, pwd)
	defer pool.Close()

	root := t.TempDir()
	backend, err := storagefs.New(root)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	svc.TempDir = t.TempDir()
	svc.GCGracePeriod = time.Second

	const userRef int64 = 424242
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := storage.NewHandler(svc, logger)

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: userRef, AuthMethod: "session"}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{PanicShim: &strictservershim.PanicShim{}, h: handler}, nil), router)

	payload := make([]byte, 12*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	want := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(want[:])

	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM storage_pins WHERE pin_subject_type='user' AND pin_subject_id=$1`, "424242")
		_, _ = pool.Exec(ctx, `DELETE FROM storage_variants WHERE object_hash=$1`, wantHex)
		_, _ = pool.Exec(ctx, `DELETE FROM storage_objects WHERE hash=$1`, wantHex)
	})

	// --- upload ---
	req := httptest.NewRequest(http.MethodPost, "/storage/objects", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var ur openapi.UploadResult
	if err := json.Unmarshal(rr.Body.Bytes(), &ur); err != nil {
		t.Fatalf("decode upload result: %v body=%s", err, rr.Body.String())
	}
	if ur.Hash != wantHex {
		t.Errorf("hash mismatch: got %s want %s", ur.Hash, wantHex)
	}
	if ur.Size != int64(len(payload)) {
		t.Errorf("size: got %d want %d", ur.Size, len(payload))
	}
	if ur.ContentType != "text/plain" {
		t.Errorf("content_type: got %q want text/plain", ur.ContentType)
	}
	if ur.Deduped {
		t.Errorf("first upload should not be deduped")
	}

	// --- second upload of same bytes: dedup hit ---
	req2 := httptest.NewRequest(http.MethodPost, "/storage/objects", bytes.NewReader(payload))
	req2.Header.Set("Content-Type", "application/octet-stream")
	req2.Header.Set("X-Content-Type", "text/plain")
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusCreated {
		t.Fatalf("second upload: status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	var ur2 openapi.UploadResult
	if err := json.Unmarshal(rr2.Body.Bytes(), &ur2); err != nil {
		t.Fatalf("decode second upload: %v", err)
	}
	if !ur2.Deduped {
		t.Errorf("second upload of same bytes should report deduped=true")
	}

	// --- full download ---
	get := httptest.NewRequest(http.MethodGet, "/storage/objects/"+wantHex, nil)
	gr := httptest.NewRecorder()
	router.ServeHTTP(gr, get)
	if gr.Code != http.StatusOK {
		t.Fatalf("download: status=%d body=%s", gr.Code, gr.Body.String())
	}
	if !bytes.Equal(gr.Body.Bytes(), payload) {
		t.Errorf("download body did not match upload (%d vs %d bytes)", gr.Body.Len(), len(payload))
	}

	// --- range download (middle 100 bytes) ---
	rng := httptest.NewRequest(http.MethodGet, "/storage/objects/"+wantHex, nil)
	rng.Header.Set("Range", "bytes=10-109")
	rnr := httptest.NewRecorder()
	router.ServeHTTP(rnr, rng)
	if rnr.Code != http.StatusPartialContent {
		t.Fatalf("range download: status=%d body=%s", rnr.Code, rnr.Body.String())
	}
	if rnr.Body.Len() != 100 {
		t.Errorf("range body len=%d want 100", rnr.Body.Len())
	}
	if !bytes.Equal(rnr.Body.Bytes(), payload[10:110]) {
		t.Errorf("range body content mismatch")
	}

	// --- 404 for missing hash ---
	missing := strings.Repeat("0", 64)
	mr := httptest.NewRecorder()
	router.ServeHTTP(mr, httptest.NewRequest(http.MethodGet, "/storage/objects/"+missing, nil))
	if mr.Code != http.StatusNotFound {
		t.Errorf("missing hash: status=%d want 404", mr.Code)
	}

	// --- unauthenticated upload ---
	bareRouter := chi.NewRouter()
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{PanicShim: &strictservershim.PanicShim{}, h: handler}, nil), bareRouter)
	ur3 := httptest.NewRecorder()
	bareRouter.ServeHTTP(ur3, httptest.NewRequest(http.MethodPost, "/storage/objects", bytes.NewReader([]byte("x"))))
	if ur3.Code != http.StatusUnauthorized {
		t.Errorf("anonymous upload: status=%d want 401", ur3.Code)
	}
}

// shimImpl routes only storage methods to the real handler;
// every other StrictServerInterface method panics via the
// embedded *strictservershim.PanicShim so a misrouted test
// surfaces loudly.
type shimImpl struct {
	*strictservershim.PanicShim
	h *storage.Handler
}

func (s shimImpl) UploadStorageObject(ctx context.Context, req openapi.UploadStorageObjectRequestObject) (openapi.UploadStorageObjectResponseObject, error) {
	return s.h.UploadStorageObject(ctx, req)
}
func (s shimImpl) DownloadStorageObject(ctx context.Context, req openapi.DownloadStorageObjectRequestObject) (openapi.DownloadStorageObjectResponseObject, error) {
	return s.h.DownloadStorageObject(ctx, req)
}
func (s shimImpl) DownloadStorageObjectVariant(ctx context.Context, req openapi.DownloadStorageObjectVariantRequestObject) (openapi.DownloadStorageObjectVariantResponseObject, error) {
	return s.h.DownloadStorageObjectVariant(ctx, req)
}



func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}



