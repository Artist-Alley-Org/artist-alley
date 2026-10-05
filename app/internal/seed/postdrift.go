// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package seed

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// postdrift.go. A reseed can add rows but it can never correct one, so
// it has to say so (#1320).
//
// THE DEFECT. `SeedInsertPost` ends in `ON CONFLICT (id) DO NOTHING`,
// and that clause is CORRECT for what it was written for: queries.sql
// states the contract at the head of the seed block, "all inserts are
// idempotent so a re-run against a partially-seeded DB is a no-op
// rather than a hard error", and an interrupted seed has to be able to
// finish. What the clause cannot do is tell anyone. A second seed
// carrying a corrected title, a member the catalogue has since gained,
// a moved collection or a reordered wall lands on it and the run logs
// its insert count and exits clean, over data it did not write. Every
// value the last four sprints fixed stayed at whatever the first seed
// put there, and nothing anywhere said so.
//
// ⛔ AND THE SUBTREE WENT WITH IT, WHICH IS THE HALF THAT MADE IT
// INVISIBLE. The conflict path used to `continue`, which skips the
// member loop, the tag loop AND the collection membership below it. So
// an existing post could not gain the member the catalogue had just
// given it, could not be moved, and could not be retagged. That is
// #1290 one table over: "the symptom was silent: the post existed, the
// wall looked fine, and only its membership was wrong."
//
// ⛔ WHY THIS REPORTS RATHER THAN REPAIRS. `DO UPDATE` was the obvious
// move and it is the wrong one:
//
//   - it destroys the resume contract's whole point. The clause exists
//     so that re-running is safe; a clause that overwrites is not safe
//     to re-run, it is a republish.
//   - `aa seed` runs against development databases people poke at, and
//     an override is data resolved OVER a shipped value, never written
//     in place of it (ADR 0081). A seed that silently reverted a hand
//     edit would be the same class of silent loss one level up.
//   - `manifest_guard` settled the same question for the archive:
//     report the disagreement, refuse to imply it does not exist.
//
// So the phase reads back what it would have written, names every
// disagreement, and points at `aa seed --reset`, which is the one thing
// in the package that can actually apply a changed catalogue.
//
// ⛔ THE EXIT CODE IS DELIBERATELY UNCHANGED. Drift is not a failed
// seed: every phase did everything it is allowed to do. The remedy is
// `--reset`, which is destructive and cannot be taken automatically, and
// the demo box reseeds on a timer. A non-zero exit would turn a corpus
// change into a broken cron with no safe automatic response. The signal
// is a counted, named warning on stdout, where the previews notice
// already goes, and structured counters on the phase's log line.

// postSubject is the state the posts phase WOULD write for one
// catalogue entry, gathered at the point the insert is attempted so the
// comparison uses exactly the values the insert used.
type postSubject struct {
	title       string
	description string
	visibility  string
	cover       pgtype.UUID
	created     pgtype.Timestamptz
	updated     pgtype.Timestamptz
	members     []pgtype.UUID
	tags        []string
	collection  pgtype.UUID

	// datesComparable is false when this run's clamp reflected a
	// future-dated catalogue timestamp (#1174 clampToPast). The written
	// value is then a function of the run's OWN start instant, so it
	// differs between two correct runs and says nothing about drift.
	// Measured 2026-08-27: the site_a catalogue still carries dates out
	// to 2026-12-14, so comparing them unconditionally would report
	// dozens of posts as drifted on every reseed and train the reader to
	// ignore the warning.
	datesComparable bool
}

// postDrift is the posts phase's honesty report.
type postDrift struct {
	// resumed counts posts that were already in the database. It is a
	// count of what the phase DECLINED TO TOUCH, which nothing reported
	// before: the old log line named inserted, skipped_no_members and
	// public, so a run that wrote nothing at all was indistinguishable
	// from a run that wrote everything.
	resumed int

	// fields[name] is how many existing posts disagree with the
	// catalogue on that value. A post can appear under several.
	fields map[string]int

	// drifted is the number of distinct posts in `fields`.
	drifted int

	// samples holds the first few drifted post ids, for the message.
	samples []string

	// orphans are catalogue posts inserted under an id the database did
	// not hold while a post carrying exactly the same assets remains
	// under a different one. See noteOrphan.
	orphans []postOrphan
}

// postOrphan is one migrated-id collision: both rows now exist and both
// reach the wall.
type postOrphan struct {
	catalogueID string
	existingID  string
}

const driftSampleLimit = 5

func newPostDrift() *postDrift {
	return &postDrift{fields: map[string]int{}}
}

// index is the database's side of the comparison: every post row as it
// stands, plus a lookup from an exact member set to the post that holds
// it.
type postIndex struct {
	byID      map[string]SeedListPostFingerprintsRow
	byMembers map[string]string // member digest -> post id
}

