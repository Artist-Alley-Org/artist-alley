// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Content reset for `aa seed --reset` (#566, #568, #569).
//
// # The bug class this file exists to close
//
// Reset clears the seeded content tables so a re-seed starts from a
// clean slate. It does that with TRUNCATE ... CASCADE, and CASCADE
// only follows FOREIGN KEYS. A POLYMORPHIC table — one that names its
// target as a (kind, id) PAIR — cannot have a foreign key on the id
// column by construction, because the id means a different table
// depending on the kind. So:
//
//   - TRUNCATE ... CASCADE never reaches it, and
//   - the row-level triggers that sweep a deleted row's dependants
//     (social_sweep_after_post_delete, acl_sweep_after_team_delete, …)
//     do NOT fire for TRUNCATE, which is statement-level.
//
// Its rows therefore SURVIVE the reset while their targets are wiped,
// and become orphans pointing at ids that no longer exist. `likes` was
// exactly this (#566/#568, ~82 orphans + a ~3.5k like_count
// undercount). This file generalises the fix so the NEXT polymorphic
// table cannot silently inherit the bug.
//
// # The policy (#569)
//
// EXPLICIT REGISTRY + DERIVED ENFORCEMENT + SWEEP-NOT-TRUNCATE.
//
//  1. Every polymorphic column in the schema is CLASSIFIED, by hand,
//     in polymorphicRefs / nonReferenceKindColumns below. Classifying
//     cannot be derived: whether an orphan should be deleted is a
//     judgement call (storage_pins must be left alone — see its
//     entry), so a purely information_schema-driven reset would make
//     the WRONG call as often as the right one.
//
//  2. DETECTION is derived, which is the half a hand-maintained list
//     gets wrong. TestPolymorphicRegistry_CoversSchema queries
//     information_schema for every text `…kind` / `…type` column on a
//     base table and fails if one is not classified here. A future
//     polymorphic table therefore fails CI with "classify me" instead
//     of silently orphaning rows. (Deriving detection from a NAMING
//     convention alone is not enough either — activities pairs
//     object_kind with object_local_id, not object_id — so the
//     registry names the id column explicitly.)
//
//  3. Cleanup is a SWEEP, not a truncate: delete the rows whose
//     (kind, id) no longer RESOLVES. Naming another table in the
//     TRUNCATE list (what #568 did for likes) is the blunt version of
//     this and is wrong for tables that also hold rows about content
//     the reset deliberately keeps — a scheduled_action against the
//     bootstrap admin must survive a content reset, an identical row
//     against a wiped asset must not. The sweep predicate is also
//     EXACTLY the assertion the test makes afterwards, so the reset
//     cannot leave behind an orphan the invariant test would catch.
//
// Kinds that are not in a ref's Targets map are LEFT ALONE by design:
// notifications.target_kind can be "license", which is an external
// identifier with no local table, and deleting rows we cannot resolve
// would be a guess.

package seed

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// polyTarget is the table + primary-key column a single kind value
// resolves to. Comparison is done as text on both sides so a uuid
// target, a bigint target and a text id column all work through one
// code path (and a malformed id string can never raise a cast error
// mid-reset).
type polyTarget struct {
	Table  string // quoted where Postgres needs it, e.g. `"user"`
	Column string
}

// polymorphicRef is one (kind, id) column pair, with the decision of
// what a reset should do about it.
type polymorphicRef struct {
	Table      string
	KindColumn string
	IDColumn   string

	// Targets maps a kind VALUE to the row it names. A kind not
	// present here is deliberately unresolvable and its rows are
	// never swept — see Reason.
	Targets map[string]polyTarget

	// Sweep false means "this table keeps its rows even when they
	// dangle". Reason is mandatory either way: for Sweep it records
	// why the rows are seed-scoped, for a keep it records why the
	// orphan is acceptable, so the next reader does not "fix" it.
	Sweep  bool
	Reason string
}

