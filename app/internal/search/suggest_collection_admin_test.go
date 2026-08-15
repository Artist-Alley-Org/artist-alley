// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1078 — the collections suggest source had no capability
// short-circuit, so a system.admin got no completions for private
// collection names they can open perfectly well from the collection
// page.
//
// It failed CLOSED, so this is not a leak suite — nobody was
// over-served. It is the mirror of #1064's asset-title gap and of
// #1059's split verdict: one rule, two surfaces, opposite answers. The
// symptom is a feature that looks broken, which is the exact wording
// visibility.CanReadCollection's doc uses.
//
// The assertions are COMPARATIVE in the shape #902's suite established,
// because a change that completed the private name for EVERYBODY would
// satisfy a "the admin sees it" test and ship a leak:
//
//   - the admin completes the private collection AND the public one;
//   - the stranger completes the public one and NOT the private one;
//   - same collection, same call shape, opposite verdicts.
//
// Plus the tombstone case, which is the trap the short-circuit sets: an
// empty fragment for an admin means the predicate's `deleted_at IS
// NULL` never runs, so without the corpus constraint in
// suggest.collections an admin's dropdown would start completing the
// trash.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/suggest"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	scaOwner    int64 = 10781101
	scaStranger int64 = 10781102
	scaAdmin    int64 = 10781103
)

// scaPrefix is nonsense on purpose: a completion has to be attributable
// to this fixture and to nothing else in a shared database.
const scaPrefix = "zurbliphant"

const (
	scaPrivateName = scaPrefix + " private vault"
	scaPublicName  = scaPrefix + " public shelf"
	scaTrashedName = scaPrefix + " trashed set"
)

// scaSeedCollection plants one collection and returns its id.
// `deleted` tombstones it.
func scaSeedCollection(t *testing.T, pool *pgxpool.Pool, name, vis string, deleted bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	del := "NULL"
	if deleted {
		del = "NOW()"
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO collections (id, name, description, owner_user_ref, visibility, membership, deleted_at)
		VALUES ($1,$2,'',$3,$4,'manual',`+del+`)`, id, name, scaOwner, vis); err != nil {
		t.Fatalf("seed collection %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, id)
	})
	return id
}

// scaAdminCaps is the checker a system.admin's identity produces.
func scaAdminCaps(code string) bool { return code == visibility.SystemAdmin }

// scaCollections runs the real endpoint service and returns the
// COLLECTION completions as a set.
func scaCollections(
	t *testing.T, pool *pgxpool.Pool, ref *int64, caps visibility.CapabilityChecker,
) map[string]bool {
	t.Helper()
	resp, err := suggest.NewService(pool).Suggest(context.Background(), suggest.Request{
		Prefix:         scaPrefix,
		Caller:         visibility.NewCaller(ref),
		CollectionCaps: caps,
		Limit:          suggest.MaxResults,
	})
	if err != nil {
		t.Fatalf("suggest: %v", err)
	}
	out := map[string]bool{}
	for _, s := range resp.Suggestions {
		if s.Kind == suggest.KindCollection {
			out[s.Value] = true
		}
	}
	return out
}

// TestSuggestCollections_AdminCompletesPrivate is #1078's acceptance:
// same collection, opposite verdicts.
func TestSuggestCollections_AdminCompletesPrivate(t *testing.T) {
	pool := coPool(t)
	scaSeedCollection(t, pool, scaPrivateName, "private", false)
	scaSeedCollection(t, pool, scaPublicName, "public", false)

	stranger := scaStranger
	admin := scaAdmin

	cases := []struct {
		name        string
		ref         *int64
		caps        visibility.CapabilityChecker
		wantPrivate bool
		wantPublic  bool
	}{
		{"anonymous", nil, nil, false, true},
		{"stranger", &stranger, func(string) bool { return false }, false, true},
		{"system.admin", &admin, scaAdminCaps, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scaCollections(t, pool, c.ref, c.caps)
			if got[scaPrivateName] != c.wantPrivate {
				if c.wantPrivate {
					t.Errorf("a system.admin got no completion for a private collection they can "+
						"open from its own page (#1078). Suggestions: %v", got)
				} else {
					t.Errorf("%s completed a PRIVATE collection's name — the short-circuit widened "+
						"the rule for everyone instead of for the admin arm", c.name)
				}
			}
			if got[scaPublicName] != c.wantPublic {
				t.Errorf("%s: public collection completion = %v, want %v — the fix must not have "+
					"narrowed the ordinary path", c.name, got[scaPublicName], c.wantPublic)
			}
		})
	}
}

// TestSuggestCollections_AdminDoesNotCompleteTheTrash is the hazard the
// admin short-circuit introduces and the reason suggest.collections
// carries its own `deleted_at IS NULL`.
//
// CollectionReadableSQL returns an EMPTY fragment for a system.admin —
// faithfully, because CanReadCollection's admin arm deliberately says
// nothing about tombstones (GetCollection's Restore branch depends on
// that). The predicate's soft-delete conjunct therefore never runs for
// them, and a corpus that relied on it would start completing deleted
// names. Delete that line from suggest.collections and this test is the
// one that goes red.
func TestSuggestCollections_AdminDoesNotCompleteTheTrash(t *testing.T) {
	pool := coPool(t)
	scaSeedCollection(t, pool, scaTrashedName, "public", true)

	admin := scaAdmin
	got := scaCollections(t, pool, &admin, scaAdminCaps)
	if got[scaTrashedName] {
		t.Errorf("a soft-deleted collection's name completed for a system.admin. The admin arm of "+
			"CollectionReadableSQL is an EMPTY fragment, so the predicate's `deleted_at IS NULL` "+
			"never runs — the corpus constraint in suggest.collections is what stops this, and it "+
			"is gone or broken. Suggestions: %v", got)
	}

	// The negative control: an ordinary caller never saw it either, so
	// a fix that hid it from everyone by breaking the source entirely
	// is not what passed above.
	if live := scaCollections(t, pool, &admin, scaAdminCaps); len(live) == 0 {
		scaSeedCollection(t, pool, scaPublicName, "public", false)
		if again := scaCollections(t, pool, &admin, scaAdminCaps); !again[scaPublicName] {
			t.Error("the collections source returned nothing at all for an admin; the assertion " +
				"above passed because the source is broken, not because the trash is excluded")
		}
	}
}
