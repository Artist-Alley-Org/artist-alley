// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The seed phase that owns what the dogfood suite kept re-creating
// (#1270).
//
// THE PROBLEM IT SOLVES
// ---------------------
// Five specs needed rows that no teardown could remove:
//
//   - four needed a principal that is not the bootstrap admin, and
//     there is NO USER-DELETE ENDPOINT — archiving does not take a row
//     off /admin/users either;
//   - marquee-select-1177 needed the signed-in admin to OWN at least
//     four assets and posts, because the profile uploads grid is
//     `isSelf` only (#1106) and every seeded asset belongs to one of the
//     fictional artists. `DELETE` on an asset or a post is a SOFT
//     delete, so even a spec that tidied up left the row behind.
//
// So on a FRESH database a suite run drifted the corpus permanently, the
// suite-level census (#1245) reported it, and #1263's plan to run that
// census in CI — where every database is fresh — would have reddened
// every run. On the long-lived coding stack the same cost is invisible,
// because it was paid months ago.
//
// The substrate is seeded here instead. A fresh database starts with it,
// the specs CONSUME it, and a run adds nothing.
//
// WHY IT IS OPT-IN
// ----------------
// These principals can log in and their passwords are committed in
// seed/profiles/dataset.fixtures.json. The public demo runs `aa seed`
// without --fixtures and therefore never creates them.
//
// ⛔ WHY IT IS NOT SUBJECT TO THE COVERAGE PROFILE
// ------------------------------------------------
// `--profile ci` shrinks the CATALOGUE — a greedy set-cover over posts.
// These rows are not catalogue entries and are written from this phase
// directly, so the shrink cannot drop them. CI is exactly where they
// have to survive.
//
// ⛔ WHY THE ROWS SURVIVE `aa sweep-fixtures`
// -------------------------------------------
// Every rule in fixturesweep.Rules had to be read before a row was
// written here, because the sweep is destructive and a seeded row it
// classifies as litter is deleted out from under the seed:
//
//	assets       fixture iff NOT (metadata ? 'acquisition_source').
//	             So each asset below carries the stamp, same as every
//	             other seeded asset.
//	"user"       fixture iff username ~ '^(acl|share|vocab|sprint|ui)[0-9]+'
//	             or '^go_[a-z]+_test_user$'. The principals are named
//	             like the seed's other artists — see the catalogue's
//	             own note on why the names do not say which spec owns
//	             them.
//	posts        PROTECTED iff author_user_ref <> 1 OR created_at <
//	             '2026-08-17'. These posts are the admin's, so they are
//	             protected by their DATE — and their titles must not
//	             match the Fixture predicate either, because a row
//	             satisfying BOTH aborts the entire sweep rather than
//	             merely losing these rows.
package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// fixtureAcquisitionSource is the provenance stamp on every asset this
// phase writes.
//
// ⛔ ADR 0095: `metadata.acquisition_source` is the ONLY thing that
// tells the fixture sweep a seeded asset from an uploaded one. An asset
// written here without it is deleted by the next `aa sweep-fixtures`,
// and the census then drifts in the opposite direction — the suite
// finds the substrate missing and re-creates it, which is the bug this
// phase exists to end.
const fixtureAcquisitionSource = "Seeded test substrate (aa seed --fixtures)"

// fixtureDefaultRole is the role the registration endpoint would have
// given these accounts. See SeedFindRoleByName's comment.
const fixtureDefaultRole = "Base"

// applyTestFixtures writes the dogfood suite's one-time substrate.
// No-op unless Options.Fixtures is set.
func (r *Runner) applyTestFixtures(ctx context.Context, cat *catalogues) error {
	if !r.opts.Fixtures {
		return nil
	}
	if cat.Fixtures == nil {
		return fmt.Errorf(
			"--fixtures was requested but %s/dataset.fixtures.json is not there. "+
				"That file IS the fixture list — the credentials it holds are read "+
				"by scripts/dogfood/ui/helpers/seeded-principal.ts as well, so "+
				"inventing defaults here would give the suite an account it cannot "+
				"sign in as", r.opts.CatalogueRoot)
	}
	if err := r.applyFixturePrincipals(ctx, cat.Fixtures.Principals); err != nil {
		return err
	}
	return r.applyFixtureAdminUploads(ctx, cat.Fixtures.Admin)
}

