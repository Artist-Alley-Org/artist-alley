// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end cover for v0.11.0 sprint 2 — a vocabulary as a searchable
// resource, gated extension, and alias-then-merge (#789, #1016,
// ADR 0092).
//
// Driven through the real HTTP handlers, like every other file in this
// family, because the questions worth asking are all about whether a
// rule is REACHED. #789 part A shipped a correct minter that no user
// edit could call for a while; a unit test on the resolver would have
// been green throughout. The same trap is live for every rule added
// here — a capability check the handler never consults, a canonical
// slug computed and not written back, a tombstone the write path never
// looks at.
//
// Every assertion that matters reads the state back from the API or
// straight from the column, never from the write's own echo: a handler
// that echoes what it was sent passes a body assertion on the bug.
package metadata_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func getJSON(t *testing.T, r chi.Router, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

// searchValues calls the value-search endpoint and decodes the page.
func searchValues(t *testing.T, r chi.Router, fieldID, query string) openapi.VocabularyValuePage {
	t.Helper()
	rr := getJSON(t, r, "/fields/"+fieldID+"/values"+query)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fields/%s/values%s: status=%d body=%s", fieldID, query, rr.Code, rr.Body.String())
	}
	var page openapi.VocabularyValuePage
	mustDecode(t, rr.Body.Bytes(), &page)
	return page
}

// bigVocabulary builds a synthetic vocabulary of n terms.
//
// #1191's spec did the same thing for the same reason, and it bears
// repeating: the seeded catalogue's largest real vocabulary is
// `keywords` at 21 values, and NOTHING about behaviour at production
// size can be learned from 21. A bound that is never reached is a bound
// that has never been tested.
//
// The labels are deliberately shaped so one prefix admits hundreds
// ("Pigment ...") while another admits exactly one, which is what lets
// the cap and the total be asserted separately.
func bigVocabulary(n int) map[string]any {
	values := make([]any, 0, n+2)
	for i := 0; i < n; i++ {
		values = append(values, map[string]any{
			"value": fmt.Sprintf("pigment-%04d", i),
			"label": fmt.Sprintf("Pigment %04d", i),
		})
	}
	values = append(values,
		map[string]any{"value": "vermilion", "label": "Vermilion", "aliases": []string{"cinnabar"}},
		map[string]any{"value": "ultramarine", "label": "Ultramarine"},
	)
	return map[string]any{"values": values}
}

// aliasVocabulary is the small curation fixture: a canonical term with
// an alias, plus two terms a merge can be run between.
func aliasVocabulary() map[string]any {
	return map[string]any{"values": []any{
		map[string]any{"value": "gb", "label": "United Kingdom", "aliases": []string{"uk", "britain"}},
		map[string]any{"value": "fr", "label": "France"},
		map[string]any{"value": "de", "label": "Germany"},
	}}
}

// ---------------------------------------------------------------------------
// A — the value-search endpoint
// ---------------------------------------------------------------------------

// The headline for ADR 0092 §1: a query against a vocabulary far larger
// than anything seeded returns a BOUNDED page, and says how much it is
// not showing.
func TestVocabularySearch_BoundHoldsAtProductionSize(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "bigvocab", bigVocabulary(2000))

	page := searchValues(t, env.router, fid, "?q=Pigment&limit=25")
	if page.Returned != 25 || len(page.Values) != 25 {
		t.Errorf("returned=%d len(values)=%d, want 25", page.Returned, len(page.Values))
	}
	if page.Matched != 2000 {
		t.Errorf("matched=%d, want 2000 — the cap must not distort the total", page.Matched)
	}
	if !page.Truncated {
		t.Error("truncated=false on 2000 matches capped at 25")
	}
	if page.VocabularySize != 2002 {
		t.Errorf("vocabulary_size=%d, want 2002", page.VocabularySize)
	}

	// The ceiling is enforced server-side. A caller asking for the whole
	// vocabulary is clamped, not obeyed — that is the entire point of
	// moving the search here.
	wide := searchValues(t, env.router, fid, "?q=Pigment&limit=100000")
	if wide.Limit != 200 || wide.Returned != 200 {
		t.Errorf("limit=%d returned=%d, want 200 — the ceiling is not enforced",
			wide.Limit, wide.Returned)
	}

	// And a default with no limit at all is still bounded.
	def := searchValues(t, env.router, fid, "?q=Pigment")
	if def.Returned != 50 {
		t.Errorf("default returned=%d, want 50", def.Returned)
	}
}

