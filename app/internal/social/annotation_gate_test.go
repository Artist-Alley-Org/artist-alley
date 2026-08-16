// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1135 — the asset gate on the text-annotation endpoints.
//
// # What was wrong, and why a single-caller test could not see it
//
// `ListAssetTextAnnotations` / `CreateAssetTextAnnotation` opened with
// `assetExists` — `SELECT EXISTS (... WHERE id = $1)`. Presence is not
// readability, so any signed-in caller could read and write annotations
// on an asset whose bytes they have never been entitled to. Presence
// also made the endpoint a UUID-existence oracle.
//
// "The owner reads their own annotations" passes on the broken handler
// and on the fixed one. So does "the list has one item". What separates
// them is the SAME asset read by TWO callers whose verdicts must
// differ, which is what every case below is built from.
//
// # Why the tiers are the axis
//
// Per ADR 0064 sensitivity gates CONTENT, and EntityAsset's
// authenticated ROW predicate is `deleted_at IS NULL` and nothing more.
// A gate built on the row plane alone therefore admits every signed-in
// caller to every undeleted asset — it would pass any test that only
// checked "a gate exists". The tier table is what proves the gate is
// the CONTENT plane: `public` must ADMIT the stranger (a gate that
// refuses everyone collapses the endpoint into "your own assets only",
// a different bug with the same refusal output), `team` must admit the
// member and refuse the stranger (which is what proves it consults
// team_memberships rather than a tier allow-list), and restricted /
// embargo must refuse both.
//
// The soft-delete case is the ROW plane's own arm: `ContentReadable`
// never looks at `deleted_at`, so without CanSee in the conjunction the
// OWNER would keep annotating a deleted document.
//
// # The refusal must be indistinguishable from absence
//
// Asserted against a freshly-minted UUID rather than a literal status,
// so a change to either keeps them in step or fails here.
//
// # The writes assert the PERSISTED value
//
// A create refusal is checked by counting rows in `comments`, not by
// reading the handler's own response — a handler that 404s and writes
// anyway passes a response-shaped assertion (#946). The update refusal
// re-reads the stored body for the same reason.
//
// Skips without AA_DB_PASSWORD, like the other social integration tests.

package social

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Synthetic refs, disjoint from every other set in this package.
const (
	agOwner    int64 = 11350001 // owns every asset + authors every annotation
	agStranger int64 = 11350002 // signed in, in no team, holds posts.comment only
	agMember   int64 = 11350003 // as agStranger, plus membership of the team below
	agViewer   int64 = 11350004 // holds content.read.all — the demo-viewer role
	agMod      int64 = 11350005 // holds comments.delete.any — the update-path principal
)

// agIdentity builds a signed-in caller holding exactly `caps`. Every
// arm below holds posts.comment, because without it CreateAsset...
// refuses at the capability check and the asset gate is never reached —
// which would make a create refusal prove nothing about the gate.
func agIdentity(ref int64, caps ...string) *auth.Identity {
	return &auth.Identity{
		UserRef:      ref,
		AuthMethod:   "session",
		Capabilities: append([]string{CapPostsComment}, caps...),
	}
}

func agCtx(ref int64, caps ...string) context.Context {
	return auth.WithIdentity(context.Background(), agIdentity(ref, caps...))
}

// agSeedAsset plants one asset at `tier` with one text annotation on
// it, and returns the asset id plus the annotation's body. The body is
// unique per asset so a leak into the wrong response is identifiable
// rather than merely counted.
func agSeedAsset(t *testing.T, pool *pgxpool.Pool, tier string, teamID *uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	assetID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, title, asset_type, owner_user_ref, sensitivity, team_id)
		 VALUES ($1, $2, 1, $3, $4, $5)`,
		assetID, "ag "+tier, agOwner, tier, teamID); err != nil {
		t.Fatalf("seed %s asset: %v", tier, err)
	}
	body := "reviewer note on the " + tier + " document"
	annID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO comments
		   (id, target_kind, target_id, root_id, depth, author_user_ref, body,
		    annotation_type, annotation_data)
		 VALUES ($1, 'asset', $2, $1, 0, $3, $4, 'text-range', $5)`,
		annID, assetID, agOwner, body,
		[]byte(`{"start_line":1,"start_col":0,"end_line":1,"end_col":9}`)); err != nil {
		t.Fatalf("seed annotation: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM comments WHERE target_id = $1`, assetID)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, assetID)
	})
	return assetID, body
}

