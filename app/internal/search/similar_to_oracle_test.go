// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1066 — THE ATTACK on the vector channel, written as the attack.
//
// #899 removed a restricted asset's title, description and thumbhash
// from its payload. #902 stopped its indexed TEXT answering queries. The
// picture had no such gate: `similar_to:<uuid>` and `POST
// /search/by-image` ranked EVERY asset for any authenticated caller,
// because the only filter on the kNN was the ROW predicate, which for a
// signed-in caller is soft-delete and nothing more.
//
// An embedding is a DERIVED COPY of the image. ADR 0064 withholds the
// thumbhash precisely because "a thumbhash IS a blur" — a low-fidelity
// copy of the picture, so handing it to someone refused the original
// hands them the original at lower resolution. A 768-dimension embedding
// is lossier, and the similarity SCORE reads it out a little at a time:
// anchor on a picture you have, watch a restricted asset come back at
// 0.99, and you have learned what an asset you may not open looks like
// without ever being shown it.
//
// So the assertions here are on the RESULT SET, never on a field. The
// fields of a restricted hit are withheld either way (#899), so a field
// assertion passes on the bug. What must move is whether the row RANKS.
//
// And they are COMPARATIVE: the same anchor, the same fixture, opposite
// verdicts for the stranger and for the four entitled callers. A change
// that dropped restricted assets from everybody's similarity results
// would satisfy half of this file and break the product — an artist who
// cannot find work like their own restricted work has no product.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	simtoOwner    int64 = 10661101
	simtoStranger int64 = 10661102
	simtoAdmin    int64 = 10661103
)

// The (provider, model, modality) tuple vector.Query filters candidates
// by. The anchor's tuple wins, and it must match what the fixture wrote
// or the kNN silently compares nothing.
const (
	simtoProvider = "router"
	simtoModel    = "nomic-embed-text"
	simtoModality = "text"
	simtoDim      = 768
)

// simtoVector builds a 768-d vector one index away from every other, so
// every seeded asset is a close neighbour of every other and the whole
// fixture comes back inside the default top-K. A fixture where the
// restricted row fell off the end for DISTANCE reasons would pass this
// file vacuously.
func simtoVector(i int) string {
	parts := make([]string, simtoDim)
	for d := range parts {
		parts[d] = "0.01"
	}
	parts[i%simtoDim] = "1"
	return "[" + strings.Join(parts, ",") + "]"
}

type simtoFixture struct {
	anchor     uuid.UUID // public, and the caller can read it: the query
	restricted uuid.UUID // the asset whose picture the stranger may not read
	public     uuid.UUID // the non-vacuity control
	team       uuid.UUID // team-tier, for the membership verdict
	teamID     uuid.UUID
	anchorRaw  string // the anchor's stored embedding, as vector.Query wants it
}

