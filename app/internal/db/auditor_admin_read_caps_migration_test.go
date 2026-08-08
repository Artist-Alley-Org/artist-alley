// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #961 — migration 00040 grants `Auditor` the three admin READ
// capabilities whose own seeding migrations (00005 / 00006 / 00011)
// name that role, and which 00039 left behind.
//
// This pins the same four properties the 00039 test pins, against a
// real, freshly-migrated database:
//
//   - before 00040 the three codes exist in the catalogue and are held
//     by nothing (the #958 defect these were the last instance of);
//   - after Up they resolve through the recursive role-chain walk the
//     app actually uses, not merely as three rows in role_capabilities;
//   - the resolved set still contains neither `system.admin` nor
//     `share.grant` nor `system.audit.pii.read` — the read-only contract
//     survives the widening, and the audit-log PII split of 00011 in
//     particular is NOT undone by handing over the log itself;
//   - Down removes exactly these three and leaves 00039's six, and a
//     re-Up restores them.

package db

import (
	"sort"
	"testing"
)

const (
	auditorReadBeforeVersion = 39 // 00039_auditor_role
	auditorReadAtVersion     = 40 // 00040_auditor_admin_read_caps
)

// auditorAdminReadCaps is exactly what 00040 attaches to the role.
//
// `system.audit.pii.read` is deliberately NOT here. 00011 split the
// actor IP out of the audit log precisely so "may read the log" would
// stop meaning "may read personal data about every user who has logged
// in" — the public demo leaked visitor IPs that way. Granting the log
// to a read-only tier is only safe because that split holds, so the PII
// half stays attached to nothing, including system.admin.
var auditorAdminReadCaps = []string{
	"system.audit.read",
	"system.jobs.read",
	"system.storage.read",
}

// auditorResolvedCapsAfter40 is the whole set a user holding ONLY the
// Auditor role can exercise globally once 00040 has run: the sixteen
// 00039 produced (its own six plus Base's ten, inherited through
// parent_id) plus these three. Asserting the entire set rather than a
// subset is what makes an accidental widening — of Base, of Auditor, of
// either migration — visible here rather than in production.
var auditorResolvedCapsAfter40 = append(
	append([]string(nil), auditorResolvedCaps...),
	auditorAdminReadCaps...,
)

