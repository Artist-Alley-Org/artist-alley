// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1084 — `team` is an admissible featured subject, and the handler and
// the database AGREE about the admissible list.
//
// The list is stated in six places (enumerated in http.go's
// AddFeaturedItem). Only the handler's and the database's can be
// asserted from Go, and the interesting property is not "does 'team'
// work" — it is that those two are the SAME list rather than one
// covering for the other.
//
// So the file is built as a pair:
//
//   * TestAddFeaturedItem_InvalidSubjectKindIs400 pins the handler's
//     half. Delete the Go check and this turns into a 500, because the
//     request reaches Postgres and comes back a 23514 the HTTP layer
//     has no branch for. That is exactly the asymmetric failure the
//     four-place change exists to prevent — a client sees "the server
//     broke" for what is a bad request.
//   * TestFeaturedItems_DatabaseRefusesUnknownSubjectKind pins the
//     database's half directly, bypassing the handler. Without it, a
//     migration that widened the constraint to `text` (no CHECK at all)
//     would pass every other test in this package while quietly making
//     the constraint stop being a backstop.
//
// The write gate is asserted here too, because "featuring is
// operator-curated" is a property of this endpoint and #1084 adds no
// capability of its own: the same system.admin that owns asset and
// collection placement owns team placement.

package featured_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/featured"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// adminRoleID is the seeded Admin role — the one that carries
// system.admin. Pinned as a literal the way baseRoleID is elsewhere.
const adminRoleID = "aa6b632d-5bef-4924-93d4-aba070dfe503"
const baseRoleIDF = "80ec6003-7fd5-4dac-9415-d26d39169d42"

// callerWithRole seeds a user holding one role and returns the context
// the middleware would build for them — capabilities resolved from the
// database, not asserted by the test. A literal Identity would let this
// file claim system.admin without the role actually granting it.
func callerWithRole(t *testing.T, pool *pgxpool.Pool, roleID string) context.Context {
	t.Helper()
	ref := seedUserF(t, pool)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_roles (user_ref, role_id) VALUES ($1, $2)`, ref, roleID); err != nil {
		t.Fatalf("assign role %s: %v", roleID, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_roles WHERE user_ref = $1`, ref)
	})
	res := &auth.Resolver{Pool: pool, Logger: discardLoggerF()}
	ctx := context.Background()
	return auth.WithIdentity(ctx, res.LoadIdentity(ctx, ref))
}

