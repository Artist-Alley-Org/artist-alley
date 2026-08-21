// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #822 — a field definition that declares `mirrors_column` is a VIEW onto
// that column of `assets`, and the two cannot disagree.
//
// # What these tests are for, and what they deliberately are NOT
//
// "No divergent values exist today" was the STARTING condition, not the
// invariant. `title` and `description` had been active field definitions on
// every install with zero `asset_field_value` rows between them — latent, one
// write away from real, and a test that merely asserted the count stayed zero
// would have passed on every build that reintroduced the bug.
//
// So every test here ATTEMPTS the divergence through a path that production
// actually runs, and asserts it cannot land:
//
//   - write through the field API, then read the COLUMN in raw SQL;
//   - write the COLUMN in raw SQL, then read through the field API;
//   - run the INSERT the seed loader and the sqlc upsert issue verbatim, and
//     require the database to refuse it;
//   - declare a mirror over a field that already holds values, and require
//     the database to refuse that too.
//
// Assertions are on PERSISTED state — a `SELECT title FROM assets` on a
// separate connection — never on the handler's own response body. A body
// assertion passes on a build where the handler echoes what it was sent and
// wrote nothing.
//
// Every negative control is proved CONSTRUCTIBLE first: each refusal test
// runs the identical statement against a NON-mirrored field and requires it
// to succeed, so a test that could never fire cannot masquerade as a passing
// guard.
package metadata_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	mirrorOwnerRef    = int64(720100)
	mirrorStrangerRef = int64(720101)
)

// ---------------------------------------------------------------------------
// The two directions of the same fact
// ---------------------------------------------------------------------------

// TestMirroredField_FieldWriteLandsInTheColumn drives the production write
// path — PUT /assets/{id}/fields/{field_id} — and then reads `assets.title`
// directly. If the field plane had storage of its own this passes while the
// column keeps its old value, which is exactly the divergence.
func TestMirroredField_FieldWriteLandsInTheColumn(t *testing.T) {
	pool := mirrorPool(t)
	t.Cleanup(pool.Close)

	assetID := mirrorAsset(t, pool, mirrorOwnerRef, "before the field write")
	titleID := mirrorFieldID(t, pool, "title")
	router := mirrorRouter(t, pool, mirrorOwnerRef, nil)

	rr := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, titleID), map[string]any{
		"value_text": "written through the field plane",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT mirrored field: status=%d body=%s", rr.Code, rr.Body.String())
	}

	if got := mirrorColumnValue(t, pool, assetID, "title"); got != "written through the field plane" {
		t.Errorf("assets.title = %q, want the value the field write sent — the field plane wrote somewhere else", got)
	}
	if n := mirrorStoredRowCount(t, pool, assetID, titleID); n != 0 {
		t.Errorf("asset_field_value holds %d row(s) for a mirrored field, want 0 — that row IS the divergent copy", n)
	}
	if n := mirrorHistoryRowCount(t, pool, assetID, titleID); n != 0 {
		t.Errorf("asset_field_value_history holds %d row(s) for a mirrored field, want 0", n)
	}
}