func TestMigration00040_AuditorAdminReadCaps_UpDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	// ── Just before 00040: the Auditor exists (00039) but the three
	// codes are held by nothing. That IS the reported defect, so
	// asserting it stops 00040 from silently becoming a no-op if some
	// earlier migration starts granting them.
	if _, err := p.UpTo(ctx, auditorReadBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", auditorReadBeforeVersion, err)
	}
	if !roleExists(t, sqlDB, "Auditor") {
		t.Fatalf("Auditor role missing before 00040 — 00039 should have created it")
	}
	for _, code := range auditorAdminReadCaps {
		if !capExists(t, sqlDB, code) {
			t.Errorf("%s missing from the capability catalogue before 00040", code)
		}
		if capHeldByAnyRole(t, sqlDB, code) {
			t.Errorf("%s already held by a role before 00040 — 00040 may be redundant", code)
		}
	}

	// ── Apply 00040 ──────────────────────────────────────────────────
	if _, err := p.UpTo(ctx, auditorReadAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", auditorReadAtVersion, err)
	}
	for _, code := range auditorAdminReadCaps {
		if !roleHasCap(t, sqlDB, auditorRoleID, code) {
			t.Errorf("Auditor missing direct grant %s after 00040 Up", code)
		}
	}

	// The resolved set — inheritance included — is exactly the nineteen
	// codes, no more.
	got := resolveRoleCaps(t, sqlDB, "Auditor")
	want := append([]string(nil), auditorResolvedCapsAfter40...)
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

	// The read-only contract, restated as explicit negatives now that the
	// tier has grown. `system.audit.pii.read` joins the list 00039 pinned:
	// the Auditor reads the audit log, and must still not read the actor
	// IPs inside it.
	//
	// These three are reachable refusals, not decoration — every one of
	// them is a code some role or grant CAN carry (`system.admin` is on
	// Admin, `share.grant` is an operator hand-out, `system.audit.pii.read`
	// is grantable per user), so a migration that widened Auditor by one
	// line would actually trip them.
	for _, forbidden := range []string{"system.admin", "share.grant", "system.audit.pii.read"} {
		for _, c := range got {
			if c == forbidden {
				t.Errorf("Auditor resolves to %s — the read-only tier must not hold it", forbidden)
			}
		}
	}

	// ── Down removes these three and leaves 00039's six ──────────────
	if _, err := p.DownTo(ctx, auditorReadBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", auditorReadBeforeVersion, err)
	}
	for _, code := range auditorAdminReadCaps {
		if roleHasCap(t, sqlDB, auditorRoleID, code) {
			t.Errorf("Auditor still holds %s after 00040 Down", code)
		}
		// The catalogue entries belong to 00005 / 00006 / the baseline —
		// rolling back a grant must not uninstall the capability.
		if !capExists(t, sqlDB, code) {
			t.Errorf("00040 Down removed %s from the catalogue — it belongs to an earlier migration", code)
		}
	}
	// The Down is scoped to its own three codes. A `DELETE ... WHERE
	// role_id = ...` with no code filter would pass every assertion above
	// and silently strip 00039's grants, so they are checked explicitly.
	for _, code := range auditorGrantedCaps {
		if !roleHasCap(t, sqlDB, auditorRoleID, code) {
			t.Errorf("00040 Down removed %s, which belongs to 00039", code)
		}
	}
	var remaining int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM role_capabilities WHERE role_id = $1`, auditorRoleID).Scan(&remaining); err != nil {
		t.Fatalf("count Auditor grants after down: %v", err)
	}
	if remaining != len(auditorGrantedCaps) {
		t.Errorf("Auditor holds %d direct grants after 00040 Down, want %d (00039's set)",
			remaining, len(auditorGrantedCaps))
	}

	// ── Up again is clean ────────────────────────────────────────────
	if _, err := p.UpTo(ctx, auditorReadAtVersion); err != nil {
		t.Fatalf("re-apply 00040 after down: %v", err)
	}
	for _, code := range auditorAdminReadCaps {
		if !roleHasCap(t, sqlDB, auditorRoleID, code) {
			t.Errorf("Auditor missing %s after re-Up", code)
		}
	}
}

// TestMigration00040_IsIdempotent re-runs the Up statement directly
// against an already-migrated database. The grants are keyed on the role
// NAME with ON CONFLICT DO NOTHING, and this is the property that claim
// rests on — a second application must not error and must not duplicate.
func TestMigration00040_IsIdempotent(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	const reapply = `
INSERT INTO public.role_capabilities (role_id, capability_code)
SELECT r.id, c.code
FROM public.roles r
CROSS JOIN (VALUES
    ('system.audit.read'),
    ('system.jobs.read'),
    ('system.storage.read')
) AS c(code)
WHERE r.name = 'Auditor'
ON CONFLICT (role_id, capability_code) DO NOTHING`
	if _, err := sqlDB.ExecContext(ctx, reapply); err != nil {
		t.Fatalf("re-applying 00040's Up errored: %v", err)
	}

	var n int
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT count(*) FROM role_capabilities
		 WHERE role_id = $1 AND capability_code = ANY($2)`,
		auditorRoleID, pgTextArray(auditorAdminReadCaps)).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if n != len(auditorAdminReadCaps) {
		t.Errorf("Auditor holds %d of the 00040 grants after re-apply, want %d", n, len(auditorAdminReadCaps))
	}
}

// pgTextArray renders a Go slice as a Postgres text[] literal for use
// with `= ANY($n)`. database/sql has no array binding of its own, and
// the codes here are compile-time constants, so a literal is enough.
func pgTextArray(ss []string) string {
	out := "{"
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += `"` + s + `"`
	}
	return out + "}"
}