// seedTeamF plants a live team. Featuring one does not require it to be
// anything in particular — featured_items has no FK on subject_id — but
// a real team keeps the fixture honest about what is being featured.
func seedTeamF(t *testing.T, pool *pgxpool.Pool, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	slug := "ftw_" + id.String()[:8] + "_" + label
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`, id, slug, label); err != nil {
		t.Fatalf("seed team %s: %v", label, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	return id
}

func addFeatured(t *testing.T, pool *pgxpool.Pool, ctx context.Context, kind string, subject uuid.UUID) openapi.AddFeaturedItemResponseObject {
	t.Helper()
	h := featured.NewHTTPHandler(featured.NewHandler(pool, discardLoggerF()), discardLoggerF())
	resp, err := h.AddFeaturedItem(ctx, openapi.AddFeaturedItemRequestObject{
		Body: &openapi.FeaturedItemInput{
			SubjectKind: openapi.FeaturedItemInputSubjectKind(kind),
			SubjectId:   openapi_types.UUID(subject),
		},
	})
	if err != nil {
		t.Fatalf("AddFeaturedItem(%s): %v", kind, err)
	}
	return resp
}

// TestAddFeaturedItem_TeamKindIsAccepted — the migration and the Go
// check agree in the ACCEPTING direction. Without migration 00048 this
// returns an error (23514) rather than a response, and the t.Fatalf
// inside addFeatured fires: a valid request 500ing is the failure mode
// of shipping the code change without the database change.
func TestAddFeaturedItem_TeamKindIsAccepted(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)

	team := seedTeamF(t, pool, "featurable")
	resp := addFeatured(t, pool, callerWithRole(t, pool, adminRoleID), "team", team)

	created, ok := resp.(openapi.AddFeaturedItem201JSONResponse)
	if !ok {
		t.Fatalf("AddFeaturedItem(team): want 201, got %T (%+v)", resp, resp)
	}
	if created.SubjectKind != "team" {
		t.Errorf("subject_kind = %q, want %q", created.SubjectKind, "team")
	}
	if uuid.UUID(created.SubjectId) != team {
		t.Errorf("subject_id = %s, want %s", uuid.UUID(created.SubjectId), team)
	}
}

// TestAddFeaturedItem_InvalidSubjectKindIs400 is the test that proves
// the Go check and the database AGREE rather than one covering for the
// other. Remove the validation in http.go and this does not merely fail
// — it fails as a 500, because the insert reaches the CHECK constraint.
func TestAddFeaturedItem_InvalidSubjectKindIs400(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)

	resp := addFeatured(t, pool, callerWithRole(t, pool, adminRoleID), "user", uuid.New())

	bad, ok := resp.(openapi.AddFeaturedItem400JSONResponse)
	if !ok {
		t.Fatalf("AddFeaturedItem(user): want 400, got %T (%+v)", resp, resp)
	}
	// The error string is the third of the four places the list is
	// written down, and an error that names the wrong set is a support
	// ticket rather than an answer.
	if !strings.Contains(bad.Error, "team") {
		t.Errorf("400 body = %q; must name every admissible kind, including team", bad.Error)
	}
}

// TestAddFeaturedItem_RequiresSystemAdmin — featuring stays
// operator-curated. #1084 introduces no capability of its own; a caller
// holding only Base is refused for a team exactly as for an asset.
func TestAddFeaturedItem_RequiresSystemAdmin(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)

	team := seedTeamF(t, pool, "not-yours")
	resp := addFeatured(t, pool, callerWithRole(t, pool, baseRoleIDF), "team", team)
	if _, ok := resp.(openapi.AddFeaturedItem403JSONResponse); !ok {
		t.Fatalf("AddFeaturedItem(team) as Base: want 403, got %T (%+v)", resp, resp)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM featured_items WHERE subject_id = $1`, team).Scan(&n); err != nil {
		t.Fatalf("count placements: %v", err)
	}
	// A 403 that still wrote the row would be a refusal in the response
	// only. Asserted because the handler's gate runs before the insert
	// and nothing else would notice if that order ever changed.
	if n != 0 {
		t.Errorf("refused request wrote %d placement rows, want 0", n)
	}
}

// TestFeaturedItems_DatabaseRefusesUnknownSubjectKind pins the
// constraint itself. The handler is not involved: this is the backstop
// that must survive a future caller which forgets to validate.
func TestFeaturedItems_DatabaseRefusesUnknownSubjectKind(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)

	_, err := pool.Exec(context.Background(),
		`INSERT INTO featured_items (subject_kind, subject_id) VALUES ('user', $1)`, uuid.New())
	if err == nil {
		t.Fatal("database accepted subject_kind='user'; the CHECK constraint is gone or too wide")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("insert error = %v; want a 23514 check violation", err)
	}
	if pgErr.ConstraintName != "featured_items_subject_kind_check" {
		t.Errorf("violated constraint = %q, want featured_items_subject_kind_check", pgErr.ConstraintName)
	}
}

// TestFeaturedItems_DatabaseAcceptsTeamSubjectKind is the accepting half
// of the same assertion, so "the constraint refuses things" cannot be
// satisfied by a constraint that refuses everything.
func TestFeaturedItems_DatabaseAcceptsTeamSubjectKind(t *testing.T) {
	pool := openPoolF(t)
	defer cleanupFeatured(t, pool)

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO featured_items (subject_kind, subject_id) VALUES ('team', $1)`,
		uuid.New()); err != nil {
		t.Fatalf("database refused subject_kind='team': %v", err)
	}
}
