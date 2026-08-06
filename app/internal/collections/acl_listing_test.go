// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #933 — a public collection stops disclosing its guest list.
//
// # What was wrong
//
// ListCollectionAcls passed on `owner OR visibility == "public" OR
// canMutate`. The middle disjunct meant ANY authenticated user could
// enumerate every ACL row on any public collection: each grantee's
// principal_id, their permission level, who granted it and when it
// expires.
//
// `visibility: public` is a statement about the collection's CONTENTS.
// It is not a statement about who the owner individually shared it with
// — that is information about the owner's working relationships, and it
// was reaching anyone with an account and no connection to the
// collection at all.
//
// #876 had already decided this exact question for posts and tightened
// ListPostAcls to write-access-only. The collection surface never got
// the same treatment. It has it now; the two rules match.
//
// # Why the "stranger still reads the collection" leg is load-bearing
//
// A "the stranger gets 403" assertion on its own is not evidence the
// disclosure closed. It passes identically if the stranger could never
// reach this collection in the first place — if the fixture is private,
// or soft-deleted, or the collection was never created. So the SAME
// caller, in the SAME test, must still get a 200 from GET
// /collections/{id}: the public disjunct that used to admit them to the
// grant list is demonstrably still admitting them to the collection.
// That pairing is the before-state. Only the guest list closed.
//
// The list is also asserted NON-EMPTY when the owner reads it, so
// "identical to a refusal" cannot be satisfied by there being nothing to
// disclose.
//
// Skips without AA_DB_PASSWORD.

package collections_test

import (
	"context"
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

// Synthetic refs, disjoint from every other set in this package.
const (
	alOwner    int64 = 9330001 // owns the collection
	alGrantee  int64 = 9330002 // holds a read grant; is the leaked datum
	alStranger int64 = 9330003 // signed in, no relationship whatsoever
	alAdmin    int64 = 9330004 // collections.admin, owns nothing
)

// alSeedCollection inserts a collection directly. Direct SQL rather than
// CreateCollection keeps the fixture independent of the activities
// writer, which this gate never touches.
func alSeedCollection(t *testing.T, pool *pgxpool.Pool, owner int64, visibility string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO collections (id, name, owner_user_ref, visibility)
		 VALUES ($1, $2, $3, $4)`,
		id, "ct_acl_listing_"+visibility, owner, visibility); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM collection_acls WHERE collection_id = $1`, id)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE id = $1`, id)
	})
	return id
}

func alSeedGrant(t *testing.T, pool *pgxpool.Pool, colID uuid.UUID, principalID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO collection_acls (collection_id, principal_type, principal_id, permission)
		 VALUES ($1, 'user', $2, 'read')`,
		colID, principalID); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
}

