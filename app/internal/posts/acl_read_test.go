// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #667 — an explicit share actually grants read.
//
// `POST /posts/{id}/acls` wrote a post_acls row that no read path ever
// consulted, so a share was a button that stored a row and changed
// nothing. readRule.sql now carries a post_acls disjunct, and because
// that one fragment feeds both the list paths and postReadable, the
// grant lands on both at once.
//
// ADR 0010 L6 is the spec, and one sentence in it is the acceptance
// criterion: "ACLs grant *additional* access beyond those defaults.
// They never restrict below them." So the load-bearing test here is NOT
// "a grantee can read the post" — that is the easy half. It is
// TestPostReadRule_ACLIsPurelyAdditive: the full caller × tier matrix
// with NO acl rows in play, pinned to exactly what the rule answered
// before the disjunct existed. If adding the branch narrowed anything
// for anyone, that test says which cell.
//
// The matrix is evaluated through BOTH the list path
// (ListPostsPageGated) and the single-item gate (postReadable) on every
// cell, so "the two paths agree" is asserted everywhere rather than
// only on the happy path.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// Synthetic refs, disjoint from the lv* set in list_visibility_test.go.
// posts.author_user_ref, user_follows and post_acls.principal_id all
// carry no FK to the user table, so these need no rows anywhere.
const (
	aclAuthor    int64 = 6670001 // owns one post per tier
	aclStranger  int64 = 6670002 // signed in, no relationship, no caps
	aclFollower  int64 = 6670003 // follows aclAuthor
	aclModerator int64 = 6670004 // posts.admin
	aclGrantee   int64 = 6670005 // holds the post_acls grant
)

// granteePrincipal is aclGrantee as post_acls stores it. Derived rather
// than written out, because a hand-typed principal_id that no longer
// matches the ref would make every grant in this file match nothing —
// which is indistinguishable from the bug under test.
var granteePrincipal = strconv.FormatInt(aclGrantee, 10)

// aclTiers is every tier a post row may hold. Unlike the constraint-read
// in list_visibility_test.go this list is explicit, because each entry
// here is paired with a hand-written expected answer per caller — a tier
// appearing without someone deciding what it should do is exactly the
// review this matrix wants to force. postVisibilityTiers guards the
// other direction: aclTiersCoverConstraint below fails if the database
// grows a tier this file has not been told about.
var aclTiers = []string{"public", "org-only", "followers", "private", "explicit-share"}

func TestACLTiersCoverConstraint(t *testing.T) {
	pool := previewPool(t)
	have := map[string]bool{}
	for _, v := range aclTiers {
		have[v] = true
	}
	for _, v := range postVisibilityTiers(t, pool) {
		if !have[v] {
			t.Errorf("posts_visibility_check admits tier %q that acl_read_test.go's matrix does not cover", v)
		}
	}
}

// aclSeedTiers plants one post per tier owned by aclAuthor and returns
// tier -> id.
func aclSeedTiers(t *testing.T, pool *pgxpool.Pool) map[string]uuid.UUID {
	t.Helper()
	out := make(map[string]uuid.UUID, len(aclTiers))
	for _, v := range aclTiers {
		out[v] = seedTierPost(t, pool, aclAuthor, v)
	}
	return out
}

// aclSeedFollow makes aclFollower follow aclAuthor for the test's life.
func aclSeedFollow(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO user_follows (follower_user_ref, followee_user_ref) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, aclFollower, aclAuthor); err != nil {
		t.Fatalf("seed follow: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM user_follows WHERE follower_user_ref=$1`, aclFollower)
	})
}

// aclGrant writes one post_acls row and removes it afterwards.
// expiresAt nil means "never expires".
func aclGrant(
	t *testing.T,
	pool *pgxpool.Pool,
	postID uuid.UUID,
	principalType, principalID string,
	expiresAt *time.Time,
) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO post_acls (post_id, principal_type, principal_id, permission, expires_at)
		 VALUES ($1,$2,$3,'read',$4)`,
		postID, principalType, principalID, expiresAt); err != nil {
		t.Fatalf("grant %s/%s on %s: %v", principalType, principalID, postID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM post_acls WHERE post_id=$1 AND principal_type=$2 AND principal_id=$3`,
			postID, principalType, principalID)
	})
}

