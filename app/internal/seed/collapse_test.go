// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package seed

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

// W6 (#1319). PRESERVATION, NOT REGRESSION: every assertion here passes
// on the code as it stands, and that is the point. The corpus is losing
// its one same-owner produced-byte duplicate, so the behaviour that made
// the defect visible stops being exercised by the real data. These pin
// it so it cannot drift once nothing else touches it.
//
// What the app does, and why the catalogue may not rely on it:
//
//	`idx_assets_owner_hash_unique` is a partial unique index on
//	(owner_user_ref, file_hash) over live rows. It is IDENTITY: no
//	DedupBehavior value relaxes it (see sysconfig/upload.go). So N
//	catalogue entries describing one owner's identical bytes produce
//	exactly ONE row, whatever N is, and the other N-1 get no id, no
//	declaration, no size and no field values. The seeder counts them
//	`deduped` and carries on, and the verifier fails each one while
//	naming the survivor.
//
// Cross-owner identical bytes are the opposite case and are legal: two
// asset rows over ONE storage object, which is the content-addressed
// store doing its job. Nothing may flag those.

// collapseWorld drives the REAL applyAssets phase over a catalogue,
// with a filesystem storage backend, so the counts under test are the
// ones a seed produces rather than a re-implementation of them.
type collapseWorld struct {
	t        *testing.T
	f        *verifyFixture
	siteRoot string
	r        *Runner
}

func newCollapseWorld(t *testing.T) *collapseWorld {
	t.Helper()
	f := newVerifyFixture(t)
	site := t.TempDir()
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage backend: %v", err)
	}
	r := NewRunner(f.pool, storage.NewService(backend, f.pool),
		Options{Logger: captureLogger(f.log), SiteRoot: site})
	r.adminRef = f.userRef
	r.users[f.username] = f.userRef
	r.assetTypes["image"] = 1
	return &collapseWorld{t: t, f: f, siteRoot: site, r: r}
}

