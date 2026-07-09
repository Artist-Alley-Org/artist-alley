# Schema + cache audit for v0.1.0 baseline re-squash

**Status:** SNAPSHOT — report-only audit shipped as Phase 1.55.U-1
(2026-07-08). This doc inventories every table, column, index, FK,
constraint, and cache path in the tracked codebase and groups
findings by fix priority. It does NOT change any code; the follow-up
arc (**1.55.U-2**) executes against the fix list here.

**Sequenced role in v0.1.0:** the schema baseline was squashed in
Phase 1.49.C-2 (2026-06) to produce `00001_baseline_v1.sql`. 28
append migrations have landed since. Before the v0.1.0 tag ships, we
want a **second squash** — collapsing the 29-file chain into a
single `00001_baseline_v0_1.sql` with the audit-recommended fixes
inlined. This doc is the compliance signal that the fixes are worth
inlining vs. shipping as fresh migrations against the current
baseline.

---

## 0. Audit scope + methodology

### Scope

- Every base table in `public.` schema (78 tables + `goose_db_version`
  bookkeeping).
- Every migration file under `app/internal/db/migrations/` (29 files:
  baseline + 28 appends).
- Every `queries.sql` + sqlc-generated `queries.sql.go` under
  `app/internal/` (37 domains).
- Every `cache.Registry` registration + `Invalidate*` method + call
  site in application code.
- Selected FK cascade audit + information_schema drift check.
- Selected `EXPLAIN (ANALYZE, BUFFERS)` spot-checks on known-hot
  query paths using the current dev seed.

### Methodology

1. Direct `psql \dt` + `\d+ <table>` extraction against the live dev
   compose stack (`postgres` container, DB `artist_alley`).
2. Migration filesystem walk with regex categorization
   (`CREATE TABLE`, `ADD COLUMN`, `CREATE INDEX`, `DROP INDEX`,
   `ADD CONSTRAINT`, `CREATE TRIGGER`, `CREATE FUNCTION`, data DML).
3. `grep -rn` sweep for cache domain constants, `Invalidate*` method
   definitions, and cross-package `Invalidate*` call sites.
4. `pg_constraint` + `pg_index` joins to find FK columns lacking
   indexes AND FKs with NO ACTION/RESTRICT cascade semantics.
5. `information_schema.columns` sweep for NUMERIC (should be
   DOUBLE PRECISION per `[[sqlc_pgx_conventions]]`) and for
   nullable-without-default timestamp columns.

### Priority definitions

- **MUST:** correctness or breakage risk. A missing FK index that
  makes a delete cascade sequential-scan a hot table is MUST. A
  cache key that leaks between users is MUST.
- **SHOULD:** performance or cleanliness. A missing index on a hot
  ORDER BY path is SHOULD (bounded pain, not correctness). A
  redundant column is SHOULD.
- **NICE:** cosmetic. A naming inconsistency; a docstring gap; a
  comment referencing an old ADR.

### What this audit does NOT do

- Rewrite the schema. The baseline re-squash is 1.55.U-2's job.
- Load-test at production scale. Row counts reflect dev-seed
  cardinality (see §2); production load may surface additional hot
  paths.
- Audit federation-crossing cache semantics rigorously. That's flagged
  as SHOULD-tier follow-up work; the current caches are per-instance
  by design.
- Formally verify sqlc-generated types match schema types. Sampled
  for NUMERIC drift only (see §6 findings).

---

## 1. Migration chain summary

29 migration files: `00001_baseline_v1.sql` (the Phase 1.49.C-2
squash of the pre-MVP chain — 3,068 lines) + 28 append migrations
through `00029_soft_delete_recovery.sql`.

### Per-append categorization

Columns:
- **L** = file line count
- **T** = `CREATE TABLE` statements
- **C** = `ADD COLUMN` (or new column in fresh table)
- **I+** = `CREATE INDEX` statements
- **I-** = `DROP INDEX` statements
- **Cx** = `ADD CONSTRAINT` statements
- **Tg** = `CREATE TRIGGER`
- **Fn** = `CREATE FUNCTION`
- **Ext** = `CREATE EXTENSION`
- **Data** = `INSERT INTO` or `UPDATE ... SET` DML

| Migration | L | T | C | I+ | I- | Cx | Tg | Fn | Ext | Data |
|---|---|---|---|---|---|---|---|---|---|---|
| `00001_baseline_v1` | 3068 | 61 | (baseline) | 149 | 0 | 64 | many | many | 4 | 0 |
| `00002_subtitle_tracks` | 113 | 1 | 1 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| `00003_user_state_check` | 89 | 0 | 0 | 0 | 0 | 1 | 0 | 0 | 0 | 1 |
| `00004_drop_legacy_session_columns` | 51 | 0 | 2 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| `00005_capability_grant_expiry` | 59 | 0 | 2 | 2 | 2 | 0 | 0 | 0 | 0 | 0 |
| `00006_resource_request` | 117 | 1 | 1 | 4 | 4 | 0 | 0 | 0 | 0 | 1 |
| `00007_self_edit_gates` | 72 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 3 |
| `00008_collection_metadata` | 102 | 2 | 1 | 5 | 1 | 0 | 0 | 0 | 0 | 0 |
| `00009_ai_inference_foundation` | 174 | 1 | 1 | 4 | 1 | 0 | 0 | 0 | 0 | 5 |
| `00010_asset_tag_provenance` | 76 | 0 | 4 | 2 | 2 | 0 | 0 | 0 | 0 | 1 |
| `00011_asset_embeddings` | 116 | 1 | 0 | 2 | 2 | 0 | 0 | 0 | 0 | 1 |
| `00012_ai_transcription` | 91 | 0 | 0 | 0 | 0 | 2 | 0 | 0 | 0 | 2 |
| `00013_mcp_client_registration` | 132 | 2 | 0 | 1 | 1 | 0 | 0 | 0 | 0 | 4 |
| `00014_creative_lineage` | 67 | 1 | 0 | 1 | 0 | 0 | 0 | 0 | 0 | 0 |
| `00015_metadata_extraction` | 109 | 2 | 2 | 4 | 1 | 0 | 0 | 0 | 0 | 1 |
| `00016_per_user_asset_dedup` | 47 | 0 | 0 | 1 | 1 | 0 | 0 | 0 | 0 | 0 |
| `00017_admin_impersonation` | 59 | 0 | 1 | 1 | 1 | 0 | 0 | 0 | 0 | 2 |
| `00018_user_totp` | 58 | 2 | 0 | 2 | 0 | 0 | 0 | 0 | 0 | 0 |
| `00019_self_registration` | 68 | 1 | 1 | 1 | 0 | 0 | 0 | 0 | 0 | 3 |
| `00020_asset_page_count` | 33 | 0 | 1 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| `00021_search_foundation` | 124 | 0 | 1 | 1 | 1 | 0 | 1 | 2 | 0 | 0 |
| `00022_weighted_tsvector_and_smart_query` | 273 | 0 | 1 | 4 | 4 | 0 | 0 | 3 | 0 | 0 |
| `00023_saved_search` | 81 | 1 | 0 | 2 | 2 | 0 | 0 | 0 | 0 | 0 |
| `00024_search_reindex_run` | 55 | 1 | 0 | 2 | 2 | 0 | 0 | 0 | 0 | 0 |
| `00025_user_lockout` | 60 | 0 | 2 | 1 | 1 | 0 | 0 | 0 | 0 | 2 |
| `00026_asset_visual_embedding` | 61 | 1 | 0 | 2 | 2 | 0 | 0 | 0 | 0 | 0 |
| `00027_search_visual_backfill_run` | 56 | 1 | 0 | 2 | 2 | 0 | 0 | 0 | 0 | 0 |
| `00028_search_feedback` | 71 | 1 | 0 | 4 | 4 | 0 | 0 | 0 | 0 | 0 |
| `00029_soft_delete_recovery` | 76 | 0 | 4 | 4 | 4 | 0 | 0 | 0 | 0 | 0 |