// A term is findable by its alias, and the exact hit outranks the
// hundreds of prefix hits around it.
func TestVocabularySearch_MatchesAliasAndRanksExactFirst(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "ranked", bigVocabulary(300))

	byAlias := searchValues(t, env.router, fid, "?q=cinnabar")
	if byAlias.Matched != 1 || byAlias.Values[0].Value != "vermilion" {
		t.Fatalf("alias search returned %+v; want vermilion", byAlias.Values)
	}

	exact := searchValues(t, env.router, fid, "?q=pigment-0100&match=substring")
	if len(exact.Values) == 0 || exact.Values[0].Value != "pigment-0100" {
		t.Fatalf("first row = %+v, want the exact hit pigment-0100", exact.Values)
	}
}

// `status` decides what is offered. The default is the set a picker MAY
// offer; `any` is the curation view, and it is the only place a
// tombstone is visible.
func TestVocabularySearch_StatusFilterAndTombstoneVisibility(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "statuses", map[string]any{"values": []any{
		map[string]any{"value": "live", "label": "Live"},
		map[string]any{"value": "old", "label": "Old", "status": "archived", "replaced_by": "live"},
		map[string]any{"value": "dead", "label": "Dead", "status": "archived"},
	}})

	active := searchValues(t, env.router, fid, "")
	if active.Matched != 1 || active.Values[0].Value != "live" {
		t.Fatalf("default status filter returned %+v; want only the active term", active.Values)
	}

	all := searchValues(t, env.router, fid, "?status=any")
	if all.Matched != 3 {
		t.Fatalf("status=any matched %d, want 3", all.Matched)
	}
	var tomb, hard *openapi.VocabularyValue
	for i := range all.Values {
		switch all.Values[i].Value {
		case "old":
			tomb = &all.Values[i]
		case "dead":
			hard = &all.Values[i]
		}
	}
	if tomb == nil || hard == nil {
		t.Fatalf("expected both archived terms in the curation view, got %+v", all.Values)
	}
	// The distinguishing property, and the reason ADR 0092 refuses to
	// delete on merge: a tombstone carries where it went, a hard retire
	// does not, and a term that never existed appears at all.
	if tomb.ReplacedBy == nil || *tomb.ReplacedBy != "live" {
		t.Errorf("tombstone.replaced_by = %v, want live", tomb.ReplacedBy)
	}
	if hard.ReplacedBy != nil {
		t.Errorf("a hard archive carries replaced_by = %v; it must not", *hard.ReplacedBy)
	}
}

// can_extend is the server's answer, not the client's guess, and it
// tracks the CALLER rather than the field.
func TestVocabularySearch_CanExtendTracksTheCaller(t *testing.T) {
	env := newVocabEnv(t)
	openID := env.openField(t, "canext", aliasVocabulary())
	closedID := env.assetField(t, "canext_closed", "multi_select", aliasVocabulary())

	holder := searchValues(t, env.router, openID, "")
	if !holder.OpenVocabulary || !holder.CanExtend {
		t.Errorf("holder: open_vocabulary=%v can_extend=%v, want true/true",
			holder.OpenVocabulary, holder.CanExtend)
	}
	closed := searchValues(t, env.router, closedID, "")
	if closed.OpenVocabulary || closed.CanExtend {
		t.Errorf("closed field: open_vocabulary=%v can_extend=%v, want false/false",
			closed.OpenVocabulary, closed.CanExtend)
	}

	// Same field, a caller holding fields.admin but NOT the extend
	// capability. can_extend must flip, or a client rendering from it
	// would offer a create row the write path refuses.
	narrow, _ := makeRouterWithCaps(t, env.pool, metadata.CapFieldsAdmin)
	page := searchValues(t, narrow, openID, "")
	if !page.OpenVocabulary {
		t.Error("open_vocabulary is a property of the FIELD and must not move with the caller")
	}
	if page.CanExtend {
		t.Error("can_extend=true for a caller without fields.vocabulary.extend")
	}
}

