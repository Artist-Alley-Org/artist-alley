// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end cover for the `reference` field type (#817).
//
// A reference value is a bare asset UUID, and every read surface that
// printed it printed a 36-character identifier where a title belongs.
// The fix resolves the target server-side, which means this file has
// two jobs that pull in opposite directions:
//
//   - prove the title IS resolved and linkable for a caller entitled
//     to it, and
//   - prove the resolution cannot become a channel for a name the
//     caller was not entitled to see.
//
// The second is the one worth writing carefully. A resolve-and-return
// added to a read path is the #665 class of bug: correct for the
// common caller, a disclosure for the uncommon one. So the visibility
// assertions here do not test the happy path with different data —
// they pin the two claims the query's join condition rests on (see
// ListAssetFieldValues in queries.sql), so that if either stops being
// true the failure lands here rather than in production.
package metadata_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// The happy path + the degradations
// ---------------------------------------------------------------------------

func TestReferenceValueResolvesToTitle(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	router, userRef := makeRouter(t, pool /*admin=*/, true)

	fieldID := mustCreateField(t, router, map[string]any{
		"code":  "metadata_test_derived_from",
		"label": "Derived From",
		"type":  "reference",
	})

	subject := mustInsertAsset(t, pool, userRef)
	// A title that is nothing like a UUID, so an assertion cannot pass
	// by coincidence if the id leaks through as the text.
	target := mustInsertTitledAsset(t, pool, userRef, "Concept Sheet 04", "active")
	cleanupAssets(t, pool, subject, target)

	rr := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", subject, fieldID),
		map[string]any{"value_ref": target})
	if rr.Code != http.StatusOK {
		t.Fatalf("set reference value: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// ── The write response resolves too ─────────────────────────────
	// Not decoration: buildAssetValue exists (#775) because two
	// endpoints disagreed about the shape of one value. Shipping a DTO
	// field only the list path populates would rebuild that exactly.
	var put openapi.AssetFieldValue
	mustDecode(t, rr.Body.Bytes(), &put)
	if put.ResolvedReference == nil {
		t.Error("PUT response has no resolved_reference; the upsert path and the list path " +
			"must agree on the shape of a reference value (#775)")
	} else if put.ResolvedReference.Title != "Concept Sheet 04" {
		t.Errorf("PUT resolved_reference.Title = %q, want %q",
			put.ResolvedReference.Title, "Concept Sheet 04")
	}

	// ── The read path — what the panel actually consumes ────────────
	v := findAssetValue(t, getAssetFields(t, router, subject), fieldID)

	if v.ValueRef == nil {
		t.Fatal("value_ref is nil; the raw id must stay on the record — it is the client's fallback")
	}
	if v.ValueRef.String() != target {
		t.Errorf("value_ref = %s, want %s", v.ValueRef, target)
	}
	if v.ResolvedReference == nil {
		t.Fatal("resolved_reference is nil for a live, visible target — this is the #817 bug")
	}
	if got := v.ResolvedReference.Title; got != "Concept Sheet 04" {
		t.Errorf("resolved_reference.title = %q, want %q", got, "Concept Sheet 04")
	}
	if got := v.ResolvedReference.Id.String(); got != target {
		t.Errorf("resolved_reference.id = %s, want %s — the client builds /assets/{id} from it", got, target)
	}
}

func TestReferenceValueDegradesForDeletedTarget(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	router, userRef := makeRouter(t, pool /*admin=*/, true)
	fieldID := mustCreateField(t, router, map[string]any{
		"code": "metadata_test_deleted_ref", "label": "Ref", "type": "reference",
	})

	subject := mustInsertAsset(t, pool, userRef)
	target := mustInsertTitledAsset(t, pool, userRef, "Doomed Original", "active")
	cleanupAssets(t, pool, subject, target)

	rr := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", subject, fieldID),
		map[string]any{"value_ref": target})
	if rr.Code != http.StatusOK {
		t.Fatalf("set reference: %d %s", rr.Code, rr.Body.String())
	}

	// Sanity: it resolved BEFORE the delete. Without this the
	// post-delete assertion passes even if resolution never worked at
	// all, which would make this test a no-op that looks like cover.
	if before := findAssetValue(t, getAssetFields(t, router, subject), fieldID); before.ResolvedReference == nil {
		t.Fatal("target did not resolve before deletion; the rest of this test would prove nothing")
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = NOW() WHERE id = $1`, target); err != nil {
		t.Fatalf("soft-delete target: %v", err)
	}

	// ── The degradation ─────────────────────────────────────────────
	// Chosen shape: resolved_reference is simply ABSENT, and the raw
	// value_ref stays. Not a 404 (the panel must survive), not a
	// "deleted" marker (that is itself a disclosure — it tells a
	// reader the row existed and was removed, which the bare id does
	// not), and not a stale cached title.
	v := findAssetValue(t, getAssetFields(t, router, subject), fieldID)
	if v.ResolvedReference != nil {
		t.Errorf("resolved_reference = %+v for a soft-deleted target, want absent — "+
			"a deleted asset's title is not the caller's to read", *v.ResolvedReference)
	}
	if v.ValueRef == nil || v.ValueRef.String() != target {
		t.Errorf("value_ref = %v, want %s — the id must remain so the client can degrade to it",
			v.ValueRef, target)
	}
}

func TestReferenceValueDegradesForDanglingRef(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	router, userRef := makeRouter(t, pool /*admin=*/, true)
	fieldID := mustCreateField(t, router, map[string]any{
		"code": "metadata_test_dangling_ref", "label": "Ref", "type": "reference",
	})
	subject := mustInsertAsset(t, pool, userRef)
	cleanupAssets(t, pool, subject)

	// A well-formed UUID that is not any asset. Writing it directly
	// rather than through the API: value_ref carries no FK, so this
	// state is reachable in production (federation, a hard delete) and
	// the read path must not blow up on it.
	orphan := uuid.New().String()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO asset_field_value (asset_id, field_id, value_ref, set_by, set_at)
		 VALUES ($1, $2, $3, 'manual', NOW())`, subject, fieldID, orphan); err != nil {
		t.Fatalf("insert dangling value: %v", err)
	}

	v := findAssetValue(t, getAssetFields(t, router, subject), fieldID)
	if v.ResolvedReference != nil {
		t.Errorf("resolved_reference = %+v for a ref to no row, want absent", *v.ResolvedReference)
	}
	if v.ValueRef == nil || v.ValueRef.String() != orphan {
		t.Errorf("value_ref = %v, want %s", v.ValueRef, orphan)
	}
}