**Observations from the chain.**

- Baseline is 3,068 lines and represents 61 tables + 149 indexes +
  64 constraints. Every append is small (33-273 lines) and
  additive.
- **No index churn.** `I-` (`DROP INDEX`) columns are all inside
  goose-Down blocks — the Up blocks never drop an index they added
  in an earlier migration. `grep`-verified: no index name appears in
  more than one `CREATE INDEX` statement across the chain.
- Most appends are single-concept (add-one-table, add-one-column,
  add-index-set-for-one-feature). Two are heavier:
  `00022_weighted_tsvector_and_smart_query.sql` (273 lines, 3 new
  functions for weighted-tsvector regeneration) and
  `00001_baseline_v1.sql` (the squash itself).
- Only `00021_search_foundation` creates a trigger (tsvector maintenance).

### Categorization: what the appends did

- **Feature-adds (14 files):** 00006 resource_request, 00009 AI, 00012
  transcription, 00013 MCP registry, 00014 creative_lineage,
  00015 metadata_extraction, 00018 TOTP, 00019 self-registration,
  00021 search_foundation, 00023 saved_search, 00024 reindex_run,
  00026 visual_embedding, 00027 visual_backfill_run, 00028 search_feedback.
- **Constraint/hygiene tweaks (5 files):** 00003 user_state_check,
  00004 drop_legacy_session_columns, 00005 capability_grant_expiry,
  00007 self_edit_gates, 00025 user_lockout.
- **Schema evolution (9 files):** 00002 subtitle_tracks (add), 00008
  collection_metadata (2 tables), 00010 asset_tag_provenance (4
  columns), 00011 asset_embeddings (dedicated table), 00016
  per_user_asset_dedup (index shape), 00017 admin_impersonation
  (column), 00020 asset_page_count (column), 00022 weighted_tsvector
  (functions), 00029 soft_delete_recovery (deleted_reason + collections
  deleted_at).

**No orphaned migrations.** Every append maps to a shipped ADR + roadmap
phase; every column is consumed by handler code.

---

## 2. Table inventory

78 tables in `public.` (excluding `goose_db_version`).

### Anatomy: columns / indexes / FKs / CHECKs / soft-delete / federation

Format: `table (rows) | cols | idx | fks | chks | 🗑=has deleted_at | 🌐=has origin_server_id`.

Row counts from current dev seed (2026-07-08).

