// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #958 — migration 00039 creates the seeded read-only administrator
// role (`Auditor`) and grants it the admin READ capabilities that no
// role could hold.
//
// This pins four properties against a real, freshly-migrated database:
//
//   - before 00039 the role does not exist and the six codes are held
//     by nothing (the bug the migration fixes);
//   - after Up the role exists, descends from Base, and RESOLVES to the
//     expected capability set through the recursive role-chain walk the
//     app actually uses — not just "six rows landed in
//     role_capabilities";
//   - the resolved set contains neither `system.admin` nor
//     `share.grant`, which is the whole point of a read-only tier;
//   - Down removes the role and its grants, and a re-Up restores them.

package db

import (
	"database/sql"
	"sort"
	"testing"
)

const (
	auditorBeforeVersion = 38 // 00038_publication_capability_descriptions
	auditorAtVersion     = 39 // 00039_auditor_role
)

// auditorRoleID is the fixed id migration 00039 pins, matching how the
// baseline pins Base/Admin/Anonymous.
const auditorRoleID = "c7a1f2e0-3b5d-4a6c-9e18-2f7b4d0a1c63"

// baseRoleID is the built-in Base role — 00039 makes Auditor its child.
const baseRoleID = "80ec6003-7fd5-4dac-9415-d26d39169d42"

// auditorGrantedCaps is exactly what 00039 attaches to the role.
// `share.grant` is deliberately NOT here: it is a privilege-granting
// write (approving an access request inserts a user_capability_grants
// row naming a requester-controlled capability code), so handing it to
// a read-only tier would be an escalation route straight out of that
// tier. See the migration header.
var auditorGrantedCaps = []string{
	"featured.read",
	"federation.read",
	"requests.read",
	"system.activities.read",
	"system.license.read",
	"system.metadata_extraction.read",
}

// auditorResolvedCaps is what a user holding ONLY the Auditor role can
// exercise globally — the six above plus everything Base contributes
// through parent_id. Asserting the whole set (not a subset) is what
// makes an accidental widening of Base visible here.
var auditorResolvedCaps = []string{
	"ai.use",
	"assets.submit",
	"caps.read",
	"comments.delete.own",
	"featured.read",
	"federation.read",
	"mcp.client.use",
	"posts.comment",
	"posts.like",
	"profile.update_self",
	"requests.read",
	"roles.read",
	"system.activities.read",
	"system.license.read",
	"system.metadata_extraction.read",
	"teams.read",
}

// roleExists reports whether a role with the given name is present.
func roleExists(t *testing.T, sqlDB *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT count(*) FROM roles WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("count role %s: %v", name, err)
	}
	return n > 0
}

