// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package seed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// verify.go: did the catalogue MATERIALIZE? (#1319)
//
// `aa seed` reports counts, and a count is the wrong instrument for the
// question this file answers. A reseed over a changed catalogue reports
// the same asset count as a reseed over an unchanged one, because the
// rows are all there; it is the VALUES inside them that can be stale,
// dropped, or written by something other than the seed. Three of those
// have already happened silently: a field value dropped with a bare
// `continue` (#807), a value written into the wrong column because the
// catalogue's type was trusted over the row's (#812), and a post row
// left at whatever an earlier seed put there (#1320).
//
// So this reads the same inputs the seeder read (MANIFEST.json and
// posts.json from the site root, the catalogue directory) and asks, for
// every value the catalogue carries, whether the database holds it
// exactly as the seeder would have written it. It writes nothing.
//
// THE OWNERSHIP AXIS IS `set_by`, NOT THE FIELD CODE. The seed writes
// every profile value with set_by='import' (queries.sql
// SeedInsertAssetFieldValue) and it is the only writer that does. Other
// writers own other rows on the same table: pixeldims.Record writes
// pixel_width/pixel_height with set_by='computed', the upload defaults
// write 'default', humans and API clients write 'manual'/'api'. So the
// rule is two-sided over PROVENANCE, never over a list of codes:
//
//   - every profile value must be present under set_by='import' with the
//     typed column equal to what fieldValueParams derives (rule A);
//   - every set_by='import' row on a catalogue asset must be a profile
//     value (rule B, the other direction), while rows under any other
//     provenance are reported by count and never failed merely for
//     being absent from the profile;
//   - a profile value whose only row carries another provenance is a
//     FAILURE, not a pass (rule C): pixeldims.Record and
//     UpsertAssetFieldValue both `ON CONFLICT DO UPDATE` and rewrite
//     set_by, so a derived or edited row can sit where the seed's value
//     should be, and a verifier that accepted it would be proving the
//     wrong writer's work.
//
// The three drop counters (unknown_code, value_rejected, type_mismatch)
// are recomputed here read-only by the seeder's own rules, so the
// verifier does not depend on a log line surviving.
//
// ⚠️ THE CONTENT-ADDRESS COLLAPSE IS DIAGNOSED, NEVER EXCUSED.
// SeedInsertAsset's ON CONFLICT DO NOTHING also catches the
// (owner_user_ref, file_hash) partial unique index: a manifest entry
// whose bytes are identical to a sibling the same owner already holds
// gets NO row of its own (measured on the coding stack: site_a seeds
// 2,004 rows from 2,005 entries, deduped=1), and its field values are
// never written. That explains the absence; it does not make the
// profile materialize. The asset id, its declaration, its size and its
// field values are all missing under that id, so the verifier FAILS the
// asset and every one of its expected values, and adds the sibling as
// the diagnosis. Whether a collapse may ever be accepted is a ruling
// this package does not make: matching bytes are not approval.

// VerifyOptions configures Verify.
type VerifyOptions struct {
	SiteRoot      string // MANIFEST.json + posts.json + bytes, as seeded
	CatalogueRoot string // seed/profiles

	// MigrationDocument is an optional post-id migration document
	// (seed/upgrades/post-id-migration.<stem>.json). When given, every
	// new_id must be a live post and every old_id must not be.
	MigrationDocument string

	// ExpectOnce lists post ids that must appear exactly once in the
	// site's posts.json and be live. Supplied, never hard-wired: the
	// four site_b ids that were published twice are the operator's
	// expectation, not this package's knowledge.
	ExpectOnce []string

	// ExpectAssets pins ai_provenance and byte size on named assets.
	ExpectAssets []AssetExpectation

	Logger *slog.Logger
}

// AssetExpectation is one supplied asset fact: the plates' `none` /
// `assisted` declarations and their sizes are the case it exists for.
type AssetExpectation struct {
	ID           string
	AiProvenance string // "" = undeclared (NULL)
	SizeBytes    int64  // 0 = not checked
}