// A field whose values are capability-gated must not leak its
// vocabulary. The term list IS the set of values the field can hold.
func TestVocabularySearch_HonoursReadCapability(t *testing.T) {
	env := newVocabEnv(t)
	fid := mustCreateField(t, env.router, map[string]any{
		"code": "mtv_secretvocab", "label": "secret", "type": "multi_select",
		"options": aliasVocabulary(), "read_capability": "test.no.one.holds.this",
	})
	rr := getJSON(t, env.router, "/fields/"+fid+"/values")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403 — a gated field's vocabulary is a disclosure; body=%s",
			rr.Code, rr.Body.String())
	}
}

func TestVocabularySearch_UnknownFieldIs404(t *testing.T) {
	env := newVocabEnv(t)
	rr := getJSON(t, env.router, "/fields/00000000-0000-0000-0000-000000000000/values")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// B — extension is gated
// ---------------------------------------------------------------------------

// A capability HOLDER types a new term through the real edit path and
// it persists. Read back from the API, not from the write's echo.
func TestVocabularyExtend_HolderCreatesAndItPersists(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "extholder", aliasVocabulary())

	status, _ := env.putAsset(t, fid, map[string]any{
		"value_options": []string{"fr", "Iceland"},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}

	// The API, not the echo: a term minted into a cached copy and never
	// persisted looks identical in a write response.
	page := searchValues(t, env.router, fid, "?q=iceland")
	if page.Matched != 1 {
		t.Fatalf("the created term is not in the vocabulary: %+v", page)
	}
	if page.Values[0].Value != "iceland" || page.Values[0].Label != "Iceland" {
		t.Errorf("created term = %+v, want value=iceland label=Iceland", page.Values[0])
	}
	// And the ROW stores the slug, not the text that was typed.
	if got := env.storedOptions(t, fid); len(got) != 2 || got[1] != "iceland" {
		t.Errorf("value_options = %v, want [fr iceland]", got)
	}
}

// A NON-holder sees no create affordance (can_extend=false, asserted
// above) and a forged request is refused CLEANLY: 422 with a reason
// that says which of the two possible causes applied, not a 500 and not
// a silent success.
func TestVocabularyExtend_NonHolderIsRefusedWithItsOwnReason(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "extnonholder", aliasVocabulary())

	// fields.admin, deliberately: proves the new capability is not
	// implied by schema authority.
	narrow, _ := makeRouterWithCaps(t, env.pool, metadata.CapFieldsAdmin)
	rr := putJSON(t, narrow, fmt.Sprintf("/assets/%s/fields/%s", env.assetID, fid),
		map[string]any{"value_options": []string{"Iceland"}})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	mustDecode(t, rr.Body.Bytes(), &body)
	if body["reason"] != string(openapi.VocabularyExtensionForbidden) {
		t.Errorf("reason=%v, want %q — a caller told `unknown_option` would go looking "+
			"for a spelling mistake instead of asking for a grant",
			body["reason"], openapi.VocabularyExtensionForbidden)
	}
	if body["option"] != "Iceland" {
		t.Errorf("option=%v, want the offending term", body["option"])
	}

	// Nothing was created, and nothing was stored.
	page := searchValues(t, env.router, fid, "?q=iceland&status=any")
	if page.Matched != 0 {
		t.Errorf("the refused term was created anyway: %+v", page.Values)
	}
}

