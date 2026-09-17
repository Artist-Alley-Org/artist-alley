// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1417, sprint 24: THE RESIDENT DERIVATION IS PINNED TO THE GO AUTHORITY.
//
// Migration 00071 makes the badge kind an ingredient of the asset search
// document, and the trigger that builds that document runs inside
// Postgres, where Go cannot be called. So a copy of the derivation has
// to live in the database: `public.asset_view_kind(asset_type,
// file_extension)`, whose body is the text viewkind.KindSQL("") renders,
// spliced verbatim into the migration.
//
// A copy with no oracle is a drift bomb (viewkind's own package comment
// says why that file is allowed to exist). These two tests are the
// oracle, and they are the whole reason app/internal/viewkind may stay
// the SOLE authority while a second, resident expression exists:
//
//  1. [TestKindVocabulary_ResidentDerivationMatchesGo] drives the whole
//     vocabulary, under every overriding ref and under none, plus every
//     edge the resolver has a definite answer for, through the resident
//     function, through KindSQL in the same session, and through
//     ForAsset, and requires all three to agree row by row. It is
//     TestKindSQLMatchesForAsset extended by one more expression.
//
//  2. [TestKindVocabulary_ResidentDerivationTextIsKindSQL] compares the
//     stored function body to the live rendering BYTE FOR BYTE. A
//     vocabulary edit in Go that is not accompanied by a migration goes
//     red here, in CI, rather than leaving the badge and the search
//     document to disagree about an extension.
//
// Both are branch-only structural acceptance: on a database without
// 00071 the function does not exist, and that is not a product finding.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/viewkind"
)

// kvdInputs is the drive: every known extension, the edges, and NULL,
// each under every ref in `refs`.
func kvdInputs() (refs []*int64, exts []*string) {
	// The overriding refs, two that do not override, and the absent
	// case. Hardcoded rather than derived from the override map so a
	// change to the map has to be noticed here too.
	refs = []*int64{nil, kgRef(1), kgRef(2), kgRef(6), kgRef(11), kgRef(13)}
	for _, e := range viewkind.KnownExtensions() {
		exts = append(exts, kgExt(e))
	}
	// An extension claimed by an earlier group (`ts` is video before
	// doc), upper case, surrounding whitespace, a leading dot, an unknown
	// extension, and the empty forms.
	for _, e := range []string{"ts", "TS", "PNG", ".png", " .PnG ", "  epub  ", "nosuchext", "tar.gz", "", "   "} {
		exts = append(exts, kgExt(e))
	}
	exts = append(exts, nil)
	return refs, exts
}

