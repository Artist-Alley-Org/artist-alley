// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1164 — suggest and run must agree about which collections a caller
// can find.
//
// The defect: `suggest.collections` composed
// [visibility.CollectionReadableSQL] (#1078, admin arm ⇒ empty
// fragment) while `Engine.runCollections` composed
// `Filter(EntityCollection)`, which has no admin disjunct at all. So a
// system.admin was OFFERED the name of a private collection and then
// handed a result page that did not contain it — the completion led
// nowhere. The owner ratified widening the run path, so both now
// compose the one authority.
//
// # Why this suite is a PAIR test and not an equality test
//
// Asserting only "suggest and run agree" is satisfiable by two
// consistently WRONG rules — the failure mode the house rule about
// *_MatchesGo twins names explicitly. Both paths narrowed back to the
// row plane would agree perfectly and re-break #1078; both widened to
// "everyone sees everything" would agree perfectly and be a leak.
//
// So every case here carries a GROUND TRUTH beside the agreement:
//
//   - admin, private   findable in BOTH  (agreement, and the direction)
//   - stranger, private  findable in NEITHER  (the leak control)
//   - anyone, public   findable in BOTH  (the ordinary path still works)
//   - admin, trashed   findable in NEITHER  (the corpus constraint)
//
// The stranger row is what makes the admin row mean something: same
// collection, same two call shapes, opposite verdicts.
//
// The trashed row is the hazard the admin arm creates on BOTH sides.
// An empty fragment means the predicate's `deleted_at IS NULL` never
// runs, so each path needs its own corpus constraint — suggest has had
// one since #1078, and runCollections grew one with this change.
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
	csrOwner    int64 = 11641101
	csrStranger int64 = 11641102
	csrAdmin    int64 = 11641103
)

// One nonsense token, so a hit is attributable to this fixture and to
// nothing else in a shared database. It has to work as a trigram prefix
// (suggest) AND as a tsquery lexeme (run), which is why it is a word
// rather than a punctuation soup.
const csrToken = "quibbleflax"

const (
	csrPrivateName = csrToken + " sealed archive"
	csrPublicName  = csrToken + " open shelf"
	csrTrashedName = csrToken + " discarded set"
)

