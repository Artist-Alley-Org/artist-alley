// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// DB-direct seed loader (issue #321). Replaces the HTTP-based
// seed/scripts/apply.py: the Runner walks the same dependency-ordered
// phases apply.py did, but writes straight to the tables via the sqlc
// queries in this package + the storage.Service for asset bytes. No
// running server, no admin login — just a config.Load() pool + a
// storage backend.
//
// Phase order (mirrors apply.py):
//
//	resolveLookups   read workflow_states + asset_types (baseline-seeded)
//	applyUsers       forge fictional users (+ federation keypair)
//	applyTeams       teams + self-closure rows
//	applyMemberships each user -> primary team
//	applyFields      custom field definitions
//	applyCollections owner = bootstrap admin
//	applyAssets      bytes -> content store, then asset row + tags +
//	                 collection membership + typed field values
//	applyPosts       post row + members + tags + collection linkage
//	applyCollectionPostBackfill
//	                 compose a post for every collection member the
//	                 dataset left bare, so the wall shows them (#1185)
//	applyComments    forge reviewer comments from asset review_notes
//	verify           count rows
//
// Timestamps are written inline at insert time (assets/posts carry the
// dataset's created_at/updated_at directly), so apply.py's separate
// backfill phase is unnecessary here.

package seed

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/preview/format3d"
	"github.com/mscrnt/artist-alley/app/internal/richtext"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// previewPriority: bulk seeds go in at backfill priority so 970 assets
// can never preempt interactive work. The upload path uses
// PriorityHigh, which is right for one interactive upload and wrong for
// a whole dataset (#355).
var previewPriority = jobs.PriorityBackfil

// Options configures a seed Run.
type Options struct {
	SiteRoot      string // populated site dir: MANIFEST.json + posts.json + bytes
	CatalogueRoot string // seed/profiles
	LimitPerExt   int    // >0: keep at most N assets per file_extension (CI shrink)
	AdminUsername string // bootstrap admin username; owns collections
	Logger        *slog.Logger

	// Profile selects a named catalogue-shrink strategy (#768).
	// "" = the full catalogue, which is what the demo runs. "ci" runs
	// coverage selection: greedy set-cover over posts plus a depth
	// floor, so every file type / relation class survives at a fraction
	// of the scale. See coverage.go for why post-first and why it
	// errors rather than warns on a gap.
	Profile string

	// CoverageDepth is the profile's depth floor: at least this many
	// posts per collection and assets per extension, bounded by what
	// the catalogue holds. It, not the cover, is what sizes the seed —
	// minimum cover is degenerate for grid / masonry / pagination
	// specs. 0 uses defaultCoverageDepth.
	CoverageDepth int

	// Previews enqueues a preview job per seeded asset (#355). Without
	// it a seeded instance has originals and zero derivatives — no card
	// thumbnails (`col`), no video hover sprites — which is what the
	// HTTP-era apply.py got for free by uploading through the API.
	//
	// Jobs are enqueued at PriorityBackfil so a bulk seed can never
	// preempt interactive work; the running app's worker pool drains
	// them. Set false for a fast metadata-only seed.
	Previews bool

	// ForcePreviews makes those jobs re-render variants that are
	// already in storage (#760).
	//
	// It exists because `--reset` and the variant store diverge BY
	// DESIGN: variants are content-addressed and describe what is on
	// the storage volume, so a TRUNCATE of the content tables cannot
	// and must not erase them (see seed.Reset). Re-seeding the same
	// dataset therefore re-enqueues a preview job per asset whose
	// output already exists — every one of which skips.
	//
	// That default is correct and cheap. What was wrong was that it
	// was SILENT: `preview.3d done: 590, failed: 0` looks identical
	// whether 590 renders happened or none did. So the seed now
	// reports the skip count up front (see previewSkipEstimate), and
	// this flag is how an operator says "the renderer changed, rebuild
	// them".
	ForcePreviews bool

	// Fixtures seeds the dogfood suite's one-time substrate — the four
	// login-capable principals and the admin-owned plates the specs used
	// to create for themselves and could never delete (#1270). Off by
	// default: these accounts have committed passwords and the public
	// demo must not have them. See fixtures.go.
	Fixtures bool

	// HashPassword persists a plaintext password on a seeded user. Nil
	// leaves the column NULL, which is right for the 31 fictional
	// artists — they are actors on posts and comments, not accounts —
	// and refused by AdminHandler.CreateUser the moment a password is
	// actually supplied, so a missing hasher fails loudly instead of
	// writing plaintext or silently dropping the credential.
	HashPassword PasswordHasher
}

// Counts is the verify-phase tally.
type Counts struct {
	Users       int
	Teams       int
	Collections int
	Assets      int
	Posts       int
	Comments    int
	Follows     int
	Likes       int
	Featured    int

	// What the posts phase found it could NOT apply (#1320). Row counts
	// alone cannot carry this: a reseed over a changed catalogue reports
	// the same post count as a reseed over an unchanged one, because the
	// rows are all there. It is the values inside them that are stale.
	// Zero on a first seed and on a clean resume, which is the contract.
	PostsDrifted  int
	PostsOrphaned int
}

// Runner executes the seed phases against a live pool + storage.
type Runner struct {
	pool    *pgxpool.Pool
	q       *Queries
	storage *storage.Service
	admin   *AdminHandler
	jobs    *jobs.Service
	log     *slog.Logger
	opts    Options

	adminRef int64

	// genTime is the instant this run started, and the ceiling every
	// row timestamp is clamped to (#1174). Captured ONCE so a run is
	// internally consistent: a post, its assets and its derived
	// likes/comments all measure against the same "now" no matter how
	// long the run takes.
	genTime time.Time

	// resolved lookups
	assetStates map[string]pgtype.UUID // asset:1 code -> state id
	postStates  map[string]pgtype.UUID // post code -> state id
	assetTypes  map[string]int64       // seed label -> asset_type ref

	// per-phase ID maps (seed natural key -> server id/ref)
	users       map[string]int64       // username -> ref
	teams       map[string]pgtype.UUID // slug -> id
	fields      map[string]fieldMeta   // code -> {id, type}
	collections map[string]pgtype.UUID // name -> id
	assets      map[string]pgtype.UUID // manifest id -> asset id (inserted only)
	posts       map[string]pgtype.UUID // post id -> post id (inserted only)

	// asset field values applyAssetFields threw away, per reason per
	// code (#807). Summarised at the end of the asset phase.
	fieldDrops *fieldDropTally

	// what the posts phase found it could not correct (#1320). Set by
	// applyPosts; nil before that phase runs.
	postDrift *postDrift
}

type fieldMeta struct {
	id  pgtype.UUID
	typ string
}

// NewRunner builds a Runner. storageSvc must be backed by the same
// backend + root the serving process uses so seeded bytes are
// retrievable after the run.
func NewRunner(pool *pgxpool.Pool, storageSvc *storage.Service, opts Options) *Runner {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.AdminUsername == "" {
		opts.AdminUsername = "admin"
	}
	return &Runner{
		pool:    pool,
		q:       New(pool),
		storage: storageSvc,
		admin:   NewAdminHandler(pool, nil, nil, nil, opts.HashPassword, nil),
		// Enqueue-only Service: the seeder never runs jobs, it just
		// inserts rows for the serving process's pool to drain. A nil
		// Registry is fine — Enqueue doesn't consult it.
		jobs:        jobs.NewService(pool, opts.Logger, nil),
		log:         opts.Logger,
		opts:        opts,
		genTime:     time.Now().UTC(),
		assetStates: map[string]pgtype.UUID{},
		postStates:  map[string]pgtype.UUID{},
		assetTypes:  map[string]int64{},
		users:       map[string]int64{},
		teams:       map[string]pgtype.UUID{},
		fields:      map[string]fieldMeta{},
		collections: map[string]pgtype.UUID{},
		assets:      map[string]pgtype.UUID{},
		posts:       map[string]pgtype.UUID{},
		fieldDrops:  newFieldDropTally(),
	}
}

// Run loads the catalogues + manifest and executes every phase in
// order, returning the verify counts.
func (r *Runner) Run(ctx context.Context) (Counts, error) {
	// Validate the profile name BEFORE touching disk: a typo should say
	// so, not fail thirty seconds later on something unrelated, and must
	// never fall through to seeding the full catalogue.
	switch r.opts.Profile {
	case "", ProfileFull, ProfileCI:
	default:
		return Counts{}, fmt.Errorf("unknown seed profile %q (want %q or %q)",
			r.opts.Profile, ProfileFull, ProfileCI)
	}
	// The two shrinks select on opposite axes and the extension limit
	// runs second, so combining them would silently re-open the hole the
	// profile exists to close: it would cascade-drop coverage-selected
	// posts whose assets it cut. Refuse rather than produce a fixture
	// whose coverage report is a lie.
	if r.opts.Profile == ProfileCI && r.opts.LimitPerExt > 0 {
		return Counts{}, errors.New(
			"seed: --profile ci and --limit-per-extension are mutually exclusive; " +
				"the profile's own depth floor is the extension control (--coverage-depth)")
	}
	cat, err := loadCatalogues(r.opts.CatalogueRoot, r.opts.SiteRoot)
	if err != nil {
		return Counts{}, err
	}
	// Before the shrinks, and long before any bytes move: a manifest
	// that declares AI over somebody else's work must not reach the
	// database (#1260). See AIDeclarableSourcePrefix for why this is
	// checked here as well as in apply_upgrade.py — the two read
	// different files, and the manifest is the one the repo cannot see.
	if err := cat.validateAIDeclarations(); err != nil {
		return Counts{}, err
	}
	if r.opts.Profile == ProfileCI {
		depth := r.opts.CoverageDepth
		if depth <= 0 {
			depth = defaultCoverageDepth
		}
		rep, cErr := cat.applyCoverageProfile(depth, r.log)
		if rep != nil {
			fmt.Print(rep.Summary())
		}
		if cErr != nil {
			return Counts{}, cErr
		}
	}
	if r.opts.LimitPerExt > 0 {
		cat.applyExtensionLimit(r.opts.LimitPerExt, r.log)
	}
	r.log.Info("seed.catalogues",
		"users", len(cat.Users), "teams", len(cat.Teams),
		"collections", len(cat.Collections), "fields", len(cat.Fields),
		"assets", len(cat.Assets), "posts", len(cat.Posts))

	type phase struct {
		name string
		fn   func(context.Context, *catalogues) error
	}
	phases := []phase{
		{"resolveLookups", r.resolveLookups},
		{"applyUsers", r.applyUsers},
		{"applyTeams", r.applyTeams},
		{"applyMemberships", r.applyMemberships},
		{"applyFollows", r.applyFollows},
		{"applyFields", r.applyFields},
		{"applyCollections", r.applyCollections},
		{"applyFeatured", r.applyFeatured},
		{"applyAssets", r.applyAssets},
		{"applyPosts", r.applyPosts},
		{"applyCollectionPostBackfill", r.applyCollectionPostBackfill},
		{"applyLikes", r.applyLikes},
		{"applyComments", r.applyComments},
		{"applyPostComments", r.applyPostComments},
		// Last, and a no-op unless --fixtures asked for it. It depends on
		// the admin ref, the workflow states and the asset types, and on
		// nothing the dataset carries — so it sits after the corpus
		// rather than inside it. See fixtures.go.
		{"applyTestFixtures", r.applyTestFixtures},
	}
	for _, p := range phases {
		start := time.Now()
		r.log.Info("seed.phase.start", "phase", p.name)
		if err := p.fn(ctx, cat); err != nil {
			return Counts{}, fmt.Errorf("phase %s: %w", p.name, err)
		}
		r.log.Info("seed.phase.done", "phase", p.name, "elapsed", time.Since(start).String())
	}
	counts, err := r.verify(ctx)
	if err != nil {
		return counts, err
	}
	return r.withDriftCounts(counts), nil
}