func TestKindVocabulary_ResidentDerivationMatchesGo(t *testing.T) {
	pool := previewPool(t)
	refs, exts := kvdInputs()

	var (
		inRefs []*int64
		inExts []*string
		want   []string
	)
	for _, r := range refs {
		for _, e := range exts {
			inRefs = append(inRefs, r)
			inExts = append(inExts, e)
			want = append(want, string(viewkind.ForAsset(r, e)))
		}
	}

	// Three expressions over one row set: the resident function the
	// document builder calls, the live KindSQL rendering the `kind:`
	// arms compose, and the Go resolver's answer carried in as a fourth
	// column so a disagreement names all three.
	rows, err := pool.Query(t.Context(), `
		SELECT public.asset_view_kind(t.asset_type, t.file_extension),
		       `+viewkind.KindSQL("t")+`,
		       t.n
		  FROM unnest($1::BIGINT[], $2::TEXT[])
		    WITH ORDINALITY AS t(asset_type, file_extension, n)
		 ORDER BY t.n`, inRefs, inExts)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	seen := map[string]int{}
	var i int
	for rows.Next() {
		var resident, live string
		var n int64
		if err := rows.Scan(&resident, &live, &n); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
		if i >= len(want) {
			t.Fatalf("Postgres returned more rows than were sent")
		}
		if resident != want[i] || live != want[i] {
			ref := "<NULL>"
			if inRefs[i] != nil {
				ref = strconv.FormatInt(*inRefs[i], 10)
			}
			ext := "<NULL>"
			if inExts[i] != nil {
				ext = strconv.Quote(*inExts[i])
			}
			t.Errorf("asset_type=%s extension=%s: resident says %q, KindSQL says %q, ForAsset says %q",
				ref, ext, resident, live, want[i])
		}
		seen[resident]++
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if i != len(want) {
		t.Fatalf("Postgres answered for %d of %d inputs", i, len(want))
	}
	if i < 500 {
		t.Errorf("only %d pairs were checked; the vocabulary is larger than that", i)
	}

	// The drive covered every kind a single asset can resolve to, and
	// `sequence` was produced by nothing: it is a real kind that no
	// single asset has, and the resident function must never invent it.
	for _, k := range viewkind.All() {
		switch k {
		case viewkind.KindSequence:
			if seen[string(k)] != 0 {
				t.Errorf("the resident derivation produced `sequence` %d times; no single asset resolves to it", seen[string(k)])
			}
		default:
			if seen[string(k)] == 0 {
				t.Errorf("kind %q was never produced by the drive; the vocabulary is not exhausted", k)
			}
		}
	}

	// `placeholder` yields NO lexeme in the asset document: the builder
	// blanks it before tokenising. Checked on the builder's own
	// expression rather than on a fixture row, so it needs no cleanup.
	var doc string
	if err := pool.QueryRow(t.Context(), `
		SELECT setweight(to_tsvector('english',
		    COALESCE(NULLIF(public.asset_view_kind(NULL, 'nosuchext'), 'placeholder'), '')), 'D')::text`).Scan(&doc); err != nil {
		t.Fatalf("placeholder expression: %v", err)
	}
	if doc != "" {
		t.Errorf("an unresolvable asset would carry %q in its document; placeholder must emit nothing", doc)
	}
	for _, kind := range []viewkind.Kind{viewkind.KindImage, viewkind.Kind3D, viewkind.KindArchive} {
		var ok bool
		if err := pool.QueryRow(t.Context(), `
			SELECT to_tsvector('english', $1) @@ plainto_tsquery('english', $1)`, string(kind)).Scan(&ok); err != nil {
			t.Fatalf("round trip %q: %v", kind, err)
		}
		if !ok {
			t.Errorf("kind %q does not round-trip through plainto_tsquery under `english`", kind)
		}
	}
}

// TestKindVocabulary_ResidentDerivationTextIsKindSQL is the drift guard.
// The function body is `SELECT ` followed by exactly the text
// viewkind.KindSQL("") renders (an empty alias names the function's
// parameters). Anything else, one byte off, is a Go vocabulary edit
// without its migration, or a hand edit of the migration.
func TestKindVocabulary_ResidentDerivationTextIsKindSQL(t *testing.T) {
	pool := previewPool(t)

	var body string
	if err := pool.QueryRow(t.Context(), `
		SELECT p.prosrc
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'public' AND p.proname = 'asset_view_kind'`).Scan(&body); err != nil {
		t.Fatalf("public.asset_view_kind is not resident (migration 00071): %v", err)
	}

	trimmed := strings.TrimSpace(body)
	const prefix = "SELECT "
	if !strings.HasPrefix(trimmed, prefix) {
		t.Fatalf("resident body does not begin with %q:\n%s", prefix, trimmed)
	}
	resident := strings.TrimPrefix(trimmed, prefix)
	live := viewkind.KindSQL("")
	if resident != live {
		// Name the first differing byte rather than dumping two 190-line
		// expressions.
		at := 0
		for at < len(resident) && at < len(live) && resident[at] == live[at] {
			at++
		}
		lo := at - 40
		if lo < 0 {
			lo = 0
		}
		t.Fatalf("the resident derivation differs from viewkind.KindSQL(\"\") at byte %d:\n"+
			"  resident: ...%q\n  KindSQL:  ...%q\n"+
			"A vocabulary change in app/internal/viewkind needs a migration that re-splices the rendering.",
			at, resident[lo:min(len(resident), at+40)], live[lo:min(len(live), at+40)])
	}
}