// VerifyReport is every verdict, plus the counts the acceptance reads.
type VerifyReport struct {
	Failures []string
	Notes    []string

	// Drop and mismatch accounting, recomputed by the seeder's rules.
	UnknownCode   int
	ValueRejected int
	TypeMismatch  int

	// Field values.
	ExpectedValues int            // profile-derived (asset, field) pairs
	ImportRows     int            // set_by='import' rows found on catalogue assets
	Provenance     map[string]int // rows on catalogue assets by non-import set_by

	// Assets and posts.
	Assets           int // manifest entries
	AssetsPresent    int
	AssetsCollapsed  int // absent AND explained by a same-owner sibling holding the same bytes; still failures
	Posts            int // catalogue posts
	PostsPresent     int
	PostsSkipped     int // catalogue posts with no resolvable member (the seed skips them; a failure here)
	ExpectedBackfill int // backfill posts the seed would mint for this state, exactly
	LiveBackfill     int // of those, live
	LiveExtra        int // live posts neither in the catalogue nor an expected backfill
}

// OK reports whether every invariant held.
func (rep *VerifyReport) OK() bool { return len(rep.Failures) == 0 }

func (rep *VerifyReport) fail(format string, args ...any) {
	rep.Failures = append(rep.Failures, fmt.Sprintf(format, args...))
}

func (rep *VerifyReport) note(format string, args ...any) {
	rep.Notes = append(rep.Notes, fmt.Sprintf(format, args...))
}

// Summary is the one paragraph an operator reads.
func (rep *VerifyReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "assets: %d in the manifest, %d present, %d absent (%d of them collapsed by content address onto a sibling; diagnosed, still failures)\n",
		rep.Assets, rep.AssetsPresent, rep.Assets-rep.AssetsPresent, rep.AssetsCollapsed)
	fmt.Fprintf(&b, "field values: %d expected from the profile, %d import rows found",
		rep.ExpectedValues, rep.ImportRows)
	if len(rep.Provenance) > 0 {
		keys := make([]string, 0, len(rep.Provenance))
		for k := range rep.Provenance {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, rep.Provenance[k]))
		}
		fmt.Fprintf(&b, "; other provenance (informational): %s", strings.Join(parts, " "))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "seed.field.drops (recomputed): unknown_code=%d value_rejected=%d type_mismatch=%d\n",
		rep.UnknownCode, rep.ValueRejected, rep.TypeMismatch)
	fmt.Fprintf(&b, "posts: %d in the catalogue, %d present, %d with no resolvable member; backfill: %d expected, %d live; %d unexplained live post(s)\n",
		rep.Posts, rep.PostsPresent, rep.PostsSkipped, rep.ExpectedBackfill, rep.LiveBackfill, rep.LiveExtra)
	if rep.OK() {
		fmt.Fprintf(&b, "RESULT: VERIFIED (0 failed)\n")
	} else {
		fmt.Fprintf(&b, "RESULT: FAILED (%d failed)\n", len(rep.Failures))
	}
	return b.String()
}

// migrationDocument is the subset of post-id-migration.<stem>.json the
// verifier reads. The one-to-one validation lives in the Python guard
// (manifest_guard.load_migration_document); here the document is a
// list of facts about which ids should and should not be live.
type migrationDocument struct {
	Profile string `json:"profile"`
	Moves   []struct {
		OldID string `json:"old_id"`
		NewID string `json:"new_id"`
	} `json:"moves"`
}

type verifyAssetRow struct {
	id       pgtype.UUID
	owner    *int64
	hash     *string
	size     *int64
	declared *string
}

type verifyValueRow struct {
	asset   pgtype.UUID
	field   pgtype.UUID
	text    *string
	num     *float64
	date    pgtype.Timestamptz
	options []string
	ref     pgtype.UUID
	setBy   string
}