// polymorphicRefs is the classified set. Adding a polymorphic table
// without adding it here fails TestPolymorphicRegistry_CoversSchema.
var polymorphicRefs = []polymorphicRef{
	{
		// Seeded content in its own right (applyComments /
		// applyPostComments) and already named in the TRUNCATE below;
		// the sweep is a backstop for comments on anything the
		// truncate list ever stops covering.
		Table: "comments", KindColumn: "target_kind", IDColumn: "target_id",
		Targets: map[string]polyTarget{
			"post":       {"posts", "id"},
			"asset":      {"assets", "id"},
			"collection": {"collections", "id"},
		},
		Sweep:  true,
		Reason: "seeded content; a comment on a wiped post is unreachable",
	},
	{
		// The original bug (#566/#568). Seeded by applyLikes.
		Table: "likes", KindColumn: "target_kind", IDColumn: "target_id",
		Targets: map[string]polyTarget{
			"post":    {"posts", "id"},
			"asset":   {"assets", "id"},
			"comment": {"comments", "id"},
		},
		Sweep:  true,
		Reason: "seeded content; surviving likes orphan AND undercount like_count on re-seed",
	},
	{
		// Seeded by applyFeatured, and NOT covered before this change:
		// featured_items' only FK is team_id, so placements survived
		// every reset. Reproduced as a live orphan when a reset swaps
		// datasets (a site_a collection stayed on the public rail
		// after site_b was seeded).
		Table: "featured_items", KindColumn: "subject_kind", IDColumn: "subject_id",
		Targets: map[string]polyTarget{
			"asset":      {"assets", "id"},
			"collection": {"collections", "id"},
		},
		Sweep:  true,
		Reason: "seeded content (applyFeatured); an orphan placement is a broken landing-page tile",
	},
	{
		// #569's first named table. Notifications are DERIVED from
		// content activity ("X commented on your post"): once the post
		// is gone the card cannot render and its deep link 404s, so a
		// dangling notification has no standalone value. Swept rather
		// than truncated so a notification about something the reset
		// keeps (the bootstrap admin) survives.
		//
		// "license" is intentionally absent from Targets: license
		// notifications carry an external licence identifier with no
		// local table, so they are unresolvable, not orphaned.
		Table: "notifications", KindColumn: "target_kind", IDColumn: "target_id",
		Targets: map[string]polyTarget{
			"post":       {"posts", "id"},
			"comment":    {"comments", "id"},
			"asset":      {"assets", "id"},
			"collection": {"collections", "id"},
			"user":       {`"user"`, "ref"},
			"request":    {"resource_request", "id"},
		},
		Sweep:  true,
		Reason: "derived from content activity; unrenderable once the target is wiped",
	},
	{
		// #569's second named table. The issue guessed "operator
		// config, leave it" — on reading it, that is not what this
		// table is. A scheduled_action is a PENDING OPERATION queued
		// against one specific target id (restrict/delete/change_state
		// on an asset, post, collection or user). It is far closer to
		// `jobs` (which this reset already prunes) than to
		// system_config, and a state='pending' row against a truncated
		// asset can only fail or no-op forever.
		//
		// The operator-config worry is real but is answered by the
		// SWEEP, not by an exemption: an action scheduled against the
		// bootstrap admin — or any user/asset the reset keeps — still
		// resolves and is left untouched. Only the ones whose target
		// the reset just deleted go.
		Table: "scheduled_actions", KindColumn: "target_kind", IDColumn: "target_id",
		Targets: map[string]polyTarget{
			"asset":      {"assets", "id"},
			"post":       {"posts", "id"},
			"collection": {"collections", "id"},
			"user":       {`"user"`, "ref"},
		},
		Sweep:  true,
		Reason: "pending operation against one content id; unrunnable once the target is wiped",
	},
	{
		// Per-resource workflow history, rendered on the asset /
		// collection page. Not seeded, but the app writes it, and with
		// the resource truncated the rows are unreachable — there is
		// no surface that can display a transition on a nonexistent
		// asset. Compliance-grade audit lives in audit_events, which
		// this reset does not touch.
		Table: "workflow_audit", KindColumn: "resource_kind", IDColumn: "resource_id",
		Targets: map[string]polyTarget{
			"asset":      {"assets", "id"},
			"collection": {"collections", "id"},
		},
		Sweep:  true,
		Reason: "per-resource history with no surface once the resource is gone; audit_events is the durable trail",
	},
	{
		// ACL grants name their PRINCIPAL polymorphically. The object
		// side is FK'd (post_acls.post_id / collection_acls
		// .collection_id), so CASCADE already empties these two when
		// posts/collections are truncated — the sweep covers the
		// principal side, which nothing else does: a grant to a
		// fictional user the reset deletes would otherwise dangle.
		Table: "post_acls", KindColumn: "principal_type", IDColumn: "principal_id",
		Targets: map[string]polyTarget{
			"user": {`"user"`, "ref"},
			"role": {"roles", "id"},
			"team": {"teams", "id"},
		},
		Sweep:  true,
		Reason: "grant to a principal the reset deleted",
	},
	{
		Table: "collection_acls", KindColumn: "principal_type", IDColumn: "principal_id",
		Targets: map[string]polyTarget{
			"user": {`"user"`, "ref"},
			"role": {"roles", "id"},
			"team": {"teams", "id"},
		},
		Sweep:  true,
		Reason: "grant to a principal the reset deleted",
	},
	{
		// asset_type_acls has no object-side FK into anything the
		// reset touches (asset_types are baseline lookups that
		// survive), so unlike the other two ACL tables NOTHING cleans
		// its principal side. The team/role sweep triggers fire on the
		// per-row `DELETE FROM teams` below, but there is no
		// equivalent for a deleted user.
		Table: "asset_type_acls", KindColumn: "principal_type", IDColumn: "principal_id",
		Targets: map[string]polyTarget{
			"user": {`"user"`, "ref"},
			"role": {"roles", "id"},
			"team": {"teams", "id"},
		},
		Sweep:  true,
		Reason: "grant to a principal the reset deleted; no trigger covers the user case",
	},

	// ── Deliberate keeps ──────────────────────────────────────────

	{
		// KEEP — the #355 exemption, and the reason this policy is a
		// registry rather than "truncate every polymorphic table for
		// symmetry". storage_pins belongs to the CONTENT-ADDRESSED
		// store, not to the content tables: it is what stops the
		// storage orphan_scan sweep from reclaiming a blob. Deleting
		// pins here would unpin every seeded blob, the sweep would be
		// free to delete the bytes, and the re-seed — which relies on
		// re-uploading the SAME hashes and deduping onto the existing
		// objects — would find them gone. That is precisely the
		// desync #355 fixed by keeping storage_objects /
		// storage_variants / storage_pins out of the truncate.
		//
		// The dangling window is bounded: seed asset ids are stable
		// (from MANIFEST.json), so a re-seed re-pins the same
		// (subject, hash) pairs and the pins resolve again.
		Table: "storage_pins", KindColumn: "pin_subject_type", IDColumn: "pin_subject_id",
		Targets: map[string]polyTarget{
			"asset": {"assets", "id"},
		},
		Sweep:  false,
		Reason: "#355: pins guard blobs against the storage sweep; unpinning here lets a re-seed's dedup target be reclaimed",
	},
	{
		// KEEP — federation state. object_id names an object in the
		// SENDING PEER's namespace; resolving it against local tables
		// is a category error, and the row is a received-activity
		// transport log, not content.
		Table: "federation_inbox", KindColumn: "object_kind", IDColumn: "object_id",
		Targets: map[string]polyTarget{},
		Sweep:   false,
		Reason:  "remote object ids; inbox is a transport log, not content",
	},
	{
		// KEEP — federation state. This reset does not clear
		// federation at all (peers, activities, outbox, inbox, shares
		// all survive), because deleting an activity we have already
		// signed and delivered to a peer desyncs
		// federation_dispatch_state.last_dispatched_activity_id and
		// leaves peers holding objects we claim never existed.
		// Resetting a FEDERATING instance is a separate decision from
		// resetting seed content and is deliberately not made here.
		Table: "activities", KindColumn: "object_kind", IDColumn: "object_local_id",
		Targets: map[string]polyTarget{},
		Sweep:   false,
		Reason:  "append-only delivered-activity ledger; deleting entries desyncs peers + dispatch state",
	},
	{
		// KEEP — same family. A revoked-vs-live share is meaningful
		// history for the peer that holds the grant.
		Table: "federation_shares", KindColumn: "object_kind", IDColumn: "object_id",
		Targets: map[string]polyTarget{},
		Sweep:   false,
		Reason:  "outbound grant history held by peers; federation state is out of scope for a content reset",
	},
}

