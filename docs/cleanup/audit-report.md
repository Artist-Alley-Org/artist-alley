# DB schema audit — pre-flatten

**Status:** AUDIT-ONLY. No schema changes in this commit. Findings drive the
destructive flatten that follows (Workstream 3 commit 4, per the
chore/pre-mvp-cleanup plan + ADR 0046).

**Method:** every `CREATE TABLE x` in `app/schema.sql` cross-referenced against
the Go reference set (`app/internal/**/*.go`, `app/internal/**/queries.sql`,
`app/cmd/**/*.go`). sqlc-generated `queries.sql.go` files count as canonical
readers. Migrations under `app/internal/db/migrations/` consulted for churn.

Authored by an Explore-agent pass; recommendations layered in for the flatten
commit author.

---

## Bucket 1 — Orphan tables

**None.** All 56 tables in `schema.sql` are referenced by either a domain
queries.sql with live Go callers, a federation handler, or a support table
(sysconfig, capabilities, roles) with live access paths.

One ambiguous case worth confirming during flatten:

- **`likes`** has six references in
  `app/internal/social/queries.sql` (LikeTarget, UnlikeTarget,
  HasUserLikedTarget, ListLikersOfTarget, plus triggers/test fixtures) but
  no FK constraints. It's polymorphic by design (`target_kind` +
  `target_id`); not an orphan.

**Flatten action:** keep every table as-is in the baseline.

---

## Bucket 2 — Orphan columns

The `assets` table carries **15 unused columns** — all legacy RS holdovers
that no current sqlc query selects and no Go field-access path touches.

| Column | Migration source | Notes |
|---|---|---|
| `rating` | RS-derived | unused; no UI surface |
| `user_rating` | RS-derived | unused; no UI surface |
| `hit_count` | RS-derived | analytics column; never written |
| `new_hit_count` | RS-derived | rolling-window analytics; never written |
| `request_count` | RS-derived | RS-only feature (request workflow); not ported |
| `archive_state` | RS-derived | RS lifecycle state machine; we use `deleted_at` instead |
| `thumb_width` | RS-derived | pre-thumbnail-as-asset; superseded by `cover_thumbnail_asset_id` shape |
| `thumb_height` | RS-derived | same as above |
| `image_red` | RS-derived | dominant-color analytics; never computed |
| `image_green` | RS-derived | same as above |
| `image_blue` | RS-derived | same as above |
| `colour_key` | RS-derived | dominant-color-as-string; never computed |
| `geo_lat` | RS-derived | geo metadata; never set |
| `geo_long` | RS-derived | geo metadata; never set |
| `country` | RS-derived | geo metadata; never set |

Confirmed: `assets/queries.sql` SELECTs omit all 15. Production code never
references these columns by name.

Other large tables (posts, comments, field_definition, notifications,
collections, users, user_profiles): no orphan columns found — every column
shows live read or write traffic in Go.

**Flatten action:** drop all 15 from the baseline. Mention in the
baseline header that the drops correspond to the RS-import scaffolding
that was never wired through the porting plan.

---

## Bucket 3 — FK isolation

Six tables have no inbound or outbound FKs. All legitimate:

| Table | Why FK-isolated |
|---|---|
| `likes` | Polymorphic `(target_kind, target_id)` — by design |
| `user_profiles` | Denormalized; `rs_user_id` is PRIMARY KEY (1:1 with user) |
| `user_preferences` | Same shape as user_profiles |
| `user_follows` | Social graph; follower/followee are BIGINTs that don't FK so a defederation cascade doesn't drop history |
| `user_blocks` | Same reasoning as user_follows |
| `direct_messages` | Sender/recipient BIGINTs; survive user deletion for audit |

**Flatten action:** keep as-is. Add an inline comment per table in the
baseline explaining why each one is intentionally FK-isolated.

---

## Bucket 4 — Naming inconsistencies (`_id UUID` vs `_ref BIGINT`)

Project-wide split:

- `*_id` (UUID): **167 occurrences**
- `*_ref` (BIGINT): **49 occurrences**