// TestReferenceWriteRejectsDanglingRef is the WRITE half of the #842
// interlock, and the pair to TestReferenceValueDegradesForDanglingRef
// above (the READ half). Together they pin the rule the two issues
// share: a dangling reference is refused on the way IN (422), but a
// value that was valid when written and whose target vanished later is
// tolerated on the way OUT (bare id, #839). If a future change ever
// makes the write path tolerant or the read path strict, exactly one of
// this pair fails and names which direction moved.
func TestReferenceWriteRejectsDanglingRef(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	router, userRef := makeRouter(t, pool /*admin=*/, true)
	fieldID := mustCreateField(t, router, map[string]any{
		"code": "metadata_test_reject_dangling", "label": "Ref", "type": "reference",
	})
	subject := mustInsertAsset(t, pool, userRef)
	target := mustInsertTitledAsset(t, pool, userRef, "Real Target", "active")
	cleanupAssets(t, pool, subject, target)

	// A well-formed UUID naming no asset — refused on write. Before #842
	// this returned 200 and stored the dangling id.
	orphan := uuid.New().String()
	rr := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", subject, fieldID),
		map[string]any{"value_ref": orphan})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("dangling reference write: status=%d want 422 body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	mustDecode(t, rr.Body.Bytes(), &body)
	if body["reason"] != "dangling_reference" {
		t.Errorf("reason=%v want dangling_reference", body["reason"])
	}

	// Nothing was stored — the write was refused, not silently accepted.
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		subject, fieldID).Scan(&count); err != nil {
		t.Fatalf("count value rows: %v", err)
	}
	if count != 0 {
		t.Errorf("a rejected dangling write left %d value row(s); it must store nothing", count)
	}

	// A reference to a REAL asset still writes and resolves.
	if ok := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", subject, fieldID),
		map[string]any{"value_ref": target}); ok.Code != http.StatusOK {
		t.Fatalf("valid reference write: status=%d want 200 body=%s", ok.Code, ok.Body.String())
	}
}

