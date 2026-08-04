// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #883 — the collection-member allow-list, asserted on the SERIALIZED
// response. Twin of posts/member_allowlist_test.go; read that file's
// header for why this is an allow-list and why it marshals rather than
// reading struct fields.
//
// The collection path also carries the two assertions the post path
// cannot make: that a soft-deleted member is absent ENTIRELY (the one
// conjunct visibility.MemberReadable deliberately leaves to SQL), and
// that owner_display_name is actually populated when the owner has a
// resolvable name — the post fixture has no `user` row, so it only ever
// exercises the absent case.
//
// Skips without AA_DB_PASSWORD.

package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	maOwner    int64 = 8831001
	maStranger int64 = 8831002
)

// maAllowList is the COMPLETE set of JSON keys a restricted collection
// member may carry: the `collection_resources` row's own columns, the
// marker, and the owner's display name. Nothing from `assets`.
var maAllowList = map[string]bool{
	"collection_id":      true,
	"asset_id":           true,
	"sort_order":         true,
	"pinned":             true,
	"expires_at":         true,
	"added_at":           true,
	"restricted":         true,
	"owner_display_name": true,
}

func maPool(t *testing.T) *pgxpool.Pool {
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
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// maSeedOwner creates a real user + profile so the display-name
// projection has something to resolve. Returns the display name.
func maSeedOwner(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	const display = "Rin Kobayashi"
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username) VALUES ($1, $2)
		 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
		maOwner, "ma-owner-883"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_profiles (user_ref, display_name) VALUES ($1, $2)
		 ON CONFLICT (user_ref) DO UPDATE SET display_name = EXCLUDED.display_name`,
		maOwner, display); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_profiles WHERE user_ref = $1`, maOwner)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, maOwner)
	})
	return display
}