// agSeedTeam creates a team with agMember in it. Returns its id.
func agSeedTeam(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	teamID := uuid.New()
	slug := "ag-team-" + teamID.String()[:8]
	if _, err := pool.Exec(ctx,
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $2)`, teamID, slug); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, teamID, agMember); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM team_memberships WHERE team_id = $1`, teamID)
		_, _ = pool.Exec(c, `DELETE FROM teams WHERE id = $1`, teamID)
	})
	return teamID
}

// agList drives the REAL handler, not the query underneath it — the
// hole was in the handler's gate.
func agList(t *testing.T, h *Handler, ctx context.Context, assetID uuid.UUID) openapi.ListAssetTextAnnotationsResponseObject {
	t.Helper()
	resp, err := h.ListAssetTextAnnotations(ctx, openapi.ListAssetTextAnnotationsRequestObject{
		Id: openapi_types.UUID(assetID),
	})
	if err != nil {
		t.Fatalf("ListAssetTextAnnotations: %v", err)
	}
	return resp
}

// agBodies pulls every annotation body out of a response, whatever its
// shape. Returning the BODIES rather than a count is what lets a
// refusal assert "nothing about this document's review reached the
// caller".
func agBodies(resp openapi.ListAssetTextAnnotationsResponseObject) []string {
	ok, is200 := resp.(openapi.ListAssetTextAnnotations200JSONResponse)
	if !is200 {
		return nil
	}
	out := make([]string, 0, len(ok))
	for _, c := range ok {
		out = append(out, c.Body)
	}
	return out
}

// TestAnnotationGate_PairPerTier is the same-asset-opposite-verdicts
// pair, run once per sensitivity tier the content rule distinguishes.
func TestAnnotationGate_PairPerTier(t *testing.T) {
	pool := cgPool(t)
	h := cgHandler(pool)
	teamID := agSeedTeam(t, pool)

	cases := []struct {
		tier string
		// The OWNER always reads their own document's annotations —
		// the control arm, and what makes a refusal below a gate
		// rather than a broken endpoint.
		wantStranger bool
		wantMember   bool
	}{
		// Readable by everyone, so the gate must NOT refuse.
		{tier: "public", wantStranger: true, wantMember: true},
		// The membership tier: the one row where the two arms differ,
		// which is what proves the gate consults team_memberships.
		{tier: "team", wantStranger: false, wantMember: true},
		// The two withholding tiers.
		{tier: "restricted", wantStranger: false, wantMember: false},
		{tier: "embargo", wantStranger: false, wantMember: false},
	}

	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			assetID, body := agSeedAsset(t, pool, tc.tier, &teamID)

			// Arm 1 — the owner. Always reads.
			got := agBodies(agList(t, h, agCtx(agOwner), assetID))
			if len(got) != 1 || got[0] != body {
				t.Fatalf("owner on %s: got bodies %q, want exactly [%q] "+
					"(the control arm — a refusal here means the endpoint is broken, not gated)",
					tc.tier, got, body)
			}

			agAssertArm(t, h, assetID, agCtx(agStranger), "stranger", tc.tier, tc.wantStranger, body)
			agAssertArm(t, h, assetID, agCtx(agMember), "team member", tc.tier, tc.wantMember, body)
		})
	}
}