// entry writes `payload` at the manifest entry's path and returns the
// entry. An empty payload writes NO FILE, which is how the
// missing-bytes case is expressed.
func (w *collapseWorld) entry(owner string, payload []byte) manifestAsset {
	w.t.Helper()
	id := uuid.New().String()
	rel := "images/" + id + ".png"
	if len(payload) > 0 {
		abs := filepath.Join(w.siteRoot, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			w.t.Fatal(err)
		}
		if err := os.WriteFile(abs, payload, 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
	w.f.cleanupAsset(parseUUID(id))
	return manifestAsset{
		ID:              id,
		AssetType:       "image",
		Title:           "aa1319 collapse " + id[:8],
		FilePath:        rel,
		FileExtension:   "png",
		FileSizeBytes:   int64(len(payload)),
		SensitivityTier: "public",
		ArchiveState:    "active",
		OwnerUsername:   owner,
		Metadata:        []byte(`{"acquisition_source":"test-fixture"}`),
		CreatedAt:       "2025-05-05T12:00:00Z",
		UpdatedAt:       "2025-05-05T12:00:00Z",
	}
}

// apply runs the phase and returns the runner's own counters, read back
// out of its structured log line rather than recomputed here.
func (w *collapseWorld) apply(assets ...manifestAsset) {
	w.t.Helper()
	if err := w.r.applyAssets(context.Background(), &catalogues{Assets: assets}); err != nil {
		w.t.Fatalf("applyAssets: %v", err)
	}
}

// rowsFor counts the live asset rows among the ids given.
func (w *collapseWorld) rowsFor(assets []manifestAsset) int {
	w.t.Helper()
	n := 0
	for _, a := range assets {
		var exists bool
		if err := w.f.pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1 AND deleted_at IS NULL)`,
			parseUUID(a.ID)).Scan(&exists); err != nil {
			w.t.Fatal(err)
		}
		if exists {
			n++
		}
	}
	return n
}

func (w *collapseWorld) fileHash(id string) *string {
	w.t.Helper()
	var hash *string
	if err := w.f.pool.QueryRow(context.Background(),
		`SELECT file_hash FROM assets WHERE id = $1`, parseUUID(id)).Scan(&hash); err != nil {
		w.t.Fatalf("file_hash %s: %v", id, err)
	}
	return hash
}

// cleanupStorage drops the storage object the group uploaded, by its
// EXACT hash, read back off a row this test wrote. The database is
// shared between suites, so a cleanup that guessed (by size, say, or by
// "the most recent row") could delete another test's object.
func (w *collapseWorld) cleanupStorage(assets ...manifestAsset) {
	w.t.Helper()
	seen := map[string]struct{}{}
	for _, a := range assets {
		if _, ok := w.r.assets[a.ID]; !ok {
			continue
		}
		h := w.fileHash(a.ID)
		if h == nil || *h == "" {
			continue
		}
		seen[*h] = struct{}{}
	}
	for hash := range seen {
		hash := hash
		w.t.Cleanup(func() {
			c := context.Background()
			_, _ = w.f.pool.Exec(c, `UPDATE assets SET file_hash = NULL WHERE file_hash = $1`, hash)
			_, _ = w.f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, hash)
			_, _ = w.f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hash)
		})
	}
}

// TestApplyAssets_SameOwnerIdenticalBytesCollapseToOneRow pins the
// cardinality at N = 2, 3, 4 and 5. One row per GROUP, never per entry,
// and the survivor is the first entry the phase reaches.
func TestApplyAssets_SameOwnerIdenticalBytesCollapseToOneRow(t *testing.T) {
	for _, n := range []int{2, 3, 4, 5} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			w := newCollapseWorld(t)
			payload := []byte("aa1319 identical bytes N=" + fmt.Sprint(n) + " " + w.f.salt)
			var group []manifestAsset
			for i := 0; i < n; i++ {
				group = append(group, w.entry(w.f.username, payload))
			}
			w.apply(group...)
			w.cleanupStorage(group...)

			if got := w.rowsFor(group); got != 1 {
				t.Fatalf("live rows %d, want 1: the (owner_user_ref, file_hash) index "+
					"is identity and cannot hold %d", got, n)
			}
			// The survivor is the FIRST entry, and it is the one the
			// runner's id map carries forward into applyPosts.
			if _, ok := w.r.assets[group[0].ID]; !ok {
				t.Fatalf("the survivor %s is not in the runner's id map, so every "+
					"post naming it would seed without it", group[0].ID)
			}
			for _, lost := range group[1:] {
				if _, ok := w.r.assets[lost.ID]; ok {
					t.Errorf("%s has no row but IS in the id map; applyPosts would "+
						"resolve a member that does not exist", lost.ID)
				}
			}
			if h := w.fileHash(group[0].ID); h == nil || *h == "" {
				t.Errorf("the survivor carries no file_hash, so nothing collapsed onto it")
			}
		})
	}
}

// TestApplyAssets_DedupedCountIsGroupSizeMinusOne reads the phase's own
// counter rather than inferring it. `deduped` is what an operator sees,
// and it is the number that says how many catalogue entries did not
// become assets.
func TestApplyAssets_DedupedCountIsGroupSizeMinusOne(t *testing.T) {
	for _, n := range []int{2, 3, 4, 5} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			w := newCollapseWorld(t)
			payload := []byte("aa1319 counter N=" + fmt.Sprint(n) + " " + w.f.salt)
			var group []manifestAsset
			for i := 0; i < n; i++ {
				group = append(group, w.entry(w.f.username, payload))
			}
			before := w.f.log.Len()
			w.apply(group...)
			w.cleanupStorage(group...)
			line := w.f.log.String()[before:]
			// The phase logs structured JSON, so the assertion has to
			// match what it actually emits. A substring that can never
			// appear is a test that cannot fail, which is the shape this
			// whole file exists to pin.
			want := fmt.Sprintf(`"deduped":%d`, n-1)
			if !containsAll(line, `"msg":"seed.assets"`, want, `"inserted":1`) {
				t.Fatalf("the phase must report one insert and %s\n%s", want, line)
			}
		})
	}
}

// TestApplyAssets_SurvivorIsTheFirstReadableMember pins what happens
// when the first member's bytes are not there. The phase logs
// `seed.asset.open`, counts it `missing`, and moves on, so the survivor
// is the first entry whose file can actually be read. A test that
// assumed "the first entry in the catalogue" would be wrong here, and
// so would any tooling that picked a survivor the same way.
func TestApplyAssets_SurvivorIsTheFirstReadableMember(t *testing.T) {
	w := newCollapseWorld(t)
	payload := []byte("aa1319 first readable " + w.f.salt)
	absent := w.entry(w.f.username, nil) // no file written
	second := w.entry(w.f.username, payload)
	third := w.entry(w.f.username, payload)
	w.apply(absent, second, third)
	w.cleanupStorage(absent, second, third)

	if _, ok := w.r.assets[absent.ID]; ok {
		t.Errorf("an entry whose bytes are unreadable must not enter the id map")
	}
	if _, ok := w.r.assets[second.ID]; !ok {
		t.Fatalf("the first READABLE member must be the survivor")
	}
	if got := w.rowsFor([]manifestAsset{absent, second, third}); got != 1 {
		t.Fatalf("live rows %d, want 1", got)
	}
	if !containsAll(w.f.log.String(), "seed.asset.open", absent.ID) {
		t.Errorf("the unreadable member must be named in the log, not skipped in silence")
	}
}

// TestApplyAssets_CrossOwnerIdenticalBytesKeepTwoRows is the other half,
// and it is the one a widened rule would break. Identity is per OWNER,
// so two users holding the same bytes get two asset rows over ONE
// storage object. That is the content-addressed store working, and the
// corpus keeps such a group on purpose as the dedup fixture.
func TestApplyAssets_CrossOwnerIdenticalBytesKeepTwoRows(t *testing.T) {
	w := newCollapseWorld(t)
	ctx := context.Background()

	other := "aa1319_other_" + w.f.salt
	var otherRef int64
	if err := w.f.pool.QueryRow(ctx,
		`INSERT INTO "user" (username, password) VALUES ($1, '') RETURNING ref`,
		other).Scan(&otherRef); err != nil {
		t.Fatalf("insert second user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = w.f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, otherRef)
	})
	w.r.users[other] = otherRef

	payload := []byte("aa1319 cross owner " + w.f.salt)
	mine := w.entry(w.f.username, payload)
	theirs := w.entry(other, payload)
	w.apply(mine, theirs)
	w.cleanupStorage(mine, theirs)

	if got := w.rowsFor([]manifestAsset{mine, theirs}); got != 2 {
		t.Fatalf("live rows %d, want 2: identity is per OWNER and these are two "+
			"different owners", got)
	}
	a, b := w.fileHash(mine.ID), w.fileHash(theirs.ID)
	if a == nil || b == nil || *a != *b {
		t.Fatalf("both rows must reference the SAME storage object: %v vs %v", a, b)
	}
	var objects int
	if err := w.f.pool.QueryRow(ctx,
		`SELECT count(*) FROM storage_objects WHERE hash = $1`, *a).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if objects != 1 {
		t.Fatalf("storage objects %d, want 1: the bytes are stored once", objects)
	}
	if !containsAll(w.f.log.String(), `"msg":"seed.assets"`, `"inserted":2`, `"deduped":0`) {
		t.Errorf("a cross-owner pair must be two inserts and zero dedups:\n%s",
			w.f.log.String())
	}
}

// TestVerify_CollapsedGroupFailsEveryLoserAndNamesTheSurvivor extends
// the existing N=2 diagnosis (verify_test.go) to a larger group: the
// verifier must fail N-1 entries, not one, and each failure must name
// the survivor so the remedy is obvious without a database.
func TestVerify_CollapsedGroupFailsEveryLoserAndNamesTheSurvivor(t *testing.T) {
	f := newVerifyFixture(t)
	ctx := context.Background()
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := storage.NewService(backend, f.pool)
	payload := []byte("aa1319 group bytes " + f.salt)

	survivor := f.newAsset("", int64(len(payload)))
	survivor.CollectionName = ""
	up, err := svc.UploadOriginal(ctx, newReader(payload), "image/png",
		storage.PinRef{SubjectType: "asset", SubjectID: survivor.ID})
	if err != nil {
		t.Fatalf("UploadOriginal: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `UPDATE assets SET file_hash = NULL WHERE file_hash = $1`, up.Hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, up.Hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, up.Hash)
	})
	if _, err := f.pool.Exec(ctx,
		`UPDATE assets SET file_hash = $1 WHERE id = $2`, up.Hash, parseUUID(survivor.ID)); err != nil {
		t.Fatal(err)
	}

	losers := make([]manifestAsset, 0, 3)
	for i := 0; i < 3; i++ {
		id := uuid.New().String()
		losers = append(losers, manifestAsset{
			ID: id, AssetType: "image", Title: "collapsed " + id[:8],
			FilePath: "images/collapsed-" + id + ".png", FileExtension: "png",
			FileSizeBytes: int64(len(payload)), SensitivityTier: "public",
			ArchiveState: "active", OwnerUsername: f.username,
			CreatedAt: "2025-05-05T12:00:00Z", UpdatedAt: "2025-05-05T12:00:00Z",
		})
	}
	f.assets = append(f.assets, survivor)
	f.assets = append(f.assets, losers...)
	f.writeSite()

	write := func(rel string, body []byte) {
		p := filepath.Join(f.siteRoot, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(survivor.FilePath, payload)
	for _, l := range losers {
		write(l.FilePath, payload)
	}

	rep := f.verify(VerifyOptions{})
	if rep.AssetsCollapsed != len(losers) {
		t.Errorf("collapsed assets %d, want %d: the diagnosis is per ENTRY, and a "+
			"group of 4 loses 3", rep.AssetsCollapsed, len(losers))
	}
	if rep.OK() {
		t.Fatal("a collapsed profile asset must fail the acceptance run")
	}
	for _, l := range losers {
		mustContain(t, rep.Failures, l.ID, "absent from the database",
			"collapsed it by content address", survivor.ID, "same owner, same bytes")
	}
}

// containsAll is a local helper so these tests read as assertions about
// one log line rather than as four strings.Contains calls.
func containsAll(hay string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(hay, n) {
			return false
		}
	}
	return true
}

func newReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
