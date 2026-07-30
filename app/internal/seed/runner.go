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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/preview/format3d"
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

	// Previews enqueues a preview job per seeded asset (#355). Without
	// it a seeded instance has originals and zero derivatives — no card
	// thumbnails (`col`), no video hover sprites — which is what the
	// HTTP-era apply.py got for free by uploading through the API.
	//
	// Jobs are enqueued at PriorityBackfil so a bulk seed can never
	// preempt interactive work; the running app's worker pool drains
	// them. Set false for a fast metadata-only seed.
	Previews bool
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
		admin:   NewAdminHandler(pool, nil, nil, nil, nil, nil),
		// Enqueue-only Service: the seeder never runs jobs, it just
		// inserts rows for the serving process's pool to drain. A nil
		// Registry is fine — Enqueue doesn't consult it.
		jobs:        jobs.NewService(pool, opts.Logger, nil),
		log:         opts.Logger,
		opts:        opts,
		assetStates: map[string]pgtype.UUID{},
		postStates:  map[string]pgtype.UUID{},
		assetTypes:  map[string]int64{},
		users:       map[string]int64{},
		teams:       map[string]pgtype.UUID{},
		fields:      map[string]fieldMeta{},
		collections: map[string]pgtype.UUID{},
		assets:      map[string]pgtype.UUID{},
		posts:       map[string]pgtype.UUID{},
	}
}

