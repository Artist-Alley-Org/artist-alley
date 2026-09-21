// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the read-only whole-corpus verifier (#1319). Class C:
// branch-only, each one distinguishing a way the seeded database can
// disagree with the catalogue that a row count cannot see.
//
// Every fixture drives the REAL writers: applyFields binds the
// definitions, applyAssetFields writes the typed import rows,
// applyPosts writes the post subtree, pixeldims.Record writes the
// computed pair and metadata's UpsertAssetFieldValue overwrites under
// another provenance. The verifier then reads a site root and a
// catalogue that describe exactly that world, so a verdict is about
// what the writers did and not about a hand-inserted row.
//
// ⚠️ The test database is shared across packages, and other suites may
// leave live posts behind. The "unexplained live post" invariant is
// therefore asserted through `ownFailures`, which drops only
// failures naming ids this fixture minted; a fixture-owned extra post
// is still asserted positively below.

package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

// verifyFieldTypes is the typed coverage the acceptance requires: text,
// boolean, number, date, datetime, multi_select and reference.
var verifyFieldTypes = []string{"text", "boolean", "number", "date", "datetime", "multi_select", "reference"}

type verifyFixture struct {
	t        *testing.T
	pool     *pgxpool.Pool
	r        *Runner
	log      *bytes.Buffer
	salt     string
	userRef  int64
	username string
	collID   pgtype.UUID
	collName string
	codes    map[string]string // type -> field code
	assets   []manifestAsset   // the manifest entries, in catalogue order
	posts    []manifestPost
	siteRoot string
	catRoot  string
	fields   []catField
}

func (f *verifyFixture) code(ftype string) string { return f.codes[ftype] }

func (f *verifyFixture) assetID(i int) pgtype.UUID { return parseUUID(f.assets[i].ID) }