// TestReferenceResolutionLeavesResolvedOptionsAlone is the
// same-seam regression guard. #817 changed buildAssetValue, which is
// also where #775/#776's resolved_options is populated; asserting the
// two coexist is cheaper than discovering later that adding one
// dropped the other.
func TestReferenceResolutionLeavesResolvedOptionsAlone(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	router, userRef := makeRouter(t, pool /*admin=*/, true)

	selectField := mustCreateField(t, router, map[string]any{
		"code": "metadata_test_coexist_select", "label": "Colour Space", "type": "select",
		"options": map[string]any{
			"values": []any{map[string]any{"value": "srgb", "label": "sRGB"}},
		},
	})
	refField := mustCreateField(t, router, map[string]any{
		"code": "metadata_test_coexist_ref", "label": "Ref", "type": "reference",
	})

	subject := mustInsertAsset(t, pool, userRef)
	target := mustInsertTitledAsset(t, pool, userRef, "Coexisting Target", "active")
	cleanupAssets(t, pool, subject, target)

	if rr := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", subject, selectField),
		map[string]any{"value_text": "srgb"}); rr.Code != http.StatusOK {
		t.Fatalf("set select: %d %s", rr.Code, rr.Body.String())
	}
	if rr := putJSON(t, router, fmt.Sprintf("/assets/%s/fields/%s", subject, refField),
		map[string]any{"value_ref": target}); rr.Code != http.StatusOK {
		t.Fatalf("set reference: %d %s", rr.Code, rr.Body.String())
	}

	values := getAssetFields(t, router, subject)

	sel := findAssetValue(t, values, selectField)
	if sel.ResolvedOptions == nil {
		t.Fatal("resolved_options is nil on a select value — #817 regressed the #775/#776 seam")
	}
	if opt, ok := (*sel.ResolvedOptions)["srgb"]; !ok || opt.Label != "sRGB" {
		t.Errorf("resolved_options[srgb] = %+v, want label sRGB", opt)
	}
	if sel.ResolvedReference != nil {
		t.Errorf("select value carries resolved_reference = %+v, want nil", *sel.ResolvedReference)
	}

	ref := findAssetValue(t, values, refField)
	if ref.ResolvedReference == nil {
		t.Error("reference value has no resolved_reference alongside a select on the same asset")
	}
	if ref.ResolvedOptions != nil {
		t.Errorf("reference value carries resolved_options = %+v, want nil", *ref.ResolvedOptions)
	}
}

// ---------------------------------------------------------------------------
// The visibility claims the join condition rests on
// ---------------------------------------------------------------------------

// TestReferenceResolutionIsUnreachableAnonymously is the leak test.
//
// The disclosure to worry about is a public asset carrying a reference
// to a DRAFT one: the anonymous asset predicate hides the draft's row,
// so resolving its title for a stranger would leak a name through a
// side channel (the #665 class).
//
// That shape cannot be constructed against this endpoint today, and
// the reason is worth stating precisely, because the query's join
// depends on it: GetAssetFields 401s a nil identity before it reads
// anything. An anonymous caller does not receive a filtered response;
// it receives no response at all. "Resolve only for authenticated
// callers" is therefore not a restriction this change added — it is a
// description of who can reach the code at all, which is why the join
// carries the AUTHENTICATED asset predicate and only that one.
//
// The risk in depending on that is silent: if this endpoint is ever
// opened to anonymous callers, the join would begin resolving titles
// under the wrong predicate and nothing would say so. So the 401 is
// pinned here as a load-bearing precondition rather than left as an
// assumption someone has to rediscover.
func TestReferenceResolutionIsUnreachableAnonymously(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	defer pool.Close()

	cleanTestFields(t, pool)
	t.Cleanup(func() { cleanTestFields(t, pool) })

	authed, userRef := makeRouter(t, pool /*admin=*/, true)
	fieldID := mustCreateField(t, authed, map[string]any{
		"code": "metadata_test_anon_ref", "label": "Ref", "type": "reference",
	})

	// The exact shape the brief describes: a subject an anonymous
	// caller could plausibly reach (public, active, ready) carrying a
	// reference to a DRAFT asset whose title the anonymous predicate
	// hides.
	subject := mustInsertTitledAsset(t, pool, userRef, "Public Subject", "active")
	secret := mustInsertTitledAsset(t, pool, userRef, "Unannounced Sequel Title", "draft")
	cleanupAssets(t, pool, subject, secret)

	if rr := putJSON(t, authed, fmt.Sprintf("/assets/%s/fields/%s", subject, fieldID),
		map[string]any{"value_ref": secret}); rr.Code != http.StatusOK {
		t.Fatalf("set reference: %d %s", rr.Code, rr.Body.String())
	}

	// Control: the draft's title IS resolvable for an authenticated
	// caller. Per ADR 0064 a title is row-plane metadata and the
	// authenticated asset predicate is soft-delete only, so this is
	// correct — and it means the anonymous assertion below is
	// asserting a real difference, not an endpoint that resolves
	// nothing for anybody.
	v := findAssetValue(t, getAssetFields(t, authed, subject), fieldID)
	if v.ResolvedReference == nil || v.ResolvedReference.Title != "Unannounced Sequel Title" {
		t.Fatalf("authenticated caller did not resolve the draft target (%+v); "+
			"the anonymous assertion below would prove nothing", v.ResolvedReference)
	}

	// The assertion: anonymously, nothing comes back at all.
	anon := makeAnonymousRouter(t, pool)
	rr := httptest.NewRecorder()
	anon.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/assets/%s/fields", subject), nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /assets/{id}/fields = %d, want 401.\n"+
			"If this endpoint has been opened to anonymous callers, the LEFT JOIN in "+
			"ListAssetFieldValues MUST gain the anonymous asset predicate "+
			"(status/sensitivity/processing) before that lands — as written it would "+
			"resolve a draft asset's title to a stranger.\nbody=%s", rr.Code, rr.Body.String())
	}
	// Belt and braces: whatever the 401 body is, the hidden title is
	// not in it.
	if body := rr.Body.String(); strings.Contains(body, "Unannounced Sequel Title") {
		t.Errorf("anonymous response leaked the draft target's title: %s", body)
	}
}

