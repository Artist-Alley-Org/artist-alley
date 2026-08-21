// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Regression for the metaAssetAdapter.GetAssetRef federation-path bug:
// the query SELECTed a nonexistent `owning_team_id` column (the real
// column on `assets` is `team_id`), so every call errored with
// `column "owning_team_id" does not exist`. Standard CI never ran the
// federation dogfood that exercises this path, so it shipped unnoticed
// (<= v0.5.2) until the nightly federation run cascaded into query-cancel
// timeouts. This asserts the query executes and that team_id maps through
// to AssetRef.OwningTeamID.
//
// Real Postgres; skips without AA_DB_PASSWORD (same convention as the
// sibling integration suites in this package).

package http

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestMetaAssetAdapterGetAssetRef(t *testing.T) {
	pool := openPoolForSensitivity(t)
	t.Cleanup(pool.Close)
	ctx := t.Context()

	// Throwaway team so the asset's team_id FK (-> teams.id) is
	// satisfied and we can assert the previously-broken column carries
	// its value through the scan.
	teamID := uuid.New()
	slug := "getassetref-test-" + teamID.String()[:8]
	if _, err := pool.Exec(ctx,
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, 'GetAssetRef test')`,
		teamID, slug,
	); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, teamID)
	})

	assetID := uuid.New()
	const ownerRef int64 = 424242
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, title, asset_type, status, owner_user_ref, team_id)
		 VALUES ($1, 'getassetref-test', 1, 'active', $2, $3)`,
		assetID, ownerRef, teamID,
	); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, assetID)
	})

	adapter := metaAssetAdapter{pool: pool}

	// The core regression: this errored with
	// `column "owning_team_id" does not exist` before the fix.
	ref, found, err := adapter.GetAssetRef(ctx, assetID)
	if err != nil {
		t.Fatalf("GetAssetRef returned an error (owning_team_id column bug?): %v", err)
	}
	if !found {
		t.Fatal("GetAssetRef: found=false for a freshly-seeded asset")
	}
	if ref.OwnerUserRef == nil || *ref.OwnerUserRef != ownerRef {
		t.Errorf("OwnerUserRef = %v, want %d", ref.OwnerUserRef, ownerRef)
	}
	if ref.OwningTeamID == nil || *ref.OwningTeamID != teamID {
		t.Errorf("OwningTeamID = %v, want %s (team_id must map through)", ref.OwningTeamID, teamID)
	}

	// The not-found path stays a clean (false, nil) — not an error.
	if _, found, err := adapter.GetAssetRef(ctx, uuid.New()); err != nil || found {
		t.Errorf("GetAssetRef(unknown id) = (found=%v, err=%v), want (false, nil)", found, err)
	}
}
