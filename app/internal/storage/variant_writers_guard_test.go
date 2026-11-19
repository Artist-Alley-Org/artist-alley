// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: every variant row and every variant byte goes through package
// storage, so the digest a row records is always the server's own,
// computed from the bytes it wrote. This guard fails on a new writer that
// bypasses it:
//
//   - a call, outside package storage, to a query that writes a
//     storage_variants row (BeginVariantWrite, RecordVariantWrite,
//     RecordOriginalVariant, RecordOriginalVariantIfAbsent,
//     TouchOriginalVariant, InsertVariantIfMissing, RecordVariantRecheck,
//     MarkVariantFixity);
//   - raw SQL that inserts into or updates storage_variants anywhere but
//     storage/queries.sql and the migrations;
//   - a direct `.Backend.Put(` outside package storage (use
//     Service.PutVariant).
//
// Test files are exempt: fixtures plant bytes and legacy rows on purpose.
// Known, reviewed exception: internal/brushpacks writes brush stamp
// bitmaps to the backend through its own Backend field. Stamps are
// addressed by the stamp's own hash, tracked in brush_pack_stamps, and
// never listed in storage_variants, so no fixity row describes them.

package storage_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestVariantWritersGoThroughStorage(t *testing.T) {
	root, err := filepath.Abs("../..") // app/
	if err != nil {
		t.Fatal(err)
	}
	rowWriters := regexp.MustCompile(`\.(BeginVariantWrite|RecordVariantWrite|RecordOriginalVariant|RecordOriginalVariantIfAbsent|TouchOriginalVariant|InsertVariantIfMissing|RecordVariantRecheck|MarkVariantFixity)\(`)
	rawSQL := regexp.MustCompile(`(?i)(INSERT\s+INTO|UPDATE)\s+(public\.)?storage_variants\b`)
	backendPut := regexp.MustCompile(`\.Backend\.Put\(`)
	storagePkg := filepath.Join(root, "internal", "storage") + string(filepath.Separator)
	migrations := filepath.Join(root, "internal", "db", "migrations") + string(filepath.Separator)

	var problems []string
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		isGo := strings.HasSuffix(path, ".go")
		isSQL := strings.HasSuffix(path, ".sql")
		if (!isGo && !isSQL) || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		inStorage := strings.HasPrefix(path, storagePkg) && filepath.Dir(path)+string(filepath.Separator) == storagePkg
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(b), "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "--") {
				continue
			}
			where := rel + ":" + itoa(i+1)
			if isGo && !inStorage && rowWriters.MatchString(line) {
				problems = append(problems, where+": variant row written outside package storage: "+trim)
			}
			if isGo && !inStorage && backendPut.MatchString(line) {
				problems = append(problems, where+": backend bytes written outside package storage (use Service.PutVariant): "+trim)
			}
			if rawSQL.MatchString(line) && !strings.HasPrefix(path, migrations) && !(inStorage && strings.HasSuffix(path, "queries.sql")) &&
				!(inStorage && strings.HasSuffix(path, "queries.sql.go")) {
				problems = append(problems, where+": raw SQL writes storage_variants: "+trim)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files under %s: the guard is not looking at the app", scanned, root)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