// simtoSeed plants four assets owned by simtoOwner plus their
// embeddings. Seeding asset_embedding_d768 is the step that makes this
// reproducible at all: the table is empty on a dev box, so every
// similarity query short-circuits and "no leak" would just mean "no
// data" — the reasoning epic #665 exists to reject.
func simtoSeed(t *testing.T, pool *pgxpool.Pool) simtoFixture {
	t.Helper()
	ctx := context.Background()

	for _, ref := range []int64{simtoOwner, simtoStranger, simtoAdmin} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO "user" (ref, username) VALUES ($1, $2)
			 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
			ref, "simto-"+uuid.NewString()[:8]); err != nil {
			t.Fatalf("seed user %d: %v", ref, err)
		}
	}

	teamID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO teams (id, name, slug) VALUES ($1, $2, $3)`,
		teamID, "simto-team-"+teamID.String()[:8], "simto-"+teamID.String()[:8]); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	f := simtoFixture{
		anchor:     uuid.New(),
		restricted: uuid.New(),
		public:     uuid.New(),
		team:       uuid.New(),
		teamID:     teamID,
	}
	seeds := []struct {
		id          uuid.UUID
		title       string
		sensitivity string
		team        *uuid.UUID
	}{
		{f.anchor, "simto anchor", "public", nil},
		{f.restricted, "simto restricted", "restricted", nil},
		{f.public, "simto public", "public", nil},
		{f.team, "simto team", "team", &teamID},
	}
	ids := make([]uuid.UUID, 0, len(seeds))
	for i, s := range seeds {
		ids = append(ids, s.id)
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status,
			                    sensitivity, processing_status, team_id, file_extension)
			VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active',$4,'ready',$5,'png')`,
			s.id, s.title, simtoOwner, s.sensitivity, s.team); err != nil {
			t.Fatalf("seed asset %s: %v", s.title, err)
		}
		raw := simtoVector(i)
		if _, err := pool.Exec(ctx, `
			INSERT INTO asset_embedding_d768 (asset_id, provider, model, modality, embedding, updated_at)
			VALUES ($1,$2,$3,$4,$5::vector,NOW())
			ON CONFLICT (asset_id, provider, model, modality) DO UPDATE
				SET embedding = EXCLUDED.embedding`,
			s.id, simtoProvider, simtoModel, simtoModality, raw); err != nil {
			t.Fatalf("seed embedding %s: %v", s.title, err)
		}
		if s.id == f.anchor {
			f.anchorRaw = raw
		}
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM asset_embedding_d768 WHERE asset_id = ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = ANY($1::uuid[])`, ids)
		_, _ = pool.Exec(c, `DELETE FROM team_memberships WHERE team_id = $1`, teamID)
		_, _ = pool.Exec(c, `DELETE FROM teams WHERE id = $1`, teamID)
	})
	return f
}

// simtoRanked runs a PURE-VECTOR search (empty text, similarity hint
// set) — the shape `similar_to:<uuid>` compiles to when it is the whole
// query — and returns the set of asset ids that came back RANKED.
//
// It goes through Engine.Run rather than calling vector.Query, so the
// merge, the enrich pass and the threshold all sit between the gate and
// the assertion, exactly as they do on /search.
func simtoRanked(
	t *testing.T,
	pool *pgxpool.Pool,
	f simtoFixture,
	ref *int64,
	caps visibility.ContentCaps,
	mut visibility.AssetMutationCaps,
) map[uuid.UUID]bool {
	t.Helper()
	res, err := NewEngine(pool).Run(context.Background(), Query{
		Types:                  []HitType{HitTypeAsset},
		Limit:                  50,
		CallerUserRef:          ref,
		Caps:                   caps,
		MutationCaps:           mut,
		SimilarityHint:         f.anchorRaw,
		SimilarityHintProvider: simtoProvider,
		SimilarityHintModel:    simtoModel,
		SimilarityHintModality: simtoModality,
		SimilarityHintID:       "asset:" + f.anchor.String(),
	})
	if err != nil {
		t.Fatalf("similarity search: %v", err)
	}
	out := make(map[uuid.UUID]bool, len(res.Hits))
	for _, h := range res.Hits {
		out[h.ID] = true
	}
	return out
}

// TestSimilarTo_RestrictedDoesNotRankForAStranger is the exploit.
//
// The anchor is a PUBLIC asset the stranger may read; the target is a
// restricted asset one coordinate away from it. Before #1066 the target
// came back with a similarity score, which is the whole disclosure: an
// asset whose picture, bytes and blur are all refused told the caller it
// closely resembles a picture they chose.
func TestSimilarTo_RestrictedDoesNotRankForAStranger(t *testing.T) {
	pool := coPool(t)
	f := simtoSeed(t, pool)
	stranger := simtoStranger

	got := simtoRanked(t, pool, f, &stranger, visibility.ContentCaps{}, visibility.AssetMutationCaps{})

	// Non-vacuity FIRST. If the public neighbour is missing, the query
	// found nothing and every absence below is meaningless.
	if !got[f.public] {
		t.Fatal("the PUBLIC neighbour did not rank — the fixture's embeddings did not take, or the " +
			"gate over-narrowed, and the assertions below would pass vacuously")
	}
	if got[f.restricted] {
		t.Error("a RESTRICTED asset ranked in an authenticated stranger's similarity search: they " +
			"have learned that an asset they may not open closely resembles the picture they " +
			"anchored on, which is the image disclosed through its derived copy (#1066)")
	}
	if got[f.team] {
		t.Error("a TEAM-tier asset ranked for a non-member — same disclosure, other tier")
	}
}

// TestSimilarTo_EntitledCallersStillRank is the other verdict on the
// SAME fixture and the SAME anchor. Without it, "drop every restricted
// asset from every similarity result" passes the test above.
func TestSimilarTo_EntitledCallersStillRank(t *testing.T) {
	pool := coPool(t)
	f := simtoSeed(t, pool)

	// The `assets.admin` holder is deliberately NOT in this list: they
	// are entitled to the FIELDS and never to the binary plane, which is
	// its own test below.
	owner, stranger := simtoOwner, simtoStranger
	for _, c := range []struct {
		name string
		ref  *int64
		caps visibility.ContentCaps
		mut  visibility.AssetMutationCaps
	}{
		{"the owner", &owner, visibility.ContentCaps{}, visibility.AssetMutationCaps{}},
		{"content.read.all", &stranger, visibility.ContentCaps{ContentReadAll: true}, visibility.AssetMutationCaps{}},
		{"system.admin", &stranger, visibility.ContentCaps{SystemAdmin: true}, visibility.AssetMutationCaps{}},
	} {
		got := simtoRanked(t, pool, f, c.ref, c.caps, c.mut)
		if !got[f.public] {
			t.Errorf("%s: the public neighbour did not rank — vacuous", c.name)
		}
		if !got[f.restricted] {
			t.Errorf("%s: the restricted asset did not rank. The gate is too wide: a caller entitled "+
				"to an asset's picture must still find work like it", c.name)
		}
	}

	// A member of the team-tier asset's team is the fourth entitled
	// caller, and the tier is part of the rule so it needs its own step.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO team_memberships (team_id, user_ref) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, f.teamID, simtoStranger); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	got := simtoRanked(t, pool, f, &stranger, visibility.ContentCaps{}, visibility.AssetMutationCaps{})
	if !got[f.team] {
		t.Error("a member of the team-tier asset's team could not rank it")
	}
	// ...and the restricted asset is STILL absent for that same caller,
	// so the membership grant widened exactly one tier.
	if got[f.restricted] {
		t.Error("joining a team admitted an unrelated RESTRICTED asset to the ranking")
	}
}

// TestSimilarTo_MutationHolderIsNotEntitled pins the plane choice, which
// is the whole of #1066 rather than an implementation detail.
//
// #902 gated the TEXT channel at the FIELD plane, because a title is a
// field, and a team-scoped `assets.admin` holder is owed the fields of
// the assets they administer (ADR 0064 / #939). An embedding derives
// from the IMAGE, so this channel is gated at the CONTENT plane instead —
// and ADR 0064 is explicit that a mutation capability "never confers the
// binary plane".
//
// The two therefore diverge for exactly this caller: they keep matching
// a restricted asset's title and stop ranking its picture. If this test
// ever goes red because the gate was "simplified" to FieldsReadableSQL
// for symmetry with #902, that is a mutation capability quietly becoming
// a content-tier grant.
func TestSimilarTo_MutationHolderIsNotEntitled(t *testing.T) {
	pool := coPool(t)
	f := simtoSeed(t, pool)
	admin := simtoAdmin

	for _, c := range []struct {
		name string
		mut  visibility.AssetMutationCaps
	}{
		{"global assets.admin", visibility.AssetMutationCaps{Global: true}},
		{"assets.admin scoped to the asset's team", visibility.AssetMutationCaps{Teams: []uuid.UUID{f.teamID}}},
	} {
		got := simtoRanked(t, pool, f, &admin, visibility.ContentCaps{}, c.mut)
		if !got[f.public] {
			t.Errorf("%s: the public neighbour did not rank — vacuous", c.name)
		}
		if got[f.restricted] {
			t.Errorf("%s ranked a restricted asset's embedding. ADR 0064: a capability that permits "+
				"mutation confers the FIELD plane and never the binary plane, and an embedding is "+
				"a derived copy of the image", c.name)
		}
	}
}

// TestSimilarTo_AnonymousUnchanged is acceptance 4. The anonymous branch
// of the row predicate already floored this path to public/active/ready,
// and the content conjunct must not move it in either direction.
func TestSimilarTo_AnonymousUnchanged(t *testing.T) {
	pool := coPool(t)
	f := simtoSeed(t, pool)

	got := simtoRanked(t, pool, f, nil, visibility.ContentCaps{}, visibility.AssetMutationCaps{})
	if !got[f.public] {
		t.Error("anonymous lost the PUBLIC neighbour — the #1066 conjunct narrowed a path it must not")
	}
	for name, id := range map[string]uuid.UUID{"restricted": f.restricted, "team": f.team} {
		if got[id] {
			t.Errorf("anonymous ranked the %s asset", name)
		}
	}
}
