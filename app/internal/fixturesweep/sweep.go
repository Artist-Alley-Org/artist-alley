// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package fixturesweep

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// quoteLiteral renders a kind value as a SQL string literal. The values
// come from Rules, which is code rather than input, but composing SQL
// without quoting is a habit worth not having.
func quoteLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// CatalogueParam names which catalogue value a rule binds as $1.
type CatalogueParam string

const (
	// ParamNone is a rule that takes no parameter — its predicates are
	// pure SQL over the table's own columns.
	ParamNone CatalogueParam = ""
	// ParamCollectionNames binds dataset.collections.json's names.
	ParamCollectionNames CatalogueParam = "collections"
	// ParamPostIDs binds the ids in studio-*.posts.json.
	ParamPostIDs CatalogueParam = "posts"
)

// Catalogue is what the seed profiles say is REAL, passed into the rules
// as bind parameters so they track the dataset instead of drifting from
// it.
type Catalogue struct {
	CollectionNames []string // dataset.collections.json
	PostIDs         []string // studio-*.posts.json
}

// argsFor returns the parameter list for one composed statement.
//
// ⛔ EVERY parameterised rule uses $1, and $1 means whatever that rule
// says it means. A global numbering — collections at $1, posts at $2 —
// looks tidier and does not work: Postgres derives a statement's
// parameter count from the highest $n it mentions, so a posts predicate
// referencing only $2 makes the server expect two parameters and then
// fail to infer a type for the $1 nothing references (42P18, "could not
// determine data type of parameter $1").
//
// ⚠️ IT TAKES THE SQL AS WELL AS THE RULE, and both are load-bearing.
// The rule says WHICH value $1 means; the statement says WHETHER it
// mentions $1 at all. The sweep composes four different statements per
// rule and they do not agree: the census uses Fixture AND Protected, the
// parent DELETE uses both, but the SATELLITE delete composes only
// Fixture — and the posts rule's parameter lives in Protected. Deciding
// from the rule alone sends an argument to a statement with no
// placeholder ("mismatched param and argument count") and breaks every
// real -apply run.
func argsFor(r Rule, sql string, cat Catalogue) []any {
	if !strings.Contains(sql, "$1") {
		return nil
	}
	switch r.Param {
	case ParamCollectionNames:
		return []any{cat.CollectionNames}
	case ParamPostIDs:
		return []any{cat.PostIDs}
	default:
		return nil
	}
}

// TableReport is one rule's census.
type TableReport struct {
	Table         string
	Total         int64
	Fixture       int64
	Protected     int64
	Contradiction int64 // rows matching BOTH — any non-zero aborts the sweep
	Unclassified  int64 // rows matching NEITHER — reported, never deleted
	Deleted       int64

	// Overlap samples the contradicting rows so the abort can name them
	// (#1276). Populated only when Contradiction > 0.
	Overlap []ContradictionRow
}

// Report is the whole run.
type Report struct {
	Tables    []TableReport
	Satellite map[string]int64
	Applied   bool
}

// ContradictionRow identifies one row that matched both predicates, so
// the abort can name it.
type ContradictionRow struct {
	ID    string
	Label string
}

// TableContradiction is one table's overlap, with a sample of the rows.
type TableContradiction struct {
	Table  string
	N      int64
	Sample []ContradictionRow
}

// contradictionSample caps how many rows the abort prints per table.
// Enough to see the pattern, few enough to read.
const contradictionSample = 10

// ErrContradiction is returned when a rule's Fixture and Protected
// predicates both match the same row. It means a rule has drifted into
// real data, and the sweep refuses to delete anything at all — not just
// the overlapping rows. A rule that is wrong about one row has given up
// its claim to be right about the rest.
//
// ⚠️ It carries EVERY contradicting table, not the first one. Reporting
// only the first means an operator fixes one rule, re-runs, and is told
// about the next — and each round trip on a persistent stack costs a
// dogfood run to reproduce.
//
// It also names the ROWS. The abort used to say "posts: 6 rows", which is
// true, actionable only by writing SQL, and the reason #1276 sat open
// while the sweep was unusable after every dogfood run.
type ErrContradiction struct {
	Tables []TableContradiction
}

