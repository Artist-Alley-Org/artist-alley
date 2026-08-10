// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #552 — the operator-configurable at-a-glance field set, and the two
// properties ADR 0012's 2026-08-10 amendment makes binding.
//
// # 1. It is a HINT, not a gate
//
// The amendment puts `show_on_card` in `display_order` / `display_group`'s
// class: *"nothing may branch on it for access, filtering, or correctness —
// a client that ignores it entirely must still be correct, merely plainer."*
//
// A test that only checks "the marked field appears" would pass on an
// implementation that also quietly changed which assets a page returns. So
// the load-bearing assertion here is the NEGATIVE one: marking a field
// changes the page's membership and ordering not at all, and unmarking it
// leaves every asset still readable with the card's own fallback intact.
//
// # 2. A gated field can never reach a card
//
// The card renders on browse, for a page of assets, where the server has
// evaluated no per-field capability. The refusal is a CHECK constraint, so
// it holds in both directions and on every path — not a filter in the query
// that someone has to remember. Both directions are exercised, each with its
// negative control proved constructible first.
//
// Assertions are on PERSISTED state and on what the handler actually
// projects, never on a request echo.
//
// Skips without AA_DB_PASSWORD.

package assets_test

import (
	"context"
	"fmt"
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

type cardWorld struct {
	t     *testing.T
	pool  *pgxpool.Pool
	h     *assets.Handler
	owner int64
}

func newCardWorld(t *testing.T) *cardWorld {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := openPool(t, pwd)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	h := assets.NewHandler(pool, storage.NewService(backend, pool), logger, nil, nil, nil)

	var owner int64
	name := "card-owner-" + uuid.NewString()[:8]
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`, name).Scan(&owner); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	w := &cardWorld{t: t, pool: pool, h: h, owner: owner}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value WHERE field_id IN (SELECT id FROM field_definition WHERE code LIKE 'card_test_%')`)
		_, _ = pool.Exec(ctx, `DELETE FROM field_definition WHERE code LIKE 'card_test_%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE owner_user_ref = $1`, owner)
		_, _ = pool.Exec(ctx, `DELETE FROM "user" WHERE ref = $1`, owner)
	})
	return w
}

func (w *cardWorld) asset(title string) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if _, err := w.pool.Exec(context.Background(),
		`INSERT INTO assets (id, owner_user_ref, title, description, asset_type, status, sensitivity, processing_status)
		 VALUES ($1,$2,$3,'',(SELECT MIN(ref) FROM asset_types),'active','public','ready')`,
		id, w.owner, title); err != nil {
		w.t.Fatalf("seed asset: %v", err)
	}
	return id
}

// field creates an ordinary asset field. `carded` is applied as a SEPARATE
// UPDATE so the same helper can prove the constraint refuses the flag as
// well as accept it.
func (w *cardWorld) field(code, label, ftype string) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(),
		`INSERT INTO field_definition (code, label, type, subject_kind, display_group, display_order)
		 VALUES ($1,$2,$3,'asset','card_test',10) RETURNING id`, code, label, ftype).Scan(&id); err != nil {
		w.t.Fatalf("seed field %s: %v", code, err)
	}
	return id
}

func (w *cardWorld) setCarded(fieldID uuid.UUID, on bool) error {
	_, err := w.pool.Exec(context.Background(),
		`UPDATE field_definition SET show_on_card = $2 WHERE id = $1`, fieldID, on)
	return err
}

func (w *cardWorld) setValue(assetID, fieldID uuid.UUID, text string) {
	w.t.Helper()
	if _, err := w.pool.Exec(context.Background(),
		`INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by) VALUES ($1,$2,$3,'import')
		 ON CONFLICT (asset_id, field_id) DO UPDATE SET value_text = EXCLUDED.value_text`,
		assetID, fieldID, text); err != nil {
		w.t.Fatalf("seed value: %v", err)
	}
}