Ratio 3.4:1 favoring UUID. The `_ref` pattern is concentrated on the legacy
RS-derived user FK paths (where the user table's PK is `ref BIGINT`).

Worst offenders:

| Pattern | Sites | Notes |
|---|---|---|
| `origin_server_id` | 20 tables | Federation FK (UUID). Consistent. |
| `rs_user_id` | 9 tables | Legacy RS naming. Coexists with `owner_user_ref`/`author_user_ref`/`sender_user_ref` in newer tables. **Worst offender.** |
| `asset_id` | 7 tables | UUID; consistent. |
| `team_id` | 6 tables | UUID; consistent. |

**Flatten decision needed:** keep both patterns (UUID for newly-minted IDs,
BIGINT-`_ref` for the user table) OR converge on one. Recommendation: keep
the existing `user_table.ref BIGINT` PK (changing it would cascade through
every audit log + every session row), but **drop every `rs_`-prefixed
identifier across schema + Go**. The prefix is dead weight — it reflected
RS source-compat back when we were going to share queries with the PHP
side, but the strangler-fig was abandoned (ADR 0003/0015 → historical).
No remaining code needs RS-compatible column names.

### Full `rs_*` inventory (schema-side)

| Identifier | Occurrences | Sites |
|---|---|---|
| `rs_user_id` | 15 | user_password_history, api_tokens, team_memberships, user_roles, user_capability_grants, user_capability_revokes, user_profiles, user_preferences, likes, plus the constraint names |
| `granted_by_rs_user_id` | 4 | capability grants, capability_grants, scope_grants (and similar) |
| `revoked_by_rs_user_id` | 1 | user_capability_revokes |
| `added_by_rs_user_id` | 1 | team_memberships |
| `assigned_by_rs_user_id` | 1 | user_roles |
| `actor_rs_user_id` | 1 | (workflow audit table around line 576) |

### `rs_*` inventory (Go-side, non-schema)

| Identifier | Sites |
|---|---|
| `rs_session` | auth/handler.go + middleware.go + session.go + queries.sql — the session cookie name + the user-table column. Renaming the cookie invalidates active sessions (pre-MVP, fine). |
| `rs_setcookie` | auth/handler.go + session.go — helper function name. |
| `rs_password_hash` | auth/password.go — column on user table (legacy RS PHP hash format). |

**Flatten action:** drop the `rs_` prefix from every identifier above.
Specifically:

- Schema column renames (mostly mechanical sed in the baseline file):
  - `rs_user_id`         → `user_ref`
  - `granted_by_rs_user_id` → `granted_by_user_ref`
  - `revoked_by_rs_user_id` → `revoked_by_user_ref`
  - `added_by_rs_user_id`   → `added_by_user_ref`
  - `assigned_by_rs_user_id` → `assigned_by_user_ref`
  - `actor_rs_user_id`      → `actor_user_ref`
  - `rs_session`            → `session_token` (on the user table)
  - `rs_password_hash`      → `password_hash`
- Constraint name updates wherever they embed the column name.
- Go side: rename `rs_session` cookie + helper to `aa_session` + `setSessionCookie`; rename the `rs_password_hash` column reference. The sqlc regen does the field renames automatically (CamelCased from the new column names).
- One-line documented in the baseline header: "session cookie renamed to aa_session in the baseline; existing dev sessions invalidate on first boot."

Pre-MVP so no migration ladder needed.

---

## Bucket 5 — Churn smell

Columns appearing in 3+ migration files (excluding routine `created_at` /
`updated_at` / `deleted_at` boilerplate):

| Column | Migration count | Pattern |
|---|---|---|
| `metadata` (JSONB) | 15 migrations | Additive — new feature added a metadata column on its domain table. No renames or drops. |
| `origin_server_id` | 20 migrations | Additive — federation groundwork sprinkled across all federated tables. |
| `asset_type` | 6 migrations | All seeding/reclassification work; no schema churn after 00028. |
| `display_name` | 5 migrations | Additive across user_profiles + federation_peers + federation_directories. |

**No mid-sequence rename or drop churn.** Every change has been additive,
which is the cheapest case for the flatten — the baseline is just the
union of all the additions.

**Flatten action:** no special handling needed — the baseline naturally
absorbs additive history.

---

## Summary — what changes at flatten

1. **Drop 15 columns** from `assets` (Bucket 2 RS-legacy list).
2. **Rename `rs_user_id` → `user_ref`** in 8 tables (Bucket 4).
3. **Keep every table** (Bucket 1 — no orphans).
4. **Add inline comments** explaining the 6 FK-isolated tables (Bucket 3).
5. **No mid-sequence drops/renames to model** in the baseline (Bucket 5).

Net effect on the baseline file:
- ~15 lines shorter from the asset-column drops.
- 8 column-name renames (cosmetic).
- Comment additions across the table headers.

Anything not in this list survives the flatten unchanged. The flatten commit
itself produces a single `00001_baseline_v1.sql` via `pg_dump --schema-only`
applied to a fresh DB that's had every migration run, then hand-edited per
this audit.

---

## Open questions for the flatten committer

1. **`asset_type` rename history** — migration 00028 renamed
   `resource_type` → `asset_types`. The baseline only knows the new name;
   anyone still grepping for `resource_type` in old branches will miss.
   Worth a one-line note in the baseline header about the rename lineage.

2. **Federation FK-cascade on user_follows / user_blocks** — these are
   FK-isolated today for a reason (defederation shouldn't drop social
   history). When 1.22.D ships the inbox/outbox worker, the design assumes
   the user_follows row is the source of truth for "this user follows
   that user across the federation." Confirm with the federation owner
   before any future flatten reconsiders the FK choice.

3. **`likes` polymorphism** — kept as-is. If we ever decide to split into
   `asset_likes` / `post_likes` / `comment_likes`, do it in its own phase,
   NOT as part of the baseline. The polymorphic shape is load-bearing for
   the existing UI's "what did this user like" feed query.