// withDriftCounts carries the posts phase's report onto the run's own
// summary, because the summary is the last thing printed and the last
// thing read. A warning that scrolled past ten thousand log lines,
// followed by a final line reporting nothing but success, is still a run
// that reads as clean (#1320).
func (r *Runner) withDriftCounts(c Counts) Counts {
	if r.postDrift == nil {
		return c
	}
	c.PostsDrifted = r.postDrift.drifted
	c.PostsOrphaned = len(r.postDrift.orphans)
	return c
}

// --- phase: resolve lookups -------------------------------------------

func (r *Runner) resolveLookups(ctx context.Context, _ *catalogues) error {
	adminUser := r.opts.AdminUsername
	ref, err := r.q.SeedGetUserRefByUsername(ctx, &adminUser)
	if err != nil {
		return fmt.Errorf("resolve admin %q: %w", r.opts.AdminUsername, err)
	}
	r.adminRef = ref
	r.users[r.opts.AdminUsername] = ref

	states, err := r.q.SeedListWorkflowStates(ctx)
	if err != nil {
		return err
	}
	for _, s := range states {
		switch {
		case s.Domain == "post":
			r.postStates[s.Code] = s.ID
		case strings.HasPrefix(s.Domain, "asset"):
			// asset:1 (only asset domain the baseline seeds); every
			// asset type collapses onto it.
			r.assetStates[s.Code] = s.ID
		}
	}

	types, err := r.q.SeedListAssetTypes(ctx)
	if err != nil {
		return err
	}
	byName := map[string]int64{}
	for _, t := range types {
		if t.Name != nil {
			byName[strings.ToLower(*t.Name)] = t.Ref
		}
	}
	// seed label -> hint names (mirrors apply.py SEED_ASSET_TYPE_HINT_NAMES)
	hints := map[string][]string{
		"image":    {"image", "raster", "picture"},
		"audio":    {"audio", "sound"},
		"3d":       {"3d object", "3d", "model"},
		"video":    {"video", "movie"},
		"document": {"document", "doc", "pdf"},
		"font":     {"font", "typeface"},
		"comic":    {"comic", "cbz"},
	}
	for label, hs := range hints {
		for _, h := range hs {
			if ref, ok := byName[h]; ok {
				r.assetTypes[label] = ref
				break
			}
		}
	}
	r.log.Info("seed.lookups", "admin_ref", r.adminRef,
		"asset_states", len(r.assetStates), "post_states", len(r.postStates),
		"asset_types", len(r.assetTypes))
	return nil
}

// --- phase: users -----------------------------------------------------

func (r *Runner) applyUsers(ctx context.Context, cat *catalogues) error {
	for _, u := range cat.Users {
		in := UserInput{Username: u.Username, Approved: true}
		if u.FullName != "" {
			in.Fullname = &u.FullName
		}
		if u.Email != "" {
			in.Email = &u.Email
		}
		res, err := r.admin.CreateUser(ctx, nil, r.adminRef, in)
		if err != nil {
			return fmt.Errorf("create user %s: %w", u.Username, err)
		}
		r.users[u.Username] = res.Ref
	}
	r.log.Info("seed.users", "count", len(cat.Users))
	return nil
}

// --- phase: teams -----------------------------------------------------

func (r *Runner) applyTeams(ctx context.Context, cat *catalogues) error {
	for _, t := range cat.Teams {
		slug := slugify(t.Name)
		id, err := r.insertTeam(ctx, t.ID, slug, t.Name)
		if err != nil {
			return fmt.Errorf("create team %s: %w", slug, err)
		}
		r.teams[slug] = id
		if err := r.q.SeedInsertTeamClosureSelf(ctx, id); err != nil {
			return fmt.Errorf("team closure %s: %w", slug, err)
		}
	}
	r.log.Info("seed.teams", "count", len(r.teams))
	return nil
}

func (r *Runner) insertTeam(ctx context.Context, seedID, slug, name string) (pgtype.UUID, error) {
	tid := parseUUID(seedID)
	id, err := r.q.SeedInsertTeam(ctx, SeedInsertTeamParams{
		ID:          tid,
		Slug:        slug,
		Name:        name,
		Description: name + " team",
	})
	if err == nil {
		return id, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Existing row (resumed run) — recover its id by slug.
		return r.q.SeedGetTeamBySlug(ctx, slug)
	}
	return pgtype.UUID{}, err
}

// --- phase: memberships ----------------------------------------------

func (r *Runner) applyMemberships(ctx context.Context, cat *catalogues) error {
	n := 0
	for _, u := range cat.Users {
		if u.PrimaryTeam == "" {
			continue
		}
		userRef, ok := r.users[u.Username]
		if !ok {
			continue
		}
		teamID, ok := r.teams[slugify(u.PrimaryTeam)]
		if !ok {
			r.log.Warn("seed.membership.skip", "user", u.Username, "team", u.PrimaryTeam)
			continue
		}
		if err := r.q.SeedInsertTeamMembership(ctx, SeedInsertTeamMembershipParams{
			TeamID:  teamID,
			UserRef: userRef,
		}); err != nil {
			return fmt.Errorf("membership %s: %w", u.Username, err)
		}
		n++
	}
	r.log.Info("seed.memberships", "count", n)
	return nil
}

// --- phase: fields ----------------------------------------------------

func (r *Runner) applyFields(ctx context.Context, cat *catalogues) error {
	// BIND THE FIELDS THAT ALREADY EXIST FIRST (#820).
	//
	// applyAssetFields resolves a manifest's field code against
	// r.fields and drops anything it cannot find as
	// `seed.field.unknown_code`. r.fields used to be built from
	// cat.Fields alone — the 20 studio codes in
	// dataset.field_definitions.json — so the nine codes the
	// MIGRATIONS ship were absent from it even though their rows were
	// sitting in field_definition, and a manifest value for `country`
	// or `keywords` was thrown away with a warning that read like a
	// typo in the manifest.
	//
	// That was invisible until #812: `aa seed --reset` used to TRUNCATE
	// field_definition, so on a seeded instance those rows genuinely did
	// not exist and "unknown code" was the truth. Since #812 they
	// survive the reset, the rows are there, and the map was simply not
	// looking at the table.
	//
	// Every existing definition is bound, not just the shipped ones. A
	// code the seed can write is "a field this install has", and there
	// is no principled line between a row a migration inserted, a row an
	// operator created and a row a federation peer minted — all three
	// are fields, and a manifest naming one means the same thing. This
	// does not weaken the unknown_code check: a code that matches no row
	// at all is still dropped and still warned about, which is the
	// manifest typo the warning is for.
	//
	// The catalogue loop below then runs over the top and REPLACES the
	// entry for any code it also declares, so its type-mismatch
	// detection (existing row's type wins over the JSON's) is unchanged.
	existing, err := r.q.SeedListFields(ctx)
	if err != nil {
		return fmt.Errorf("list existing fields: %w", err)
	}
	for _, f := range existing {
		r.fields[f.Code] = fieldMeta{id: f.ID, typ: f.Type}
	}
	if len(existing) > 0 {
		r.log.Info("seed.fields.preexisting", "count", len(existing))
	}

	for _, f := range cat.Fields {
		opts := []byte("{}")
		if len(f.Options) > 0 {
			b, err := json.Marshal(map[string]any{"values": f.Options})
			if err != nil {
				return err
			}
			// Same validation + canonicalisation the admin write path
			// runs (metadata handler, create and update). The seed is
			// NOT trusted input just because it lives in the repo: the
			// catalogue is hand-edited, and the invariant that matters
			// most here — slugs unique across the whole option tree —
			// is exactly the one a human adding a `tree` branch breaks
			// by copy-paste. Break it and values resolve to the wrong
			// node with no error anywhere (ADR 0012, tree amendment).
			//
			// Fatal, not warn-only, unlike the per-value drops below: a
			// bad catalogue is a repo defect that every seed of every
			// instance would inherit, not one manifest row.
			b, err = metadata.NormalizeOptionsDoc(b)
			if err != nil {
				return fmt.Errorf("field %s options: %w", f.Name, err)
			}
			opts = b
		}
		// typ is the type values for this code will be WRITTEN as. It
		// starts as the catalogue's and is replaced by the existing
		// row's below when the insert binds to one, because the column
		// the value lands in is chosen by the row, not by the JSON.
		typ := f.Type
		id, err := r.q.SeedInsertField(ctx, SeedInsertFieldParams{
			Code:             f.Name,
			Label:            f.Label,
			Type:             f.Type,
			Options:          opts,
			ExtractionSource: f.ExtractionSource,
			ExtractionMode:   f.ExtractionMode,
		})
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("create field %s: %w", f.Name, err)
			}
			// ON CONFLICT (code) DO NOTHING returned no row: a
			// definition with this code already exists. Since #812 that
			// is the NORMAL case for a code the migrations ship —
			// `aa seed --reset` no longer truncates field_definition, so
			// pixel_width/pixel_height are always already there.
			//
			// Binding to the existing row is right. Trusting the
			// CATALOGUE's type for it is not: applyAssetFields picks the
			// value column from fieldMeta.typ, so a catalogue entry
			// declaring `date` against a `datetime` row would have every
			// value written into the wrong column — or silently dropped
			// as value_rejected — while the field count still looked
			// correct. No row count can see it, which is the same shape
			// as the reference NULL-row bug (#809).
			existing, getErr := r.q.SeedGetFieldByCode(ctx, f.Name)
			if getErr != nil {
				return fmt.Errorf("create field %s: %w", f.Name, getErr)
			}
			id = existing.ID
			if existing.Type != f.Type {
				typ = existing.Type
				if r.fieldDrops.record(dropTypeMismatch, f.Name) {
					r.log.Warn("seed.field.type_mismatch",
						"code", f.Name,
						"catalogue_type", f.Type,
						"existing_type", existing.Type)
				}
			}
		}
		r.fields[f.Name] = fieldMeta{id: id, typ: typ}
	}
	r.log.Info("seed.fields", "count", len(r.fields))
	return nil
}

