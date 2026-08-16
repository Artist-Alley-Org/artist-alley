// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #964 — a migration number written in a Go comment must name a
// migration file that exists.
//
// # The failure this catches
//
// The v0.1 baseline fold collapsed ~56 migrations into
// 00001_baseline_v0_1.sql and restarted the numbering. Every comment
// that had cited one of the folded files kept its number, and the
// numbers were then re-issued to unrelated new migrations. A reader
// chasing `migration 00009` for the ai_provider_call columns lands on
// 00009_resource_request_capability_fk.sql. Two hundred and seventy-two
// such citations were in the tree when this test was written; the
// sweep repointed 118 of them at the baseline.
//
// # What this test CANNOT catch — read this before trusting it
//
// It checks EXISTENCE ONLY. A citation whose number names a real file
// about a completely different subject passes here, and that is over
// half of the damage the fold caused: every wrong citation the #964
// sweep fixed pointed at a file that exists. `00002` really is a
// migration; it is just about featured_items and not about the Admin
// role, and no mechanical check can know which one the sentence meant.
//
// So this is a floor, not a guarantee. It stops the cheap half — a
// number for a migration that was never written, or one deleted later
// — and it stops a number surviving a renumber. It says NOTHING about
// whether the cited file contains what the comment claims. That
// remains a reading job, and the only defence against it is writing
// the citation while looking at the migration.
//
// # Scope
//
// Comments only, and hand-written files only. Generated output
// (*.sql.go from sqlc, openapi.gen.go) is skipped: its comment text is
// copied from .sql / .yaml sources, so a finding there is not
// actionable in the .go file and hand-editing it would be undone by
// the next regeneration.

package db

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// citationRe matches a zero-padded five-digit migration number. The
// leading `[^0-9.]` guard is load-bearing: without it the literal
// `0.00045` in a cost calculation (ai/providers/openai) reads as a
// citation of migration 00045.
var citationRe = regexp.MustCompile(`(^|[^0-9.])(00[0-9]{3})\b`)

func TestMigrationCitations_NameAMigrationThatExists(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	known := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if len(n) > 5 && strings.HasSuffix(n, ".sql") {
			known[n[:5]] = n
		}
	}
	if len(known) < 2 {
		t.Fatalf("only %d migrations found; the fixture is wrong, not the tree", len(known))
	}

	// This file lives in app/internal/db, so the app tree is two up.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve app root: %v", err)
	}

	type finding struct {
		file, line, num, text string
	}
	var bad []finding
	checked := 0

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "vendor", ".git", "migrations":
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		// Generated output — see the scope note in the file header.
		if strings.HasSuffix(name, ".sql.go") || name == "openapi.gen.go" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		rel, _ := filepath.Rel(root, path)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		lineNo := 0
		inBlock := false
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			var comment string
			switch {
			case inBlock:
				comment = line
			case strings.HasPrefix(strings.TrimSpace(line), "//"):
				comment = line
			default:
				if i := strings.Index(line, "//"); i >= 0 {
					comment = line[i:]
				}
			}
			if strings.Contains(line, "/*") {
				inBlock = true
			}
			if strings.Contains(line, "*/") {
				inBlock = false
			}
			if comment == "" {
				continue
			}
			for _, m := range citationRe.FindAllStringSubmatch(comment, -1) {
				num := m[2]
				checked++
				if _, ok := known[num]; !ok {
					bad = append(bad, finding{rel, itoa(lineNo), num, strings.TrimSpace(line)})
				}
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// Guard the guard: a walk that found nothing would pass silently
	// forever. The tree has hundreds of these.
	if checked < 100 {
		t.Fatalf("only %d citations scanned; the walk is not reaching the tree", checked)
	}
	t.Logf("scanned %d migration-number citations in Go comments", checked)

	for _, b := range bad {
		t.Errorf("%s:%s cites migration %s, which does not exist\n    %s",
			b.file, b.line, b.num, b.text)
	}
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