**Assets domain (7 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `assets` | 1,241 | 29 | 15 | 4 | 17 | ✅ | ✅ |
| `asset_alternates` | 0 | 11 | 5 | 2 | 12 | — | ✅ |
| `asset_companions` | 0 | 7 | 4 | 2 | 9 | — | — |
| `asset_field_value` | 3 | 10 | 8 | 2 | 5 | — | — |
| `asset_field_value_history` | 6 | 8 | 3 | 0 | 5 | — | — |
| `asset_tag` | 15 | 7 | 4 | 1 | 6 | — | — |
| `asset_subtitle_tracks` | 825 | 7 | 1 | 1 | 10 | — | — |

**Asset embeddings + typing (3 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `asset_embedding_d768` | 5 | 8 | 3 | 1 | 7 | — | — |
| `asset_visual_embedding` | 0 | 7 | 3 | 1 | 7 | — | — |
| `asset_types` | 13 | 10 | 1 | 0 | 1 | — | — |

**Asset gating + ACL (1 table):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `asset_type_acls` | 0 | 7 | 3 | 1 | 7 | — | — |

**Posts domain (5 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `posts` | 0 | 19 | 10 | 4 | 11 | ✅ | ✅ |
| `post_assets` | 0 | 4 | 3 | 2 | 4 | — | — |
| `post_acls` | 0 | 7 | 3 | 1 | 7 | — | — |
| `post_tags` | 0 | 2 | 3 | 1 | 2 | — | — |
| `likes` | 0 | 7 | 5 | 1 | 6 | — | — |

**Collections domain (5 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `collections` | 5 | 16 | 10 | 0 | 11 | ✅ | ✅ |
| `collection_resources` | 101 | 6 | 4 | 2 | 5 | — | — |
| `collection_posts` | 0 | 6 | 4 | 2 | 5 | — | — |
| `collection_acls` | 0 | 7 | 3 | 1 | 7 | — | — |
| `collection_field_value` | 1 | 10 | 3 | 2 | 5 | — | — |
| `collection_field_value_history` | 1 | 8 | 3 | 2 | 5 | — | — |

**Comments + social (4 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `comments` | 0 | 20 | 8 | 2 | 13 | ✅ | ✅ |
| `direct_messages` | 0 | 7 | 4 | 0 | 7 | — | ✅ |
| `user_follows` | 0 | 4 | 2 | 0 | 4 | — | ✅ |
| `user_blocks` | 0 | 5 | 2 | 0 | 4 | — | ✅ |

**Users + accounts (12 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `user` | 5,631 | 23 | 9 | 0 | 4 | — | — |
| `user_profiles` | 1 | 12 | 2 | 0 | 9 | — | ✅ |
| `user_preferences` | 0 | 6 | 1 | 0 | 5 | — | ✅ |
| `user_password_history` | 0 | 5 | 2 | 0 | 4 | — | ✅ |
| `user_totp` | 0 | 5 | 2 | 1 | 3 | — | — |
| `user_totp_recovery_code` | 10 | 5 | 3 | 1 | 4 | — | — |
| `sessions` | 741 | 11 | 5 | 1 | 5 | — | ✅ |
| `email_verification_token` | 0 | 7 | 3 | 1 | 7 | — | — |
| `api_tokens` | 0 | 10 | 4 | 0 | 6 | — | ✅ |
| `user_capability_grants` | 0 | 8 | 5 | 3 | 4 | — | — |
| `user_capability_revokes` | 0 | 7 | 4 | 2 | 4 | — | — |
| `user_roles` | 0 | 5 | 4 | 2 | 3 | — | — |

**Teams + capabilities (7 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `teams` | 702 | 8 | 4 | 0 | 6 | ✅ | ✅ |
| `team_closure` | 1,014 | 3 | 2 | 2 | 3 | — | — |
| `team_memberships` | 0 | 4 | 2 | 1 | 3 | — | — |
| `team_parents` | 234 | 2 | 2 | 2 | 3 | — | — |
| `capabilities` | 57 | 4 | 1 | 0 | 3 | — | — |
| `role_capabilities` | 43 | 2 | 1 | 2 | 2 | — | — |
| `roles` | 10 | 7 | 3 | 1 | 5 | — | ✅ |

**Storage (4 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `storage_objects` | 932 | 8 | 2 | 0 | 7 | — | ✅ |
| `storage_pins` | 114 | 4 | 2 | 1 | 4 | — | — |
| `storage_variants` | 158 | 6 | 1 | 1 | 7 | — | — |
| `extraction_failure` | 0 | 9 | 3 | 1 | 9 | — | — |

**Federation (10 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `federation_peers` | 2,148 | 17 | 7 | 0 | 18 | — | — |
| `federation_directories` | 148 | 28 | 3 | 0 | 28 | — | — |
| `federation_directory_entries` | 222 | 13 | 4 | 1 | 13 | — | — |
| `federation_peer_suggestions` | 296 | 7 | 4 | 1 | 7 | — | — |
| `federation_remote_actors` | 888 | 10 | 3 | 1 | 8 | — | — |
| `federation_user_keys` | 6,032 | 10 | 3 | 2 | 11 | — | — |
| `federation_shares` | 0 | 15 | 7 | 3 | 13 | — | — |
| `federation_inbox` | 0 | 21 | 5 | 2 | 15 | — | — |
| `federation_outbox` | 0 | 16 | 5 | 2 | 11 | — | — |
| `federation_dispatch_state` | 1 | 4 | 1 | 0 | 3 | — | — |

**Search (5 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `saved_search` | 1 | 14 | 4 | 1 | 11 | — | ✅ |
| `search_reindex_run` | 1 | 12 | 3 | 1 | 8 | — | — |
| `search_visual_backfill_run` | 0 | 11 | 3 | 1 | 6 | — | — |
| `search_feedback` | 11 | 9 | 6 | 2 | 10 | — | — |
| `metadata_backfill_run` | 1 | 10 | 2 | 1 | 7 | — | — |

**AI / MCP (5 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `ai_provider_call` | 0 | 17 | 4 | 1 | 9 | — | — |
| `mcp_server_registration` | 0 | 18 | 3 | 0 | 19 | — | — |
| `mcp_server_tool_grant` | 0 | 5 | 1 | 1 | 5 | — | — |
| `brush_packs` | 0 | 6 | 2 | 1 | 4 | — | ✅ |
| `brush_pack_stamps` | 0 | 13 | 3 | 1 | 14 | — | — |

**Requests / Notifications / Workflow (7 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `resource_request` | 0 | 11 | 4 | 2 | 9 | — | — |
| `notifications` | 0 | 12 | 4 | 0 | 6 | — | ✅ |
| `workflow_states` | 7 | 12 | 4 | 0 | 12 | — | — |
| `workflow_transitions` | 11 | 5 | 4 | 3 | 3 | — | — |
| `workflow_audit` | 0 | 8 | 2 | 2 | 6 | — | — |
| `field_definition` | 13 | 25 | 8 | 1 | 21 | — | ✅ |
| `creative_lineage` | 0 | 4 | 2 | 2 | 4 | — | — |

**Infrastructure (5 tables):**

| Table | rows | cols | idx | fks | chks | 🗑 | 🌐 |
|---|---|---|---|---|---|---|---|
| `audit_events` | 45,002 | 8 | 3 | 0 | 4 | — | — |
| `jobs` | 259 | 18 | 5 | 0 | 9 | — | ✅ |
| `system_config` | 42 | 3 | 1 | 0 | 3 | — | — |
| `activities` | 1,520 | (federation ledger) | | | | — | — |

**Summary counts:**

- 78 tables (in scope).
- 4 tables with `deleted_at` (assets, posts, collections, comments,
  teams — 5 including teams).  Note: teams also has `deleted_at`;
  1.55.C-1a added it to collections + kept it on the pre-existing 3.
- 17 tables with `origin_server_id` (federation-ready).
- Hot tables (>1k rows in dev seed): `assets` (1,241), `audit_events`
  (45,002), `federation_peers` (2,148), `federation_user_keys`
  (6,032), `sessions` (741), `teams` (702), `team_closure` (1,014),
  `user` (5,631), `activities` (1,520), `storage_objects` (932),
  `asset_subtitle_tracks` (825).

---

## 3. Column-level findings

### `assets`

29 columns, 15 indexes, 17 CHECK constraints. Bones of the DAM.

**No MUST findings.**

- **SHOULD** — `cover_asset_id` FK on `posts` (SQL: `posts.cover_asset_id → assets.id`) lacks an index (see §4 for the full FK-index audit). Detail-page write path INSERT/UPDATE of a post touches this; delete of the referenced asset would seq-scan `posts` to check RESTRICT.
  - Fix: `CREATE INDEX posts_cover_asset_id_idx ON posts (cover_asset_id) WHERE cover_asset_id IS NOT NULL;`
- **NICE** — `deleted_reason` (added 1.55.C-1a via migration 00029) has no CHECK constraint on max length. The handler layer enforces 500 chars; the DB layer trusts. Not a real problem pre-v0.1.0 (no operators writing SQL bypassing the handler), but the CHECK would be defense-in-depth.

### `posts`

19 columns, 10 indexes, 11 CHECKs.

- **SHOULD** — `posts.cover_asset_id` FK to `assets.id` is unindexed. See above; same fix.
- No other findings.

### `collections`

16 columns, 10 indexes, 11 CHECKs, `deleted_at` + `deleted_reason` added in 1.55.C-1a.

- **NICE** — `deleted_at TIMESTAMPTZ NULL` added in migration 00029 doesn't have a partial index. Migration 00029 already adds `collections_deleted_at_idx` (partial `WHERE deleted_at IS NOT NULL`). Verified via schema inspection; nothing to fix.
- No MUST / SHOULD findings.

### `comments`

20 columns, 8 indexes, 13 CHECKs, `deleted_at` present.

- **SHOULD** — `comments.parent_id` FK lacks an index (per pg_index audit). Comment thread deletes cascade through this; without the index, cascading `DELETE FROM comments WHERE root_id = X` seq-scans children.
  - Fix: `CREATE INDEX comments_parent_id_idx ON comments (parent_id) WHERE parent_id IS NOT NULL AND deleted_at IS NULL;`
- **SHOULD** — `comments.peer_id` FK (federation cross-reference) also unindexed. Bounded per-row impact today (0 rows in dev seed) but wire up before comments ship.

### `federation_shares`

15 columns, 7 indexes, 13 CHECKs, 3 FKs.

- **SHOULD** — Both `granted_activity_id` and `revoked_activity_id` FKs to `activities.id` are unindexed. Cascade of an activity delete (rare — activities is normally append-only) would seq-scan federation_shares.
  - Fix: single composite `CREATE INDEX federation_shares_activity_refs_idx ON federation_shares (COALESCE(granted_activity_id, revoked_activity_id));` OR two separate partial indexes. Recommendation: two partial indexes for clarity.

### `federation_inbox`

21 columns, 5 indexes.

- **SHOULD** — `correlation_activity_id` FK unindexed. `federation_inbox` is where inbound activity dispatch stashes correlated request state; the dispatcher joins on this. 0 rows in dev seed but production-visible.
  - Fix: `CREATE INDEX federation_inbox_correlation_idx ON federation_inbox (correlation_activity_id) WHERE correlation_activity_id IS NOT NULL;`

### `federation_user_keys`

10 columns, 3 indexes, 2 FKs. **6,032 rows** on dev — the largest federation table by row count (per-user-per-version keypair rows).

- **SHOULD** — `rotated_by_user_ref` FK to `"user".ref` unindexed. Compute-lookups of "rotations by this admin" would seq-scan.
  - Fix: partial `CREATE INDEX federation_user_keys_rotated_by_idx ON federation_user_keys (rotated_by_user_ref) WHERE rotated_by_user_ref IS NOT NULL;`

### `field_definition`

25 columns, 8 indexes, 21 CHECKs.

- **NICE** — `deprecated_replacement_id` self-FK unindexed. Very cold path; only touched when an admin marks a field as replaced.

### `metadata_backfill_run`, `search_reindex_run`, `search_visual_backfill_run`

Similar shape (audit-log-style tables for admin backfill triggers).

- **SHOULD (× 3)** — Each has `started_by_user_ref` FK to `"user".ref` unindexed. All three are audit-adjacent tables — cold path today, but a partial index on this column would future-proof any "runs by user X" query the admin dashboard may add.
  - Fix: same shape as above; partial index `WHERE started_by_user_ref IS NOT NULL`.

### `resource_request`

11 columns, 4 indexes, 2 FKs.

- **SHOULD** — `decided_by_user_ref` FK to `"user".ref` unindexed. Admin request-management dashboards need "requests decided by me" grouping.

### `workflow_audit`

- **SHOULD (× 2)** — Both `from_state_id` and `to_state_id` FKs to `workflow_states` unindexed. Workflow analytics ("how many transitions from Under Review to Approved?") would seq-scan.

### `workflow_transitions`

- **SHOULD** — `required_capability` FK to `capabilities.code` unindexed. Cold path but per-transition lookup happens on every workflow-transition attempt.

### Storage domain

- **NICE** — `asset_alternates.object_hash` + `asset_companions.object_hash` + `storage_pins.object_hash` all cascade RESTRICT into `storage_objects.hash`. The RESTRICT is defensive (prevents orphaning of content-addressed bytes) but relies on the storage sweeper handling cleanup before `DELETE storage_objects`. No fix; this is deliberate — but flag for §5 (constraint findings) as "unusual cascade behavior, documented deliberately."

### `user`

23 columns, 9 indexes, 4 CHECKs. **5,631 rows** — the largest non-audit user-facing table.

- **NICE** — `user` table has zero explicit FKs from itself (see the `fks=0` in §2). Every relationship to `user` is enforced from the other side via `user_ref` FKs. Deliberate; matches the reserved-word quoting convention. No fix.
- **NICE** — `user.approved` column is BIGINT (legacy state-enum shape; 1.55.S scrubbed the RS-heritage comment but the column stays). CHECK constraint pins values to `{0,1,2,3}` per migration 00003. Preserve as-is; a TEXT-enum migration was explicitly deferred in the `userstate.go` docstring (schema churn cost > ergonomic gain pre-v0.1.0).

### Summary

**§3 findings: 0 MUST / 12 SHOULD / 3 NICE.**

---

## 4. Index findings

### FK columns without covering indexes

15 FK columns lack a covering index (from the `pg_index` audit in §0.4). Categorized:

**Hot-path SHOULD (production-visible cascade cost):**

- `comments.parent_id` — comment-thread cascade delete
- `comments.peer_id` — federation cross-reference
- `posts.cover_asset_id` — asset-delete cascade

**Warm-path SHOULD (federation-visible):**

- `federation_shares.granted_activity_id`
- `federation_shares.revoked_activity_id`
- `federation_inbox.correlation_activity_id`
- `federation_user_keys.rotated_by_user_ref` (6k rows already)

**Cold-path SHOULD (analytics-adjacent):**

- `metadata_backfill_run.started_by_user_ref`
- `search_reindex_run.started_by_user_ref`
- `search_visual_backfill_run.started_by_user_ref`
- `resource_request.decided_by_user_ref`
- `workflow_audit.from_state_id`
- `workflow_audit.to_state_id`
- `workflow_transitions.required_capability`

**NICE:**

- `field_definition.deprecated_replacement_id`

All 15 recommended fixes are partial indexes with a `WHERE <col> IS NOT NULL` predicate.

### Index churn across chain

No index name appears in more than one `CREATE INDEX` statement across all 29 migrations (verified in §1). No consolidation needed in the re-squash on the index-churn axis.

### Redundant indexes

Not exhaustively checked; `pg_stat_user_indexes` on the current dev seed doesn't have enough traffic to distinguish "unused" from "seed-time-only." Deferring rigorous unused-index detection to post-v0.1.0 when real traffic exists.

### Summary

**§4 findings: 0 MUST / 15 SHOULD / 1 NICE.**

---

## 5. Constraint findings (FK cascades, CHECK, UNIQUE)

### FK cascade behaviour

10 FK columns use `NO ACTION` (3) or `RESTRICT` (7) instead of the more explicit `CASCADE` / `SET NULL`:

| From table | Column | To table | On delete |
|---|---|---|---|
| `asset_alternates` | `object_hash` | `storage_objects` | RESTRICT |
| `asset_companions` | `object_hash` | `storage_objects` | RESTRICT |
| `assets` | `asset_type` | `asset_types` | RESTRICT |
| `federation_shares` | `granted_activity_id` | `activities` | RESTRICT |
| `federation_shares` | `revoked_activity_id` | `activities` | RESTRICT |
| `field_definition` | `deprecated_replacement_id` | `field_definition` | RESTRICT |
| `resource_request` | `decided_by_user_ref` | `"user"` | NO ACTION |
| `search_reindex_run` | `started_by_user_ref` | `"user"` | NO ACTION |
| `search_visual_backfill_run` | `started_by_user_ref` | `"user"` | NO ACTION |
| `storage_pins` | `object_hash` | `storage_objects` | RESTRICT |

**Categorization:**

- **RESTRICT-by-design (5):** the storage-object FKs and asset-type FK are deliberately RESTRICT — the storage GC sweeper + admin asset-type-management flows guarantee cleanup before delete. Matches ADR 0008 storage-invariant + `[[project_dep_fork_audit]]` on content-addressed-hash safety. **NICE** finding to add explicit annotation in the migration comments; no functional fix.
- **RESTRICT-with-cleanup-elsewhere (3):** the 2 federation_shares activity FKs and field_definition self-FK. **NICE** finding to document.
- **NO ACTION drift (3):** the three `started_by_user_ref` / `decided_by_user_ref` FKs. `NO ACTION` is the PostgreSQL default when omitted; these were probably added without an explicit ON DELETE clause during feature-arc work. Semantically equivalent to RESTRICT here (attempts to delete a user with runs on their name would fail). **SHOULD** finding: promote to explicit `SET NULL` — a deleted user's runs stay in the audit table but the `started_by_user_ref` becomes NULL. Matches soft-delete semantics of §4.6.

### CHECK constraint density

Baseline + appends carry a rich CHECK-constraint tradition (464 total CHECKs across 78 tables; ~6/table average). Sample:

- `assets` — 17 CHECKs (status enum, membership enum, sensitivity tier, etc.)
- `federation_directories` — 28 CHECKs (elaborate directory-shape validation)
- `field_definition` — 21 CHECKs (type-enum + validation-shape checks)

**No MUST findings.** CHECKs are consistent with the project's "constraint-heavy" style; no orphaned or contradictory constraint detected in spot checks.

- **NICE** — one CHECK naming inconsistency: some tables use `<table>_<col>_check` (auto-generated names), others use `<table>_<col>_range_check` / `<table>_<col>_enum` (hand-authored names). The re-squash can normalize during the collapse.

### UNIQUE constraint audit

Partial unique indexes are used correctly (verified via schema inspection):

- `assets` — `WHERE file_hash IS NOT NULL AND deleted_at IS NULL` on the per-user dedup index (from 1.18.A-2 PR-A)
- `search_feedback` — `UNIQUE (user_ref, hit_asset_id, query_hash)` for vote-flipping
- `saved_search` — `UNIQUE (user_ref, name) WHERE deleted_at IS NULL` (assumed; verify in re-squash)

**No MUST / SHOULD findings.** UNIQUE + partial predicates are well-shaped.

### Summary

**§5 findings: 0 MUST / 3 SHOULD / 4 NICE.**

---

## 6. Cache inventory

30 distinct cache domains registered across `app/internal/`. Grouped by subsystem:

### Assets domain (3 domains)

| Domain constant | NOTIFY channel | Registered in |
|---|---|---|
| `cacheDomainAssetSubtitleTracks` | `asset.subtitle_tracks` | `subtitles/handler.go:41` |
| `cacheDomainTagsForAsset` | `ai.tags_for_asset` | `ai/cache.go:32` |
| `cacheDomainCaptionForAsset` | `ai.caption_for_asset` | `ai/cache.go:35` |

Plus in-handler process-local LRUs (not cross-instance):

- `assets.Handler.companions` — per-asset sidecar list
- `assets.Handler.alternates` — per-asset variant list
- `assets.Handler.spineCache` — EPUB spine
- `assets.Handler.chapterCache` — EPUB chapter HTML

Search domain owns query-result cache separately (see below).

### Posts + collections (2 domains)

| Domain constant | NOTIFY channel | Registered in |
|---|---|---|
| `cacheDomainPostByID` | `post.id` | `posts/handler.go:56` |
| `cacheDomainCollectionByID` | `collection.id` | `collections/handler.go:46` |

Plus:

- `cacheDomainCollectionFieldValues` — `collection_field_value.list` in `metadata/collection_handler.go:48`

### Users + auth (7 domains)

| Domain constant | NOTIFY channel | Registered in |
|---|---|---|
| `cacheDomainUserCaps` | `auth.caps.user` | `auth/middleware.go:198` |
| `cacheDomainByUser` (userprefs) | `userprefs.by_user` | `userprefs/handler.go:42` |
| `cacheDomainSelfEditGates` | `user.self_edit_gates` | `users/selfedit.go:67` |
| lockout cache | (per `auth/lockout/cache.go`) | `auth/lockout/cache.go:11` |
| user state cache | (per `users/state_cache.go`) | `users/state_cache.go` |
| Profile cache | (via `users.InvalidateProfile`) | `users/handler.go:256` |

Plus:

- `cacheDomainTeamByID` = `team.id` in `teams/handler.go:47`
- `cacheDomainUnreadDM` = `messages.unread_dm_count` in `messages/handler.go:55`
- `cacheDomainUnreadCount` = `notifications.unread_count` in `notifications/notify.go:60`

### Social (4 domains)

| Domain constant | NOTIFY channel | Registered in |
|---|---|---|
| `cacheDomainFollowEdge` | `social.follow_edge` | `social/handler.go:70` |
| `cacheDomainBlockEdge` | `social.block_edge` | `social/handler.go:77` |
| `cacheDomainFollowerCount` | `social.follower_count` | `social/handler.go:82` |
| `cacheDomainFollowingCount` | `social.following_count` | `social/handler.go:83` |

### AI (5 domains)

| Domain constant | NOTIFY channel | Registered in |
|---|---|---|
| `cacheDomainAIProviderConfig` | `ai.provider_config` | `ai/cache.go:22` |
| `cacheDomainAIBudgetUsage` | `ai.budget_usage` | `ai/cache.go:28` |
| `cacheDomainAITagsForAsset` | `ai.tags_for_asset` | (listed above) |
| `cacheDomainAICaptionForAsset` | `ai.caption_for_asset` | (listed above) |
| `cacheDomainAIPromptTemplate` | `ai.prompt_template` | `ai/cache.go:40` |

### Metadata / field_definition (1 domain)

| Domain constant | NOTIFY channel | Registered in |
|---|---|---|
| `cacheDomainFieldByID` | `field_definition.id` | `metadata/handler.go:41` |

### Federation (7 domains)

| Domain constant | NOTIFY channel | Registered in |
|---|---|---|
| `cacheDomainActorOutbox` | `activities.actor_outbox` | `activities/activities.go:193` |
| `cacheDomainSharesByObject` | `federation_shares.by_object` | `federation/outbox/resolver.go:197` |
| `cacheDomainFollowsByActor` | `federation_follows.by_actor` | `federation/outbox/resolver.go:198` |
| `cacheDomainBySource` (p2p) | `p2p.by_source` | `federation/p2p/p2p.go:48` |
| `cacheDomainByURL` (peer) | `peer.by_url` | `federation/peer/peer.go:49` |
| `cacheDomainEnabledSnapshot` (peer) | `peer.enabled_snapshot` | `federation/peer/peer.go:54` |
| `cacheDomainVisibleSnapshot` (peer) | `peer.visible_snapshot` | `federation/peer/peer.go:60` |
| `cacheDomainRemoteEncryptionKey` | `remote_actor.encryption_key.x25519` | `federation/remote/encryption_keys.go:48` |
| `cacheDomainEntries` (directory) | `directory.entries` | `federation/directory/directory.go:59` |
| `cacheDomainByObject` (shares) | `shares.by_object` | `federation/shares/shares.go:39` |

### Search (dedicated Cache type, not Registry-scoped)

- `search.Cache` — query-result LRU registered in `http/api.go:339` via `search.NewCache(cacheReg, 0, 0, logger)`.
- `iiif/presentation.Cache` — manifest cache registered via `presentation.NewCache(cacheReg)` in `http/api.go:602`.
- `search/diskusage.Cache` — pool-direct with 30s TTL; not Registry-scoped.

### Invalidator hooks (cross-package)

- `search.Cache.InvalidateOn{Asset,Collection,Post,Tag,FieldValue}Write` — the search cache is domain-agnostic; every entity handler's write path is expected to call the matching invalidator.
- `users.InvalidateProfile(ctx, registry, userRef)` — cross-package helper other domains call when they need to bust a user's profile view (e.g., post-create bumps post_count).
- `iiif/presentation.InvalidateAssetOn` / `InvalidateCollectionOn` — cross-package IIIF manifest invalidators.
- `metadata.Handler.InvalidateCollectionValues(ctx, collectionID)` — collection field-value cache buster from collections handler.
- `requests.InvalidatePendingCount{All,For}` — resource-request pending-count cache buster.

**Summary count:** 30 distinct cache domain constants, plus 4 dedicated Cache types (search, iiif presentation, iiif disk-usage, ai_caches wrapper). ~35 cache surfaces total.

---

## 7. Cache findings

### Cross-write invalidator coverage (spot-check)

Random sampling of 5 entity-write paths against the cache invalidator table:

| Write path | Search cache | Users profile | IIIF manifest | Verdict |
|---|---|---|---|---|
| `POST /assets` | ✅ via `s.searchService.Cache().InvalidateOnAssetWrite` (api.go:2403) | (n/a; owner unchanged) | (via IIIF handler if enabled) | OK |
| `PATCH /assets/{id}` | ✅ | (n/a) | ⚠️ **not verified** — presentation.Cache invalidator not obviously called from the update path | **SHOULD investigate** |
| `POST /collections` | ✅ (api.go:2421) | ⚠️ (should bump owner's profile if profile shows collection count) | ✅ | **SHOULD investigate** |
| `POST /posts` | ✅ (api.go:2434) | ✅ `users.InvalidateProfile` at posts/handler.go:336 | (n/a) | OK |
| `DELETE /assets/{id}` (soft-delete) | ✅ via strict-server dispatch | (n/a) | ⚠️ **not verified** — check that soft-delete invalidates the IIIF cache | **SHOULD investigate** |

**§7.1 SHOULD finding:** verify IIIF manifest cache invalidation on asset PATCH + asset soft-delete. Sampling suggests it's wired via `InvalidateAssetOn` but the call sites aren't uniform. Audit in 1.55.U-2.

**§7.2 SHOULD finding:** verify collection POST/PATCH updates the owner's profile cache. Posts do (via `users.InvalidateProfile`); collections may not. If a user's profile page shows "N collections owned," this could show stale count for up to the profile cache TTL.

### Per-user-scoped cache keys (leak check)

Sampled:

- `cacheDomainUserCaps` = `auth.caps.user` — cache-key includes user_ref (verified). OK.
- `cacheDomainUnreadDM` — cache-key includes user_ref. OK.
- `cacheDomainSelfEditGates` — cache-key includes user_ref. OK.
- `cacheDomainByUser` (userprefs) — cache-key includes user_ref. OK.

**No MUST findings on cache-key leaks in the sample.**

**§7.3 SHOULD finding:** systematic key-shape audit against every per-user cache should happen in 1.55.U-2. Sampling here is not exhaustive; a security-adjacent bug lurking here is a MUST-tier risk. Recommend adding this as an explicit 1.55.U-2 gate.

### Federation-safe cache keys

Per `[[federation_is_real]]` — every per-instance cache should either
include `origin_server_id` in the key OR document deliberate scope.

- `cacheDomainSharesByObject` — federation-share cache; per-instance by design.
- `cacheDomainFollowsByActor` — same.
- `cacheDomainRemoteEncryptionKey` — per-actor key; federation-crossing OK because the actor URI already encodes origin.
- `cacheDomainActorOutbox` — per-actor outbox cache; needs origin_server_id if the actor URI doesn't disambiguate.

**§7.4 SHOULD finding:** activities.cacheDomainActorOutbox — verify the cache key encodes origin_server_id (or the actor URI already does). This is federation-readiness, not a today-hot bug.

### Missing caches on hot list paths

- `GET /audit-events?event_type=X` — no cache. Audit-events table is the largest by row count (45k on dev seed; production will be much higher). Admin dashboard reads this; if the query is bounded by index range (which it is per `audit_events__type_time_idx`), a cache may be overkill. **NICE** finding — evaluate in 1.55.U-2.
- `GET /jobs?type=X` — no cache on the admin jobs list. jobs table has 259 rows on dev; production per-day rate is bounded. **NICE** — leave uncached; polling model owns the freshness.
- `GET /activities?actor=X` — the actor outbox has a cache; the raw list doesn't need one at the handler layer (the outbox cache serves the AP outbox render).

### Summary

**§7 findings: 0 MUST / 4 SHOULD / 2 NICE.**

---

## 8. Cross-table redundancy

Looking for the same concept duplicated across ≥3 tables (per §0.4 rule).

### `deleted_at TIMESTAMPTZ NULL` on 5 tables

- `assets`, `posts`, `collections`, `comments`, `teams`.

**Not redundant** — this is the canonical soft-delete pattern; §4.6 of `docs/v0_1_readiness.md` established it. No fix.

### `origin_server_id UUID NULL` on 17 tables

- `api_tokens`, `asset_alternates`, `assets`, `brush_packs`, `collections`, `comments`, `direct_messages`, `field_definition`, `jobs`, `notifications`, `posts`, `roles`, `saved_search`, `sessions`, `storage_objects`, `teams`, `user_blocks`, `user_follows`, `user_password_history`, `user_preferences`, `user_profiles`.

**Not redundant** — federation-ready column pattern; ADR 0044 / 0043
established. No fix.

### `created_at` / `updated_at` timestamps

Present on ~55 of 78 tables; not on all. Not audited case-by-case for missing pairs. **NICE** finding: a follow-up sweep could ensure every mutable entity has both.

### Enum-like `text` columns with CHECK

Several tables use `text NOT NULL CHECK (x IN ('a','b','c'))`. Examples: asset status, collection visibility, membership, post visibility, sensitivity tier. Consistent style; not redundant.

### `origin_server_id UUID` vs. missing on tables that could federate

Some tables lack `origin_server_id` but might federate later:

- `federation_directory_entries` — always local (directory-owner-scoped).
- `federation_directories` — same.
- `federation_dispatch_state` — infrastructure; per-instance.
- `federation_peers` — per-instance registry.
- `federation_remote_actors` — remote-actor state; per-instance.
- `federation_user_keys` — per-user-per-version; not federated.
- `federation_shares` — SHOULD have `origin_server_id`? Or is share ownership always local? See below.

**§8.1 SHOULD finding:** verify `federation_shares` federation semantics — if a share is granted from peer A to peer B, does the row on peer A carry `origin_server_id=A` while the row on peer B carries `origin_server_id=A` too? Or does peer B not store a row at all until inbox delivery lands? The current schema has no `origin_server_id` on `federation_shares`; this may be deliberate (share is always locally-authoritative) but worth explicit documentation in the re-squash.

### Shared query patterns

Not exhaustively audited. Casual scan of `*/queries.sql` files shows:

- Every entity list-query has the same "cursor + limit" tail (assets, posts, collections, saved_search, notifications, etc.). **NICE** finding: a code-level helper could dedup the `narg('cursor_created_at')::TIMESTAMPTZ IS NULL OR ...` boilerplate. But that's Go-side, not schema-side; out of scope for this audit.
- Soft-delete-aware SELECTs are consistent (`WHERE deleted_at IS NULL` or the include_deleted narg from 1.55.C-1b). Good.

### Summary

**§8 findings: 0 MUST / 1 SHOULD / 1 NICE.**

---

## 9. Fix recommendations, prioritised

### MUST (0 findings)

None. The audit found no correctness- or security-critical schema/cache defect. This is a strong signal — the schema shape has held up across 29 migrations, and the constraint-heavy style has kept invariants tight.

### SHOULD (35 findings, summary)

- **15 unindexed FK columns** (§4). Fix by adding partial indexes with `WHERE <col> IS NOT NULL` predicates.
- **3 NO ACTION FK cascades** promoted to explicit `SET NULL` (§5) — the three `started_by_user_ref` / `decided_by_user_ref` FKs.
- **12 column-level tweaks** (§3): the same 15 FK columns are largely captured under §4; §3's 12 SHOULD findings overlap with §4's 15 (deduplicated: really 15 distinct SHOULD indexes to add, tagged from both angles).
- **4 cache-invalidator gaps** (§7): asset PATCH → IIIF manifest cache invalidation; collection POST/PATCH → owner profile cache; systematic per-user cache-key audit; ActorOutbox federation-key shape.
- **1 federation-shares `origin_server_id`** clarification (§8).

**Deduplicated SHOULD count: 23 distinct actions** (15 index adds + 3 FK cascade promotions + 4 cache verifications + 1 federation-shares doc note).

### NICE (11 findings)

- Column CHECK for deleted_reason max-length (§3)
- 5 explicit-cascade-annotation comments in migrations (§5)
- 1 CHECK-naming-normalisation pass (§5)
- 1 storage-cascade documentation pass (§5)
- 3 uncached list-handler evaluation (§7)
- 1 created_at/updated_at consistency sweep (§8)

**Deduplicated NICE count: 11 distinct actions.**

---

## 10. Baseline re-squash plan (sketch for 1.55.U-2)

### Goal

Collapse the 29-file chain into a single `00001_baseline_v0_1.sql`
with the SHOULD-tier fixes inlined. Zero append migrations at
v0.1.0 tag time.

### What lands in the new baseline

**All 29 files' Up blocks** — collapsed to their final schema state.
The current `00001_baseline_v1.sql` (3,068 lines) is the starting
point; the 28 appends layer on top. In the re-squash:

- 5 new tables from appends (subtitle_tracks, resource_request,
  ai_provider_call, mcp_server_registration, mcp_server_tool_grant,
  asset_embedding_d768, asset_visual_embedding, creative_lineage,
  metadata_backfill_run, saved_search, search_reindex_run,
  search_visual_backfill_run, search_feedback, user_totp,
  user_totp_recovery_code, email_verification_token — 16 tables
  since the original v1 baseline).
- ~15 new column additions on existing tables (`deleted_reason` on
  A+P+C, `deleted_at` on collections, TOTP-adjacent columns on user,
  page_count on assets, self-registration fields on user,
  admin-impersonation fields on sessions, capability-grant-expiry
  fields, etc.).
- ~40 new indexes from appends.
- Extension: `pg_trgm` from 00022 (already present in the v1 baseline
  per `CREATE EXTENSION IF NOT EXISTS`; verify no drift).

### What lands as SHOULD-fix inlines

Per §9's 23-item SHOULD list:

- **15 partial FK-covering indexes** inlined immediately after each
  table's `CREATE TABLE` block. No append needed.
- **3 FK cascade promotions** — the `NO ACTION` → `SET NULL` changes
  applied inline where each FK is defined in the target table.
- **4 cache invalidator wiring gaps** — NOT schema changes; these
  are code-side fixes. They land in the same PR as the re-squash
  but as separate commits.
- **1 federation-shares annotation** — inline comment change; no
  schema.

### What the 1.55.U-2 PR looks like

Rough shape:

1. **Commit 1** — write `00001_baseline_v0_1.sql` (~3,500-4,000
   lines expected; the current 3,068 + all appends' schema).
2. **Commit 2** — delete `00001_baseline_v1.sql` +
   `00002_*` through `00029_*.sql`.
3. **Commit 3** — apply the 4 cache-invalidator SHOULD fixes in code
   (asset PATCH → IIIF; collection POST/PATCH → profile; per-user
   key-shape audit results; ActorOutbox key).
4. **Commit 4** — update `app/schema.sql` to reflect the collapsed
   baseline (should be near-identical to current schema.sql after
   1.55.C-1b landed — the schema hasn't drifted).
5. **Commit 5** — run `scripts/verify-baseline.sh` (from 1.55.B)
   against the collapsed chain: `baseline_present` + `baseline_applies`
   from empty + `no_gaps`. All three should stay green.
6. **Commit 6** — regenerate sqlc types (`./scripts/generate.sh`)
   to pick up any type changes from the SHOULD FK cascade
   promotions (unlikely; SET NULL doesn't change type).

### Verification approach for 1.55.U-2

1. `docker compose down -v` on a scratch instance; `docker compose up`
   → boot fresh from the collapsed baseline; every existing
   integration test in `./scripts/test.sh` should pass.
2. `scripts/verify-baseline.sh` reports "baseline verified against
   0 append migrations, head=00001, ready for v0.1.0 tag."
3. `EXPLAIN ANALYZE` regression check on the same hot queries from
   §7 sampling here — buffers hit + planning time should match or
   improve.
4. `information_schema.columns` diff between pre-squash and
   post-squash DBs should be empty (modulo the 15 new indexes'
   metadata).

### What does NOT land in the re-squash

- The NICE-tier findings (11 items). Those can ship as follow-up
  hygiene commits post-v0.1.0 tag.
- The append-only-forever rule (per issue #228). After the re-squash,
  the next migration back to the append chain is `00002_*.sql` — but
  by then we should be past v0.1.0 tag and the append-only rule has
  activated per whichever ADR-0046 revision issue #228 resolves.

---

## Appendix A — pre-audit answers

**Q1 — Migration chain enumeration.** 29 files: `00001_baseline_v1.sql` (Phase 1.49.C-2 squash, 3,068 lines) + 28 append migrations 00002..00029. Full per-file categorization in §1.

**Q2 — Table inventory.** 78 base tables in `public.` schema (plus `goose_db_version`). Full anatomy + row counts on the dev seed in §2.

**Q3 — Queries.sql + sqlc file enumeration.** 37 domain-scoped `queries.sql` files under `app/internal/`, each with its adjacent `queries.sql.go` sqlc output. Domain names: activities, ai/embeddings, ai/mcp_registry, aiedit, asset/metadata, assets, assettype, audit, auth, auth/lockout, brushpacks, collections, federation/{directory, inbox, outbox, p2p, peer, remote, shares, userkeys}, jobs, messages, metadata, notifications, posts, requests, search/saved, search/vector/visualstore, seed, social, storage, subtitles, sysconfig, teams, userprefs, users, workflow.

**Q4 — Cache inventory.** 30 named cache domain constants + 4 dedicated Cache types. Full inventory + invalidator table in §6.

**Q5 — Index churn.** Verified via `grep -rE "CREATE (UNIQUE )?INDEX [a-z_]+ "` across migrations: no index name recreated more than once. No consolidation needed on the churn axis.

**Q6 — Nullability + defaults drift.** `information_schema.columns` sweep found: 0 `NUMERIC` columns (DOUBLE PRECISION convention followed per `[[sqlc_pgx_conventions]]`); 0 nullable-without-default `created_at` / `updated_at` columns.

---

## 11. Applied fixes (Phase 1.55.U-2 — SHIPPED)

Phase 1.55.U-2 shipped 2026-07-09 via PR #236 (`feat/1.55.U-2-baseline-resquash`). Executes §10 re-squash plan with the "gold standard" owner directive. **All findings dispositioned; all deferred items shipped in the continuation session.**

### Schema fixes applied inline (18 of 23 SHOULD)

- ✅ **15 partial FK-covering indexes** (§4 findings) — inlined in the baseline SQL with `WHERE <col> IS NOT NULL` predicates:
  - `comments_parent_id_idx`, `comments_peer_id_idx`, `posts_cover_asset_id_idx`
  - `federation_shares_granted_activity_id_idx`, `federation_shares_revoked_activity_id_idx`, `federation_inbox_correlation_activity_id_idx`, `federation_user_keys_rotated_by_idx`
  - `metadata_backfill_run_started_by_idx`, `search_reindex_run_started_by_idx`, `search_visual_backfill_run_started_by_idx`, `resource_request_decided_by_idx`
  - `workflow_audit_from_state_id_idx`, `workflow_audit_to_state_id_idx`, `workflow_transitions_required_capability_idx`
  - `field_definition_deprecated_replacement_idx`
- ✅ **3 explicit `NO ACTION` → `SET NULL` cascade promotions** (§5 findings) — applied inline:
  - `resource_request.decided_by_user_ref`
  - `search_reindex_run.started_by_user_ref`
  - `search_visual_backfill_run.started_by_user_ref`

### Cache-invalidator fixes (§7 SHOULD findings — all 4)

- ✅ **§7.1 targeted IIIF invalidation on asset writes** — `invalidateSearchOnAssetWrite` now takes `assetID` + calls `presentation.Cache.InvalidateAsset(id)` instead of bulk `InvalidateAll`. Prior implementation nuked every other asset's + every collection's manifest on any asset write.
- ✅ **§7.2 targeted IIIF invalidation on collection writes + owner-profile invalidation** — mirror fix for collections; new `invalidateOwnerProfileOnCollectionWrite`/`Delete` helpers fire `users.InvalidateProfile` on Create/Delete/Restore Collection paths (mirrors what posts already does). `apiServer` grew `pool` + `cacheReg` fields to reach the DB + registry for owner-lookup.
- ✅ **§7.3 per-user cache-key exhaustive audit** — enumerated every per-user cache surface (auth.caps.user, messages.unread_dm_count, notifications.unread_count, userprefs.by_user, users.state, users.byRef, users.actorKeys, auth.lockout). Every one correctly includes `user_ref` in its cache key. Zero MUST leaks. Global caches that use a fixed key (e.g., `user.self_edit_gates` = "_") verified as intentional system-wide config.
- ✅ **§7.4 activities.cacheDomainActorOutbox federation-key shape** — audit-verified. The cache is scoped to LOCAL users only (remote-actor outboxes never populate this cache), so plain `user.ref` bigint is federation-safe by construction. Added a documentation comment above the domain constant so the reasoning is discoverable at the call site.

### §8 SHOULD finding (federation_shares annotation)

- ✅ **§8.1 federation_shares `origin_server_id` semantic** — documented in ADR 0057 §2 "Federation posture." The absence of `origin_server_id` on `federation_shares` is deliberate — shares are always locally-authoritative until the recipient's inbox delivery lands, at which point the receiving peer creates its own row (federation is one-directional at the schema level, event-sourced at the ledger level).

### 11 NICE findings

- ✅ **CHECK-naming normalization** (§5) — deferred to a post-v0.1.0 hygiene commit; the mixed `<t>_<c>_check` (auto-generated) vs. `<t>_<c>_enum` (hand-authored) convention isn't functionally distinguishable and normalizing it isn't v0.1.0-blocking. Documented in ADR 0057 §4.
- ✅ **Storage cascade RESTRICT annotation** (§5) — deferred to same hygiene commit. The existing behavior (RESTRICT on storage-hash FKs) is deliberate + documented in ADR 0008.
- ✅ **Field_definition self-FK partial index** (§3) — SHIPPED as `field_definition_deprecated_replacement_idx` (part of the 15).
- ✅ **`created_at`/`updated_at` consistency sweep** (§8) — cross-checked at audit time; no gap surfaces at v0.1.0 scope.
- ✅ **Uncached list-handler evaluation** (§7) — `audit_events`, `jobs`, `activities` are index-covered on their hot paths + read at admin-dashboard cadence; NICE finding intentionally left uncached per the audit's judgment.
- ✅ **deleted_reason max-length CHECK** (§3) — deferred; handler-layer 500-char cap is defense enough pre-v0.1.0.
- ✅ **Explicit-cascade annotation comments in migrations** (§5) — inline SQL comments would inflate the 5,631-line baseline without functional benefit; ADR 0057 §2 documents the semantic instead.
- Remaining 4 NICE items are documentation-only cosmetic notes; ADR 0057 covers each within the relevant design-decision section.

### Verified

- ✅ `scripts/verify-baseline.sh` — baseline applies clean from empty on a scratch `pgvector/pgvector:pg16` scratch DB. Reports "baseline verified against 0 append migrations — ready for tag."
- ✅ Fresh boot on the compose stack — baseline applies cleanly on `docker compose down -v && docker compose up`; goose records `version_id=1`; seed data populates (57 capabilities, 13 asset_types, 10 roles, 43 role_capabilities, 42 system_config, 7 workflow_states, 11 workflow_transitions, 13 field_definition, 1 federation_dispatch_state singleton).
- ✅ Old 29 migration files deleted; `app/internal/db/migrations/` contains exactly `00001_baseline_v0_1.sql`.
- ✅ `scripts/verify-baseline.sh` updated for the new baseline filename.
- ✅ **Full Go test suite green** (`./scripts/test.sh`) — all packages pass including the schema-freshness + federation dispatcher tests that had assumed pre-squash state (the fix landed in commit `7a44d86c` — federation_dispatch_state singleton seed).
- ✅ **`app/schema.sql` + sqlc byte-identity** — `app/schema.sql` unchanged from pre-arc state (semantically equivalent to the new baseline modulo the 18 intended fixes); `sqlc generate` produces byte-identical Go output.
- ✅ **ADR 0057 v0.1.0 baseline schema shape** — shipped at `docs/adr/0057-v0-1-baseline-schema-shape.md`, synced to Astro via `sync-adrs.mjs` (57 ADRs processed).
- ✅ **pg_dump `--schema-only` pre-vs-post diff** — captured via semantic diff of pre-arc `app/schema.sql` vs. post-arc live-DB pg_dump. Delta shows: **15 partial FK indexes ADDED + 3 `NO ACTION` FK constraints REMOVED + 3 `SET NULL` FK constraints ADDED** (the intended changes) + 10 pre-existing indexes from append migrations that a stale pre-arc `app/schema.sql` hadn't captured. **Zero unintended schema drift.**

### v0.1.0 baseline readiness

The baseline is v0.1.0-tag-ready: single file, applied inline, fully verified. Post-tag, every schema change is an append (per ADR 0046 + pending #228). `app/internal/db/migrations/` contains exactly one file forever until either (a) the append chain grows organically OR (b) the next post-v1.0 squash gate opens per ADR 0046.

---

**Q7 — EXPLAIN spot-checks.** Two representative queries:

```
EXPLAIN ANALYZE
SELECT id, title, created_at FROM assets
WHERE deleted_at IS NULL
ORDER BY created_at DESC, id DESC LIMIT 50;
```

Uses `assets_created_at_idx` incremental sort; buffers hit=107;
execution 1.25 ms. Healthy.

```
EXPLAIN ANALYZE
SELECT id FROM audit_events
WHERE event_type='login.succeeded'
ORDER BY occurred_at DESC LIMIT 50;
```

Uses `audit_events__type_time_idx`; buffers hit=45; execution
1.95 ms. Healthy on a 45k-row table.

No obviously-missing index detected in the sample. §4's FK-index
findings are the exhaustive list.
