// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1596 (slice 1): local asset versions, pinned on a real database and
// the fs backend.
//
//   - every insert path (CreateAsset, an aiedit-style raw INSERT, the
//     seeder) gives a local asset with bytes its version 1 and the
//     version's storage pin in the same transaction; an asset without
//     bytes, and a remote asset, get none;
//   - the version pin is what keeps old bytes: removing the asset's own
//     pin and the uploader's pin never makes them GC-eligible, and that
//     holds for a version inserted by raw SQL; an unpinned version or
//     file row cannot commit, and a live version's pin cannot be
//     deleted, moved or truncated away;
//   - soft delete keeps the pins; a hard purge releases them in its own
//     transaction and schedules the bytes with the configured grace; a
//     delete that would leave a version pin behind is refused;
//   - a metadata edit keeps the version; a direct file_hash change is
//     refused; the chain has no gaps or forks and never goes backwards;
//   - a version's defining columns never change; its file set is written
//     only by asset_version_seal() in the transaction that claimed it,
//     holds only the original until verified access copies exist, and
//     never changes after; two concurrent seals cannot both win.

package assets_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/seed"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

type versionRow struct {
	ID           uuid.UUID
	Number       int64
	Predecessor  *uuid.UUID
	Origin       string
	OriginalHash string
	Size         int64
	MediaType    string
	SealedAt     *time.Time
	Created      time.Time
}

