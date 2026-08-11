// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #883 — a COUNT is an oracle.
//
// The visible-fields half of "membership never widens" is asserted on
// the payloads (posts/collections member_allowlist_test.go). This is the
// half that leaks without showing a field at all: if the number of
// results a caller gets changes because a non-visible item exists, the
// caller can confirm the item — and, one token at a time, its title —
// without ever being shown one.
//
// THE BUG THIS PINS. `rebuild_post_search_text` folded every member
// asset's own search_text into the POST's document at weight D, filtered
// on nothing but `deleted_at IS NULL`. The post row is then matched by
// the POST predicate, which for a public post admits anonymous. So an
// anonymous caller searching a phrase that appears ONLY in a restricted
// member's title got a hit on the containing post. Migration 00034
// restricts the fold to members every caller could see standalone.
//
// The test is written as a DIFFERENCE, not as an absolute: measure the
// count, ATTACH the member, measure again, require equality. An "expect
// 0 results" assertion passes just as well when search is broken, and
// would not notice a leak that changed the count from 3 to 4.
//
// The asset EXISTS before the first measurement, deliberately. An
// authenticated caller can already find that asset by its own title —
// the authenticated EntityAsset predicate is soft-delete only (ADR 0063
// / 0064), so the asset row is a hit whether or not it is in any post.
// Seeding it first is what isolates the effect of MEMBERSHIP from the
// effect of the asset existing, which is exactly the quantity #883 is
// about. Measuring before the asset exists would attribute an
// ADR-0064-sanctioned hit to this issue and fail for the wrong reason.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	coOwner    int64 = 8832001
	coStranger int64 = 8832002
)

// coSecretPhrase appears ONLY in the restricted member's title. Nothing
// else in the fixture — and, being nonsense, nothing else in any
// developer's database — contains it, so a non-zero count is
// attributable to that one row.
const coSecretPhrase = "zarquon vexlimit"

func coPool(t *testing.T) *pgxpool.Pool {
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

func coSeedAsset(t *testing.T, pool *pgxpool.Pool, title, sensitivity string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
		VALUES ($1, $2, $3, (SELECT MIN(ref) FROM asset_types), 'active', $4, 'ready')`,
		id, title, coOwner, sensitivity); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id) })
	return id
}

func coSeedPost(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, $3, 'public')`,
		id, coOwner, "co public post"); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, id)
	})
	return id
}

// coCount runs the query and returns the total for a caller.
func coCount(t *testing.T, e *Engine, ref *int64) int {
	t.Helper()
	res, err := e.Run(context.Background(), Query{
		Text:          coSecretPhrase,
		Limit:         50,
		CallerUserRef: ref,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return res.TotalCount
}

// coPostIsHit reports whether the given post is among the caller's hits.
func coPostIsHit(t *testing.T, e *Engine, ref *int64, postID uuid.UUID) bool {
	t.Helper()
	res, err := e.Run(context.Background(), Query{
		Text:          coSecretPhrase,
		Limit:         50,
		CallerUserRef: ref,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, h := range res.Hits {
		if h.Type == HitTypePost && h.ID == postID {
			return true
		}
	}
	return false
}

// TestSearchCount_RestrictedMemberIsNotAnOracle is the acceptance check.
func TestSearchCount_RestrictedMemberIsNotAnOracle(t *testing.T) {
	pool := coPool(t)
	e := NewEngine(pool)

	postID := coSeedPost(t, pool)
	strangerRef := coStranger

	// The asset exists but is attached to NOTHING — see the header for
	// why the baseline is taken here rather than before it exists.
	restricted := coSeedAsset(t, pool, "Project "+coSecretPhrase+" boss", "restricted")
	anonBefore := coCount(t, e, nil)
	strangerBefore := coCount(t, e, &strangerRef)

	// Attach it to the PUBLIC post. The post_assets trigger rebuilds the
	// post document; the only thing that changed is membership.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, 0)`,
		postID, restricted); err != nil {
		t.Fatalf("attach member: %v", err)
	}

	if got := coCount(t, e, nil); got != anonBefore {
		t.Errorf("anonymous result total changed %d -> %d when a RESTRICTED member was "+
			"added to a public post. The count is an oracle: querying a phrase that "+
			"appears only in that member's title confirms the item exists, and a "+
			"stranger can walk the title token by token without ever being shown a field.",
			anonBefore, got)
	}
	if got := coCount(t, e, &strangerRef); got != strangerBefore {
		t.Errorf("authenticated stranger result total changed %d -> %d for the same reason",
			strangerBefore, got)
	}
}

// TestSearchCount_PublicMemberStillCounts is the control. Without it the
// test above passes on a build where post documents carry no member text
// at all, which would be a silent search regression rather than a fix.
func TestSearchCount_PublicMemberStillCounts(t *testing.T) {
	pool := coPool(t)
	e := NewEngine(pool)

	postID := coSeedPost(t, pool)
	pub := coSeedAsset(t, pool, "Project "+coSecretPhrase+" splash", "public")
	before := coCount(t, e, nil) // the asset alone is already a hit

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, 0)`,
		postID, pub); err != nil {
		t.Fatalf("attach member: %v", err)
	}

	after := coCount(t, e, nil)
	if after <= before {
		t.Fatalf("total %d -> %d: a PUBLIC member's words must still reach the containing "+
			"post's search document, or the #883 filter has turned member search off "+
			"entirely rather than gating it", before, after)
	}
}

// TestSearchCount_SensitivityChangeRebuildsTheDocument covers the half a
// filter alone does not fix. Nothing rebuilt a post's document when a
// MEMBER asset changed — the baseline triggers fire on post_assets,
// post_tags and the post's own title/description, never on the assets
// row — so an asset flipped public -> restricted would leave its words
// in the post document indefinitely. Migration 00034 adds the trigger.
//
// This one asserts on the POST hit rather than on the total, because
// restricting the asset also removes the ASSET's own hit for an
// anonymous caller (its row predicate demands sensitivity='public'), and
// a raw total would move for two reasons at once.
func TestSearchCount_SensitivityChangeRebuildsTheDocument(t *testing.T) {
	pool := coPool(t)
	e := NewEngine(pool)

	postID := coSeedPost(t, pool)
	asset := coSeedAsset(t, pool, "Project "+coSecretPhrase+" reveal", "public")
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, 0)`,
		postID, asset); err != nil {
		t.Fatalf("attach member: %v", err)
	}
	if !coPostIsHit(t, e, nil, postID) {
		t.Fatal("precondition: a public member's words should make the post findable")
	}

	// The NDA lands late. ADR 0020's scheduled actions do exactly this.
	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET sensitivity = 'restricted' WHERE id = $1`, asset); err != nil {
		t.Fatalf("restrict: %v", err)
	}

	if coPostIsHit(t, e, nil, postID) {
		t.Error("the post is still findable by a phrase that only appears in a member " +
			"that was restricted AFTER it was attached. Filtering the aggregation is " +
			"only worth as much as its refresh — nothing rebuilt post documents when a " +
			"member asset changed until migration 00034 added the trigger.")
	}
}