func (w *cardWorld) get(assetID uuid.UUID) openapi.Asset {
	w.t.Helper()
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: w.owner, AuthMethod: "session"})
	resp, err := w.h.GetAsset(ctx, openapi.GetAssetRequestObject{Id: openapi_types.UUID(assetID)})
	if err != nil {
		w.t.Fatalf("GetAsset: %v", err)
	}
	ok, is := resp.(openapi.GetAsset200JSONResponse)
	if !is {
		w.t.Fatalf("GetAsset returned %T, want 200", resp)
	}
	return openapi.Asset(ok)
}

// pageIDs drives the real browse list as the owner and returns the ids in
// the order served — the thing a display hint must never change.
func (w *cardWorld) pageIDs() []string {
	w.t.Helper()
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: w.owner, AuthMethod: "session"})
	limit := 50
	ref := w.owner
	resp, err := w.h.ListAssets(ctx, openapi.ListAssetsRequestObject{
		Params: openapi.ListAssetsParams{Limit: &limit, OwnerRef: &ref},
	})
	if err != nil {
		w.t.Fatalf("ListAssets: %v", err)
	}
	ok, is := resp.(openapi.ListAssets200JSONResponse)
	if !is {
		w.t.Fatalf("ListAssets returned %T, want 200", resp)
	}
	out := make([]string, 0, len(ok.Items))
	for _, a := range ok.Items {
		out = append(out, a.Id.String())
	}
	return out
}

func cardValues(a openapi.Asset) map[string]string {
	out := map[string]string{}
	if a.CardFields == nil {
		return out
	}
	for _, f := range *a.CardFields {
		out[f.Code] = f.Value
	}
	return out
}

// ---------------------------------------------------------------------------

// TestCardFields_MarkedFieldsRideOnTheAssetPayload is the positive case, and
// it covers the two storage shapes a card must not be able to tell apart: a
// stored value, and a MIRRORED field whose value is the `assets` column
// (#822). A card that rendered one and not the other would make the mirror
// visible to the client, which is exactly what the mirror exists to prevent.
//
// It also pins the vocabulary rule: a `select` value stores a SLUG and the
// card must show the LABEL. Printing the slug is #775's defect on a new
// surface.
func TestCardFields_MarkedFieldsRideOnTheAssetPayload(t *testing.T) {
	w := newCardWorld(t)
	assetID := w.asset("a titled asset")

	text := w.field("card_test_stage_note", "Stage note", "text")
	w.setValue(assetID, text, "ready for review")
	if err := w.setCarded(text, true); err != nil {
		t.Fatalf("mark field: %v", err)
	}

	// A select field, to prove the slug is resolved rather than printed.
	var sel uuid.UUID
	if err := w.pool.QueryRow(context.Background(),
		`INSERT INTO field_definition (code, label, type, subject_kind, display_group, display_order, options, show_on_card)
		 VALUES ('card_test_stage','Stage','select','asset','card_test',20,
		         '{"values":[{"value":"pass-1","label":"Pass 1"}]}'::jsonb, true)
		 RETURNING id`).Scan(&sel); err != nil {
		t.Fatalf("seed select field: %v", err)
	}
	w.setValue(assetID, sel, "pass-1")

	// The MIRRORED field. Nothing special is done to it here: it is the
	// shipped `title` definition, and its value is the column.
	if _, err := w.pool.Exec(context.Background(),
		`UPDATE field_definition SET show_on_card = true WHERE code = 'title' AND subject_kind = 'asset'`); err != nil {
		t.Fatalf("card the mirrored field: %v", err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(),
			`UPDATE field_definition SET show_on_card = false WHERE code = 'title' AND subject_kind = 'asset'`)
	})

	got := cardValues(w.get(assetID))
	if got["card_test_stage_note"] != "ready for review" {
		t.Errorf("stored field on the card = %q, want %q", got["card_test_stage_note"], "ready for review")
	}
	if got["card_test_stage"] != "Pass 1" {
		t.Errorf("vocabulary field on the card = %q, want the LABEL %q — a card showing the slug is #775 again",
			got["card_test_stage"], "Pass 1")
	}
	if got["title"] != "a titled asset" {
		t.Errorf("mirrored field on the card = %q, want the column's value — the card can tell a mirror from a stored field",
			got["title"])
	}

	// A field with nothing on this asset is omitted, not rendered blank.
	empty := w.field("card_test_empty", "Empty", "text")
	if err := w.setCarded(empty, true); err != nil {
		t.Fatalf("mark empty field: %v", err)
	}
	if v, present := cardValues(w.get(assetID))["card_test_empty"]; present {
		t.Errorf("a field with no value produced a card entry %q; the tile would render a blank row", v)
	}
}