// upload stores fresh bytes through the real upload path (which leaves
// the uploader's pin, as POST /storage/objects does).
func (f *maFixture) upload(userRef int64, label string) (string, int64) {
	f.t.Helper()
	tail := make([]byte, 24)
	if _, err := rand.Read(tail); err != nil {
		f.t.Fatal(err)
	}
	body := append([]byte("version test "+label+": "), tail...)
	res, err := f.h.Storage.UploadOriginal(f.ctx, bytes.NewReader(body), "image/png",
		storage.PinRef{SubjectType: storage.PinSubjectTypeUser, SubjectID: strconv.FormatInt(userRef, 10)})
	if err != nil {
		f.t.Fatalf("upload: %v", err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1 AND pin_subject_type <> 'asset_version'`, res.Hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, res.Hash)
	})
	return res.Hash, int64(len(body))
}

func (f *maFixture) createWithFile(userRef int64, hash string) uuid.UUID {
	f.t.Helper()
	ext := "png"
	resp, err := f.h.CreateAsset(f.identity(userRef), openapi.CreateAssetRequestObject{Body: &openapi.AssetCreate{
		Title: "version test", AssetType: 1, FileHash: &hash, FileExtension: &ext,
	}})
	if err != nil {
		f.t.Fatalf("CreateAsset: %v", err)
	}
	ok, isOK := resp.(openapi.CreateAsset201JSONResponse)
	if !isOK {
		f.t.Fatalf("CreateAsset: %T %+v", resp, resp)
	}
	id := uuid.UUID(ok.Id)
	f.t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id) })
	return id
}

func (f *maFixture) versions(asset uuid.UUID) []versionRow {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT id, number, predecessor_id, origin, original_hash, original_size_bytes,
		original_media_type, sealed_at, created_at FROM asset_versions WHERE asset_id = $1 ORDER BY number`, asset)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []versionRow
	for rows.Next() {
		var v versionRow
		if err := rows.Scan(&v.ID, &v.Number, &v.Predecessor, &v.Origin, &v.OriginalHash, &v.Size, &v.MediaType, &v.SealedAt, &v.Created); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func (f *maFixture) currentVersion(asset uuid.UUID) *uuid.UUID {
	f.t.Helper()
	var cur *uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT current_version_id FROM assets WHERE id = $1`, asset).Scan(&cur); err != nil {
		f.t.Fatal(err)
	}
	return cur
}

func (f *maFixture) versionPinned(hash string, version uuid.UUID) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM storage_pins
		WHERE object_hash = $1 AND pin_subject_type = 'asset_version' AND pin_subject_id = $2`, hash, version.String()).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

func (f *maFixture) gcEligibleAt(hash string) *time.Time {
	f.t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT gc_eligible_at FROM storage_objects WHERE hash = $1`, hash).Scan(&at); err != nil {
		f.t.Fatal(err)
	}
	return at
}

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// keepTriggerEnabled re-enables a trigger a test disables inside a
// transaction it expects to be refused: if a regression lets that commit
// through, the trigger must not stay off for the rest of the package.
func (f *maFixture) keepTriggerEnabled(table, trigger string) {
	f.t.Cleanup(func() {
		if _, err := f.pool.Exec(context.Background(), `ALTER TABLE `+table+` ENABLE TRIGGER `+trigger); err != nil {
			f.t.Errorf("re-enable %s: %v", trigger, err)
		}
	})
}

// refused runs q and requires an integrity refusal naming #1596.
func (f *maFixture) refused(what, q string, args ...any) {
	f.t.Helper()
	_, err := f.pool.Exec(f.ctx, q, args...)
	if err == nil || !strings.Contains(err.Error(), "#1596") {
		f.t.Errorf("%s: err %v, want a #1596 refusal", what, err)
	}
}

// requireVersionOne checks the asset's single draft v1 and its pin.
func (f *maFixture) requireVersionOne(who string, asset uuid.UUID, hash string, size int64, origin string) versionRow {
	f.t.Helper()
	vs := f.versions(asset)
	if len(vs) != 1 {
		f.t.Fatalf("%s: %d versions, want exactly v1", who, len(vs))
	}
	v := vs[0]
	if v.Number != 1 || v.Predecessor != nil || v.Origin != origin || v.OriginalHash != hash || v.Size != size || v.MediaType == "" || v.SealedAt != nil {
		f.t.Fatalf("%s: v1 = %+v, want number 1, no predecessor, %s, hash %s, %d bytes, a draft", who, v, origin, hash[:12], size)
	}
	if cur := f.currentVersion(asset); cur == nil || *cur != v.ID {
		f.t.Fatalf("%s: current_version_id %v, want v1 %s", who, cur, v.ID)
	}
	if !f.versionPinned(hash, v.ID) {
		f.t.Fatalf("%s: v1's bytes are not pinned to the version", who)
	}
	if at := f.gcEligibleAt(hash); at != nil {
		f.t.Fatalf("%s: object GC-eligible at %v while its version exists", who, at)
	}
	return v
}

func TestAssetVersions_EveryInsertPathCreatesPinnedVersionOne(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("v-owner")

	// CreateAsset, the API path.
	h1, s1 := f.upload(owner, "create")
	a1 := f.createWithFile(owner, h1)
	f.requireVersionOne("CreateAsset", a1, h1, s1, "ingest")

	// The aiedit job's own INSERT (jobs.go), raw SQL with a file.
	h2, s2 := f.upload(owner, "raw")
	a2 := uuid.New()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_extension, processing_status)
		VALUES ($1, 'raw insert', (SELECT MIN(ref) FROM asset_types), $2, $3, 'png', 'pending')`, a2, owner, h2); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, a2) })
	f.requireVersionOne("raw insert", a2, h2, s2, "ingest")

	// The seeder's insert.
	h3, s3 := f.upload(owner, "seed")
	a3 := uuid.New()
	ext := "png"
	var atype int64
	if err := f.pool.QueryRow(f.ctx, `SELECT MIN(ref) FROM asset_types`).Scan(&atype); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.New(f.pool).SeedInsertAsset(f.ctx, seed.SeedInsertAssetParams{
		ID: pgtype.UUID{Bytes: a3, Valid: true}, Title: "seeded", AssetType: atype, OwnerUserRef: &owner, Status: "active",
		FileHash: &h3, FileExtension: &ext, Metadata: []byte(`{}`), Sensitivity: "public",
		CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(-48 * time.Hour), Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, a3) })
	f.requireVersionOne("seed insert", a3, h3, s3, "ingest")

	// Only the version machinery names a current version.
	f.refused("insert naming its own current version",
		`INSERT INTO assets (title, asset_type, file_hash, current_version_id) VALUES ('x', (SELECT MIN(ref) FROM asset_types), $1, $2)`, h1, uuid.New())
}

