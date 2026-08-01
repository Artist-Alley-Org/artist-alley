// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Upload defaults against a real database (#793, ADR 0081 §3).
//
// The unit tests in defaults_test.go check the decisions. This file
// checks that the decisions survive contact with Postgres: that a
// default lands in the column the storage pin names, that it carries
// the provenance the extraction applier needs, that a team override
// beats the field default and falls back cleanly when removed, and —
// the two negatives — that a default never overwrites a value that is
// already there.
//
// Everything is asserted against the COLUMNS, not against what a
// writer says it wrote. That is the #778 lesson: six call sites all
// agreed with themselves.
package metadata_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
)

const defaultsTestUserRef = int64(420000) // matches makeRouter's synthetic identity

// The two request shapes this file needs that the shared plumbing in
// handler_test.go does not already provide.

func getJSONBody(t *testing.T, r chi.Router, path string) []byte {
	t.Helper()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d body=%s", path, rr.Code, rr.Body.String())
	}
	return rr.Body.Bytes()
}

func deleteReq(t *testing.T, r chi.Router, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, path, nil))
	return rr
}

// storedValue is one asset_field_value row as the database holds it.
type storedValue struct {
	Text    *string
	Num     *float64
	Date    *time.Time
	Options []string
	Ref     *uuid.UUID
	SetBy   string
}

func readStored(t *testing.T, pool *pgxpool.Pool, assetID, fieldID string) (storedValue, bool) {
	t.Helper()
	var v storedValue
	err := pool.QueryRow(context.Background(), `
		SELECT value_text, value_num, value_date, value_options, value_ref, set_by
		  FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		assetID, fieldID).Scan(&v.Text, &v.Num, &v.Date, &v.Options, &v.Ref, &v.SetBy)
	if err != nil {
		return storedValue{}, false
	}
	return v, true
}

// populatedColumns names every value_* column the row actually holds.
// A default that fills two, or none, is as broken as one that fills the
// wrong one.
func (v storedValue) populatedColumns() []string {
	var out []string
	if v.Text != nil {
		out = append(out, "value_text")
	}
	if v.Num != nil {
		out = append(out, "value_num")
	}
	if v.Date != nil {
		out = append(out, "value_date")
	}
	if v.Options != nil {
		out = append(out, "value_options")
	}
	if v.Ref != nil {
		out = append(out, "value_ref")
	}
	return out
}

func mustApplyDefaults(t *testing.T, pool *pgxpool.Pool, assetID string) []metadata.AppliedDefault {
	t.Helper()
	aid, err := uuid.Parse(assetID)
	if err != nil {
		t.Fatalf("asset id: %v", err)
	}
	applied, err := metadata.ApplyAssetDefaults(context.Background(), pool, metadata.ApplyDefaultsParams{
		AssetID:   aid,
		AssetType: 1,
		UserRef:   defaultsTestUserRef,
		Now:       time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ApplyAssetDefaults: %v", err)
	}
	return applied
}

// makeDefaultsTeam creates a throwaway team the synthetic test user is
// a member of, and registers its teardown.
func makeDefaultsTeam(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO teams (slug, name) VALUES ($1, $2) RETURNING id`,
		slug, "Defaults "+slug).Scan(&id); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2)`,
		id, defaultsTestUserRef); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
	t.Cleanup(func() {
		// FK cascade takes the memberships and the overrides with it.
		_, _ = pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	return id.String()
}

func cleanDefaultsTestRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, _ = pool.Exec(ctx, `DELETE FROM teams WHERE slug LIKE 'mtd-%'`)
	_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value_history WHERE field_id IN (SELECT id FROM field_definition WHERE code LIKE 'mtv_%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value WHERE field_id IN (SELECT id FROM field_definition WHERE code LIKE 'mtv_%')`)
}

func openDefaultsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	cleanTestFields(t, pool)
	cleanDefaultsTestRows(t, pool)
	t.Cleanup(func() {
		cleanDefaultsTestRows(t, pool)
		cleanTestFields(t, pool)
	})
	return pool
}

// ---------------------------------------------------------------------------
// The chain, end to end
// ---------------------------------------------------------------------------