// --- phase: collections ----------------------------------------------

func (r *Runner) applyCollections(ctx context.Context, cat *catalogues) error {
	// The catalogue lists every collection across BOTH studios, but each
	// site ships only its own studio's assets (plus shared) — so creating
	// all of them left site_a with 11 of 18 collections holding zero
	// assets, and any `featured` flag on one of those put an empty
	// collection on the front rail (#565).
	//
	// Skip the ones with no content HERE. This is not "the studio split is
	// wrong" — Project Toybox is empty on site_a and 516 assets deep on
	// site_b, which is the split working. It is that an empty shell should
	// not be created at all. applyFeatured then drops the corresponding
	// featured entry on its own, since it only features collections that
	// made it into r.collections.
	withContent := make(map[string]struct{}, len(cat.Assets))
	for _, a := range cat.Assets {
		if a.CollectionName != "" {
			withContent[a.CollectionName] = struct{}{}
		}
	}
	skipped := 0
	for _, c := range cat.Collections {
		if _, ok := withContent[c.Name]; !ok {
			skipped++
			continue
		}
		cid := parseUUID(c.ID)
		// org-only unless the catalogue says otherwise. That default is
		// the restrictive one on purpose: a demo dataset should not
		// publish anything by accident, and every entry that predates
		// the field keeps exactly the tier it had.
		vis := c.Visibility
		if vis == "" {
			vis = "org-only"
		}
		id, err := r.q.SeedInsertCollection(ctx, SeedInsertCollectionParams{
			ID:           cid,
			OwnerUserRef: r.adminRef,
			Name:         c.Name,
			// No em dash (#1306): this string is rendered on the
			// browse wall under every collection tile, so it is
			// seeded COPY, not an internal label.
			Description: "Seeded collection: " + c.Name,
			Visibility:  vis,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				id = cid // stable id insert; conflict means it already exists with this id
			} else {
				return fmt.Errorf("create collection %s: %w", c.Name, err)
			}
		}
		r.collections[c.Name] = id
	}
	r.log.Info("seed.collections", "count", len(r.collections), "skipped_empty", skipped)
	return nil
}

// --- phase: featured --------------------------------------------------

// applyFeatured places each collection flagged `"featured": true` in
// dataset.collections.json (#380) onto the public rail, so the demo's
// featured surfaces aren't empty on a fresh reset.
//
// ONE writer, not two. This phase used to write both a featured_items
// row AND collections.featured, because there were two featured
// mechanisms and the boolean was the only one with a public renderer.
// ADR 0065 collapsed them: featuring is a placement, the boolean is
// gone, and #417 gave featured_items the public rail the boolean used
// to stand in for. Since #416 that rail is the entire landing page for
// a logged-out visitor, which is why these placements are scope
// 'public' — an empty front page on a fresh demo is exactly the
// failure this phase exists to prevent.
//
// The catalogue's `"featured": true` stays as vocabulary; it now means
// "place this publicly" rather than "set a column".
//
// Runs after applyCollections, so every flagged collection already has
// a row + a stable id in r.collections. subject_kind is always
// 'collection': the featured_items CHECK also accepts 'asset', but
// assets live in the archive manifest, not this versioned catalogue, so
// featuring them would belong to a different phase.
//
// Idempotent: ON CONFLICT DO NOTHING on the placement constraint, and
// the owner is the bootstrap admin — the same created_by_user_ref
// applyCollections uses for the collections.
func (r *Runner) applyFeatured(ctx context.Context, cat *catalogues) error {
	featured := 0
	for _, c := range cat.Collections {
		if !c.Featured {
			continue
		}
		id, ok := r.collections[c.Name]
		if !ok {
			// A featured flag on a collection applyCollections didn't
			// create is a catalogue mistake, not a fatal seed error.
			r.log.Warn("seed.featured.unknown_collection", "name", c.Name)
			continue
		}
		if err := r.q.SeedInsertFeatured(ctx, SeedInsertFeaturedParams{
			SubjectKind:      "collection",
			SubjectID:        id,
			CreatedByUserRef: &r.adminRef,
		}); err != nil {
			return fmt.Errorf("feature collection %s: %w", c.Name, err)
		}
		featured++
	}
	r.log.Info("seed.featured", "count", featured)
	return nil
}

// --- phase: assets ----------------------------------------------------

func (r *Runner) applyAssets(ctx context.Context, cat *catalogues) error {
	inserted, deduped, missing, queued, willSkip := 0, 0, 0, 0, 0
	// Assets already present from an earlier seed. Reported separately
	// from `deduped` because they mean opposite things: a resumed row IS
	// this manifest entry, a deduped one never had a row of its own.
	resumed := 0
	for i, a := range cat.Assets {
		abs := filepath.Join(r.opts.SiteRoot, a.FilePath)
		f, err := os.Open(abs)
		if err != nil {
			r.log.Warn("seed.asset.open", "id", a.ID, "path", abs, "err", err.Error())
			missing++
			continue
		}
		up, err := r.storage.UploadOriginal(ctx, f, guessContentType(a.FileExtension),
			storage.PinRef{SubjectType: "asset", SubjectID: a.ID})
		f.Close()
		if err != nil {
			return fmt.Errorf("upload asset %s: %w", a.ID, err)
		}

		typeRef, ok := r.assetTypes[a.AssetType]
		if !ok {
			typeRef = 1 // fall back to Image
		}
		created, updated := r.rowTimes(a.CreatedAt, a.UpdatedAt)
		var ownerRef *int64
		if ref, ok := r.users[a.OwnerUsername]; ok {
			ownerRef = &ref
		}
		metadata := []byte("{}")
		if len(a.Metadata) > 0 {
			metadata = a.Metadata
		}
		hash := up.Hash
		size := up.Size
		ext := a.FileExtension
		params := SeedInsertAssetParams{
			ID:            parseUUID(a.ID),
			Title:         orDefault(a.Title, "Untitled"),
			Description:   a.Description,
			AssetType:     typeRef,
			OwnerUserRef:  ownerRef,
			Status:        assetStatus(a.ArchiveState),
			FileHash:      &hash,
			FileExtension: &ext,
			FileSizeBytes: &size,
			Metadata:      metadata,
			StateID:       r.resolveAssetState(a.WorkflowState),
			TeamID:        r.teamIDForName(a.TeamName),
			Sensitivity:   sensitivity(a.SensitivityTier),
			// The catalogue's own label, written verbatim (ADR 0090):
			// mature is a rating, sensitivity is a clearance, and the
			// two are never merged. Posts are NOT written from here —
			// their flag is derived by the 00052/00054 triggers off
			// membership (and off the cover), which is why applyPosts
			// has nothing to say about it.
			Mature: a.Mature,
			// The maker's AI declaration, written verbatim and NULL
			// when the catalogue says nothing (#1251 slice 3, ADR
			// 0094). NULL is UNDECLARED, not `none`: writing `none`
			// over a work nobody was asked about would fabricate that
			// maker's disclaimer, which is the one thing the nullable
			// column exists to prevent.
			//
			// Posts are NOT written from here — `ai_provenance` and
			// `ai_pure` are both derived by the 00060/00061 triggers off
			// the post's contributors (members UNION the two covers),
			// same as `mature` above.
			AiProvenance: a.AiProvenance,
			CreatedAt:    created,
			UpdatedAt:    updated,
		}
		id, err := r.q.SeedInsertAsset(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			// TWO different conflicts land here and they need opposite
			// handling — see SeedGetAssetIDByID for the full story.
			//
			//   id pkey          a RESUMED run. The row is this entry.
			//                    Register it, or every later phase acts
			//                    as though the asset does not exist.
			//   owner+file_hash  a byte-identical sibling the same owner
			//                    already holds — e.g. the texture
			//                    exported beside both the OBJ and the
			//                    FBX of a model. The collapse is CORRECT
			//                    (it mirrors the app refusing a re-upload
			//                    of identical bytes by the same owner)
			//                    and there is no row under this id, so
			//                    skipping is right.
			//
			// Treating both as "skip" is what made an incremental
			// re-seed drop members: `applyPosts` resolves members from
			// the map this loop fills, so a post added to the catalogue
			// after the first seed lost every member that already
			// existed, and a post whose members ALL existed vanished as
			// a no-member post. Found driving #1290 — the mixed-state
			// post seeded with one of its two members.
			existing, gErr := r.q.SeedGetAssetIDByID(ctx, params.ID)
			if gErr == nil {
				r.assets[a.ID] = existing
				resumed++
				continue
			}
			if !errors.Is(gErr, pgx.ErrNoRows) {
				return fmt.Errorf("recover asset %s: %w", a.ID, gErr)
			}
			deduped++
			continue
		}
		if err != nil {
			return fmt.Errorf("insert asset %s: %w", a.ID, err)
		}
		r.assets[a.ID] = id

		// Register multi-file companions (#486, #750). A .gltf/.glb/.obj
		// declares its buffer/textures/.mtl as sibling files; without
		// companion rows the render stages nothing and the interactive
		// viewer's GLTFLoader 404s on the .bin/textures, so the model
		// renders blank or untextured. Do this BEFORE the preview enqueue
		// below so the worker finds them staged. Best-effort: a companion
		// hiccup shouldn't fail an otherwise-good seed.
		r.applyAssetCompanions(ctx, id, a.ID, abs)

		// Dispatch the preview job (#355). Enqueued AFTER the asset row
		// exists — the handler resolves the asset by id — and using the
		// same dispatch map + payload shape as the upload path, so the
		// two can't diverge. Best-effort: a queue hiccup shouldn't fail
		// an otherwise-good seed, so we log and carry on.
		// Skip exts no handler can render — they'd dispatch to
		// preview.raster and terminal-fail (#366). CanPreview keeps the
		// seed from minting dead jobs for the odd unpreviewable file in
		// the catalogue.
		if r.opts.Previews && dispatch.CanPreview(&ext) {
			// Count the ones whose card thumbnail is ALREADY on the
			// storage volume before enqueueing. Without --force-previews
			// their jobs will skip, and "queued 590" with no further
			// detail is indistinguishable from 590 real renders — the
			// ambiguity #760 was filed over.
			if !r.opts.ForcePreviews && variantOnDisk(ctx, r.storage, hash, "col") {
				willSkip++
			}
			// PlanForExt, so a seeded video gets its cheap poster job
			// alongside the full ladder (#818). On a seed this is the
			// difference between a browse grid that fills in as the run
			// proceeds and one that stays blank behind 74%-of-CPU video
			// encodes. `queued` still counts assets, not jobs.
			payload := dispatch.NewPayload(uuid.UUID(id.Bytes), hash, &ext, r.opts.ForcePreviews)
			enqueued := 0
			for _, step := range dispatch.PlanForExt(&ext, previewPriority) {
				priority := step.Priority
				if _, jErr := r.jobs.Enqueue(ctx, step.Type, payload,
					jobs.EnqueueOpts{Priority: &priority},
				); jErr != nil {
					r.log.Warn("seed.preview.enqueue_failed", "asset", a.ID,
						"job_type", string(step.Type), "err", jErr.Error())
					continue
				}
				enqueued++
			}
			if enqueued > 0 {
				queued++
			}
		}

		// tags
		for _, tag := range dedupStrings(a.Tags) {
			if err := r.q.SeedInsertAssetTag(ctx, SeedInsertAssetTagParams{AssetID: id, Tag: tag}); err != nil {
				return fmt.Errorf("asset tag %s: %w", a.ID, err)
			}
		}
		// Collection membership — a `collection_resources` row, written
		// DELIBERATELY and kept after #1236 retired every rendered
		// surface that read it.
		//
		// It is not what publishes the asset: applyCollectionPostBackfill
		// below authors a post for every member the dataset left bare,
		// and THAT is what the collection page and the cover mosaic
		// draw. This row is the dataset's own record of which collection
		// an asset belongs to, and two live consumers still resolve
		// "assets inside this collection" through it — the `collection:`
		// search facet and the reindex job's ScopeCollection. Dropping
		// the write would make a seeded install's scoped search answer
		// empty. See the header note in collections/handler.go for the
		// full consumer list.
		if a.CollectionName != "" {
			if cid, ok := r.collections[a.CollectionName]; ok {
				if err := r.q.SeedInsertCollectionResource(ctx, SeedInsertCollectionResourceParams{
					CollectionID: cid, AssetID: id,
				}); err != nil {
					return fmt.Errorf("collection resource %s: %w", a.ID, err)
				}
			}
		}
		// typed field values
		if err := r.applyAssetFields(ctx, id, a.FieldValues); err != nil {
			return fmt.Errorf("asset fields %s: %w", a.ID, err)
		}
		inserted++
		if (i+1)%200 == 0 {
			r.log.Info("seed.assets.progress", "processed", i+1, "inserted", inserted)
		}
	}
	r.logFieldDrops()
	r.log.Info("seed.assets", "inserted", inserted,
		"deduped", deduped, "resumed", resumed, "missing", missing,
		"previews_queued", queued,
		"previews_force", r.opts.ForcePreviews,
		"previews_will_skip", willSkip)
	// Say it in plain words too, on stdout, where whoever ran the seed
	// is looking. A log line at INFO inside a JSON handler is not where
	// an operator finds out that the renders they were waiting for are
	// not going to happen.
	if willSkip > 0 {
		fmt.Printf(
			"NOTE: %d of %d queued preview jobs will SKIP — those assets already have "+
				"rendered variants on the storage volume, which --reset deliberately does "+
				"not erase (they are content-addressed). Re-run with --force-previews to "+
				"re-render them.\n", willSkip, queued)
	}
	return nil
}

