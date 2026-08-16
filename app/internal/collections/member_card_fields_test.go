// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1133 — `show_on_card` on the collection member grid.
//
// # The bug this pins
//
// The flag shipped in #552 and worked on every surface `assets` serves.
// `GET /collections/{id}/resources` builds its rows in `collections`,
// which never called the decoration — and could not have, since
// `assets` is unreachable from here (assets → posts → collections is an
// import cycle). So a member tile rendered the same card as a browse
// tile with the strip missing, for a year, and nothing failed.
//
// # Why this drives the HANDLER and not the query
//
// `member_allowlist_test.go` next door tests the gated query plus
// `resourceRowToAPI`, which is right for the allow-list it asserts. It
// would be exactly the wrong layer here: the row and its serialiser were
// never the problem, the missing DECORATION PASS was, and a test that
// stops below the handler passes on the bug.
//
// # The two behaviours #552 defines, both asserted
//
//  1. A marked field's value reaches the tile.
//  2. A vocabulary field arrives as its LABEL, not the stored slug —
//     printing the slug is #775's defect on a new surface. This one
//     matters most here, because it is the assertion that proves the
//     value came through metadata.DisplayValue rather than being read
//     straight off the column by a second, simpler projection.
//
// Plus the two negative arms that keep the fix inside the display
// plane: an UNMARKED field must not appear (or the strip is "every
// field", not "the marked ones"), and a RESTRICTED member must carry no
// strip at all (#883's allow-list — a withheld row's key set does not
// grow because a display hint was added).
//
// Skips without AA_DB_PASSWORD.

package collections_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const (
	mcfOwner    int64 = 11330001
	mcfStranger int64 = 11330002
)

func mcfPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + env("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// mcfField plants one field definition and returns its id. `carded`
// drives `show_on_card`; the UNMARKED case is a real fixture here, not
// an absence, so "the strip is the marked fields" is an assertion rather
// than an assumption.
func mcfField(t *testing.T, pool *pgxpool.Pool, code, label, ftype, options string, carded bool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO field_definition
		     (code, label, type, subject_kind, display_group, display_order, options, show_on_card)
		 VALUES ($1, $2, $3, 'asset', 'mcf', 10, $4::jsonb, $5)
		 RETURNING id`,
		code, label, ftype, options, carded).Scan(&id); err != nil {
		t.Fatalf("seed field %s: %v", code, err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE field_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM field_definition WHERE id = $1`, id)
	})
	return id
}

func mcfSetValue(t *testing.T, pool *pgxpool.Pool, assetID, fieldID uuid.UUID, text string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO asset_field_value (asset_id, field_id, value_text, set_by, set_at)
		 VALUES ($1, $2, $3, 'manual', NOW())
		 ON CONFLICT (asset_id, field_id) DO UPDATE SET value_text = EXCLUDED.value_text`,
		assetID, fieldID, text); err != nil {
		t.Fatalf("set value: %v", err)
	}
}

func mcfAsset(t *testing.T, pool *pgxpool.Pool, title, sensitivity string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	sum := sha256.Sum256(id[:])
	hash := hex.EncodeToString(sum[:])
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 4242, 'fs')
		 ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                    processing_status, file_hash, file_extension, file_size_bytes)
		VALUES ($1, $2, $3, (SELECT MIN(ref) FROM asset_types), 'active', $4, 'ready', $5, 'png', 1234)`,
		id, title, mcfOwner, sensitivity, hash); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

func mcfCollection(t *testing.T, pool *pgxpool.Pool, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO collections (id, name, owner_user_ref, visibility) VALUES ($1, $2, $3, 'public')`,
		id, "mcf collection", mcfOwner); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	for i, m := range members {
		if _, err := pool.Exec(ctx,
			`INSERT INTO collection_resources (collection_id, asset_id, sort_order, pinned)
			 VALUES ($1, $2, $3, TRUE)`, id, m, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM collection_resources WHERE collection_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE id = $1`, id)
	})
	return id
}

// mcfMembers drives the REAL endpoint as `ref`.
func mcfMembers(
	t *testing.T,
	h *collections.Handler,
	ref int64,
	colID uuid.UUID,
) []openapi.CollectionResource {
	t.Helper()
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: ref, AuthMethod: "session"})
	resp, err := h.ListCollectionResources(ctx, openapi.ListCollectionResourcesRequestObject{
		Id: openapi_types.UUID(colID),
	})
	if err != nil {
		t.Fatalf("ListCollectionResources: %v", err)
	}
	ok, is200 := resp.(openapi.ListCollectionResources200JSONResponse)
	if !is200 {
		t.Fatalf("ListCollectionResources: got %T, want a 200", resp)
	}
	return ok.Items
}

