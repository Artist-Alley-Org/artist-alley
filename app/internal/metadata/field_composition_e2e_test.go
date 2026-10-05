// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// EFFECTIVE READABILITY AND NON-DISCLOSURE, asserted at the API boundary
// (#1173, #1119, ADR 0099 §5).
//
// # What this file has to prove, and why a helper test would not
//
// Conditional visibility evaluated over field values the caller may not
// read is an ORACLE over protected metadata: the dependent's visibility
// is observable and the condition is stored, so watching whether a
// control appears reads out the controller's value. Closing that needs
// TWO different things, and a single change cannot supply both:
//
//  1. NON-DISCLOSURE — the protected value must not cross the wire.
//  2. TRUSTWORTHY EFFECTIVE READABILITY — the caller must be able to tell
//     "withheld" from "unset", because those have OPPOSITE consequences
//     for a condition: withheld makes the term unevaluable and SHOWS the
//     dependent, while a readable controller holding nothing is a real
//     FALSE and HIDES it.
//
// ⛔ EVERY ASSERTION HERE IS MADE ON THE RESPONSE BODY, THROUGH THE
// ROUTER. Testing `fieldReadableOnSubject` directly would prove the
// helper and say nothing about what a browser receives, and "the value
// does not cross the wire" is a claim about a response.
//
// # The grants are TEAM-SCOPED, which is the whole point
//
// The four callers below differ ONLY in what they hold. R-1 and R-2 use
// the SAME field and the SAME subject, so a difference between them can
// only have come from the server deriving readability per caller. A
// client-side derivation would answer identically for both, because
// `Identity.Capabilities` carries GLOBAL codes only and neither caller
// holds this one globally.
package metadata_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

// The capability the fixture field is gated behind. A code no shipped
// role holds, so nothing in the baseline can accidentally satisfy it.
const compReadCap = "metadata_test_secret.read"

type compEnv struct {
	pool *pgxpool.Pool
	// admin writes the definitions and the values.
	admin   chi.Router
	assetID string
	collID  string
	teamID  uuid.UUID
	otherID uuid.UUID
	userRef int64
}

// routerFor builds a router whose identity holds `globals` globally and
// `scoped` on `team`.
//
// The scoped half goes through auth.SetIdentityScopedCapForTest, which is
// the only way to reach `Identity.scopedCaps` from an external test
// package. That field is assembled by the real resolver from direct
// grants, a recursive walk of role parents carrying `user_roles.team_id`,
// minus revokes, then expanded through `team_closure`; the helper exists
// so a test can construct the RESULT without paying for all four.
func (e *compEnv) routerFor(t *testing.T, globals []string, scoped string, team uuid.UUID) chi.Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := metadata.NewHandler(e.pool, logger, nil)
	router := chi.NewRouter()
	ref := e.userRef
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{
				UserRef:      ref,
				AuthMethod:   "session",
				Capabilities: append([]string{}, globals...),
			}
			if scoped != "" {
				auth.SetIdentityScopedCapForTest(id, scoped, team)
			}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(metaShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router
}

