// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package userprefs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

// The team rail's curation (#1113) — the list of chips the reader took
// out of their rail, and the order they dragged the rest into.
//
// # What these tests assert, and why it is the DB row
//
// The whole preference is a round trip: the manage panel writes it,
// /auth/me reads it back on the next session, and the rail renders from
// that. A test that saves through the handler and reads back through
// the same handler proves only that the handler agrees with itself —
// the cache in front of it would satisfy that even if the column were
// never written. So these read the JSONB column directly.
//
// That is the #946 lesson applied here: a handler that echoes its own
// write passes an equality assertion on the bug.

const (
	teamA = "3b6770c6-b35a-90d1-88c7-e35d00136825"
	teamB = "988ed4d0-4b3e-66a9-5606-c056c32bee04"
	teamC = "c0e2652d-65b1-cb0f-8c25-3260a5b8834d"
)

func readTeamRailColumn(t *testing.T, h *Handler, ref int64) TeamRail {
	t.Helper()
	var raw []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT team_rail FROM user_preferences WHERE user_ref = $1`, ref,
	).Scan(&raw); err != nil {
		t.Fatalf("read team_rail column: %v", err)
	}
	var got TeamRail
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode team_rail %q: %v", raw, err)
	}
	return got
}

func TestSavePreferences_PersistsTeamRailToTheColumn(t *testing.T) {
	pool := unsubTestPool(t)
	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref := seedUnsubUser(t, pool)
	ctx := context.Background()

	if err := h.savePreferences(ctx, ref, Preferences{
		TeamRail: TeamRail{
			HiddenTeamIDs: []string{teamA},
			TeamOrder:     []string{teamC, teamB},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := readTeamRailColumn(t, h, ref)
	if len(got.HiddenTeamIDs) != 1 || got.HiddenTeamIDs[0] != teamA {
		t.Errorf("hidden_team_ids=%v want [%s]", got.HiddenTeamIDs, teamA)
	}
	// ORDER, not membership. The whole point of the key is the sequence,
	// so a set comparison here would pass on a save that sorted it.
	if len(got.TeamOrder) != 2 || got.TeamOrder[0] != teamC || got.TeamOrder[1] != teamB {
		t.Errorf("team_order=%v want [%s %s]", got.TeamOrder, teamC, teamB)
	}
}

// An untouched rail must persist as `{}` — the same bytes the column
// defaults to — so "saved but never curated" and "never saved" are
// indistinguishable on disk. Same guarantee MarshalFeedFilters carries
// for the boolean bag, and it is what lets the /auth/me decoder omit
// the object for the overwhelming majority of accounts.
func TestSavePreferences_DefaultTeamRailPersistsAsEmptyObject(t *testing.T) {
	pool := unsubTestPool(t)
	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref := seedUnsubUser(t, pool)

	if err := h.savePreferences(context.Background(), ref, Preferences{}); err != nil {
		t.Fatalf("save: %v", err)
	}
	var raw []byte
	if err := h.pool.QueryRow(context.Background(),
		`SELECT team_rail FROM user_preferences WHERE user_ref = $1`, ref,
	).Scan(&raw); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "{}" {
		t.Errorf("team_rail=%s want {}", raw)
	}
}

// The #891 defect class, aimed at the column this change adds: an
// unrelated write must not reset a preference it does not mention. The
// one-click unsubscribe link is the write a user takes without thinking
// about their rail, and it goes through the same savePreferences.
func TestUnsubscribeEmail_PreservesTeamRail(t *testing.T) {
	pool := unsubTestPool(t)
	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref := seedUnsubUser(t, pool)
	ctx := context.Background()

	if err := h.savePreferences(ctx, ref, Preferences{
		NotificationChannels: NotificationChannels{EventMentionOfMe: {ChannelInApp, ChannelEmail}},
		TeamRail:             TeamRail{HiddenTeamIDs: []string{teamA}, TeamOrder: []string{teamB}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := h.UnsubscribeEmail(ctx, ref, EventMentionOfMe); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}

	got := readTeamRailColumn(t, h, ref)
	if len(got.HiddenTeamIDs) != 1 || len(got.TeamOrder) != 1 {
		t.Fatalf("unsubscribing reset the reader's rail curation: %+v", got)
	}
}

func TestTeamRailSanitized_DropsUnusableEntries(t *testing.T) {
	in := TeamRail{
		HiddenTeamIDs: []string{teamA, "", teamA, "not-a-uuid", teamB},
		TeamOrder:     []string{"also-not-a-uuid"},
	}
	got := in.Sanitized()

	want := []string{teamA, teamB}
	if len(got.HiddenTeamIDs) != len(want) {
		t.Fatalf("hidden_team_ids=%v want %v", got.HiddenTeamIDs, want)
	}
	for i := range want {
		if got.HiddenTeamIDs[i] != want[i] {
			t.Errorf("hidden_team_ids[%d]=%s want %s", i, got.HiddenTeamIDs[i], want[i])
		}
	}
	// Nil rather than an empty slice, so it marshals away under
	// `omitempty` and the blob stays `{}`.
	if got.TeamOrder != nil {
		t.Errorf("team_order=%v want nil — an all-garbage list must not persist as []", got.TeamOrder)
	}
}

// An id naming a team that no longer exists, or one this caller cannot
// see, is KEPT. It is inert at render (the rail intersects with what the
// server returned) and dropping it here would silently un-hide a team
// the moment it briefly left the reader's visibility.
func TestTeamRailSanitized_KeepsUnknownButWellformedIDs(t *testing.T) {
	const ghost = "00000000-0000-0000-0000-0000000000ff"
	got := TeamRail{HiddenTeamIDs: []string{ghost}}.Sanitized()
	if len(got.HiddenTeamIDs) != 1 || got.HiddenTeamIDs[0] != ghost {
		t.Errorf("hidden_team_ids=%v want [%s]", got.HiddenTeamIDs, ghost)
	}
}

func TestValidatePreferences_RejectsOversizedTeamRailLists(t *testing.T) {
	long := make([]string, MaxTeamRailIDs+1)
	for i := range long {
		long[i] = teamA
	}
	if err := ValidatePreferences(Preferences{TeamRail: TeamRail{HiddenTeamIDs: long}}); err == nil {
		t.Error("a hidden_team_ids list past the cap was accepted")
	}
	if err := ValidatePreferences(Preferences{TeamRail: TeamRail{TeamOrder: long}}); err == nil {
		t.Error("a team_order list past the cap was accepted")
	}
}
