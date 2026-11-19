// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: migration 00082 gives every storage_variants row a fixity state.
// Pinned against a real, fresh database:
//
//   - rows that predate it (an original and a derived variant) come up
//     unverifiable, with no digest, at generation 0: nothing is promoted,
//     not even the original whose address is its hash;
//   - the checks hold: a verified or mismatch row needs a complete
//     digest, an unverifiable one has none, a digest is 64 lowercase hex,
//     a digest's size is the row's size_bytes, a status or source outside
//     the vocabulary is refused;
//   - Down removes the columns and Up again restores the defaults.

package db

import (
	"strings"
	"testing"
)

const (
	variantFixityBeforeVersion = 81 // 00081_asset_original_basename
	variantFixityAtVersion     = 82 // 00082_storage_variant_fixity
)

func TestMigration00082_StorageVariantFixity_LegacyRowsStayUnverifiable(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, variantFixityBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", variantFixityBeforeVersion, err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	hash := strings.Repeat("d", 64)
	exec(`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 10, 'fs')`, hash)
	exec(`INSERT INTO storage_variants (object_hash, variant_key, size_bytes) VALUES ($1, 'original', 10), ($1, 'preview', 4)`, hash)

	if _, err := p.UpTo(ctx, variantFixityAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", variantFixityAtVersion, err)
	}
	check := func(stage string) {
		t.Helper()
		rows, err := sqlDB.QueryContext(ctx, `SELECT variant_key, fixity_status, content_sha256 IS NULL, content_size_bytes IS NULL, digest_source IS NULL, generation
			FROM storage_variants WHERE object_hash = $1 ORDER BY variant_key`, hash)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var key, status string
			var noDigest, noSize, noSource bool
			var gen int64
			if err := rows.Scan(&key, &status, &noDigest, &noSize, &noSource, &gen); err != nil {
				t.Fatal(err)
			}
			n++
			if status != "unverifiable" || !noDigest || !noSize || !noSource || gen != 0 {
				t.Errorf("%s: %s = %s digest-null=%v size-null=%v source-null=%v gen=%d, want unverifiable with nothing at 0",
					stage, key, status, noDigest, noSize, noSource, gen)
			}
		}
		if n != 2 {
			t.Fatalf("%s: %d rows, want 2", stage, n)
		}
	}
	check("after Up")

	refused := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err == nil {
			t.Fatalf("accepted: %s", q)
		}
	}
	digest := strings.Repeat("e", 64)
	where := ` WHERE object_hash = '` + hash + `' AND variant_key = 'preview'`
	refused(`UPDATE storage_variants SET fixity_status = 'verified'` + where)                                                    // verified with no digest
	refused(`UPDATE storage_variants SET content_sha256 = $1, content_size_bytes = 4, digest_source = 'write'`+where, digest)    // digest on an unverifiable row
	refused(`UPDATE storage_variants SET fixity_status = 'verified', content_sha256 = $1, content_size_bytes = 4`+where, digest) // incomplete: no source
	refused(`UPDATE storage_variants SET fixity_status = 'verified', content_sha256 = 'ABC', content_size_bytes = 4, digest_source = 'write'` + where)
	refused(`UPDATE storage_variants SET fixity_status = 'verified', content_sha256 = $1, content_size_bytes = 4, digest_source = 'caller'`+where, digest)
	refused(`UPDATE storage_variants SET fixity_status = 'trusted'` + where)
	refused(`UPDATE storage_variants SET fixity_status = 'mismatch'` + where) // mismatch needs the digest it contradicts
	refused(`UPDATE storage_variants SET generation = -1` + where)
	// A digest's size is the row's size: contradictory accounting is refused.
	refused(`UPDATE storage_variants SET fixity_status = 'verified', content_sha256 = $1, content_size_bytes = 5, digest_source = 'recheck'`+where, digest)
	refused(`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_sha256, content_size_bytes, digest_source, fixity_status)
		VALUES ($1, 'screen', 10, $2, 11, 'write', 'verified')`, hash, digest)
	exec(`UPDATE storage_variants SET fixity_status = 'verified', content_sha256 = $1, content_size_bytes = 4, digest_source = 'recheck'`+where, digest)
	refused(`UPDATE storage_variants SET size_bytes = 9` + where) // a digest-bearing row's size cannot drift from its digest
	exec(`UPDATE storage_variants SET fixity_status = 'mismatch'` + where)
	exec(`UPDATE storage_variants SET fixity_status = 'missing'` + where)
	exec(`UPDATE storage_variants SET fixity_status = 'unverifiable', content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL` + where)

	if _, err := p.DownTo(ctx, variantFixityBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", variantFixityBeforeVersion, err)
	}
	var cols int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'storage_variants'
		AND column_name IN ('content_sha256','content_size_bytes','digest_source','generation','fixity_status','fixity_checked_at')`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("after Down: %d fixity columns remain (%v)", cols, err)
	}
	if _, err := p.UpTo(ctx, variantFixityAtVersion); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	check("after Up again")
}
