// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #954 — `PostCreate.team_id` was caller-asserted.
//
// # What was wrong
//
// CreatePost took `team_id` verbatim out of the request body:
//
//	var teamID pgtype.UUID
//	if in.TeamId != nil {
//	    teamID = pgtype.UUID{Bytes: uuid.UUID(*in.TeamId), Valid: true}
//	}
//
// There was no membership check anywhere in the handler. The only
// validation was the FOREIGN KEY, which rejects a nonexistent team with
// a 404 and accepts every EXISTING one. So any authenticated caller
// could attribute a post to any studio on the instance.
//
// # Severity, stated precisely
//
// This is not a read leak — visibility/post_rule.go never consults
// team_id, so nothing became readable that was not readable before. It
// is a write-side integrity gap of the same class as #916's unvalidated
// principal_id, with a second edge: canMutatePost is
// `author OR Can(posts.admin, InTeam(team_id)) OR global`, so naming
// team X handed X's posts.admin holders edit and delete rights over
// your post. The field looked like a label and behaved like a grant.
//
// # Red-first
//
// Every refusal case in this file returned **201** against the previous
// handler, with a `posts` row carrying the foreign team. That is why
// each one asserts the DATABASE state and not only the status: a gate
// that refuses after writing is a gate that does not exist, and a
// status-only assertion cannot tell the two apart.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// tgCtx is a context carrying the identity the MIDDLEWARE would build —
// loaded through auth.Resolver against real rows, so a team-scoped grant
// arrives closure-expanded from `team_closure` rather than from a
// literal this test wrote. ctxAs (the synthetic Identity used elsewhere
// in this package) cannot carry a scoped grant at all, so it would make
// the descendant case assert nothing.
func tgCtx(f *pmFixture, userRef int64) context.Context {
	f.t.Helper()
	return auth.WithIdentity(context.Background(), f.identity(userRef))
}

// tgJoin makes userRef a DIRECT member of team.
func tgJoin(f *pmFixture, team uuid.UUID, userRef int64) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, team, userRef,
	); err != nil {
		f.t.Fatalf("seed membership: %v", err)
	}
}

