// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.9.B — collections.Create required-collection-field gate.
//
// These tests inject a fake MetadataGate so collections can verify
// the create-time pre-insert validation + value seeding without
// pulling the metadata package into the test binary. The real
// metadata-backed gate is exercised end-to-end by the
// http_integration tests.
package collections_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

// fakeMetadataGate is a minimal MetadataGate implementation that
// returns canned required-field rows + records every seeded value.
type fakeMetadataGate struct {
	required []collections.RequiredField
	seeds    []seedCall
	upsertFn func(ctx context.Context, tx pgx.Tx, cID, fID uuid.UUID, raw collections.CollectionFieldValueInput, callerRef int64) error
}

type seedCall struct {
	collectionID uuid.UUID
	fieldID      uuid.UUID
	value        collections.CollectionFieldValueInput
	callerRef    int64
}

func (g *fakeMetadataGate) RequiredCollectionFields(ctx context.Context) ([]collections.RequiredField, error) {
	return g.required, nil
}

func (g *fakeMetadataGate) UpsertCollectionFieldValueInTx(
	ctx context.Context, tx pgx.Tx,
	cID, fID uuid.UUID,
	raw collections.CollectionFieldValueInput,
	callerRef int64,
) error {
	g.seeds = append(g.seeds, seedCall{cID, fID, raw, callerRef})
	if g.upsertFn != nil {
		return g.upsertFn(ctx, tx, cID, fID, raw, callerRef)
	}
	return nil
}

// TestCreateCollection_MissingRequiredField_422 covers the gate
// firing — required collection field exists, create body omits it,
// handler returns 422 with the structured reason + field code.
func TestCreateCollection_MissingRequiredField_422(t *testing.T) {
	pwd := readEnvPwd(t)
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestCollections(t, pool)
	t.Cleanup(func() { cleanTestCollections(t, pool) })

	router, _ := makeRouterWithGate(t, pool, 420001, true, &fakeMetadataGate{
		required: []collections.RequiredField{
			{ID: uuid.New(), Code: "client_name", Label: "Client", Type: "text"},
		},
	})

	rr := postJSON(t, router, "/collections", map[string]any{"name": "ct_missing_field"})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422 body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	mustDecode(t, rr.Body.Bytes(), &body)
	if body["reason"] != "required_collection_field_missing" {
		t.Errorf("reason=%v want required_collection_field_missing", body["reason"])
	}
	if body["field_code"] != "client_name" {
		t.Errorf("field_code=%v want client_name", body["field_code"])
	}
}

// TestCreateCollection_AllRequiredFieldsProvided_Seeds — same field
// is now present in the body, create succeeds, gate records the
// seed call inside the create tx.
func TestCreateCollection_AllRequiredFieldsProvided_Seeds(t *testing.T) {
	pwd := readEnvPwd(t)
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestCollections(t, pool)
	t.Cleanup(func() { cleanTestCollections(t, pool) })

	fieldID := uuid.New()
	gate := &fakeMetadataGate{
		required: []collections.RequiredField{
			{ID: fieldID, Code: "client_name", Label: "Client", Type: "text"},
		},
	}
	router, _ := makeRouterWithGate(t, pool, 420002, true, gate)

	rr := postJSON(t, router, "/collections", map[string]any{
		"name": "ct_with_field",
		"field_values": []map[string]any{
			{"field_id": fieldID.String(), "value_text": "Acme"},
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d want 201 body=%s", rr.Code, rr.Body.String())
	}
	if len(gate.seeds) != 1 {
		t.Fatalf("gate.seeds = %d, want 1", len(gate.seeds))
	}
	if gate.seeds[0].fieldID != fieldID {
		t.Errorf("seed fieldID = %s, want %s", gate.seeds[0].fieldID, fieldID)
	}
	if gate.seeds[0].value.ValueText == nil || *gate.seeds[0].value.ValueText != "Acme" {
		t.Errorf("seed value_text = %v, want \"Acme\"", gate.seeds[0].value.ValueText)
	}
}

// TestCreateCollection_NoRequiredFields_Succeeds — no required
// fields configured, create proceeds without any values.
func TestCreateCollection_NoRequiredFields_Succeeds(t *testing.T) {
	pwd := readEnvPwd(t)
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestCollections(t, pool)
	t.Cleanup(func() { cleanTestCollections(t, pool) })

	gate := &fakeMetadataGate{} // empty required list
	router, _ := makeRouterWithGate(t, pool, 420003, true, gate)

	rr := postJSON(t, router, "/collections", map[string]any{"name": "ct_no_required"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d want 201 body=%s", rr.Code, rr.Body.String())
	}
	if len(gate.seeds) != 0 {
		t.Errorf("gate.seeds = %d, want 0", len(gate.seeds))
	}
}

// TestCreateCollection_UpsertFails_RollsBack — the gate's upsert
// returns an error; the whole create tx aborts and the collection
// row should NOT exist after.
func TestCreateCollection_UpsertFails_RollsBack(t *testing.T) {
	pwd := readEnvPwd(t)
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestCollections(t, pool)
	t.Cleanup(func() { cleanTestCollections(t, pool) })

	fieldID := uuid.New()
	gate := &fakeMetadataGate{
		required: []collections.RequiredField{
			{ID: fieldID, Code: "client_name", Label: "Client", Type: "text"},
		},
		upsertFn: func(_ context.Context, _ pgx.Tx, _, _ uuid.UUID, _ collections.CollectionFieldValueInput, _ int64) error {
			return errSentinel
		},
	}
	router, _ := makeRouterWithGate(t, pool, 420004, true, gate)

	rr := postJSON(t, router, "/collections", map[string]any{
		"name": "ct_rollback_test",
		"field_values": []map[string]any{
			{"field_id": fieldID.String(), "value_text": "Acme"},
		},
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500 body=%s", rr.Code, rr.Body.String())
	}
	// Verify the row didn't land — the activities.WithEmissionFn tx
	// should have rolled back when the seed failed.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM collections WHERE name = 'ct_rollback_test'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("collection row count = %d, want 0 (tx should have rolled back)", n)
	}
}

var errSentinel = &sentinelErr{}

type sentinelErr struct{}

func (*sentinelErr) Error() string { return "test-only seed failure" }

func readEnvPwd(t *testing.T) string {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	return pwd
}

// makeRouterWithGate is a variant of makeRouter that injects a
// MetadataGate so the required-field path can be exercised without
// dragging in the metadata package's full handler.
func makeRouterWithGate(
	t *testing.T,
	pool *pgxpool.Pool,
	userRef int64,
	admin bool,
	gate collections.MetadataGate,
) (chi.Router, int64) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := collections.NewHandler(pool, logger, nil)
	actWriter := activities.NewWriter(pool, logger, nil)
	h.SetActivitiesWriter(actWriter, func(ctx context.Context) string { return "https://test.example" })
	h.SetMetadataGate(gate)

	caps := []string{}
	if admin {
		caps = []string{collections.CapCollectionsAdmin}
	}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{
				UserRef:      userRef,
				AuthMethod:   "session",
				Capabilities: caps,
			}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(collShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router, userRef
}