// aclCaller names one caller class. A nil identity is the anonymous
// branch of the rule — reachable in production through /posts/by-asset,
// the one posts route on the public-mode allowlist.
type aclCaller struct {
	name string
	id   *auth.Identity
}

func aclCallers() []aclCaller {
	return []aclCaller{
		{"anonymous", nil},
		{"author", lvIdentity(aclAuthor)},
		{"stranger", lvIdentity(aclStranger)},
		{"follower", lvIdentity(aclFollower)},
		{"admin", lvIdentity(aclModerator, CapPostsAdmin)},
		{"grantee", lvIdentity(aclGrantee)},
	}
}

// aclListedByAuthor runs the shared list path for one caller, narrowed
// to aclAuthor's posts, and returns the set of ids handed out. Using
// ListPostsPageGated directly rather than the HTTP handler is what lets
// the anonymous branch into the matrix: ListPosts requires a session,
// but the fragment it splices does not, and /posts/by-asset reaches it.
func aclListedByAuthor(t *testing.T, h *Handler, id *auth.Identity) map[uuid.UUID]bool {
	t.Helper()
	author := aclAuthor
	rows, err := h.ListPostsPageGated(t.Context(), id, ListPostsPageParams{
		AuthorUserRef: &author,
		RowLimit:      200,
	})
	if err != nil {
		t.Fatalf("ListPostsPageGated: %v", err)
	}
	got := make(map[uuid.UUID]bool, len(rows))
	for _, r := range rows {
		got[uuid.UUID(r.ID.Bytes)] = true
	}
	return got
}

// aclAssertMatrix checks both read paths against `want[caller][tier]`
// and reports every disagreeing cell. Returning nothing and failing
// per-cell is deliberate: a narrowing regression usually hits several
// cells, and the useful output is all of them at once.
func aclAssertMatrix(
	t *testing.T,
	h *Handler,
	ids map[string]uuid.UUID,
	want map[string]map[string]bool,
) {
	t.Helper()
	for _, c := range aclCallers() {
		w, described := want[c.name]
		if !described {
			continue
		}
		listed := aclListedByAuthor(t, h, c.id)
		for _, tier := range aclTiers {
			postID, seeded := ids[tier]
			if !seeded {
				continue
			}
			// A missing key would read as `false` and quietly assert
			// "hidden" — the cheapest way for a typo to turn a cell
			// of this matrix into a tautology.
			expect, stated := w[tier]
			if !stated {
				t.Fatalf("caller %s has no expectation for tier %s — the matrix is incomplete", c.name, tier)
			}

			if got := listed[postID]; got != expect {
				t.Errorf("LIST: caller %s, tier %s: visible=%v want %v", c.name, tier, got, expect)
			}
			gate, err := h.postReadable(t.Context(), c.id, postID)
			if err != nil {
				t.Fatalf("postReadable(%s, %s): %v", c.name, tier, err)
			}
			if gate != expect {
				t.Errorf("GATE: caller %s, tier %s: readable=%v want %v", c.name, tier, gate, expect)
			}
			// The #660 invariant, restated per cell: whatever the
			// two paths answer, they must answer the same thing.
			// Asserted separately from `expect` so a wrong-but-
			// consistent rule and a split-brain rule fail with
			// different messages.
			if listed[postID] != gate {
				t.Errorf("SPLIT: caller %s, tier %s: list=%v but gate=%v — the paths disagree",
					c.name, tier, listed[postID], gate)
			}
		}
	}
}