// Run loads the catalogues + manifest and executes every phase in
// order, returning the verify counts.
func (r *Runner) Run(ctx context.Context) (Counts, error) {
	cat, err := loadCatalogues(r.opts.CatalogueRoot, r.opts.SiteRoot)
	if err != nil {
		return Counts{}, err
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
		{"applyLikes", r.applyLikes},
		{"applyComments", r.applyComments},
		{"applyPostComments", r.applyPostComments},
	}
	for _, p := range phases {
		start := time.Now()
		r.log.Info("seed.phase.start", "phase", p.name)
		if err := p.fn(ctx, cat); err != nil {
			return Counts{}, fmt.Errorf("phase %s: %w", p.name, err)
		}
		r.log.Info("seed.phase.done", "phase", p.name, "elapsed", time.Since(start).String())
	}
	return r.verify(ctx)
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
	for _, f := range cat.Fields {
		opts := []byte("{}")
		if len(f.Options) > 0 {
			b, err := json.Marshal(map[string]any{"values": f.Options})
			if err != nil {
				return err
			}
			opts = b
		}
		id, err := r.q.SeedInsertField(ctx, SeedInsertFieldParams{
			Code:             f.Name,
			Label:            f.Label,
			Type:             f.Type,
			Options:          opts,
			ExtractionSource: f.ExtractionSource,
			ExtractionMode:   f.ExtractionMode,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				id, err = r.q.SeedGetFieldByCode(ctx, f.Name)
			}
			if err != nil {
				return fmt.Errorf("create field %s: %w", f.Name, err)
			}
		}
		r.fields[f.Name] = fieldMeta{id: id, typ: f.Type}
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
			Description:  c.Name + " — seeded collection",
			Visibility:   vis,
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
	inserted, deduped, missing, queued := 0, 0, 0, 0
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
		created := parseTime(a.CreatedAt)
		updated := parseTime(a.UpdatedAt)
		if !updated.Valid {
			updated = created
		}
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
			CreatedAt:     created,
			UpdatedAt:     updated,
		}
		id, err := r.q.SeedInsertAsset(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			// Either the id already exists (resumed run) or the
			// (owner_user_ref, file_hash) unique index collapsed a
			// byte-identical sibling this owner already holds — e.g.
			// the same texture exported next to both the OBJ and FBX
			// of a model. That collapse is the CORRECT behaviour: it
			// mirrors the real app refusing a re-upload of identical
			// bytes by the same owner. Skip the duplicate; posts that
			// reference only it fall out as no-member posts.
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
			if _, jErr := r.jobs.Enqueue(ctx,
				dispatch.JobTypeForExt(&ext),
				map[string]string{
					"asset_id":       id.String(),
					"file_hash":      hash,
					"file_extension": ext,
				},
				jobs.EnqueueOpts{Priority: &previewPriority},
			); jErr != nil {
				r.log.Warn("seed.preview.enqueue_failed", "asset", a.ID, "err", jErr.Error())
			} else {
				queued++
			}
		}

		// tags
		for _, tag := range dedupStrings(a.Tags) {
			if err := r.q.SeedInsertAssetTag(ctx, SeedInsertAssetTagParams{AssetID: id, Tag: tag}); err != nil {
				return fmt.Errorf("asset tag %s: %w", a.ID, err)
			}
		}
		// collection membership
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
	r.log.Info("seed.assets", "inserted", inserted,
		"deduped", deduped, "missing", missing, "previews_queued", queued)
	return nil
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
			continue
		}
		params, ok := fieldValueParams(fm.typ, vals[code])
		if !ok {
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
	inserted, skipped := 0, 0
	for _, p := range cat.Posts {
		// Resolve members from inserted assets (apply.py "any member" rule).
		var members []pgtype.UUID
		for _, aid := range p.AssetIDs {
			if sid, ok := r.assets[aid]; ok {
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
		created := parseTime(p.CreatedAt)
		updated := parseTime(p.UpdatedAt)
		if !updated.Valid {
			updated = created
		}
		cover := members[0]
		id, err := r.q.SeedInsertPost(ctx, SeedInsertPostParams{
			ID:            parseUUID(p.ID),
			AuthorUserRef: authorRef,
			Title:         orDefault(p.Title, "Untitled"),
			Description:   p.Description,
			Visibility:    "org-only",
			CoverAssetID:  cover,
			StateID:       r.postStates["published"],
			TeamID:        r.teamIDForName(p.TeamName),
			CreatedAt:     created,
			UpdatedAt:     updated,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Already inserted in a prior run.
				r.posts[p.ID] = parseUUID(p.ID)
				continue
			}
			return fmt.Errorf("insert post %s: %w", p.ID, err)
		}
		r.posts[p.ID] = id
		for i, m := range members {
			if err := r.q.SeedInsertPostAsset(ctx, SeedInsertPostAssetParams{
				PostID: id, AssetID: m, SortOrder: int32(i),
			}); err != nil {
				return fmt.Errorf("post asset %s: %w", p.ID, err)
			}
		}
		for _, tag := range dedupStrings(p.Tags) {
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
	r.log.Info("seed.posts", "inserted", inserted, "skipped_no_members", skipped)
	return nil
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
		created := parseTime(a.UpdatedAt)
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
func (r *Runner) contentSpan(cat *catalogues) (time.Time, time.Time) {
	var lo, hi time.Time
	for _, p := range cat.Posts {
		t := parseTime(p.CreatedAt)
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
// Like ROWS per post, only from users who can see it. Seed posts are all
// org-only (applyPosts) → visible to every fictional user (the walled
// garden), so the eligible pool is all users minus the author; a private
// post (none in the dataset today) would collapse to the author. The
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
		// Eligible viewers = who can see the post. Seed posts are all
		// org-only (applyPosts) ⇒ every fictional user can see every post;
		// the author is excluded from their own likes below. If the seed
		// ever grows private/followers-gated posts, the eligible pool
		// would narrow here (ADR 0010) — today it's the whole membership.
		eligible := names
		// Skewed like count: a baseline for every post, plus a big bump
		// for ~1-in-5 "popular" posts.
		count := 1 + stableIntn(5, "likebase", p.ID)
		if stableIntn(5, "likepop", p.ID) == 0 {
			count += 8 + stableIntn(12, "likebonus", p.ID)
		}
		postCreated := parseTime(p.CreatedAt).Time
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
		postCreated := parseTime(p.CreatedAt).Time
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
		s := scalarString(raw)
		p.ValueText = &s
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
	case "date", "datetime":
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
		p.ValueRef = parseUUID(scalarString(raw))
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
