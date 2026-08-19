// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// End-to-end cover for the field participation flags (#1173, ADR 0092 §3).
//
// A field can now say where it appears. Three things have to be true
// for that to be safe to ship mid-release, and each is a test below.
//
//  1. UNSET MEANS TODAY. A field nobody configures must be offered
//     everywhere it is offered now. The flags therefore default TRUE
//     (and NULL for the tab), and the assertion is on the PERSISTED
//     row rather than on the create response — a handler that echoed
//     its own defaults back without writing them would pass a body
//     assertion and fail the next reader.
//
//  2. THE `searchable` BOUNDARY HOLDS. `show_in_advanced_search` is a
//     control-visibility flag; `searchable` is what puts a field's
//     text into `assets.search_text`. Turning the first off must leave
//     the second — and the index it governs — completely alone. If
//     these ever collapse into one setting, an operator tidying the
//     search FORM silently makes records unfindable, which is the
//     defect ADR 0090 exists to warn about and the reason this sprint
//     added a new column instead of reusing the old one.
//
//  3. "NO TAB" HAS EXACTLY ONE REPRESENTATION. NULL. The empty string
//     is refused on write so that a form posting a blank box cannot
//     invent a third state that neither the CHECK constraint nor any
//     future tab renderer knows how to read.
//
// These drive the real HTTP handlers rather than the query layer,
// because the interesting failures are all about whether the handler
// PASSES the flag through: sqlc will happily generate a params field
// that nothing ever assigns, and a unit test on the query would not
// notice.
package metadata_test

import (
	"context"
	"net/http"
	"testing"
)

// participationRow is the persisted state of the three flags, read
// from the DATABASE. Every assertion in this file goes through here
// rather than through a response body — see the note about echoed
// writes at the top.
type participationRow struct {
	AdvancedSearch bool
	Upload         bool
	EditTab        *string
	Searchable     bool
}

func readParticipation(t *testing.T, e *vocabEnv, fieldID string) participationRow {
	t.Helper()
	var got participationRow
	if err := e.pool.QueryRow(context.Background(),
		`SELECT show_in_advanced_search, show_on_upload, edit_tab, searchable
		   FROM field_definition WHERE id = $1`, fieldID).
		Scan(&got.AdvancedSearch, &got.Upload, &got.EditTab, &got.Searchable); err != nil {
		t.Fatalf("read participation flags: %v", err)
	}
	return got
}

// ---------------------------------------------------------------------------
// 1. Unset means today's behaviour
// ---------------------------------------------------------------------------

// A field created without a word about participation must be offered
// on every surface, because that is what every field on every existing
// install does the moment before the migration runs. Defaulting these
// to FALSE would have emptied the advanced form and the upload form on
// the deploy that added the feature.
func TestParticipation_DefaultsAreTodaysBehaviour(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "partdefaults", "multi_select", vocabSet())

	got := readParticipation(t, env, fid)
	if !got.AdvancedSearch {
		t.Error("a brand-new field is NOT offered on advanced search; " +
			"unset must mean today's behaviour, and today every field is offered")
	}
	if !got.Upload {
		t.Error("a brand-new field is NOT offered at upload; " +
			"unset must mean today's behaviour")
	}
	if got.EditTab != nil {
		t.Errorf("a brand-new field was assigned edit_tab %q; unassigned must be NULL", *got.EditTab)
	}

	// The API says the same thing. A surface reading `show_in_advanced_search`
	// off the payload must see `true`, not an absent key it would have to
	// guess about — the whole point of the flag is that surfaces stop guessing.
	rr := getJSON(t, env.router, "/fields/"+fid)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET field: %d %s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr.Body.Bytes())
	if body["show_in_advanced_search"] != true {
		t.Errorf("payload show_in_advanced_search = %v, want true", body["show_in_advanced_search"])
	}
	if body["show_on_upload"] != true {
		t.Errorf("payload show_on_upload = %v, want true", body["show_on_upload"])
	}
	if v, ok := body["edit_tab"]; ok && v != nil {
		t.Errorf("payload edit_tab = %v, want absent or null", v)
	}
}

