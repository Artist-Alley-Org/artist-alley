// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package fixturesweep identifies rows that a test run left behind in a
// long-lived development database, so they can be removed without
// touching the seeded corpus (#1245).
//
// THE SHAPE OF THE PROBLEM
// ------------------------
// The shared coding stack is deliberately persistent: reseeding it costs
// 10-15 minutes, so it is reseeded rarely and every dogfood run adds its
// fixtures to it permanently. Measured on 2026-08-20 it held 3,491
// assets, of which 1,544 were fixtures — 44% of the corpus was litter,
// and the newest 200 assets contained no raster image at all, which is
// why `ui-30` could not pass there no matter what it asserted.
//
// WHY THIS IS NOT A HEURISTIC OVER NAMES
// --------------------------------------
// The obvious rule — "fixtures are the ones called ui-something, or the
// .txt ones" — is wrong in both directions, and both errors were real:
//
//   - It MISSES most of them. The largest fixture families on the stack
//     are `Dogfood PNG (ui-13-…)` ×147 and `cvrfix… member-a` ×65, all
//     png/jpg/pdf. An extension rule sees none of those.
//   - It DELETES real data. Three `Ofl (document)`, one `Tilemap
//     (document)` and one `Tilesheet (document)` are genuine seeded
//     assets that happen to be .txt — font licences and tile metadata
//     shipped alongside real art.
//
// A date rule fails too, and not subtly: a dogfood run OVERLAPPED the
// seed. The first fixture collection was created at 01:46:15 and the
// last real post at 01:46:22 the same morning, so no instant separates
// them.
//
// WHAT ACTUALLY SEPARATES THEM
// ----------------------------
// Provenance. The seeder stamps every asset it writes with
// `metadata->'acquisition_source'`; the upload API never does. That
// single key partitions the asset table exactly — 1,947 marked, 1,544
// unmarked, and the unmarked set agrees to the row with an independent
// signal (creation after the seed's own high-water mark). Two unrelated
// tests, zero disagreements, on every row in the table.
//
// The other tables have no such stamp, so they use positive
// identification of what is REAL and treat the remainder as fixtures
// only where a rule can also name the fixture family. Every rule below
// carries the count it matched when it was written.
//
// For collections and posts, "positive identification of what is REAL"
// means the SEED CATALOGUE: the names in dataset.collections.json and
// the ids in studio-*.posts.json, bound as parameters so the rules track
// the dataset instead of drifting from it. The catalogue is a provenance
// record — the seeder writes the id the catalogue gave it and nothing
// else can — which is why it satisfies ADR 0095 where an author or date
// heuristic did not.
//
// ⚠️ POSTS IS THE ONLY TABLE THAT CAN CONTRADICT. Every other rule's
// Fixture and Protected are exact complements of one another, so their
// overlap is empty by construction and their unclassified gap is empty
// too. Posts is the one place where a rule can be wrong about a row, and
// it is the one place a real contradiction has ever appeared (#1276).
//
// THE SAFETY MODEL
// ----------------
// Deleting from a shared database is irreversible, so the design is
// deny-by-default and the tool refuses rather than guesses:
//
//   - Dry run is the DEFAULT. Deleting requires an explicit flag.
//   - Every rule declares a Protected predicate naming rows that must
//     NEVER be deleted. Before anything is removed the sweep asserts
//     that Fixture AND Protected selects ZERO rows, and ABORTS the whole
//     run if it does not. A rule that has drifted into overlapping real
//     data stops the sweep instead of running it.
//   - Rows matching neither predicate are reported as UNCLASSIFIED and
//     left alone. Leaving a fixture behind costs a dirty row; deleting a
//     real one cannot be undone.
package fixturesweep