// loadPostIndex reads every live post back before the phase writes
// anything.
//
// ⚠️ IT IS READ ONCE, BEFORE THE LOOP, AND DELIBERATELY NOT REFRESHED
// OR ADDED TO. The point of comparison is the database as the run
// FOUND it. Nothing this run writes is ever compared against the
// catalogue that wrote it.
//
// ⛔ THAT IS NOT A TIDINESS RULE, IT IS THE DIFFERENCE BETWEEN A
// WARNING AND A NUISANCE. Registering each insert in the member index
// looks like it would catch a catalogue that carries one membership
// twice. Measured 2026-08-27 on the published site_a wall: 861 posts
// hold only 785 distinct member sets, so 76 of them would report
// themselves as duplicates of a sibling on EVERY FRESH SEED. They are
// not duplicates. A solo showcase and a team roundup legitimately frame
// the same asset.
func (r *Runner) loadPostIndex(ctx context.Context) (*postIndex, error) {
	rows, err := r.q.SeedListPostFingerprints(ctx)
	if err != nil {
		return nil, fmt.Errorf("read existing posts: %w", err)
	}
	idx := &postIndex{
		byID:      make(map[string]SeedListPostFingerprintsRow, len(rows)),
		byMembers: make(map[string]string, len(rows)),
	}
	for _, row := range rows {
		id := uuidString(row.ID)
		idx.byID[id] = row
		if d := memberDigest(row.MemberIds); d != "" {
			// First writer wins. A digest already claimed means the
			// database itself holds two posts over one member set, which
			// is a pre-existing condition and not this run's doing.
			if _, seen := idx.byMembers[d]; !seen {
				idx.byMembers[d] = id
			}
		}
	}
	return idx, nil
}