// nonReferenceKindColumns are the text `…kind` / `…type` columns that
// are plain enums/discriminators, NOT polymorphic references. They are
// listed so the completeness test can prove every such column in the
// schema was looked at by a human, rather than skipped by a heuristic.
var nonReferenceKindColumns = map[string]string{
	"activities.activity_type":          "ActivityPub verb (Create/Follow/…)",
	"asset_alternates.content_type":     "MIME type",
	"asset_alternates.kind":             "alternate role (preview/proxy/…)",
	"asset_companions.content_type":     "MIME type",
	"audit_events.event_type":           "audit verb",
	"comments.annotation_type":          "annotation geometry (point/rect/…)",
	"extraction_failure.error_kind":     "failure classification",
	"federation_inbox.activity_type":    "ActivityPub verb",
	"field_definition.subject_kind":     "which entity TYPE the field applies to; no companion id",
	"field_definition.type":             "field data type",
	"jobs.type":                         "job name",
	"mcp_server_registration.auth_kind": "auth scheme",
	"storage_objects.content_type":      "MIME type",
	"storage_sweep_runs.kind":           "sweep mode (orphan_scan/checksum_verify)",
	"storage_variants.content_type":     "MIME type",
}

// sweepStmt is one orphan-delete: the SQL plus the kind value it is
// scoped to. The kind travels as a BIND PARAMETER — every identifier in
// the SQL comes from this file's own registry, nothing from a row.
type sweepStmt struct {
	Table string
	Kind  string
	SQL   string
}