// variantOnDisk reports whether a rendered variant is already on the
// storage backend. Deliberately the same question the preview handlers
// ask (backend Stat, not a storage_variants row) so the seed's estimate
// and the worker's decision cannot disagree.
func variantOnDisk(ctx context.Context, st *storage.Service, hash, key string) bool {
	if st == nil {
		return false
	}
	_, err := st.Backend.Stat(ctx, hash, key)
	return err == nil
}

// applyAssetCompanions parses a seeded model's declared external
// resources (glTF/GLB buffers[].uri + images[].uri; OBJ mtllib → MTL
// map_*) and registers each sibling that exists on disk as a companion
// (#486). Mirrors what the HTTP AddAssetCompanion handler does — upload
// bytes content-addressed, pin them, insert the asset+path→blob row —
// but drives it straight off the parsed model instead of per-file API
// calls, so the seed self-wires any complete multi-file model dropped
// into its source tree.
//
// Which extensions carry companions is ResolveCompanions' call alone.
// This used to pre-filter on `case "gltf", "obj"`, which duplicated that
// list and let it rot: GLB was excluded here as "self-contained" and
// stayed excluded even once the resolver could read it, so 363 seeded
// GLBs got zero companion rows and rendered untextured (#750). One
// source of truth now — an extension with no declared references costs a
// string switch and no file I/O.
//
// Every step is soft-fail: single-file models resolve to no companions
// and return immediately; an unparseable model, or a missing or
// unreadable sibling, logs and is skipped so the asset still seeds (it
// just renders untextured, which beats failing the whole seed).
func (r *Runner) applyAssetCompanions(ctx context.Context, assetID pgtype.UUID, manifestID, mainPath string) {
	found, missing, err := format3d.ResolveCompanions(mainPath)
	if err != nil {
		r.log.Warn("seed.companion.resolve", "id", manifestID, "err", err.Error())
		return
	}
	for _, m := range missing {
		r.log.Warn("seed.companion.missing", "id", manifestID, "path", m)
	}

	dir := filepath.Dir(mainPath)
	registered := 0
	for _, rel := range found {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		f, oErr := os.Open(abs)
		if oErr != nil {
			r.log.Warn("seed.companion.open", "id", manifestID, "path", rel, "err", oErr.Error())
			continue
		}
		up, uErr := r.storage.UploadOriginal(ctx, f, guessContentType(companionExt(rel)),
			storage.PinRef{SubjectType: "asset_companion", SubjectID: manifestID + "/" + rel})
		f.Close()
		if uErr != nil {
			r.log.Warn("seed.companion.upload", "id", manifestID, "path", rel, "err", uErr.Error())
			continue
		}
		if err := r.q.SeedInsertAssetCompanion(ctx, SeedInsertAssetCompanionParams{
			AssetID:       assetID,
			CompanionPath: rel,
			ObjectHash:    up.Hash,
			ContentType:   guessContentType(companionExt(rel)),
			SizeBytes:     up.Size,
		}); err != nil {
			r.log.Warn("seed.companion.insert", "id", manifestID, "path", rel, "err", err.Error())
			continue
		}
		registered++
	}
	if registered > 0 {
		r.log.Info("seed.companion", "id", manifestID, "registered", registered)
	}
}

// companionExt pulls the extension (no dot) from a relative companion
// path for MIME guessing — path.Ext keeps this working for the
// forward-slash relative paths the parser emits.
func companionExt(rel string) string {
	return strings.TrimPrefix(filepath.Ext(rel), ".")
}

// --- dropped field values (#807) --------------------------------------
//
// applyAssetFields discards a value in two places, and until #807 both
// spellings of "discard" were the same bare `continue`. A malformed
// value and a code that was never defined produced identical output —
// none — so the seed reported success, the field was simply absent, and
// every downstream check that counts rows agreed with itself. That is
// the "green suite over a state production cannot reach" class, except
// here the seed MANUFACTURES the unreachable state.
//
// Both drops now warn, and they warn DIFFERENTLY, because the fixes are
// different: an unknown code is a schema/catalogue mismatch (add the
// definition, or fix the slug), a rejected value is a bad value (fix the
// manifest, or widen the coercion).
//
// Warn-only, for now. There is in-repo precedent for strictness — the
// `ci` coverage profile errors rather than warns on a gap (coverage.go).
// The reason not to be strict was that both seed manifests carried six
// codes with no definition, so every asset hit the unknown-code branch
// by design and a hard error would have broken the seed and therefore
// CI. #808 added those six definitions and a full site_a seed now
// reports unknown_code=0 / value_rejected=0, so the blocker is gone:
// promoting unknown-code to a hard error under ProfileCI is the
// sensible follow-up, and this comment is the record that it is now
// possible.
//
// Note the catalogue's OWN validity is already fatal — applyFields
// runs metadata.NormalizeOptionsDoc and returns the error. The
// asymmetry is deliberate: a broken catalogue is one repo defect every
// instance inherits, a dropped value is one row of one manifest.
const (
	dropUnknownCode   = "unknown_code"
	dropValueRejected = "value_rejected"

	// dropTypeMismatch is counted per FIELD, not per value: it is
	// recorded once by applyFields when a catalogue entry binds to an
	// existing definition of a different type (#812). It rides the same
	// tally because it belongs on the same summary line — the tally is
	// what an operator reads to find out that the catalogue and the
	// database disagree — and because record()'s per-code log limit is
	// exactly the throttling this needs too.
	//
	// It does not itself drop anything. The seed proceeds using the
	// EXISTING row's type, so the values land in the right column; what
	// gets reported is that the catalogue is lying about a field, which
	// is a repo defect to fix rather than a bad manifest row.
	dropTypeMismatch = "type_mismatch"
)

// fieldDropLogLimit caps the per-asset detail warnings emitted for any
// one (reason, code) pair. A single misconfigured field is ~1,900
// identical lines against site_a, which buries every other warning the
// run produced — and the operator learns nothing from line 900 that
// line 1 did not already say. A handful of examples name the code, the
// type and an offending value; the end-of-phase summary carries the
// true totals.
const fieldDropLogLimit = 3

// fieldDropTally counts discarded field values per reason per code.
type fieldDropTally struct {
	byReason map[string]map[string]int // reason -> code -> dropped
	logged   map[string]int            // reason + "|" + code -> warnings emitted
}

func newFieldDropTally() *fieldDropTally {
	return &fieldDropTally{
		byReason: map[string]map[string]int{},
		logged:   map[string]int{},
	}
}

// record counts one drop and reports whether the caller should emit a
// detail warning for it (see fieldDropLogLimit).
func (t *fieldDropTally) record(reason, code string) bool {
	codes, ok := t.byReason[reason]
	if !ok {
		codes = map[string]int{}
		t.byReason[reason] = codes
	}
	codes[code]++
	key := reason + "|" + code
	if t.logged[key] >= fieldDropLogLimit {
		return false
	}
	t.logged[key]++
	return true
}

func (t *fieldDropTally) total(reason string) int {
	n := 0
	for _, c := range t.byReason[reason] {
		n += c
	}
	return n
}