// TestDefaults_PrecedenceAgainstTheDatabase walks every position of
// ADR 0081 §3's chain against real rows, including both negatives.
func TestDefaults_PrecedenceAgainstTheDatabase(t *testing.T) {
	pool := openDefaultsPool(t)
	router, userRef := makeRouter(t, pool /*admin=*/, true)

	// A vocabulary with a retired term, so the rejection case has
	// something real to aim at.
	vocab := map[string]any{"values": []any{
		"greybox", "polish",
		map[string]any{"value": "retired", "label": "Retired", "status": "deprecated"},
	}}
	fieldID := mustCreateField(t, router, map[string]any{
		"code": "mtv_stage", "label": "Stage", "type": "select",
		"options": vocab,
		"default_value": map[string]any{
			"kind": "literal", "value_text": "greybox",
		},
	})

	teamID := makeDefaultsTeam(t, pool, "mtd-textures")

	t.Run("field default applies to a new asset", func(t *testing.T) {
		assetID := mustInsertAsset(t, pool, userRef)
		applied := mustApplyDefaults(t, pool, assetID)
		if len(applied) != 1 {
			t.Fatalf("applied %d defaults, want 1", len(applied))
		}

		got, ok := readStored(t, pool, assetID, fieldID)
		if !ok {
			t.Fatal("no asset_field_value row — the default did not apply")
		}
		if got.Text == nil || *got.Text != "greybox" {
			t.Errorf("stored %v, want value_text=greybox", got)
		}
		// Acceptance item 6: the column the pin names for `select`.
		if cols := got.populatedColumns(); len(cols) != 1 || cols[0] != "value_text" {
			t.Errorf("a select default populated %v — a `select` value lives in value_text "+
				"and nowhere else (valuecolumn_test.go)", cols)
		}
		// The provenance that makes the rest of the chain possible.
		if got.SetBy != metadata.SetByDefault {
			t.Errorf("set_by = %q, want %q — without it the extraction applier "+
				"cannot tell a default from a human's edit and skip_if_set protects it forever",
				got.SetBy, metadata.SetByDefault)
		}
	})

	t.Run("a default never overwrites a value already set", func(t *testing.T) {
		assetID := mustInsertAsset(t, pool, userRef)
		// Someone chose a value first — the upload modal's per-field
		// input, an API caller, an import.
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by)
			VALUES ($1, $2, 'polish', 'manual')`, assetID, fieldID); err != nil {
			t.Fatalf("seed manual value: %v", err)
		}

		if applied := mustApplyDefaults(t, pool, assetID); len(applied) != 0 {
			t.Errorf("the defaults pass reported %d writes over an occupied field", len(applied))
		}
		got, _ := readStored(t, pool, assetID, fieldID)
		if got.Text == nil || *got.Text != "polish" || got.SetBy != "manual" {
			t.Errorf("a default clobbered a chosen value: %v — "+
				"the writer's ON CONFLICT DO NOTHING is what makes this true regardless "+
				"of where the caller sits in the sequence", got)
		}
	})

	t.Run("a default never overwrites an extracted value", func(t *testing.T) {
		assetID := mustInsertAsset(t, pool, userRef)
		// Extraction got there first — the case ADR 0081 §3 calls out
		// by name. Same guarantee, different provenance, and worth its
		// own assertion because it is the one the ADR's own text says
		// the mechanism could not deliver.
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by)
			VALUES ($1, $2, 'polish', 'exif')`, assetID, fieldID); err != nil {
			t.Fatalf("seed extracted value: %v", err)
		}

		mustApplyDefaults(t, pool, assetID)
		got, _ := readStored(t, pool, assetID, fieldID)
		if got.Text == nil || *got.Text != "polish" || got.SetBy != "exif" {
			t.Errorf("a default overwrote an extracted value: %v", got)
		}
	})

	t.Run("a team override beats the field default", func(t *testing.T) {
		rr := putJSON(t, router, "/fields/"+fieldID+"/default-overrides/"+teamID,
			map[string]any{"kind": "literal", "value_text": "polish"})
		if rr.Code != http.StatusOK {
			t.Fatalf("set override: %d body=%s", rr.Code, rr.Body.String())
		}

		assetID := mustInsertAsset(t, pool, userRef)
		applied := mustApplyDefaults(t, pool, assetID)
		if len(applied) != 1 {
			t.Fatalf("applied %d defaults, want 1", len(applied))
		}
		if !applied[0].TeamID.Valid {
			t.Error("the applied value was not attributed to the team that supplied it")
		}

		got, _ := readStored(t, pool, assetID, fieldID)
		if got.Text == nil || *got.Text != "polish" {
			t.Errorf("stored %v, want the team's override (polish) rather than the field default (greybox)", got)
		}
	})

	t.Run("removing the override falls back to the field default", func(t *testing.T) {
		rr := deleteReq(t, router, "/fields/"+fieldID+"/default-overrides/"+teamID)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("delete override: %d body=%s", rr.Code, rr.Body.String())
		}

		assetID := mustInsertAsset(t, pool, userRef)
		mustApplyDefaults(t, pool, assetID)
		got, _ := readStored(t, pool, assetID, fieldID)
		if got.Text == nil || *got.Text != "greybox" {
			t.Errorf("stored %v, want the field default back (greybox)", got)
		}

		// And the override really is gone from the admin view.
		list := getJSONBody(t, router, "/fields/"+fieldID+"/default-overrides")
		var overrides []map[string]any
		mustDecode(t, list, &overrides)
		if len(overrides) != 0 {
			t.Errorf("the removed override is still listed: %s", list)
		}
	})
}