// mcfStrip reduces one member to code → value.
func mcfStrip(t *testing.T, items []openapi.CollectionResource, assetID uuid.UUID) map[string]string {
	t.Helper()
	for _, it := range items {
		if uuid.UUID(it.AssetId) != assetID {
			continue
		}
		out := map[string]string{}
		if it.CardFields == nil {
			return out
		}
		for _, f := range *it.CardFields {
			out[f.Code] = f.Value
		}
		return out
	}
	t.Fatalf("asset %v is not in the member list", assetID)
	return nil
}

// TestMemberCardFields_MarkedFieldsRideOnTheMemberRow is #1133's
// acceptance: the flag renders here exactly as it does on browse.
func TestMemberCardFields_MarkedFieldsRideOnTheMemberRow(t *testing.T) {
	pool := mcfPool(t)
	h := collections.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	assetID := mcfAsset(t, pool, "mcf member", "public")

	note := mcfField(t, pool, "mcf_stage_note", "Stage note", "text", `{}`, true)
	mcfSetValue(t, pool, assetID, note, "ready for review")

	// The vocabulary field. Its VALUE column holds a slug; the card must
	// show the label. This is the arm that proves the strip came through
	// metadata.DisplayValue and not off the column.
	stage := mcfField(t, pool, "mcf_stage", "Stage", "select",
		`{"values":[{"value":"pass-1","label":"Pass 1"}]}`, true)
	mcfSetValue(t, pool, assetID, stage, "pass-1")

	// Marked but EMPTY on this asset — no row, not a blank one.
	empty := mcfField(t, pool, "mcf_empty", "Empty", "text", `{}`, true)
	_ = empty

	// Present, populated, and NOT marked. If this appears, the pass is
	// "every field" rather than "the marked ones".
	hidden := mcfField(t, pool, "mcf_internal", "Internal", "text", `{}`, false)
	mcfSetValue(t, pool, assetID, hidden, "do not show this")

	colID := mcfCollection(t, pool, assetID)
	strip := mcfStrip(t, mcfMembers(t, h, mcfOwner, colID), assetID)

	if got := strip["mcf_stage_note"]; got != "ready for review" {
		t.Errorf("marked text field on the member tile = %q, want %q — #1133 is that this "+
			"strip never reached the collection grid at all", got, "ready for review")
	}
	if got := strip["mcf_stage"]; got != "Pass 1" {
		t.Errorf("vocabulary field on the member tile = %q, want the LABEL %q — a tile showing "+
			"the slug means the value bypassed metadata.DisplayValue", got, "Pass 1")
	}
	if got, present := strip["mcf_empty"]; present {
		t.Errorf("a marked field with no value produced an entry %q; the tile renders a blank row", got)
	}
	if got, present := strip["mcf_internal"]; present {
		t.Errorf("an UNMARKED field reached the member tile as %q — the pass is projecting "+
			"every field, not the carded ones", got)
	}
}

// TestMemberCardFields_RestrictedMemberCarriesNoStrip keeps the fix
// inside #883's allow-list.
//
// A restricted member's permitted key set is the `collection_resources`
// row's own columns plus `restricted` and `owner_display_name` — nothing
// from `assets`. `card_fields` IS something from assets, so adding the
// decoration without excluding placeholders would have widened a
// withheld row through a display hint. Asserted on the SERIALIZED row,
// because that is what a client receives.
func TestMemberCardFields_RestrictedMemberCarriesNoStrip(t *testing.T) {
	pool := mcfPool(t)
	h := collections.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	restricted := mcfAsset(t, pool, "SECRET concept", "restricted")
	pub := mcfAsset(t, pool, "public splash", "public")

	f := mcfField(t, pool, "mcf_leaky", "Leaky", "text", `{}`, true)
	mcfSetValue(t, pool, restricted, f, "UNRELEASED — do not distribute")
	mcfSetValue(t, pool, pub, f, "fine to show")

	colID := mcfCollection(t, pool, restricted, pub)
	items := mcfMembers(t, h, mcfStranger, colID)

	// The control: the readable member in the same page DOES carry the
	// strip. Without this the assertion below passes on a decoration pass
	// that never ran.
	if got := mcfStrip(t, items, pub)["mcf_leaky"]; got != "fine to show" {
		t.Fatalf("readable member's strip = %q, want %q", got, "fine to show")
	}

	for _, it := range items {
		if uuid.UUID(it.AssetId) != restricted {
			continue
		}
		if !it.Restricted {
			t.Fatalf("fixture wrong: the restricted member came back readable to a stranger")
		}
		raw, err := json.Marshal(it)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, present := keys["card_fields"]; present {
			t.Errorf("a WITHHELD member carries card_fields %s — #883's allow-list does not "+
				"grow because a display hint was added", raw)
		}
		return
	}
	t.Fatalf("the restricted member is missing from the page entirely; #883 requires a placeholder")
}