// The nine shipped definitions are the ones an existing install
// actually has, and the migration backfilled them by column default
// rather than by an UPDATE naming each one. Assert the result on the
// real rows: a field the seed shipped must be offered everywhere,
// which is what makes "the advanced page renders the same list before
// and after" true rather than hoped for.
func TestParticipation_SeededFieldsAreUnchanged(t *testing.T) {
	env := newVocabEnv(t)

	// Scoped past the codes the test suites mint and sweep, so a probe
	// field another file deliberately opted out cannot be mistaken for a
	// migration that touched a real row.
	var offCount int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM field_definition
		  WHERE (NOT show_in_advanced_search OR NOT show_on_upload OR edit_tab IS NOT NULL)
		    AND code NOT LIKE 'metadata_test_%' AND code NOT LIKE 'mtv_%'
		    AND code NOT LIKE 'nonadmin_%'   AND code NOT LIKE 'mcoltest_%'
		    AND code NOT LIKE 'probe_%'      AND code NOT LIKE 'ui18_%'
		    AND code NOT LIKE 'sprint%'`).
		Scan(&offCount); err != nil {
		t.Fatalf("count opted-out fields: %v", err)
	}
	if offCount != 0 {
		t.Errorf("%d pre-existing field(s) came out of the migration with a participation flag "+
			"already set; the migration must leave every row rendering exactly as it did", offCount)
	}
}

// ---------------------------------------------------------------------------
// 2. The `searchable` boundary
// ---------------------------------------------------------------------------

// Turning a field off the advanced form removes a CONTROL. It must not
// touch the index, the stored values, or `searchable` itself — the
// separation this sprint added a column to preserve.
func TestParticipation_AdvancedSearchOffDoesNotUnindexTheField(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "partboundary", "text", map[string]any{})

	if status, body := env.putAsset(t, fid, map[string]any{
		"value_text": "quintessential zibeline",
	}); status != http.StatusOK {
		t.Fatalf("seed value: status=%d body=%v", status, body)
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
		t.Fatal("the value never reached the search document; the rest of this test " +
			"would pass vacuously")
	}

	rr := patchJSON(t, env.router, "/fields/"+fid,
		map[string]any{"show_in_advanced_search": false})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch show_in_advanced_search: %d %s", rr.Code, rr.Body.String())
	}

	got := readParticipation(t, env, fid)
	if got.AdvancedSearch {
		t.Error("show_in_advanced_search was not persisted as false")
	}
	if !got.Searchable {
		t.Error("turning the field off the advanced form also cleared `searchable` — " +
			"these are two independent settings and collapsing them makes records " +
			"unfindable as a side effect of tidying a form")
	}
	if !indexed() {
		t.Error("the field's value stopped answering a text query after it was taken off " +
			"the advanced FORM; participation governs the control, never the index")
	}

	// And the value is still there to be read and filtered on. A caller
	// who composes `filter=field:<code>=<value>` by hand is answered by
	// the same rows as before — the flag never became an access rule.
	var stored string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT value_text FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		env.assetID, fid).Scan(&stored); err != nil {
		t.Fatalf("read stored value: %v", err)
	}
	if stored != "quintessential zibeline" {
		t.Errorf("stored value = %q, want it untouched by a participation change", stored)
	}
}

// The converse, and the reason both columns exist: `searchable: false`
// must leave the participation flag alone. If one write silently moved
// the other, an operator could never express "indexed but not on the
// form" or "on the form but not in the free-text index".
func TestParticipation_SearchableOffDoesNotChangeParticipation(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "partindependent", "text", map[string]any{})

	rr := patchJSON(t, env.router, "/fields/"+fid, map[string]any{"searchable": false})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch searchable: %d %s", rr.Code, rr.Body.String())
	}
	got := readParticipation(t, env, fid)
	if got.Searchable {
		t.Fatal("searchable was not persisted as false; the rest of this test is vacuous")
	}
	if !got.AdvancedSearch || !got.Upload {
		t.Error("writing `searchable` moved a participation flag with it; " +
			"the two questions must stay independently answerable")
	}
}

// ---------------------------------------------------------------------------
// 3. show_on_upload round-trips
// ---------------------------------------------------------------------------

// Nothing renders this flag yet (#1119 does), so the only thing that
// can be true about it today is that what an operator sets is what
// comes back. Asserted against the row, not the echo.
func TestParticipation_ShowOnUploadRoundTrips(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "partupload", "text", map[string]any{})

	rr := patchJSON(t, env.router, "/fields/"+fid, map[string]any{"show_on_upload": false})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch show_on_upload: %d %s", rr.Code, rr.Body.String())
	}
	if got := readParticipation(t, env, fid); got.Upload {
		t.Error("show_on_upload was not persisted as false")
	}

	// It is NOT an access rule, and this is the line the create/edit
	// sprint must not cross: a field hidden from the upload FORM is
	// still writable through the value endpoint. Hiding a control and
	// forbidding a write are different promises, and only the second
	// one belongs to a capability.
	if status, body := env.putAsset(t, fid, map[string]any{"value_text": "still writable"}); status != http.StatusOK {
		t.Errorf("writing a value to a field hidden at upload: status=%d body=%v; "+
			"participation is a form hint, never an access control", status, body)
	}

	// Back on again, so the flag is a dial rather than a one-way door.
	rr = patchJSON(t, env.router, "/fields/"+fid, map[string]any{"show_on_upload": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch show_on_upload back on: %d %s", rr.Code, rr.Body.String())
	}
	if got := readParticipation(t, env, fid); !got.Upload {
		t.Error("show_on_upload could not be turned back on")
	}
}

// ---------------------------------------------------------------------------
// 4. edit_tab: set, clear, and the two ways to say it wrong
// ---------------------------------------------------------------------------

func TestParticipation_EditTabSetAndClear(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "parttab", "text", map[string]any{})

	rr := patchJSON(t, env.router, "/fields/"+fid, map[string]any{"edit_tab": "  Production  "})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch edit_tab: %d %s", rr.Code, rr.Body.String())
	}
	got := readParticipation(t, env, fid)
	if got.EditTab == nil || *got.EditTab != "Production" {
		t.Errorf("edit_tab = %v, want the trimmed %q — a tab name that differs from "+
			"another only by whitespace would be two tabs nobody can tell apart", got.EditTab, "Production")
	}

	// A partial update cannot say NULL, so unassigning needs its own verb.
	rr = patchJSON(t, env.router, "/fields/"+fid, map[string]any{"clear_edit_tab": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("clear edit_tab: %d %s", rr.Code, rr.Body.String())
	}
	if got := readParticipation(t, env, fid); got.EditTab != nil {
		t.Errorf("edit_tab = %v after clear_edit_tab, want NULL", *got.EditTab)
	}
}

// Blank is refused rather than stored, because "" is neither a tab nor
// the absence of one. The refusal is a 400 with a sentence — the same
// choice 00045 made for `show_on_card` on a gated field, and for the
// same reason: a setting that is accepted and does nothing is the
// worst of the three options.
func TestParticipation_EditTabRefusesBlankAndContradiction(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "parttabbad", "text", map[string]any{})

	for _, blank := range []string{"", "   "} {
		rr := patchJSON(t, env.router, "/fields/"+fid, map[string]any{"edit_tab": blank})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("edit_tab=%q: status=%d want 400; body=%s", blank, rr.Code, rr.Body.String())
		}
	}

	rr := patchJSON(t, env.router, "/fields/"+fid, map[string]any{
		"edit_tab": "Production", "clear_edit_tab": true,
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("edit_tab + clear_edit_tab together: status=%d want 400; body=%s",
			rr.Code, rr.Body.String())
	}
	if got := readParticipation(t, env, fid); got.EditTab != nil {
		t.Errorf("a refused request still wrote edit_tab = %q", *got.EditTab)
	}
}

// The database refuses a blank tab too, on every path — not only the
// one the Go handler guards. A seed loader, a psql session or a future
// import that has not learned the rule fails at the constraint instead
// of quietly creating the third state.
func TestParticipation_EditTabBlankRefusedByTheDatabase(t *testing.T) {
	env := newVocabEnv(t)
	fid := env.assetField(t, "parttabdb", "text", map[string]any{})

	if _, err := env.pool.Exec(context.Background(),
		`UPDATE field_definition SET edit_tab = '' WHERE id = $1`, fid); err == nil {
		t.Error("the database accepted an empty edit_tab; the CHECK constraint is the " +
			"single authority here and a Go-side guard alone would be a rule expressed " +
			"in the one place every new write path has to remember")
	}
}

// ---------------------------------------------------------------------------
// 5. The read gate composes on top
// ---------------------------------------------------------------------------

// A participation flag can never widen who may see a field. The gate
// is unchanged by this sprint, and this asserts the pair does not
// interact: a gated field may still be marked for the advanced page —
// which is what an operator asking for it wants — and the capability
// keeps deciding who is offered it.
//
// Contrast `show_on_card` (00045), which IS refused on a gated field:
// a card renders on browse where no per-field capability has been
// evaluated, so there the combination would be a leak. The advanced
// page filters by the caller's capabilities and
// `facet.Selection.Authorize` refuses an unauthorised `field:` term
// server-side regardless of what any client sends, so here it is not.
func TestParticipation_GatedFieldMayStillDeclareTheAdvancedPage(t *testing.T) {
	env := newVocabEnv(t)
	fid := mustCreateField(t, env.router, map[string]any{
		"code": "mtv_partgated", "label": "partgated", "type": "multi_select",
		"options": vocabSet(), "read_capability": "assets.read",
	})

	rr := patchJSON(t, env.router, "/fields/"+fid,
		map[string]any{"show_in_advanced_search": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("a gated field must still be allowed to declare the advanced page: %d %s",
			rr.Code, rr.Body.String())
	}
	if got := readParticipation(t, env, fid); !got.AdvancedSearch {
		t.Error("show_in_advanced_search was not persisted on a gated field")
	}

	var gate *string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT read_capability FROM field_definition WHERE id = $1`, fid).Scan(&gate); err != nil {
		t.Fatalf("read capability: %v", err)
	}
	if gate == nil || *gate != "assets.read" {
		t.Errorf("read_capability = %v after a participation write; the gate must be untouched", gate)
	}
}
