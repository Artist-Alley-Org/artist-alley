// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #958 — a capability nobody can hold is dead weight, and until this
// test nothing noticed.
//
// Migration 00003 seeded seven capability codes and granted none of
// them to any role. `federation.read` gates five federation admin
// packages and thirteen live admin tiles; it sat unreachable for
// months, and the only way anyone found out was a user opening
// /admin/federation/peers and getting a bare refusal. Every existing
// test asked "does this code gate the handler" — none asked "can anyone
// get this code".
//
// This is that question, asked once, against a freshly-migrated
// database. Every row in `capabilities` must either be attached to some
// role, or be named in one of the two lists below. Both lists are
// explicit, so adding a capability nobody can hold is a deliberate edit
// with a stated reason rather than an oversight.
//
// The lists are also checked for STALENESS: a listed code that no
// longer exists, or that some role has since been granted, fails too.
// Otherwise the exemptions would silently accumulate into a suppression
// list, which is the failure mode this test exists to prevent.

package db

import (
	"sort"
	"testing"
)

// grantOnlyCapabilities are codes that deliberately belong to NO role.
// Each entry needs a source that says so — a migration comment, a
// handler doc, or "it is a test fixture". These are exercised through a
// per-user grant, or written by a workflow, or gate a surface that does
// not exist yet.
//
// Ordered by code. The reason is the point of the entry; an entry
// without one should not be added.
var grantOnlyCapabilities = map[string]string{
	// --- Operator hands these out per install; a tier default would be wrong.
	"assets.admin": "00037 + CHANGELOG: 'neither is attached to any role, and seeding a " +
		"capability gives nobody anything until an administrator hands it out'. Team-scoped operator grant.",
	"collections.admin": "00037: read by canMutateCollection since the package was written; " +
		"an operator grant, not a tier default.",
	"posts.admin": "00037: 'Held globally it is the instance moderator role' — a deliberate hand-out.",
	"share.grant": "00003 + 00039: catalogue-only, 'a code an operator hands to a named approver, " +
		"never a tier default'. Approving a request inserts a user_capability_grants row naming a " +
		"requester-controlled code, so it must never ride along on a read-only tier.",
	"system.asset_types.admin": "00001: 'Edit asset_type definitions and manage their per-type ACLs'. " +
		"An admin WRITE (assettype/acls_handler.go gates grant + revoke on it, not just the list), so it " +
		"is not an Auditor cap; and Admin reaches it through the system.admin wildcard, so a role default " +
		"would grant nobody anything they lack. Same class as assets.admin / collections.admin / " +
		"posts.admin: the operator delegates per-type ACL administration to a named account. " +
		"#961 also gave its admin tile this cap so the hand-out is discoverable (sections.ts).",
	"users.approve": "users/admin.go: 'Distinct from users.write so a future \"User Approver\" role can " +
		"move accounts through pending → active → disabled without inheriting role-assignment + " +
		"grant/revoke rights.' #961: that role is a COMPOSITION an operator builds at /admin/roles " +
		"(users.read + users.approve), not a tier we ship — one capability does not justify a seeded " +
		"role, and Admin already performs the lifecycle through the system.admin wildcard.",
	"users.password.reset": "00001: 'Issue a one-shot password reset for any user (admin helpdesk " +
		"action)'. #961: a write over another person's credentials, so not an Auditor cap; and the " +
		"helpdesk tier it names is the same operator composition as users.approve. Admin performs the " +
		"reset through the system.admin wildcard — auth/password_handler.go checks Can, which the " +
		"wildcard satisfies.",

	// --- Deliberately ungranted, including to system.admin.
	"content.read.all": "00014: 'Deliberately NOT granted to anything here.' The demo's read-only " +
		"account gets it from the demo deploy bundle, not from core.",
	"system.audit.pii.read": "00011: 'Deliberately NOT granted to anything here, including system.admin.'",
	"users.pii.read":        "00018: 'Deliberately NOT granted to anything here, including system.admin.'",

	// --- Markers a workflow writes; nobody exercises them as a permission.
	"content.access.request": "00035: 'Not seeded onto any role. Nobody needs to HOLD it — it is a " +
		"marker the request workflow writes, not a permission anyone exercises.'",

	// --- Per-tool operator grants on the MCP client; no tier holds them.
	"mcp.client.images.read": "docs/operator/mcp-client-setup.md lists it as a 'Suggested extra cap' " +
		"an operator pins on a specific tool grant; no Go gate reads it.",
	"mcp.client.images.write": "aiedit/handlers.go: 'the per-tool gate operators pin on the img2img grant'.",

	// --- Licensed placeholders for surfaces that are not built.
	"system.sso.ldap.read":  "No handler and no tile — enterprise SSO surface is unbuilt (ADR 0057).",
	"system.sso.ldap.write": "No handler and no tile — enterprise SSO surface is unbuilt (ADR 0057).",
	"system.sso.saml.read":  "No handler and no tile — enterprise SSO surface is unbuilt (ADR 0041/0057).",
	"system.sso.saml.write": "No handler and no tile — enterprise SSO surface is unbuilt (ADR 0041/0057).",
	"system.tenancy.read":   "No handler and no tile — multi-tenant surface is unbuilt (ADR 0041/0057).",
	"system.tenancy.write":  "No handler and no tile — multi-tenant surface is unbuilt (ADR 0041/0057).",

	// --- Test fixtures folded into the baseline. Granted per-user by the
	// tests that use them; attaching them to a real role would leak test
	// permissions into a shipped install.
	"test.granted":            "Fixture for auth/capabilities_test.go — direct per-user grant only.",
	"test.scoped.directgrant": "Fixture for auth/scoped_caps_test.go — direct per-user grant only.",
}

