// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1209: `anonymously_visible` on the browse row, which is the surface
// the cover picker searches.
//
// WHY THIS IS A SEPARATE TEST FROM THE PREDICATE PIN. The pin in
// internal/visibility proves the rule has all four conjuncts and that
// the Go evaluator agrees with the SQL. It proves nothing about whether
// the flag REACHES the payload the picker reads, which is a different
// claim and the one #1209 is actually about: the picker could only
// check `status` because the row it holds carried nothing else.
//
// The case that matters is `status = 'active'` AND
// `sensitivity != 'public'`. That is precisely what the old client-side
// check could not catch, so it is the case a green run has to include
// rather than merely be compatible with.
//
// ⚠️ IT IS NOT A READABILITY DECISION, and the last test says so with a
// caller who can read a row the flag reports false for. A surface that
// started gating on this would be wrong for every signed-in caller.
//
// Skips without AA_DB_PASSWORD, same convention as the sibling suites.

package assets

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// anonVisibleRow finds one seeded asset in the owner's browse page.
func anonVisibleRow(t *testing.T, id uuid.UUID) ListAssetsPageGatedRow {
	t.Helper()
	caller := visibility.NewCaller(ptrTo(ladderOwner))
	row, ok := ladderRow(t, caller, nil, id, []string{"col"})
	if !ok {
		t.Fatalf("seeded asset %s did not come back on the owner's browse page", id)
	}
	return row
}

func TestAnonymouslyVisible_OnTheBrowseRow(t *testing.T) {
	// The one combination that passes, so a suite reporting "false
	// everywhere" cannot read as agreement.
	t.Run("an active public ready asset is anonymously visible", func(t *testing.T) {
		id := seedLadderAsset(t, "public", "col")
		if !anonVisibleRow(t, id).AnonymouslyVisible {
			t.Errorf("an active/public/ready asset must be anonymously visible")
		}
	})

	// ⭐ THE #1209 CASE. Active, so the old `status` check passed it and
	// the picker stayed silent; team-tier, so an anonymous visitor is
	// shown the fallback instead.
	for _, tier := range []string{"team", "restricted", "embargo"} {
		t.Run("an active "+tier+"-tier asset is NOT", func(t *testing.T) {
			id := seedLadderAsset(t, tier, "col")
			row := anonVisibleRow(t, id)
			if row.AnonymouslyVisible {
				t.Errorf("sensitivity=%s is not for strangers, but the row says it is visible to them", tier)
			}
			// The half the client could already see, pinned so a
			// regression cannot be mistaken for this test passing on
			// `status` alone.
			if row.Status != "active" {
				t.Fatalf("fixture is not the case under test: status=%q, want active", row.Status)
			}
		})
	}

	t.Run("the flag is about the ROW, not about the caller", func(t *testing.T) {
		id := seedLadderAsset(t, "restricted", "col")
		// The OWNER, who reads this row perfectly well.
		row := anonVisibleRow(t, id)
		if !row.Readable {
			t.Fatalf("the owner must be able to read their own asset's columns")
		}
		if row.AnonymouslyVisible {
			t.Errorf("a caller who can read the row must still be told a stranger cannot")
		}
	})
}

func TestAnonymouslyVisible_SoftDeleteAndProcessing(t *testing.T) {
	pool := listPagePool(t)
	ctx := t.Context()

	t.Run("an asset still processing is not", func(t *testing.T) {
		id := seedLadderAsset(t, "public", "col")
		if _, err := pool.Exec(ctx,
			`UPDATE assets SET processing_status = 'pending' WHERE id = $1`, id); err != nil {
			t.Fatalf("set processing_status: %v", err)
		}
		if anonVisibleRow(t, id).AnonymouslyVisible {
			t.Errorf("an asset with no derivatives yet must not be reported visible to strangers")
		}
	})

	t.Run("a soft-deleted asset is not", func(t *testing.T) {
		id := seedLadderAsset(t, "public", "col")
		if _, err := pool.Exec(ctx,
			`UPDATE assets SET deleted_at = NOW() WHERE id = $1`, id); err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		// A soft-deleted row is off the ordinary browse page, so this
		// asks for the admin trash view instead of anonVisibleRow.
		rows, err := ListAssetsPageGated(ctx, pool, visibility.NewCaller(ptrTo(ladderOwner)), nil,
			ListAssetsPageGatedParams{
				OwnerUserRef:   ptrTo(ladderOwner),
				RowLimit:       50,
				Ladder:         []string{"col"},
				IncludeDeleted: ptrTo(true),
			})
		if err != nil {
			t.Fatalf("gated list: %v", err)
		}
		found := false
		for _, r := range rows {
			if uuid.UUID(r.ID.Bytes) != id {
				continue
			}
			found = true
			if r.AnonymouslyVisible {
				t.Errorf("a soft-deleted asset must not be reported visible to strangers")
			}
		}
		if !found {
			t.Fatalf("the soft-deleted fixture did not come back on the trash view; "+
				"the assertion above proved nothing about asset %s", id)
		}
	})
}
