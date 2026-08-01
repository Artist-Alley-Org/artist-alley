---
id: "0057"
title: v0.1.0 baseline schema shape
status: accepted
date: 2026-07-08
area: architecture
phases:
  - "1.55.U"
supersedes: []
related:
  - "0043"
  - "0046"
  - "0056"
tags:
  - schema
  - migrations
  - baseline
  - federation
  - caching
excerpt: >-
  The v0.1.0 tag ships a single `00001_baseline_v0_1.sql` migration
  that collapses the pre-tag chain (baseline_v1 + 28 append
  migrations) into one canonical schema. This ADR documents the
  design invariants the baseline captures so future contributors +
  the eventual append-only-forever rule (per ADR 0046 pending
  issue #228) have a fixed reference.
---

## Context

The artist-alley schema evolved across two prior squashes: the
Phase 1.49.C-2 pre-MVP squash (2026-06) that produced
`00001_baseline_v1.sql`, and 28 append migrations that landed
between then and the pre-tag audit. Phase 1.55.U-1's audit
([docs/schema_audit_v0_1.md](../schema_audit_v0_1.md)) enumerated
the resulting shape — 78 tables, 200 indexes, 27 plpgsql functions,
190 constraints, 4 extensions — and found zero MUST findings + 23
SHOULD findings (mostly missing FK-covering indexes + three
`NO ACTION` cascade defaults that should be explicit `SET NULL`) +
11 NICE findings.

Per ADR 0046 + the pending append-only-forever decision (issue
#228), one more squash is permitted before the v0.1.0 tag ships;
after that, every schema change is a new append migration forever.
The audit + fix-application arc (Phase 1.55.U-2, PR #236) is that
squash. This ADR documents the resulting shape so a future
contributor reading `00001_baseline_v0_1.sql` for the first time
can navigate it without cross-referencing 15 external documents.

## Decision

The v0.1.0 baseline is a single file at
`app/internal/db/migrations/00001_baseline_v0_1.sql`. Its shape is
locked around six design decisions:

### 1. Ownership model — row-level `owner_user_ref` + team scope

Every user-created entity (`assets`, `posts`, `collections`,
`teams`, `brush_packs`, `saved_search`) carries an
`owner_user_ref BIGINT` FK to `"user".ref`. Row-level access checks
run against this column via the shared `visibility.Filter` package
(established in Phase 1.16.B-followup — see ADR 0056). Team
membership overlays are threaded through per-entity `_acls` tables
(`post_acls`, `collection_acls`, `asset_type_acls`).

The `user` table itself is named with quotes because `user` is a
Postgres reserved word. Every FK from other tables uses `user_ref`
naming rather than `user_id` so grep queries survive the reserved-
word quoting boundary.

Superadmin bypass is not a schema concern — it's a runtime check
in `auth.Identity.Can(SuperAdminCapability)` — but the row-level
predicates rely on the fact that admin queries skip the visibility
join.

### 2. Federation posture — `origin_server_id` on 17 tables; per-instance on the rest

Tables that federate carry an `origin_server_id UUID NULL` column
that identifies the peer instance that authored the row.
Enumerated: `api_tokens`, `asset_alternates`, `assets`,
`brush_packs`, `collections`, `comments`, `direct_messages`,
`field_definition`, `jobs`, `notifications`, `posts`, `roles`,
`saved_search`, `sessions`, `storage_objects`, `teams`,
`user_blocks`, `user_follows`, `user_password_history`,
`user_preferences`, `user_profiles`.

Tables without `origin_server_id` are per-instance by design —
federation infrastructure (`federation_peers`, `federation_directories`,
`federation_dispatch_state`, `federation_inbox`, `federation_outbox`,
`federation_peer_suggestions`, `federation_remote_actors`,
`federation_user_keys`), authentication surface tables
(`sessions` scope, `user_totp`), search-adjacent tables (search
runs are per-instance), and infrastructure (`audit_events`,
`system_config`, `goose_db_version`).

Per ADR 0043's federation walled-garden design, cross-instance
writes always carry an origin. The `origin_server_id IS NULL` case
means "authored on this instance"; non-NULL means "arrived via
federation dispatch." Every federated write path checks this before
emitting outbox rows to prevent broadcast loops.

### 3. Soft-delete pattern — `deleted_at TIMESTAMPTZ NULL` on 5 tables

`assets`, `posts`, `collections`, `comments`, `teams` carry
`deleted_at`. The pattern was established in Phase 1.55.C-1a
(see the readiness doc §4.6). Every list query filters
`WHERE deleted_at IS NULL` on the default path; admin surfaces
opt in via `?include_deleted=true` (Phase 1.55.C-1b).

Restore + hard-delete-by-gc semantics are handled by
`app/internal/softdelete/` package + its nightly coordinator job.
The v0.1.0 baseline pre-seeds `sysconfig.SoftDeleteConfig` with
30-day retention on assets/posts/collections and 90-day retention
on users (users use the `UserStateArchived` state rather than a
`deleted_at` column — see the hybrid scope decision in Phase
1.55.C-1a).

### 4. Indexing strategy — partial indexes on hot paths, FK-covering indexes on cascade paths

Every list query has a covering index. Partial predicates match the
WHERE clause on the hot path — e.g.:

- `assets_created_at_idx (created_at DESC) WHERE deleted_at IS NULL`
- `assets_owner_idx (owner_user_ref) WHERE deleted_at IS NULL AND owner_user_ref IS NOT NULL`
- `assets_search_text_gin (search_text) WHERE deleted_at IS NULL`

Foreign keys that participate in cascade paths (or in admin analytics
"show me rows created by user X" queries) carry partial FK-covering
indexes with `WHERE <col> IS NOT NULL`. Phase 1.55.U-2 §4 audit
added 15 such indexes inline in the baseline (see
`schema_audit_v0_1.md` §4 for the enumerated list) — the pattern
becomes the norm going forward: every FK column that supports a
join or a cascade delete gets its covering index.

pgvector-backed similarity search (`asset_embedding_d768`,
`asset_visual_embedding`) uses HNSW indexes. Per ADR 0056, hybrid
BM25 + cosine ranking is implemented at the query layer, not the
index layer.

### 5. Cache invalidation model — Registry-scoped domains with cross-package invalidator helpers

30 named cache domains, plus 4 dedicated Cache types
(`search.Cache`, `iiif/presentation.Cache`,
`search/diskusage.Cache`, `ai.Caches` wrapper). Every hot query
path is fronted by a `cache.Registry`-registered domain. See §6
of the schema audit doc for the full enumeration.

Per Phase 1.55.U-2 §7 audit + fix pass:

- Per-user caches (auth.caps.user, messages.unread_dm_count,
  notifications.unread_count, userprefs.by_user, users.state,
  users.byRef, users.actorKeys, auth.lockout) are all correctly
  keyed by `user_ref`. No cross-user leak.
- Federation-crossing caches (activities.actor_outbox,
  federation_shares.by_object, federation_follows.by_actor,
  peer.by_url, peer.enabled_snapshot, peer.visible_snapshot,
  remote_actor.encryption_key.x25519, directory.entries,
  shares.by_object, p2p.by_source) are either per-instance by
  design OR key by URIs that already encode origin.
- Cross-write invalidators are targeted per-entity (post-Phase
  1.55.U-2 §7.1 + §7.2) — asset PATCH invalidates only that
  asset's IIIF manifest cache slot, not the whole cache.
- Owner-profile invalidation on collection writes mirrors the
  posts-side pattern (posts InvalidateProfile → collections now
  also InvalidateProfile on Create/Delete/Restore).

### 6. Data seeds preserved in the baseline

The baseline pre-seeds the constant data every fresh boot needs:

- 13 `asset_types` (Image, Document, Video, Audio, 3D Object,
  Archive, Font, Comic, Ebook, Audiobook, Texture, Sprite, Code)
- 57 `capabilities` — every capability code seeded through the
  pre-tag chain (system.*, users.*, teams.*, assets.*, posts.*,
  system.sso.ldap.*, system.sso.saml.*, system.tenancy.*)
- 10 `roles` (Base, Admin, Anonymous + shipped tenant/team roles)
- 43 `role_capabilities` mappings
- 42 `system_config` rows (SMTP defaults, feature flags,
  moderation thresholds, sysconfig sections)
- 7 `workflow_states` + 11 `workflow_transitions` (asset workflow
  DAG)
- 13 `field_definition` rows (title, description, credit,
  copyright, capture_date, keywords, country + 6 more — see the
  2026-08-01 amendment for what those six actually were)
- 1 `federation_dispatch_state` singleton (id=1 CHECK enforced;
  seeded so the outbox dispatcher can find its cursor from the
  first tick)

Fresh boot via `docker compose down -v && docker compose up`
produces a working stack — bootstrap admin fires, seed data lands
via this baseline, no dev-side hand-seeding required.

## Consequences

**Append-only-forever from v0.1.0 tag onward** (pending the
final trigger decision in issue #228). Every schema change after
v0.1.0 is a new numbered migration (`00002_*.sql`) that ADDs
columns/tables/indexes; NEVER modifies pre-v0.1.0 rows or drops
pre-v0.1.0 constraints. `scripts/verify-baseline.sh` gate stays
green forever.

**Fresh-boot reproducibility.** A contributor running
`docker compose down -v && docker compose up --build` on any
commit past v0.1.0 gets a working stack. No hand-seeding, no
undocumented state.

**Test-suite stability.** Tests that depend on schema shape
(dispatcher tests reading `federation_dispatch_state`; freshness
tests reading `goose_db_version`) work against the baseline
without special test-fixture SQL. The one shakeout during Phase
1.55.U-2 (federation_dispatch_state singleton missed on the
initial squash) was caught + fixed in commit `7a44d86c` of
PR #236.

**Migration semantics stabilise.** Every migration post-v0.1.0
follows the pattern established in the append chain:
`-- +goose Up\n-- +goose StatementBegin\n...\n-- +goose StatementEnd`
with a `-- +goose Down` block. Data mutations (INSERT into seed
tables) can appear in migrations OR in app boot logic; the
convention across v0.1.0 baseline is that constant-data seeds
live in migrations, ephemeral state seeds live in app boot.

**Historical migrations are archived, not resurrected.** The
Phase 1.55.U-2 squash deleted 29 files
(`00001_baseline_v1.sql` + 28 appends). Git history preserves
them for anyone who needs to audit specific changes; the working
tree only ships the current baseline. Per ADR 0046 §"Down blocks
in a baseline": the Down section stays empty by design — rollback
from a baseline means restoring from backup, not running a
reverse migration.

**No plpgsql function evolution during the append chain.** The
audit confirmed no plpgsql function was created, altered, and
re-created across the chain. All 27 functions live in
`00001_baseline_v1.sql`'s original definitions; the squash
inherits them unchanged.

## Amendment — 2026-08-01: six of the baseline's field definitions were captured fixtures (#812)

The inventory above described the baseline's 13 `field_definition` rows as "seven
editorial fields plus the 6 metadata extraction field defs added in append
migrations". That was wrong in both halves, and the error mattered: it read as
"the baseline ships thirteen considered defaults", which is why nobody looked at
them again.

All 13 rows are in the **baseline itself**; no append migration contributed to
that count. And the six beyond the editorial seven were not extraction field
definitions. They were **test fixtures the fold captured** — `mcoltest_fedguard`,
`ctest_fedguard`, `mtv_text`, `mtv_due`, `mtv_score`, `mtv_tags`, every one of
them carrying `created_by_user_ref = 420000`, a user ref no install has, from the
same dump already known to have captured dev secrets into `system_config`. They
shipped to every operator as "Text Field", "Score", "Due", "Tags" and two copies
of "Fed Guard".

Migration `00023_drop_folded_field_definition_fixtures.sql` removes them. The
seven editorial fields **stay**: they are a considered default catalogue, and
their IPTC/XMP/EXIF `source` mappings are the same ones ResourceSpace ships in
`dbstruct/data_resource_type_field.txt`. Shipping an opinionated default catalogue
is normal.

A fresh install's field catalogue is therefore **nine** rows — the seven, plus
`pixel_width` / `pixel_height` from migration 00017. That set is now named
explicitly in `app/internal/db/shippedfields.go` and pinned by
`TestShippedFieldCatalogue_*`, because it is also the keep-list `aa seed --reset`
sweeps against: before #812 the reset TRUNCATEd `field_definition`, so no dev, CI
or demo instance had ever run the catalogue production ships.

Two follow-ups this amendment does **not** settle: `country` is typed `tree` where
RS types its equivalent as a node-backed keyword list (a divergence to revisit
deliberately, with its own migration), and the `source` column those seven declare
their extraction intent in is still read by nothing (#813).

**The re-fold lesson.** `system_config` is scrubbed on a re-fold because it is
known to capture secrets. `field_definition` was not, and it captured fixtures.
Any future baseline fold should treat every table a test can write to as
contaminated until inspected — the scrub list is not "the tables that hold
credentials".

## References

- [ADR 0043](./0043-federation-walled-garden-protocol/) — federation posture
- [ADR 0046](./0046-migration-baseline-and-squash-policy/) — baseline + squash policy
- [ADR 0056](./0056-cross-entity-search-with-visibility-floor/) — search architecture + visibility floor
- [docs/schema_audit_v0_1.md](../schema_audit_v0_1.md) — audit that drove this baseline
- PR #234 — 1.55.U-1 audit report
- PR #236 — 1.55.U-2 baseline re-squash + fix application
- Issue #228 — append-only-forever trigger decision