func csrSeed(t *testing.T, pool *pgxpool.Pool, name, vis string, deleted bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	del := "NULL"
	if deleted {
		del = "NOW()"
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO collections (id, name, description, owner_user_ref, visibility, membership, deleted_at)
		VALUES ($1,$2,'',$3,$4,'manual',`+del+`)`, id, name, csrOwner, vis); err != nil {
		t.Fatalf("seed collection %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, id)
	})
	return id
}

func csrAdminCaps(code string) bool { return code == visibility.SystemAdmin }

// csrSuggested is the completion side: the real suggest service.
func csrSuggested(
	t *testing.T, pool *pgxpool.Pool, ref *int64, caps visibility.CapabilityChecker,
) map[string]bool {
	t.Helper()
	resp, err := suggest.NewService(pool).Suggest(context.Background(), suggest.Request{
		Prefix:         csrToken,
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

// csrReturned is the results side: the real Engine, collections only.
//
// `Caps` rather than `CapChecker`, matching the engine: the resolved
// ContentCaps is what `keyForQuery` folds into the result cache key, so
// it is the only capability input a widening may key on.
func csrReturned(
	t *testing.T, pool *pgxpool.Pool, ref *int64, admin bool,
) map[string]bool {
	t.Helper()
	res, err := (&Engine{Pool: pool}).Run(context.Background(), Query{
		Text:          csrToken,
		Types:         []HitType{HitTypeCollection},
		CallerUserRef: ref,
		Caps:          visibility.ContentCaps{SystemAdmin: admin},
		Limit:         50,
	})
	if err != nil {
		t.Fatalf("engine run: %v", err)
	}
	out := map[string]bool{}
	for _, h := range res.Hits {
		if h.Type == HitTypeCollection {
			out[h.Title] = true
		}
	}
	return out
}

func TestCollections_SuggestAndRunAgree(t *testing.T) {
	pool := coPool(t)
	csrSeed(t, pool, csrPrivateName, "private", false)
	csrSeed(t, pool, csrPublicName, "public", false)

	stranger := csrStranger
	admin := csrAdmin

	cases := []struct {
		caller      string
		ref         *int64
		admin       bool
		caps        visibility.CapabilityChecker
		wantPrivate bool
	}{
		// The direction the owner ratified: an admin can already open
		// any collection from its own page, so a search that returns
		// one grants no new reach — and the completion now leads
		// somewhere.
		{"system.admin", &admin, true, csrAdminCaps, true},
		// The control that makes the row above mean something. A
		// shared-wrong rule — both paths widened for everybody — passes
		// the agreement assertion and fails here.
		{"stranger", &stranger, false, func(string) bool { return false }, false},
		{"anonymous", nil, false, nil, false},
	}

	for _, c := range cases {
		t.Run(c.caller, func(t *testing.T) {
			suggested := csrSuggested(t, pool, c.ref, c.caps)
			returned := csrReturned(t, pool, c.ref, c.admin)

			// 1. Agreement, on the private collection.
			if suggested[csrPrivateName] != returned[csrPrivateName] {
				t.Errorf("suggest and run disagree about a PRIVATE collection for %s: "+
					"completed=%v returned=%v (#1164). One authority governs both paths — "+
					"visibility.CollectionReadableSQL — so this means one of them stopped "+
					"composing it.\n  suggested=%v\n  returned=%v",
					c.caller, suggested[csrPrivateName], returned[csrPrivateName],
					suggested, returned)
			}

			// 2. Ground truth, so agreement cannot be reached by two
			//    consistently wrong rules.
			if suggested[csrPrivateName] != c.wantPrivate {
				t.Errorf("%s: suggest offered the private collection = %v, want %v",
					c.caller, suggested[csrPrivateName], c.wantPrivate)
			}
			if returned[csrPrivateName] != c.wantPrivate {
				if c.wantPrivate {
					t.Errorf("%s: /search did not return the private collection it completed — "+
						"this is #1164 itself", c.caller)
				} else {
					t.Errorf("%s: /search returned a PRIVATE collection to a caller with no "+
						"claim on it — the widening was applied to the wrong arm", c.caller)
				}
			}

			// 3. The ordinary path is untouched: the public collection
			//    is findable both ways by everyone, so neither fix
			//    passed by breaking the source.
			if !suggested[csrPublicName] || !returned[csrPublicName] {
				t.Errorf("%s lost the PUBLIC collection: completed=%v returned=%v",
					c.caller, suggested[csrPublicName], returned[csrPublicName])
			}
		})
	}
}

// The corpus hazard, on the side that just grew it. runCollections'
// `deleted_at IS NULL` is the only expression of the rule on the admin
// arm, because CollectionReadableSQL returns an empty fragment there and
// GetCollection's Restore branch depends on it doing so. Delete that
// conjunct from runCollections and this test is what goes red.
func TestCollections_AdminSearchDoesNotReturnTheTrash(t *testing.T) {
	pool := coPool(t)
	csrSeed(t, pool, csrTrashedName, "public", true)
	csrSeed(t, pool, csrPublicName, "public", false)

	admin := csrAdmin
	suggested := csrSuggested(t, pool, &admin, csrAdminCaps)
	returned := csrReturned(t, pool, &admin, true)

	if returned[csrTrashedName] {
		t.Errorf("/search returned a SOFT-DELETED collection to a system.admin. The admin arm of " +
			"CollectionReadableSQL is an empty fragment, so the predicate's `deleted_at IS NULL` " +
			"never runs — runCollections' own corpus constraint is what stops this (#1164).")
	}
	if suggested[csrTrashedName] {
		t.Errorf("suggest completed a SOFT-DELETED collection's name for a system.admin (#1078's " +
			"corpus constraint).")
	}
	// The live control: both surfaces are actually answering, so the
	// two assertions above did not pass on an empty result.
	if !returned[csrPublicName] || !suggested[csrPublicName] {
		t.Errorf("neither surface produced the live public collection (returned=%v suggested=%v); "+
			"the trash assertions above proved nothing",
			returned[csrPublicName], suggested[csrPublicName])
	}
}