// newVerifyFixture seeds a two-asset, one-post world through the real
// phases and lays out a site root + catalogue describing it.
func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	pool := openCompanionTestPool(t)
	ctx := context.Background()
	salt := strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
	f := &verifyFixture{t: t, pool: pool, log: &bytes.Buffer{}, salt: salt, codes: map[string]string{}}

	f.username = "aa1319_" + salt
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, password) VALUES ($1, '') RETURNING ref`,
		f.username).Scan(&f.userRef); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, f.userRef)
	})

	f.collName = "aa1319 collection " + salt
	if err := pool.QueryRow(ctx,
		`INSERT INTO collections (name, owner_user_ref) VALUES ($1, $2) RETURNING id`,
		f.collName, f.userRef).Scan(&f.collID); err != nil {
		t.Fatalf("insert collection: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM collection_posts WHERE collection_id = $1`, f.collID)
		_, _ = pool.Exec(c, `DELETE FROM collection_resources WHERE collection_id = $1`, f.collID)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE id = $1`, f.collID)
	})

	f.r = NewRunner(pool, nil, Options{Logger: captureLogger(f.log)})
	f.r.adminRef = f.userRef
	f.r.users[f.username] = f.userRef
	f.r.collections[f.collName] = f.collID

	// Field definitions through applyFields, one per required type.
	for _, ft := range verifyFieldTypes {
		code := "aa1319_" + ft + "_" + salt
		f.codes[ft] = code
		f.fields = append(f.fields, catField{Name: code, Label: code, Type: ft})
	}
	f.cleanupFields()
	if err := f.r.applyFields(ctx, &catalogues{Fields: f.fields}); err != nil {
		t.Fatalf("applyFields: %v", err)
	}

	// Two assets. The first carries every type; the second one text
	// value, so a field the profile does not carry exists for rule B.
	a1 := f.newAsset("none", 11404)
	a2 := f.newAsset("", 20)
	a1.FieldValues = map[string]any{
		f.code("text"):         "hello",
		f.code("boolean"):      true,
		f.code("number"):       float64(42),
		f.code("date"):         "2026-03-14",
		f.code("datetime"):     "2026-03-14T09:30:00Z",
		f.code("multi_select"): []any{"a", "b"},
		f.code("reference"):    a2.ID,
	}
	a2.FieldValues = map[string]any{f.code("text"): "second"}
	f.assets = []manifestAsset{a1, a2}
	f.seedAssetFields()

	// One post over both assets, through the real phase.
	f.posts = []manifestPost{f.newPost(uuid.New().String(), a1.ID, a2.ID)}
	f.seedPosts(f.posts...)

	f.siteRoot = t.TempDir()
	f.catRoot = t.TempDir()
	f.writeSite()
	return f
}

func (f *verifyFixture) cleanupFields() {
	f.t.Cleanup(func() {
		c := context.Background()
		for _, fd := range f.fields {
			_, _ = f.pool.Exec(c, `DELETE FROM asset_field_value_history WHERE field_id IN (SELECT id FROM field_definition WHERE code = $1)`, fd.Name)
			_, _ = f.pool.Exec(c, `DELETE FROM asset_field_value WHERE field_id IN (SELECT id FROM field_definition WHERE code = $1)`, fd.Name)
			_, _ = f.pool.Exec(c, `DELETE FROM field_definition WHERE code = $1`, fd.Name)
		}
	})
}

// newAsset inserts an asset row the way the seeder does (its own
// query, no bytes: file_hash stays NULL) and returns the manifest entry
// describing it. declared "" is undeclared (NULL).
func (f *verifyFixture) newAsset(declared string, size int64) manifestAsset {
	f.t.Helper()
	ctx := context.Background()
	id := uuid.New().String()
	var decl *string
	if declared != "" {
		d := declared
		decl = &d
	}
	ext, status := "png", "active"
	at := pgtype.Timestamptz{Time: time.Date(2025, 5, 5, 12, 0, 0, 0, time.UTC), Valid: true}
	owner := f.userRef
	got, err := New(f.pool).SeedInsertAsset(ctx, SeedInsertAssetParams{
		ID:            parseUUID(id),
		Title:         "aa1319 " + id[:8],
		AssetType:     1,
		OwnerUserRef:  &owner,
		Status:        status,
		FileExtension: &ext,
		FileSizeBytes: &size,
		Metadata:      []byte(`{"acquisition_source":"test-fixture"}`),
		Sensitivity:   "public",
		AiProvenance:  decl,
		CreatedAt:     at,
		UpdatedAt:     at,
	})
	if err != nil {
		f.t.Fatalf("SeedInsertAsset: %v", err)
	}
	f.cleanupAsset(got)
	f.r.assets[id] = got
	return manifestAsset{
		ID:              id,
		AssetType:       "image",
		Title:           "aa1319 " + id[:8],
		FilePath:        "images/" + id + ".png",
		FileExtension:   "png",
		FileSizeBytes:   size,
		SensitivityTier: "public",
		ArchiveState:    "active",
		OwnerUsername:   f.username,
		CollectionName:  f.collName,
		AiProvenance:    decl,
		Metadata:        json.RawMessage(`{"acquisition_source":"test-fixture"}`),
		CreatedAt:       "2025-05-05T12:00:00Z",
		UpdatedAt:       "2025-05-05T12:00:00Z",
	}
}

func (f *verifyFixture) cleanupAsset(id pgtype.UUID) {
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM post_assets WHERE asset_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM collection_resources WHERE asset_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
	})
}

func (f *verifyFixture) seedAssetFields() {
	f.t.Helper()
	for _, a := range f.assets {
		if err := f.r.applyAssetFields(context.Background(), f.r.assets[a.ID], a.FieldValues); err != nil {
			f.t.Fatalf("applyAssetFields %s: %v", a.ID, err)
		}
	}
}

func (f *verifyFixture) newPost(id string, assetIDs ...string) manifestPost {
	f.t.Helper()
	pgID := parseUUID(id)
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM collection_posts WHERE post_id = $1`, pgID)
		_, _ = f.pool.Exec(c, `DELETE FROM post_tags WHERE post_id = $1`, pgID)
		_, _ = f.pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, pgID)
		_, _ = f.pool.Exec(c, `DELETE FROM posts WHERE id = $1`, pgID)
	})
	return manifestPost{
		ID:              id,
		Title:           "aa1319 post " + id[:8],
		Description:     "seeded by the verifier fixture",
		AssetIDs:        assetIDs,
		AuthorUsername:  f.username,
		CollectionName:  f.collName,
		Tags:            []string{"alpha", "beta"},
		SensitivityTier: "public",
		CreatedAt:       "2025-06-01T00:00:00Z",
		UpdatedAt:       "2025-06-02T00:00:00Z",
	}
}