// TestMirroredField_ColumnWriteReadsBackThroughTheFieldPath is the reverse.
// The asset plane changes the column; the field plane must report the new
// value, not a stale copy and not nothing at all.
func TestMirroredField_ColumnWriteReadsBackThroughTheFieldPath(t *testing.T) {
	pool := mirrorPool(t)
	t.Cleanup(pool.Close)

	assetID := mirrorAsset(t, pool, mirrorOwnerRef, "the original title")
	titleID := mirrorFieldID(t, pool, "title")
	router := mirrorRouter(t, pool, mirrorOwnerRef, nil)

	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET title = $2 WHERE id = $1`, assetID, "changed on the asset plane"); err != nil {
		t.Fatalf("column write: %v", err)
	}

	values := mirrorGetFields(t, router, assetID)
	got, ok := values[titleID]
	if !ok {
		t.Fatalf("GET /assets/{id}/fields returned no entry for the mirrored field; a client that wrote it would read nothing back")
	}
	if got.ValueText == nil || *got.ValueText != "changed on the asset plane" {
		t.Errorf("field plane reports %v, want the column's current value", got.ValueText)
	}
	if string(got.SetBy) != metadata.SetByMirror {
		t.Errorf("set_by = %q, want %q — the value came from the column, and saying otherwise claims an author nothing recorded",
			got.SetBy, metadata.SetByMirror)
	}
	if got.SetByUserRef != nil {
		t.Errorf("set_by_user_ref = %v, want nil: the column records no author", *got.SetByUserRef)
	}

	// An EMPTY column is "unset", the same as a field nobody has written.
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET description = '' WHERE id = $1`, assetID); err != nil {
		t.Fatalf("clear description: %v", err)
	}
	descID := mirrorFieldID(t, pool, "description")
	if _, present := mirrorGetFields(t, router, assetID)[descID]; present {
		t.Error("an empty mirrored column produced a field entry; every asset in the catalogue would carry a blank one")
	}
}

// ---------------------------------------------------------------------------
// The divergence is refused by the DATABASE, not by a rule in Go
// ---------------------------------------------------------------------------

// TestMirroredField_StoredCopyIsRefusedAtTheDatabase runs the exact INSERT
// the seed loader (SeedInsertAssetFieldValue) and the sqlc upsert issue. A Go
// path nobody has taught about mirroring fails LOUDLY here rather than quietly
// writing a second copy — which is the whole reason the guard is a trigger and
// not a branch in the handler.
//
// The negative control is proved constructible first: the identical statement
// against an ordinary field must succeed, so this cannot be a test that never
// fires.
func TestMirroredField_StoredCopyIsRefusedAtTheDatabase(t *testing.T) {
	pool := mirrorPool(t)
	t.Cleanup(pool.Close)

	assetID := mirrorAsset(t, pool, mirrorOwnerRef, "guarded")
	titleID := mirrorFieldID(t, pool, "title")
	ordinaryID := mirrorOrdinaryField(t, pool)

	insert := `INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by)
	           VALUES ($1, $2, $3, 'import')`

	// CONSTRUCTIBILITY: the same statement, an ordinary field, must land.
	if _, err := pool.Exec(context.Background(), insert, assetID, ordinaryID, "ordinary value"); err != nil {
		t.Fatalf("control insert on a NON-mirrored field failed (%v) — the refusal below would prove nothing", err)
	}

	if _, err := pool.Exec(context.Background(), insert, assetID, titleID, "DIVERGENT"); err == nil {
		t.Fatal("the database accepted a stored value for a mirrored field; divergence is possible again")
	}

	// And the history table, for the same reason: a per-field trail that
	// exists only for changes made through the field plane lies by omission.
	histInsert := `INSERT INTO asset_field_value_history (asset_id, field_id, new_value, set_by)
	               VALUES ($1, $2, '{"type":"text","value":"x"}'::jsonb, 'import')`
	if _, err := pool.Exec(context.Background(), histInsert, assetID, ordinaryID); err != nil {
		t.Fatalf("control history insert on a NON-mirrored field failed: %v", err)
	}
	if _, err := pool.Exec(context.Background(), histInsert, assetID, titleID); err == nil {
		t.Fatal("the database accepted a history row for a mirrored field")
	}
}