func TestAssetVersions_NullFileAndRemoteAssetsAreUnversioned(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("v-null")
	h, _ := f.upload(owner, "remote")
	nullFile, remote := uuid.New(), uuid.New()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO assets (id, title, asset_type) VALUES ($1, 'no bytes', (SELECT MIN(ref) FROM asset_types))`, nullFile); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO assets (id, title, asset_type, file_hash, origin_server_id)
		VALUES ($1, 'received', (SELECT MIN(ref) FROM asset_types), $2, $3)`, remote, h, uuid.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM assets WHERE id IN ($1, $2)`, nullFile, remote)
	})
	for who, id := range map[string]uuid.UUID{"null-file": nullFile, "remote": remote} {
		if vs := f.versions(id); len(vs) != 0 || f.currentVersion(id) != nil {
			t.Errorf("%s asset: %d versions, current %v; want none", who, len(vs), f.currentVersion(id))
		}
	}
	// A remote asset never gets a local version.
	f.refused("a version of a remote asset",
		`SELECT asset_version_create($1, gen_random_uuid(), 1, NULL, 'ingest', $2, 'png', NULL, NULL, NULL)`, remote, h)
}

func TestAssetVersions_RemovingAssetAndUploaderPinsNeverOrphansVersionBytes(t *testing.T) {
	// The negative test for a missing new-ingest version pin: without the
	// asset_version pin, removing the asset's own pin (soft delete does)
	// would leave v1's bytes with no pin and schedule them for GC.
	f := newMAFixture(t)
	owner := f.user("v-pins")
	h, _ := f.upload(owner, "pins")
	a := f.createWithFile(owner, h)
	v1 := f.versions(a)[0]
	for _, pin := range []storage.PinRef{
		{SubjectType: assets.PinSubjectTypeAsset, SubjectID: a.String()},
		{SubjectType: storage.PinSubjectTypeUser, SubjectID: strconv.FormatInt(owner, 10)},
	} {
		if err := f.h.Storage.RemovePin(f.ctx, pin, h); err != nil {
			t.Fatal(err)
		}
	}
	if !f.versionPinned(h, v1.ID) {
		t.Fatal("the version pin went with the asset pin")
	}
	if at := f.gcEligibleAt(h); at != nil {
		t.Fatalf("v1's bytes became GC-eligible at %v after the asset and uploader pins were removed", at)
	}
}

func TestAssetVersions_SoftDeleteKeepsPins_HardPurgeReleasesWithGrace(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("v-purge")
	h, _ := f.upload(owner, "purge")
	a := f.createWithFile(owner, h)
	v1 := f.versions(a)[0]

	// Soft delete (the API path) removes the asset's pin; the version's
	// stays, so the bytes stay.
	resp, err := f.h.DeleteAsset(f.identity(owner), openapi.DeleteAssetRequestObject{Id: a})
	if err != nil {
		t.Fatalf("DeleteAsset: %v", err)
	}
	if _, ok := resp.(openapi.DeleteAsset204Response); !ok {
		t.Fatalf("DeleteAsset: %T", resp)
	}
	if !f.versionPinned(h, v1.ID) || f.gcEligibleAt(h) != nil {
		t.Fatal("soft delete released the version's bytes")
	}

	// Hard purge, past retention, with a non-default grace.
	if _, err := f.pool.Exec(f.ctx, `UPDATE assets SET deleted_at = now() - interval '40 days' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	sd := softdelete.NewService(f.pool, nil)
	sd.GCGrace = 2 * time.Hour
	before := time.Now()
	if _, err := sd.HardDeletePastAssets(f.ctx, 30, 1000); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := len(f.versions(a)); n != 0 {
		t.Fatalf("%d versions survived the purge", n)
	}
	if f.versionPinned(h, v1.ID) {
		t.Fatal("the purge left the version pin")
	}
	at := f.gcEligibleAt(h)
	if at == nil || at.Before(before.Add(2*time.Hour-time.Minute)) || at.After(time.Now().Add(2*time.Hour+time.Minute)) {
		t.Fatalf("after the purge gc_eligible_at = %v, want about now + 2h", at)
	}
}