// memberDigest keys a post by the SET of assets it frames, order
// independent. Empty for a post with no members, which must never match
// anything: applyPosts skips a post whose members all fell out, so "no
// members" is an absence, not an identity.
func memberDigest(members []pgtype.UUID) string {
	if len(members) == 0 {
		return ""
	}
	parts := make([]string, 0, len(members))
	for _, m := range members {
		parts = append(parts, uuidString(m))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// compare names every value on which an existing row disagrees with what
// the catalogue would have written.
//
// ⛔ THE THREE SUBTREE COMPARISONS ARE SUBSET, NOT EQUALITY, AND THE
// ASYMMETRY IS THE POINT. Every subtree insert this phase makes is
// `ON CONFLICT DO NOTHING` and nothing in the package removes a row, so
// the seeder can only ever ADD a member, a tag or a collection. A value
// the catalogue names and the row lacks is therefore something the seed
// failed to apply. An extra tag or an extra collection on the row is
// somebody's edit, and reporting an edit as drift would make the
// warning fire on a database being used exactly as intended.
//
// Members are the exception and are compared in ORDER. `sort_order` is
// the wall's hero placement, which is the whole of the curation work
// (#1309), so a reordering is a real disagreement and not an addition.
func (s postSubject) compare(have SeedListPostFingerprintsRow) []string {
	var out []string
	if s.title != have.Title {
		out = append(out, "title")
	}
	if s.description != have.Description {
		out = append(out, "description")
	}
	if s.visibility != have.Visibility {
		out = append(out, "visibility")
	}
	if uuidString(s.cover) != uuidString(have.CoverAssetID) {
		out = append(out, "cover")
	}
	if s.datesComparable {
		if !sameInstant(s.created, have.CreatedAt) {
			out = append(out, "created_at")
		}
		if !sameInstant(s.updated, have.UpdatedAt) {
			out = append(out, "updated_at")
		}
	}
	if !sameOrderedUUIDs(s.members, have.MemberIds) {
		out = append(out, "members")
	}
	if !coversStrings(have.Tags, s.tags) {
		out = append(out, "tags")
	}
	if s.collection.Valid && !containsUUID(have.CollectionIds, s.collection) {
		out = append(out, "collection")
	}
	return out
}

func sameInstant(a, b pgtype.Timestamptz) bool {
	if a.Valid != b.Valid {
		return false
	}
	if !a.Valid {
		return true
	}
	return a.Time.Equal(b.Time)
}

func sameOrderedUUIDs(want, have []pgtype.UUID) bool {
	if len(want) != len(have) {
		return false
	}
	for i := range want {
		if uuidString(want[i]) != uuidString(have[i]) {
			return false
		}
	}
	return true
}

func containsUUID(haystack []pgtype.UUID, needle pgtype.UUID) bool {
	want := uuidString(needle)
	for _, h := range haystack {
		if uuidString(h) == want {
			return true
		}
	}
	return false
}

// coversStrings reports whether every wanted value is present in have.
func coversStrings(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, h := range have {
		set[h] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

// note records one existing post's disagreements.
func (d *postDrift) note(postID string, fields []string) {
	if len(fields) == 0 {
		return
	}
	d.drifted++
	for _, f := range fields {
		d.fields[f]++
	}
	if len(d.samples) < driftSampleLimit {
		d.samples = append(d.samples, postID)
	}
}

// noteOrphan records a catalogue post that inserted cleanly while a row
// framing exactly the same assets sits under a different id THAT THE
// CATALOGUE NO LONGER NAMES.
//
// ⛔ ALL THREE CONDITIONS ARE LOAD-BEARING AND THE THIRD IS THE ONE
// THAT MAKES THIS USABLE. Sharing a member set is far too weak on its
// own: the published wall holds 861 posts over 785 distinct member
// sets. What separates a migrated id from a sibling is that the row
// left behind is one NOTHING IN THE CATALOGUE WILL EVER TOUCH AGAIN.
// A new catalogue post framing assets an older post already frames is
// ordinary and is not reported, because that older post is still named.
//
// The seeder cannot consult `post-id-migration.*.json` to be certain:
// that document lives in seed/upgrades and catalogues.go reads
// MANIFEST.json and posts.json from the SITE ROOT. Nor can it key on
// the title, because #1306 rewrote 774 of them in the same arc that
// moved the ids. Membership plus abandonment is what the data actually
// supports.
//
// ⛔ THIS IS NOT HYPOTHETICAL AND IT IS NOT YET REACHABLE, WHICH IS WHY
// IT IS BUILT NOW. runner.go's own comment says post ids are
// stableUUID-derived, "so a re-run re-composes the same posts and lands
// on SeedInsertPost's ON CONFLICT path rather than duplicating the
// wall". That premise holds only while ids do not move, and #1310 moved
// 107 site_a, 216 site_b and 295 dataset post ids. After #1319
// republishes, a migrated post no longer conflicts: it inserts as a new
// row while the row under its old id remains, with members, tags and
// collection links written under BOTH. The only removal path in the
// package is `Reset`'s TRUNCATE, so nothing would ever clear the old
// one, and the wall would carry each migrated post twice.
func (d *postDrift) noteOrphan(catalogueID, existingID string) {
	d.orphans = append(d.orphans, postOrphan{
		catalogueID: catalogueID, existingID: existingID,
	})
}

// summary renders the counted disagreement, or "" when there is nothing
// to say. Plain words on stdout, where whoever ran the seed is looking:
// a structured log line at INFO is not where an operator finds out that
// the corpus change they just made did not land.
func (d *postDrift) summary() string {
	if d.drifted == 0 && len(d.orphans) == 0 {
		return ""
	}
	var b strings.Builder
	if d.drifted > 0 {
		fmt.Fprintf(&b, "WARNING: %d of %d post(s) already in the database "+
			"disagree with the catalogue.\n", d.drifted, d.resumed)
		b.WriteString("  `aa seed` never rewrites a post row it already " +
			"wrote, so these still hold\n  whatever an earlier seed put " +
			"there. Disagreeing values: " + d.fieldSummary() + ".\n")
		if len(d.samples) > 0 {
			fmt.Fprintf(&b, "  For example: %s\n",
				strings.Join(d.samples, ", "))
		}
	}
	if len(d.orphans) > 0 {
		fmt.Fprintf(&b, "WARNING: %d catalogue post(s) were inserted under a "+
			"new id while a post holding\n  exactly the same assets remains "+
			"under an older one. Both rows now exist\n  and both reach the "+
			"wall.\n", len(d.orphans))
		for i, o := range d.orphans {
			if i >= driftSampleLimit {
				fmt.Fprintf(&b, "  ... and %d more\n",
					len(d.orphans)-driftSampleLimit)
				break
			}
			fmt.Fprintf(&b, "  catalogue %s duplicates existing %s\n",
				o.catalogueID, o.existingID)
		}
	}
	b.WriteString("  Run `aa seed --reset` to rebuild the corpus from the " +
		"catalogue. Nothing short of\n  that can change a post row this " +
		"seeder has already written.\n")
	return b.String()
}

// fieldSummary renders the per-value counts in a stable order, so two
// runs over the same drift produce the same sentence.
func (d *postDrift) fieldSummary() string {
	names := make([]string, 0, len(d.fields))
	for name := range d.fields {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if d.fields[names[i]] != d.fields[names[j]] {
			return d.fields[names[i]] > d.fields[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s (%d)", name, d.fields[name]))
	}
	return strings.Join(parts, ", ")
}