// TestPostReadRule_ACLIsPurelyAdditive is the invariant that matters.
//
// No post_acls rows exist anywhere in this fixture, so every cell below
// is the answer the rule gave BEFORE the ACL disjunct was added. If the
// new branch narrowed any existing outcome — a bad join, a NOT EXISTS,
// an accidental AND — this fails and names the cell.
func TestPostReadRule_ACLIsPurelyAdditive(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	ids := aclSeedTiers(t, pool)
	aclSeedFollow(t, pool)

	aclAssertMatrix(t, h, ids, map[string]map[string]bool{
		// Anonymous gets the public tier and nothing else. No author
		// comparison is made for them at all, so `private` stays shut
		// even though a synthetic ref could in principle be 0.
		"anonymous": {
			"public": true, "org-only": false, "followers": false,
			"private": false, "explicit-share": false,
		},
		// The author sees every one of their own tiers.
		"author": {
			"public": true, "org-only": true, "followers": true,
			"private": true, "explicit-share": true,
		},
		// Signed in, no relationship: the walled-garden floor.
		"stranger": {
			"public": true, "org-only": true, "followers": false,
			"private": false, "explicit-share": false,
		},
		// Following opens `followers` and nothing else.
		"follower": {
			"public": true, "org-only": true, "followers": true,
			"private": false, "explicit-share": false,
		},
		// posts.admin opens `private`. It does NOT open
		// explicit-share — a moderator bypass is not a grant, and
		// #667 must not quietly turn it into one.
		"admin": {
			"public": true, "org-only": true, "followers": false,
			"private": true, "explicit-share": false,
		},
		// aclGrantee holds no row in this fixture, so they are just
		// another stranger. This row is what makes "the grant did it"
		// provable in the next test rather than assumed.
		"grantee": {
			"public": true, "org-only": true, "followers": false,
			"private": false, "explicit-share": false,
		},
	})
}

// TestPostReadRule_GrantOpensTiersAndOnlyForGrantee re-runs the same
// matrix with a live grant to aclGrantee on EVERY tier.
//
// Two claims at once:
//   - the grantee's row flips to all-true (the feature works, on both
//     paths, for every tier — ADR 0010 L6 grants are additive, not
//     scoped to `explicit-share`), and
//   - every other row is byte-identical to the additive test above (the
//     grant reaches the grantee and nobody else).
func TestPostReadRule_GrantOpensTiersAndOnlyForGrantee(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	ids := aclSeedTiers(t, pool)
	aclSeedFollow(t, pool)
	for _, tier := range aclTiers {
		aclGrant(t, pool, ids[tier], "user", granteePrincipal, nil)
	}

	aclAssertMatrix(t, h, ids, map[string]map[string]bool{
		"anonymous": {
			// Unchanged: an anonymous caller has no principal, so no
			// grant can name them. The anonymous branch binds no arg
			// and carries no ACL disjunct at all.
			"public": true, "org-only": false, "followers": false,
			"private": false, "explicit-share": false,
		},
		"stranger": {
			"public": true, "org-only": true, "followers": false,
			"private": false, "explicit-share": false,
		},
		"follower": {
			"public": true, "org-only": true, "followers": true,
			"private": false, "explicit-share": false,
		},
		"admin": {
			"public": true, "org-only": true, "followers": false,
			"private": true, "explicit-share": false,
		},
		"grantee": {
			"public": true, "org-only": true, "followers": true,
			"private": true, "explicit-share": true,
		},
	})
}