// applyFixturePrincipals creates the accounts four specs sign in as.
//
// Idempotent on username: CreateUser returns the existing row with
// AlreadyExisted=true, and the role assignment is a DELETE-then-INSERT,
// so a re-seed onto a live database converges rather than duplicating.
func (r *Runner) applyFixturePrincipals(ctx context.Context, ps []catFixturePrincipal) error {
	// ⛔ SERIALIZED, for the same reason applyTeams is: the loop below
	// calls SeedSetUserGlobalRole, and a role assignment is an authority
	// mutation — more consequential than a direct grant, because a
	// team-scoped ROLE produces zero rows in `user_capability_grants`
	// and is invisible to anything watching that table.
	//
	// ⭐ The phase is the semantic unit: these principals are seeded as a
	// SET, and a reader resolving authority midway through would see a
	// fixture world that is neither the old one nor the new one.
	release, err := auth.AcquireStructuralAuthorityLock(ctx, r.pool, r.log)
	if err != nil {
		return err
	}
	defer release()

	if len(ps) == 0 {
		return nil
	}
	roleID, err := r.q.SeedFindRoleByName(ctx, fixtureDefaultRole)
	if err != nil {
		// Loud, not soft. The registration path warns and continues
		// because a human can fix a role afterwards; nobody is watching
		// a seed, and a principal with no role signs in and is then
		// refused every write its spec drives — which reads as a
		// permission regression and is a missing fixture.
		return fmt.Errorf("resolve %q role for seeded principals: %w",
			fixtureDefaultRole, err)
	}
	created, reused := 0, 0
	for _, p := range ps {
		if p.Username == "" || p.Password == "" {
			return fmt.Errorf("fixture principal %q: username and password are both "+
				"required — an account the suite cannot sign in as is not a fixture",
				p.Username)
		}
		in := UserInput{
			Username: p.Username,
			Password: &p.Password,
			Approved: true,
		}
		if p.FullName != "" {
			in.Fullname = &p.FullName
		}
		if p.Email != "" {
			in.Email = &p.Email
		}
		res, err := r.admin.CreateUser(ctx, nil, r.adminRef, in)
		if err != nil {
			return fmt.Errorf("create fixture principal %s: %w", p.Username, err)
		}
		if err := r.q.SeedSetUserGlobalRole(ctx, SeedSetUserGlobalRoleParams{
			UserRef:           res.Ref,
			RoleID:            roleID,
			AssignedByUserRef: &r.adminRef,
		}); err != nil {
			return fmt.Errorf("assign %s role to %s: %w",
				fixtureDefaultRole, p.Username, err)
		}
		// ⛔ NO TEAM MEMBERSHIP, deliberately. The registered accounts
		// these replace had none, and post-acl-share-667 asserts that a
		// grantee CANNOT read an unshared post — putting a principal on
		// a team is a permission change dressed as tidiness.
		r.users[p.Username] = res.Ref
		if res.AlreadyExisted {
			reused++
		} else {
			created++
		}
	}
	r.log.Info("seed.fixtures.principals", "created", created, "reused", reused)
	return nil
}