// Acceptance item 4, through the API rather than through the validator
// directly — the rejection has to reach an operator, not merely exist.
func TestDefaults_RetiredOptionRejectedByTheAPI(t *testing.T) {
	pool := openDefaultsPool(t)
	router, _ := makeRouter(t, pool /*admin=*/, true)

	fieldID := mustCreateField(t, router, map[string]any{
		"code": "mtv_stage2", "label": "Stage", "type": "select",
		"options": map[string]any{"values": []any{
			"greybox",
			map[string]any{"value": "retired", "label": "Retired", "status": "deprecated"},
			map[string]any{"value": "mistake", "label": "Mistake", "status": "archived"},
		}},
	})

	for _, slug := range []string{"retired", "mistake", "never-existed"} {
		t.Run("field default: "+slug, func(t *testing.T) {
			rr := patchJSON(t, router, "/fields/"+fieldID, map[string]any{
				"default_value": map[string]any{"kind": "literal", "value_text": slug},
			})
			if rr.Code != http.StatusBadRequest {
				t.Errorf("PATCH with default %q returned %d, want 400: %s", slug, rr.Code, rr.Body.String())
			}
		})
	}

	teamID := makeDefaultsTeam(t, pool, "mtd-props")
	for _, slug := range []string{"retired", "mistake"} {
		t.Run("team override: "+slug, func(t *testing.T) {
			rr := putJSON(t, router, "/fields/"+fieldID+"/default-overrides/"+teamID,
				map[string]any{"kind": "literal", "value_text": slug})
			if rr.Code != http.StatusBadRequest {
				t.Errorf("override with %q returned %d, want 400 — an override is validated "+
					"exactly as the field default is, or the retirement rule has a hole in it: %s",
					slug, rr.Code, rr.Body.String())
			}
		})
	}

	// The active term still works on both surfaces.
	if rr := patchJSON(t, router, "/fields/"+fieldID, map[string]any{
		"default_value": map[string]any{"kind": "literal", "value_text": "greybox"},
	}); rr.Code != http.StatusOK {
		t.Errorf("PATCH with an active term returned %d: %s", rr.Code, rr.Body.String())
	}
	if rr := putJSON(t, router, "/fields/"+fieldID+"/default-overrides/"+teamID,
		map[string]any{"kind": "literal", "value_text": "greybox"}); rr.Code != http.StatusOK {
		t.Errorf("override with an active term returned %d: %s", rr.Code, rr.Body.String())
	}

	// clear_default removes it — the state a PATCH cannot otherwise
	// express, since a null there means "leave alone".
	if rr := patchJSON(t, router, "/fields/"+fieldID, map[string]any{"clear_default": true}); rr.Code != http.StatusOK {
		t.Fatalf("clear_default returned %d: %s", rr.Code, rr.Body.String())
	}
	var def map[string]any
	mustDecode(t, getJSONBody(t, router, "/fields/"+fieldID), &def)
	if v, present := def["default_value"]; present && v != nil {
		t.Errorf("clear_default left %v behind", v)
	}
}