// tgPostCount asks the DATABASE, not the handler.
func tgPostCount(t *testing.T, f *pmFixture, author int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM posts WHERE author_user_ref = $1`, author).Scan(&n); err != nil {
		t.Fatalf("count posts: %v", err)
	}
	return n
}

// tgCreate drives the real POST /posts with one member the caller owns,
// optionally naming a team.
func tgCreate(t *testing.T, f *pmFixture, h *Handler, author int64, team *uuid.UUID) openapi.CreatePostResponseObject {
	t.Helper()
	assetID := seedPreviewAssetOwned(t, h.Pool, "public", false, author)
	title := "tg post"
	body := &openapi.PostCreate{
		Title:   &title,
		Members: []openapi.PostAssetWrite{{AssetId: openapi_types.UUID(assetID)}},
	}
	if team != nil {
		v := openapi_types.UUID(*team)
		body.TeamId = &v
	}
	resp, err := h.CreatePost(tgCtx(f, author), openapi.CreatePostRequestObject{Body: body})
	if err != nil {
		t.Fatalf("CreatePost: %v", err)
	}
	if created, ok := resp.(openapi.CreatePost201JSONResponse); ok {
		id := uuid.UUID(openapi.Post(created).Id)
		t.Cleanup(func() {
			c := context.Background()
			_, _ = h.Pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, id)
			_, _ = h.Pool.Exec(c, `DELETE FROM posts WHERE id = $1`, id)
		})
	}
	return resp
}

// tgRefusal reports the refusal body, or "" when the handler did not
// refuse at all. Deliberately non-fatal: against the PRE-GATE handler
// every refusal case answered 201, and a t.Fatal here would stop the
// test before the row-count assertion — hiding the more important half
// of the evidence, that the post was actually WRITTEN.
func tgRefusal(t *testing.T, resp openapi.CreatePostResponseObject) string {
	t.Helper()
	nf, ok := resp.(openapi.CreatePost404JSONResponse)
	if !ok {
		t.Errorf("CreatePost returned %T, want CreatePost404JSONResponse", resp)
		return ""
	}
	return nf.Error
}

// TestCreatePost_TeamGate is #954's matrix. Every wantOK=false row here
// returned 201 before the gate landed.
func TestCreatePost_TeamGate(t *testing.T) {
	f := newPMFixture(t)
	h := wireWriteHandler(t)

	parent := f.team("tg-division", nil)
	child := f.team("tg-squad", &parent)
	unrelated := f.team("tg-other", nil)

	member := f.user("member")
	tgJoin(f, unrelated, member)

	stranger := f.user("stranger")

	director := f.user("director") // scoped posts.admin on the PARENT
	f.grant(director, CapPostsAdmin, &parent)

	moderator := f.user("moderator") // GLOBAL posts.admin
	f.grant(moderator, CapPostsAdmin, nil)

	root := f.user("root")
	f.grant(root, CapSystemAdmin, nil)

	dead := f.team("tg-dead", nil)
	tgJoin(f, dead, member)
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
			// ⭐ THE case #954 is about. 201 before the gate.
			name:   "stranger attributes a post to a studio they are not in",
			caller: stranger, team: &unrelated, wantOK: false,
			why: "no membership, no scoped grant. The FK accepted it because the " +
				"team EXISTS, which was the entire validation",
		},
		{
			name:   "nonexistent team",
			caller: stranger, team: &missing, wantOK: false,
			why: "the pre-existing FK 404; compared byte-for-byte against the case above",
		},
		{
			name:   "direct member of the team",
			caller: member, team: &unrelated, wantOK: true, wantTID: &unrelated,
			why: "the ordinary path the upload modal's team picker drives",
		},
		{
			name:   "scoped posts.admin on the PARENT, assigning to the DESCENDANT",
			caller: director, team: &child, wantOK: true, wantTID: &child,
			why: "a delegated administrative right closes over team_closure " +
				"(ADR 0010 Layer 5); the resolver did the expansion, not this test",
		},
		{
			name:   "scoped posts.admin on the parent, assigning to an UNRELATED team",
			caller: director, team: &unrelated, wantOK: false,
			why: "descendants, not siblings",
		},
		{
			name:   "GLOBAL posts.admin, not a member",
			caller: moderator, team: &unrelated, wantOK: false,
			why: "deliberate. Moderating every post on the instance is not a claim " +
				"on any studio's identity — planting a post in a studio's space is " +
				"the provenance problem #954 names. system.admin is the escape hatch",
		},
		{
			name:   "system.admin, not a member",
			caller: root, team: &unrelated, wantOK: true, wantTID: &unrelated,
			why: "the escape hatch, checked explicitly",
		},
		{
			name:   "member of a SOFT-DELETED team",
			caller: member, team: &dead, wantOK: false,
			why: "the FK does not read teams.deleted_at; the liveness probe does",
		},
		{
			name:   "no team at all",
			caller: stranger, team: nil, wantOK: true,
			why: "the common case; team_id is optional and NULL is the norm today",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tgPostCount(t, f, tc.caller)
			resp := tgCreate(t, f, h, tc.caller, tc.team)

			if !tc.wantOK {
				// ⭐ Checked FIRST, and on its own: this is the
				// assertion that distinguishes a gate from a message.
				// No post row may land.
				if after := tgPostCount(t, f, tc.caller); after != before {
					t.Errorf("WROTE THE POST: rows by author %d went %d -> %d (%s)",
						tc.caller, before, after, tc.why)
				}
				if body := tgRefusal(t, resp); body != "team not found" {
					t.Errorf("refusal body = %q, want %q (%s)", body, "team not found", tc.why)
				}
				return
			}

			created, ok := resp.(openapi.CreatePost201JSONResponse)
			if !ok {
				t.Fatalf("CreatePost returned %T, want 201 (%s)", resp, tc.why)
			}
			p := openapi.Post(created)
			switch {
			case tc.wantTID == nil && p.TeamId != nil:
				t.Errorf("team_id = %v, want absent (%s)", *p.TeamId, tc.why)
			case tc.wantTID != nil && p.TeamId == nil:
				t.Errorf("team_id absent, want %v (%s)", *tc.wantTID, tc.why)
			case tc.wantTID != nil && uuid.UUID(*p.TeamId) != *tc.wantTID:
				t.Errorf("team_id = %v, want %v (%s)", uuid.UUID(*p.TeamId), *tc.wantTID, tc.why)
			}

			// And the column the authorisation rules actually read.
			var stored *uuid.UUID
			if err := f.pool.QueryRow(context.Background(),
				`SELECT team_id FROM posts WHERE id = $1`, uuid.UUID(p.Id)).Scan(&stored); err != nil {
				t.Fatalf("read back team_id: %v", err)
			}
			switch {
			case tc.wantTID == nil && stored != nil:
				t.Errorf("posts.team_id = %v, want NULL", *stored)
			case tc.wantTID != nil && (stored == nil || *stored != *tc.wantTID):
				t.Errorf("posts.team_id = %v, want %v", stored, *tc.wantTID)
			}
		})
	}
}

// TestCreatePost_TeamGate_RefusalsAreIndistinguishable pins the
// enumeration property on its own. Both refusals being 404 is not
// enough: a caller who can tell "no such team" from "not your team" can
// enumerate every studio on the instance one UUID at a time, which is
// the same discipline #922 / #941 / #952 hold the asset gates to.
func TestCreatePost_TeamGate_RefusalsAreIndistinguishable(t *testing.T) {
	f := newPMFixture(t)
	h := wireWriteHandler(t)

	real := f.team("tg-secret-studio", nil)
	stranger := f.user("prober")

	unauthorised := tgRefusal(t, tgCreate(t, f, h, stranger, &real))
	nonexistent := tgRefusal(t, tgCreate(t, f, h, stranger, uuidPtr(uuid.New())))

	if unauthorised != nonexistent {
		t.Errorf("an unauthorised team is distinguishable from a nonexistent one:\n"+
			"  unauthorised: %q\n  nonexistent:  %q\n"+
			"POST /posts is a team-existence probe", unauthorised, nonexistent)
	}
}

func uuidPtr(v uuid.UUID) *uuid.UUID { return &v }