func (e *ErrContradiction) Error() string {
	var b strings.Builder
	var total int64
	for _, t := range e.Tables {
		total += t.N
	}
	fmt.Fprintf(&b, "%d row(s) across %d table(s) match BOTH fixture and protected; "+
		"refusing to delete anything. A rule that is wrong about one row has given up "+
		"its claim to be right about the rest — re-derive it before sweeping.",
		total, len(e.Tables))
	for _, t := range e.Tables {
		fmt.Fprintf(&b, "\n\n  %s — %d row(s):", t.Table, t.N)
		for _, r := range t.Sample {
			fmt.Fprintf(&b, "\n    %s  %s", r.ID, r.Label)
		}
		if int64(len(t.Sample)) < t.N {
			fmt.Fprintf(&b, "\n    … and %d more", t.N-int64(len(t.Sample)))
		}
	}
	return b.String()
}

// Census counts every rule without modifying anything. It is what the
// dry run prints, and it also runs before an --apply so the abort
// happens before the first DELETE.
func Census(ctx context.Context, q pgx.Tx, cat Catalogue) (*Report, error) {
	rep := &Report{Satellite: map[string]int64{}}
	for _, r := range Rules {
		tr := TableReport{Table: r.Table}
		sql := fmt.Sprintf(`
			SELECT count(*),
			       count(*) FILTER (WHERE %[1]s),
			       count(*) FILTER (WHERE %[2]s),
			       count(*) FILTER (WHERE (%[1]s) AND (%[2]s)),
			       count(*) FILTER (WHERE NOT ((%[1]s) OR (%[2]s)))
			FROM %[3]s`, r.Fixture, r.Protected, r.Table)
		row := q.QueryRow(ctx, sql, argsFor(r, sql, cat)...)
		if err := row.Scan(&tr.Total, &tr.Fixture, &tr.Protected,
			&tr.Contradiction, &tr.Unclassified); err != nil {
			return nil, fmt.Errorf("census %s: %w", r.Table, err)
		}
		if tr.Contradiction > 0 {
			sample, err := contradictingRows(ctx, q, r, cat)
			if err != nil {
				return nil, err
			}
			tr.Overlap = sample
		}
		rep.Tables = append(rep.Tables, tr)
	}
	return rep, nil
}

