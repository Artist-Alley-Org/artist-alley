// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25b: a saved `!lastN` stores the canonical `last:N`,
// replays as the N newest eligible ASSETS under the executor's existing
// asset-only contract, and the window beside a similarity anchor is
// refused before persistence on create and on patch.
//
// Reuses sprint25a_test.go's rig. On `dev` at d54714bb `!last3` is an
// unknown verb, so the accepted create below goes red there.
//
// Skips without AA_DB_PASSWORD.

package saved_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// ssRecentBase dates fixture rows two days ahead so they are the newest
// rows in the database whatever a sibling test inserts at NOW().
func ssRecentBase() time.Time { return time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond) }

func ssRecentAsset(t *testing.T, pool *pgxpool.Pool, label string, at time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status, file_extension, created_at)
		VALUES ($1,$2,'fixture body',$3,(SELECT MIN(ref) FROM asset_types),'active','public','ready','png',$4)`,
		id, ssPhrase+" "+label, ssOwner, at); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { testdb.Purge(t, pool, id, `DELETE FROM assets WHERE id = $1`) })
	return id
}

func ssRecentPost(t *testing.T, pool *pgxpool.Pool, label string, at time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO posts (id, author_user_ref, title, description, visibility, created_at, posted_at)
		VALUES ($1, $2, $3, $3, 'public', $4, $4)`, id, ssOwner, ssPhrase+" "+label, at); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() { testdb.Purge(t, pool, id, `DELETE FROM posts WHERE id = $1`) })
	return id
}

// TestSavedSearch_LastSavesCanonicalAndReplaysAssetOnly: witness K. A
// post newer than every asset does not take a slot, because the
// executor requests assets and the window is formed over the requested
// types.
func TestSavedSearch_LastSavesCanonicalAndReplaysAssetOnly(t *testing.T) {
	pool := ssPool(t)
	ssSeed(t, pool)
	rig := ssNewRig(t, pool)
	b := ssRecentBase()
	newest := ssRecentAsset(t, pool, "newest", b.Add(3*time.Second))
	middle := ssRecentAsset(t, pool, "middle", b.Add(2*time.Second))
	older := ssRecentAsset(t, pool, "older", b.Add(1*time.Second))
	ssRecentAsset(t, pool, "oldest", b)
	ssRecentPost(t, pool, "newer than every asset", b.Add(10*time.Second))

	for i, in := range []string{"!last3", "last:3", ssPhrase + " AND !last3", "!last03"} {
		name := "last-" + string(rune('a'+i))
		status, body := rig.create(name, in, nil)
		if status != http.StatusCreated {
			t.Fatalf("create %q → %d %v", in, status, body)
		}
		stored := rig.stored(name)
		if strings.Contains(stored, "!") || !strings.Contains(stored, "last:3") {
			t.Errorf("create %q stored %q; the column must hold the canonical last:3", in, stored)
		}
		got := rig.replay(body["id"].(string))
		want := map[uuid.UUID]bool{newest: true, middle: true, older: true}
		if len(got) != 3 {
			t.Errorf("replay of %q returned %d ids %v, want the three newest assets", in, len(got), got)
		}
		for _, id := range got {
			if !want[id] {
				t.Errorf("replay of %q returned %v, which is not one of the three newest assets (a newer post must not take a slot)", in, id)
			}
		}
	}
	// And through the `filters` half of the body, the same column.
	status, body := rig.create("last-filter", ssPhrase, []string{"last:2"})
	if status != http.StatusCreated {
		t.Fatalf("create with filters → %d %v", status, body)
	}
	if stored := rig.stored("last-filter"); stored != "("+ssPhrase+") AND last:2" {
		t.Errorf("stored %q", stored)
	}
	if got := rig.replay(body["id"].(string)); len(got) != 2 {
		t.Errorf("last:2 replayed %d ids", len(got))
	}
}

// TestSavedSearch_LastWithSimilarityIsRefusedBeforePersistence: witness
// J's saved half. Create writes no row; patch leaves the row unchanged.
func TestSavedSearch_LastWithSimilarityIsRefusedBeforePersistence(t *testing.T) {
	pool := ssPool(t)
	f := ssSeed(t, pool)
	rig := ssNewRig(t, pool)
	anchor := f.noCol.String()
	var contract map[string]any
	for _, c := range []struct {
		name    string
		dsl     string
		filters []string
	}{
		{"typed", "last:5 AND similar_to:" + anchor, nil},
		{"alias", "!last5 AND similar_to:" + anchor, nil},
		{"split across dsl and filters", "similar_to:" + anchor, []string{"last:5"}},
		{"two windows", "last:3 AND last:5", nil},
		{"placement", "NOT !last3", nil},
		{"range", "!last10001", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := rig.rows()
			status, body := rig.create("rej-"+c.name, c.dsl, c.filters)
			if status != http.StatusBadRequest || body["error"] != "dsl_error" {
				t.Errorf("→ %d %v, want 400 dsl_error", status, body)
			}
			if after := rig.rows(); after != before {
				t.Errorf("a saved_search row was WRITTEN for a query execution rejects (%d → %d rows)", before, after)
			}
			if strings.Contains(c.dsl, "similar_to") {
				got := map[string]any{"kind": body["kind"], "message": body["message"]}
				if contract == nil {
					contract = got
				} else if got["kind"] != contract["kind"] || got["message"] != contract["message"] {
					t.Errorf("contract %v differs from %v", got, contract)
				}
			}
		})
	}

	status, body := rig.create("patched-25b", ssPhrase, nil)
	if status != http.StatusCreated {
		t.Fatalf("create → %d %v", status, body)
	}
	id := body["id"].(string)
	original := rig.stored("patched-25b")
	for _, bad := range []string{"last:5 AND similar_to:" + anchor, "!last5 AND similar_to:" + anchor, "last:3 AND last:5", "!last0"} {
		status, body := rig.do(http.MethodPatch, "/search/saved/"+id, map[string]any{"dsl": bad})
		if status != http.StatusBadRequest || body["error"] != "dsl_error" {
			t.Errorf("patch %q → %d %v, want 400 dsl_error", bad, status, body)
		}
		if now := rig.stored("patched-25b"); now != original {
			t.Errorf("patch %q CHANGED the stored row: %q → %q", bad, original, now)
		}
	}
	// An accepted patch stores the canonical form.
	status, body = rig.do(http.MethodPatch, "/search/saved/"+id, map[string]any{"dsl": ssPhrase + " AND !last4"})
	if status != http.StatusOK {
		t.Fatalf("patch → %d %v", status, body)
	}
	if now := rig.stored("patched-25b"); now != ssPhrase+" AND last:4" {
		t.Errorf("accepted patch stored %q, want the canonical %q", now, ssPhrase+" AND last:4")
	}
}