func agAssertArm(
	t *testing.T,
	h *Handler,
	assetID uuid.UUID,
	ctx context.Context,
	who, tier string,
	want bool,
	body string,
) {
	t.Helper()
	resp := agList(t, h, ctx, assetID)
	got := agBodies(resp)
	if want {
		if len(got) != 1 || got[0] != body {
			t.Errorf("%s on %s: got bodies %q, want exactly [%q]", who, tier, got, body)
		}
		if _, ok := resp.(openapi.ListAssetTextAnnotations200JSONResponse); !ok {
			t.Errorf("%s on %s: response is %T, want a 200", who, tier, resp)
		}
		return
	}
	// Refused. The status must be the SAME 404 an absent asset produces
	// — asserted against a freshly-minted UUID rather than a hardcoded
	// literal, so a change to the message keeps the two in step or
	// fails here.
	absent := agList(t, h, ctx, uuid.New())
	if !agSameRefusal(resp, absent) {
		t.Errorf("%s on %s: refusal is %#v, absent-asset is %#v — a distinguishable "+
			"refusal makes this endpoint a UUID-existence oracle", who, tier, resp, absent)
	}
	if len(got) != 0 {
		t.Errorf("%s on %s: refusal LEAKED annotation bodies %q — the bodies are the "+
			"payload this gate exists to withhold", who, tier, got)
	}
}

// agSameRefusal compares two refusals. The 200 case is a slice, which
// is not comparable with ==, so the comparison goes through JSON —
// which is also what the client actually sees.
func agSameRefusal(a, b openapi.ListAssetTextAnnotationsResponseObject) bool {
	ja, erra := json.Marshal(a)
	jb, errb := json.Marshal(b)
	if erra != nil || errb != nil {
		return false
	}
	return string(ja) == string(jb)
}