func newCompEnv(t *testing.T) *compEnv {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	cleanTestFields(t, pool)
	cleanCollectionTestRows(t, pool)

	admin, userRef := makeRouter(t, pool /*admin=*/, true)

	// Two teams: the one that OWNS the asset, and one that does not. The
	// second is what turns "holds a scoped grant" into "holds the RIGHT
	// scoped grant" — a test with one team cannot tell those apart.
	var teamID, otherID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO teams (slug, name) VALUES ('metadata-test-owner-20b', 'owner') RETURNING id`).Scan(&teamID); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO teams (slug, name) VALUES ('metadata-test-other-20b', 'other') RETURNING id`).Scan(&otherID); err != nil {
		t.Fatalf("insert other team: %v", err)
	}

	var assetID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO assets (title, asset_type, owner_user_ref, team_id) VALUES ('comp test asset', 1, $1, $2) RETURNING id`,
		userRef, teamID).Scan(&assetID); err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	collID := mustInsertCollection(t, pool, userRef, "mcoltest comp col")

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = $1`, assetID)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = $1`, assetID)
		_, _ = pool.Exec(c, `DELETE FROM teams WHERE id = ANY($1)`, []uuid.UUID{teamID, otherID})
		cleanTestFields(t, pool)
		cleanCollectionTestRows(t, pool)
	})

	return &compEnv{
		pool: pool, admin: admin, assetID: assetID.String(), collID: collID,
		teamID: teamID, otherID: otherID, userRef: userRef,
	}
}

// gatedField creates a definition behind compReadCap.
func (e *compEnv) gatedField(t *testing.T, code, subjectKind string) string {
	t.Helper()
	body := map[string]any{
		"code": "metadata_test_" + code, "label": code, "type": "text",
		"read_capability": compReadCap,
	}
	if subjectKind == "collection" {
		body["code"] = collectionTestPrefix + code
		body["subject_kind"] = "collection"
	}
	return mustCreateField(t, e.admin, body)
}

// getRaw is `getJSON` plus the response BYTES, which every
// non-disclosure assertion in this file needs. The package already has a
// `getJSON` returning only the recorder; this one exists so a caller
// cannot forget to reach for the body.
func getRaw(t *testing.T, r chi.Router, path string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	rr := getJSON(t, r, path)
	return rr, rr.Body.Bytes()
}

type compState struct {
	FieldID   string `json:"field_id"`
	FieldCode string `json:"field_code"`
	Readable  bool   `json:"readable"`
}

// composition reads one subject's composition state and returns it keyed
// by field code, plus the RAW body so a test can assert on the bytes.
func composition(t *testing.T, r chi.Router, path string) (map[string]compState, []byte) {
	t.Helper()
	rr, raw := getRaw(t, r, path)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", path, rr.Code, raw)
	}
	var rows []compState
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode composition: %v body=%s", err, raw)
	}
	out := map[string]compState{}
	for _, s := range rows {
		out[s.FieldCode] = s
	}
	return out, raw
}

// ---------------------------------------------------------------------------
// R-1 vs R-2 — the signal is SERVER-DERIVED
// ---------------------------------------------------------------------------

// TestComposition_R1vsR2_AssetTeamScoped is A-8's core discriminator.
//
// ONE field, ONE asset, NO stored value, and two callers who differ only
// in the grant they hold:
//
//	R-1  holds compReadCap SCOPED TO THE ASSET'S TEAM -> readable
//	R-2  holds it scoped to ANOTHER team              -> NOT readable
//
// A client-side derivation cannot produce this difference: neither caller
// holds the code globally, so `Identity.Capabilities` is identical for
// both and a browser inspecting it would answer "not readable" twice.
func TestComposition_R1vsR2_AssetTeamScoped(t *testing.T) {
	env := newCompEnv(t)
	code := "metadata_test_r1r2"
	env.gatedField(t, "r1r2", "asset")

	inTeam := env.routerFor(t, nil, compReadCap, env.teamID)
	wrongTeam := env.routerFor(t, nil, compReadCap, env.otherID)

	path := "/assets/" + env.assetID + "/field-composition"

	got, _ := composition(t, inTeam, path)
	if s, ok := got[code]; !ok || !s.Readable {
		t.Fatalf("R-1: caller holding the grant on the ASSET'S team should read it; got %+v (present=%v)", s, ok)
	}
	got2, _ := composition(t, wrongTeam, path)
	if s, ok := got2[code]; !ok || s.Readable {
		t.Fatalf("R-2: caller holding the grant on ANOTHER team must not read it; got %+v (present=%v)", s, ok)
	}
}

// TestComposition_GlobalGrantWorksInAnyScope pins the other arm of the
// same helper: a GLOBAL holding is not team-shaped and must pass.
func TestComposition_GlobalGrantWorksInAnyScope(t *testing.T) {
	env := newCompEnv(t)
	code := "metadata_test_global"
	env.gatedField(t, "global", "asset")

	global := env.routerFor(t, []string{compReadCap}, "", uuid.Nil)
	got, _ := composition(t, global, "/assets/"+env.assetID+"/field-composition")
	if s := got[code]; !s.Readable {
		t.Fatalf("a global holding must be readable in any scope; got %+v", s)
	}

	none := env.routerFor(t, nil, "", uuid.Nil)
	got2, _ := composition(t, none, "/assets/"+env.assetID+"/field-composition")
	if s := got2[code]; s.Readable {
		t.Fatalf("a caller holding nothing must not read a gated field; got %+v", s)
	}
}

// TestComposition_UngatedFieldIsReadable is the floor: most fields carry
// no read gate, and a helper that answered "false" for them would empty
// every form in the product.
func TestComposition_UngatedFieldIsReadable(t *testing.T) {
	env := newCompEnv(t)
	mustCreateField(t, env.admin, map[string]any{
		"code": "metadata_test_open", "label": "open", "type": "text",
	})
	none := env.routerFor(t, nil, "", uuid.Nil)
	got, _ := composition(t, none, "/assets/"+env.assetID+"/field-composition")
	if s, ok := got["metadata_test_open"]; !ok || !s.Readable {
		t.Fatalf("an ungated field must be readable by any authenticated caller; got %+v (present=%v)", s, ok)
	}
}

// ---------------------------------------------------------------------------
// R-3a vs R-3b — the DATA PATH differs, not merely a flag
// ---------------------------------------------------------------------------

// TestComposition_R3_NonDisclosureAtTheBoundary is the non-disclosure
// acceptance, and it asserts on BYTES.
//
// A real secret is stored. The caller WITHOUT the grant must receive the
// unreadable STATE and no trace of the value anywhere in either response;
// the caller WITH it must receive the actual value. Asserting only the
// flag would pass against an implementation that shipped the value
// alongside `readable: false`, which ADR 0099 §5 names as one of the four
// designs that look equivalent and are not.
func TestComposition_R3_NonDisclosureAtTheBoundary(t *testing.T) {
	env := newCompEnv(t)
	code := "metadata_test_r3"
	fid := env.gatedField(t, "r3", "asset")

	const secret = "SEVEN-THOUSAND-POUNDS"
	rr := putJSON(t, env.admin, fmt.Sprintf("/assets/%s/fields/%s", env.assetID, fid),
		map[string]any{"value_text": secret, "if_absent": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("seed secret: %d %s", rr.Code, rr.Body.String())
	}

	compPath := "/assets/" + env.assetID + "/field-composition"
	valPath := "/assets/" + env.assetID + "/fields"

	t.Run("R-3a without the grant", func(t *testing.T) {
		out := env.routerFor(t, nil, compReadCap, env.otherID)

		states, rawComp := composition(t, out, compPath)
		s, ok := states[code]
		if !ok {
			t.Fatal("the field must still be LISTED: a caller has to know the definition exists to know its term is unevaluable")
		}
		if s.Readable {
			t.Fatal("readable must be false for a caller without the effective grant")
		}
		assertAbsent(t, rawComp, secret, "composition response")

		_, rawVals := getRaw(t, out, valPath)
		assertAbsent(t, rawVals, secret, "asset field values response")
		// And the row is DROPPED rather than blanked, which is what makes
		// the value response indistinguishable from "never set" and why
		// the composition read exists at all.
		if containsFieldID(t, rawVals, fid) {
			t.Fatal("the withheld field still has a row in the value response; the row must be dropped, not blanked")
		}
	})

	t.Run("R-3b with the grant", func(t *testing.T) {
		in := env.routerFor(t, nil, compReadCap, env.teamID)

		states, _ := composition(t, in, compPath)
		if s := states[code]; !s.Readable {
			t.Fatalf("readable must be true for a caller holding the effective grant; got %+v", s)
		}
		_, rawVals := getRaw(t, in, valPath)
		if !containsSecret(rawVals, secret) {
			t.Fatalf("the caller HOLDING the grant must receive the value; body=%s", rawVals)
		}
	})
}

// TestComposition_GetAssetFieldsFiltersAtAll is the narrower claim on its
// own, because it is the one that did not hold before this sprint:
// GetAssetFields contained ZERO per-field capability checks, so any
// authenticated caller received every gated value on the asset.
func TestComposition_GetAssetFieldsFiltersAtAll(t *testing.T) {
	env := newCompEnv(t)
	fid := env.gatedField(t, "leak", "asset")
	const secret = "LEAK-CANARY-20B"
	if rr := putJSON(t, env.admin, fmt.Sprintf("/assets/%s/fields/%s", env.assetID, fid),
		map[string]any{"value_text": secret, "if_absent": true}); rr.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}
	plain := env.routerFor(t, nil, "", uuid.Nil)
	_, raw := getRaw(t, plain, "/assets/"+env.assetID+"/fields")
	assertAbsent(t, raw, secret, "GET /assets/{id}/fields for a caller holding nothing")
}

// TestComposition_UngatedValuesStillArrive is the counterweight, and it
// is not optional: a filter that dropped everything would pass every
// non-disclosure assertion above while breaking the product.
//
// ⚠️ CLASS B. Measured, not assumed: it PASSES on a6f01c64, because
// before this sprint GetAssetFields returned everything and therefore
// also returned this. It proves nothing was broken and must never be
// counted as fail-before evidence.
func TestComposition_UngatedValuesStillArrive(t *testing.T) {
	env := newCompEnv(t)
	fid := mustCreateField(t, env.admin, map[string]any{
		"code": "metadata_test_plain", "label": "plain", "type": "text",
	})
	const visible = "ORDINARY-VALUE"
	if rr := putJSON(t, env.admin, fmt.Sprintf("/assets/%s/fields/%s", env.assetID, fid),
		map[string]any{"value_text": visible, "if_absent": true}); rr.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}
	plain := env.routerFor(t, nil, "", uuid.Nil)
	_, raw := getRaw(t, plain, "/assets/"+env.assetID+"/fields")
	if !containsSecret(raw, visible) {
		t.Fatalf("an ungated value must still be returned; body=%s", raw)
	}
	// The mirrored columns survive the new filter too. `title` carries no
	// read capability, and exempting the mirrored branch from a security
	// filter because of what the data happens to look like is how the
	// exemption outlives its reason — so the filter runs on it and must
	// let it through.
	if !containsSecret(raw, "comp test asset") {
		t.Fatalf("the mirrored title projection must survive the read filter; body=%s", raw)
	}
}

// ---------------------------------------------------------------------------
// The COLLECTION side, where this matters MORE
// ---------------------------------------------------------------------------

// TestComposition_R1vsR2_Collection is the collection twin of the
// server-derived proof.
//
// It matters more here than on assets, and for a reason easy to miss:
// GET /collections/{id}/fields has ALWAYS filtered by read_capability,
// but it filters by DROPPING THE ROW, so a withheld value and a value
// that was never set arrive as the same nothing. Those two states have
// opposite consequences for a condition, so before this endpoint a
// collection form could not tell them apart at all.
//
// `collections` carries no team_id, so a team-scoped grant confers
// nothing here and the global holding is the whole answer. That is the
// difference between the two callers below, and it is a real difference
// in EFFECTIVE readability rather than an artefact.
func TestComposition_R1vsR2_Collection(t *testing.T) {
	env := newCompEnv(t)
	code := collectionTestPrefix + "r1r2"
	env.gatedField(t, "r1r2", "collection")

	path := "/collections/" + env.collID + "/field-composition"

	global := env.routerFor(t, []string{compReadCap}, "", uuid.Nil)
	states, _ := composition(t, global, path)
	if s, ok := states[code]; !ok || !s.Readable {
		t.Fatalf("R-1 (collection): a global holding must read the field; got %+v (present=%v)", s, ok)
	}

	// The SAME definition and the SAME collection; only the grant differs.
	scopedOnly := env.routerFor(t, nil, compReadCap, env.teamID)
	states2, _ := composition(t, scopedOnly, path)
	if s, ok := states2[code]; !ok || s.Readable {
		t.Fatalf("R-2 (collection): a team-scoped grant confers nothing on a collection, which has no team; got %+v (present=%v)", s, ok)
	}
}

// TestComposition_CollectionNonDisclosure asserts the collection value
// path keeps withholding, through the shared helper rather than its own
// `id.Can` call.
//
// ⚠️ CLASS B, and deliberately so. Measured on a6f01c64: it PASSES there,
// because GetCollectionFields has filtered by read_capability since
// before this sprint. That is exactly what makes it a preservation test:
// moving that handler onto the shared helper must not have widened or
// narrowed what a collection caller receives. The thing that did NOT hold
// before is the collection READABILITY read, which is
// TestComposition_R1vsR2_Collection, and that one fails on a6f01c64.
func TestComposition_CollectionNonDisclosure(t *testing.T) {
	env := newCompEnv(t)
	fid := env.gatedField(t, "secret", "collection")
	const secret = "COLLECTION-SECRET-20B"
	if rr := putJSON(t, env.admin, fmt.Sprintf("/collections/%s/fields/%s", env.collID, fid),
		map[string]any{"value_text": secret, "if_absent": true}); rr.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}
	out := env.routerFor(t, nil, compReadCap, env.teamID)
	_, raw := getRaw(t, out, "/collections/"+env.collID+"/fields")
	assertAbsent(t, raw, secret, "GET /collections/{id}/fields for a caller without the effective grant")

	in := env.routerFor(t, []string{compReadCap}, "", uuid.Nil)
	_, raw2 := getRaw(t, in, "/collections/"+env.collID+"/fields")
	if !containsSecret(raw2, secret) {
		t.Fatalf("the caller holding the grant globally must receive the collection value; body=%s", raw2)
	}
}

// ---------------------------------------------------------------------------
// R-4 and the composition set
// ---------------------------------------------------------------------------

// TestComposition_ArchivedIsNotListed is R-4's server half and A-19's
// step 5 mechanism: an archived definition never appears on a composition
// surface, so it resolves to nothing, so a term naming it is unevaluable
// and the whole condition fails open.
//
// The stored condition is NOT touched by any of that, which the
// end-to-end drift test asserts separately.
func TestComposition_ArchivedIsNotListed(t *testing.T) {
	env := newCompEnv(t)
	fid := mustCreateField(t, env.admin, map[string]any{
		"code": "metadata_test_arch", "label": "arch", "type": "text",
	})
	states, _ := composition(t, env.admin, "/assets/"+env.assetID+"/field-composition")
	if _, ok := states["metadata_test_arch"]; !ok {
		t.Fatal("an active field must be listed before the discriminator means anything")
	}
	if rr := patchJSON(t, env.admin, "/fields/"+fid, map[string]any{"status": "archived"}); rr.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rr.Code, rr.Body.String())
	}
	states2, _ := composition(t, env.admin, "/assets/"+env.assetID+"/field-composition")
	if _, ok := states2["metadata_test_arch"]; ok {
		t.Fatal("an archived field must not appear in the composition set: it is never drawn, so reporting readability for it would describe a control that is not there")
	}
	// And DEPRECATED must stay, because edit surfaces render active and
	// deprecated together (#528). Without this arm the test above would
	// also pass against an implementation that listed only `active`.
	fid2 := mustCreateField(t, env.admin, map[string]any{
		"code": "metadata_test_dep", "label": "dep", "type": "text",
	})
	if rr := patchJSON(t, env.admin, "/fields/"+fid2, map[string]any{"status": "deprecated"}); rr.Code != http.StatusOK {
		t.Fatalf("deprecate: %d %s", rr.Code, rr.Body.String())
	}
	states3, _ := composition(t, env.admin, "/assets/"+env.assetID+"/field-composition")
	if _, ok := states3["metadata_test_dep"]; !ok {
		t.Fatal("a DEPRECATED field must stay in the composition set: the edit surfaces render it, so a condition naming it must keep evaluating")
	}
}

// TestComposition_SubjectKindsDoNotCross: the asset read must not report
// collection definitions and vice versa, or a form would evaluate a term
// against a field that has no value on its subject.
func TestComposition_SubjectKindsDoNotCross(t *testing.T) {
	env := newCompEnv(t)
	mustCreateField(t, env.admin, map[string]any{
		"code": "metadata_test_akind", "label": "a", "type": "text",
	})
	mustCreateField(t, env.admin, map[string]any{
		"code": collectionTestPrefix + "ckind", "label": "c", "type": "text",
		"subject_kind": "collection",
	})
	assetStates, _ := composition(t, env.admin, "/assets/"+env.assetID+"/field-composition")
	if _, ok := assetStates[collectionTestPrefix+"ckind"]; ok {
		t.Fatal("a collection definition leaked into the asset composition set")
	}
	if _, ok := assetStates["metadata_test_akind"]; !ok {
		t.Fatal("the asset definition is missing from the asset composition set")
	}
	collStates, _ := composition(t, env.admin, "/collections/"+env.collID+"/field-composition")
	if _, ok := collStates["metadata_test_akind"]; ok {
		t.Fatal("an asset definition leaked into the collection composition set")
	}
	if _, ok := collStates[collectionTestPrefix+"ckind"]; !ok {
		t.Fatal("the collection definition is missing from the collection composition set")
	}
}

// TestComposition_RequiresAuthentication keeps the endpoints on the same
// floor as the value reads they sit beside.
func TestComposition_RequiresAuthentication(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := metadata.NewHandler(pool, logger, nil)
	router := chi.NewRouter() // no identity middleware at all
	openapi.HandlerFromMux(openapi.NewStrictHandler(metaShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)

	for _, path := range []string{
		"/assets/" + uuid.New().String() + "/field-composition",
		"/collections/" + uuid.New().String() + "/field-composition",
	} {
		rr, _ := getRaw(t, router, path)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s anonymous: status=%d, want 401", path, rr.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// Byte-level helpers
// ---------------------------------------------------------------------------

// assertAbsent fails when `secret` appears anywhere in the response
// BYTES. Deliberately a substring search over the whole body rather than
// a structured field check: the requirement is that the value does not
// cross the wire, and a structured check only looks where somebody
// thought to look.
func assertAbsent(t *testing.T, body []byte, secret, what string) {
	t.Helper()
	if containsSecret(body, secret) {
		t.Fatalf("%s DISCLOSED the protected value %q; body=%s", what, secret, body)
	}
}

func containsSecret(body []byte, secret string) bool {
	return len(secret) > 0 && len(body) > 0 &&
		bytesContains(body, []byte(secret))
}

func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func containsFieldID(t *testing.T, body []byte, fieldID string) bool {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode value list: %v body=%s", err, body)
	}
	for _, r := range rows {
		if s, ok := r["field_id"].(string); ok && s == fieldID {
			return true
		}
	}
	return false
}
