package assets

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
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

// ---------------------------------------------------------------------------
// parseSingleRange — pure function, plain table-driven test
// ---------------------------------------------------------------------------

func TestParseSingleRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in          string
		wantOffset  int64
		wantLength  int64
		wantOK      bool
	}{
		{"", 0, 0, false},
		{"items=0-99", 0, 0, false}, // wrong unit
		{"bytes=", 0, 0, false},
		{"bytes=0-99", 0, 100, true},
		{"bytes=100-199", 100, 100, true},
		{"bytes=0-0", 0, 1, true},
		{"bytes=500-", 500, 0, true}, // to EOF
		{"bytes=-500", 0, 0, false},  // suffix range, not supported
		{"bytes=0-99,200-299", 0, 0, false}, // multi-range
		{"bytes=99-0", 0, 0, false},          // inverted
		{"bytes=abc-99", 0, 0, false},
		{"bytes=0-abc", 0, 0, false},
		{"bytes=-1", 0, 0, false},
	}
	for _, tc := range cases {
		off, length, ok := parseSingleRange(tc.in)
		if ok != tc.wantOK || off != tc.wantOffset || length != tc.wantLength {
			t.Errorf("parseSingleRange(%q) = (%d, %d, %v); want (%d, %d, %v)",
				tc.in, off, length, ok, tc.wantOffset, tc.wantLength, tc.wantOK)
		}
	}
}

// ---------------------------------------------------------------------------
// End-to-end integration: real DB + fs backend, wired through the
// strict-handler chain just like the production server.
// ---------------------------------------------------------------------------

func TestUploadDownload_RoundTrip(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}

	pool := openPool(t, pwd)
	defer pool.Close()

	// fs backend rooted in a temp dir.
	root := t.TempDir()
	backend, err := storagefs.New(root)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	svc := storage.NewService(backend, pool)
	svc.TempDir = t.TempDir()
	svc.GCGracePeriod = time.Second // small grace so we can assert orphan scheduling

	// Synthesise an identity by short-circuiting the resolver: we
	// install our own middleware that injects an identity for every
	// request. Real auth resolution is covered by the auth package
	// tests; here we're testing the assets handler in isolation.
	const userRef int64 = 424242
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(svc, logger)

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: userRef, AuthMethod: "session"}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{handler}, nil), router)

	// Random payload so reruns don't dedup against the previous run.
	payload := make([]byte, 12*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	want := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(want[:])

	t.Cleanup(func() {
		cleanCtx := context.Background()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM storage_pins WHERE pin_subject_type='user' AND pin_subject_id=$1`,
			"424242")
		_, _ = pool.Exec(cleanCtx, `DELETE FROM storage_variants WHERE object_hash=$1`, wantHex)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM storage_objects WHERE hash=$1`, wantHex)
	})

	// ---- upload ----

	req := httptest.NewRequest(http.MethodPost, "/assets", bytes.NewReader(payload))
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

	// ---- second upload of same bytes: dedup hit ----
	req2 := httptest.NewRequest(http.MethodPost, "/assets", bytes.NewReader(payload))
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

	// ---- full download ----
	get := httptest.NewRequest(http.MethodGet, "/assets/"+wantHex, nil)
	gr := httptest.NewRecorder()
	router.ServeHTTP(gr, get)
	if gr.Code != http.StatusOK {
		t.Fatalf("download: status=%d body=%s", gr.Code, gr.Body.String())
	}
	if !bytes.Equal(gr.Body.Bytes(), payload) {
		t.Errorf("download body did not match upload (%d vs %d bytes)", gr.Body.Len(), len(payload))
	}

	// ---- range download (middle 100 bytes) ----
	rng := httptest.NewRequest(http.MethodGet, "/assets/"+wantHex, nil)
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

	// ---- 404 for missing hash ----
	missing := strings.Repeat("0", 64)
	mr := httptest.NewRecorder()
	router.ServeHTTP(mr, httptest.NewRequest(http.MethodGet, "/assets/"+missing, nil))
	if mr.Code != http.StatusNotFound {
		t.Errorf("missing hash: status=%d want 404", mr.Code)
	}

	// ---- unauthenticated upload ----
	bareRouter := chi.NewRouter()
	openapi.HandlerFromMux(openapi.NewStrictHandler(shimImpl{handler}, nil), bareRouter)
	ur3 := httptest.NewRecorder()
	bareRouter.ServeHTTP(ur3, httptest.NewRequest(http.MethodPost, "/assets",
		bytes.NewReader([]byte("x"))))
	if ur3.Code != http.StatusUnauthorized {
		t.Errorf("anonymous upload: status=%d want 401", ur3.Code)
	}
}

