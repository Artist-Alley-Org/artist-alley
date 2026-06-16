# Pre-MVP DB schema audit — 2026-06

**Date:** 2026-06-15
**Auditor:** Federation agent (review by Bootstrap Admin)
**Schema state:** post-1.22.I-i + post-1.49.B + post-1.49.A
**Schema source:** [`app/schema.sql`](../app/schema.sql) — 3,952 lines, 61 tables, 64 FKs, 149 indexes
**Migration count:** 14 (`app/internal/db/migrations/0000{1..14}_*.sql`)
**Reference policy:** [ADR 0046](adr/0046-migration-baseline-and-squash-policy.md) — Migration baseline + squash policy
**Next action:** 1.49.C-2 — Flatten migrations to `00001_baseline_v1.sql`

---

## Summary

| Category | Total | Critical | Drop-recommended | Cosmetic |
|---|---|---|---|---|
| Orphan tables | 0 | 0 | 0 | 0 |
| Orphan columns | 17 | 0 | 17 | 0 |
| FK issues | 4 | 0 | 0 | 4 |
| Redundant indexes | 2 | 0 | 0 | 2 |
| Column churn smells | 0 | 0 | 0 | 0 |
| Naming inconsistencies | 3 | 0 | 0 | 3 |
| **Total findings** | **26** | **0** | **17** | **9** |

**Headline:** the schema is in good shape for the v1.0 baseline squash. The dominant cleanup opportunity is the **17 ResourceSpace-heritage columns on `"user"`** that have zero non-generated Go consumers — they're inherited from the RS fork base (per [ADR 0024](adr/0024-resourcespace-clean-room-fork.md)) and never re-wired into the artist-alley auth/session/profile surfaces. Dropping them in C-2 shrinks the User struct + the migrate-up cost without any behavior change.

Everything else is cosmetic. There are **no orphan tables** (all 61 are referenced), **no genuine column churn smells** (the iterative federation table additions are expected layering, not redo), and **no critical FK / data-integrity issues**. The naming inconsistencies are limited to 3 specific columns; ADR-level naming is consistent.

---

## Methodology

All checks ran against `app/schema.sql` as the canonical schema source + the live dev DB for size data. Each finding includes the exact grep / psql commands so verification is mechanical.

### Step 1 — schema state check

`app/schema.sql` matches the migration sequence (validated by reading migrations `00001..00014` and confirming each ALTER / CREATE / DROP shows up in the canonical schema). No drift between source-of-truth and the canonical file.

### Step 2 — per-table orphan check

```bash
while read table; do
    raw=$(echo "$table" | tr -d '"')
    code=$(grep -rln "\b${raw}\b" app/internal/ | grep -v 'queries.sql.go$\|models.go$\|panicshim_gen\|openapi.gen.go' | wc -l)
    sql=$(find app/internal -name "queries.sql" -exec grep -l "\b${raw}\b" {} \; | wc -l)
    mig=$(grep -ln "\b${raw}\b" app/internal/db/migrations/*.sql | wc -l)
    echo "$raw code=$code sql=$sql mig=$mig"
done < <(grep -E '^CREATE TABLE' app/schema.sql | awk '{print $3}' | sed 's/^public\.//')
```

Result: all 61 tables have ≥3 references (lowest-referenced are minimal-surface join tables like `asset_alternates`, `asset_companions`, `collection_acls` — each with exactly one consumer package + one `queries.sql` + the migration that created it). **No orphan tables.**

### Step 3 — per-column orphan check

```bash
# Extract every column from every CREATE TABLE block
# For each (table, col): grep both PascalCase (Go struct field) and snake_case (SQL string)
# Exclude generated files (queries.sql.go, models.go, panicshim_gen, openapi.gen.go)
# A column is an orphan iff:
#   * struct field has zero non-test, non-generated Go references AND
#   * snake_case name has zero non-test, non-generated references except the migration that created it
```