func TestAssetVersions_DeleteLeavingAVersionPinIsRefused(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("v-dangle")
	h, _ := f.upload(owner, "dangle")
	a := f.createWithFile(owner, h)
	v1 := f.versions(a)[0]

	// With the release trigger switched off for this transaction only, the
	// deferred check must refuse the commit.
	f.keepTriggerEnabled("asset_versions", "asset_versions_release_pins_trg")
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(f.ctx, `ALTER TABLE asset_versions DISABLE TRIGGER asset_versions_release_pins_trg`); err != nil {
		t.Fatalf("disable release: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `DELETE FROM assets WHERE id = $1`, a); err != nil {
		t.Fatalf("delete: %v", err)
	}
	err = tx.Commit(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "removed with its pins still in place") {
		t.Fatalf("commit: %v, want the dangling-pin refusal", err)
	}
	if !f.versionPinned(h, v1.ID) || len(f.versions(a)) != 1 {
		t.Fatal("the refused delete changed something")
	}
}

func TestAssetVersions_DirectlyInsertedVersionIsPinned(t *testing.T) {
	// The retention invariant does not depend on asset_version_create():
	// a raw INSERT of v2, the asset moved to it, and every ordinary pin
	// removed still leave both versions' bytes pinned and not GC-eligible.
	f := newMAFixture(t)
	owner := f.user("v-direct")
	h, _ := f.upload(owner, "direct v1")
	h2, s2 := f.upload(owner, "direct v2")
	a := f.createWithFile(owner, h)
	v1 := f.versions(a)[0]
	// v2's bytes are already scheduled for GC, as unpinned bytes would be.
	if _, err := f.pool.Exec(f.ctx, `UPDATE storage_objects SET gc_eligible_at = now() + interval '1 hour' WHERE hash = $1`, h2); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO asset_versions (id, asset_id, number, predecessor_id, origin, original_hash, original_size_bytes, original_media_type)
		VALUES ($1, $2, 2, $3, 'replacement', $4, $5, 'image/png')`
	f.refused("a direct insert misstating the size", insert, uuid.New(), a, v1.ID, h2, s2+1)
	v2 := uuid.New()
	if _, err := f.pool.Exec(f.ctx, insert, v2, a, v1.ID, h2, s2); err != nil {
		t.Fatalf("direct insert of v2: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE assets SET file_hash = $2, current_version_id = $3 WHERE id = $1`, a, h2, v2); err != nil {
		t.Fatalf("move to v2: %v", err)
	}
	for _, pin := range []struct {
		ref  storage.PinRef
		hash string
	}{
		{storage.PinRef{SubjectType: assets.PinSubjectTypeAsset, SubjectID: a.String()}, h},
		{storage.PinRef{SubjectType: storage.PinSubjectTypeUser, SubjectID: strconv.FormatInt(owner, 10)}, h},
		{storage.PinRef{SubjectType: storage.PinSubjectTypeUser, SubjectID: strconv.FormatInt(owner, 10)}, h2},
	} {
		if err := f.h.Storage.RemovePin(f.ctx, pin.ref, pin.hash); err != nil {
			t.Fatal(err)
		}
	}
	var others int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM storage_pins WHERE object_hash IN ($1, $2) AND pin_subject_type <> 'asset_version'`, h, h2).Scan(&others); err != nil || others != 0 {
		t.Fatalf("%d ordinary pins remain (%v), want 0", others, err)
	}
	for _, c := range []struct {
		what    string
		hash    string
		version uuid.UUID
	}{{"v1", h, v1.ID}, {"directly inserted v2", h2, v2}} {
		if !f.versionPinned(c.hash, c.version) {
			t.Errorf("%s: bytes not pinned to the version", c.what)
		}
		if at := f.gcEligibleAt(c.hash); at != nil {
			t.Errorf("%s: bytes GC-eligible at %v while the version exists", c.what, at)
		}
	}
}

func TestAssetVersions_UnpinnedVersionOrFileIsRefusedAtCommit(t *testing.T) {
	// The deferred backstop: with the pinning trigger switched off in the
	// transaction, an unpinned version or file row cannot commit.
	f := newMAFixture(t)
	owner := f.user("v-unpinned")
	h, _ := f.upload(owner, "unpinned v1")
	h2, s2 := f.upload(owner, "unpinned v2")
	a := f.createWithFile(owner, h)
	v1 := f.versions(a)[0]
	f.keepTriggerEnabled("asset_versions", "asset_versions_pin_trg")
	f.keepTriggerEnabled("asset_version_files", "asset_version_files_write_check_trg")
	for _, c := range []struct {
		what, disable, q string
		args             []any
	}{
		{"a version", `ALTER TABLE asset_versions DISABLE TRIGGER asset_versions_pin_trg`,
			`INSERT INTO asset_versions (asset_id, number, predecessor_id, origin, original_hash, original_size_bytes, original_media_type)
			VALUES ($1, 2, $2, 'replacement', $3, $4, 'image/png')`, []any{a, v1.ID, h2, s2}},
		{"a file row", `ALTER TABLE asset_version_files DISABLE TRIGGER asset_version_files_write_check_trg`,
			`INSERT INTO asset_version_files (version_id, role, object_hash, size_bytes, media_type, derived_from_hash, derived_variant_key)
			VALUES ($1, 'access', $2, $3, 'image/webp', $4, 'hires')`, []any{v1.ID, h2, s2, h}},
	} {
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(f.ctx, c.disable); err != nil {
			t.Fatalf("%s: disable: %v", c.what, err)
		}
		if _, err := tx.Exec(f.ctx, c.q, c.args...); err != nil {
			t.Fatalf("%s: insert: %v", c.what, err)
		}
		err = tx.Commit(f.ctx)
		_ = tx.Rollback(context.Background())
		if err == nil || !strings.Contains(err.Error(), "is not pinned to it") {
			t.Errorf("%s without its pin: commit %v, want the unpinned refusal", c.what, err)
		}
	}
	if vs := f.versions(a); len(vs) != 1 {
		t.Fatalf("a refused commit left %d versions", len(vs))
	}
}

func TestAssetVersions_LiveVersionPinCannotBeRemovedOrMoved(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("v-livepin")
	h, _ := f.upload(owner, "live pin")
	other, _ := f.upload(owner, "live pin other")
	a := f.createWithFile(owner, h)
	v1 := f.versions(a)[0]
	pinOf := `WHERE object_hash = $1 AND pin_subject_type = 'asset_version' AND pin_subject_id = $2`

	f.refused("deleting a live version's pin", `DELETE FROM storage_pins `+pinOf, h, v1.ID.String())
	f.refused("moving the pin to another subject", `UPDATE storage_pins SET pin_subject_id = gen_random_uuid()::text `+pinOf, h, v1.ID.String())
	f.refused("moving the pin to other bytes", `UPDATE storage_pins SET object_hash = $3 `+pinOf, h, v1.ID.String(), other)
	f.refused("retyping the pin", `UPDATE storage_pins SET pin_subject_type = 'user' `+pinOf, h, v1.ID.String())
	if err := f.h.Storage.RemovePin(f.ctx, storage.PinRef{SubjectType: "asset_version", SubjectID: v1.ID.String()}, h); err == nil ||
		!strings.Contains(err.Error(), "#1596") {
		t.Errorf("storage.Service.RemovePin of a live version pin: %v, want the #1596 refusal", err)
	}
	// TRUNCATE skips row triggers; it is refused while versions exist (in a
	// transaction rolled back either way, so a regression cannot wipe pins).
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(f.ctx, `TRUNCATE storage_pins`)
	_ = tx.Rollback(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "cannot be truncated while asset versions exist") {
		t.Errorf("TRUNCATE storage_pins: %v, want the refusal", err)
	}

	// The check runs at commit: a pin removed and restored in one
	// transaction is fine.
	tx, err = f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(f.ctx, `DELETE FROM storage_pins `+pinOf, h, v1.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($1, 'asset_version', $2)`, h, v1.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatalf("remove and restore in one transaction: %v", err)
	}
	if !f.versionPinned(h, v1.ID) || f.gcEligibleAt(h) != nil {
		t.Fatal("v1's bytes lost their pin")
	}
}

