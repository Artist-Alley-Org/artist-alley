// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #953 — a user could not put their own upload in a team.
//
// # What was wrong
//
// `assets.team_id` is FK'd, indexed, and read by three separate
// authorisation rules: the `team` sensitivity tier (ADR 0064), the
// team-scoped `assets.admin` mutation grant (#930), and the team-scoped
// half of the field plane (#939). The SEEDER wrote it — 1,947 rows in
// the live dev database — and `AssetCreate` had no such field at all.
// So every one of those rules worked on catalogue data and on nothing a
// real user made: the demo demonstrated a capability the product did not
// offer.
//
// # What makes this file worth reading
//
// The assertion that matters is "through the API". A test that seeds a
// row with `INSERT INTO assets (..., team_id, ...)` — as five test files
// in this repo already do, correctly, to test the RULES — cannot fail
// when the WRITE PATH is missing. That is exactly how this shipped four
// sprints running with a green suite. So every case here goes through
// h.CreateAsset, reads the answer back through h.GetAsset, and the
// end-to-end case proves the persisted column is authorisation-live
// rather than merely echoed.
//
// The gate is the same rule POST /posts runs (#954) — one predicate,
// visibility.CanAssignToTeam, two call sites. Its cases:
//
//   - a non-member with no grant is REFUSED, and no asset row lands;
//   - the refusal is byte-identical to the one a NONEXISTENT team gets,
//     or POST /assets becomes a team-existence probe across the
//     instance;
//   - a direct member succeeds;
//   - a scoped `assets.admin` on a PARENT team reaches a DESCENDANT,
//     through a hierarchy this file builds with team_parents and lets
//     the 00015 trigger materialise — the closure is the database's
//     answer, not a literal;
//   - a GLOBAL `assets.admin` is deliberately NOT enough (see
//     CanAssignToTeam);
//   - `system.admin` may assign anywhere;
//   - omitting the field still works, and remains the common case.
//
// Skips without AA_DB_PASSWORD.
package assets_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// taCreate drives the real POST /assets as `userRef`, optionally naming
// a team. Nothing here touches SQL — that is the point of the file.
func taCreate(f *maFixture, userRef int64, team *uuid.UUID) openapi.CreateAssetResponseObject {
	f.t.Helper()
	body := &openapi.AssetCreate{Title: "ta-upload", AssetType: 1}
	if team != nil {
		v := openapi_types.UUID(*team)
		body.TeamId = &v
	}
	resp, err := f.h.CreateAsset(f.identity(userRef), openapi.CreateAssetRequestObject{Body: body})
	if err != nil {
		f.t.Fatalf("CreateAsset: %v", err)
	}
	return resp
}

// taCreated unwraps a 201 and registers cleanup for the row it made.
func taCreated(f *maFixture, resp openapi.CreateAssetResponseObject) openapi.Asset {
	f.t.Helper()
	created, ok := resp.(openapi.CreateAsset201JSONResponse)
	if !ok {
		f.t.Fatalf("CreateAsset returned %T, want 201", resp)
	}
	a := openapi.Asset(created)
	id := uuid.UUID(a.Id)
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
	})
	return a
}

// taJoin makes userRef a DIRECT member of team. Deliberately raw: what
// #953 is about is the ASSET write path, and membership already has an
// API of its own.
func taJoin(f *maFixture, team uuid.UUID, userRef int64) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, team, userRef,
	); err != nil {
		f.t.Fatalf("seed membership: %v", err)
	}
}

// taRefusal asserts a 404 and hands back the body so two refusals can be
// compared for sameness.
// Non-fatal on the wrong shape, so a handler that answers 201 still
// reaches the row-count assertion — the more important half of the
// evidence is that a row was WRITTEN, not that the status was wrong.
func taRefusal(f *maFixture, resp openapi.CreateAssetResponseObject) string {
	f.t.Helper()
	nf, ok := resp.(openapi.CreateAsset404JSONResponse)
	if !ok {
		f.t.Errorf("CreateAsset returned %T, want CreateAsset404JSONResponse", resp)
		return ""
	}
	return nf.Error
}