// TestCardFields_TheFlagChangesNothingButPresentation is the assertion the
// ADR amendment actually binds. A display hint that altered which rows a
// browse page returns, or their order, would be a filter wearing a hint's
// name.
func TestCardFields_TheFlagChangesNothingButPresentation(t *testing.T) {
	w := newCardWorld(t)
	for i := 0; i < 3; i++ {
		w.asset(fmt.Sprintf("hint asset %d", i))
	}
	before := w.pageIDs()
	if len(before) != 3 {
		t.Fatalf("fixture: page has %d assets, want 3", len(before))
	}

	f := w.field("card_test_hint", "Hint", "text")
	// Deliberately valued on NO asset: if the flag were participating in
	// row selection, a field nothing carries is the case that would drop
	// rows from the page.
	if err := w.setCarded(f, true); err != nil {
		t.Fatalf("mark field: %v", err)
	}

	after := w.pageIDs()
	if len(after) != len(before) {
		t.Fatalf("marking a field changed the page size: %d -> %d; the hint is filtering", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("marking a field reordered the page at %d (%s -> %s); the hint is sorting", i, before[i], after[i])
		}
	}

	// And a client that ignores the array entirely is merely plainer: every
	// asset is still fully readable, which is what "still correct" means on
	// this surface.
	for _, id := range after {
		a := w.get(uuid.MustParse(id))
		if a.Restricted {
			t.Fatalf("asset %s became restricted after a display hint was set", id)
		}
		if a.Title == nil || *a.Title == "" {
			t.Fatalf("asset %s lost its title after a display hint was set", id)
		}
	}
}

// TestCardFields_AGatedFieldCannotBeCarded exercises the CHECK constraint in
// both directions, each after proving the same statement lands on a field
// that does not trip it.
func TestCardFields_AGatedFieldCannotBeCarded(t *testing.T) {
	w := newCardWorld(t)
	ctx := context.Background()

	// Direction 1: card a field that already carries a read capability.
	gated := w.field("card_test_gated", "Gated", "text")
	if _, err := w.pool.Exec(ctx,
		`UPDATE field_definition SET read_capability = 'assets.admin' WHERE id = $1`, gated); err != nil {
		t.Fatalf("seed read capability: %v", err)
	}
	// CONSTRUCTIBILITY: the identical UPDATE on an UNGATED field lands.
	ungated := w.field("card_test_ungated", "Ungated", "text")
	if err := w.setCarded(ungated, true); err != nil {
		t.Fatalf("control: carding an ungated field failed (%v); the refusal below would prove nothing", err)
	}
	if err := w.setCarded(gated, true); err == nil {
		t.Fatal("a field carrying a read_capability was allowed onto the card; the card is a side door around the gate")
	}

	// Direction 2: gate a field that is already on the card. The constraint
	// holds this way too, so an operator cannot turn a live card into a leak
	// by editing the capability instead of the flag.
	if _, err := w.pool.Exec(ctx,
		`UPDATE field_definition SET read_capability = 'assets.admin' WHERE id = $1`, ungated); err == nil {
		t.Fatal("a carded field was allowed to acquire a read capability")
	}
	// CONSTRUCTIBILITY for direction 2: the same UPDATE on a field that is
	// NOT carded succeeds, so the refusal is about the pairing.
	plain := w.field("card_test_plain", "Plain", "text")
	if _, err := w.pool.Exec(ctx,
		`UPDATE field_definition SET read_capability = 'assets.admin' WHERE id = $1`, plain); err != nil {
		t.Fatalf("control: gating a non-carded field failed (%v); the refusal above would prove nothing", err)
	}
}