// Verify runs every invariant against pool and returns the report. It
// never writes. An error is an inability to run (unreadable catalogue,
// database down), never a failed invariant: those are in the report.
func Verify(ctx context.Context, pool *pgxpool.Pool, opts VerifyOptions) (*VerifyReport, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	cat, err := loadCatalogues(opts.CatalogueRoot, opts.SiteRoot)
	if err != nil {
		return nil, err
	}
	rep := &VerifyReport{Provenance: map[string]int{}, Assets: len(cat.Assets), Posts: len(cat.Posts)}
	q := New(pool)
	// A Runner, for its lookups and for postSubjectFor. nil storage and
	// no phase is ever run: it is a container for the maps the subject
	// derivation reads.
	r := NewRunner(pool, nil, Options{SiteRoot: opts.SiteRoot, CatalogueRoot: opts.CatalogueRoot, Logger: opts.Logger})

	// -- field definitions, bound exactly as applyFields binds them ------
	existing, err := q.SeedListFields(ctx)
	if err != nil {
		return nil, fmt.Errorf("list field definitions: %w", err)
	}
	fields := make(map[string]fieldMeta, len(existing))
	codeByID := make(map[string]string, len(existing))
	for _, f := range existing {
		fields[f.Code] = fieldMeta{id: f.ID, typ: f.Type}
		codeByID[uuidString(f.ID)] = f.Code
	}
	for _, f := range cat.Fields {
		row, ok := fields[f.Name]
		if !ok {
			rep.fail("field definition %s: absent from the database (the seed creates it, so this database was not seeded from this catalogue)", f.Name)
			continue
		}
		if row.typ != f.Type {
			rep.TypeMismatch++
			rep.fail("field definition %s: the catalogue declares type %s but the existing definition is %s (type_mismatch; values are compared by the existing type)",
				f.Name, f.Type, row.typ)
		}
	}

	// -- lookups the subject derivation and the collapse check need -----
	if err := verifyLoadUsers(ctx, pool, r); err != nil {
		return nil, err
	}
	if err := verifyLoadCollections(ctx, pool, r); err != nil {
		return nil, err
	}

	// -- assets ----------------------------------------------------------
	manifestIDs := make([]pgtype.UUID, 0, len(cat.Assets))
	for _, a := range cat.Assets {
		manifestIDs = append(manifestIDs, parseUUID(a.ID))
	}
	present, err := verifyLoadAssets(ctx, pool, manifestIDs)
	if err != nil {
		return nil, err
	}
	var presentIDs []pgtype.UUID
	// absent[a.ID] = the reason, for the field-value pass below: every
	// expected value of an absent asset is a failure of its own, named,
	// and stays in the expected population.
	absent := map[string]string{}
	for _, a := range cat.Assets {
		row, ok := present[uuidString(parseUUID(a.ID))]
		if !ok {
			sibling, why, cerr := verifyCollapsedOnto(ctx, pool, r, opts.SiteRoot, a)
			if cerr != nil {
				return nil, cerr
			}
			if sibling == "" {
				absent[a.ID] = why
				rep.fail("asset %s: absent from the database (%s)", a.ID, why)
				continue
			}
			rep.AssetsCollapsed++
			absent[a.ID] = fmt.Sprintf("collapsed by content address onto %s: %s", sibling, why)
			rep.fail("asset %s: absent from the database; SeedInsertAsset collapsed it by content address onto %s (%s), so its id, declaration, size and %d field value(s) did not materialize under this id",
				a.ID, sibling, why, len(a.FieldValues))
			rep.note("asset %s: collapse diagnosis, sibling %s (%s)", a.ID, sibling, why)
			continue
		}
		rep.AssetsPresent++
		r.assets[a.ID] = row.id
		presentIDs = append(presentIDs, row.id)
		if !sameOptionalString(a.AiProvenance, row.declared) {
			rep.fail("asset %s: ai_provenance is %s in the manifest and %s in the database",
				a.ID, optionalString(a.AiProvenance), optionalString(row.declared))
		}
		if a.FileSizeBytes > 0 {
			if row.size == nil || *row.size != a.FileSizeBytes {
				rep.fail("asset %s: file_size_bytes is %d in the manifest and %s in the database",
					a.ID, a.FileSizeBytes, optionalInt(row.size))
			}
		}
	}

	// -- field values: rules A, B and C ----------------------------------
	have, err := verifyLoadValues(ctx, pool, presentIDs)
	if err != nil {
		return nil, err
	}
	for _, a := range cat.Assets {
		assetID, hasRow := r.assets[a.ID]
		codes := make([]string, 0, len(a.FieldValues))
		for c := range a.FieldValues {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		for _, code := range codes {
			raw := a.FieldValues[code]
			fm, ok := fields[code]
			if !ok {
				rep.UnknownCode++
				rep.fail("asset %s field %s: no field definition carries this code (unknown_code; the seed dropped %s)",
					a.ID, code, dropValueRepr(raw))
				continue
			}
			params, ok := fieldValueParams(fm.typ, raw)
			if !ok {
				rep.ValueRejected++
				rep.fail("asset %s field %s: value %s is not acceptable to type %s (value_rejected; the seed dropped it)",
					a.ID, code, dropValueRepr(raw), fm.typ)
				continue
			}
			rep.ExpectedValues++
			if !hasRow {
				rep.fail("asset %s field %s (%s): expected seed value %s did not materialize; the asset has no row of its own (%s)",
					a.ID, code, fm.typ, dropValueRepr(raw), absent[a.ID])
				continue
			}
			key := uuidString(assetID) + "/" + uuidString(fm.id)
			row, ok := have[key]
			if !ok {
				rep.fail("asset %s field %s (%s): expected seed value %s has no row", a.ID, code, fm.typ, dropValueRepr(raw))
				continue
			}
			delete(have, key)
			if row.setBy != "import" {
				// Rule C. A row under another provenance is not proof the
				// seed wrote anything, whatever value it holds.
				rep.fail("asset %s field %s (%s): expected seed value present under provenance %q, not import",
					a.ID, code, fm.typ, row.setBy)
				continue
			}
			rep.ImportRows++
			if diff := typedMismatch(fm.typ, params, row); diff != "" {
				rep.fail("asset %s field %s (%s): %s", a.ID, code, fm.typ, diff)
			}
		}
	}
	// Rule B: whatever is left on the catalogue assets is either another
	// writer's row (counted) or a seed-owned row the profile does not
	// carry (failed).
	leftover := make([]string, 0, len(have))
	for k := range have {
		leftover = append(leftover, k)
	}
	sort.Strings(leftover)
	for _, k := range leftover {
		row := have[k]
		code := codeByID[uuidString(row.field)]
		if code == "" {
			code = uuidString(row.field)
		}
		if row.setBy == "import" {
			rep.ImportRows++
			rep.fail("asset %s field %s: unexpected import row; the profile does not carry this value",
				uuidString(row.asset), code)
			continue
		}
		rep.Provenance[row.setBy]++
	}

	// -- posts: identity and content ------------------------------------
	live, err := verifyLoadLivePosts(ctx, pool)
	if err != nil {
		return nil, err
	}
	index, err := r.loadPostIndex(ctx)
	if err != nil {
		return nil, err
	}
	assetTiers := make(map[string]string, len(cat.Assets))
	for _, a := range cat.Assets {
		assetTiers[a.ID] = sensitivity(a.SensitivityTier)
	}
	catalogueIDs := make(map[string]int, len(cat.Posts)) // canonical id -> rows in posts.json
	for _, p := range cat.Posts {
		catalogueIDs[uuidString(parseUUID(p.ID))]++
	}
	for _, p := range cat.Posts {
		canonical := uuidString(parseUUID(p.ID))
		subject, ok := r.postSubjectFor(p, assetTiers)
		if !ok {
			// The seed skips such a post, so it never materializes. On a
			// full-site acceptance that is a catalogue post the database
			// does not hold, and a skip is not a pass.
			rep.PostsSkipped++
			rep.fail("post %s: no resolvable member, so the seed skips it and the post does not materialize", p.ID)
			continue
		}
		if !live[canonical] {
			rep.fail("post %s: absent from the database", p.ID)
			continue
		}
		rep.PostsPresent++
		// Registered exactly as applyPosts registers an inserted or
		// resumed post, so the backfill plan below sees the same coverage
		// the seed saw.
		r.posts[p.ID] = parseUUID(p.ID)
		if row, ok := index.byID[canonical]; ok {
			if diff := subject.compare(row); len(diff) > 0 {
				rep.fail("post %s: the database row disagrees with the catalogue on %s",
					p.ID, strings.Join(diff, ", "))
			}
		}
	}
	// The EXACT backfill population for this seeded state, derived by the
	// same function applyCollectionPostBackfill runs: every one of these
	// must be live, and a live post outside the catalogue must be one of
	// them. A backfill-shaped id the current state would not mint (its
	// asset covered, or never materialized) is a stale row, and stale
	// rows reach the wall.
	order, _ := r.planCollectionPostBackfill(cat, r.backfillCoverage(cat))
	expectedBackfill := make(map[string]bool, len(order))
	for _, key := range order {
		id := backfillPostID(key).String()
		expectedBackfill[id] = true
		rep.ExpectedBackfill++
		if live[id] {
			rep.LiveBackfill++
		} else {
			rep.fail("post %s: expected collection backfill post (%s) is not live", id, backfillKeyLabel(key))
		}
	}
	extras := make([]string, 0)
	for id := range live {
		if _, ok := catalogueIDs[id]; ok {
			continue
		}
		if expectedBackfill[id] {
			continue
		}
		extras = append(extras, id)
	}
	sort.Strings(extras)
	for _, id := range extras {
		rep.LiveExtra++
		rep.fail("post %s: live in the database but neither in the catalogue nor an expected collection backfill post for this seeded state", id)
	}
	for _, id := range opts.ExpectOnce {
		canonical := uuidString(parseUUID(id))
		n := catalogueIDs[canonical]
		if n != 1 {
			rep.fail("post %s: expected exactly once in posts.json, found %d row(s)", id, n)
		}
		if !live[canonical] {
			rep.fail("post %s: expected live, absent from the database", id)
		}
	}
	if opts.MigrationDocument != "" {
		if err := verifyMigration(opts.MigrationDocument, live, catalogueIDs, rep); err != nil {
			return nil, err
		}
	}

	// -- supplied asset expectations --------------------------------------
	for _, e := range opts.ExpectAssets {
		row, ok := present[uuidString(parseUUID(e.ID))]
		if !ok {
			rep.fail("asset %s: expected present, absent from the database", e.ID)
			continue
		}
		var want *string
		if e.AiProvenance != "" {
			w := e.AiProvenance
			want = &w
		}
		if !sameOptionalString(want, row.declared) {
			rep.fail("asset %s: expected ai_provenance %s, database holds %s",
				e.ID, optionalString(want), optionalString(row.declared))
		}
		if e.SizeBytes > 0 && (row.size == nil || *row.size != e.SizeBytes) {
			rep.fail("asset %s: expected file_size_bytes %d, database holds %s",
				e.ID, e.SizeBytes, optionalInt(row.size))
		}
	}
	return rep, nil
}

func verifyLoadUsers(ctx context.Context, pool *pgxpool.Pool, r *Runner) error {
	rows, err := pool.Query(ctx, `SELECT ref, username FROM "user" WHERE username IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref int64
		var name string
		if err := rows.Scan(&ref, &name); err != nil {
			return fmt.Errorf("scan user: %w", err)
		}
		r.users[name] = ref
	}
	return rows.Err()
}

func verifyLoadCollections(ctx context.Context, pool *pgxpool.Pool, r *Runner) error {
	rows, err := pool.Query(ctx, `SELECT id, name FROM collections WHERE deleted_at IS NULL`)
	if err != nil {
		return fmt.Errorf("list collections: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id pgtype.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return fmt.Errorf("scan collection: %w", err)
		}
		r.collections[name] = id
	}
	return rows.Err()
}

func verifyLoadAssets(ctx context.Context, pool *pgxpool.Pool, ids []pgtype.UUID) (map[string]verifyAssetRow, error) {
	out := make(map[string]verifyAssetRow, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT id, owner_user_ref, file_hash, file_size_bytes, ai_provenance
		   FROM assets WHERE deleted_at IS NULL AND id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a verifyAssetRow
		if err := rows.Scan(&a.id, &a.owner, &a.hash, &a.size, &a.declared); err != nil {
			return nil, fmt.Errorf("scan asset: %w", err)
		}
		out[uuidString(a.id)] = a
	}
	return out, rows.Err()
}

func verifyLoadValues(ctx context.Context, pool *pgxpool.Pool, ids []pgtype.UUID) (map[string]verifyValueRow, error) {
	out := make(map[string]verifyValueRow)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT asset_id, field_id, value_text, value_num, value_date, value_options, value_ref, set_by
		   FROM asset_field_value WHERE asset_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("list field values: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v verifyValueRow
		if err := rows.Scan(&v.asset, &v.field, &v.text, &v.num, &v.date, &v.options, &v.ref, &v.setBy); err != nil {
			return nil, fmt.Errorf("scan field value: %w", err)
		}
		out[uuidString(v.asset)+"/"+uuidString(v.field)] = v
	}
	return out, rows.Err()
}

func verifyLoadLivePosts(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT id FROM posts WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("list posts: %w", err)
	}
	defer rows.Close()
	live := map[string]bool{}
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan post: %w", err)
		}
		live[uuidString(id)] = true
	}
	return live, rows.Err()
}

// verifyCollapsedOnto answers, for a manifest asset with no row of its
// own, whether SeedInsertAsset's content-address collapse explains it:
// the site file's sha256 (the storage hash) is held by a live sibling
// row with the same owner. Returns the sibling id, a one-line reason,
// or "" when nothing explains the absence.
func verifyCollapsedOnto(ctx context.Context, pool *pgxpool.Pool, r *Runner, siteRoot string, a manifestAsset) (string, string, error) {
	path := filepath.Join(siteRoot, a.FilePath)
	hash, err := sha256File(path)
	if err != nil {
		return "", fmt.Sprintf("file %s unreadable: %v", a.FilePath, err), nil
	}
	var owner *int64
	if ref, ok := r.users[a.OwnerUsername]; ok {
		owner = &ref
	}
	var sibling pgtype.UUID
	err = pool.QueryRow(ctx,
		`SELECT id FROM assets
		  WHERE deleted_at IS NULL AND file_hash = $1 AND owner_user_ref IS NOT DISTINCT FROM $2
		  ORDER BY created_at, id LIMIT 1`, hash, owner).Scan(&sibling)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", fmt.Sprintf("file %s, sha256 %s, no sibling holds those bytes", a.FilePath, hash), nil
		}
		return "", "", fmt.Errorf("look up sibling by hash: %w", err)
	}
	return uuidString(sibling), fmt.Sprintf("same owner, same bytes, sha256 %s", hash), nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// backfillKeyLabel renders a bundle key for a message: the collection
// and the group id or asset id it bundles.
func backfillKeyLabel(key string) string {
	if i := strings.IndexByte(key, 0); i >= 0 {
		return "collection " + key[:i] + ", " + key[i+1:]
	}
	return key
}

func verifyMigration(path string, live map[string]bool, catalogueIDs map[string]int, rep *VerifyReport) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read migration document: %w", err)
	}
	var doc migrationDocument
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse migration document %s: %w", path, err)
	}
	rep.note("migration document %s: %d move(s) for %s", path, len(doc.Moves), doc.Profile)
	for _, m := range doc.Moves {
		newID := uuidString(parseUUID(m.NewID))
		oldID := uuidString(parseUUID(m.OldID))
		if !live[newID] {
			rep.fail("migration %s -> %s: the new id is not a live post", m.OldID, m.NewID)
		}
		if live[oldID] {
			rep.fail("migration %s -> %s: the old id is still a live post", m.OldID, m.NewID)
		}
		if catalogueIDs[oldID] > 0 {
			rep.fail("migration %s -> %s: the old id is still in posts.json", m.OldID, m.NewID)
		}
	}
	return nil
}

// typedMismatch compares the typed column fieldValueParams would have
// written against the row, by the EFFECTIVE type, and returns "" when
// they agree. Only the column the type writes is compared; the seeder
// leaves the others NULL and nothing about them is a profile fact.
func typedMismatch(ftype string, want SeedInsertAssetFieldValueParams, have verifyValueRow) string {
	switch strings.ToLower(ftype) {
	case "number", "boolean":
		if want.ValueNum == nil || have.num == nil || *want.ValueNum != *have.num {
			return fmt.Sprintf("value_num: expected %s, database holds %s", optionalFloat(want.ValueNum), optionalFloat(have.num))
		}
	case "date", "datetime":
		if !want.ValueDate.Valid || !have.date.Valid || !want.ValueDate.Time.Equal(have.date.Time) {
			return fmt.Sprintf("value_date: expected %s, database holds %s", optionalTime(want.ValueDate), optionalTime(have.date))
		}
	case "multi_select":
		if !sameStrings(want.ValueOptions, have.options) {
			return fmt.Sprintf("value_options: expected %v, database holds %v", want.ValueOptions, have.options)
		}
	case "reference":
		if !want.ValueRef.Valid || !have.ref.Valid || uuidString(want.ValueRef) != uuidString(have.ref) {
			return fmt.Sprintf("value_ref: expected %s, database holds %s", optionalUUID(want.ValueRef), optionalUUID(have.ref))
		}
	default:
		// text, longtext, rich_text (after sanitisation), select, tree,
		// and any unknown type name: fieldValueParams writes value_text.
		if want.ValueText == nil || have.text == nil || *want.ValueText != *have.text {
			return fmt.Sprintf("value_text: expected %s, database holds %s", optionalString(want.ValueText), optionalString(have.text))
		}
	}
	return ""
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func optionalString(s *string) string {
	if s == nil {
		return "NULL"
	}
	return fmt.Sprintf("%q", *s)
}

func optionalInt(n *int64) string {
	if n == nil {
		return "NULL"
	}
	return fmt.Sprintf("%d", *n)
}

func optionalFloat(f *float64) string {
	if f == nil {
		return "NULL"
	}
	return fmt.Sprintf("%g", *f)
}

func optionalTime(t pgtype.Timestamptz) string {
	if !t.Valid {
		return "NULL"
	}
	return t.Time.UTC().Format("2006-01-02T15:04:05.999999Z07:00")
}

func optionalUUID(u pgtype.UUID) string {
	if !u.Valid {
		return "NULL"
	}
	return uuidString(u)
}