// TestAnnotationGate_SoftDeletedAsset is the ROW plane's own arm.
//
// ContentReadable never looks at deleted_at, so a gate that composed
// only the CONTENT plane would let the OWNER — who passes every tier —
// keep reading and writing annotations on a document that has been
// deleted out from under them. The owner is deliberately the caller
// here: any other caller would be refused by the content plane anyway
// and would prove nothing about the row plane.
func TestAnnotationGate_SoftDeletedAsset(t *testing.T) {
	pool := cgPool(t)
	h := cgHandler(pool)

	assetID, body := agSeedAsset(t, pool, "public", nil)

	// Readable while live — the control.
	if got := agBodies(agList(t, h, agCtx(agOwner), assetID)); len(got) != 1 || got[0] != body {
		t.Fatalf("owner pre-delete: got %q, want [%q]", got, body)
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = now() WHERE id = $1`, assetID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	resp := agList(t, h, agCtx(agOwner), assetID)
	if got := agBodies(resp); len(got) != 0 {
		t.Errorf("owner post-delete: got %q, want none — the ROW plane conjunct "+
			"(visibility.CanSee) is missing from the gate", got)
	}
	if !agSameRefusal(resp, agList(t, h, agCtx(agOwner), uuid.New())) {
		t.Errorf("soft-deleted refusal differs from absent-asset refusal")
	}
}

// TestAnnotationGate_ContentReadAll is the positive arm for the
// capability short-circuit the composition inherits from
// CanReadContent.
//
// Without it, `restricted` in the table above is indistinguishable from
// "a tier nobody but the owner can ever reach" — which a gate that
// dropped the capability disjunct would also produce. The demo-viewer
// role (content.read.all) exists to render a mostly-restricted
// catalogue; it must reach the annotations on what it renders.
func TestAnnotationGate_ContentReadAll(t *testing.T) {
	pool := cgPool(t)
	h := cgHandler(pool)

	assetID, body := agSeedAsset(t, pool, "restricted", nil)

	// Refused without the capability…
	if got := agBodies(agList(t, h, agCtx(agViewer), assetID)); len(got) != 0 {
		t.Fatalf("pre-grant: plain caller read %q on a restricted asset", got)
	}
	// …and admitted with it, on the same asset.
	ctx := agCtx(agViewer, visibility.ContentReadAll)
	got := agBodies(agList(t, h, ctx, assetID))
	if len(got) != 1 || got[0] != body {
		t.Errorf("content.read.all holder got %q, want exactly [%q] — the capability "+
			"disjunct is missing from the gate", got, body)
	}
}

// TestAnnotationGate_AnonymousArm drives the rule's anonymous branch
// directly.
//
// The handler 401s an anonymous caller before the gate runs, so an
// end-to-end assertion alone would prove only that the 401 is still
// there — true before the fix too. What matters is that the gate itself
// takes the anonymous branch correctly, because that is the branch a
// public-mode allowlist entry would switch on (#709). So this asserts
// BOTH.
func TestAnnotationGate_AnonymousArm(t *testing.T) {
	pool := cgPool(t)
	h := cgHandler(pool)
	ctx := context.Background()

	publicAsset, _ := agSeedAsset(t, pool, "public", nil)
	restrictedAsset, _ := agSeedAsset(t, pool, "restricted", nil)

	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"public", publicAsset, true},
		{"restricted", restrictedAsset, false},
		{"absent", uuid.New(), false},
	} {
		got, err := h.assetContentReadable(ctx, nil, tc.id)
		if err != nil {
			t.Fatalf("assetContentReadable(anonymous, %s): %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("assetContentReadable(anonymous, %s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	// And the endpoint's own answer to an anonymous caller: 401, with no
	// annotations, on the PUBLIC asset — the one the gate above admits.
	resp := agList(t, h, ctx, publicAsset)
	if _, ok := resp.(openapi.ListAssetTextAnnotations401JSONResponse); !ok {
		t.Errorf("anonymous listing: got %T, want a 401", resp)
	}
	if got := agBodies(resp); len(got) != 0 {
		t.Errorf("anonymous listing leaked bodies %q", got)
	}
}

// TestAnnotationGate_CreateRefusalPersistsNothing asserts the WRITE arm
// against the database rather than against the handler's own answer.
//
// A handler that returns 404 and inserts anyway passes every
// response-shaped assertion (#946). The row count is the only thing
// that can tell the two apart.
func TestAnnotationGate_CreateRefusalPersistsNothing(t *testing.T) {
	pool := cgPool(t)
	h := cgHandler(pool)
	ctx := context.Background()

	assetID, _ := agSeedAsset(t, pool, "restricted", nil)

	before := agCountAnnotations(t, pool, assetID)

	anchorAny := openapi.TextAnnotationAnchor{
		Color: "#fef08a", Style: "highlight",
		StartLine: 4, StartCol: 0, EndLine: 4, EndCol: 12,
	}
	resp, err := h.CreateAssetTextAnnotation(agCtx(agStranger), openapi.CreateAssetTextAnnotationRequestObject{
		Id: openapi_types.UUID(assetID),
		Body: &openapi.CreateAssetTextAnnotationJSONRequestBody{
			Anchor: anchorAny,
			Body:   ptr("I should never be stored"),
		},
	})
	if err != nil {
		t.Fatalf("CreateAssetTextAnnotation: %v", err)
	}
	if _, refused := resp.(openapi.CreateAssetTextAnnotation404JSONResponse); !refused {
		t.Errorf("stranger creating on a restricted asset: got %T, want a 404", resp)
	}
	if after := agCountAnnotations(t, pool, assetID); after != before {
		t.Errorf("annotation rows on the restricted asset went %d → %d — the refusal "+
			"still WROTE; the gate must run before the insert", before, after)
	}

	// The control: the owner's identical create SUCCEEDS and does
	// persist. Without it a gate that refuses every write passes above.
	ownerResp, err := h.CreateAssetTextAnnotation(agCtx(agOwner), openapi.CreateAssetTextAnnotationRequestObject{
		Id: openapi_types.UUID(assetID),
		Body: &openapi.CreateAssetTextAnnotationJSONRequestBody{
			Anchor: anchorAny,
			Body:   ptr("the owner's own note"),
		},
	})
	if err != nil {
		t.Fatalf("owner CreateAssetTextAnnotation: %v", err)
	}
	if _, ok := ownerResp.(openapi.CreateAssetTextAnnotation201JSONResponse); !ok {
		t.Fatalf("owner create: got %T, want a 201", ownerResp)
	}
	if after := agCountAnnotations(t, pool, assetID); after != before+1 {
		t.Errorf("owner create persisted %d rows, want %d", after, before+1)
	}
	_ = ctx
}

// TestAnnotationGate_UpdateIsGatedOnTheAsset covers the third endpoint,
// whose principal is the reason it needs the gate at all.
//
// `UpdateTextAnnotation` authorises on author-or-moderator. A moderator
// (comments.delete.any) is by construction NOT the author and NOT the
// asset's owner, so before the fix they could edit — and, since the
// handler echoes the row back, READ — annotations on a document they
// were never admitted to. The refusal is asserted against the stored
// body, not the response.
func TestAnnotationGate_UpdateIsGatedOnTheAsset(t *testing.T) {
	pool := cgPool(t)
	h := cgHandler(pool)

	assetID, body := agSeedAsset(t, pool, "restricted", nil)
	annID := agAnnotationID(t, pool, assetID)

	resp, err := h.UpdateTextAnnotation(
		agCtx(agMod, CapCommentsDeleteAny),
		openapi.UpdateTextAnnotationRequestObject{
			Id:   openapi_types.UUID(annID),
			Body: &openapi.UpdateTextAnnotationJSONRequestBody{Body: ptr("moderator rewrote this")},
		})
	if err != nil {
		t.Fatalf("UpdateTextAnnotation: %v", err)
	}
	if _, refused := resp.(openapi.UpdateTextAnnotation404JSONResponse); !refused {
		t.Errorf("moderator updating an annotation on an unreachable asset: got %T, want a 404", resp)
	}
	if got := agStoredBody(t, pool, annID); got != body {
		t.Errorf("stored body is now %q, want the original %q — the refusal still WROTE", got, body)
	}

	// The control: the author's own update on the same annotation
	// succeeds, so the refusal above is the asset gate and not a
	// blanket "updates are broken".
	ok, err := h.UpdateTextAnnotation(agCtx(agOwner), openapi.UpdateTextAnnotationRequestObject{
		Id:   openapi_types.UUID(annID),
		Body: &openapi.UpdateTextAnnotationJSONRequestBody{Body: ptr("the author's revision")},
	})
	if err != nil {
		t.Fatalf("author UpdateTextAnnotation: %v", err)
	}
	if _, is200 := ok.(openapi.UpdateTextAnnotation200JSONResponse); !is200 {
		t.Fatalf("author update: got %T, want a 200", ok)
	}
	if got := agStoredBody(t, pool, annID); got != "the author's revision" {
		t.Errorf("author update did not persist: stored body is %q", got)
	}
}

func agCountAnnotations(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM comments
		  WHERE target_kind = 'asset' AND target_id = $1
		    AND annotation_type = 'text-range' AND deleted_at IS NULL`,
		assetID).Scan(&n); err != nil {
		t.Fatalf("count annotations: %v", err)
	}
	return n
}

func agAnnotationID(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM comments
		  WHERE target_kind = 'asset' AND target_id = $1 AND annotation_type = 'text-range'
		  LIMIT 1`, assetID).Scan(&id); err != nil {
		t.Fatalf("load annotation id: %v", err)
	}
	return id
}

func agStoredBody(t *testing.T, pool *pgxpool.Pool, annID uuid.UUID) string {
	t.Helper()
	var body string
	if err := pool.QueryRow(context.Background(),
		`SELECT body FROM comments WHERE id = $1`, annID).Scan(&body); err != nil {
		t.Fatalf("load annotation body: %v", err)
	}
	return body
}

func ptr[T any](v T) *T { return &v }