// maSeedAsset plants an asset owned by maOwner with every field a leak
// could travel through populated.
func maSeedAsset(t *testing.T, pool *pgxpool.Pool, title, sensitivity string) uuid.UUID {
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
	if _, err := pool.Exec(ctx,
		`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1, 'col', 1)
		 ON CONFLICT (object_hash, variant_key) DO NOTHING`, hash); err != nil {
		t.Fatalf("seed variant: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status, file_hash, file_extension,
		                    file_size_bytes, thumbhash)
		VALUES ($1, $2, $3, $4, (SELECT MIN(ref) FROM asset_types), 'active', $5, 'ready',
		        $6, 'psd', 987654, $7)`,
		id, title, "UNRELEASED — do not distribute", maOwner, sensitivity, hash,
		[]byte{0xde, 0xad, 0xbe, 0xef}); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id) })
	return id
}

func maSeedCollection(t *testing.T, pool *pgxpool.Pool, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO collections (id, name, owner_user_ref, visibility) VALUES ($1, $2, $3, 'public')`,
		id, "ma public collection", maOwner); err != nil {
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

func maList(t *testing.T, pool *pgxpool.Pool, colID uuid.UUID, caller visibility.Caller) []openapi.CollectionResource {
	t.Helper()
	rows, err := ListCollectionResourcesPageGated(context.Background(), pool, caller, nil,
		ListCollectionResourcesPageGatedParams{
			CollectionID: pgtype.UUID{Bytes: colID, Valid: true},
			RowLimit:     50,
		})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := make([]openapi.CollectionResource, 0, len(rows))
	for _, r := range rows {
		out = append(out, resourceRowToAPI(r))
	}
	return out
}

func maKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func maFind(t *testing.T, items []openapi.CollectionResource, id uuid.UUID) openapi.CollectionResource {
	t.Helper()
	for _, it := range items {
		if uuid.UUID(it.AssetId) == id {
			return it
		}
	}
	t.Fatalf("asset %v is not in the contents list — a restricted member must be PRESENT "+
		"as a placeholder, never dropped (#883)", id)
	return openapi.CollectionResource{}
}

// TestCollectionMember_RestrictedIsAllowListed is the core assertion.
func TestCollectionMember_RestrictedIsAllowListed(t *testing.T) {
	pool := maPool(t)
	display := maSeedOwner(t, pool)

	restricted := maSeedAsset(t, pool, "SECRET boss concept", "restricted")
	pub := maSeedAsset(t, pool, "Public splash art", "public")
	colID := maSeedCollection(t, pool, restricted, pub)

	strangerRef := maStranger
	for _, c := range []struct {
		name   string
		caller visibility.Caller
	}{
		{"anonymous", visibility.NewCaller(nil)},
		{"authenticated stranger", visibility.NewCaller(&strangerRef)},
	} {
		items := maList(t, pool, colID, c.caller)
		if len(items) != 2 {
			t.Fatalf("%s: got %d members, want 2 — the restricted one must be a "+
				"placeholder in the list, not filtered out of it", c.name, len(items))
		}

		got := maFind(t, items, restricted)
		if !got.Restricted {
			t.Fatalf("%s: restricted member did not report restricted=true", c.name)
		}
		for _, k := range maKeys(t, got) {
			if !maAllowList[k] {
				raw, _ := json.Marshal(got)
				t.Errorf("%s: key %q is NOT on the #883 allow-list. Full payload: %s\n"+
					"If it belongs on a placeholder, add it to maAllowList with a reason; "+
					"otherwise stop sending it.", c.name, k, raw)
			}
		}
		if got.OwnerDisplayName == nil {
			t.Errorf("%s: the owner's display name is the ONE thing the placeholder is "+
				"supposed to carry, and it is absent", c.name)
		} else if *got.OwnerDisplayName != display {
			t.Errorf("%s: owner_display_name = %q, want %q", c.name, *got.OwnerDisplayName, display)
		}

		// The public member is untouched — a redaction that swallowed
		// everything would pass the loop above.
		other := maFind(t, items, pub)
		if other.Restricted {
			t.Errorf("%s: a PUBLIC member was marked restricted", c.name)
		}
		for _, field := range []struct {
			name string
			ok   bool
		}{
			{"title", other.Title != nil && *other.Title == "Public splash art"},
			{"asset_type", other.AssetType != nil},
			{"status", other.Status != nil},
			{"file_hash", other.FileHash != nil},
			{"file_extension", other.FileExtension != nil},
			{"thumbhash", other.Thumbhash != nil},
			{"preview_available", other.PreviewAvailable != nil && *other.PreviewAvailable},
			{"ladder_available", other.LadderAvailable != nil},
			{"scrub_available", other.ScrubAvailable != nil},
			{"asset_created_at", other.AssetCreatedAt != nil},
		} {
			if !field.ok {
				t.Errorf("%s: a readable member lost %s — those fields became `omitempty` "+
					"pointers so the PLACEHOLDER could omit them, and #595's argument for "+
					"sending them unconditionally on every other row is unchanged",
					c.name, field.name)
			}
		}
	}
}

// TestCollectionMember_OwnerSeesEverything — the rule must not cost the
// owner their own collection.
func TestCollectionMember_OwnerSeesEverything(t *testing.T) {
	pool := maPool(t)
	maSeedOwner(t, pool)
	restricted := maSeedAsset(t, pool, "SECRET boss concept", "restricted")
	colID := maSeedCollection(t, pool, restricted)

	ownerRef := maOwner
	items := maList(t, pool, colID, visibility.NewCaller(&ownerRef))
	got := maFind(t, items, restricted)
	if got.Restricted {
		t.Fatal("the OWNER was shown a placeholder for their own restricted asset")
	}
	if got.Title == nil || *got.Title != "SECRET boss concept" {
		t.Fatal("the owner lost the title of their own restricted member")
	}
	if got.PreviewAvailable == nil || !*got.PreviewAvailable {
		t.Error("the owner's restricted member reported preview_available=false")
	}
}

// TestCollectionMember_SoftDeletedIsAbsentNotPlaceholdered pins the one
// conjunct visibility.MemberReadable deliberately does NOT decide.
//
// A restriction and a deletion are different facts and must not look the
// same: a deleted asset is gone, and announcing "something was here" for
// it would both be untrue and leak that a row once existed. The SQL owns
// this, which is why MemberReadable has no Deleted field — see its doc.
func TestCollectionMember_SoftDeletedIsAbsentNotPlaceholdered(t *testing.T) {
	pool := maPool(t)
	maSeedOwner(t, pool)
	gone := maSeedAsset(t, pool, "Deleted work", "public")
	kept := maSeedAsset(t, pool, "Live work", "public")
	colID := maSeedCollection(t, pool, gone, kept)

	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = NOW() WHERE id = $1`, gone); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	ownerRef := maOwner
	for _, c := range []struct {
		name   string
		caller visibility.Caller
	}{
		{"anonymous", visibility.NewCaller(nil)},
		{"owner", visibility.NewCaller(&ownerRef)},
	} {
		items := maList(t, pool, colID, c.caller)
		for _, it := range items {
			if uuid.UUID(it.AssetId) == gone {
				t.Errorf("%s: a soft-deleted member came back (restricted=%v) — "+
					"deleted is ABSENT, not a placeholder", c.name, it.Restricted)
			}
		}
		if len(items) != 1 {
			t.Errorf("%s: got %d members, want 1 (the live one)", c.name, len(items))
		}
	}
}