// TestPostReadRule_GrantExpiry pins expiry in both directions, in one
// fixture.
//
// Both directions in the SAME test on purpose: an "expired grant does
// not grant" assertion passes just as happily when the disjunct is
// missing entirely, when principal_id never matches, or when the whole
// feature is reverted. Pairing it with a future-dated grant on an
// identical post — same tier, same author, same principal, differing
// only in expires_at — makes the pair distinguish "expiry works" from
// "nothing matched".
func TestPostReadRule_GrantExpiry(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	past := seedTierPost(t, pool, aclAuthor, "explicit-share")
	future := seedTierPost(t, pool, aclAuthor, "explicit-share")
	never := seedTierPost(t, pool, aclAuthor, "explicit-share")

	yesterday := time.Now().Add(-24 * time.Hour)
	tomorrow := time.Now().Add(24 * time.Hour)
	aclGrant(t, pool, past, "user", granteePrincipal, &yesterday)
	aclGrant(t, pool, future, "user", granteePrincipal, &tomorrow)
	aclGrant(t, pool, never, "user", granteePrincipal, nil)

	id := lvIdentity(aclGrantee)
	listed := aclListedByAuthor(t, h, id)

	for _, tc := range []struct {
		label  string
		postID uuid.UUID
		want   bool
	}{
		{"expires_at in the past", past, false},
		{"expires_at in the future", future, true},
		{"expires_at NULL", never, true},
	} {
		if got := listed[tc.postID]; got != tc.want {
			t.Errorf("LIST: grant with %s: visible=%v want %v", tc.label, got, tc.want)
		}
		gate, err := h.postReadable(t.Context(), id, tc.postID)
		if err != nil {
			t.Fatalf("postReadable(%s): %v", tc.label, err)
		}
		if gate != tc.want {
			t.Errorf("GATE: grant with %s: readable=%v want %v", tc.label, gate, tc.want)
		}
	}
}

// TestPostReadRule_NonUserPrincipalsDoNotGrant fixes the scope line.
//
// post_acls admits 'role' and 'team' principals and ADR 0010 specs them,
// but resolving either needs Layer 5 (the caller's role set, the team
// closure) threaded through this fragment — deliberately not shipped
// here, exactly as collection_acls has not shipped it. The risk in a
// polymorphic principal table is a disjunct that forgets to constrain
// principal_type and matches a role id that happens to equal a user ref
// as text; this asserts it does not.
func TestPostReadRule_NonUserPrincipalsDoNotGrant(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	postID := seedTierPost(t, pool, aclAuthor, "explicit-share")
	aclGrant(t, pool, postID, "role", granteePrincipal, nil)
	aclGrant(t, pool, postID, "team", granteePrincipal, nil)

	id := lvIdentity(aclGrantee)
	if aclListedByAuthor(t, h, id)[postID] {
		t.Error("LIST: a role/team grant admitted a user principal — principal_type is not being constrained")
	}
	gate, err := h.postReadable(t.Context(), id, postID)
	if err != nil {
		t.Fatalf("postReadable: %v", err)
	}
	if gate {
		t.Error("GATE: a role/team grant admitted a user principal — principal_type is not being constrained")
	}
}

// TestPostAcl_GranteeReachesBothHTTPPaths drives the real handlers, not
// the internal helpers: the grantee must find the post through
// `GET /posts?visibility=explicit-share` AND fetch it through
// `GET /posts/{id}`. The two share readRule.sql, so a disagreement here
// means a splice site was missed rather than that the rule is wrong.
func TestPostAcl_GranteeReachesBothHTTPPaths(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	shared := seedTierPost(t, pool, aclAuthor, "explicit-share")
	// A second explicit-share post with no grant on it, so "the list
	// returned something" cannot be satisfied by the tier filter alone.
	unshared := seedTierPost(t, pool, aclAuthor, "explicit-share")
	aclGrant(t, pool, shared, "user", granteePrincipal, nil)

	id := lvIdentity(aclGrantee)
	tier := "explicit-share"

	var sawShared, sawUnshared bool
	var sharedItem openapi.Post
	for _, p := range lvListed(t, h, id, &tier) {
		switch uuid.UUID(p.Id) {
		case shared:
			sawShared, sharedItem = true, p
		case unshared:
			sawUnshared = true
		}
	}
	if !sawShared {
		t.Fatal("GET /posts?visibility=explicit-share omitted the post the caller was granted — the share still grants nothing")
	}
	if sawUnshared {
		t.Error("GET /posts?visibility=explicit-share returned an explicit-share post with NO grant — the disjunct is matching too much")
	}
	if !lvGetOK(t, h, id, sharedItem) {
		t.Error("GET /posts/{id} refused a post the list handed out — the ACL disjunct reached the list path only")
	}
	if lvGetOK(t, h, id, openapi.Post{Id: openapi_types.UUID(unshared)}) {
		t.Error("GET /posts/{id} returned an explicit-share post with NO grant")
	}
}
