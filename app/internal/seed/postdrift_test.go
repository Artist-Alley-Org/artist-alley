// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package seed

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------
// #1320. A reseed can add rows but it can never correct one.
//
// These drive the REAL posts phase against a live database, twice, the
// way `aa seed` is actually used. The unit tests below them pin the
// comparison rules the phase depends on.
// ---------------------------------------------------------------------

// postDriftFixture is a minimal but real seeded world: a user, three
// assets, a collection, and a Runner wired to them.
type postDriftFixture struct {
	pool       *pgxpool.Pool
	r          *Runner
	log        *bytes.Buffer
	assets     [3]pgtype.UUID
	assetKeys  [3]string
	collection pgtype.UUID
	collName   string
}

func newPostDriftFixture(t *testing.T) *postDriftFixture {
	t.Helper()
	pool := openCompanionTestPool(t)
	ctx := context.Background()
	salt := strconv.FormatInt(time.Now().UnixNano()%1e9, 36)

	var ref int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, password) VALUES ($1, '') RETURNING ref`,
		"aa1320_"+salt).Scan(&ref); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})

	f := &postDriftFixture{pool: pool, log: &bytes.Buffer{}}
	for i := range f.assets {
		f.assets[i] = newCompanionTestAsset(t, pool)
		f.assetKeys[i] = "aa1320-asset-" + strconv.Itoa(i) + "-" + salt
	}

	f.collName = "aa1320 collection " + salt
	if err := pool.QueryRow(ctx,
		`INSERT INTO collections (name, owner_user_ref) VALUES ($1, $2) RETURNING id`,
		f.collName, ref).Scan(&f.collection); err != nil {
		t.Fatalf("insert collection: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM collection_posts WHERE collection_id = $1`, f.collection)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE id = $1`, f.collection)
	})

	f.r = NewRunner(pool, nil, Options{Logger: captureLogger(f.log)})
	f.r.adminRef = ref
	for i, key := range f.assetKeys {
		f.r.assets[key] = f.assets[i]
	}
	f.r.collections[f.collName] = f.collection
	return f
}

// post builds a catalogue entry over the fixture's assets.
func (f *postDriftFixture) post(t *testing.T, id string, memberIdx ...int) manifestPost {
	t.Helper()
	p := manifestPost{
		ID:             id,
		Title:          "aa1320 original title",
		Description:    "original description",
		CreatedAt:      "2025-01-01T00:00:00Z",
		UpdatedAt:      "2025-01-02T00:00:00Z",
		Tags:           []string{"alpha", "beta"},
		CollectionName: f.collName,
	}
	for _, i := range memberIdx {
		p.AssetIDs = append(p.AssetIDs, f.assetKeys[i])
	}
	f.cleanupPost(t, id)
	return p
}

func (f *postDriftFixture) cleanupPost(t *testing.T, id string) {
	t.Helper()
	pgID := parseUUID(id)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM collection_posts WHERE post_id = $1`, pgID)
		_, _ = f.pool.Exec(c, `DELETE FROM post_tags WHERE post_id = $1`, pgID)
		_, _ = f.pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, pgID)
		_, _ = f.pool.Exec(c, `DELETE FROM posts WHERE id = $1`, pgID)
	})
}

func (f *postDriftFixture) run(t *testing.T, posts ...manifestPost) *postDrift {
	t.Helper()
	// A fresh Runner per pass, exactly as a second `aa seed` invocation
	// is: no map, no counter and no genTime survives the process.
	r := NewRunner(f.pool, nil, Options{Logger: captureLogger(f.log)})
	r.adminRef = f.r.adminRef
	for k, v := range f.r.assets {
		r.assets[k] = v
	}
	for k, v := range f.r.collections {
		r.collections[k] = v
	}
	cat := &catalogues{Posts: posts}
	if err := r.applyPosts(context.Background(), cat); err != nil {
		t.Fatalf("applyPosts: %v", err)
	}
	if r.postDrift == nil {
		t.Fatal("applyPosts left no drift report at all")
	}
	return r.postDrift
}