// A context default resolves from real rows: the uploader's display
// name and the team they belong to, read out of the database rather
// than handed in by a fixture.
func TestDefaults_ContextValuesResolveFromRealRows(t *testing.T) {
	pool := openDefaultsPool(t)
	router, userRef := makeRouter(t, pool /*admin=*/, true)
	ctx := context.Background()

	// The synthetic identity has no `user` row of its own; give it one
	// so the display-name lookup has something to find. A test whose
	// fixture production cannot reach proves nothing.
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username, fullname) VALUES ($1, 'mtd_uploader', 'Ada Lovelace')
		 ON CONFLICT (ref) DO UPDATE SET fullname = EXCLUDED.fullname`, userRef); err != nil {
		t.Skipf("cannot create the uploading user row: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, userRef) })

	teamID := makeDefaultsTeam(t, pool, "mtd-vfx")
	var teamName string
	if err := pool.QueryRow(ctx, `SELECT name FROM teams WHERE id = $1`, teamID).Scan(&teamName); err != nil {
		t.Fatalf("read team name: %v", err)
	}

	userField := mustCreateField(t, router, map[string]any{
		"code": "mtv_author", "label": "Author", "type": "text",
		"default_value": map[string]any{"kind": "context", "context": "uploading_user"},
	})
	teamField := mustCreateField(t, router, map[string]any{
		"code": "mtv_owning_team", "label": "Owning team", "type": "text",
		"default_value": map[string]any{"kind": "context", "context": "uploading_team"},
	})
	dateField := mustCreateField(t, router, map[string]any{
		"code": "mtv_captured", "label": "Captured", "type": "datetime",
		"default_value": map[string]any{"kind": "context", "context": "current_date"},
	})

	assetID := mustInsertAsset(t, pool, userRef)
	mustApplyDefaults(t, pool, assetID)

	if got, ok := readStored(t, pool, assetID, userField); !ok || got.Text == nil || *got.Text != "Ada Lovelace" {
		t.Errorf("uploading_user stored %v, want the user's display name", got)
	}
	if got, ok := readStored(t, pool, assetID, teamField); !ok || got.Text == nil || *got.Text != teamName {
		t.Errorf("uploading_team stored %v, want %q", got, teamName)
	}
	got, ok := readStored(t, pool, assetID, dateField)
	if !ok || got.Date == nil {
		t.Fatalf("current_date stored %v, want a timestamp", got)
	}
	if cols := got.populatedColumns(); len(cols) != 1 || cols[0] != "value_date" {
		t.Errorf("current_date on a datetime field populated %v, want value_date only", cols)
	}
}

// A default naming an expression is not a default. Nothing resembling
// the prior art's macro column can be stored, whichever door it comes
// through.
func TestDefaults_NoExpressionLanguage(t *testing.T) {
	pool := openDefaultsPool(t)
	router, _ := makeRouter(t, pool /*admin=*/, true)

	fieldID := mustCreateField(t, router, map[string]any{
		"code": "mtv_macro", "label": "Macro", "type": "text",
	})

	for _, body := range []map[string]any{
		{"kind": "context", "context": "{{ .User.Email }}"},
		{"kind": "context", "context": "php:getUserEmail()"},
		{"kind": "macro", "value_text": "anything"},
		{"kind": "literal"},
		{"kind": "literal", "value_text": "x", "context": "uploading_user"},
	} {
		raw, _ := json.Marshal(body)
		rr := patchJSON(t, router, "/fields/"+fieldID, map[string]any{"default_value": body})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("stored %s (status %d) — the default document is a closed shape, "+
				"not a place to put code: %s", raw, rr.Code, rr.Body.String())
		}
	}
}
