// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1167 / ADR 0094 — the post-level derivation of the AI declaration.
//
// # What this file exists to catch, and why an equality test would not
//
// The derivation is ASYMMETRIC and the asymmetry is the whole design:
// a positive claim propagates on ANY, the negative claim requires ALL.
// A test that only checked "one generated member makes the post
// generated" would pass against a naive "strongest value wins" over the
// total order NULL < none < assisted < generated — and that
// implementation gets the case this feature exists for exactly
// backwards: a post of {none, undeclared} would read `none`, i.e. the
// post would disclaim AI on behalf of a maker nobody asked. That is the
// fabricated disclaimer the nullable column exists to prevent,
// reintroduced one level up.
//
// So the unanimity arm is asserted on its own, positively and
// negatively, and so is the covers arm — `posts.mature` shipped
// members-only and #1147 was the bill.
//
// Skips without AA_DB_PASSWORD.

package assets

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const aipOwner int64 = 11670101

func aipPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok && v != "" {
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

// aipAsset plants one asset with the given declaration. `decl` of ""
// means UNDECLARED (a NULL column), which is a distinct case from every
// declared value and is what half this file turns on.
func aipAsset(t *testing.T, pool *pgxpool.Pool, decl string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var v any
	if decl != "" {
		v = decl
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, description, owner_user_ref, asset_type,
		                    status, sensitivity, processing_status, ai_provenance)
		VALUES ($1, 'aip fixture', '', $2, (SELECT MIN(ref) FROM asset_types),
		        'active', 'public', 'ready', $3)`, id, aipOwner, v); err != nil {
		t.Fatalf("seed asset (%q): %v", decl, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

// aipPost plants a post with the given members, letting the triggers do
// the derivation. Returns the post id.
func aipPost(t *testing.T, pool *pgxpool.Pool, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO posts (id, author_user_ref, title, description, visibility)
		VALUES ($1, $2, 'aip fixture', '', 'public')`, id, aipOwner); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, id)
	})
	for i, m := range members {
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, $3)`,
			id, m, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	return id
}

// aipRead returns the post's stored (not recomputed) declaration.
// Reading the COLUMN rather than calling post_ai_provenance() is the
// point: the column is what every consumer sees, and a trigger that
// never fired leaves a correct function and a stale column.
func aipRead(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID) string {
	t.Helper()
	var v *string
	if err := pool.QueryRow(context.Background(),
		`SELECT ai_provenance FROM posts WHERE id = $1`, postID).Scan(&v); err != nil {
		t.Fatalf("read post: %v", err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func aipWant(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID, want, why string) {
	t.Helper()
	got := aipRead(t, pool, postID)
	if got != want {
		t.Errorf("post ai_provenance = %q, want %q — %s", disp(got), disp(want), why)
	}
}

func disp(v string) string {
	if v == "" {
		return "<undeclared>"
	}
	return v
}

// TestAIProvenance_PositiveClaimPropagatesOnAny — the arm ADR 0094
// spells out.
func TestAIProvenance_PositiveClaimPropagatesOnAny(t *testing.T) {
	pool := aipPool(t)

	t.Run("one generated member makes the post generated", func(t *testing.T) {
		p := aipPost(t, pool,
			aipAsset(t, pool, "none"),
			aipAsset(t, pool, ""),
			aipAsset(t, pool, "generated"))
		aipWant(t, pool, p, "generated",
			"a bundle containing an AI-generated piece IS a bundle containing one")
	})

	t.Run("generated outranks assisted", func(t *testing.T) {
		p := aipPost(t, pool,
			aipAsset(t, pool, "assisted"),
			aipAsset(t, pool, "generated"))
		aipWant(t, pool, p, "generated",
			"understating AI involvement is the harm to avoid, so the stronger claim wins")
	})

	t.Run("one assisted member with no generated makes the post assisted", func(t *testing.T) {
		p := aipPost(t, pool,
			aipAsset(t, pool, "none"),
			aipAsset(t, pool, "assisted"))
		aipWant(t, pool, p, "assisted", "positive claims propagate on ANY")
	})
}

// TestAIProvenance_NegativeClaimRequiresAll is the arm that a
// "strongest value over a total order" implementation FAILS, and it is
// the one this feature exists for. A naive implementation returns
// `none` for every case below.
func TestAIProvenance_NegativeClaimRequiresAll(t *testing.T) {
	pool := aipPool(t)

	t.Run("unanimous none IS none", func(t *testing.T) {
		p := aipPost(t, pool,
			aipAsset(t, pool, "none"),
			aipAsset(t, pool, "none"))
		aipWant(t, pool, p, "none",
			"every contributor declared no AI, so the post may say so")
	})

	t.Run("one undeclared member makes the WHOLE post undeclared", func(t *testing.T) {
		p := aipPost(t, pool,
			aipAsset(t, pool, "none"),
			aipAsset(t, pool, ""))
		aipWant(t, pool, p, "",
			"deriving `none` here would fabricate the undeclared maker's disclaimer at "+
				"the post level — the exact error the nullable column exists to prevent")
	})

	t.Run("all undeclared is undeclared", func(t *testing.T) {
		p := aipPost(t, pool, aipAsset(t, pool, ""), aipAsset(t, pool, ""))
		aipWant(t, pool, p, "", "nobody was asked")
	})

	t.Run("a post with no members is undeclared", func(t *testing.T) {
		p := aipPost(t, pool)
		aipWant(t, pool, p, "", "there is nothing to derive from")
	})
}

// TestAIProvenance_TracksMembershipAndDeclarationChanges — the column is
// MAINTAINED, not computed once at insert. Both directions, because a
// derivation that only ever ratchets up is a different bug from one
// that never fires.
func TestAIProvenance_TracksMembershipAndDeclarationChanges(t *testing.T) {
	pool := aipPool(t)
	ctx := context.Background()

	member := aipAsset(t, pool, "none")
	p := aipPost(t, pool, member)
	aipWant(t, pool, p, "none", "baseline")

	// The asset's own declaration changes.
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET ai_provenance = 'generated' WHERE id = $1`, member); err != nil {
		t.Fatalf("update declaration: %v", err)
	}
	aipWant(t, pool, p, "generated",
		"assets_ai_provenance_sync_trg must re-derive every post holding the asset")

	// And back down again.
	if _, err := pool.Exec(ctx,
		`UPDATE assets SET ai_provenance = NULL WHERE id = $1`, member); err != nil {
		t.Fatalf("clear declaration: %v", err)
	}
	aipWant(t, pool, p, "",
		"re-derivation must be able to REMOVE a value, not only raise one")

	// A new member joins.
	joiner := aipAsset(t, pool, "assisted")
	if _, err := pool.Exec(ctx,
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, 9)`,
		p, joiner); err != nil {
		t.Fatalf("add member: %v", err)
	}
	aipWant(t, pool, p, "assisted", "post_assets_ai_provenance_sync_trg must fire on INSERT")

	// And leaves.
	if _, err := pool.Exec(ctx,
		`DELETE FROM post_assets WHERE post_id = $1 AND asset_id = $2`, p, joiner); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	aipWant(t, pool, p, "", "…and on DELETE")
}

// TestAIProvenance_SoftDeletedMemberIsNotAContributor — a deleted member
// is not a member, following 00052/00054 and the contents listings.
func TestAIProvenance_SoftDeletedMemberIsNotAContributor(t *testing.T) {
	pool := aipPool(t)
	ctx := context.Background()

	live := aipAsset(t, pool, "none")
	doomed := aipAsset(t, pool, "generated")
	p := aipPost(t, pool, live, doomed)
	aipWant(t, pool, p, "generated", "baseline: both members live")

	if _, err := pool.Exec(ctx,
		`UPDATE assets SET deleted_at = NOW() WHERE id = $1`, doomed); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	aipWant(t, pool, p, "none",
		"a soft-deleted member is not a contributor, and the surviving one is unanimous")

	if _, err := pool.Exec(ctx,
		`UPDATE assets SET deleted_at = NULL WHERE id = $1`, doomed); err != nil {
		t.Fatalf("restore: %v", err)
	}
	aipWant(t, pool, p, "generated", "restoring it brings the value back")
}

// TestAIProvenance_CoverPicturesAreContributors is the #1147 arm.
//
// `posts.mature` derived from `post_assets` alone and a labelled COVER
// over unlabelled members computed `false` — four surfaces then painted
// that cover to a viewer who had opted out, and a cover is the FIRST
// picture a card shows. 00054 had to come back and complete the rule.
// Shipping this column with the same hole, knowing it was there, is
// what this test refuses.
func TestAIProvenance_CoverPicturesAreContributors(t *testing.T) {
	pool := aipPool(t)
	ctx := context.Background()

	member := aipAsset(t, pool, "none")
	cover := aipAsset(t, pool, "generated")
	p := aipPost(t, pool, member)
	aipWant(t, pool, p, "none", "baseline: members only, unanimous")

	// A cover is NOT a member — that is the whole point of the arm.
	if _, err := pool.Exec(ctx,
		`UPDATE posts SET cover_asset_id = $2 WHERE id = $1`, p, cover); err != nil {
		t.Fatalf("set cover: %v", err)
	}
	aipWant(t, pool, p, "generated",
		"an AI-generated COVER is AI-generated work the post shows first")

	if _, err := pool.Exec(ctx,
		`UPDATE posts SET cover_asset_id = NULL WHERE id = $1`, p); err != nil {
		t.Fatalf("clear cover: %v", err)
	}
	aipWant(t, pool, p, "none", "removing the cover re-derives from the members alone")

	// The thumbnail pointer is the other half of the same hole.
	thumb := aipAsset(t, pool, "assisted")
	if _, err := pool.Exec(ctx,
		`UPDATE posts SET cover_thumbnail_asset_id = $2 WHERE id = $1`, p, thumb); err != nil {
		t.Fatalf("set cover thumbnail: %v", err)
	}
	aipWant(t, pool, p, "assisted",
		"cover_thumbnail_asset_id is a standalone non-member too, and 00054 covers both")

	// And an UNDECLARED cover takes a unanimous post back to undeclared,
	// because the post shows a picture nobody was asked about.
	undeclaredCover := aipAsset(t, pool, "")
	if _, err := pool.Exec(ctx, `
		UPDATE posts SET cover_thumbnail_asset_id = NULL, cover_asset_id = $2
		 WHERE id = $1`, p, undeclaredCover); err != nil {
		t.Fatalf("swap cover: %v", err)
	}
	aipWant(t, pool, p, "",
		"the unanimity arm counts covers too — a post cannot disclaim AI over a "+
			"picture whose maker was never asked")
}