// taAssetCount asks the DATABASE how many assets this user owns. "The
// write did not happen" must not be answered by the code path that might
// have let it happen — a gate that refuses AFTER inserting passes every
// status-only assertion.
func taAssetCount(f *maFixture, userRef int64) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM assets WHERE owner_user_ref = $1`, userRef,
	).Scan(&n); err != nil {
		f.t.Fatalf("count assets: %v", err)
	}
	return n
}

// TestCreateAsset_TeamGate is the whole matrix, driven through the API.
func TestCreateAsset_TeamGate(t *testing.T) {
	f := newMAFixture(t)

	parent := f.team("ta-division", nil)
	child := f.team("ta-squad", &parent)
	unrelated := f.team("ta-other", nil)

	member := f.user("member")
	taJoin(f, unrelated, member)

	stranger := f.user("stranger")

	director := f.user("director") // scoped assets.admin on the PARENT
	f.grant(director, visibility.AssetsAdmin, &parent)

	moderator := f.user("moderator") // GLOBAL assets.admin, no team
	f.grant(moderator, visibility.AssetsAdmin, nil)

	root := f.user("root")
	f.grant(root, visibility.SystemAdmin, nil)

	// A soft-deleted team the caller IS a member of. The FK does not
	// read deleted_at, so without the liveness probe this would land.
	dead := f.team("ta-dead", nil)
	taJoin(f, dead, member)
	if _, err := f.pool.Exec(f.ctx, `UPDATE teams SET deleted_at = now() WHERE id = $1`, dead); err != nil {
		t.Fatalf("soft-delete team: %v", err)
	}

	missing := uuid.New()

	cases := []struct {
		name    string
		caller  int64
		team    *uuid.UUID
		wantOK  bool
		wantTID *uuid.UUID
		why     string
	}{
		{
			name: "stranger names a real team they have nothing to do with",
			// ⭐ THE case. Before #953 this could not be expressed at
			// all; the posts twin of it (#954) returned 201 and wrote
			// the row.
			caller: stranger, team: &unrelated, wantOK: false,
			why: "not a member, no scoped grant — the provenance gap",
		},
		{
			name:   "nonexistent team",
			caller: stranger, team: &missing, wantOK: false,
			why: "compared byte-for-byte against the case above",
		},
		{
			name:   "direct member of the team",
			caller: member, team: &unrelated, wantOK: true, wantTID: &unrelated,
			why: "membership is the ordinary path and must not be gated away",
		},
		{
			name:   "scoped assets.admin on the PARENT, assigning to the DESCENDANT",
			caller: director, team: &child, wantOK: true, wantTID: &child,
			why: "the grant half closes over team_closure (ADR 0010 Layer 5); " +
				"the hierarchy here is real and the expansion is the resolver's",
		},
		{
			name:   "scoped assets.admin on the parent, assigning to an UNRELATED team",
			caller: director, team: &unrelated, wantOK: false,
			why: "the closure reaches descendants, not siblings — a gate that " +
				"admitted this would be a gate on nothing",
		},
		{
			name:   "GLOBAL assets.admin, not a member",
			caller: moderator, team: &unrelated, wantOK: false,
			why: "deliberate. A global grant is the instance-moderator role; " +
				"moderating every asset is not a claim on any team's identity. " +
				"Identity.ScopedTeams excludes globals for exactly this reason",
		},
		{
			name:   "system.admin, not a member",
			caller: root, team: &unrelated, wantOK: true, wantTID: &unrelated,
			why: "the one escape hatch, checked explicitly",
		},
		{
			name:   "member of a SOFT-DELETED team",
			caller: member, team: &dead, wantOK: false,
			why: "the FK does not read teams.deleted_at; the liveness probe does",
		},
		{
			name:   "no team at all",
			caller: stranger, team: nil, wantOK: true,
			why: "the common case — team_id is optional and NULL is the norm",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := taAssetCount(f, tc.caller)
			resp := taCreate(f, tc.caller, tc.team)

			if !tc.wantOK {
				// Checked FIRST and on its own — the gate must refuse
				// BEFORE the insert, and a status-only assertion cannot
				// tell that from a gate that refuses after writing.
				if after := taAssetCount(f, tc.caller); after != before {
					t.Errorf("WROTE THE ASSET: rows owned by %d went %d -> %d (%s)",
						tc.caller, before, after, tc.why)
				}
				if body := taRefusal(f, resp); body != "team not found" {
					t.Errorf("refusal body = %q, want %q (%s)", body, "team not found", tc.why)
				}
				return
			}

			a := taCreated(f, resp)
			switch {
			case tc.wantTID == nil && a.TeamId != nil:
				t.Errorf("team_id = %v, want absent (%s)", *a.TeamId, tc.why)
			case tc.wantTID != nil && a.TeamId == nil:
				t.Errorf("team_id absent, want %v (%s)", *tc.wantTID, tc.why)
			case tc.wantTID != nil && uuid.UUID(*a.TeamId) != *tc.wantTID:
				t.Errorf("team_id = %v, want %v (%s)", uuid.UUID(*a.TeamId), *tc.wantTID, tc.why)
			}

			// ⭐ #953's actual acceptance: read it back THROUGH THE API.
			// The create response could in principle be echoing the
			// request; GET reads the row.
			got := fpGet(f, f.identity(tc.caller), uuid.UUID(a.Id))
			switch {
			case tc.wantTID == nil && got.TeamId != nil:
				t.Errorf("GET team_id = %v, want absent", *got.TeamId)
			case tc.wantTID != nil && (got.TeamId == nil || uuid.UUID(*got.TeamId) != *tc.wantTID):
				t.Errorf("GET team_id = %v, want %v", got.TeamId, *tc.wantTID)
			}
		})
	}
}

// TestCreateAsset_TeamGate_RefusalsAreIndistinguishable pins the
// enumeration property on its own, because it is the one an ordinary
// review misses: both refusals being 404 is not enough if the BODIES
// differ. A caller who can tell "no such team" from "not your team" can
// enumerate every studio on the instance one UUID at a time.
func TestCreateAsset_TeamGate_RefusalsAreIndistinguishable(t *testing.T) {
	f := newMAFixture(t)
	real := f.team("ta-secret-studio", nil)
	stranger := f.user("prober")

	unauthorised := taRefusal(f, taCreate(f, stranger, &real))
	nonexistent := taRefusal(f, taCreate(f, stranger, uuidPtr(uuid.New())))

	if unauthorised != nonexistent {
		t.Errorf("an unauthorised team is distinguishable from a nonexistent one:\n"+
			"  unauthorised: %q\n  nonexistent:  %q\n"+
			"POST /assets is a team-existence probe", unauthorised, nonexistent)
	}
}

// TestCreateAsset_TeamAssetIsTeamReadable is #953's end-to-end: the
// property the SEEDED catalogue already demonstrated, now reachable
// without the seeder.
//
// An asset created THROUGH THE API into a team, then marked
// `sensitivity='team'`, is readable by a fellow member and withheld from
// a non-member. That is what proves the persisted column is
// authorisation-live rather than a value the create response echoed
// back: `ContentReadable` is `case "team": return isTeamMember`, and
// isTeamMember is resolved from `team_memberships` against THIS row's
// team_id.
//
// `sensitivity` is set with SQL because no API surface carries it — that
// is a separate gap, and it is not what this test is about. The
// team_id — the thing #953 is about — arrives entirely through the API.
func TestCreateAsset_TeamAssetIsTeamReadable(t *testing.T) {
	f := newMAFixture(t)
	team := f.team("ta-studio", nil)

	uploader := f.user("uploader")
	colleague := f.user("colleague")
	outsider := f.user("outsider")
	taJoin(f, team, uploader)
	taJoin(f, team, colleague)

	a := taCreated(f, taCreate(f, uploader, &team))
	id := uuid.UUID(a.Id)
	if a.TeamId == nil || uuid.UUID(*a.TeamId) != team {
		t.Fatalf("created asset carries team_id %v, want %v", a.TeamId, team)
	}
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE assets SET sensitivity = 'team' WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("set sensitivity: %v", err)
	}

	if got := fpGet(f, f.identity(colleague), id); got.Restricted {
		t.Errorf("a fellow team member was refused an asset at sensitivity='team' " +
			"that the API put in their team — the team tier admits nobody")
	}
	if got := fpGet(f, f.identity(outsider), id); !got.Restricted {
		t.Errorf("a non-member READ an asset at sensitivity='team' — the tier gates nothing")
	}
}

func uuidPtr(v uuid.UUID) *uuid.UUID { return &v }