func (f *verifyFixture) seedPosts(posts ...manifestPost) {
	f.t.Helper()
	cat := &catalogues{Posts: posts, Assets: f.assets}
	if err := f.r.applyPosts(context.Background(), cat); err != nil {
		f.t.Fatalf("applyPosts: %v", err)
	}
}

func (f *verifyFixture) writeSite() {
	f.t.Helper()
	write := func(path string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	write(filepath.Join(f.siteRoot, "MANIFEST.json"), f.assets)
	write(filepath.Join(f.siteRoot, "posts.json"), f.posts)
	write(filepath.Join(f.catRoot, "dataset.users.json"), []catUser{{Username: f.username}})
	write(filepath.Join(f.catRoot, "dataset.teams.json"), []catTeam{})
	write(filepath.Join(f.catRoot, "dataset.collections.json"), []catCollection{{ID: "c1", Name: f.collName}})
	write(filepath.Join(f.catRoot, "dataset.field_definitions.json"), f.fields)
}

func (f *verifyFixture) verify(opts VerifyOptions) *VerifyReport {
	f.t.Helper()
	opts.SiteRoot = f.siteRoot
	opts.CatalogueRoot = f.catRoot
	opts.Logger = captureLogger(&bytes.Buffer{})
	rep, err := Verify(context.Background(), f.pool, opts)
	if err != nil {
		f.t.Fatalf("Verify: %v", err)
	}
	return rep
}

// ownFailures drops the one class of failure a SHARED database can
// produce on its own: live posts that belong to some other suite.
func (f *verifyFixture) ownFailures(rep *VerifyReport) []string {
	var out []string
	for _, msg := range rep.Failures {
		if strings.Contains(msg, "live in the database but neither in the catalogue") &&
			!strings.Contains(msg, f.salt) && !f.namesOwnPost(msg) {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func (f *verifyFixture) namesOwnPost(msg string) bool {
	for _, p := range f.posts {
		if strings.Contains(msg, p.ID) {
			return true
		}
	}
	return false
}

func mustContain(t *testing.T, failures []string, want ...string) {
	t.Helper()
	for _, msg := range failures {
		ok := true
		for _, w := range want {
			if !strings.Contains(msg, w) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
	}
	t.Fatalf("no failure names all of %q; failures were:\n  %s", want, strings.Join(failures, "\n  "))
}

func mustNotContain(t *testing.T, failures []string, fragment string) {
	t.Helper()
	for _, msg := range failures {
		if strings.Contains(msg, fragment) {
			t.Fatalf("a failure names %q, and must not: %s", fragment, msg)
		}
	}
}

// 1. A complete valid profile whose expected typed import rows are all
// present passes, with the drop counters at zero.
func TestVerify_CompleteProfilePasses(t *testing.T) {
	f := newVerifyFixture(t)
	rep := f.verify(VerifyOptions{})
	if own := f.ownFailures(rep); len(own) != 0 {
		t.Fatalf("a complete profile failed:\n  %s", strings.Join(own, "\n  "))
	}
	if rep.ExpectedValues != 8 || rep.ImportRows != 8 {
		t.Errorf("expected 8 profile values and 8 import rows, got %d and %d", rep.ExpectedValues, rep.ImportRows)
	}
	if rep.UnknownCode != 0 || rep.ValueRejected != 0 || rep.TypeMismatch != 0 {
		t.Errorf("drops on a clean profile: %d/%d/%d", rep.UnknownCode, rep.ValueRejected, rep.TypeMismatch)
	}
	if rep.AssetsPresent != 2 || rep.PostsPresent != 1 {
		t.Errorf("assets present %d (want 2), posts present %d (want 1)", rep.AssetsPresent, rep.PostsPresent)
	}
	if n := f.r.fieldDrops.total(dropUnknownCode) + f.r.fieldDrops.total(dropValueRejected); n != 0 {
		t.Errorf("the seeding runner itself dropped %d value(s)", n)
	}
}

// 2. An expected row deleted after an otherwise valid seed fails, naming
// the asset and the field code.
func TestVerify_DeletedExpectedRowFails(t *testing.T) {
	f := newVerifyFixture(t)
	code := f.code("text")
	if _, err := f.pool.Exec(context.Background(),
		`DELETE FROM asset_field_value WHERE asset_id = $1
		    AND field_id = (SELECT id FROM field_definition WHERE code = $2)`,
		f.assetID(0), code); err != nil {
		t.Fatal(err)
	}
	rep := f.verify(VerifyOptions{})
	mustContain(t, rep.Failures, f.assets[0].ID, code, "has no row")
	if rep.ImportRows != 7 {
		t.Errorf("import rows %d, want 7", rep.ImportRows)
	}
}

// 3. A wrong typed column value fails, naming the asset and field, for
// every one of the seven types.
func TestVerify_WrongTypedValueFailsForEachType(t *testing.T) {
	f := newVerifyFixture(t)
	ctx := context.Background()
	corrupt := map[string]string{
		"text":         `value_text = 'edited'`,
		"boolean":      `value_num = 0`,
		"number":       `value_num = 43`,
		"date":         `value_date = '2026-03-15T00:00:00Z'`,
		"datetime":     `value_date = '2026-03-14T09:31:00Z'`,
		"multi_select": `value_options = ARRAY['a','c']`,
		"reference":    `value_ref = '` + uuid.New().String() + `'`,
	}
	for ft, set := range corrupt {
		if _, err := f.pool.Exec(ctx,
			`UPDATE asset_field_value SET `+set+` WHERE asset_id = $1
			    AND field_id = (SELECT id FROM field_definition WHERE code = $2)`,
			f.assetID(0), f.code(ft)); err != nil {
			t.Fatalf("corrupt %s: %v", ft, err)
		}
	}
	rep := f.verify(VerifyOptions{})
	column := map[string]string{
		"text": "value_text", "boolean": "value_num", "number": "value_num",
		"date": "value_date", "datetime": "value_date",
		"multi_select": "value_options", "reference": "value_ref",
	}
	for ft := range corrupt {
		mustContain(t, rep.Failures, f.assets[0].ID, f.code(ft), column[ft])
	}
	if n := len(f.ownFailures(rep)); n != len(corrupt) {
		t.Errorf("%d failure(s) for %d corrupted values:\n  %s", n, len(corrupt), strings.Join(rep.Failures, "\n  "))
	}
}

// 4. An extra set_by='import' row for a field the profile does not
// carry fails, naming it (rule B, the other direction).
func TestVerify_ExtraImportRowFails(t *testing.T) {
	f := newVerifyFixture(t)
	// The second asset carries only the text field; write the number
	// field on it through the seeder's own insert.
	fm := f.r.fields[f.code("number")]
	params, ok := fieldValueParams("number", float64(7))
	if !ok {
		t.Fatal("fieldValueParams refused a plain number")
	}
	params.AssetID = f.assetID(1)
	params.FieldID = fm.id
	if err := New(f.pool).SeedInsertAssetFieldValue(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	rep := f.verify(VerifyOptions{})
	mustContain(t, rep.Failures, f.assets[1].ID, f.code("number"), "unexpected import row")
}

// 5 + 6. A legitimate set_by='computed' row absent from the profile is
// informational, and specifically pixel_width / pixel_height written
// through pixeldims.Record are never called unexpected.
func TestVerify_ComputedRowsAreInformationalNotFailures(t *testing.T) {
	f := newVerifyFixture(t)
	ctx := context.Background()
	if err := pixeldims.Record(ctx, f.pool, uuid.UUID(f.assetID(1).Bytes), 640, 480); err != nil {
		t.Fatalf("pixeldims.Record: %v", err)
	}
	// A computed row on one of the fixture's own fields, absent from
	// the second asset's profile entry, for the general rule.
	fm := f.r.fields[f.code("boolean")]
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO asset_field_value (asset_id, field_id, value_num, set_by) VALUES ($1, $2, 1, 'computed')`,
		f.assetID(1), fm.id); err != nil {
		t.Fatal(err)
	}
	rep := f.verify(VerifyOptions{})
	if own := f.ownFailures(rep); len(own) != 0 {
		t.Fatalf("computed rows produced failures:\n  %s", strings.Join(own, "\n  "))
	}
	if rep.Provenance["computed"] != 3 {
		t.Errorf("computed rows reported %d, want 3 (pixel_width, pixel_height, one more)", rep.Provenance["computed"])
	}
	mustNotContain(t, rep.Failures, "pixel_width")
	mustNotContain(t, rep.Failures, "pixel_height")
	if rep.ImportRows != 8 {
		t.Errorf("import rows %d, want 8: derived rows must not be counted as seed-owned", rep.ImportRows)
	}
}

// 7. An expected profile field overwritten to set_by='manual' through
// UpsertAssetFieldValue, with the SAME value, fails under rule C.
func TestVerify_ExpectedValueUnderAnotherProvenanceFails(t *testing.T) {
	f := newVerifyFixture(t)
	fm := f.r.fields[f.code("text")]
	same := "hello"
	caller := f.userRef
	if _, err := metadata.New(f.pool).UpsertAssetFieldValue(context.Background(),
		metadata.UpsertAssetFieldValueParams{
			AssetID: f.assetID(0), FieldID: fm.id, ValueText: &same,
			SetBy: "manual", SetByUserRef: &caller,
		}); err != nil {
		t.Fatalf("UpsertAssetFieldValue: %v", err)
	}
	rep := f.verify(VerifyOptions{})
	mustContain(t, rep.Failures, f.assets[0].ID, f.code("text"), `provenance "manual"`)
	if rep.ImportRows != 7 {
		t.Errorf("import rows %d, want 7: a manual row is not proof the seed wrote the value", rep.ImportRows)
	}
}

// 8. A profile carrying an unknown field code: the seed reports
// unknown_code 1 and the verifier fails naming asset and code.
func TestVerify_UnknownCodeFails(t *testing.T) {
	f := newVerifyFixture(t)
	code := "aa1319_nosuch_" + f.salt
	f.assets[1].FieldValues[code] = "x"
	if err := f.r.applyAssetFields(context.Background(), f.assetID(1), map[string]any{code: "x"}); err != nil {
		t.Fatal(err)
	}
	if n := f.r.fieldDrops.total(dropUnknownCode); n != 1 {
		t.Fatalf("the seed counted %d unknown_code drops, want 1", n)
	}
	f.writeSite()
	rep := f.verify(VerifyOptions{})
	mustContain(t, rep.Failures, f.assets[1].ID, code, "unknown_code")
	if rep.UnknownCode != 1 {
		t.Errorf("verifier recomputed unknown_code=%d, want 1", rep.UnknownCode)
	}
}

// 9. A value the effective type rejects: value_rejected 1 on both sides.
func TestVerify_RejectedValueFails(t *testing.T) {
	f := newVerifyFixture(t)
	code := f.code("number")
	f.assets[1].FieldValues[code] = "42" // a number as a JSON string
	if err := f.r.applyAssetFields(context.Background(), f.assetID(1), map[string]any{code: "42"}); err != nil {
		t.Fatal(err)
	}
	if n := f.r.fieldDrops.total(dropValueRejected); n != 1 {
		t.Fatalf("the seed counted %d value_rejected drops, want 1", n)
	}
	f.writeSite()
	rep := f.verify(VerifyOptions{})
	mustContain(t, rep.Failures, f.assets[1].ID, code, "value_rejected")
	if rep.ValueRejected != 1 {
		t.Errorf("verifier recomputed value_rejected=%d, want 1", rep.ValueRejected)
	}
}

// 10. A catalogue definition whose type differs from the existing
// database definition: the seed binds the existing type
// (type_mismatch 1), the verifier reports the field distinctly and
// compares the value by the effective type, and the acceptance result
// is a failure under the zero-mismatch rule.
func TestVerify_TypeMismatchIsReportedDistinctlyAndFails(t *testing.T) {
	f := newVerifyFixture(t)
	ctx := context.Background()
	code := "aa1319_mismatch_" + f.salt
	// The row that already exists says datetime; the catalogue says date.
	newDropTestField(t, f.pool, code, "datetime")
	f.fields = append(f.fields, catField{Name: code, Label: code, Type: "date"})
	if err := f.r.applyFields(ctx, &catalogues{Fields: []catField{{Name: code, Label: code, Type: "date"}}}); err != nil {
		t.Fatal(err)
	}
	if n := f.r.fieldDrops.total(dropTypeMismatch); n != 1 {
		t.Fatalf("the seed counted %d type mismatches, want 1", n)
	}
	f.assets[1].FieldValues[code] = "2026-03-14T09:30:00Z"
	if err := f.r.applyAssetFields(ctx, f.assetID(1), map[string]any{code: "2026-03-14T09:30:00Z"}); err != nil {
		t.Fatal(err)
	}
	f.writeSite()
	rep := f.verify(VerifyOptions{})
	if rep.TypeMismatch != 1 {
		t.Errorf("verifier recomputed type_mismatch=%d, want 1", rep.TypeMismatch)
	}
	mustContain(t, rep.Failures, code, "declares type date", "existing definition is datetime")
	// The VALUE agrees by the effective type: no second failure for it.
	for _, msg := range rep.Failures {
		if strings.Contains(msg, code) && strings.Contains(msg, "value_date") {
			t.Errorf("the value was compared by the catalogue's type, not the row's: %s", msg)
		}
	}
	if rep.OK() {
		t.Error("a type mismatch must fail the acceptance run")
	}
}

// Posts: a migrated new id must be live and its old id must not be; a
// post row edited after the seed is drift the verifier names; supplied
// once-ids are checked in posts.json and in the database.
func TestVerify_PostIdentityAndContent(t *testing.T) {
	f := newVerifyFixture(t)
	ctx := context.Background()

	// An old id still live: seed a second post the site's posts.json does
	// NOT carry, and record it as the old id of the fixture's post.
	old := f.newPost(uuid.New().String(), f.assets[0].ID)
	f.seedPosts(old)
	doc := map[string]any{
		"profile": "fixture.posts.json",
		"moves": []map[string]string{
			{"old_id": old.ID, "new_id": f.posts[0].ID},
			{"old_id": uuid.New().String(), "new_id": uuid.New().String()}, // new id not live
		},
	}
	docPath := filepath.Join(t.TempDir(), "post-id-migration.fixture.json")
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(docPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	rep := f.verify(VerifyOptions{MigrationDocument: docPath})
	mustContain(t, rep.Failures, old.ID, "old id is still a live post")
	mustContain(t, rep.Failures, old.ID, "live in the database but neither in the catalogue")
	mustContain(t, rep.Failures, "the new id is not a live post")

	// Content drift: the row was edited after the seed.
	if _, err := f.pool.Exec(ctx, `UPDATE posts SET title = 'edited after the seed' WHERE id = $1`,
		parseUUID(f.posts[0].ID)); err != nil {
		t.Fatal(err)
	}
	rep = f.verify(VerifyOptions{})
	mustContain(t, rep.Failures, f.posts[0].ID, "disagrees with the catalogue on title")

	// Once-ids: duplicated in posts.json, and absent from the database.
	f.posts = append(f.posts, f.posts[0])
	f.writeSite()
	absent := uuid.New().String()
	rep = f.verify(VerifyOptions{ExpectOnce: []string{f.posts[0].ID, absent}})
	mustContain(t, rep.Failures, f.posts[0].ID, "expected exactly once in posts.json, found 2")
	mustContain(t, rep.Failures, absent, "expected live, absent")
}

// The content-address collapse: a manifest entry whose bytes are
// identical to a sibling the same owner holds gets no row of its own
// (SeedInsertAsset's owner+file_hash conflict). The verifier names it as
// collapsed and does not call it missing; an entry whose bytes nothing
// holds IS missing.
func TestVerify_CollapsedAssetIsNotMissing(t *testing.T) {
	f := newVerifyFixture(t)
	ctx := context.Background()
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := storage.NewService(backend, f.pool)
	payload := []byte("aa1319 identical bytes " + f.salt)
	sibling := f.newAsset("", int64(len(payload)))
	up, err := svc.UploadOriginal(ctx, bytes.NewReader(payload), "image/png",
		storage.PinRef{SubjectType: "asset", SubjectID: sibling.ID})
	if err != nil {
		t.Fatalf("UploadOriginal: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `UPDATE assets SET file_hash = NULL WHERE file_hash = $1`, up.Hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, up.Hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, up.Hash)
	})
	if _, err := f.pool.Exec(ctx, `UPDATE assets SET file_hash = $1 WHERE id = $2`, up.Hash, parseUUID(sibling.ID)); err != nil {
		t.Fatal(err)
	}
	// The collapsed entry: same owner, same bytes, no row.
	collapsed := manifestAsset{
		ID: uuid.New().String(), AssetType: "image", Title: "collapsed",
		FilePath: "images/collapsed.png", FileExtension: "png",
		FileSizeBytes: int64(len(payload)), SensitivityTier: "public", ArchiveState: "active",
		OwnerUsername: f.username, CollectionName: f.collName,
		FieldValues: map[string]any{f.code("text"): "never written"},
		CreatedAt:   "2025-05-05T12:00:00Z", UpdatedAt: "2025-05-05T12:00:00Z",
	}
	// And a genuinely missing entry: bytes nothing holds.
	missing := manifestAsset{
		ID: uuid.New().String(), AssetType: "image", Title: "missing",
		FilePath: "images/missing.png", FileExtension: "png",
		FileSizeBytes: 5, SensitivityTier: "public", ArchiveState: "active",
		OwnerUsername: f.username, CreatedAt: "2025-05-05T12:00:00Z", UpdatedAt: "2025-05-05T12:00:00Z",
	}
	f.assets = append(f.assets, sibling, collapsed, missing)
	f.writeSite()
	for rel, content := range map[string][]byte{
		sibling.FilePath: payload, collapsed.FilePath: payload, missing.FilePath: []byte("other"),
	} {
		p := filepath.Join(f.siteRoot, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rep := f.verify(VerifyOptions{})
	if rep.AssetsCollapsed != 1 {
		t.Errorf("collapsed assets %d, want 1", rep.AssetsCollapsed)
	}
	mustNotContain(t, rep.Failures, collapsed.ID)
	mustContain(t, rep.Failures, missing.ID, "absent from the database")
	found := false
	for _, n := range rep.Notes {
		if strings.Contains(n, collapsed.ID) && strings.Contains(n, sibling.ID) {
			found = true
		}
	}
	if !found {
		t.Errorf("no note names the collapse %s -> %s:\n  %s", collapsed.ID, sibling.ID, strings.Join(rep.Notes, "\n  "))
	}
	if rep.ExpectedValues != 8 {
		t.Errorf("expected values %d, want 8: a collapsed asset's values are never written and must not be expected", rep.ExpectedValues)
	}
}

// Supplied asset expectations and the manifest-versus-row declaration
// and size checks: the two plates' `none` / `assisted` and byte sizes
// are the case this exists for.
func TestVerify_AssetDeclarationAndSize(t *testing.T) {
	f := newVerifyFixture(t)
	a := f.assets[0]
	rep := f.verify(VerifyOptions{ExpectAssets: []AssetExpectation{
		{ID: a.ID, AiProvenance: "none", SizeBytes: 11404},
		{ID: f.assets[1].ID, AiProvenance: "", SizeBytes: 20},
	}})
	if own := f.ownFailures(rep); len(own) != 0 {
		t.Fatalf("correct expectations failed:\n  %s", strings.Join(own, "\n  "))
	}
	rep = f.verify(VerifyOptions{ExpectAssets: []AssetExpectation{
		{ID: a.ID, AiProvenance: "assisted", SizeBytes: 1},
		{ID: uuid.New().String(), AiProvenance: "", SizeBytes: 0},
	}})
	mustContain(t, rep.Failures, a.ID, `expected ai_provenance "assisted"`)
	mustContain(t, rep.Failures, a.ID, "expected file_size_bytes 1")
	mustContain(t, rep.Failures, "expected present, absent")

	// The manifest itself versus the row, for every asset.
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE assets SET ai_provenance = 'generated', file_size_bytes = 99 WHERE id = $1`, f.assetID(0)); err != nil {
		t.Fatal(err)
	}
	rep = f.verify(VerifyOptions{})
	mustContain(t, rep.Failures, a.ID, `ai_provenance is "none" in the manifest and "generated"`)
	mustContain(t, rep.Failures, a.ID, "file_size_bytes is 11404 in the manifest and 99")
}

func TestTypedMismatch_ComparesOnlyTheTypedColumn(t *testing.T) {
	text := "x"
	num := 1.0
	row := verifyValueRow{text: &text, num: &num}
	if d := typedMismatch("text", SeedInsertAssetFieldValueParams{ValueText: &text}, row); d != "" {
		t.Errorf("equal text reported %q", d)
	}
	other := "y"
	if d := typedMismatch("text", SeedInsertAssetFieldValueParams{ValueText: &other}, row); d == "" {
		t.Error("different text not reported")
	}
	if d := typedMismatch("boolean", SeedInsertAssetFieldValueParams{ValueNum: &num}, row); d != "" {
		t.Errorf("equal boolean reported %q", d)
	}
	opts := []string{"a", "b"}
	if d := typedMismatch("multi_select", SeedInsertAssetFieldValueParams{ValueOptions: opts},
		verifyValueRow{options: []string{"b", "a"}}); d == "" {
		t.Error("reordered options not reported: the seeder writes the manifest's order")
	}
}