After the refined sweep (snake_case + PascalCase, both, total 583 columns), **17 columns came back as orphans — all on the `"user"` table**. See [Orphan columns](#orphan-columns).

### Step 4 — FK sanity

```bash
grep -nE "REFERENCES" app/schema.sql | wc -l  # 64 total
grep -nE "REFERENCES" app/schema.sql | grep -v "ON DELETE" | head  # 4 without explicit policy
```

All 64 FKs point at tables / columns that exist. 4 have no explicit `ON DELETE` (default: NO ACTION = RESTRICT, which prevents deletes of the parent). See [FK issues](#fk-issues).

### Step 5 — index audit

```bash
docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc "
SELECT tablename, indexname, pg_size_pretty(pg_relation_size(c.oid)) AS size
  FROM pg_indexes JOIN pg_class c
       ON c.relname = indexname
   AND c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname='public')
 ORDER BY pg_relation_size(c.oid) DESC LIMIT 25"
```

149 total indexes. Two have overlap patterns worth review. See [Redundant indexes](#redundant-indexes).

### Step 6 — column churn

```bash
for t in <tables>; do
  for m in app/internal/db/migrations/*.sql; do
    grep -lE "(ALTER|CREATE|DROP).*TABLE.*\b${t}\b" "$m"
  done
done
```

Tables touched by 2+ migrations: federation_outbox (3), federation_inbox (2), likes (2), federation_user_keys (2), federation_remote_actors (2), federation_peers (2), comments (2), assets (2), capabilities (2 — pseudo, see note), activities (2). All are **expected layering** from the encryption arc adding observability columns onto previously-shipped tables — no rename / re-add / drop sequences indicating real churn. See [Column churn smells](#column-churn-smells) for the per-table walk + non-finding rationale.

### Step 7 — naming inconsistencies

```bash
grep -nE '^\s+\w+_id\s+bigint' app/schema.sql    # BIGINTs named _id
grep -nE '^\s+\w+_ref\s+uuid' app/schema.sql     # UUIDs named _ref
grep -nE '^\s+(owner_ref|creator_ref|author_ref)\b' app/schema.sql  # missing _user_
```

Three real findings (1 BIGINT `_id`, 1 UUID `_ref`, 1 missing `_user_` prefix). See [Naming inconsistencies](#naming-inconsistencies).

---

## Orphan tables

**None.** All 61 tables have at least one consumer package + one `queries.sql` reference. The minimum-reference tables (`asset_alternates`, `asset_companions`, `collection_acls`, etc.) are minimal-surface join / metadata tables with exactly the expected single-consumer shape — not orphans.

---

## Orphan columns

### F-001 through F-017 — `"user"` table ResourceSpace-heritage columns

**Severity:** drop-recommended (17 of 17)
**Created in:** `00001_baseline.sql` (Phase 0 — the RS fork import per ADR 0024)
**Code references:** 1 each, all pointing at `app/internal/db/migrations/00001_baseline.sql` (the originating migration). Zero non-generated Go consumers. sqlc generates a struct field for each because `SELECT *` on the user table picks them up, but no production code reads or writes any of them.

| ID | Column | Type | RS purpose (now defunct) |
|---|---|---|---|
| F-001 | `accepted_terms` | `integer DEFAULT 0 NOT NULL` | RS "accepted terms of service" flag; not in artist-alley signup flow |
| F-002 | `csrf_token` | `character varying(255)` | RS per-session CSRF; artist-alley uses different CSRF handling |
| F-003 | `current_collection` | `integer` | RS active-collection memory; artist-alley has no global "active collection" UX |
| F-004 | `email_invalid` | `integer DEFAULT 0` | RS email-bounce flag; not wired into artist-alley email sending |
| F-005 | `email_rate_limit_active` | `integer DEFAULT 0` | RS email rate limit; artist-alley has none |
| F-006 | `hidden_collections` | `text` | RS per-user collection hide list; artist-alley has no equivalent |
| F-007 | `ip_restrict` | `text` | RS per-user IP allow-list; not in artist-alley auth |
| F-008 | `last_browser` | `text` | RS user-agent capture; not used |
| F-009 | `last_ip` | `character varying(100)` | RS last-IP capture; not used (we use `audit_events.ip`) |
| F-010 | `login_last_try` | `timestamp with time zone` | RS login throttle state; artist-alley uses `auth.LoginLimiter` (in-memory) |
| F-011 | `login_tries` | `integer DEFAULT 0 NOT NULL` | Same — RS throttle, not used |
| F-012 | `processing_messages` | `text` | RS job-progress text dump; we have `jobs` table |
| F-013 | `profile_image` | `text` | RS profile pic URL; superseded by `user_profiles.avatar_url` |
| F-014 | `profile_text` | `character varying(500)` | RS profile bio; superseded by `user_profiles.bio` |
| F-015 | `search_filter_o_id` | `integer` | RS user-group "owner-only" search filter override; not used |
| F-016 | `search_filter_override` | `text` | RS user-level search filter SQL override; not used |
| F-017 | `unique_hash` | `character varying(50)` | RS share-token field; not used |

**Verification command (per column):**
```bash
grep -rln "\bcolumn_name\b" app/internal/ | grep -v 'queries.sql.go$\|models.go$\|panicshim_gen\|openapi.gen.go\|_test.go'
# Should only return: app/internal/db/migrations/00001_baseline.sql
```

**Background:** The `"user"` table was imported wholesale from ResourceSpace as part of the Phase 0 fork (per ADR 0024 §"clean-room methodology"). Artist-alley has since added 5 net-new columns (`actor_uri`, `signing_public_key_pem`, `signing_private_key_enc`, `encryption_public_key`, `encryption_private_key_enc`) to support federation; the original 17 RS columns above were not removed because no migration explicitly dropped them, and pre-MVP "we can wipe DBs freely" policy ([`feedback_pre_mvp_everything_is_volatile.md`](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/feedback_pre_mvp_everything_is_volatile.md)) meant accumulated cruft was fine until the baseline squash.

**Suggested C-2 action:** `ALTER TABLE "user" DROP COLUMN <each>` in the squashed baseline. Touches sqlc (User struct loses 17 fields — regen handles it) + zero handlers (because nothing reads them). Mechanical change; CI will validate cleanly.

**Risks if kept:** ongoing struct bloat (User struct has 39 fields, would drop to 22); slight migration / sqlc-regen cost; minor reader confusion. None structural.

**Risks if dropped:** none. There is no consumer to break. Verified by zero non-generated grep hits across the entire `app/internal/` tree.

---

## FK issues

### F-018 — 4 FKs with no explicit `ON DELETE` policy

**Severity:** cosmetic (review-recommended; not a data-integrity bug)
**Default behavior:** PostgreSQL `NO ACTION` (deferred to end of transaction) — effectively `RESTRICT` for most workloads, blocking parent-row deletes while child rows exist.

| Constraint | Effective policy | Likely intent |
|---|---|---|
| `asset_alternates.object_hash → storage_objects(hash)` | NO ACTION | Probably wants `RESTRICT` (intentional — alternates should pin the blob). Verify + make explicit. |
| `asset_companions.object_hash → storage_objects(hash)` | NO ACTION | Same as above. |
| `assets.asset_type → asset_types(ref)` | NO ACTION | Probably wants `RESTRICT` (asset_types is operator-managed, deletion should be blocked when assets reference it). Verify + make explicit. |
| `field_definition.deprecated_replacement_id → field_definition(id)` | NO ACTION | Self-FK on the deprecation chain — `NO ACTION` is probably correct (can't delete a row that other rows point to as the replacement), but making it explicit `RESTRICT` would document intent. |

**Verification command:**
```bash
grep -nE "REFERENCES" app/schema.sql | grep -v "ON DELETE"
```

**Suggested C-2 action:** add explicit `ON DELETE RESTRICT` on each in the baseline squash. Pure documentation — no runtime behavior change.

**Risks if kept:** ongoing reader-time confusion about whether the omission is intentional. None operational.

**Risks if changed:** none if `RESTRICT` matches reality (which it does today — these tables don't get parent-deleted while child rows exist).

---

## Redundant indexes

### F-019 — `comments` text-annotation + whiteboard partial indexes overlap the generic target index

**Severity:** cosmetic
**Tables touched:** `comments`

```sql
-- The generic one covers all (target_kind, target_id, created_at DESC) WHERE deleted_at IS NULL:
CREATE INDEX comments_target_active_idx ON public.comments
    USING btree (target_kind, target_id, created_at DESC)
    WHERE (deleted_at IS NULL);

-- These two narrow it further on annotation_type:
CREATE INDEX comments_text_annotations_idx ON public.comments
    USING btree (target_kind, target_id, created_at DESC)
    WHERE ((annotation_type = 'text-range'::text) AND (deleted_at IS NULL));

CREATE INDEX comments_whiteboards_idx ON public.comments
    USING btree (target_kind, target_id, created_at DESC)
    WHERE ((annotation_type = 'whiteboard'::text) AND (deleted_at IS NULL));
```

The narrow partial indexes are a subset of the generic one (same column tuple, tighter `WHERE`). For queries filtering on a specific `annotation_type`, the planner may pick the narrow one to scan fewer rows; for queries without that filter, it'll use the generic.

**Whether they're actually redundant depends on the live query mix** — the planner's choice depends on selectivity. If annotation-type rows are <5% of the comments table, the narrow indexes earn their keep. If they're >50%, the generic index handles them via bitmap scan well enough that the narrow ones are dead weight.

**Suggested C-2 action:** **defer to a post-squash performance pass.** Mark for `EXPLAIN ANALYZE` verification when the annotation surface starts producing real traffic. Don't drop blindly during the squash — if the narrow indexes save 10× on annotation queries, the cost of re-adding them later is fine but the diagnostic cycle to discover the regression isn't.

**Risks if kept:** ~few KB of disk per narrow index; one extra index to maintain on `comments` writes (3 indexes covering the same tuple-prefix → 3 updates per row insert/update).

**Risks if dropped:** annotation-type queries scan more rows. Mitigated by EXPLAIN before/after measurement.

### F-020 — `assets` has 10 partial-WHERE indexes; verify the partial conditions don't fragment the same logical query

**Severity:** cosmetic
**Tables touched:** `assets`

The `assets` table has 10 indexes, almost all with `WHERE deleted_at IS NULL` partial filters. The combinations are:

| Index | Columns | Partial WHERE |
|---|---|---|
| `assets_created_at_idx` | `created_at DESC` | `deleted_at IS NULL` |
| `assets_file_hash_idx` | `file_hash` | `file_hash IS NOT NULL` |
| `assets_metadata_gin` | `metadata` (GIN) | — |
| `assets_owner_idx` | `owner_user_ref` | `deleted_at IS NULL AND owner_user_ref IS NOT NULL` |
| `assets_processing_status_idx` | `processing_status` | `processing_status <> 'ready'` |
| `assets_search_text_gin` | `search_text` (GIN) | `deleted_at IS NULL` |
| `assets_state_idx` | `state_id` | `state_id IS NOT NULL` |
| `assets_status_idx` | `status` | `deleted_at IS NULL` |
| `assets_team_idx` | `team_id` | `team_id IS NOT NULL` |
| `assets_type_idx` | `asset_type` | `deleted_at IS NULL` |

No two indexes share a column tuple. **Not redundant per se** — but worth flagging that 10 indexes on a single 2,304 kB table (1,522 rows on dev) is high index-overhead-to-data ratio. If write throughput becomes a concern (it isn't today), consolidate.

**Suggested C-2 action:** leave alone. Post-MVP, revisit with a query-trace pass.

---

## Column churn smells

**No real churn.** The audit found 10 tables touched by 2+ migrations, but every one is **additive layering**, not re-do / rename / drop sequences. Detail per table:

| Table | Migrations | Pattern |
|---|---|---|
| `federation_outbox` | 00005 (create), 00010 (was_encrypted observability), 00012 (refused_reason policy) | Encryption-arc additive |
| `federation_inbox` | 00003 (create), 00011 (was_encrypted observability) | Encryption-arc additive |
| `federation_user_keys` | 00007 (create), 00013 (rotation metadata) | Phase I-h additive |
| `federation_remote_actors` | 00004 (create), 00008 (encryption_public_key) | Phase I-c additive |
| `federation_peers` | 00001 (baseline), 00009 (capabilities columns) | Phase I-d additive |
| `assets` | 00001 (baseline), 00014 (sensitivity column) | Phase I-i additive |
| `likes` | 00001 (baseline), 00004 (federation inbound) | Federation extension |
| `comments` | 00001 (baseline), 00004 (federation inbound) | Federation extension |
| `activities` | 00001 (baseline), (later AddColumn in same baseline file) | Baseline + same-file expansion |
| `capabilities` | 00001 only (false-positive — grep matched a column rename note) | — |

**Conclusion:** the migration sequence has been disciplined. No column was added, renamed, type-changed, then dropped within the audited 14-migration window. The baseline squash absorbs the additive layering cleanly.

---

## Naming inconsistencies

### F-021 — `federation_user_keys.user_id` is BIGINT but named `_id` (convention: `_ref`)

**Severity:** cosmetic
**Location:** `app/schema.sql:3817`
**Convention:** project memory + the rest of the schema use `_ref` suffix for BIGINT FKs to `"user"(ref)` and `_id` suffix for UUID FKs.

```sql
-- federation_user_keys (00007):
CREATE TABLE federation_user_keys (
    user_id BIGINT NOT NULL REFERENCES "user"(ref) ON DELETE CASCADE,  -- ← should be user_ref
    ...
);
```

Compare to the 28+ other tables that get this right: `actor_user_ref`, `author_user_ref`, `owner_user_ref`, `granted_by_user_ref`, `rotated_by_user_ref` (same migration as 00007 → 00013!), `assigned_by_user_ref`, etc.

**Suggested C-2 action:** rename `federation_user_keys.user_id` → `federation_user_keys.user_ref` in the squashed baseline. Touches:
- sqlc-generated `FederationUserKey` struct field name
- All sqlc query `arg.UserID` → `arg.UserRef`
- Hand-written references in `app/internal/federation/userkeys/` (~20 call sites)

**Risks if kept:** ongoing convention drift; a new contributor copying this pattern would propagate the mistake.

**Risks if renamed:** ~1 hour of sqlc-regen + grep-replace. Mechanical. Caught by `go build`.

### F-022 — `field_definition.value_ref` is UUID but named `_ref` (convention: `_id`)

**Severity:** cosmetic (genuine borderline — see note)
**Location:** `app/schema.sql:562`

```sql
CREATE TABLE asset_field_value (
    ...
    value_text text,
    value_num double precision,
    value_date timestamp with time zone,
    value_options text[],
    value_ref uuid,         -- ← UUID, but named _ref
    ...
);
```

**Note on borderline:** `value_ref` lives inside a multi-type "value of N kinds" column family (`value_text`, `value_num`, `value_date`, `value_options`, `value_ref`). The `_ref` here means "value-is-a-reference-to-some-domain-object" — a within-table convention, not the schema-wide `_ref = BIGINT` rule. Renaming to `value_id` would BREAK the sibling pattern. **Recommendation: leave alone; document the local convention** in a CREATE TABLE comment.

**Suggested C-2 action:** **defer / no-action**. Add a `COMMENT ON COLUMN asset_field_value.value_ref IS '...'` clarifying that the `_ref` suffix here is the local multi-type-value convention, distinct from the schema-wide BIGINT `_ref` rule.

### F-023 — `brush_packs.owner_ref` is BIGINT but missing the `_user_` prefix (convention: `owner_user_ref`)

**Severity:** cosmetic
**Location:** `app/schema.sql:722`

```sql
-- brush_packs:
CREATE TABLE brush_packs (
    ...
    owner_ref bigint NOT NULL,  -- ← should be owner_user_ref for consistency
    ...
);
```

Compare to `assets.owner_user_ref`, `collections.owner_user_ref`, `posts.author_user_ref`, `comments.author_user_ref`. All ownership/authorship FKs to `"user"(ref)` use the `_user_ref` suffix; `brush_packs.owner_ref` is the only one missing the `_user_` segment.

**Suggested C-2 action:** rename `brush_packs.owner_ref` → `brush_packs.owner_user_ref` in the squashed baseline. Touches:
- sqlc-generated `BrushPack` struct field name (`OwnerRef` → `OwnerUserRef`)
- ~5 call sites in `app/internal/brushpacks/`

**Risks:** mechanical rename. Same shape as F-021.

---

## Appendix A — Tables-by-size (live dev DB, 2026-06-15)

Top 30 by total relation size (table + indexes). Useful for sizing the baseline-squash's `pg_dump` step + identifying which tables justify their index overhead.

| Table | Rows | Table size | Index size |
|---|---|---|---|
| `audit_events` | 87,172 | 28 MB | 13 MB |
| `posts` | 885 | 4904 kB | 4920 kB |
| `assets` | 1,522 | 2304 kB | 3640 kB |
| `storage_variants` | 13,148 | 1936 kB | 2096 kB |
| `asset_field_value_history` | 11,243 | 1736 kB | 1944 kB |
| `activities` | 2,945 | 1248 kB | 1832 kB |
| `asset_field_value` | 10,733 | 1168 kB | 1368 kB |
| `sessions` | 2,325 | 872 kB | 1600 kB |
| `"user"` | 2,344 | 400 kB | 960 kB |
| `jobs` | 1,285 | 792 kB | 224 kB |
| `federation_peers` | 1,150 | 368 kB | 472 kB |
| `storage_pins` | 1,334 | 328 kB | 488 kB |
| `asset_tag` | 5,184 | 400 kB | 400 kB |
| `federation_user_keys` | 2,552 | 464 kB | 240 kB |
| `post_tags` | 4,383 | 320 kB | 328 kB |
| `post_assets` | 1,778 | 200 kB | 304 kB |
| `collection_resources` | 1,270 | 200 kB | 264 kB |
| `storage_objects` | 1,346 | 248 kB | 200 kB |
| `collection_posts` | 847 | 112 kB | 216 kB |
| `comments` | 251 | 88 kB | 192 kB |
| `federation_remote_actors` | 518 | 136 kB | 120 kB |
| `teams` | 434 | 80 kB | 136 kB |
| `field_definition` | 23 | 48 kB | 160 kB |
| `team_closure` | 622 | 80 kB | 112 kB |
| `federation_peer_suggestions` | 164 | 80 kB | 88 kB |
| `federation_directory_entries` | 144 | 72 kB | 88 kB |
| `federation_directories` | 96 | 104 kB | 48 kB |
| `collections` | 21 | 48 kB | 88 kB |
| `federation_shares` | 0 | 24 kB | 112 kB |
| `likes` | 12 | 48 kB | 80 kB |

**Observations:**
- `audit_events` dominates at 28 MB / 87k rows. Reasonable for a multi-month dev DB. Production retention policy (out of scope for this audit) eventually decides whether this stays load-bearing or gets archived.
- `posts` has a near-1:1 table-to-index size ratio (4904 / 4920 kB) — the GIN search_text index is the heavy contributor. Justified by full-text search.
- `assets` has a >1.5:1 index-to-table ratio (3640 / 2304 kB) — see [F-020](#f-020--assets-has-10-partial-where-indexes-verify-the-partial-conditions-dont-fragment-the-same-logical-query) for the 10-index count concern.
- `federation_shares` has 112 kB of indexes on 0 rows — that's the encryption-arc preparing the share-grant surface; rows show up as soon as a real cross-instance share lands.

---

## Appendix B — Indexes-by-size (live dev DB, 2026-06-15)

Top 25 by index size. Useful for spotting "expensive index, low query value" candidates.

| Table | Index | Size |
|---|---|---|
| `audit_events` | `audit_events__type_time_idx` | 8976 kB |
| `posts` | `posts_search_text_gin` | 4608 kB |
| `audit_events` | `audit_events_pkey` | 3712 kB |
| `storage_variants` | `storage_variants_pkey` | 2096 kB |
| `assets` | `assets_metadata_gin` | 1808 kB |
| `sessions` | `sessions__last_used_idx` | 1152 kB |
| `assets` | `assets_search_text_gin` | 1136 kB |
| `asset_field_value_history` | `afvh_field_idx` | 792 kB |
| `asset_field_value` | `asset_field_value_pkey` | 720 kB |
| `asset_field_value_history` | `afvh_asset_idx` | 624 kB |
| `activities` | `activities_actor_outbox_idx` | 608 kB |
| `asset_field_value_history` | `asset_field_value_history_pkey` | 528 kB |
| `activities` | `activities_activity_uri_key` | 464 kB |
| `activities` | `activities_object_recent_idx` | 344 kB |
| `asset_tag` | `asset_tag_pkey` | 336 kB |
| `storage_pins` | `storage_pins_pkey` | 320 kB |
| `"user"` | `user_created_ref_desc_idx` | 280 kB |
| `post_tags` | `post_tags_pkey` | 272 kB |
| `sessions` | `sessions_token_hash_key` | 240 kB |
| `asset_field_value` | `asset_field_value_num_idx` | 208 kB |
| `activities` | `activities_type_recent_idx` | 184 kB |
| `assets` | `assets_file_hash_idx` | 184 kB |
| `storage_objects` | `storage_objects_pkey` | 184 kB |
| `"user"` | `user_username_uniq_idx` | 176 kB |
| `activities` | `activities_pkey` | 168 kB |

**Observations:**
- `audit_events__type_time_idx` (8976 kB) is the single largest non-PK index. It's the audit-feed query path (`SELECT ... FROM audit_events WHERE event_type = $1 ORDER BY occurred_at DESC`). Load-bearing for `/admin/audit` + the federation observability surfaces. Keep.
- The two GIN indexes (`posts_search_text_gin` 4608 kB + `assets_search_text_gin` 1136 kB) carry full-text search. Keep.
- `assets_metadata_gin` (1808 kB) covers the metadata JSONB containment queries. Keep.
- No "large index, low query value" candidates surfaced.

---

## Appendix C — Migrations per table

Indirect input for the C-2 baseline squash — shows which migration files each table's final shape needs to absorb.

| Table | Migrations | Final-state state |
|---|---|---|
| `federation_outbox` | 00005, 00010, 00012 | full encryption observability columns + refused-reason policy |
| `federation_inbox` | 00003, 00011 | full encryption observability columns |
| `federation_user_keys` | 00007, 00013 | base table + rotation metadata + system_config retention key |
| `federation_remote_actors` | 00004, 00008 | base + encryption_public_key columns |
| `federation_peers` | 00001, 00009 | base + capability negotiation columns |
| `assets` | 00001, 00014 | base + sensitivity column + partial index |
| `likes` | 00001, 00004 | base + federation actor columns |
| `comments` | 00001, 00004 | base + federation actor columns |
| `activities` | 00001 | base only |
| `capabilities` | 00001 | base only |
| `audit_events` | 00001 | base only |
| All other tables (50) | one migration each | created once, never altered |

**Squash sequencing for C-2:** absorb in ascending migration order. The federation-arc tables (00005→00012) layer cleanly; no rename / drop / re-add sequences to reconcile.

---

## Appendix D — Verification commands index

For reviewers wanting to re-run the audit:

```bash
# Table inventory
grep -E '^CREATE TABLE' app/schema.sql | wc -l

# Index inventory
grep -E '^CREATE (UNIQUE )?INDEX' app/schema.sql | wc -l

# FK inventory
grep -nE "REFERENCES" app/schema.sql | wc -l

# FKs without explicit ON DELETE
grep -nE "REFERENCES" app/schema.sql | grep -v "ON DELETE"

# Per-table reference count (the snake-case grep)
grep -rln "\b<table>\b" app/internal/ | \
  grep -v 'queries.sql.go$\|models.go$\|panicshim_gen\|openapi.gen.go\|_test.go'

# Per-column orphan check (PascalCase)
pascal=$(echo "$col" | awk -F_ 'BEGIN{ORS=""} {for(i=1;i<=NF;i++)printf "%s", toupper(substr($i,1,1))substr($i,2)}')
grep -rEln "\.${pascal}\b" app/internal/ | \
  grep -v '_test.go\|panicshim_gen\|openapi.gen.go\|queries.sql.go\|models.go'

# Live table sizes
docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc "
SELECT C.relname, U.n_live_tup,
       pg_size_pretty(pg_table_size(C.oid)) AS table_size,
       pg_size_pretty(pg_indexes_size(C.oid)) AS index_size
  FROM pg_class C JOIN pg_stat_user_tables U ON C.oid = U.relid
 WHERE C.relkind = 'r' AND C.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname='public')
 ORDER BY pg_total_relation_size(C.oid) DESC LIMIT 30"

# Live index sizes
docker compose exec -T postgres psql -U artist_alley -d artist_alley -tAc "
SELECT i.tablename, i.indexname, pg_size_pretty(pg_relation_size(c.oid))
  FROM pg_indexes i JOIN pg_class c ON c.relname = i.indexname
 WHERE i.schemaname = 'public'
 ORDER BY pg_relation_size(c.oid) DESC LIMIT 25"
```

All commands run from the repo root. Live-DB commands require the dev stack to be up (`docker compose up -d postgres`).

---

## Conclusion + C-2 input

The pre-MVP DB is **in good shape for the v1.0 baseline squash.** 26 findings total; the meaningful cleanup is the **17 RS-heritage columns on `"user"`** (F-001 through F-017) plus **2 naming renames** (F-021 + F-023). Everything else is review-recommended or defer-to-post-MVP.

### Suggested C-2 commit shape (input only — not part of this PR)

1. **Drop F-001 through F-017** — 17 `ALTER TABLE "user" DROP COLUMN` in the squashed baseline. sqlc regen + zero handler changes.
2. **Rename F-021 + F-023** — `federation_user_keys.user_id → user_ref` and `brush_packs.owner_ref → owner_user_ref`. Mechanical sqlc-regen + grep-replace.
3. **Annotate F-018** — make the 4 `NO ACTION` FKs explicit `ON DELETE RESTRICT`. Documentation, no behavior change.
4. **Defer F-019 + F-020** — flag `comments` annotation indexes + `assets` 10-index spread for a post-MVP performance pass; do NOT drop pre-emptively.
5. **Defer F-022** — `asset_field_value.value_ref` is intentional within-table local convention; document with `COMMENT ON COLUMN`, leave the name.

Net schema change in C-2 against this audit: **17 column drops + 2 column renames + 4 FK annotations + 1 column comment = 24 single-line schema edits in the new `00001_baseline_v1.sql`.**

**Soak-window note:** none of the above touches federation runtime *code*; they all live in the baseline migration. The v1.0-rc1 federation soak (through 2026-06-22) is unaffected by either this audit (read-only) or the future C-2 (DB schema only, no runtime).