// TestReferenceJoinMatchesAuthenticatedAssetPredicate pins the other
// half of the claim: that `r.deleted_at IS NULL` in the join IS the
// visibility rule for this endpoint's callers, not a coincidence.
//
// visibility.Predicate is the single enforcement point (ADR 0063), but
// a sqlc query is static SQL and cannot splice it. This test is the
// join that a splice would have made automatic — if the authenticated
// asset predicate ever tightens (#210's sensitivity rule is the
// expected one), it fails here and names the query that has to move.
func TestReferenceJoinMatchesAuthenticatedAssetPredicate(t *testing.T) {
	p, err := visibility.Filter(context.Background(), visibility.EntityAsset,
		visibility.NewCaller(ptrInt64(420000)))
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	frag, args := p.ToSQL("r", 0)

	const want = " AND (r.deleted_at IS NULL)"
	if frag != want {
		t.Errorf("authenticated asset predicate = %q, want %q.\n"+
			"ListAssetFieldValues' LEFT JOIN hard-codes `r.deleted_at IS NULL` as this "+
			"rule. The predicate has changed, so that join now under-enforces: update "+
			"queries.sql (and GetReferencedAsset) to match, then update this test.", frag, want)
	}
	if len(args) != 0 {
		t.Errorf("authenticated asset predicate binds %d args; the join condition binds none, "+
			"so it can no longer be expressed as static SQL", len(args))
	}
}

// TestCollectionReferenceJoinMatchesAuthenticatedAssetPredicate is the
// collection sibling of the pin above (#840). ListCollectionFieldValues
// and GetCollectionFieldValue LEFT JOIN assets on the same
// `r.deleted_at IS NULL` the asset queries use, because a reference
// target's visibility does not change with the subject that points at
// it — an asset the caller may not see is no more visible because a
// collection references it. This pins that the collection join enforces
// the SAME authenticated-asset predicate, so if a future sensitivity
// rule (#210) tightens it, the failure lands here by name too and not
// only on the asset path.
func TestCollectionReferenceJoinMatchesAuthenticatedAssetPredicate(t *testing.T) {
	p, err := visibility.Filter(context.Background(), visibility.EntityAsset,
		visibility.NewCaller(ptrInt64(420001)))
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	frag, args := p.ToSQL("r", 0)

	const want = " AND (r.deleted_at IS NULL)"
	if frag != want {
		t.Errorf("authenticated asset predicate = %q, want %q.\n"+
			"ListCollectionFieldValues / GetCollectionFieldValue LEFT JOIN assets hard-code "+
			"`r.deleted_at IS NULL` as this rule. The predicate has changed, so those joins "+
			"now under-enforce: update queries.sql to match, then update this test.", frag, want)
	}
	if len(args) != 0 {
		t.Errorf("authenticated asset predicate binds %d args; the join condition binds none, "+
			"so it can no longer be expressed as static SQL", len(args))
	}
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// makeAnonymousRouter is makeRouter without the identity middleware —
// no auth.WithIdentity on the context, which is what a real
// unauthenticated request looks like by the time it reaches a handler.
func makeAnonymousRouter(t *testing.T, pool *pgxpool.Pool) chi.Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := metadata.NewHandler(pool, logger, nil)
	router := chi.NewRouter()
	openapi.HandlerFromMux(
		openapi.NewStrictHandler(metaShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil),
		router,
	)
	return router
}

// mustInsertTitledAsset inserts an asset with a chosen title and
// publication status. mustInsertAsset always writes 'test asset' /
// default status, which cannot express either of the states these
// tests turn on.
func mustInsertTitledAsset(t *testing.T, pool *pgxpool.Pool, userRef int64, title, status string) string {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO assets (title, asset_type, owner_user_ref, status)
		 VALUES ($1, 1, $2, $3) RETURNING id`,
		title, userRef, status).Scan(&id); err != nil {
		t.Fatalf("insert asset %q: %v", title, err)
	}
	return id.String()
}

func cleanupAssets(t *testing.T, pool *pgxpool.Pool, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range ids {
			_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM asset_field_value WHERE asset_id = $1 OR value_ref = $1`, id)
			_, _ = pool.Exec(ctx, `DELETE FROM assets WHERE id = $1`, id)
		}
	})
}

func ptrInt64(v int64) *int64 { return &v }
