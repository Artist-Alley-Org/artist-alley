// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.9.B — collections.Create's PRE-INSERT gates.
//
// Two of them now: the required-collection-field check (1.9.B) and the
// input-rule check #1173 added beside it. Both run before the create
// transaction opens, for the same reason — a refusal must not leave a
// half-created collection behind — so both are asserted against the
// DATABASE as well as against the status code. A 422 alone cannot tell
// a gate that ran early from one that ran late and rolled back.
//
// These tests inject a fake MetadataGate so collections can verify
// the create-time pre-insert validation + value seeding without
// pulling the metadata package into the test binary. The real
// metadata-backed gate is exercised end-to-end by the
// http_integration tests; the metadata half of #1173's gate is
// asserted against real field definitions in
// internal/metadata/input_rules_e2e_test.go.
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
	// #1173's pre-insert input-rule gate. `validateFn` stands in for a
	// field definition's regexp_filter; `validated` records exactly what
	// the handler passed, which is how the ORDER assertion is made.
	validateFn func(values []collections.SeedFieldValue) *collections.SeedFieldRefusal
	validated  [][]collections.SeedFieldValue
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

func (g *fakeMetadataGate) ValidateSeedFieldValues(
	ctx context.Context, values []collections.SeedFieldValue,
) (*collections.SeedFieldRefusal, error) {
	g.validated = append(g.validated, values)
	if g.validateFn == nil {
		return nil, nil
	}
	return g.validateFn(values), nil
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

// ---------------------------------------------------------------------------
// The input-rule gate (#1173)
// ---------------------------------------------------------------------------

// A seeded value the field definition refuses stops the create BEFORE
// the transaction opens.
//
// The pre-insert placement is the whole assertion, and it is asserted
// against the DATABASE rather than against the status code: refusing
// from inside SeedCollectionFieldValueInTx would already have written
// the collection row, so a 422 alone cannot tell a gate that ran early
// from one that ran late and rolled back. A rollback is not the same
// promise — it depends on the seed running in the caller's tx, which is
// a property of the seeding path rather than of the refusal.
func TestCreateCollection_SeededValueRefused_422_BeforeInsert(t *testing.T) {
	pwd := readEnvPwd(t)
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestCollections(t, pool)
	t.Cleanup(func() { cleanTestCollections(t, pool) })

	fieldID := uuid.New()
	gate := &fakeMetadataGate{
		validateFn: func([]collections.SeedFieldValue) *collections.SeedFieldRefusal {
			return &collections.SeedFieldRefusal{
				Code:    "shot_code",
				Label:   "Shot code",
				Message: "shot_code must match the pattern [A-Z]{3}_[0-9]{4}",
			}
		},
	}
	router, _ := makeRouterWithGate(t, pool, 420005, true, gate)

	rr := postJSON(t, router, "/collections", map[string]any{
		"name": "ct_pattern_refused",
		"field_values": []map[string]any{
			{"field_id": fieldID.String(), "value_text": "nope"},
		},
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422 body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	mustDecode(t, rr.Body.Bytes(), &body)
	if body["reason"] != "collection_field_pattern_mismatch" {
		t.Errorf("reason=%v want collection_field_pattern_mismatch", body["reason"])
	}
	if body["field_code"] != "shot_code" {
		t.Errorf("field_code=%v want shot_code", body["field_code"])
	}

	if len(gate.seeds) != 0 {
		t.Errorf("gate.seeds = %d, want 0 — a refused body must reach no writer", len(gate.seeds))
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM collections WHERE name = 'ct_pattern_refused'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("collection row count = %d, want 0 — the gate must run before the INSERT", n)
	}
}

// The gate sees every supplied value, in BODY order.
//
// Order is load-bearing rather than cosmetic: the handler holds these
// values in a map for the required-field check, and validating from
// that map would make WHICH refusal an operator sees depend on Go's map
// iteration. Two identical requests would then disagree about which
// field is wrong.
func TestCreateCollection_SeedGateSeesEveryValueInBodyOrder(t *testing.T) {
	pwd := readEnvPwd(t)
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestCollections(t, pool)
	t.Cleanup(func() { cleanTestCollections(t, pool) })

	first, second, third := uuid.New(), uuid.New(), uuid.New()
	gate := &fakeMetadataGate{}
	router, _ := makeRouterWithGate(t, pool, 420006, true, gate)

	rr := postJSON(t, router, "/collections", map[string]any{
		"name": "ct_seed_order",
		"field_values": []map[string]any{
			{"field_id": first.String(), "value_text": "one"},
			{"field_id": second.String(), "value_text": "two"},
			{"field_id": third.String(), "value_text": "three"},
		},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d want 201 body=%s", rr.Code, rr.Body.String())
	}
	if len(gate.validated) != 1 {
		t.Fatalf("the gate ran %d times, want exactly 1", len(gate.validated))
	}
	got := gate.validated[0]
	want := []uuid.UUID{first, second, third}
	if len(got) != len(want) {
		t.Fatalf("the gate saw %d values, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].FieldID != want[i] {
			t.Errorf("value %d = %s, want %s (body order)", i, got[i].FieldID, want[i])
		}
	}
}

// "You left one out" comes before "the one you sent is wrong". Both
// gates are pre-insert, so the ordering is a choice rather than an
// accident, and it is the order an operator can act on: a missing
// required field is the larger problem and mentioning the pattern first
// would send them to fix the smaller one.
func TestCreateCollection_MissingRequiredBeatsPatternRefusal(t *testing.T) {
	pwd := readEnvPwd(t)
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	cleanTestCollections(t, pool)
	t.Cleanup(func() { cleanTestCollections(t, pool) })

	gate := &fakeMetadataGate{
		required: []collections.RequiredField{
			{ID: uuid.New(), Code: "client_name", Label: "Client", Type: "text"},
		},
		validateFn: func([]collections.SeedFieldValue) *collections.SeedFieldRefusal {
			return &collections.SeedFieldRefusal{Code: "other", Label: "Other", Message: "pattern"}
		},
	}
	router, _ := makeRouterWithGate(t, pool, 420007, true, gate)

	rr := postJSON(t, router, "/collections", map[string]any{"name": "ct_gate_order"})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want 422 body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	mustDecode(t, rr.Body.Bytes(), &body)
	if body["reason"] != "required_collection_field_missing" {
		t.Errorf("reason=%v want required_collection_field_missing", body["reason"])
	}
	if len(gate.validated) != 0 {
		t.Errorf("the input-rule gate ran %d times; the required check refuses first", len(gate.validated))
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
