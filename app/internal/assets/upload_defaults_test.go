// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The defaults pass is wired into asset CREATION (#793, ADR 0081 §3).
//
// app/internal/metadata covers what a default resolves to and where it
// lands. This file covers the one thing that package cannot: that
// CreateAsset actually calls it, inside its own transaction, on the
// path a real upload takes.
//
// Without this, deleting the call from handler.go leaves every other
// test green — the resolver would still be correct and nothing would
// ever run it.

package assets_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

func TestCreateAsset_AppliesUploadDefaults(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(func() { pool.Close() })
	ctx := context.Background()

	const ownerRef int64 = 9_142_793

	// A field carrying a literal default.
	var fieldID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO field_definition (code, label, type, subject_kind, options, default_value)
		VALUES ('aud_stage', 'Stage', 'select', 'asset',
		        '{"values":["greybox","polish"]}'::jsonb,
		        '{"kind":"literal","value_text":"greybox"}'::jsonb)
		RETURNING id`).Scan(&fieldID); err != nil {
		t.Fatalf("seed field: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value_history WHERE field_id = $1`, fieldID)
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE field_id = $1`, fieldID)
		_, _ = pool.Exec(c, `DELETE FROM field_definition WHERE id = $1`, fieldID)
	})

	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	h := assets.NewHandler(pool, storage.NewService(backend, pool),
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)

	create := func(t *testing.T) uuid.UUID {
		t.Helper()
		resp, err := h.CreateAsset(
			auth.WithIdentity(ctx, &auth.Identity{UserRef: ownerRef, Capabilities: []string{}}),
			openapi.CreateAssetRequestObject{
				Body: &openapi.AssetCreate{Title: "defaults-test", AssetType: 1},
			})
		if err != nil {
			t.Fatalf("CreateAsset: %v", err)
		}
		created, ok := resp.(openapi.CreateAsset201JSONResponse)
		if !ok {
			t.Fatalf("CreateAsset returned %T, want 201", resp)
		}
		id := uuid.UUID(openapi.Asset(created).Id)
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, id)
			_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = $1`, id)
			_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		})
		return id
	}

	assetID := create(t)

	var (
		valueText *string
		setBy     string
	)
	if err := pool.QueryRow(ctx, `
		SELECT value_text, set_by FROM asset_field_value
		 WHERE asset_id = $1 AND field_id = $2`, assetID, fieldID).Scan(&valueText, &setBy); err != nil {
		t.Fatalf("the created asset carries no value for a field that has a default — "+
			"CreateAsset is not running the defaults pass: %v", err)
	}
	if valueText == nil || *valueText != "greybox" {
		t.Errorf("value_text = %v, want greybox", valueText)
	}
	if setBy != "default" {
		t.Errorf("set_by = %q, want \"default\" — the provenance is what lets extraction "+
			"improve on this later without also overwriting what a person chose", setBy)
	}

	// The write is inside CreateAsset's transaction, so a committed
	// asset always has its defaults. Asserting it via the history row
	// rather than by racing the commit: history is written in the same
	// tx as the value, so its presence proves both landed together.
	var historyRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM asset_field_value_history
		 WHERE asset_id = $1 AND field_id = $2 AND set_by = 'default'`,
		assetID, fieldID).Scan(&historyRows); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if historyRows != 1 {
		t.Errorf("history rows = %d, want 1 — a value with no history entry is a value "+
			"with no story, and an operator asking where it came from gets nothing", historyRows)
	}

	// Clearing the default stops it applying to the NEXT asset. The
	// column is nullable and NULL means "no default"; a field that
	// keeps defaulting after an operator removed the default is the
	// most confusing possible outcome.
	if _, err := pool.Exec(ctx,
		`UPDATE field_definition SET default_value = NULL WHERE id = $1`, fieldID); err != nil {
		t.Fatalf("clear default: %v", err)
	}
	next := create(t)
	var after int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		next, fieldID).Scan(&after); err != nil {
		t.Fatalf("read next: %v", err)
	}
	if after != 0 {
		t.Errorf("a cleared default still applied to a new asset (%d rows)", after)
	}
}