// Rule describes how to tell one table's fixtures from its real rows.
//
// Fixture and Protected are SQL boolean expressions evaluated against
// the named table. They are deliberately not complements of each other:
// the gap between them is the unclassified set, which the sweep reports
// and never touches.
type Rule struct {
	// Table is the parent table. Children are removed by the schema's
	// ON DELETE CASCADE, which covers asset_field_value, post_assets,
	// collection_resources and the rest.
	Table string

	// Fixture selects rows created by a test run.
	Fixture string

	// Protected selects rows that must survive. If any row satisfies
	// both this and Fixture, the sweep aborts.
	Protected string

	// IDColumn is the primary key the satellite tables reference.
	// Everything uses "id" except "user", whose key is "ref".
	IDColumn string

	// LabelColumn is the human-readable column the abort prints beside
	// each contradicting id. Without it an operator is told "posts: 6
	// rows" and has to write SQL to find out which six (#1276).
	LabelColumn string

	// Param names the catalogue value this rule binds as $1, or is empty
	// when the rule takes no parameter.
	//
	// ⛔ ONE SLOT, PER-RULE MEANING — not a global $1/$2 numbering. A
	// statement that mentions only $2 still makes Postgres expect two
	// parameters, and it cannot infer a type for the $1 nothing
	// references: `could not determine data type of parameter $1`
	// (42P18). Giving every rule the same slot and a different meaning
	// keeps each composed statement self-consistent.
	Param CatalogueParam

	// Kind is the value the polymorphic satellite tables store in their
	// *_kind column for this table's rows. Empty means the table has no
	// polymorphic references and the satellite pass skips it.
	Kind string

	// Why records the evidence for the rule and the count it matched
	// when it was written, so a future reader can tell whether it still
	// holds rather than trusting it.
	Why string
}