// offenders renders the worst codes for a reason as "code=n, code=n",
// heaviest first and ties broken by code so the line is reproducible.
func (t *fieldDropTally) offenders(reason string, limit int) string {
	codes := t.byReason[reason]
	if len(codes) == 0 {
		return ""
	}
	keys := make([]string, 0, len(codes))
	for c := range codes {
		keys = append(keys, c)
	}
	sort.Slice(keys, func(i, j int) bool {
		if codes[keys[i]] != codes[keys[j]] {
			return codes[keys[i]] > codes[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	parts := make([]string, 0, len(keys))
	for _, c := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", c, codes[c]))
	}
	return strings.Join(parts, ", ")
}

// dropValueRepr renders the offending value for a warning: enough to
// recognise which manifest entry is wrong, bounded so a long rich_text
// body cannot own the log line.
func dropValueRepr(raw any) string {
	if raw == nil {
		return "<nil>"
	}
	s := fmt.Sprintf("%v", raw)
	// Rune-bounded, not byte-bounded: a manifest value is arbitrary
	// text, and slicing mid-rune would put a replacement character in
	// the log where the operator is trying to read a value.
	const max = 120
	if utf8.RuneCountInString(s) > max {
		return string([]rune(s)[:max]) + "…"
	}
	return s
}

// logFieldDrops closes the asset phase with the part an operator
// actually reads. Always logged — zeros are the evidence the check ran
// — with a plain-words stdout NOTE when something was actually dropped,
// same reasoning as the preview-skip note above: a WARN inside a JSON
// handler is not where whoever ran the seed finds out.
func (r *Runner) logFieldDrops() {
	unknown := r.fieldDrops.total(dropUnknownCode)
	rejected := r.fieldDrops.total(dropValueRejected)
	mistyped := r.fieldDrops.total(dropTypeMismatch)
	r.log.Info("seed.field.drops",
		"unknown_code", unknown,
		"value_rejected", rejected,
		"type_mismatch", mistyped,
		"unknown_code_by_code", r.fieldDrops.offenders(dropUnknownCode, 10),
		"value_rejected_by_code", r.fieldDrops.offenders(dropValueRejected, 10),
		"type_mismatch_by_code", r.fieldDrops.offenders(dropTypeMismatch, 10))
	if mistyped > 0 {
		fmt.Printf(
			"NOTE: %d catalogue field(s) declare a type the EXISTING definition does not "+
				"have; the seed used the existing type. Fix the type in "+
				"seed/profiles/dataset.field_definitions.json, or migrate the definition: %s\n",
			mistyped, r.fieldDrops.offenders(dropTypeMismatch, 10))
	}
	if unknown == 0 && rejected == 0 {
		return
	}
	fmt.Printf(
		"NOTE: %d asset field values were DROPPED and are absent from the seeded "+
			"database.\n", unknown+rejected)
	if unknown > 0 {
		fmt.Printf(
			"  %d had no field definition for their code (add the definition, or fix "+
				"the slug in the manifest): %s\n",
			unknown, r.fieldDrops.offenders(dropUnknownCode, 10))
	}
	if rejected > 0 {
		fmt.Printf(
			"  %d carried a value the field's declared type could not accept (see the "+
				"seed.field.value_rejected warnings for examples): %s\n",
			rejected, r.fieldDrops.offenders(dropValueRejected, 10))
	}
}

func (r *Runner) applyAssetFields(ctx context.Context, assetID pgtype.UUID, vals map[string]any) error {
	if len(vals) == 0 {
		return nil
	}
	// Deterministic order for reproducibility.
	codes := make([]string, 0, len(vals))
	for c := range vals {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, code := range codes {
		fm, ok := r.fields[code]
		if !ok {
			// No definition carries this code. A schema/catalogue
			// mismatch, NOT a bad value — the two used to share one
			// silent `continue` and were indistinguishable (#807).
			if r.fieldDrops.record(dropUnknownCode, code) {
				r.log.Warn("seed.field.unknown_code",
					"code", code,
					"asset_id", uuidString(assetID),
					"value", dropValueRepr(vals[code]))
			}
			continue
		}
		params, ok := fieldValueParams(fm.typ, vals[code])
		if !ok {
			// The code is defined; the VALUE could not be coerced into
			// the column its declared type writes.
			if r.fieldDrops.record(dropValueRejected, code) {
				r.log.Warn("seed.field.value_rejected",
					"code", code,
					"type", fm.typ,
					"asset_id", uuidString(assetID),
					"value", dropValueRepr(vals[code]))
			}
			continue
		}
		params.AssetID = assetID
		params.FieldID = fm.id
		if err := r.q.SeedInsertAssetFieldValue(ctx, params); err != nil {
			return err
		}
	}
	return nil
}

// --- phase: posts -----------------------------------------------------

func (r *Runner) applyPosts(ctx context.Context, cat *catalogues) error {
	assetTiers := make(map[string]string, len(cat.Assets))
	for _, a := range cat.Assets {
		assetTiers[a.ID] = sensitivity(a.SensitivityTier)
	}
	// Read the wall back BEFORE writing anything (#1320). Everything
	// this phase would write is `ON CONFLICT DO NOTHING`, so a post the
	// database already holds is one this run cannot correct; the index
	// is what lets it say which ones and on what. See postdrift.go.
	index, err := r.loadPostIndex(ctx)
	if err != nil {
		return err
	}
	drift := newPostDrift()
	// Every id THIS catalogue names, canonicalised. A pre-existing row
	// in here is one a later pass may still touch; one that is not is
	// abandoned. See noteOrphan.
	named := make(map[string]struct{}, len(cat.Posts))
	for _, p := range cat.Posts {
		named[uuidString(parseUUID(p.ID))] = struct{}{}
	}

	inserted, skipped, madePublic := 0, 0, 0
	for _, p := range cat.Posts {
		// Resolve members from inserted assets (apply.py "any member" rule).
		var members []pgtype.UUID
		var coverManifestID string
		for _, aid := range p.AssetIDs {
			if sid, ok := r.assets[aid]; ok {
				if len(members) == 0 {
					coverManifestID = aid
				}
				members = append(members, sid)
			}
		}
		if len(members) == 0 {
			skipped++
			continue
		}
		authorRef, ok := r.users[p.AuthorUsername]
		if !ok {
			authorRef = r.adminRef
		}
		created, updated := r.rowTimes(p.CreatedAt, p.UpdatedAt)
		cover := members[0]
		vis := postVisibility(p, assetTiers[coverManifestID])
		if vis == "public" {
			madePublic++
		}
		collectionID := r.collections[p.CollectionName]
		tags := dedupStrings(p.Tags)
		// Captured HERE, from the values the insert is about to use, so
		// the comparison below can never drift from the write it
		// describes (#1320).
		subject := postSubject{
			title:           orDefault(p.Title, "Untitled"),
			description:     p.Description,
			visibility:      vis,
			cover:           cover,
			created:         created,
			updated:         updated,
			members:         members,
			tags:            tags,
			collection:      collectionID,
			datesComparable: r.datesSurviveTheClamp(p.CreatedAt, p.UpdatedAt),
		}
		rowID := parseUUID(p.ID)
		id, err := r.q.SeedInsertPost(ctx, SeedInsertPostParams{
			ID:            rowID,
			AuthorUserRef: authorRef,
			Title:         subject.title,
			Description:   subject.description,
			Visibility:    vis,
			CoverAssetID:  cover,
			StateID:       r.postStates["published"],
			TeamID:        r.teamIDForName(p.TeamName),
			CreatedAt:     created,
			UpdatedAt:     updated,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// A row already stands under this id. `ON CONFLICT (id)`
				// names exactly one constraint, so unlike the asset path
				// there is only ONE cause to tell apart here and the id
				// is knowable without a recovery query.
				//
				// Registering it is load-bearing and always was: every
				// later phase resolves posts out of this map. What was
				// missing is the rest of this sentence, which is that
				// the run just declined to apply the catalogue to that
				// post and used to say nothing at all about it.
				r.posts[p.ID] = rowID
				drift.resumed++
				// Keyed on the CANONICAL form, not the catalogue's
				// spelling. `uuid.Parse` accepts braces, a urn: prefix
				// and upper case; the index is built from
				// `uuidString`, which only ever emits one of them. A
				// raw string lookup would miss and report nothing,
				// which is this whole issue's failure mode in
				// miniature.
				if have, ok := index.byID[uuidString(rowID)]; ok {
					drift.note(uuidString(rowID), subject.compare(have))
				}
				continue
			}
			return fmt.Errorf("insert post %s: %w", p.ID, err)
		}
		// A CLEAN insert, and the wall may already carry this content
		// under a different id (#1310 moved 618 post ids across the
		// three catalogues). Only a pre-existing row this catalogue no
		// longer names is an orphan; a sibling it still names is an
		// ordinary post that happens to frame the same assets, and 76
		// of the published wall's 861 posts are exactly that. See
		// noteOrphan.
		if digest := memberDigest(members); digest != "" {
			mine := uuidString(rowID)
			if other, ok := index.byMembers[digest]; ok && other != mine {
				if _, stillNamed := named[other]; !stillNamed {
					drift.noteOrphan(mine, other)
				}
			}
		}
		r.posts[p.ID] = id
		for i, m := range members {
			if err := r.q.SeedInsertPostAsset(ctx, SeedInsertPostAssetParams{
				PostID: id, AssetID: m, SortOrder: int32(i),
			}); err != nil {
				return fmt.Errorf("post asset %s: %w", p.ID, err)
			}
		}
		// The SAME slice the comparison above captured, not a second
		// call to dedupStrings. Two computations of "the tags this post
		// gets" is one of them going stale.
		for _, tag := range tags {
			if err := r.q.SeedInsertPostTag(ctx, SeedInsertPostTagParams{PostID: id, Tag: tag}); err != nil {
				return fmt.Errorf("post tag %s: %w", p.ID, err)
			}
		}
		if p.CollectionName != "" {
			if cid, ok := r.collections[p.CollectionName]; ok {
				if err := r.q.SeedInsertCollectionPost(ctx, SeedInsertCollectionPostParams{
					CollectionID: cid, PostID: id,
				}); err != nil {
					return fmt.Errorf("collection post %s: %w", p.ID, err)
				}
			}
		}
		inserted++
	}
	r.log.Info("seed.posts", "inserted", inserted,
		"resumed", drift.resumed, "drifted", drift.drifted,
		"orphaned", len(drift.orphans),
		"skipped_no_members", skipped, "public", madePublic)
	r.postDrift = drift
	if msg := drift.summary(); msg != "" {
		fmt.Print(msg)
	}
	return nil
}

// postVisibility decides a seeded post's visibility tier (#1176).
//
// Every seeded post used to be written 'org-only', so an instance with
// public mode ON served anonymous visitors a 200 with zero items: the
// public tier existed in the CHECK constraint, was enforced and tested
// in the ACL, and was offered by the compose form, but nothing in the
// corpus ever used it. The anonymous wall — the whole point of public
// mode — could not be looked at.
//
// The rule is the DATASET'S OWN declaration, not an arbitrary hash
// slice: a post goes public when it declares sensitivity_tier 'public'
// AND its cover asset does too. Both halves are load-bearing.
//
//   - The post tier is what says this content is publishable at all.
//     ~24% of the corpus declares it; the other ~76% (team + restricted)
//     stays org-only, so team-scoped content remains the majority, which
//     is the ruling: uploaders get the OPTION of anonymous viewing, it
//     does not become the default.
//   - The COVER tier is what decides whether the card can be LOOKED at.
//     A member asset the viewer may not read is redacted per caller
//     (#883), which is correct — but when the redacted member is the
//     cover, the card renders as a placeholder with no image. Admitting
//     those would put a visibly broken tile on ~18% of the anonymous
//     wall. Requiring a public cover drops that to zero.
//
// Non-cover members are deliberately NOT constrained: ~47 of the posts
// this admits carry a team-tier member behind a public cover, so the
// anonymous redaction path finally has seed coverage instead of being a
// branch no fixture reaches.
//
// Measured against site_a: 164 of 847 posts (19.4%) go public.
func postVisibility(p manifestPost, coverTier string) string {
	if p.SensitivityTier == "public" && coverTier == "public" {
		return "public"
	}
	return "org-only"
}

// --- phase: collection post backfill ----------------------------------

// applyCollectionPostBackfill authors a post for every collection member
// asset the dataset left bare (#1185).
//
// A collection renders POSTS and nothing else now. `collection_resources`
// is still written by applyAssets — deliberately, see the note there —
// but since #1236 it feeds NOTHING that is drawn: the cover mosaic and
// the featured rail's item count were the last two surfaces reading it,
// and both now compose from `collection_posts` alone. So an asset pinned
// to a collection that no post in that collection frames vanishes from
// the page AND from the tile, which is why this backfill matters more
// than it did when the mosaic still papered over it. Measured
// against site_a that is 37 of Internet Reference's 118 members: the
// dataset's own generator groups loose assets into bundles keyed on
// (collection, team, asset_type) and leaves a tail behind, and 36 of those
// 37 ARE in a post — one filed under Project Echo. Membership of a
// collection and authorship of a post are different relations in this
// corpus, which is exactly why the gap exists and why the fix belongs
// here rather than in the dataset: a subset profile (`--profile ci`,
// `--limit-per-extension`) can widen it arbitrarily by dropping the
// members a post needed, and applyPosts then skips that post entirely.
//
// Composition:
//
//   - Members are grouped by `metadata.group_id`, the dataset's own
//     grouping key and the same one seed/scripts/sanitize_and_assemble.py
//     composes its `asset_group` posts from. Assets without one become
//     single-asset posts. In site_a every one of the 37 is groupless, so
//     the group arm is here for the subset profiles and for future
//     datasets, not for decoration.
//   - The author is the first member's OWNER, not the bootstrap admin. A
//     post is someone's work; attributing a backfilled one to `admin`
//     would put a wall of admin-authored posts on collections owned by
//     artists and make every "posts by this user" surface lie.
//   - VISIBILITY RULE: `public` iff EVERY member asset is sensitivity
//     `public`; otherwise `org-only`. This is stricter than
//     postVisibility's dataset rule, which constrains only the cover so
//     that ~47 posts carry a withheld member behind a public cover and
//     the anonymous redaction path (#883) gets seed coverage. That
//     coverage already exists and is deliberate; a backfilled post has no
//     authored `sensitivity_tier` to honour, so widening one to public on
//     the strength of its cover alone would be inventing a publication
//     decision nobody made.
//
// Post ids are stableUUID-derived, so a re-run re-composes the same posts
// and lands on SeedInsertPost's ON CONFLICT path rather than duplicating
// the wall.
func (r *Runner) applyCollectionPostBackfill(ctx context.Context, cat *catalogues) error {
	// covered[collection name] = manifest asset ids already framed by a
	// post PINNED IN THAT COLLECTION. Keyed on the collection because the
	// same asset can be framed by a post filed elsewhere, and that post
	// does not put it on this collection's wall.
	covered := make(map[string]map[string]struct{}, len(r.collections))
	for _, p := range cat.Posts {
		if p.CollectionName == "" {
			continue
		}
		// Only posts that were actually INSERTED count. applyPosts skips a
		// post whose members all fell out, and a skipped post frames
		// nothing.
		if _, ok := r.posts[p.ID]; !ok {
			continue
		}
		if _, ok := r.collections[p.CollectionName]; !ok {
			continue
		}
		set := covered[p.CollectionName]
		if set == nil {
			set = make(map[string]struct{})
			covered[p.CollectionName] = set
		}
		for _, aid := range p.AssetIDs {
			set[aid] = struct{}{}
		}
	}

	type bundle struct {
		collection string
		members    []manifestAsset
	}
	// Insertion order is the catalogue's, so the composed wall is stable
	// across runs — a map range would shuffle titles and sort_order.
	var order []string
	index := make(map[string]*bundle)
	for _, a := range cat.Assets {
		if a.CollectionName == "" {
			continue
		}
		if _, ok := r.collections[a.CollectionName]; !ok {
			continue
		}
		if _, ok := r.assets[a.ID]; !ok {
			continue // never inserted: missing bytes, or deduped on hash
		}
		if _, ok := covered[a.CollectionName][a.ID]; ok {
			continue
		}
		key := a.CollectionName + "\x00"
		if gid := assetGroupID(a); gid != "" {
			key += "g:" + gid
		} else {
			key += "a:" + a.ID
		}
		b := index[key]
		if b == nil {
			b = &bundle{collection: a.CollectionName}
			index[key] = b
			order = append(order, key)
		}
		b.members = append(b.members, a)
	}

	inserted, madePublic, bare := 0, 0, 0
	for _, key := range order {
		b := index[key]
		bare += len(b.members)
		first := b.members[0]

		authorRef, ok := r.users[first.OwnerUsername]
		if !ok {
			authorRef = r.adminRef
		}
		vis := "public"
		var tags []string
		for _, m := range b.members {
			if sensitivity(m.SensitivityTier) != "public" {
				vis = "org-only"
			}
			tags = append(tags, m.Tags...)
		}
		if vis == "public" {
			madePublic++
		}
		created, updated := r.rowTimes(first.CreatedAt, first.UpdatedAt)
		title := orDefault(first.Title, "Untitled")
		description := first.Description
		if len(b.members) > 1 {
			// ⛔ A SECOND TITLE GENERATOR (#1306). This backfill mints
			// post titles in Go, so the Python assembler's wording fix
			// left it producing exactly the shape the issue is about:
			// an em dash and a count that CardKindBadge already prints
			// beside it. Kept in step with
			// sanitize_and_assemble.title_group_set.
			title += " and variants"
			description = fmt.Sprintf("%s working set for %s. %d assets pulled together for review.",
				orDefault(first.TeamName, "Reference"), b.collection, len(b.members))
		}
		postID := stableUUID("collection-post-backfill", key)
		id, err := r.q.SeedInsertPost(ctx, SeedInsertPostParams{
			ID:            parseUUID(postID.String()),
			AuthorUserRef: authorRef,
			Title:         title,
			Description:   description,
			Visibility:    vis,
			CoverAssetID:  r.assets[first.ID],
			StateID:       r.postStates["published"],
			TeamID:        r.teamIDForName(first.TeamName),
			CreatedAt:     created,
			UpdatedAt:     updated,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Already composed by a prior run.
				continue
			}
			return fmt.Errorf("insert backfill post %s: %w", key, err)
		}
		for i, m := range b.members {
			if err := r.q.SeedInsertPostAsset(ctx, SeedInsertPostAssetParams{
				PostID: id, AssetID: r.assets[m.ID], SortOrder: int32(i),
			}); err != nil {
				return fmt.Errorf("backfill post asset %s: %w", key, err)
			}
		}
		for _, tag := range dedupStrings(tags) {
			if err := r.q.SeedInsertPostTag(ctx, SeedInsertPostTagParams{PostID: id, Tag: tag}); err != nil {
				return fmt.Errorf("backfill post tag %s: %w", key, err)
			}
		}
		if err := r.q.SeedInsertCollectionPost(ctx, SeedInsertCollectionPostParams{
			CollectionID: r.collections[b.collection], PostID: id,
		}); err != nil {
			return fmt.Errorf("backfill collection post %s: %w", key, err)
		}
		inserted++
	}
	r.log.Info("seed.collection_post_backfill",
		"bare_members", bare, "posts", inserted, "public", madePublic)
	return nil
}

// assetGroupID reads the dataset's grouping key out of an asset's opaque
// metadata blob. It is NOT a manifestAsset field: `metadata` is carried
// through to `assets.metadata` as raw jsonb, and `group_id` is one key
// inside it (1802 of site_a's 1947 assets carry one).
func assetGroupID(a manifestAsset) string {
	if len(a.Metadata) == 0 {
		return ""
	}
	var md struct {
		GroupID string `json:"group_id"`
	}
	if err := json.Unmarshal(a.Metadata, &md); err != nil {
		return ""
	}
	return md.GroupID
}

// --- phase: comments --------------------------------------------------

func (r *Runner) applyComments(ctx context.Context, cat *catalogues) error {
	// asset id -> first inserted post containing it.
	assetToPost := map[string]pgtype.UUID{}
	for _, p := range cat.Posts {
		postID, ok := r.posts[p.ID]
		if !ok {
			continue
		}
		for _, aid := range p.AssetIDs {
			if _, seen := assetToPost[aid]; !seen {
				if _, created := r.assets[aid]; created {
					assetToPost[aid] = postID
				}
			}
		}
	}

	forged := 0
	for _, a := range cat.Assets {
		notes := strings.TrimSpace(a.ReviewNotes)
		if notes == "" || a.ReviewerUsername == "" {
			continue
		}
		reviewerRef, ok := r.users[a.ReviewerUsername]
		if !ok {
			continue
		}
		postID, ok := assetToPost[a.ID]
		if !ok {
			continue
		}
		if _, ok := r.assets[a.ID]; !ok {
			continue
		}
		cid := stableUUID("comment", a.ID, uuidString(postID))
		created := r.rowTime(a.UpdatedAt)
		in := CommentInput{
			ID:            &cid,
			TargetKind:    CommentTargetPost,
			TargetID:      uuid.UUID(postID.Bytes),
			AuthorUserRef: reviewerRef,
			Body:          notes,
		}
		if created.Valid {
			t := created.Time
			in.CreatedAt = &t
		}
		if _, err := r.admin.CreateComment(ctx, nil, r.adminRef, in); err != nil {
			return fmt.Errorf("forge comment for asset %s: %w", a.ID, err)
		}
		forged++
	}
	r.log.Info("seed.comments", "forged", forged)
	return nil
}

// --- phase: verify ----------------------------------------------------

func (r *Runner) verify(ctx context.Context) (Counts, error) {
	var c Counts
	q := func(sql string) (int, error) {
		var n int
		err := r.pool.QueryRow(ctx, sql).Scan(&n)
		return n, err
	}
	var err error
	if c.Users, err = q(`SELECT count(*) FROM "user"`); err != nil {
		return c, err
	}
	if c.Teams, err = q(`SELECT count(*) FROM teams WHERE deleted_at IS NULL`); err != nil {
		return c, err
	}
	if c.Collections, err = q(`SELECT count(*) FROM collections WHERE deleted_at IS NULL`); err != nil {
		return c, err
	}
	if c.Assets, err = q(`SELECT count(*) FROM assets WHERE deleted_at IS NULL`); err != nil {
		return c, err
	}
	if c.Posts, err = q(`SELECT count(*) FROM posts WHERE deleted_at IS NULL`); err != nil {
		return c, err
	}
	if c.Comments, err = q(`SELECT count(*) FROM comments WHERE deleted_at IS NULL`); err != nil {
		return c, err
	}
	if c.Follows, err = q(`SELECT count(*) FROM user_follows`); err != nil {
		return c, err
	}
	if c.Likes, err = q(`SELECT count(*) FROM likes`); err != nil {
		return c, err
	}
	if c.Featured, err = q(`SELECT count(*) FROM featured_items`); err != nil {
		return c, err
	}
	return c, nil
}

// --- helpers ----------------------------------------------------------

func (r *Runner) resolveAssetState(seedState string) pgtype.UUID {
	alias := map[string]string{
		"draft": "draft", "in_review": "pending_review",
		"approved": "published", "final": "published", "archived": "archived",
	}[seedState]
	if alias == "" {
		alias = "published"
	}
	if id, ok := r.assetStates[alias]; ok {
		return id
	}
	return pgtype.UUID{}
}

func (r *Runner) teamIDForName(name string) pgtype.UUID {
	if name == "" {
		return pgtype.UUID{}
	}
	if id, ok := r.teams[slugify(name)]; ok {
		return id
	}
	return pgtype.UUID{}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" {
		s = "team"
	}
	return s
}

func assetStatus(archiveState string) string {
	switch archiveState {
	case "active":
		return "active"
	case "archived":
		return "archived"
	default:
		return "draft"
	}
}

func sensitivity(tier string) string {
	switch tier {
	case "public", "team", "restricted", "embargo":
		return tier
	default:
		return "public"
	}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func parseUUID(s string) pgtype.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: u, Valid: true}
}

func uuidString(u pgtype.UUID) string {
	return uuid.UUID(u.Bytes).String()
}

func parseTime(s string) pgtype.Timestamptz {
	if s == "" {
		return pgtype.Timestamptz{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// clampToPast maps a dataset timestamp onto the closed past (#1174).
//
// The site datasets scatter created_at/updated_at around the date they
// were generated in BOTH directions, so a fresh seed lands rows dated
// months ahead of the machine seeding them: 36 posts and 155 assets out
// to 2026-12-14 when the bug was found. Future-dated rows sit pinned at
// the top of every Newest sort until their date arrives (a permanently
// static feed head), display dates that have not happened, and quietly
// widen any date-window logic tested against the seed.
//
// Timestamps already in the past are returned UNCHANGED — that is the
// overwhelming majority of the corpus, so the seed keeps its dataset
// dates and stays byte-reproducible run to run. Only the overshoot is
// rewritten, and it is REFLECTED rather than pinned to `now`: a row
// four months ahead becomes four months old. Pinning would collapse
// every future row onto one instant, which trades the future-dates bug
// for a clump of 155 identically-dated rows sitting at the head of the
// feed — the same static-head symptom the fix exists to remove.
// Reflection spreads them back across the corpus's own date range.
//
// Reflection reverses ORDER within the reflected set, so a row's
// created_at can come out later than its updated_at; callers pair this
// with an updated >= created normalisation (see rowTimes).
func clampToPast(t, now time.Time) time.Time {
	if !t.After(now) {
		return t
	}
	return now.Add(-t.Sub(now))
}

// rowTime parses a dataset timestamp destined for a ROW's created_at /
// updated_at / liked_at and clamps it to the generation instant.
//
// It is deliberately NOT folded into parseTime: parseTime also reads
// metadata FIELD values (a `datetime` field's contents, parseDateValue),
// which are user data and legitimately hold future dates — a shoot date,
// a licence expiry. Clamping those would corrupt the value.
func (r *Runner) rowTime(s string) pgtype.Timestamptz {
	ts := parseTime(s)
	if !ts.Valid {
		return ts
	}
	return pgtype.Timestamptz{Time: clampToPast(ts.Time, r.genTime), Valid: true}
}

// rowTimes clamps a row's created/updated pair and restores the
// invariant that a row is not updated before it exists — which both an
// absent updated_at and clampToPast's order reversal can break.
func (r *Runner) rowTimes(createdAt, updatedAt string) (created, updated pgtype.Timestamptz) {
	created = r.rowTime(createdAt)
	updated = r.rowTime(updatedAt)
	if !updated.Valid {
		updated = created
	}
	if created.Valid && updated.Valid && updated.Time.Before(created.Time) {
		updated = created
	}
	return created, updated
}

// datesSurviveTheClamp reports whether this run would write a post's
// catalogue timestamps VERBATIM, which is the only case in which they
// can be compared against a row an earlier run wrote (#1320).
//
// clampToPast REFLECTS a future-dated timestamp around the instant the
// run started, so a catalogue date still in the future produces a
// different stored value on every run, by design. Comparing it would
// report drift on two correct seeds. The site_a catalogue carries dates
// out to 2026-12-14, so this is dozens of posts, not a corner.
func (r *Runner) datesSurviveTheClamp(createdAt, updatedAt string) bool {
	for _, s := range [2]string{createdAt, updatedAt} {
		if ts := parseTime(s); ts.Valid && ts.Time.After(r.genTime) {
			return false
		}
	}
	return true
}

// dateOnlyLayout is the calendar date a `date`-typed field accepts in
// addition to RFC3339. Midnight UTC, matching what the API's own date
// writer stores.
const dateOnlyLayout = "2006-01-02"

// parseDateValue parses a `date` field's manifest value: RFC3339 first
// (unchanged, so every existing timestamp keeps working), then the bare
// calendar date. Used ONLY for `date` — see fieldValueParams for why
// `datetime` stays strict.
func parseDateValue(s string) pgtype.Timestamptz {
	if ts := parseTime(s); ts.Valid {
		return ts
	}
	t, err := time.Parse(dateOnlyLayout, s)
	if err != nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// --- social history (#563): follows, likes, threaded post comments ---
//
// All three phases build a DETERMINISTIC graph from a stable hash of the
// natural keys (same shape as stableUUID) so re-seeds — and both sites —
// are byte-reproducible. Timestamps are distributed across the content's
// created_at span (earliest → latest post) rather than now(), so history
// reads as lived-in AND stays reproducible (now() would drift per run).

// stableHash: 64-bit digest over a fixed namespace + the parts. The
// engine behind the deterministic counts, picks, and timestamps below.
func stableHash(parts ...string) uint64 {
	h := sha256.New()
	h.Write([]byte("artist-alley.seed.hist.v1"))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

// stableIntn returns a deterministic int in [0, n).
func stableIntn(n int, parts ...string) int {
	if n <= 0 {
		return 0
	}
	return int(stableHash(parts...) % uint64(n))
}

// stableFrac returns a deterministic float in [0, 1).
func stableFrac(parts ...string) float64 {
	return float64(stableHash(parts...)) / (float64(math.MaxUint64) + 1)
}

// stableTimeBetween returns a deterministic instant in [lo, hi].
func stableTimeBetween(lo, hi time.Time, parts ...string) time.Time {
	if !hi.After(lo) {
		return lo
	}
	return lo.Add(time.Duration(stableFrac(parts...) * float64(hi.Sub(lo))))
}

// sortedUsernames returns the fictional-user natural keys in a stable
// order so map iteration never leaks into the graph shape.
func (r *Runner) sortedUsernames() []string {
	names := make([]string, 0, len(r.users))
	for u := range r.users {
		names = append(names, u)
	}
	sort.Strings(names)
	return names
}

// contentSpan is the [earliest, latest] post created_at window. Follow /
// like / comment timestamps are distributed inside it (dataset-derived,
// so reproducible — no now()).
//
// It reads the CLAMPED post dates (#1174), which is what makes the whole
// derived social graph inherit the clamp: hi is the newest post the
// seeder actually wrote, so every follow, like and comment placed inside
// the span lands at or before the generation instant. Reading the raw
// dataset dates here would leave the derived rows in the future even
// after the posts themselves were pulled back.
func (r *Runner) contentSpan(cat *catalogues) (time.Time, time.Time) {
	var lo, hi time.Time
	for _, p := range cat.Posts {
		t := r.rowTime(p.CreatedAt)
		if !t.Valid {
			continue
		}
		if lo.IsZero() || t.Time.Before(lo) {
			lo = t.Time
		}
		if t.Time.After(hi) {
			hi = t.Time
		}
	}
	if lo.IsZero() {
		lo = hi.AddDate(-1, 0, 0)
	}
	if !hi.After(lo) {
		hi = lo.AddDate(0, 1, 0)
	}
	return lo, hi
}

// pickDistinct deterministically selects up to count names from pool
// (excluding `exclude`), ordered by a salted hash — a stable shuffle.
func pickDistinct(pool []string, count int, exclude string, salt ...string) []string {
	type scored struct {
		name string
		h    uint64
	}
	scoredNames := make([]scored, 0, len(pool))
	for _, name := range pool {
		if name == exclude {
			continue
		}
		scoredNames = append(scoredNames, scored{name, stableHash(append(append([]string{}, salt...), name)...)})
	}
	sort.Slice(scoredNames, func(i, j int) bool { return scoredNames[i].h < scoredNames[j].h })
	if count > len(scoredNames) {
		count = len(scoredNames)
	}
	out := make([]string, count)
	for i := 0; i < count; i++ {
		out[i] = scoredNames[i].name
	}
	return out
}

// Generic, IP-clean art-feedback lines (per the IP-scrub policy — no
// real-property references). Picked deterministically per comment.
var seedPostComments = []string{
	"Love the composition here.",
	"The lighting really sells the mood.",
	"Nice restraint on the palette.",
	"This reads great even at thumbnail size.",
	"The silhouette is super clear — easy to parse.",
	"Textures are doing a lot of work here.",
	"Great sense of depth in this one.",
	"The framing draws the eye right where it wants to go.",
	"Really clean linework.",
	"That focal point lands perfectly.",
	"The colour story holds together top to bottom.",
	"Strong values — bet this reads in greyscale too.",
}

var seedPostReplies = []string{
	"Agreed — the balance is spot on.",
	"Same, the mood is the standout for me.",
	"Good call, hadn't clocked that.",
	"This. The read is effortless.",
	"Yeah, the palette choice makes it.",
	"Nice catch — the depth is subtle but it's there.",
}

// --- phase: follows ---------------------------------------------------
//
// Deterministic follow graph over the fictional users. A directed RING
// guarantees every user has ≥1 follower AND ≥1 followee; skewed extras
// then bias toward the sorted head so some users read as "popular".
func (r *Runner) applyFollows(ctx context.Context, cat *catalogues) error {
	names := r.sortedUsernames()
	if len(names) < 2 {
		r.log.Info("seed.follows", "inserted", 0, "note", "need ≥2 users")
		return nil
	}
	lo, hi := r.contentSpan(cat)
	edges := followEdges(names)
	inserted := 0
	for _, e := range edges {
		follower, followee := e[0], e[1]
		ts := stableTimeBetween(lo, hi, "follow", follower, followee)
		if err := r.q.SeedInsertFollow(ctx, SeedInsertFollowParams{
			FollowerUserRef: r.users[follower],
			FolloweeUserRef: r.users[followee],
			CreatedAt:       pgtype.Timestamptz{Time: ts, Valid: true},
		}); err != nil {
			return fmt.Errorf("follow %s->%s: %w", follower, followee, err)
		}
		inserted++
	}
	r.log.Info("seed.follows", "inserted", inserted, "users", len(names))
	return nil
}

// followEdges is the PURE follow-graph computation (deterministic, no DB)
// so the "every user has ≥1 follower AND ≥1 followee" invariant and
// reproducibility are unit-testable. `names` must be pre-sorted.
//
// A directed RING (u[i]→u[i+1]) gives every node exactly one guaranteed
// follower + followee; skewed extras (2–6 per user, min-of-two draws)
// then bias toward the sorted head so early users read as "popular".
// Returns deduplicated [follower, followee] pairs; self-edges dropped.
func followEdges(names []string) [][2]string {
	n := len(names)
	if n < 2 {
		return nil
	}
	seen := make(map[[2]string]struct{})
	var edges [][2]string
	add := func(follower, followee string) {
		if follower == followee {
			return
		}
		key := [2]string{follower, followee}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		edges = append(edges, key)
	}
	// Ring base.
	for i, u := range names {
		add(u, names[(i+1)%n])
	}
	// Skewed extras.
	for i, u := range names {
		extra := 2 + stableIntn(5, "followcnt", u)
		for j := 0; j < extra; j++ {
			js := strconv.Itoa(j)
			a := stableIntn(n, "followa", u, js)
			b := stableIntn(n, "followb", u, js)
			idx := a
			if b < idx {
				idx = b
			}
			if idx == i {
				idx = (idx + 1) % n
			}
			add(u, names[idx])
		}
	}
	return edges
}

// --- phase: likes -----------------------------------------------------
//
// Like ROWS per post, only from users who can see it. Seed posts are
// org-only or public (applyPosts, #1176) → both are visible to every
// fictional user (the walled garden, plus everyone outside it), so the
// eligible pool is all users minus the author; a private post (none in
// the dataset today) would collapse to the author. The
// likes_after_insert trigger keeps posts.like_count == the row count.
func (r *Runner) applyLikes(ctx context.Context, cat *catalogues) error {
	names := r.sortedUsernames()
	if len(names) < 2 {
		r.log.Info("seed.likes", "inserted", 0, "note", "need ≥2 users")
		return nil
	}
	_, hi := r.contentSpan(cat)
	inserted := 0
	for _, p := range cat.Posts {
		postID, ok := r.posts[p.ID]
		if !ok {
			continue
		}
		// Eligible viewers = who can see the post. Seed posts are
		// org-only or public (applyPosts) ⇒ every fictional user can see
		// every post; the author is excluded from their own likes below.
		// If the seed ever grows private/followers-gated posts, the
		// eligible pool would narrow here (ADR 0010) — today it's the
		// whole membership.
		eligible := names
		// Skewed like count: a baseline for every post, plus a big bump
		// for ~1-in-5 "popular" posts.
		count := 1 + stableIntn(5, "likebase", p.ID)
		if stableIntn(5, "likepop", p.ID) == 0 {
			count += 8 + stableIntn(12, "likebonus", p.ID)
		}
		postCreated := r.rowTime(p.CreatedAt).Time
		for _, liker := range pickDistinct(eligible, count, p.AuthorUsername, "like", p.ID) {
			ref := r.users[liker]
			ts := stableTimeBetween(postCreated, hi, "likeat", p.ID, liker)
			if ts.Before(postCreated) {
				ts = postCreated
			}
			if err := r.q.SeedInsertLike(ctx, SeedInsertLikeParams{
				TargetKind: "post",
				TargetID:   postID,
				UserRef:    &ref,
				LikedAt:    pgtype.Timestamptz{Time: ts, Valid: true},
			}); err != nil {
				return fmt.Errorf("like post %s by %s: %w", p.ID, liker, err)
			}
			inserted++
		}
	}
	r.log.Info("seed.likes", "inserted", inserted)
	return nil
}

// --- phase: post comments ---------------------------------------------
//
// Threaded post DISCUSSION (distinct from applyComments' one-per-asset
// review notes): 0–3 top-level comments per post from varied non-author
// users, each with a ~1-in-3 chance of a threaded reply. The
// comments_after_insert trigger maintains posts.comment_count.
func (r *Runner) applyPostComments(ctx context.Context, cat *catalogues) error {
	names := r.sortedUsernames()
	if len(names) < 2 {
		r.log.Info("seed.post_comments", "made", 0, "note", "need ≥2 users")
		return nil
	}
	_, hi := r.contentSpan(cat)
	made := 0
	for _, p := range cat.Posts {
		postID, ok := r.posts[p.ID]
		if !ok {
			continue
		}
		postCreated := r.rowTime(p.CreatedAt).Time
		nTop := stableIntn(4, "cmtcnt", p.ID) // 0..3
		for ci, author := range pickDistinct(names, nTop, p.AuthorUsername, "cmt", p.ID) {
			cis := strconv.Itoa(ci)
			cid := stableUUID("postcmt", p.ID, author)
			ts := stableTimeBetween(postCreated, hi, "cmtat", p.ID, author)
			if ts.Before(postCreated) {
				ts = postCreated
			}
			body := seedPostComments[stableIntn(len(seedPostComments), "cmtbody", p.ID, author)]
			authorRef := r.users[author]
			if _, err := r.admin.CreateComment(ctx, nil, authorRef, CommentInput{
				ID:            &cid,
				TargetKind:    CommentTargetPost,
				TargetID:      uuid.UUID(postID.Bytes),
				AuthorUserRef: authorRef,
				Body:          body,
				CreatedAt:     &ts,
			}); err != nil {
				return fmt.Errorf("post comment %s by %s: %w", p.ID, author, err)
			}
			made++

			// ~1-in-3 top-level comments get one threaded reply from a
			// different user.
			if stableIntn(3, "cmtreply", p.ID, author, cis) != 0 {
				continue
			}
			repliers := pickDistinct(names, 1, author, "cmtreplier", p.ID, author)
			if len(repliers) == 0 {
				continue
			}
			replier := repliers[0]
			rid := stableUUID("postreply", p.ID, author, replier)
			rts := stableTimeBetween(ts, hi, "replyat", p.ID, author, replier)
			if rts.Before(ts) {
				rts = ts
			}
			rbody := seedPostReplies[stableIntn(len(seedPostReplies), "replybody", p.ID, author, replier)]
			replierRef := r.users[replier]
			if _, err := r.admin.CreateComment(ctx, nil, replierRef, CommentInput{
				ID:            &rid,
				TargetKind:    CommentTargetPost,
				TargetID:      uuid.UUID(postID.Bytes),
				ParentID:      &cid,
				AuthorUserRef: replierRef,
				Body:          rbody,
				CreatedAt:     &rts,
			}); err != nil {
				return fmt.Errorf("post reply %s by %s: %w", p.ID, replier, err)
			}
			made++
		}
	}
	r.log.Info("seed.post_comments", "made", made)
	return nil
}

// stableUUID mirrors apply.py's _stable_uuid: sha256 over a fixed
// namespace + the parts, sliced into UUID text form. Deterministic so
// re-runs hit the comment ON CONFLICT path.
func stableUUID(parts ...string) uuid.UUID {
	h := sha256.New()
	h.Write([]byte("artist-alley.seed.v1"))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	sum := h.Sum(nil)
	var u uuid.UUID
	copy(u[:], sum[:16])
	return u
}

// fieldValueParams translates a raw JSON field value into the
// asset_field_value column set based on the field's declared type
// (mirrors apply.py._field_value_body + metadata handler.go).
func fieldValueParams(ftype string, raw any) (SeedInsertAssetFieldValueParams, bool) {
	var p SeedInsertAssetFieldValueParams
	if raw == nil {
		return p, false
	}
	switch strings.ToLower(ftype) {
	case "text", "longtext", "rich_text", "select", "tree":
		// A `tree` value in a dataset MANIFEST is a single option slug
		// — the leaf node — not a "NA/US/CA" path and not an array.
		// Slugs are unique across the whole option tree, so the leaf
		// addresses the node on its own and the ancestor path is
		// reassembled at read time. See the 2026-07-31 tree-storage
		// amendment to ADR 0012.
		s := scalarString(raw)
		// SeedInsertAssetFieldValue goes straight at the table, so no
		// handler gate runs on a seeded value. The read side sanitises
		// regardless (#816), but a dataset should not be able to leave
		// a live payload sitting in a column either.
		p.ValueText = richtext.SanitizeValueText(strings.ToLower(ftype), &s)
	case "number":
		f, ok := raw.(float64)
		if !ok {
			return p, false
		}
		p.ValueNum = &f
	case "boolean":
		var n float64
		if b, ok := raw.(bool); ok && b {
			n = 1
		}
		p.ValueNum = &n
	case "date":
		// A `date` field takes the obvious spelling — "2026-03-14" —
		// as well as a full RFC3339 timestamp (#807). It used to take
		// RFC3339 only, so the obvious spelling was DISCARDED without a
		// word; the only reason #805's manifests avoided it is that
		// their author read parseTime first.
		p.ValueDate = parseDateValue(scalarString(raw))
		if !p.ValueDate.Valid {
			return p, false
		}
	case "datetime":
		// Deliberately NOT widened to the bare date. A datetime field
		// handed a date has lost its time of day somewhere upstream,
		// which is worth reporting rather than papering over with
		// midnight UTC.
		p.ValueDate = parseTime(scalarString(raw))
		if !p.ValueDate.Valid {
			return p, false
		}
	case "multi_select":
		var opts []string
		switch v := raw.(type) {
		case []any:
			for _, e := range v {
				if e != nil {
					opts = append(opts, scalarString(e))
				}
			}
		default:
			opts = []string{scalarString(raw)}
		}
		if len(opts) == 0 {
			return p, false
		}
		p.ValueOptions = opts
	case "reference":
		// An unparseable UUID used to fall through as ACCEPTED, which
		// wrote a row with every value column NULL — a field that reads
		// as "set to nothing" rather than as absent, and the one drop
		// shape that even a row count could not catch (#807). Refuse it
		// so the caller warns.
		ref := parseUUID(scalarString(raw))
		if !ref.Valid {
			return p, false
		}
		p.ValueRef = ref
	default:
		s := scalarString(raw)
		p.ValueText = &s
	}
	return p, true
}

func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", v)
	}
}