func (f *postDriftFixture) countPosts(t *testing.T, ids ...string) int {
	t.Helper()
	n := 0
	for _, id := range ids {
		var c int
		if err := f.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM posts WHERE id = $1`, parseUUID(id)).Scan(&c); err != nil {
			t.Fatalf("count posts: %v", err)
		}
		n += c
	}
	return n
}

// ⛔ THE PROPERTY THE FIX MUST NOT BREAK, AND WITHOUT WHICH IT IS
// UNFALSIFIABLE. queries.sql states it: "all inserts are idempotent
// (ON CONFLICT DO NOTHING) so a re-run against a partially-seeded DB is
// a no-op rather than a hard error." Every line of #1320's reporting
// sits on top of that clause, so a change that reported drift by
// weakening idempotence would have traded one silent loss for a louder
// one. `resume_test.go` is the model, one table over.
func TestApplyPosts_ResumeDuplicatesNothingAndReportsNoDrift(t *testing.T) {
	f := newPostDriftFixture(t)
	p := f.post(t, uuid.New().String(), 0, 1)

	first := f.run(t, p)
	if first.resumed != 0 || first.drifted != 0 || len(first.orphans) != 0 {
		t.Fatalf("a first seed against an empty wall reported resumed=%d drifted=%d orphans=%d; "+
			"it wrote every row itself and can have found nothing",
			first.resumed, first.drifted, len(first.orphans))
	}

	// The interrupted-seed case: the identical catalogue, run again.
	second := f.run(t, p)
	if second.resumed != 1 {
		t.Errorf("a re-run reported resumed=%d, want 1. The phase has to COUNT what it "+
			"declined to touch, or a run that wrote nothing reads like a run that wrote "+
			"everything", second.resumed)
	}
	if second.drifted != 0 {
		t.Errorf("an unchanged catalogue reported %d drifted post(s) on re-seed (%s). "+
			"Two seeds over the same catalogue is the RESUME CONTRACT and must stay a "+
			"silent no-op", second.drifted, second.fieldSummary())
	}
	if len(second.orphans) != 0 {
		t.Errorf("a re-run reported %d orphan(s); the post landed on its own id",
			len(second.orphans))
	}
	if s := second.summary(); s != "" {
		t.Errorf("a clean resume printed a warning:\n%s", s)
	}
	if n := f.countPosts(t, p.ID); n != 1 {
		t.Errorf("two seeds left %d rows under one post id, want 1", n)
	}
}

// ⛔ THE REPORTED DEFECT. A reseed picking up a corpus change appears to
// work and does not: the post row, its members, its tags and its
// collection all stay at whatever the first seed wrote, and the run
// logs its counts and exits clean.
func TestApplyPosts_ADriftedCatalogueIsCountedNotWavedThrough(t *testing.T) {
	f := newPostDriftFixture(t)
	id := uuid.New().String()
	f.run(t, f.post(t, id, 0, 1))

	// Everything the last four sprints have actually changed about a
	// post: its title, its member order, its membership and its tags.
	changed := f.post(t, id, 1, 0, 2)
	changed.Title = "aa1320 corrected title"
	changed.Tags = []string{"alpha", "beta", "gamma"}

	got := f.run(t, changed)
	if got.drifted != 1 {
		t.Fatalf("a changed catalogue reported %d drifted post(s), want 1. "+
			"A reseed that cannot apply the change must not report a clean success", got.drifted)
	}
	for _, want := range []string{"title", "members", "tags"} {
		if got.fields[want] != 1 {
			t.Errorf("drift on %q was not reported (fields: %s)", want, got.fieldSummary())
		}
	}
	msg := got.summary()
	if msg == "" {
		t.Fatal("drift was counted but nothing was printed; a counter nobody reads is the bug")
	}
	if !strings.Contains(msg, "aa seed --reset") {
		t.Errorf("the warning does not name the remedy:\n%s", msg)
	}
	if !strings.Contains(msg, id) {
		t.Errorf("the warning does not name the post:\n%s", msg)
	}

	// And the row really is unchanged, which is the half that makes the
	// report necessary rather than decorative.
	var title string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT title FROM posts WHERE id = $1`, parseUUID(id)).Scan(&title); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if title != "aa1320 original title" {
		t.Fatalf("the row changed to %q; this test asserts the REPORT, and it is only "+
			"needed because the write does not happen", title)
	}
}

// A post that gains a collection it was not in is drift too. The
// membership loop sat AFTER the `continue`, so an existing post could
// not be moved, and nothing counted it.
func TestApplyPosts_AMovedCollectionIsDrift(t *testing.T) {
	f := newPostDriftFixture(t)
	id := uuid.New().String()
	first := f.post(t, id, 0)
	first.CollectionName = ""
	f.run(t, first)

	got := f.run(t, f.post(t, id, 0))
	if got.fields["collection"] != 1 {
		t.Errorf("a post the catalogue moved into a collection reported no collection "+
			"drift (fields: %s)", got.fieldSummary())
	}
}