func TestAssetVersions_PurgeRacingANewPinNeverSchedulesPinnedBytes(t *testing.T) {
	// Another transaction pins the bytes (row lock, GC cleared, pin
	// inserted: the order every pin path uses) while a purge releases the
	// version's pin. The purge waits for that lock and must then see the
	// committed pin: the bytes end pinned and NOT scheduled for GC.
	f := newMAFixture(t)
	owner := f.user("v-race-gc")
	h, _ := f.upload(owner, "race gc")
	a := f.createWithFile(owner, h)
	for _, pin := range []storage.PinRef{
		{SubjectType: assets.PinSubjectTypeAsset, SubjectID: a.String()},
		{SubjectType: storage.PinSubjectTypeUser, SubjectID: strconv.FormatInt(owner, 10)},
	} {
		if err := f.h.Storage.RemovePin(f.ctx, pin, h); err != nil {
			t.Fatal(err)
		}
	}
	pinner, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pinner.Rollback(context.Background()) }()
	for _, q := range []string{
		`SELECT 1 FROM storage_objects WHERE hash = $1 FOR UPDATE`,
		`UPDATE storage_objects SET gc_eligible_at = NULL WHERE hash = $1`,
		`INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($1, 'user', 'race-gc')`,
	} {
		if _, err := pinner.Exec(f.ctx, q, h); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	purged := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, a)
		purged <- err
	}()
	select {
	case err := <-purged:
		t.Fatalf("the purge did not wait for the pinning transaction's lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := pinner.Commit(f.ctx); err != nil {
		t.Fatalf("pinner commit: %v", err)
	}
	if err := <-purged; err != nil {
		t.Fatalf("purge: %v", err)
	}
	if at := f.gcEligibleAt(h); at != nil {
		t.Fatalf("bytes pinned by a concurrent transaction were scheduled for GC at %v", at)
	}
}