// unreachableBacklog are codes in exactly the state #958 reported:
// a real capability gating a real handler (and in three cases a live
// admin tile), whose own seeding migration names an intended holder,
// which no role holds. They are NOT grant-only — they are the same
// defect, still open.
//
// This list is deliberately separate from grantOnlyCapabilities so the
// two are never confused. Filing one of these under "grant-only" would
// launder a bug into a design decision. Fixing one means deleting its
// entry here and granting it, not editing the reason.
//
// IT IS EMPTY, AND THAT IS THE POINT (#961). The six entries #958 filed
// here are resolved: migration 00040 grants `system.audit.read`,
// `system.jobs.read` and `system.storage.read` to Auditor — the role
// each of 00011 / 00005 / 00006 names in its own header — and the other
// three moved up to grantOnlyCapabilities with the sources describing
// them as operator hand-outs rather than tier defaults.
//
// The map stays declared rather than being deleted with its checks. An
// empty backlog is a state the test asserts is reachable, not a section
// that no longer exists: adding an entry here is how a future sprint
// admits "this is #958 again, and it is not fixed yet", which is a more
// honest edit than quietly widening grantOnlyCapabilities. If it is
// still empty at the next audit, nothing has regressed.
var unreachableBacklog = map[string]string{}

// TestEveryCapabilityIsReachableOrExempt is the standing invariant.
func TestEveryCapabilityIsReachableOrExempt(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	// code → held by at least one role?
	held := map[string]bool{}
	rows, err := sqlDB.QueryContext(ctx, `
		SELECT c.code, EXISTS (
		    SELECT 1 FROM role_capabilities rc WHERE rc.capability_code = c.code
		)
		FROM capabilities c
		ORDER BY c.code`)
	if err != nil {
		t.Fatalf("query capabilities: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var code string
		var isHeld bool
		if err := rows.Scan(&code, &isHeld); err != nil {
			t.Fatalf("scan: %v", err)
		}
		held[code] = isHeld
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate capabilities: %v", err)
	}
	if len(held) == 0 {
		t.Fatal("no capabilities found — the migration set did not seed the catalogue")
	}

	// ── 1. Nothing is unreachable without an explicit, reasoned entry ──
	var orphans []string
	for code, isHeld := range held {
		if isHeld {
			continue
		}
		if _, ok := grantOnlyCapabilities[code]; ok {
			continue
		}
		if _, ok := unreachableBacklog[code]; ok {
			continue
		}
		orphans = append(orphans, code)
	}
	sort.Strings(orphans)
	for _, code := range orphans {
		t.Errorf("capability %q is held by no role and is not listed as grant-only or backlogged.\n"+
			"    Either grant it to a role in a migration, or add it to grantOnlyCapabilities "+
			"(with the source that says nobody should hold it) / unreachableBacklog (if it is the #958 defect again).", code)
	}

	// ── 2. No stale exemptions ────────────────────────────────────────
	//
	// A listed code that has since been granted, or that no longer
	// exists, must be removed from the list. Without this the lists rot
	// into a permanent suppression file.
	for _, list := range []struct {
		name    string
		entries map[string]string
	}{
		{"grantOnlyCapabilities", grantOnlyCapabilities},
		{"unreachableBacklog", unreachableBacklog},
	} {
		var codes []string
		for code := range list.entries {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			isHeld, exists := held[code]
			switch {
			case !exists:
				t.Errorf("%s lists %q, which is not in the capability catalogue — drop the entry.",
					list.name, code)
			case isHeld:
				t.Errorf("%s lists %q, but a role now holds it — drop the entry.", list.name, code)
			}
			if list.entries[code] == "" {
				t.Errorf("%s entry %q has no reason. Every exemption needs one.", list.name, code)
			}
		}
	}

	// ── 3. The nine codes the Auditor migrations granted are reachable ─
	//
	// Stated positively so a regression in 00039 / 00040 fails HERE too,
	// with a message about reachability, rather than only in the
	// migration tests.
	for _, code := range auditorGrantedCaps {
		if !held[code] {
			t.Errorf("capability %q is held by no role — migration 00039 should have granted it to Auditor.", code)
		}
	}
	for _, code := range auditorAdminReadCaps {
		if !held[code] {
			t.Errorf("capability %q is held by no role — migration 00040 should have granted it to Auditor.", code)
		}
	}

	// ── 4. The backlog is empty, and stays that way ───────────────────
	//
	// #961 closed the last six entries. The staleness pass above already
	// rejects a listed code that some role now holds, but it says nothing
	// about a NEW entry, and "add it to the backlog" is the cheapest way
	// to make a future unreachable capability stop failing this test.
	// Naming the empty state as an assertion makes that a deliberate,
	// visible edit — you have to delete this check to do it quietly.
	if len(unreachableBacklog) != 0 {
		var codes []string
		for code := range unreachableBacklog {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		t.Errorf("unreachableBacklog is not empty: %v.\n"+
			"    #961 emptied it. A capability nobody can hold is either granted to a role in a "+
			"migration, or grant-only with the source that says so — the backlog is for admitting an "+
			"UNFIXED #958, not for parking new ones.", codes)
	}
}