// A non-holder can still USE the vocabulary. Extension is not
// membership: the refusal above must be about the new term only.
func TestVocabularyExtend_NonHolderMayStillPickExistingTerms(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "extexisting", aliasVocabulary())

	narrow, _ := makeRouterWithCaps(t, env.pool, metadata.CapFieldsAdmin)
	rr := putJSON(t, narrow, fmt.Sprintf("/assets/%s/fields/%s", env.assetID, fid),
		map[string]any{"value_options": []string{"fr", "uk"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// `uk` is an ALIAS. Resolving one is matching, not extending, so a
	// caller without the capability must still get the redirect.
	if got := env.storedOptions(t, fid); len(got) != 2 || got[1] != "gb" {
		t.Errorf("value_options = %v, want [fr gb] — an alias must resolve for a "+
			"caller who may not create", got)
	}
}

// ---------------------------------------------------------------------------
// C — alias, on both an open and a CLOSED vocabulary
// ---------------------------------------------------------------------------

func TestVocabularyAlias_RedirectsOnAClosedSelectField(t *testing.T) {
	env := newVocabEnv(t)
	// `select`: one slug in value_text, closed. Curation happens on
	// exactly these fields, so the redirect has to work here.
	fid := env.assetField(t, "aliasselect", "select", aliasVocabulary())

	status, _ := env.putAsset(t, fid, map[string]any{"value_text": "uk"})
	if status != http.StatusOK {
		t.Fatalf("status=%d, want 200", status)
	}
	var stored string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT value_text FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		env.assetID, fid).Scan(&stored); err != nil {
		t.Fatalf("read value_text: %v", err)
	}
	if stored != "gb" {
		t.Fatalf("value_text = %q, want gb — the gate approved the alias and the row "+
			"kept the text, which is the silent half of this bug", stored)
	}
}

// The alias is non-retroactive and reversible: removing it stops the
// redirect and leaves the rows it steered alone.
func TestVocabularyAlias_IsNonRetroactiveAndReversible(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "aliasrev", "multi_select", aliasVocabulary())

	if status, _ := env.putAsset(t, fid, map[string]any{"value_options": []string{"uk"}}); status != http.StatusOK {
		t.Fatalf("first write status=%d", status)
	}
	if got := env.storedOptions(t, fid); len(got) != 1 || got[0] != "gb" {
		t.Fatalf("value_options = %v, want [gb]", got)
	}

	// Drop the alias.
	env.retire(t, fid, map[string]any{"values": []any{
		map[string]any{"value": "gb", "label": "United Kingdom"},
		map[string]any{"value": "fr", "label": "France"},
		map[string]any{"value": "de", "label": "Germany"},
	}})

	// The stored row is untouched — nothing was rewritten.
	if got := env.storedOptions(t, fid); len(got) != 1 || got[0] != "gb" {
		t.Errorf("value_options = %v after removing the alias; nothing may be rewritten", got)
	}
	// And the redirect is gone: `uk` is once again a slug the field
	// does not have.
	rr := putJSON(t, env.router, fmt.Sprintf("/assets/%s/fields/%s", env.assetID, fid),
		map[string]any{"value_options": []string{"uk"}})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=%d after the alias was removed, want 422 — an alias that "+
			"outlives its record is not reversible; body=%s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// C — merge
// ---------------------------------------------------------------------------

func mergeReq(t *testing.T, r chi.Router, fieldID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	rr := postJSON(t, r, "/fields/"+fieldID+"/values/merge", body)
	return rr.Code, decodeBody(t, rr.Body.Bytes())
}

// The headline: a merge rewrites stored values on both assets and
// collections, tombstones the source, and the tombstone keeps
// resolving.
func TestVocabularyMerge_RewritesValuesAndLeavesATombstone(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "mergeasset", aliasVocabulary())
	cfid := mustCreateField(t, env.router, map[string]any{
		"code": collectionTestPrefix + "mergecoll", "label": "mergecoll",
		"type": "multi_select", "subject_kind": "collection",
		"options": aliasVocabulary(), "open_vocabulary": true,
	})

	if status, _ := env.putAsset(t, fid, map[string]any{"value_options": []string{"de", "fr"}}); status != http.StatusOK {
		t.Fatalf("seed asset value")
	}
	if status, _ := env.putCollection(t, cfid, map[string]any{"value_options": []string{"de"}}); status != http.StatusOK {
		t.Fatalf("seed collection value")
	}

	status, body := mergeReq(t, env.router, fid, map[string]any{
		"source": "de", "target": "fr",
		"reason": "the two terms named the same studio after the 2026 rename",
	})
	if status != http.StatusOK {
		t.Fatalf("merge status=%d body=%v", status, body)
	}
	if body["asset_values_rewritten"] != float64(1) {
		t.Errorf("asset_values_rewritten=%v, want 1", body["asset_values_rewritten"])
	}
	if body["tombstoned"] != true {
		t.Errorf("tombstoned=%v, want true", body["tombstoned"])
	}

	// The asset row: `de` became `fr`, and because the row ALREADY held
	// `fr`, the result is one term rather than a duplicate pair.
	if got := env.storedOptions(t, fid); len(got) != 1 || got[0] != "fr" {
		t.Errorf("value_options = %v, want [fr] — a row holding both sides of a merge "+
			"must dedupe, not end up with the target twice", got)
	}

	// The tombstone exists, is archived, and points where the value went.
	page := searchValues(t, env.router, fid, "?q=de&status=any")
	var tomb *openapi.VocabularyValue
	for i := range page.Values {
		if page.Values[i].Value == "de" {
			tomb = &page.Values[i]
		}
	}
	if tomb == nil {
		t.Fatal("the source term was DELETED — a merged-away term is then " +
			"indistinguishable from one that never existed, permanently, on every peer")
	}
	if tomb.Status != openapi.VocabularyValueStatus(metadata.OptionArchived) {
		t.Errorf("tombstone status=%q, want archived", tomb.Status)
	}
	if tomb.ReplacedBy == nil || *tomb.ReplacedBy != "fr" {
		t.Errorf("tombstone replaced_by=%v, want fr", tomb.ReplacedBy)
	}

	// A late write naming the merged-away term still lands somewhere
	// real — the property a federated peer that predates the merge
	// depends on.
	if status, _ := env.putAsset(t, fid, map[string]any{"value_options": []string{"de"}}); status != http.StatusOK {
		t.Fatalf("a write naming the tombstone was refused (status=%d)", status)
	}
	if got := env.storedOptions(t, fid); len(got) != 1 || got[0] != "fr" {
		t.Errorf("value_options = %v after writing the tombstoned slug, want [fr]", got)
	}

	// A per-value history row was written for the rewrite, so the
	// asset's own audit trail does not describe a value it stopped
	// holding.
	var n int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM asset_field_value_history
		  WHERE asset_id = $1 AND field_id = $2 AND set_by = 'computed'`,
		env.assetID, fid).Scan(&n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if n != 1 {
		t.Errorf("merge history rows = %d, want 1", n)
	}
}

// The dry run reports what would change and changes nothing — counts
// measured by the real statements, then rolled back.
func TestVocabularyMerge_DryRunChangesNothing(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "mergedry", aliasVocabulary())
	if status, _ := env.putAsset(t, fid, map[string]any{"value_options": []string{"de"}}); status != http.StatusOK {
		t.Fatalf("seed value")
	}

	status, body := mergeReq(t, env.router, fid, map[string]any{
		"source": "de", "target": "fr", "dry_run": true,
		"reason": "checking the blast radius before committing to it",
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if body["asset_values_rewritten"] != float64(1) {
		t.Errorf("dry run reported %v rewrites, want 1 — a preview that cannot count "+
			"is not a preview", body["asset_values_rewritten"])
	}
	if body["tombstoned"] != false {
		t.Errorf("tombstoned=%v on a dry run", body["tombstoned"])
	}
	if got := env.storedOptions(t, fid); len(got) != 1 || got[0] != "de" {
		t.Errorf("value_options = %v after a DRY RUN, want [de]", got)
	}
	page := searchValues(t, env.router, fid, "?q=de")
	if page.Matched != 1 || page.Values[0].Status != openapi.VocabularyValueStatus(metadata.OptionActive) {
		t.Errorf("the dry run tombstoned the term: %+v", page.Values)
	}
}

// The friction is real: the capability, and the reason.
func TestVocabularyMerge_Refusals(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.openField(t, "mergerefuse", aliasVocabulary())

	t.Run("without the capability", func(t *testing.T) {
		// fields.admin AND extend, but not merge — curating an option
		// list is not authority to rewrite other people's records.
		narrow, _ := makeRouterWithCaps(t, env.pool,
			metadata.CapFieldsAdmin, metadata.CapVocabularyExtend)
		status, _ := mergeReq(t, narrow, fid, map[string]any{
			"source": "de", "target": "fr", "reason": "no capability held",
		})
		if status != http.StatusForbidden {
			t.Fatalf("status=%d, want 403", status)
		}
	})

	t.Run("without a reason", func(t *testing.T) {
		status, _ := mergeReq(t, env.router, fid, map[string]any{
			"source": "de", "target": "fr", "reason": "",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("status=%d, want 400 — an unexplained merge is not auditable", status)
		}
	})

	t.Run("source is not a term", func(t *testing.T) {
		status, body := mergeReq(t, env.router, fid, map[string]any{
			"source": "atlantis", "target": "fr", "reason": "typo in the source slug",
		})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%v, want 422", status, body)
		}
	})

	t.Run("source equals target", func(t *testing.T) {
		status, _ := mergeReq(t, env.router, fid, map[string]any{
			"source": "fr", "target": "fr", "reason": "merging a term into itself",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("status=%d, want 400", status)
		}
	})

	// Nothing above may have changed anything.
	page := searchValues(t, env.router, fid, "?status=any")
	if page.Matched != 3 {
		t.Errorf("a refused merge changed the vocabulary: %d terms, want 3", page.Matched)
	}
}

// ---------------------------------------------------------------------------
// D — `searchable` (#1016)
// ---------------------------------------------------------------------------

// The acceptance #1016 wrote for branch (1), asserted on the SEARCH
// DOCUMENT rather than on the stored flag: a field whose `searchable`
// is turned off stops answering text queries, while its values stay
// readable through the API.
//
// The gap this covers is specifically the ORDER: the flag was always
// read by rebuild_asset_search_text(), and that function only ever ran
// from a trigger on the VALUE. So turning the flag off left every
// already-indexed value in place, and the operator's action did nothing
// until something happened to touch each asset.
func TestSearchable_TurningItOffRemovesAlreadyIndexedValues(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "searchflag", "text", map[string]any{})

	if status, _ := env.putAsset(t, fid, map[string]any{
		"value_text": "quintessential zibeline",
	}); status != http.StatusOK {
		t.Fatalf("seed value")
	}

	indexed := func() bool {
		t.Helper()
		var hit bool
		if err := env.pool.QueryRow(context.Background(),
			`SELECT search_text @@ plainto_tsquery('english', 'zibeline') FROM assets WHERE id = $1`,
			env.assetID).Scan(&hit); err != nil {
			t.Fatalf("query search_text: %v", err)
		}
		return hit
	}

	if !indexed() {
		t.Fatal("a searchable field's value never reached the search document; " +
			"the rest of this test would pass vacuously")
	}

	rr := patchJSON(t, env.router, "/fields/"+fid, map[string]any{"searchable": false})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch searchable: %d %s", rr.Code, rr.Body.String())
	}

	if indexed() {
		t.Error("the value still answers a text query after `searchable` was turned off — " +
			"which is the state #1016 called shipping the lie")
	}

	// …and the value is still READABLE. `searchable` governs findability
	// and nothing else.
	listRR := getJSON(t, env.router, "/assets/"+env.assetID+"/fields")
	if listRR.Code != http.StatusOK {
		t.Fatalf("list values: %d", listRR.Code)
	}
	var values []openapi.AssetFieldValue
	mustDecode(t, listRR.Body.Bytes(), &values)
	found := false
	for _, v := range values {
		if v.FieldId.String() == fid {
			found = true
			if v.ValueText == nil || *v.ValueText != "quintessential zibeline" {
				t.Errorf("value_text = %v, want the stored text", v.ValueText)
			}
		}
	}
	if !found {
		t.Error("a non-searchable field's value disappeared from the API; " +
			"`searchable` must not gate readability")
	}

	// Turning it back on restores findability, so the control is a
	// toggle rather than a one-way door.
	rr = patchJSON(t, env.router, "/fields/"+fid, map[string]any{"searchable": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch searchable back: %d %s", rr.Code, rr.Body.String())
	}
	if !indexed() {
		t.Error("turning `searchable` back on did not restore the value to the search document")
	}
}

// Archiving a field is the other conjunct of the same rule.
func TestSearchable_ArchivingAFieldRemovesItsValuesFromSearch(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "searcharchive", "text", map[string]any{})
	if status, _ := env.putAsset(t, fid, map[string]any{
		"value_text": "polyphonic marmalade",
	}); status != http.StatusOK {
		t.Fatalf("seed value")
	}

	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/fields/"+fid, nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("archive field: %d %s", rr.Code, rr.Body.String())
	}

	var hit bool
	if err := env.pool.QueryRow(context.Background(),
		`SELECT search_text @@ plainto_tsquery('english', 'marmalade') FROM assets WHERE id = $1`,
		env.assetID).Scan(&hit); err != nil {
		t.Fatalf("query search_text: %v", err)
	}
	if hit {
		t.Error("an archived field's values still answer text queries")
	}
}

// A relabel must NOT trigger the rebuild — it would rewrite a tsvector
// per asset for a change nothing in the search document reads.
func TestSearchable_UnrelatedEditDoesNotRebuild(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "searchnoop", "text", map[string]any{})
	if status, _ := env.putAsset(t, fid, map[string]any{"value_text": "kaleidoscopic"}); status != http.StatusOK {
		t.Fatalf("seed value")
	}
	rr := patchJSON(t, env.router, "/fields/"+fid, map[string]any{"label": "Renamed"})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch label: %d", rr.Code)
	}
	var hit bool
	if err := env.pool.QueryRow(context.Background(),
		`SELECT search_text @@ plainto_tsquery('english', 'kaleidoscopic') FROM assets WHERE id = $1`,
		env.assetID).Scan(&hit); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !hit {
		t.Error("a relabel dropped the value out of the search document")
	}
}

// ---------------------------------------------------------------------------
// The options document survives a round trip through the API
// ---------------------------------------------------------------------------

// Aliases are stored, echoed and re-read. Cheap, and it is the check
// that catches an alias silently dropped by a normaliser.
func TestVocabularyAlias_SurvivesTheOptionsEditor(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "aliaspersist", "multi_select", aliasVocabulary())

	var raw []byte
	if err := env.pool.QueryRow(context.Background(),
		`SELECT options FROM field_definition WHERE id = $1`, fid).Scan(&raw); err != nil {
		t.Fatalf("read options: %v", err)
	}
	var doc struct {
		Values []metadata.FieldOption `json:"values"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode options %s: %v", raw, err)
	}
	for _, v := range doc.Values {
		if v.Value != "gb" {
			continue
		}
		if len(v.Aliases) != 2 || v.Aliases[0] != "uk" || v.Aliases[1] != "britain" {
			t.Fatalf("aliases = %v, want [uk britain]", v.Aliases)
		}
		return
	}
	t.Fatal("the aliased term is missing from the stored document")
}