func alHandler(t *testing.T, pool *pgxpool.Pool) *collections.Handler {
	t.Helper()
	// nil registry: this gate never touches the cache, and the other
	// collections tests wire it the same way.
	return collections.NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

func alIdentity(ref int64, caps ...string) *auth.Identity {
	return &auth.Identity{UserRef: ref, AuthMethod: "session", Capabilities: caps}
}

func alListAcls(
	t *testing.T,
	h *collections.Handler,
	id *auth.Identity,
	colID uuid.UUID,
) openapi.ListCollectionAclsResponseObject {
	t.Helper()
	resp, err := h.ListCollectionAcls(
		auth.WithIdentity(context.Background(), id),
		openapi.ListCollectionAclsRequestObject{Id: openapi_types.UUID(colID)},
	)
	if err != nil {
		t.Fatalf("ListCollectionAcls: %v", err)
	}
	return resp
}

// TestListCollectionAcls_PublicCollectionDoesNotDiscloseItsGuestList is
// #933 in one fixture, and the pairing is the test.
func TestListCollectionAcls_PublicCollectionDoesNotDiscloseItsGuestList(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	h := alHandler(t, pool)
	colID := alSeedCollection(t, pool, alOwner, "public")
	alSeedGrant(t, pool, colID, "9330002")

	stranger := alIdentity(alStranger)

	// The before-state, asserted rather than asserted-about. This is the
	// caller and the collection that USED to hand over the grant list,
	// and the route by which they did it — the public disjunct — is
	// still live: the stranger reads the collection itself perfectly
	// well. So the 403 below is the guest list closing, not the fixture
	// being unreachable.
	readResp, err := h.GetCollection(
		auth.WithIdentity(context.Background(), stranger),
		openapi.GetCollectionRequestObject{Id: openapi_types.UUID(colID)},
	)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	if _, is := readResp.(openapi.GetCollection200JSONResponse); !is {
		t.Fatalf("the stranger cannot even READ this public collection (%T) — the fixture "+
			"is wrong, and the 403 below would prove nothing", readResp)
	}

	// The disclosure itself.
	strangerResp := alListAcls(t, h, stranger, colID)
	if _, is := strangerResp.(openapi.ListCollectionAcls403JSONResponse); !is {
		t.Errorf("an authenticated stranger listed the ACLs of a PUBLIC collection: %T, want 403.\n"+
			"`public` describes the collection's CONTENTS; it does not license enumerating "+
			"who the owner individually shared it with (#933, matching #876 on posts)",
			strangerResp)
	}

	// Not over-tightened: the owner still manages their own grants, and
	// the list is NON-EMPTY, so the refusal above is refusing something
	// real.
	ownerResp := alListAcls(t, h, alIdentity(alOwner), colID)
	rows, is := ownerResp.(openapi.ListCollectionAcls200JSONResponse)
	if !is {
		t.Fatalf("the owner got %T from their own collection's ACL list, want 200 — the gate "+
			"must not deny everybody", ownerResp)
	}
	if len(rows) == 0 {
		t.Fatalf("the owner's ACL list is empty, so the stranger's 403 could be hiding " +
			"nothing at all — the fixture must have a grant to leak")
	}

	// The admin bypass the row-visibility predicate deliberately does
	// not carry is unchanged.
	adminResp := alListAcls(t, h, alIdentity(alAdmin, collections.CapCollectionsAdmin), colID)
	if _, is := adminResp.(openapi.ListCollectionAcls200JSONResponse); !is {
		t.Errorf("collections.admin got %T, want 200 — the management bypass is unchanged by #933",
			adminResp)
	}
	sysResp := alListAcls(t, h, alIdentity(alAdmin, collections.CapSystemAdmin), colID)
	if _, is := sysResp.(openapi.ListCollectionAcls200JSONResponse); !is {
		t.Errorf("system.admin got %T, want 200", sysResp)
	}
}

// TestListCollectionAcls_ReadGranteeIsStillRefused pins the disjunct
// #661 dropped, alongside the one #933 dropped. A read grantee may USE
// the collection without learning who else was granted what.
func TestListCollectionAcls_ReadGranteeIsStillRefused(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	h := alHandler(t, pool)
	colID := alSeedCollection(t, pool, alOwner, "private")
	alSeedGrant(t, pool, colID, "9330002")

	grantee := alIdentity(alGrantee)

	// The grant works — the grantee reads the private collection. Same
	// pairing as above: without this leg, "grantee gets 403" would pass
	// on a grant that simply never took.
	readResp, err := h.GetCollection(
		auth.WithIdentity(context.Background(), grantee),
		openapi.GetCollectionRequestObject{Id: openapi_types.UUID(colID)},
	)
	if err != nil {
		t.Fatalf("GetCollection: %v", err)
	}
	if _, is := readResp.(openapi.GetCollection200JSONResponse); !is {
		t.Fatalf("the grantee cannot read the collection they hold a grant on (%T) — "+
			"the fixture is wrong", readResp)
	}

	if _, is := alListAcls(t, h, grantee, colID).(openapi.ListCollectionAcls403JSONResponse); !is {
		t.Errorf("a read grantee listed the guest list, want 403")
	}
}

// TestCanMutateCollection_AnonymousAndRefZero covers the hardening #936
// applied to canMutatePost and left off its collection twin.
//
// canMutateCollection is unexported, so it is driven through the gate
// this PR just put in front of it: ListCollectionAcls is now
// `!canMutateCollection(...)` and nothing else, which makes it the
// function's observable surface.
//
// An anonymous identity carries UserRef 0. A collection row with
// owner_user_ref = 0 would have matched it as its OWNER — the same
// latent class #936 closed for assets. `collections.owner_user_ref` is
// `bigint NOT NULL`, so unlike assets there is no NULL-owner case; ref 0
// is the whole of it.
func TestCanMutateCollection_AnonymousAndRefZero(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	h := alHandler(t, pool)
	zeroOwned := alSeedCollection(t, pool, 0, "private")
	alSeedGrant(t, pool, zeroOwned, "9330002")

	anon := &auth.Identity{UserRef: 0, AuthMethod: "anonymous"}
	anonResp := alListAcls(t, h, anon, zeroOwned)
	if _, is := anonResp.(openapi.ListCollectionAcls403JSONResponse); !is {
		t.Errorf("an anonymous caller was admitted to a collection owned by ref 0, want 403 — "+
			"ref 0 is the anonymous SENTINEL, not a user, and must not match on either side "+
			"of the ownership comparison (%T)", anonResp)
	}

	// A signed-in caller carrying ref 0 (a malformed session rather than
	// the anonymous sentinel) must not match either.
	zeroSession := &auth.Identity{UserRef: 0, AuthMethod: "session"}
	if _, is := alListAcls(t, h, zeroSession, zeroOwned).(openapi.ListCollectionAcls403JSONResponse); !is {
		t.Errorf("a ref-0 session matched a ref-0-owned collection, want 403")
	}

	// And the collection is still reachable by an admin, so the refusals
	// above are not "this row is unreadable by anyone".
	admin := alIdentity(alAdmin, collections.CapSystemAdmin)
	if _, is := alListAcls(t, h, admin, zeroOwned).(openapi.ListCollectionAcls200JSONResponse); !is {
		t.Errorf("system.admin cannot read a ref-0-owned collection's ACLs, want 200")
	}
}