// applyFixtureAdminUploads gives the bootstrap admin assets and posts of
// its own, for the marquee sweep (#1177).
//
// The bytes are SYNTHESISED rather than read from the dataset. A fixture
// plate is not dataset content: adding four files to a published,
// shared, Kaggle-mirrored corpus so a selection spec has four tiles
// would put test scaffolding in front of every person who downloads it.
// Text is what the spec wants anyway — a .txt occupies a tile with no
// rendered variant, so the sweep is not racing a preview worker.
func (r *Runner) applyFixtureAdminUploads(ctx context.Context, spec catFixtureAdmin) error {
	if spec.Count <= 0 {
		return nil
	}
	typeRef, ok := r.assetTypes["document"]
	if !ok {
		typeRef = 1
	}
	created, updated := r.rowTimes(spec.CreatedAt, spec.CreatedAt)
	titlePrefix := orDefault(spec.TitlePrefix, "Studio admin plate")
	postPrefix := orDefault(spec.PostTitlePrefix, titlePrefix)

	metadata, err := json.Marshal(map[string]any{
		"acquisition_source": fixtureAcquisitionSource,
		"filename":           "admin-plate.txt",
		"license":            "CC0 1.0",
		"attribution":        "Artist Alley seed",
	})
	if err != nil {
		return fmt.Errorf("fixture metadata: %w", err)
	}

	assets, posts := 0, 0
	for i := 1; i <= spec.Count; i++ {
		assetID := stableUUID("fixture", "admin-upload", "asset", fmt.Sprint(i))
		postID := stableUUID("fixture", "admin-upload", "post", fmt.Sprint(i))

		// ⚠️ DISTINCT BYTES PER PLATE. The (owner_user_ref, file_hash)
		// unique index COLLAPSES byte-identical uploads by one owner —
		// correctly, it mirrors the API refusing a re-upload — so four
		// identical plates would become one asset and the grid would
		// hold one tile.
		body := fmt.Sprintf(
			"%s %d\n\nSeeded by `aa seed --fixtures` so the dogfood suite does not "+
				"have to create it (#1270). Safe to ignore.\n", titlePrefix, i)
		up, err := r.storage.UploadOriginal(ctx, bytes.NewReader([]byte(body)), "text/plain",
			storage.PinRef{SubjectType: "asset", SubjectID: assetID.String()})
		if err != nil {
			return fmt.Errorf("upload fixture plate %d: %w", i, err)
		}
		hash, size, ext := up.Hash, up.Size, "txt"

		id, err := r.q.SeedInsertAsset(ctx, SeedInsertAssetParams{
			ID:            pgtype.UUID{Bytes: assetID, Valid: true},
			Title:         fmt.Sprintf("%s %d", titlePrefix, i),
			Description:   spec.Why,
			AssetType:     typeRef,
			OwnerUserRef:  &r.adminRef,
			Status:        assetStatus("active"),
			FileHash:      &hash,
			FileExtension: &ext,
			FileSizeBytes: &size,
			Metadata:      metadata,
			StateID:       r.resolveAssetState("approved"),
			Sensitivity:   sensitivity("team"),
			CreatedAt:     created,
			UpdatedAt:     updated,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Already present from an earlier seed — which is the whole
			// point. Recover the id so the post below still lands.
			id = pgtype.UUID{Bytes: assetID, Valid: true}
		} else if err != nil {
			return fmt.Errorf("insert fixture plate %d: %w", i, err)
		} else {
			assets++
		}

		postRowID, err := r.q.SeedInsertPost(ctx, SeedInsertPostParams{
			ID:            pgtype.UUID{Bytes: postID, Valid: true},
			AuthorUserRef: r.adminRef,
			Title:         fmt.Sprintf("%s %d", postPrefix, i),
			Description:   spec.Why,
			Visibility:    "org-only",
			CoverAssetID:  id,
			StateID:       r.postStates["published"],
			CreatedAt:     created,
			UpdatedAt:     updated,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			postRowID = pgtype.UUID{Bytes: postID, Valid: true}
		} else if err != nil {
			return fmt.Errorf("insert fixture post %d: %w", i, err)
		} else {
			posts++
		}
		if err := r.q.SeedInsertPostAsset(ctx, SeedInsertPostAssetParams{
			PostID: postRowID, AssetID: id, SortOrder: 0,
		}); err != nil {
			return fmt.Errorf("fixture post member %d: %w", i, err)
		}
	}
	r.log.Info("seed.fixtures.admin_uploads",
		"assets", assets, "posts", posts, "requested", spec.Count)
	return nil
}