// TestMirroredField_DeclaringAMirrorOverExistingValuesIsRefused closes the
// door from the other side. A field that already carries values cannot become
// a view onto a column, because that would MANUFACTURE the divergence the
// other guard prevents.
func TestMirroredField_DeclaringAMirrorOverExistingValuesIsRefused(t *testing.T) {
	pool := mirrorPool(t)
	t.Cleanup(pool.Close)

	assetID := mirrorAsset(t, pool, mirrorOwnerRef, "declaring")
	ordinaryID := mirrorOrdinaryField(t, pool)

	// CONSTRUCTIBILITY: with no values on it, the declaration is legal —
	// so the refusal below is about the values and not about the column.
	if _, err := pool.Exec(context.Background(),
		`UPDATE field_definition SET mirrors_column = 'title' WHERE id = $1`, ordinaryID); err != nil {
		t.Fatalf("control: declaring a mirror on an EMPTY field failed (%v); the refusal below would prove nothing", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE field_definition SET mirrors_column = NULL WHERE id = $1`, ordinaryID); err != nil {
		t.Fatalf("undo control declaration: %v", err)
	}

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by) VALUES ($1, $2, 'held', 'import')`,
		assetID, ordinaryID); err != nil {
		t.Fatalf("seed a value on the ordinary field: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE field_definition SET mirrors_column = 'title' WHERE id = $1`, ordinaryID); err == nil {
		t.Fatal("a field holding stored values was allowed to become a mirror; both copies would now exist")
	}
}

// TestMirroredField_OnlyDeclaredColumnsAreMirrorable pins the CHECK
// constraint that is the single enumeration of what may be mirrored. Nothing
// in Go carries that list, so if this constraint stops enforcing it, an
// operator could point a field at a column that does not exist.
func TestMirroredField_OnlyDeclaredColumnsAreMirrorable(t *testing.T) {
	pool := mirrorPool(t)
	t.Cleanup(pool.Close)

	ordinaryID := mirrorOrdinaryField(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE field_definition SET mirrors_column = 'file_hash' WHERE id = $1`, ordinaryID); err == nil {
		t.Fatal("a field was allowed to mirror a column outside the declared set")
	}
}

// ---------------------------------------------------------------------------
// The permission answer (#822 scope item 3)
// ---------------------------------------------------------------------------

// TestMirroredField_WriteIsGatedByTheColumnsRule is the escalation this arc
// nearly shipped. The field plane admits any authenticated caller; the column
// plane demands owner / team-scoped `assets.admin` / the global grant. A
// mirrored write carries a COLUMN as its payload, so the column's rule binds
// — otherwise declaring `title` a mirror would have handed every signed-in
// account the power to retitle every asset on the instance.
func TestMirroredField_WriteIsGatedByTheColumnsRule(t *testing.T) {
	pool := mirrorPool(t)
	t.Cleanup(pool.Close)

	assetID := mirrorAsset(t, pool, mirrorOwnerRef, "owned by someone else")
	titleID := mirrorFieldID(t, pool, "title")
	ordinaryID := mirrorOrdinaryField(t, pool)
	stranger := mirrorRouter(t, pool, mirrorStrangerRef, nil)

	// CONSTRUCTIBILITY: the stranger's session is real and the field plane
	// admits it. If this failed, the 403 below would be about the session
	// rather than about the mirror.
	ctrl := putJSON(t, stranger, fmt.Sprintf("/assets/%s/fields/%s", assetID, ordinaryID), map[string]any{
		"value_text": "a stranger may write an ordinary field",
	})
	if ctrl.Code != http.StatusOK {
		t.Fatalf("control write on a NON-mirrored field: status=%d body=%s — the refusal below would prove nothing",
			ctrl.Code, ctrl.Body.String())
	}

	rr := putJSON(t, stranger, fmt.Sprintf("/assets/%s/fields/%s", assetID, titleID), map[string]any{
		"value_text": "a stranger retitled your asset",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("stranger writing a mirrored field: status=%d want 403 body=%s", rr.Code, rr.Body.String())
	}
	// The refusal must change nothing. A gate that answers 403 after writing
	// is a gate that does not exist, and a status-only assertion misses it.
	if got := mirrorColumnValue(t, pool, assetID, "title"); got != "owned by someone else" {
		t.Errorf("assets.title = %q after a refused write; the gate ran too late", got)
	}

	// A holder of the GLOBAL assets.admin passes, so the gate is the
	// column's rule and not a blanket "only the owner".
	admin := mirrorRouter(t, pool, mirrorStrangerRef, []string{visibility.AssetsAdmin})
	ok := putJSON(t, admin, fmt.Sprintf("/assets/%s/fields/%s", assetID, titleID), map[string]any{
		"value_text": "an assets.admin may retitle it",
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("assets.admin writing a mirrored field: status=%d body=%s", ok.Code, ok.Body.String())
	}
	if got := mirrorColumnValue(t, pool, assetID, "title"); got != "an assets.admin may retitle it" {
		t.Errorf("assets.title = %q, want the admin's value", got)
	}
}

// TestMirroredField_RequiredCannotBeBlanked keeps the two planes agreeing.
// `PATCH /assets/{id}` refuses an empty title; a mirrored write or clear that
// accepted one would let the field plane put the row into a state the asset
// plane forbids, through a verb nobody thinks of as an edit to the asset.
func TestMirroredField_RequiredCannotBeBlanked(t *testing.T) {
	pool := mirrorPool(t)
	t.Cleanup(pool.Close)

	assetID := mirrorAsset(t, pool, mirrorOwnerRef, "keep me")
	titleID := mirrorFieldID(t, pool, "title")
	descID := mirrorFieldID(t, pool, "description")
	router := mirrorRouter(t, pool, mirrorOwnerRef, nil)

	blank := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, titleID), map[string]any{
		"value_text": "   ",
	})
	if blank.Code != http.StatusBadRequest {
		t.Errorf("blanking a required mirrored field: status=%d want 400 body=%s", blank.Code, blank.Body.String())
	}
	if got := mirrorColumnValue(t, pool, assetID, "title"); got != "keep me" {
		t.Errorf("assets.title = %q after a refused blanking", got)
	}

	del := mirrorDelete(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, titleID))
	if del.Code != http.StatusBadRequest {
		t.Errorf("clearing a required mirrored field: status=%d want 400 body=%s", del.Code, del.Body.String())
	}
	if got := mirrorColumnValue(t, pool, assetID, "title"); got != "keep me" {
		t.Errorf("assets.title = %q after a refused clear — the DELETE answered 204 and did nothing, or worse", got)
	}

	// An OPTIONAL mirrored field clears, and clearing empties the COLUMN.
	// Without the mirrored branch this DELETE finds no row, answers 204, and
	// leaves the description exactly where it was — a lie the caller has no
	// way to detect.
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET description = 'to be cleared' WHERE id = $1`, assetID); err != nil {
		t.Fatalf("set description: %v", err)
	}
	clr := mirrorDelete(t, router, fmt.Sprintf("/assets/%s/fields/%s", assetID, descID))
	if clr.Code != http.StatusNoContent {
		t.Fatalf("clearing an optional mirrored field: status=%d body=%s", clr.Code, clr.Body.String())
	}
	if got := mirrorColumnValue(t, pool, assetID, "description"); got != "" {
		t.Errorf("assets.description = %q after a 204 clear; the clear reported success and wrote nothing", got)
	}
}

// TestMirroredField_ShippedDeclarationsAreWired asserts the migration seeded
// the two definitions the issue is about. A silent failure here turns every
// other test in this file into a test of ordinary fields.
func TestMirroredField_ShippedDeclarationsAreWired(t *testing.T) {
	pool := mirrorPool(t)
	t.Cleanup(pool.Close)

	for code, want := range map[string]string{"title": "title", "description": "description"} {
		var got *string
		if err := pool.QueryRow(context.Background(),
			`SELECT mirrors_column FROM field_definition WHERE code = $1 AND subject_kind = 'asset'`,
			code).Scan(&got); err != nil {
			t.Fatalf("read mirrors_column for %s: %v", code, err)
		}
		if got == nil || *got != want {
			t.Errorf("field %q mirrors_column = %v, want %q", code, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mirrorPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	mirrorClean(t, pool)
	t.Cleanup(func() { mirrorClean(t, pool) })
	return pool
}

func mirrorClean(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value_history WHERE asset_id IN (SELECT id FROM assets WHERE owner_user_ref IN ($1, $2))`, mirrorOwnerRef, mirrorStrangerRef)
	_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value WHERE asset_id IN (SELECT id FROM assets WHERE owner_user_ref IN ($1, $2))`, mirrorOwnerRef, mirrorStrangerRef)
	_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE owner_user_ref IN ($1, $2)`, mirrorOwnerRef, mirrorStrangerRef)
	_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value_history WHERE field_id IN (SELECT id FROM field_definition WHERE code LIKE 'mirror_test_%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value WHERE field_id IN (SELECT id FROM field_definition WHERE code LIKE 'mirror_test_%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM field_definition WHERE code LIKE 'mirror_test_%'`)
}

func mirrorAsset(t *testing.T, pool *pgxpool.Pool, owner int64, title string) string {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO assets (title, description, asset_type, owner_user_ref)
		 VALUES ($1, 'seeded description', 1, $2) RETURNING id`,
		title, owner).Scan(&id); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	return id.String()
}

func mirrorFieldID(t *testing.T, pool *pgxpool.Pool, code string) string {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM field_definition WHERE code = $1 AND subject_kind = 'asset'`, code).Scan(&id); err != nil {
		t.Fatalf("look up shipped field %q: %v", code, err)
	}
	return id.String()
}

// mirrorOrdinaryField is the control: a field of the same TYPE as `title`
// that declares no mirror, so every refusal in this file can be shown to be
// about the declaration rather than about the statement or the caller.
func mirrorOrdinaryField(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO field_definition (code, label, type, subject_kind)
		 VALUES ('mirror_test_control', 'Mirror Control', 'text', 'asset')
		 ON CONFLICT (code) DO UPDATE SET label = EXCLUDED.label
		 RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("create control field: %v", err)
	}
	return id.String()
}

func mirrorColumnValue(t *testing.T, pool *pgxpool.Pool, assetID, column string) string {
	t.Helper()
	// The assertion reads the COLUMN, on its own connection, through the
	// same accessor the production read path uses. Never the handler's
	// response body: an echo passes on a build that wrote nothing.
	var v *string
	if err := pool.QueryRow(context.Background(),
		`SELECT public.asset_mirror_read($1, $2)`, assetID, column).Scan(&v); err != nil {
		t.Fatalf("read %s: %v", column, err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func mirrorStoredRowCount(t *testing.T, pool *pgxpool.Pool, assetID, fieldID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		assetID, fieldID).Scan(&n); err != nil {
		t.Fatalf("count values: %v", err)
	}
	return n
}

func mirrorHistoryRowCount(t *testing.T, pool *pgxpool.Pool, assetID, fieldID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM asset_field_value_history WHERE asset_id = $1 AND field_id = $2`,
		assetID, fieldID).Scan(&n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	return n
}

func mirrorGetFields(t *testing.T, router chi.Router, assetID string) map[string]openapi.AssetFieldValue {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/assets/"+assetID+"/fields", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET fields: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var values []openapi.AssetFieldValue
	mustDecode(t, rr.Body.Bytes(), &values)
	out := make(map[string]openapi.AssetFieldValue, len(values))
	for _, v := range values {
		out[v.FieldId.String()] = v
	}
	return out
}

func mirrorDelete(t *testing.T, router chi.Router, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, path, nil))
	return rr
}

// mirrorRouter builds the metadata router for a specific principal. Unlike
// makeRouter it takes the user ref and the capability set, because the whole
// point of the permission tests is that WHO is asking changes the answer.
func mirrorRouter(t *testing.T, pool *pgxpool.Pool, userRef int64, caps []string) chi.Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := metadata.NewHandler(pool, logger, nil)
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
	openapi.HandlerFromMux(openapi.NewStrictHandler(metaShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router
}