// ---------------------------------------------------------------------------
// Test plumbing
// ---------------------------------------------------------------------------

// shimImpl satisfies the full StrictServerInterface but only routes
// the asset methods to the real handler. Everything else panics, so a
// mis-routed test surfaces loudly rather than silently passing.
type shimImpl struct{ h *Handler }

func (s shimImpl) UploadAsset(ctx context.Context, req openapi.UploadAssetRequestObject) (openapi.UploadAssetResponseObject, error) {
	return s.h.UploadAsset(ctx, req)
}
func (s shimImpl) DownloadAssetOriginal(ctx context.Context, req openapi.DownloadAssetOriginalRequestObject) (openapi.DownloadAssetOriginalResponseObject, error) {
	return s.h.DownloadAssetOriginal(ctx, req)
}
func (s shimImpl) DownloadAssetVariant(ctx context.Context, req openapi.DownloadAssetVariantRequestObject) (openapi.DownloadAssetVariantResponseObject, error) {
	return s.h.DownloadAssetVariant(ctx, req)
}

func (shimImpl) Login(context.Context, openapi.LoginRequestObject) (openapi.LoginResponseObject, error) {
	panic("Login called from assets test shim")
}
func (shimImpl) Logout(context.Context, openapi.LogoutRequestObject) (openapi.LogoutResponseObject, error) {
	panic("Logout called from assets test shim")
}
func (shimImpl) GetCurrentUser(context.Context, openapi.GetCurrentUserRequestObject) (openapi.GetCurrentUserResponseObject, error) {
	panic("GetCurrentUser called from assets test shim")
}
func (shimImpl) ListApiTokens(context.Context, openapi.ListApiTokensRequestObject) (openapi.ListApiTokensResponseObject, error) {
	panic("ListApiTokens called from assets test shim")
}
func (shimImpl) CreateApiToken(context.Context, openapi.CreateApiTokenRequestObject) (openapi.CreateApiTokenResponseObject, error) {
	panic("CreateApiToken called from assets test shim")
}
func (shimImpl) RevokeApiToken(context.Context, openapi.RevokeApiTokenRequestObject) (openapi.RevokeApiTokenResponseObject, error) {
	panic("RevokeApiToken called from assets test shim")
}
func (shimImpl) ListCapabilities(context.Context, openapi.ListCapabilitiesRequestObject) (openapi.ListCapabilitiesResponseObject, error) {
	panic("ListCapabilities called from assets test shim")
}
func (shimImpl) ListRoles(context.Context, openapi.ListRolesRequestObject) (openapi.ListRolesResponseObject, error) {
	panic("ListRoles called from assets test shim")
}
func (shimImpl) GetMyCapabilities(context.Context, openapi.GetMyCapabilitiesRequestObject) (openapi.GetMyCapabilitiesResponseObject, error) {
	panic("GetMyCapabilities called from assets test shim")
}
func (shimImpl) SetUserRole(context.Context, openapi.SetUserRoleRequestObject) (openapi.SetUserRoleResponseObject, error) {
	panic("SetUserRole called from assets test shim")
}
func (shimImpl) ListResourceTypes(context.Context, openapi.ListResourceTypesRequestObject) (openapi.ListResourceTypesResponseObject, error) {
	panic("ListResourceTypes called from assets test shim")
}
func (shimImpl) GetSetupStatus(context.Context, openapi.GetSetupStatusRequestObject) (openapi.GetSetupStatusResponseObject, error) {
	panic("GetSetupStatus called from assets test shim")
}
func (shimImpl) CompleteSetup(context.Context, openapi.CompleteSetupRequestObject) (openapi.CompleteSetupResponseObject, error) {
	panic("CompleteSetup called from assets test shim")
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