func TestAssetVersions_MetadataEditKeepsVersion_DirectFileChangeRefused(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("v-edit")
	h, _ := f.upload(owner, "edit")
	other, _ := f.upload(owner, "other")
	a := f.createWithFile(owner, h)
	v1 := f.versions(a)[0]

	title := "renamed, metadata only"
	resp, err := f.h.UpdateAsset(f.identity(owner), openapi.UpdateAssetRequestObject{Id: a, Body: &openapi.AssetUpdate{Title: &title}})
	if err != nil {
		t.Fatalf("UpdateAsset: %v", err)
	}
	if _, ok := resp.(openapi.UpdateAsset200JSONResponse); !ok {
		t.Fatalf("UpdateAsset: %T", resp)
	}
	if vs := f.versions(a); len(vs) != 1 || vs[0].ID != v1.ID || *f.currentVersion(a) != v1.ID {
		t.Fatalf("a metadata edit changed the versions: %+v", vs)
	}

	f.refused("direct file_hash change", `UPDATE assets SET file_hash = $2 WHERE id = $1`, a, other)
	f.refused("clearing file_hash", `UPDATE assets SET file_hash = NULL WHERE id = $1`, a)
	f.refused("pointing at a version that does not exist", `UPDATE assets SET current_version_id = gen_random_uuid() WHERE id = $1`, a)
	f.refused("making a versioned asset remote", `UPDATE assets SET origin_server_id = gen_random_uuid() WHERE id = $1`, a)

	// The DB-level shape of a later replacement: a new version, then the
	// asset moves to it with its bytes. Never backwards afterwards.
	v2 := uuid.New()
	if _, err := f.pool.Exec(f.ctx, `SELECT asset_version_create($1, $2, 2, $3, 'replacement', $4, 'png', NULL, NULL, $5)`,
		a, v2, v1.ID, other, owner); err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE assets SET file_hash = $2, current_version_id = $3 WHERE id = $1`, a, other, v2); err != nil {
		t.Fatalf("move to v2: %v", err)
	}
	if !f.versionPinned(h, v1.ID) || !f.versionPinned(other, v2) {
		t.Fatal("both versions' bytes must stay pinned")
	}
	f.refused("moving back to v1", `UPDATE assets SET file_hash = $2, current_version_id = $3 WHERE id = $1`, a, h, v1.ID)
}

func TestAssetVersions_ChainHasNoGapsForksOrForeignPredecessors(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("v-chain")
	h, _ := f.upload(owner, "chain")
	h2, _ := f.upload(owner, "chain2")
	a := f.createWithFile(owner, h)
	b := f.createWithFile(owner, h2)
	v1 := f.versions(a)[0]
	bv1 := f.versions(b)[0]
	create := `SELECT asset_version_create($1, gen_random_uuid(), $2, $3, $4, $5, 'png', NULL, NULL, NULL)`

	f.refused("a second version 1", create, a, 1, nil, "ingest", h)
	f.refused("a gap", create, a, 3, v1.ID, "replacement", h2)
	f.refused("another asset's predecessor", create, a, 2, bv1.ID, "replacement", h2)
	f.refused("a rendition with different bytes", create, a, 2, v1.ID, "rendition", h2)
	if _, err := f.pool.Exec(f.ctx, create, a, 2, v1.ID, "replacement", h2); err != nil {
		t.Fatalf("v2: %v", err)
	}
	// A fork: another successor of v1.
	if _, err := f.pool.Exec(f.ctx, create, a, 2, v1.ID, "replacement", h); err == nil {
		t.Fatal("a fork of v1 was accepted")
	}
	f.refused("a successor of a non-latest version", create, a, 3, v1.ID, "replacement", h)
	f.refused("creating a sealed version", `INSERT INTO asset_versions (asset_id, number, origin, original_hash, original_size_bytes, original_media_type, sealed_xid, sealed_at)
		VALUES ($1, 9, 'replacement', $2, 1, 'image/png', pg_current_xact_id(), now())`, a, h)
}

func TestAssetVersions_DefiningColumnsAreImmutable(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("v-immut")
	h, _ := f.upload(owner, "immut")
	a := f.createWithFile(owner, h)
	v1 := f.versions(a)[0]
	for what, q := range map[string]string{
		"original hash":                 `UPDATE asset_versions SET original_hash = repeat('a', 64) WHERE id = $1`,
		"number":                        `UPDATE asset_versions SET number = 7 WHERE id = $1`,
		"origin":                        `UPDATE asset_versions SET origin = 'backfill_current' WHERE id = $1`,
		"created_at":                    `UPDATE asset_versions SET created_at = now() - interval '1 year' WHERE id = $1`,
		"delete":                        `DELETE FROM asset_versions WHERE id = $1`,
		"announce a draft":              `UPDATE asset_versions SET announced_at = now() WHERE id = $1`,
		"claim for another transaction": `UPDATE asset_versions SET sealed_xid = '1'::xid8 WHERE id = $1`,
	} {
		f.refused(what, q, v1.ID)
	}
}

func TestAssetVersions_SealIsControlledAndFinal(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("v-seal")
	h, size := f.upload(owner, "seal")
	access, accessSize := f.upload(owner, "access copy")
	a := f.createWithFile(owner, h)
	v1 := f.versions(a)[0]
	originalInsert := `INSERT INTO asset_version_files (version_id, role, object_hash, size_bytes, media_type)
		VALUES ($1, 'original', $2, $3, 'image/png')`
	accessEntry := `[{"role":"access","object_hash":"` + access + `","size_bytes":` + strconv.FormatInt(accessSize, 10) +
		`,"media_type":"image/webp","derived_from_hash":"` + h + `","derived_variant_key":"hires"}]`

	// A draft's files cannot be written outside the seal.
	f.refused("a file row outside the seal", originalInsert, v1.ID, h, size)
	// A claim that is not completed in its transaction is refused at commit.
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE asset_versions SET sealed_xid = pg_current_xact_id() WHERE id = $1`, v1.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := tx.Commit(f.ctx); err == nil || !strings.Contains(err.Error(), "claimed for sealing but not sealed") {
		t.Fatalf("commit of an unfinished claim: %v", err)
	}

	// No access copy can be sealed before verified promotion exists: not
	// through the seal procedure (even an existing, correctly sized
	// object), and not by claiming the version by hand and writing the row.
	if _, err := f.pool.Exec(f.ctx, `SELECT asset_version_seal($1, $2::jsonb)`, v1.ID, accessEntry); err == nil ||
		!strings.Contains(err.Error(), "access copies cannot be sealed until verified promotion exists") {
		t.Fatalf("a seal with an access entry: %v, want the refusal", err)
	}
	tx, err = f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(f.ctx, `UPDATE asset_versions SET sealed_xid = pg_current_xact_id() WHERE id = $1`, v1.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, err = tx.Exec(f.ctx, `INSERT INTO asset_version_files (version_id, role, object_hash, size_bytes, media_type, derived_from_hash, derived_variant_key)
		VALUES ($1, 'access', $2, $3, 'image/webp', $4, 'hires')`, v1.ID, access, accessSize, h)
	if err == nil || !strings.Contains(err.Error(), "only the original is accepted until verified access copies exist") {
		t.Fatalf("an access row under a hand-made claim: %v, want the refusal", err)
	}
	_ = tx.Rollback(f.ctx)
	if got := f.versions(a)[0]; got.SealedAt != nil {
		t.Fatal("a refused seal sealed the version")
	}

	// The seal: the original only.
	if _, err := f.pool.Exec(f.ctx, `SELECT asset_version_seal($1)`, v1.ID); err != nil {
		t.Fatalf("seal: %v", err)
	}
	var files int
	var origSize int64
	var origHash string
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*), max(size_bytes) FILTER (WHERE role = 'original'), max(object_hash) FILTER (WHERE role = 'original')
		FROM asset_version_files WHERE version_id = $1`, v1.ID).Scan(&files, &origSize, &origHash); err != nil {
		t.Fatal(err)
	}
	if files != 1 || origSize != size || origHash != h || f.versions(a)[0].SealedAt == nil || !f.versionPinned(h, v1.ID) {
		t.Fatalf("sealed file set: %d files, original %s %d bytes (want 1, %s, %d), pinned %v", files, origHash, origSize, h[:12], size, f.versionPinned(h, v1.ID))
	}
	if f.versionPinned(access, v1.ID) {
		t.Fatal("the refused access copy left a version pin")
	}

	// After the seal nothing in the file set or seal can change.
	f.refused("adding a file after the seal", originalInsert, v1.ID, h, size)
	f.refused("changing a sealed file", `UPDATE asset_version_files SET media_type = 'image/webp' WHERE version_id = $1`, v1.ID)
	f.refused("removing a sealed file", `DELETE FROM asset_version_files WHERE version_id = $1 AND role = 'original'`, v1.ID)
	f.refused("re-sealing", `SELECT asset_version_seal($1)`, v1.ID)
	f.refused("re-sealing with an access copy", `SELECT asset_version_seal($1, $2::jsonb)`, v1.ID, accessEntry)
	f.refused("moving the seal time", `UPDATE asset_versions SET sealed_at = now() + interval '1 day' WHERE id = $1`, v1.ID)
	if _, err := f.pool.Exec(f.ctx, `UPDATE asset_versions SET announced_at = now() WHERE id = $1`, v1.ID); err != nil {
		t.Fatalf("announcing a sealed version: %v", err)
	}
	f.refused("announcing twice", `UPDATE asset_versions SET announced_at = now() WHERE id = $1`, v1.ID)
}

func TestAssetVersions_ConcurrentSealsOneWins(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("v-race")
	h, _ := f.upload(owner, "race")
	a := f.createWithFile(owner, h)
	v1 := f.versions(a)[0]

	first, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(context.Background()) }()
	if _, err := first.Exec(f.ctx, `SELECT asset_version_seal($1)`, v1.ID); err != nil {
		t.Fatalf("first seal: %v", err)
	}
	second := make(chan error, 1)
	go func() {
		_, err := f.pool.Exec(context.Background(), `SELECT asset_version_seal($1)`, v1.ID)
		second <- err
	}()
	// The second seal waits on the version's row lock until the first ends.
	select {
	case err := <-second:
		t.Fatalf("the second seal did not wait for the first: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := first.Commit(f.ctx); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	err = <-second
	if err == nil || !strings.Contains(err.Error(), "already sealed") {
		t.Fatalf("second seal: %v, want already sealed", err)
	}
	var files int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM asset_version_files WHERE version_id = $1`, v1.ID).Scan(&files); err != nil || files != 1 {
		t.Fatalf("file set after two seals: %d (%v), want the one original", files, err)
	}
}
