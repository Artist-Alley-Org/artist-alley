package assettype

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

// TestListAssetTypes_Live exercises the handler at the
// StrictServerInterface level: feed in the generated request type,
// inspect the generated response type, no HTTP marshalling.
func TestListAssetTypes_Live(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := openPool(t, pwd)
	defer pool.Close()
	ensureAssetTypeSeed(t, ctx, pool)

	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	resp, err := h.ListAssetTypes(ctx, openapi.ListAssetTypesRequestObject{})
	if err != nil {
		t.Fatalf("ListAssetTypes: %v", err)
	}

	ok, isOk := resp.(openapi.ListAssetTypes200JSONResponse)
	if !isOk {
		t.Fatalf("expected ListAssetTypes200JSONResponse, got %T", resp)
	}
	if len(ok) < 4 {
		t.Fatalf("expected at least 4 seeded rows, got %d", len(ok))
	}

	want := map[int64]string{1: "Image", 2: "Document", 3: "Video", 4: "Audio"}
	for ref, name := range want {
		found := false
		for _, rt := range ok {
			if rt.Ref == ref {
				if rt.Name == nil || *rt.Name != name {
					got := "<nil>"
					if rt.Name != nil {
						got = *rt.Name
					}
					t.Errorf("ref=%d: expected name=%q, got %q", ref, name, got)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing seeded ref=%d (%s)", ref, name)
		}
	}
}

// TestListAssetTypes_HTTP exercises the handler at the HTTP layer:
// mount it on a chi router exactly the way the real server does, fire
// a request, and verify the wire bytes match the OpenAPI contract.
func TestListAssetTypes_HTTP(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := openPool(t, pwd)
	defer pool.Close()
	ensureAssetTypeSeed(t, ctx, pool)

	// The StrictServerInterface now spans every endpoint group; we
	// pad the missing slices with the codegen-supplied Unimplemented
	// (which returns 501 for unsupported methods at the strict-server
	// layer). For the route under test that's a non-issue because
	// only ListAssetTypes is exercised.
	impl := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	strict := openapi.NewStrictHandler(rtOnly{PanicShim: &strictservershim.PanicShim{}, h: impl}, nil)

	router := chi.NewRouter()
	openapi.HandlerFromMux(strict, router)

	req := httptest.NewRequest(http.MethodGet, "/asset_types", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type=%q", ct)
	}

	var rows []openapi.AssetType
	if err := json.NewDecoder(rr.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) < 4 {
		t.Fatalf("expected at least 4 rows, got %d", len(rows))
	}
}

// rtOnly is a StrictServerInterface implementation that forwards
// ListAssetTypes to the real handler and panics for every other
// method, so a wrong-test-route bug surfaces immediately instead of
// silently returning a "not implemented" response.
// rtOnly is a StrictServerInterface shim: every method panics
// via the embedded *PanicShim except the ones overridden below
// (just ListAssetTypes here). Lets this test exercise the
// assettype handler without needing to stub every other openapi
// method (PanicShim covers them).
type rtOnly struct {
	*strictservershim.PanicShim
	h *Handler
}

func (r rtOnly) ListAssetTypes(ctx context.Context, req openapi.ListAssetTypesRequestObject) (openapi.ListAssetTypesResponseObject, error) {
	return r.h.ListAssetTypes(ctx, req)
}

func (r rtOnly) ListAssetTypeAcls(ctx context.Context, req openapi.ListAssetTypeAclsRequestObject) (openapi.ListAssetTypeAclsResponseObject, error) {
	return r.h.ListAssetTypeAcls(ctx, req)
}
func (r rtOnly) AddAssetTypeAcl(ctx context.Context, req openapi.AddAssetTypeAclRequestObject) (openapi.AddAssetTypeAclResponseObject, error) {
	return r.h.AddAssetTypeAcl(ctx, req)
}
func (r rtOnly) RemoveAssetTypeAcl(ctx context.Context, req openapi.RemoveAssetTypeAclRequestObject) (openapi.RemoveAssetTypeAclResponseObject, error) {
	return r.h.RemoveAssetTypeAcl(ctx, req)
}

// --- test helpers -----------------------------------------------------------

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

// ensureAssetTypeSeed makes sure the four canonical RS asset
// types exist. Defensive — the table is normally created and seeded by
// CheckDBStruct on the PHP side during install.
func ensureAssetTypeSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	const ensure = `
INSERT INTO asset_types (ref, name, icon)
VALUES (1, 'Image', 'image'),
       (2, 'Document', 'file-text'),
       (3, 'Video', 'video'),
       (4, 'Audio', 'music')
ON CONFLICT (ref) DO NOTHING
`
	if _, err := pool.Exec(ctx, ensure); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}