// contradictingRows fetches the rows the abort will name. Only ever runs
// when the census has already counted a non-zero overlap, so the normal
// path costs nothing.
func contradictingRows(ctx context.Context, q pgx.Tx, r Rule, cat Catalogue) ([]ContradictionRow, error) {
	label := r.LabelColumn
	if label == "" {
		label = r.IDColumn
	}
	sql := fmt.Sprintf(
		`SELECT %[1]s::text, coalesce(%[2]s::text, '(no %[2]s)')
		 FROM %[3]s WHERE (%[4]s) AND (%[5]s)
		 ORDER BY %[1]s LIMIT %[6]d`,
		r.IDColumn, label, r.Table, r.Fixture, r.Protected, contradictionSample)
	rows, err := q.Query(ctx, sql, argsFor(r, sql, cat)...)
	if err != nil {
		return nil, fmt.Errorf("contradiction sample %s: %w", r.Table, err)
	}
	defer rows.Close()
	var out []ContradictionRow
	for rows.Next() {
		var c ContradictionRow
		if err := rows.Scan(&c.ID, &c.Label); err != nil {
			return nil, fmt.Errorf("contradiction sample %s: %w", r.Table, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Run performs the sweep. With apply=false nothing is written and the
// transaction is rolled back regardless, so a dry run cannot leave a
// trace even if a predicate turns out to be expensive or wrong.
func Run(ctx context.Context, pool *pgxpool.Pool, cat Catalogue, apply bool) (*Report, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// Rolled back on every path that does not explicitly commit.
	defer func() { _ = tx.Rollback(ctx) }()

	rep, err := Census(ctx, tx, cat)
	if err != nil {
		return nil, err
	}
	// Abort BEFORE any delete if a rule overlaps real data. EVERY
	// contradicting table is collected, not the first — an operator who
	// is told about one at a time pays a dogfood run per round trip.
	var bad []TableContradiction
	for _, tr := range rep.Tables {
		if tr.Contradiction > 0 {
			bad = append(bad, TableContradiction{
				Table: tr.Table, N: tr.Contradiction, Sample: tr.Overlap})
		}
	}
	if len(bad) > 0 {
		return rep, &ErrContradiction{Tables: bad}
	}
	if !apply {
		return rep, nil
	}

	// Satellite tables first: they have no FK to their subject, so
	// CASCADE never reaches them. Clearing them while the parent rows
	// still exist is what lets the subquery find the ids at all.
	for _, s := range SatelliteTables {
		var n int64
		for _, r := range Rules {
			if r.Kind == "" {
				continue
			}
			// Compare as text on both sides. activities.object_local_id
			// and notifications.target_id are TEXT while likes and
			// comments use UUID, and "user" is keyed by a bigint ref —
			// four column types against one expression. Casting both
			// sides costs an index scan on tables of this size and buys
			// a statement that cannot be wrong about a type.
			//
			// The *_kind filter is not decoration: these tables are
			// polymorphic, so without it a post id would be matched
			// against asset rows as well.
			sql := fmt.Sprintf(
				`DELETE FROM %s WHERE %s = %s AND %s::text IN (SELECT %s::text FROM %s WHERE %s)`,
				s.Table, s.KindColumn, quoteLiteral(r.Kind), s.Column, r.IDColumn, r.Table, r.Fixture)
			tag, dErr := tx.Exec(ctx, sql, argsFor(r, sql, cat)...)
			if dErr != nil {
				return rep, fmt.Errorf("satellite %s <- %s: %w", s.Table, r.Table, dErr)
			}
			n += tag.RowsAffected()
		}
		rep.Satellite[s.Table] = n
	}

	// Parents, children-first order: posts and collections reference
	// assets by SET NULL columns, so removing them first keeps the
	// asset delete from having to null out rows it is about to remove.
	order := []string{"posts", "collections", "field_definition", "assets", `"user"`}
	byTable := map[string]*TableReport{}
	for i := range rep.Tables {
		byTable[rep.Tables[i].Table] = &rep.Tables[i]
	}
	for _, name := range order {
		var rule *Rule
		for i := range Rules {
			if Rules[i].Table == name {
				rule = &Rules[i]
				break
			}
		}
		if rule == nil {
			continue
		}
		// Belt and braces: exclude Protected explicitly in the DELETE
		// even though Census proved the sets are disjoint. The census
		// and the delete are two statements; making the delete state
		// the safety property itself means a race or an edit between
		// them cannot turn into data loss.
		sql := fmt.Sprintf(`DELETE FROM %s WHERE (%s) AND NOT (%s)`,
			rule.Table, rule.Fixture, rule.Protected)
		tag, dErr := tx.Exec(ctx, sql, argsFor(*rule, sql, cat)...)
		if dErr != nil {
			return rep, fmt.Errorf("delete %s: %w", rule.Table, dErr)
		}
		byTable[name].Deleted = tag.RowsAffected()
	}

	if err := tx.Commit(ctx); err != nil {
		return rep, err
	}
	rep.Applied = true
	return rep, nil
}

// String renders the report as an aligned table for the console.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-18s %9s %9s %9s %13s %12s %9s\n",
		"TABLE", "TOTAL", "FIXTURE", "REAL", "CONTRADICT", "UNCLASSIFIED", "DELETED")
	for _, t := range r.Tables {
		fmt.Fprintf(&b, "%-18s %9d %9d %9d %13d %12d %9d\n",
			t.Table, t.Total, t.Fixture, t.Protected, t.Contradiction, t.Unclassified, t.Deleted)
	}
	if len(r.Satellite) > 0 {
		fmt.Fprintf(&b, "\nsatellite (no FK, cleared explicitly):\n")
		for _, s := range SatelliteTables {
			fmt.Fprintf(&b, "  %-16s %9d\n", s.Table, r.Satellite[s.Table])
		}
	}
	return b.String()
}
