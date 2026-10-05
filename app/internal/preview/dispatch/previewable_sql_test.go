// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25a: [PreviewableSQL] is held to [CanPreview].
//
// A SQL twin of a Go rule is sanctioned only with a parity test beside
// it (the visibility package's *_MatchesGo tests are the model), because
// the two agree on the day they are written and nothing else makes them
// keep agreeing. This drives every extension the router declares, a set
// of extensions it does not, the spellings [Normalize] is expected to
// fold and the ones it is expected NOT to fold, and the NULL/empty
// cases, through Postgres and through Go, and requires one answer.
//
// Skips without AA_DB_PASSWORD.

package dispatch

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func parityPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	envOr := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	dsn := "host=" + envOr("AA_DB_HOST", "postgres") +
		" port=" + envOr("AA_DB_PORT", "5432") +
		" user=" + envOr("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// parityDomain is every input both sides are asked about.
func parityDomain() []*string {
	seen := map[string]struct{}{}
	for _, set := range []map[string]struct{}{
		ImageExts, GifExts, VideoExts, ModelExts, AudioExts, PDFExts, FontExts,
		EbookExts, EPSExts, PSDExts, ComicExts, TextExts, ArchiveExts,
	} {
		for e := range set {
			seen[e] = struct{}{}
			seen["."+e] = struct{}{}
			seen[" "+e] = struct{}{}
			seen[e+" "] = struct{}{}
			seen[strings.ToUpper(e)] = struct{}{}
			seen["."+strings.ToUpper(e)] = struct{}{}
		}
	}
	// ⚠️ NOT in the domain: a DOUBLE leading dot. [Normalize]'s contract
	// is "strip a leading dot", singular, and the twin implements that.
	// [CanPreview] happens to strip two, because JobTypeForExt normalises
	// and then Has normalises its input again; needsProcessing strips
	// one. That is a pre-existing artifact of the authority, outside its
	// documented contract, recorded in the sprint 25a handoff and not
	// pinned here in either direction. No stored file_extension carries
	// a leading dot at all.
	// Non-members, including the live corpus's four non-previewable
	// no-`col` extensions and near-misses of real ones.
	for _, e := range []string{
		"md", "bin", "mtl", "npz", "docx", "rtf", "zzz", "png2", "pn", "",
		"PNG", ".PNG", "Jpg", ".MP4", "mp4.", "mp 4", "png'; DROP TABLE x;--",
	} {
		seen[e] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*string, 0, len(keys)+1)
	for _, k := range keys {
		k := k
		out = append(out, &k)
	}
	return append(out, nil)
}

func TestPreviewableSQL_MatchesCanPreview(t *testing.T) {
	pool := parityPool(t)
	ctx := context.Background()
	sql := `SELECT ` + PreviewableSQL("$1::TEXT")
	checked := 0
	for _, in := range parityDomain() {
		var got bool
		var arg any
		label := "<nil>"
		if in != nil {
			arg = *in
			label = *in
		}
		if err := pool.QueryRow(ctx, sql, arg).Scan(&got); err != nil {
			t.Fatalf("%q: %v", label, err)
		}
		if want := CanPreview(in); got != want {
			t.Errorf("%q: SQL says %v, CanPreview says %v", label, got, want)
		}
		checked++
	}
	// The denominator, so a domain that silently shrank cannot pass.
	if checked < 100 {
		t.Fatalf("only %d inputs checked; the domain is supposed to cover every declared set", checked)
	}
	// And the fragment is TOTAL: NULL in, false out, never NULL.
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT (`+PreviewableSQL("NULL::TEXT")+`) IS NULL`).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if isNull {
		t.Error("PreviewableSQL(NULL) is NULL rather than false; it must be total")
	}
}

func TestPreviewableSQL_IsDerivedFromPreviewableExts(t *testing.T) {
	sql := PreviewableSQL("x")
	for _, e := range PreviewableExts() {
		if !containsQuoted(sql, e) {
			t.Errorf("%q is previewable and absent from the fragment", e)
		}
	}
	for _, e := range []string{"md", "bin", "docx"} {
		if containsQuoted(sql, e) {
			t.Errorf("%q is not previewable and present in the fragment", e)
		}
	}
}

func containsQuoted(sql, e string) bool {
	return strings.Contains(sql, "'"+e+"'")
}
