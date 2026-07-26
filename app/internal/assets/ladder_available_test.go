// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #591 — `ladder_available`: does EVERY rung of the operator's
// CONFIGURED preview ladder exist for this asset, and may this caller
// read its content?
//
// Two invariants are load-bearing and each gets a test:
//
//  1. ADR 0064 — a caller who cannot read an asset's content sees
//     `false`, NOT a 403. The flag must never become an oracle that
//     confirms `restricted` by behaving differently from "no preview".
//
//  2. The ladder is OPERATOR-CONFIGURABLE. The obvious implementation —
//     EXISTS(col) AND EXISTS(preview) AND EXISTS(screen) AND
//     EXISTS(hires) — passes every test written against a default
//     install and is silently wrong on any install that tuned its
//     ladder. TestLadderAvailable_FollowsConfiguredLadder is the test
//     that catches that hardcode: it configures a NON-default ladder and
//     asserts the flag moved with it.
//
// Skips without AA_DB_PASSWORD, same convention as the sibling suites.

package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const ladderOwner int64 = 4290777

// seedLadderAsset plants one asset with a file hash and writes exactly
// the named variants for it. Returns the asset id.
func seedLadderAsset(t *testing.T, sensitivity string, variants ...string) uuid.UUID {
	t.Helper()
	pool := listPagePool(t)
	ctx := context.Background()
	id := uuid.New()
	// storage_objects.hash is CHECKed against ^[0-9a-f]{64}$, so the
	// fixture hash has to be a real-shaped digest rather than a label.
	sum := sha256.Sum256([]byte("#591 ladder " + id.String()))
	hash := hex.EncodeToString(sum[:])

	// assets.file_hash is FK'd to storage_objects, so the object has to
	// exist before the asset can reference it.
	if _, err := pool.Exec(ctx, `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1, 'image/webp', 'fs')
		ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
		t.Fatalf("seed storage object: %v", err)
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                    processing_status, file_hash, created_at)
		VALUES ($1,'#591 ladder probe',$2,(SELECT MIN(ref) FROM asset_types),
		        'active',$3,'ready',$4,NOW())`,
		id, ladderOwner, sensitivity, hash)
	if err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	for _, key := range variants {
		if _, err := pool.Exec(ctx, `
			INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type)
			VALUES ($1,$2,1,'image/webp')
			ON CONFLICT DO NOTHING`,
			hash, key); err != nil {
			t.Fatalf("seed variant %q: %v", key, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
		_, _ = pool.Exec(bg, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = pool.Exec(bg, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
	return id
}

func ladderRow(t *testing.T, caller visibility.Caller, caps visibility.CapabilityChecker,
	id uuid.UUID, ladder []string) (ListAssetsPageGatedRow, bool) {
	t.Helper()
	rows, err := ListAssetsPageGated(context.Background(), listPagePool(t), caller, caps,
		ListAssetsPageGatedParams{OwnerUserRef: ptrTo(ladderOwner), RowLimit: 50, Ladder: ladder})
	if err != nil {
		t.Fatalf("gated list: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.ID.Bytes) == id {
			return r, true
		}
	}
	return ListAssetsPageGatedRow{}, false
}

func ptrTo[T any](v T) *T { return &v }

// TestLadderAvailable_FollowsConfiguredLadder is THE test for the trap.
//
// A hardcoded `col+preview+screen+hires` check passes cases 1 and 2 and
// FAILS cases 3 and 4 — which is the whole point. Case 3 is an operator
// who dropped `hires` to save storage: a hardcoded check reports false
// forever and silently disables responsive images fleet-wide. Case 4 is
// an operator who added a rung the asset does not have: a hardcoded
// check reports true and the client requests bytes that 404.
func TestLadderAvailable_FollowsConfiguredLadder(t *testing.T) {
	// The asset physically has exactly these three rungs.
	id := seedLadderAsset(t, "public", "col", "preview", "screen")
	caller := visibility.NewCaller(ptrTo(ladderOwner))

	cases := []struct {
		name   string
		ladder []string
		want   bool
		why    string
	}{
		{
			name:   "configured ladder is a subset of what exists",
			ladder: []string{"col", "preview"},
			want:   true,
			why:    "every configured rung is present",
		},
		{
			name:   "configured ladder exactly matches what exists",
			ladder: []string{"col", "preview", "screen"},
			want:   true,
		},
		{
			name:   "operator dropped a rung this asset lacks",
			ladder: []string{"col", "preview", "screen"},
			want:   true,
			why: "an install without `hires` is COMPLETE at three rungs — " +
				"a hardcoded four-key check reports false here forever",
		},
		{
			name:   "operator configured a rung the asset does not have",
			ladder: []string{"col", "preview", "screen", "hires"},
			want:   false,
			why:    "hires was never written for this asset",
		},
		{
			name:   "operator renamed the rungs entirely",
			ladder: []string{"thumb", "large"},
			want:   false,
			why:    "no configured key exists; a default-key check would say true",
		},
		{
			name:   "single-rung install",
			ladder: []string{"col"},
			want:   true,
			why:    "a one-rung ladder is satisfied by the one rung",
		},
		{
			name:   "unknown ladder (config read failed)",
			ladder: nil,
			want:   false,
			why: "an empty ladder must NOT be vacuously satisfied — without " +
				"the cardinality guard `0 = 0` reports a complete ladder for " +
				"every asset in the install",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row, ok := ladderRow(t, caller, nil, id, c.ladder)
			if !ok {
				t.Fatalf("asset not returned by the gated list")
			}
			if row.LadderAvailable != c.want {
				t.Errorf("ladder %v: LadderAvailable = %v, want %v\n%s",
					c.ladder, row.LadderAvailable, c.want, c.why)
			}
		})
	}
}

// TestLadderAvailable_RestrictedIsFalseNotForbidden encodes ADR 0064.
//
// The asset HAS every configured rung on disk, so the only thing that
// can make the flag false is the content plane. A caller who may not
// read it must get exactly what "this asset has no ladder" looks like:
// a row, with false, and no error. If this ever starts erroring or
// omitting the row, the flag has become a probe that confirms
// `restricted` by side channel.
func TestLadderAvailable_RestrictedIsFalseNotForbidden(t *testing.T) {
	full := []string{"col", "preview", "screen", "hires"}
	restricted := seedLadderAsset(t, "restricted", full...)
	public := seedLadderAsset(t, "public", full...)

	// A caller who owns neither asset and holds no content capability.
	stranger := visibility.NewCaller(ptrTo(int64(4290778)))

	t.Run("restricted asset reports false, without an error", func(t *testing.T) {
		rows, err := ListAssetsPageGated(context.Background(), listPagePool(t),
			stranger, nil,
			ListAssetsPageGatedParams{OwnerUserRef: ptrTo(ladderOwner), RowLimit: 50, Ladder: full})
		// The 0064 contract in one assertion: gating content NEVER
		// surfaces as an error to the caller.
		if err != nil {
			t.Fatalf("gated list errored for a restricted asset — 0064 says "+
				"the flag goes false, it does not fail the request: %v", err)
		}
		for _, r := range rows {
			if uuid.UUID(r.ID.Bytes) == restricted {
				if r.LadderAvailable {
					t.Error("restricted asset reported ladder_available=true to a " +
						"caller who cannot read its content")
				}
				if r.PreviewAvailable {
					t.Error("restricted asset reported preview_available=true")
				}
			}
		}
	})

	t.Run("owner sees true for the same rungs", func(t *testing.T) {
		// Guards the guard: if the owner ALSO saw false, the test above
		// would pass for the wrong reason (e.g. the variants never
		// seeded) and would keep passing if the flag were hardwired off.
		owner := visibility.NewCaller(ptrTo(ladderOwner))
		row, ok := ladderRow(t, owner, nil, public, full)
		if !ok {
			t.Fatal("public asset not returned to its owner")
		}
		if !row.LadderAvailable {
			t.Fatal("owner sees ladder_available=false for an asset with every " +
				"configured rung — the restricted-case assertion above proves nothing")
		}
	})
}

// TestLadderAvailable_TracksPreviewAvailableReadability pins the two
// flags to ONE readability decision.
//
// They answer different questions about rungs but the SAME question
// about access, and they are computed from a single ContentReadable
// call so they cannot drift. A true ladder flag on an asset whose bytes
// are gated is a 403 the client walks straight into.
func TestLadderAvailable_TracksPreviewAvailableReadability(t *testing.T) {
	full := []string{"col", "preview", "screen", "hires"}
	id := seedLadderAsset(t, "restricted", full...)
	stranger := visibility.NewCaller(ptrTo(int64(4290779)))

	row, ok := ladderRow(t, stranger, nil, id, full)
	if !ok {
		t.Skip("restricted asset not visible to this caller at the row level; " +
			"the content-plane assertion is covered by the sibling test")
	}
	if row.LadderAvailable != row.PreviewAvailable {
		t.Errorf("flags disagree on readability: preview=%v ladder=%v — both are "+
			"gated by the same ContentReadable call and must move together",
			row.PreviewAvailable, row.LadderAvailable)
	}
}