// capHeldByAnyRole reports whether ANY role attaches the code — the
// property #958 is about. A capability no role holds can only ever be
// exercised through a per-user grant.
func capHeldByAnyRole(t *testing.T, sqlDB *sql.DB, code string) bool {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT count(*) FROM role_capabilities WHERE capability_code = $1`, code).Scan(&n); err != nil {
		t.Fatalf("count holders of %s: %v", code, err)
	}
	return n > 0
}

// resolveRoleCaps walks roles.parent_id recursively and returns the
// union of every capability the chain attaches. This mirrors the
// role_chain CTE in auth.EffectiveCapabilitiesForUser — the point is to
// prove inheritance actually delivers Base's codes to an Auditor, not
// to re-check the six rows the migration inserted.
func resolveRoleCaps(t *testing.T, sqlDB *sql.DB, roleName string) []string {
	t.Helper()
	const q = `
WITH RECURSIVE role_chain AS (
    SELECT r.id, r.parent_id, 0 AS depth
    FROM roles r
    WHERE r.name = $1

    UNION ALL

    SELECT r.id, r.parent_id, rc.depth + 1
    FROM roles r
    JOIN role_chain rc ON r.id = rc.parent_id
    WHERE rc.depth < 32
)
SELECT DISTINCT rc.capability_code
FROM role_chain ch
JOIN role_capabilities rc ON rc.role_id = ch.id
ORDER BY 1`
	rows, err := sqlDB.QueryContext(t.Context(), q, roleName)
	if err != nil {
		t.Fatalf("resolve caps for %s: %v", roleName, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatalf("scan cap: %v", err)
		}
		out = append(out, code)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate caps: %v", err)
	}
	sort.Strings(out)
	return out
}

func TestMigration00039_AuditorRole_UpDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	// ── Just before 00039: no Auditor, and the six codes exist in the
	// catalogue but are held by nothing. This IS the reported bug, so
	// asserting it here stops the migration from silently becoming a
	// no-op if some earlier migration starts granting them.
	if _, err := p.UpTo(ctx, auditorBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", auditorBeforeVersion, err)
	}
	if roleExists(t, sqlDB, "Auditor") {
		t.Fatalf("Auditor role exists before 00039")
	}
	for _, code := range auditorGrantedCaps {
		if !capExists(t, sqlDB, code) {
			t.Errorf("%s missing from the capability catalogue before 00039", code)
		}
		if capHeldByAnyRole(t, sqlDB, code) {
			t.Errorf("%s already held by a role before 00039 — 00039 may be redundant", code)
		}
	}

	// ── Apply 00039 ──────────────────────────────────────────────────
	if _, err := p.UpTo(ctx, auditorAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", auditorAtVersion, err)
	}
	if !roleExists(t, sqlDB, "Auditor") {
		t.Fatalf("Auditor role missing after 00039 Up")
	}

	// Parent is Base — the decision the migration header argues, pinned
	// so a later edit cannot quietly reparent it (which would change
	// what the role implicitly holds).
	var parent sql.NullString
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT parent_id::text FROM roles WHERE id = $1`, auditorRoleID).Scan(&parent); err != nil {
		t.Fatalf("read Auditor parent: %v", err)
	}
	if !parent.Valid || parent.String != baseRoleID {
		t.Errorf("Auditor parent_id = %v, want Base (%s)", parent, baseRoleID)
	}

	for _, code := range auditorGrantedCaps {
		if !roleHasCap(t, sqlDB, auditorRoleID, code) {
			t.Errorf("Auditor missing direct grant %s after 00039 Up", code)
		}
	}

	// The resolved set — inheritance included — is exactly what we
	// expect, no more.
	got := resolveRoleCaps(t, sqlDB, "Auditor")
	want := append([]string(nil), auditorResolvedCaps...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Errorf("Auditor resolves to %d caps, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	} else {
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("Auditor resolved caps differ at %d: got %q want %q\n got: %v\nwant: %v",
					i, got[i], want[i], got, want)
				break
			}
		}
	}

	// The read-only contract, stated as two explicit negatives rather
	// than left implicit in the list above.
	for _, forbidden := range []string{"system.admin", "share.grant"} {
		for _, c := range got {
			if c == forbidden {
				t.Errorf("Auditor resolves to %s — the read-only tier must not hold it", forbidden)
			}
		}
	}

	// ── Down removes the role and its grants ─────────────────────────
	if _, err := p.DownTo(ctx, auditorBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", auditorBeforeVersion, err)
	}
	if roleExists(t, sqlDB, "Auditor") {
		t.Errorf("Auditor role still present after 00039 Down")
	}
	var leftover int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM role_capabilities WHERE role_id = $1`, auditorRoleID).Scan(&leftover); err != nil {
		t.Fatalf("count leftover grants: %v", err)
	}
	if leftover != 0 {
		t.Errorf("%d role_capabilities rows survive 00039 Down", leftover)
	}
	// The capability codes themselves belong to 00003 and must survive
	// this Down — rolling back the role must not uninstall the
	// catalogue entries other migrations own.
	for _, code := range auditorGrantedCaps {
		if !capExists(t, sqlDB, code) {
			t.Errorf("00039 Down removed %s from the catalogue — it belongs to 00003", code)
		}
	}

	// ── Up again is clean ────────────────────────────────────────────
	if _, err := p.UpTo(ctx, auditorAtVersion); err != nil {
		t.Fatalf("re-apply 00039 after down: %v", err)
	}
	if !roleExists(t, sqlDB, "Auditor") {
		t.Errorf("Auditor role not restored after re-Up")
	}
	for _, code := range auditorGrantedCaps {
		if !roleHasCap(t, sqlDB, auditorRoleID, code) {
			t.Errorf("Auditor missing %s after re-Up", code)
		}
	}
}