// Rules is the full sweep, parents first. Order matters only for
// reporting; deletion order is handled by the caller, which removes
// children of no-FK satellite tables before their parents.
var Rules = []Rule{
	{
		Table:       "assets",
		IDColumn:    "id",
		LabelColumn: "title",
		Kind:        "asset",
		Fixture:     `NOT (metadata ? 'acquisition_source')`,
		Protected:   `metadata ? 'acquisition_source'`,
		Why: "The seeder stamps acquisition_source on every asset it writes; the " +
			"upload API never does. Verified 2026-08-20: 1,947 marked / 1,544 unmarked, " +
			"and the split agrees exactly with 'created after the seed high-water mark' " +
			"on all 3,491 rows. This is the one table with a real provenance marker.",
	},
	{
		Table:       "collections",
		IDColumn:    "id",
		LabelColumn: "name",
		Kind:        "collection",
		// Positive identification of REAL, because the seeder can only
		// ever create the 18 names in seed/profiles/dataset.collections.json
		// (it skips the ones with no content — runner.go:558, which is why
		// only 7 are present). Anything else was created by a test.
		Param:     ParamCollectionNames,
		Fixture:   `name <> ALL($1::text[])`,
		Protected: `name = ANY($1::text[])`,
		Why: "Real collections are exactly the seed catalogue's names, passed in as $1. " +
			"Verified 2026-08-20: 7 of the catalogue's 18 present (the other 11 hold no " +
			"assets on this site and are skipped by design), 878 fixtures.",
	},
	{
		Table:       "posts",
		IDColumn:    "id",
		LabelColumn: "title",
		Kind:        "post",
		// Posts have no provenance stamp of their own, so REAL is
		// identified positively from the seed catalogue's ids — the same
		// shape as the collections rule one entry up, and for the same
		// reason. The catalogue IS the provenance record: a post the
		// seeder wrote has the id the catalogue gave it, and nothing else
		// can.
		//
		// Fixture still names the families explicitly, and that is
		// deliberate: it must NOT be "everything not in the catalogue".
		// 37 posts on the coding stack are stale SEED output — Pexels
		// solo posts whose catalogue entries changed id across #602/#658
		// and #1260 — and 4 are the `admin_uploads` plates the seeder
		// generates with runtime ids from dataset.fixtures.json. None of
		// those is test litter, and a rule that swept "unrecognised"
		// would delete all 41. They fall in the unclassified gap, which
		// is exactly what the gap is for.
		Fixture: `(title IS NULL OR title = '' OR ` +
			`title ~ '^(share|acl|ui|UI-)[0-9]*[ _-]' OR ` +
			`title ~ '[0-9]{13}' OR ` +
			`title ~* '(dogfood|fixture|probe)')`,
		Param:     ParamPostIDs,
		Protected: `id = ANY($1::uuid[])`,
		Why: "Real posts are exactly the seed catalogue's ids, passed in as $1 from " +
			"seed/profiles/studio-*.posts.json. Verified 2026-08-26 on 1,077 rows: " +
			"861 in the catalogue (site_a's whole set, no false negatives), 175 " +
			"fixtures, 41 unclassified (37 stale seed output + 4 admin_uploads " +
			"plates), 0 contradictions.\n" +
			"\n" +
			"⛔ WHAT THIS REPLACED, AND WHY IT HAD TO GO. The old Protected was " +
			"`author_user_ref <> 1 OR created_at < '2026-08-17'`, on the claim that " +
			"\"fixture posts are all authored by the bootstrap admin\". That claim " +
			"stopped being true when #1270 gave the suite seeded principals to run " +
			"as: `asset-usage-1237` posts as omar.haddad — a REAL seeded persona, " +
			"deliberately named so the sweep reads it as real — with a 13-digit epoch " +
			"in the title. Fixture matched on the title, Protected matched on the " +
			"author, and the sweep aborted after every dogfood run, which is a " +
			"correct refusal over a rule that had drifted. It also encoded a date " +
			"cutoff, which ADR 0095 forbids outright: a dogfood run once overlapped " +
			"the seed by seven seconds.",
	},
	{
		Table:       "field_definition",
		IDColumn:    "id",
		LabelColumn: "code",
		Kind:        "",
		Fixture:     `code ~ '^(ui|vocab)[0-9]+_' OR code ~ '^(probe|sprint)[0-9]*[_a-z]'`,
		Protected:   `code !~ '^(ui|vocab)[0-9]+_' AND code !~ '^(probe|sprint)[0-9]*[_a-z]'`,
		Why: "Fixture field codes are generated with a numeric run id: ui35_card_…, " +
			"vocab789_1, probe_1173_participation, sprint2_scale_probe. Verified " +
			"2026-08-20: 1,155 fixtures / 27 real — the 7 bootstrap defaults, 2 added " +
			"by a migration, and the seed catalogue's 18.",
	},
	{
		Table:       `"user"`,
		IDColumn:    "ref",
		LabelColumn: "username",
		Kind:        "user",
		Fixture:     `username ~ '^(acl|share|vocab|sprint|ui)[0-9]+' OR username ~ '^go_[a-z]+_test_user$'`,
		Protected:   `username !~ '^(acl|share|vocab|sprint|ui)[0-9]+' AND username !~ '^go_[a-z]+_test_user$'`,
		Why: "Fixture users are named for the spec that made them plus its run id: " +
			"share875_2, acl667_grantee, vocab789_nonholder, sprint7probe. Verified " +
			"2026-08-20: 61 fixtures / 34 real (the seed's 31 plus the bootstrap admin). " +
			"go_auth_test_user is the Go suite's fixture (#870) — it should never appear " +
			"in a dev database, and the rule removes it if it does.",
	},
}

// SatelliteTables have no foreign key to their subject, so CASCADE does
// not reach them and the sweep must clear them explicitly. They are the
// polymorphic tables — an activity or a notification names its subject
// by id and type, not by reference.
//
// Each entry is a table and the column holding the subject id.
var SatelliteTables = []struct {
	Table      string
	Column     string
	KindColumn string
}{
	{"activities", "object_local_id", "object_kind"},
	{"notifications", "target_id", "target_kind"},
	{"likes", "target_id", "target_kind"},
	{"comments", "target_id", "target_kind"},
}