// sweepStatements renders the orphan-delete for every sweepable ref, in
// a stable order. The predicate is shared with the invariant test,
// which asserts the same NOT EXISTS finds nothing afterwards.
func sweepStatements() []sweepStmt {
	var out []sweepStmt
	for _, ref := range polymorphicRefs {
		if !ref.Sweep {
			continue
		}
		kinds := make([]string, 0, len(ref.Targets))
		for k := range ref.Targets {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			t := ref.Targets[kind]
			out = append(out, sweepStmt{
				Table: ref.Table,
				Kind:  kind,
				SQL: fmt.Sprintf(
					`DELETE FROM %s src WHERE src.%s = $1 AND NOT EXISTS (`+
						`SELECT 1 FROM %s tgt WHERE tgt.%s::text = src.%s::text)`,
					ref.Table, ref.KindColumn, t.Table, t.Column, ref.IDColumn),
			})
		}
	}
	return out
}

// Reset clears the content tables so a re-seed starts from a clean
// slate. Baseline lookups (workflow_states, asset_types) and the
// bootstrap admin survive; every fictional user + all their content is
// removed. CASCADE handles the FK-linked dependants; the polymorphic
// sweep at the end handles the ones CASCADE structurally cannot reach.
func Reset(ctx context.Context, pool *pgxpool.Pool, adminUsername string) error {
	// storage_objects is deliberately NOT truncated (#355). The content
	// store is content-addressed and asset-independent: storage_objects
	// / storage_variants are keyed by object_hash and describe what is
	// physically on the storage volume, which a DB truncate obviously
	// does not erase. Truncating them (CASCADE also took storage_variants
	// + storage_pins) desynced the two — the blobs stayed on disk while
	// their rows vanished, and because the preview handlers skip any
	// variant whose blob already exists, a re-seed would then regenerate
	// nothing and leave the instance with originals and zero variant
	// rows: /variants/col 404s.
	//
	// Leaving them alone is also idempotent: the seed's asset ids are
	// stable (from MANIFEST.json), so a re-seed re-uploads the same
	// hashes, dedups onto the existing objects, and re-pins identically.
	//
	// likes is named explicitly (#566/#568) and stays named: it is
	// seeded content, so a truncate is the honest statement of intent
	// and RESTART IDENTITY applies. The polymorphic sweep below would
	// reach the same end state and is the backstop, not the mechanism.
	const truncate = `TRUNCATE
	    assets, posts, comments, collections, field_definition, likes
	    RESTART IDENTITY CASCADE`
	if _, err := pool.Exec(ctx, truncate); err != nil {
		return fmt.Errorf("truncate content: %w", err)
	}
	// teams gets a per-row DELETE, NOT a slot in the TRUNCATE above.
	// TRUNCATE ... CASCADE empties dependent tables WHOLESALE, and
	// user_roles / user_capability_grants / user_capability_revokes all
	// carry a team_id FK — so naming teams in the TRUNCATE wiped every
	// role and capability grant in the install, including the bootstrap
	// admin's GLOBAL (team_id IS NULL) role. The instance then answered
	// needs_setup=true and nobody could log in, after every --reset.
	//
	// A per-row DELETE follows the same FK with ON DELETE CASCADE but
	// only removes the rows that actually reference a deleted team, so
	// global roles + grants survive. It also lets the row-level
	// acl_sweep_after_team_delete / asset_type_acl_sweep_after_team_delete
	// triggers fire, which a TRUNCATE would have skipped.
	if _, err := pool.Exec(ctx, `DELETE FROM teams`); err != nil {
		return fmt.Errorf("delete teams: %w", err)
	}
	// Fictional users (everyone but the bootstrap admin). Their
	// federation keys cascade.
	if _, err := pool.Exec(ctx, `DELETE FROM "user" WHERE username <> $1`, adminUsername); err != nil {
		return fmt.Errorf("delete users: %w", err)
	}
	// user_follows is the SAME bug class as the polymorphic tables
	// without being polymorphic: follower_user_ref / followee_user_ref
	// carry no FK to "user", so the DELETE above never reaches them.
	// Seeded (applyFollows) against fictional users whose `ref` is a
	// SERIAL — a re-seed mints NEW refs, so every previous run's follow
	// edges survived pointing at dead refs AND the table grew by a full
	// dataset on every reset (measured: 149 -> 298 -> 447 across three
	// runs).
	//
	// Swept, not truncated, for the same reason the polymorphic tables
	// are: an edge whose BOTH ends still exist is not this reset's to
	// delete. With every non-admin user gone that is currently the empty
	// set, but stating the rule rather than the consequence keeps it
	// correct if the reset ever spares more users.
	if _, err := pool.Exec(ctx, `
	    DELETE FROM user_follows f
	     WHERE NOT EXISTS (SELECT 1 FROM "user" u WHERE u.ref = f.follower_user_ref)
	        OR NOT EXISTS (SELECT 1 FROM "user" u WHERE u.ref = f.followee_user_ref)`); err != nil {
		return fmt.Errorf("sweep user_follows: %w", err)
	}
	// Preview jobs for the content we just truncated (#355). Every
	// preview job names an asset_id; with the assets gone they'd churn
	// through their retries and land in `failed`. Now that a seed
	// dispatches one per asset, skipping this would orphan a whole
	// dataset's worth on every --reset.
	if _, err := pool.Exec(ctx, `DELETE FROM jobs WHERE type LIKE 'preview.%'`); err != nil {
		return fmt.Errorf("delete preview jobs: %w", err)
	}
	// Polymorphic sweep — LAST, so every deletion above is already
	// visible to the NOT EXISTS predicates (#569).
	for _, stmt := range sweepStatements() {
		if _, err := pool.Exec(ctx, stmt.SQL, stmt.Kind); err != nil {
			return fmt.Errorf("polymorphic sweep (%s/%s): %w", stmt.Table, stmt.Kind, err)
		}
	}
	return nil
}
