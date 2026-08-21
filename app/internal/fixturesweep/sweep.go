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

// argsFor returns the parameter list for a composed statement. Only the
// collections rule references $1 (the seed catalogue names); sending a
// parameter to a statement that does not use it makes Postgres reject
// the bind outright ("bind message supplies 1 parameters, but prepared
// statement requires 0"), so the arg list has to follow the SQL.
func argsFor(sql string, seedCollections []string) []any {
	if strings.Contains(sql, "$1") {
		return []any{seedCollections}
	}
	return nil
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
}

// Report is the whole run.
type Report struct {
	Tables    []TableReport
	Satellite map[string]int64
	Applied   bool
}

// ErrContradiction is returned when a rule's Fixture and Protected
// predicates both match the same row. It means a rule has drifted into
// real data, and the sweep refuses to delete anything at all — not just
// the overlapping rows. A rule that is wrong about one row has given up
// its claim to be right about the rest.
type ErrContradiction struct {
	Table string
	N     int64
}

func (e *ErrContradiction) Error() string {
	return fmt.Sprintf(
		"rule for %s matches %d row(s) as BOTH fixture and protected; refusing to delete "+
			"anything. The rule has drifted from the data — re-derive it before sweeping.",
		e.Table, e.N)
}

// Census counts every rule without modifying anything. It is what the
// dry run prints, and it also runs before an --apply so the abort
// happens before the first DELETE.
func Census(ctx context.Context, q pgx.Tx, seedCollections []string) (*Report, error) {
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
		row := q.QueryRow(ctx, sql, argsFor(sql, seedCollections)...)
		if err := row.Scan(&tr.Total, &tr.Fixture, &tr.Protected,
			&tr.Contradiction, &tr.Unclassified); err != nil {
			return nil, fmt.Errorf("census %s: %w", r.Table, err)
		}
		rep.Tables = append(rep.Tables, tr)
	}
	return rep, nil
}

// Run performs the sweep. With apply=false nothing is written and the
// transaction is rolled back regardless, so a dry run cannot leave a
// trace even if a predicate turns out to be expensive or wrong.
func Run(ctx context.Context, pool *pgxpool.Pool, seedCollections []string, apply bool) (*Report, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// Rolled back on every path that does not explicitly commit.
	defer func() { _ = tx.Rollback(ctx) }()

	rep, err := Census(ctx, tx, seedCollections)
	if err != nil {
		return nil, err
	}
	// Abort BEFORE any delete if a rule overlaps real data.
	for _, tr := range rep.Tables {
		if tr.Contradiction > 0 {
			return rep, &ErrContradiction{Table: tr.Table, N: tr.Contradiction}
		}
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
			tag, dErr := tx.Exec(ctx, sql, argsFor(sql, seedCollections)...)
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
		tag, dErr := tx.Exec(ctx, sql, argsFor(sql, seedCollections)...)
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