// ⛔ AN EDIT IS NOT DRIFT. The seeder's subtree inserts are all
// `ON CONFLICT DO NOTHING` and nothing in the package removes a row, so
// it can only ever ADD a tag or a collection. Reporting somebody's extra
// tag as a failed seed would make the warning fire on a database being
// used exactly as intended, which is how a warning gets ignored.
func TestApplyPosts_AnExtraTagOnTheRowIsNotDrift(t *testing.T) {
	f := newPostDriftFixture(t)
	id := uuid.New().String()
	f.run(t, f.post(t, id, 0, 1))

	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO post_tags (post_id, tag) VALUES ($1, 'hand-added')`,
		parseUUID(id)); err != nil {
		t.Fatalf("hand-add tag: %v", err)
	}

	got := f.run(t, f.post(t, id, 0, 1))
	if got.drifted != 0 {
		t.Errorf("a hand-added tag was reported as drift (fields: %s)", got.fieldSummary())
	}
}

// ⛔ THE ORPHAN. runner.go documents "post ids are stableUUID-derived,
// so a re-run re-composes the same posts and lands on SeedInsertPost's
// ON CONFLICT path rather than duplicating the wall". #1310 moved 618
// post ids across the three catalogues, so after #1319 republishes, a
// migrated post inserts as a NEW row while the row under its old id
// remains. The only removal path in the package is Reset's TRUNCATE.
//
// ⚠️ THE CONDITION IS NOT YET REACHABLE FROM THE PUBLISHED DATASET, on
// purpose: catalogues.go reads MANIFEST.json and posts.json from the
// SITE ROOT, so the migrated ids reach no database until the republish.
// The state is therefore constructed directly rather than argued about.
func TestApplyPosts_AMigratedIdLeavesAnOrphanAndSaysSo(t *testing.T) {
	f := newPostDriftFixture(t)
	oldID := uuid.New().String()
	f.run(t, f.post(t, oldID, 0, 1))

	// The same post, same assets, under the id #1310 gave it.
	newID := uuid.New().String()
	got := f.run(t, f.post(t, newID, 0, 1))

	if len(got.orphans) != 1 {
		t.Fatalf("a migrated post id left %d orphan(s), want 1. Both rows now exist and "+
			"both reach the wall", len(got.orphans))
	}
	o := got.orphans[0]
	if o.catalogueID != newID || o.existingID != oldID {
		t.Errorf("orphan named %s/%s, want catalogue %s duplicating existing %s",
			o.catalogueID, o.existingID, newID, oldID)
	}
	msg := got.summary()
	if !strings.Contains(msg, oldID) || !strings.Contains(msg, newID) {
		t.Errorf("the warning names neither row:\n%s", msg)
	}
	if !strings.Contains(msg, "aa seed --reset") {
		t.Errorf("the warning does not name the remedy:\n%s", msg)
	}
	if n := f.countPosts(t, oldID, newID); n != 2 {
		t.Fatalf("expected both rows to exist (that is the bug being reported), got %d", n)
	}
}

// ⛔⛔ THE FALSE POSITIVE THAT WOULD HAVE MADE THE WARNING USELESS.
// Measured 2026-08-27 on the published site_a wall: 861 posts hold only
// 785 distinct member sets, so 76 legitimately frame the same assets as
// a sibling. A solo showcase and a team roundup over one asset are not
// duplicates of each other, and reporting them would fire on EVERY
// FRESH SEED.
func TestApplyPosts_TwoCatalogueSiblingsOverOneMemberSetAreNotOrphans(t *testing.T) {
	f := newPostDriftFixture(t)
	a := f.post(t, uuid.New().String(), 0, 1)
	b := f.post(t, uuid.New().String(), 0, 1)
	b.Title = "the roundup that frames the same two assets"

	got := f.run(t, a, b)
	if len(got.orphans) != 0 {
		t.Fatalf("two catalogue posts over one member set reported %d orphan(s). "+
			"76 of the published wall's 861 posts are this shape, so it would fire "+
			"on every fresh seed", len(got.orphans))
	}
}

// ⛔ AND THE SAME SHAPE ARRIVING LATER IS STILL NOT AN ORPHAN. A new
// post framing assets an older post already frames is ordinary. What
// separates a migrated id is that the row left behind is one NOTHING IN
// THE CATALOGUE WILL EVER TOUCH AGAIN.
func TestApplyPosts_ANewPostOverAlreadyFramedAssetsIsNotAnOrphan(t *testing.T) {
	f := newPostDriftFixture(t)
	existing := f.post(t, uuid.New().String(), 0, 1)
	f.run(t, existing)

	// Second seed: the older post is STILL in the catalogue, and a new
	// one arrives over the same assets.
	arriving := f.post(t, uuid.New().String(), 0, 1)
	got := f.run(t, existing, arriving)
	if len(got.orphans) != 0 {
		t.Fatalf("a new post over already-framed assets reported %d orphan(s); the "+
			"post it matched is still named by the catalogue", len(got.orphans))
	}
}

// Two posts over DIFFERENT assets are not orphans of each other, or
// every ordinary seed would report the whole wall.
func TestApplyPosts_DistinctMembershipsAreNotOrphans(t *testing.T) {
	f := newPostDriftFixture(t)
	f.run(t, f.post(t, uuid.New().String(), 0, 1))
	got := f.run(t, f.post(t, uuid.New().String(), 2))
	if len(got.orphans) != 0 {
		t.Errorf("two posts over different assets reported %d orphan(s)", len(got.orphans))
	}
}

// ---------------------------------------------------------------------
// The comparison rules, without a database.
// ---------------------------------------------------------------------

func TestPostSubjectCompare_MemberOrderIsADisagreement(t *testing.T) {
	a := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	b := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	s := postSubject{members: []pgtype.UUID{a, b}}
	// `sort_order` is the wall's hero placement, which is the whole of
	// the #1309 curation. Comparing members as a SET would call every
	// reordering a no-op.
	got := s.compare(SeedListPostFingerprintsRow{MemberIds: []pgtype.UUID{b, a}})
	if len(got) != 1 || got[0] != "members" {
		t.Errorf("a reordered membership reported %v, want [members]", got)
	}
}

func TestPostSubjectCompare_ClampedDatesAreNotCompared(t *testing.T) {
	// clampToPast reflects a future-dated timestamp around the instant
	// the run started, so the stored value is a function of THIS run and
	// differs between two correct seeds.
	past := pgtype.Timestamptz{Time: time.Unix(1000, 0).UTC(), Valid: true}
	other := pgtype.Timestamptz{Time: time.Unix(2000, 0).UTC(), Valid: true}
	s := postSubject{created: past, updated: past, datesComparable: false}
	if got := s.compare(SeedListPostFingerprintsRow{
		CreatedAt: other, UpdatedAt: other}); len(got) != 0 {
		t.Errorf("a reflected date was compared anyway and reported %v; the site_a "+
			"catalogue carries dates out to 2026-12-14, so this would cry wolf on "+
			"dozens of posts every reseed", got)
	}
	s.datesComparable = true
	if got := s.compare(SeedListPostFingerprintsRow{
		CreatedAt: other, UpdatedAt: other}); len(got) != 2 {
		t.Errorf("an unclamped date disagreement reported %v, want both dates", got)
	}
}

func TestDatesSurviveTheClamp(t *testing.T) {
	r := &Runner{genTime: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)}
	if !r.datesSurviveTheClamp("2025-01-01T00:00:00Z", "2025-06-01T00:00:00Z") {
		t.Error("two past dates are written verbatim and must be comparable")
	}
	if r.datesSurviveTheClamp("2025-01-01T00:00:00Z", "2026-12-14T00:00:00Z") {
		t.Error("a future updated_at is reflected, so the PAIR is not comparable")
	}
	if !r.datesSurviveTheClamp("", "") {
		t.Error("an absent timestamp is not a clamped one")
	}
}

func TestMemberDigest_IsOrderIndependentAndEmptyForNoMembers(t *testing.T) {
	a := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	b := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if memberDigest([]pgtype.UUID{a, b}) != memberDigest([]pgtype.UUID{b, a}) {
		t.Error("the orphan digest must key on the SET of assets, not their order")
	}
	// applyPosts skips a post whose members all fell out, so "no
	// members" is an absence and must never match another absence.
	if memberDigest(nil) != "" {
		t.Error("an empty membership must not be an identity")
	}
}

// A counter nobody reads is the bug this sprint is about, so the hop
// from the phase's report to the run's summary is pinned rather than
// left to compile-checking.
func TestWithDriftCounts_CarriesTheReportOntoTheSummary(t *testing.T) {
	r := &Runner{}
	if got := r.withDriftCounts(Counts{Posts: 9}); got.PostsDrifted != 0 ||
		got.PostsOrphaned != 0 {
		t.Errorf("a run whose posts phase never ran reported drift: %+v", got)
	}
	d := newPostDrift()
	d.note("p1", []string{"title"})
	d.noteOrphan("new", "old")
	r.postDrift = d
	got := r.withDriftCounts(Counts{Posts: 9})
	if got.PostsDrifted != 1 || got.PostsOrphaned != 1 {
		t.Errorf("the phase found 1 drift and 1 orphan; the summary says "+
			"drifted=%d orphaned=%d", got.PostsDrifted, got.PostsOrphaned)
	}
	if got.Posts != 9 {
		t.Errorf("the row counts were disturbed: %+v", got)
	}
}

func TestPostDriftSummary_IsSilentWhenThereIsNothingToSay(t *testing.T) {
	d := newPostDrift()
	d.resumed = 400
	if s := d.summary(); s != "" {
		t.Errorf("400 clean resumes printed:\n%s", s)
	}
}
