# ADR 0005: Postgres-only, no MySQL

- Date: 2026-05-23
- Status: Accepted
- Supersedes the dual-database stance in ADR 0003 and the "adjunct"
  framing in ADR 0004 (which is now retired as Superseded).

## Context

Phase 0 brought up two databases: MySQL (for forked ResourceSpace) and
Postgres + pgvector (for new-feature data). The original rationale was
that RS is mysqli-only and migrating to Postgres would be a "3-6 month
multi-thousand-call-site rewrite."

Closer inspection contradicted that estimate:

- Direct `mysqli_*` calls are concentrated in **~68 sites across 10 files**,
  most of them in `include/database_functions.php`, `pages/setup.php`, and
  a handful of low-level files.
- Application code routes through high-level helpers (`ps_query`,
  `ps_value`, `sql_insert_id`, etc.), so the *call sites* of those helpers
  are largely portable.
- We have **no real data** yet (RS was installed an hour before this
  decision). No data migration risk.
- Operating two databases (MySQL for RS, Postgres for sidecars) doubles
  backup, monitoring, upgrade, and developer onboarding cost forever.

The user's call: kill MySQL, migrate RS to Postgres now, before any
sidecar code is written and before any production data exists.

## Decision

### Single database: PostgreSQL 16+ with pgvector

- Drop the `mysql` service from `docker-compose.yml`.
- Point ResourceSpace at the existing `postgres` container.
- All future sidecars use the same Postgres instance.

### Database driver: `pdo_pgsql`

Replace `mysqli` with `PDO + pgsql`. PDO is the right migration target
because RS's helpers already use prepared statements with positional or
named parameters — PDO's `prepare/bindValue/execute` model fits cleanly.
The `pdo_pgsql` extension is already baked into our php-fpm image.

We will not introduce a heavy ORM (Doctrine, Eloquent) during the
migration — that's a separate refactor with different goals. Keep the
existing helper function surface (`ps_query`, `ps_value`, etc.), just
re-implement them against PDO+pgsql.

### Schema improvements ("gold standard" calibration)

Where the upstream MySQL schema makes mediocre type choices, we upgrade
to Postgres-native types. **Names stay the same** to minimize app-code
churn; only types and constraints change.

| MySQL upstream | Postgres in artist-alley | Why |
|---|---|---|
| `int(11)` for IDs | `BIGINT` | Future-proof; cost is negligible |
| `tinyint(1)` boolean | `BOOLEAN` | True type, better tooling, plays well with ORM/codegen |
| `datetime` | `TIMESTAMPTZ` | Time-zone aware, UTC-stored, the only sane default in 2026 |
| `text` holding serialized data | `JSONB` (case-by-case) | Indexable, queryable, validatable |
| `varchar(N)` with arbitrary N | keep `VARCHAR(N)` or promote to `TEXT` | `TEXT` is identical in performance in PG; bounded varchar only where intent is clear |
| FULLTEXT indexes | (none initially — see "Full-text search" below) | RS uses `MATCH ... AGAINST`; Postgres uses `tsvector`. Not a 1:1 swap. |

We do **not** retrofit foreign-key constraints onto every RS table during
this migration. RS's code occasionally relies on the absence of FK
enforcement (orphan rows during workflows). FKs come in a follow-up pass,
selectively, after we know which relationships are safe to enforce.

### Schema definition system

ResourceSpace defines its schema in `dbstruct/*.txt` files (CSV format,
loaded by `CheckDBStruct()`). This system is part of the plugin ecosystem
the user wants to preserve: plugins drop their own `dbstruct/` and RS
auto-creates the tables.

**We keep the `dbstruct/` system but rewrite its loader to emit
Postgres DDL** instead of MySQL. The on-disk CSV format stays unchanged,
so existing plugins (and any third-party plugins someone might install
later) "just work."

For **our own new tables** (events outbox, review sessions, comments v2,
annotations, embeddings, user mirror — see ADR 0004's now-retired list),
we use **`goose`** with plain `.sql` files in `infra/db/migrations/`.
Two coexisting systems with clear ownership:

- `dbstruct/` — for RS core tables and RS plugin tables (Postgres-emitting
  loader). Compatible with the upstream plugin ecosystem.
- `infra/db/migrations/` — for artist-alley-specific tables. Versioned,
  reversible, the same migration runner the Go sidecars use.

### Plugin system

Keep the RS plugin system intact and use it where it fits. Sidecars
(Go services in `services/`) handle workloads PHP plugins can't —
streaming, concurrency, ML, non-PHP runtimes.

**Hybrid policy** for new artist-alley capabilities:

- **Build as an RS plugin** when the capability is:
  - PHP-friendly (form handling, simple DB ops, HTML pages)
  - Tightly bound to RS sessions, permissions, or hooks
  - Useful to other RS installations beyond artist-alley
  - Small enough that a separate service would be over-engineered
- **Build as a Go sidecar** when the capability is:
  - Real-time / streaming (SSE review sessions, presence)
  - Compute-heavy (video transcoding, ML inference)
  - Language-native (Go's HTTP stack, Python's ML stack)
  - Provider-agnostic abstraction (AI gateway, embeddings)
  - Operationally distinct (separate scaling, separate deployment cadence)

We will create a single RS plugin named **`artist_alley`** that holds the
PHP side of the bridge: an event-publishing helper (writes to the
Postgres `events` table), auth-header injection for sidecar requests,
and hooks into upload/comment/login. That plugin becomes the canonical
home for PHP-side artist-alley logic; scattered `// ARTIST-ALLEY:`
patches to RS core PHP files are minimized in favor of plugin hooks.

### Full-text search

RS uses MySQL's `MATCH ... AGAINST` syntax with FULLTEXT indexes. Postgres
uses `tsvector`/`tsquery` — different API, different model.

A naive translation (e.g., trigram similarity via `pg_trgm`) would work
for some queries but produce different ranking and missing features.
**We defer the proper full-text rewrite** to a follow-up commit and
accept that search is broken or degraded during the migration window.
A `TODO(ARTIST-ALLEY): full-text-search` marker goes everywhere we leave
a stub. The longer-term goal is a `tsvector`-based search inside RS plus
an optional sidecar that does semantic search (pgvector ANN) — see
ADR 0004's now-retired embeddings table sketch.

## Consequences

**Positive**
- One database to operate, monitor, back up, and reason about.
- Postgres-native types from day one across both RS and our additions.
- pgvector, pg_trgm, JSONB, LISTEN/NOTIFY available everywhere — no
  cross-DB acrobatics.
- The `events` outbox + sidecars pattern (from ADR 0004) becomes
  cheaper since there's no DB boundary inside the app.

**Negative**
- The migration itself is real work — see "Strategy" below.
- We diverge significantly from upstream RS. Cherry-picking future RS
  fixes that touch SQL becomes harder. Acceptable per ADR 0001 (hard
  fork, no upstream tracking).
- Full-text search is degraded until the rewrite lands.
- Plugins that contain raw MySQL DDL or mysqli calls (the simplesaml
  plugin notably bundles vendored Symfony code that uses mysqli) will
  break when enabled. We accept that — enabling such plugins is a
  per-plugin port effort, on demand.

## Strategy (phased, on feature branches)

All work happens on feature branches (per the user's preferred workflow)
that merge into `dev` when each phase passes its smoke test, and `dev`
into `main` when major milestones are stable.

### Phase 0.5.A — Driver swap and connection layer

Branch: `feat/postgres-connection-layer`

1. Drop the `mysql` service from `docker-compose.yml`.
2. Add a `db` service alias to `postgres` so RS's existing `$mysql_server`
   config name doesn't have to change (or alternatively rename `$mysql_*`
   variables in config to `$db_*` for clarity). Decide during
   implementation.
3. Rewrite `include/database_functions.php` to use PDO+pgsql. Keep the
   public helper signatures (`ps_query`, `ps_value`, `sql_insert_id`,
   etc.) unchanged so callers don't move.
4. Re-implement identifier quoting (double-quotes), placeholder syntax
   (`?` works in PDO for both, prefer `?`), and `LASTVAL()` /
   `pg_get_serial_sequence` for auto-increment ID recovery.
5. Update `pages/setup.php` to install against Postgres. Most of the file
   becomes ARTIST-ALLEY-tagged divergence.
6. Smoke test: `./scripts/bootstrap.sh` succeeds, installer reaches the
   "configure base URL" step.

### Phase 0.5.B — Schema emitter

Branch: `feat/postgres-schema-emitter`

1. Rewrite `CheckDBStruct()` (and helpers in `include/database_functions.php`)
   to translate `dbstruct/*.txt` rows into Postgres DDL.
2. Mapping: MySQL types → Postgres-native types per the table above.
3. Add a column-level override mechanism (a small config file or an extra
   column in `dbstruct/*.txt`) so we can opt specific columns into
   `JSONB`, `BOOLEAN`, etc. when the MySQL type was a poor fit.
4. Smoke test: installer creates all 57 core tables on Postgres without
   errors.

### Phase 0.5.C — Query dialect porting

Branch: `feat/postgres-query-dialect` (likely multiple sub-branches)

1. Run the installer and click through the major RS pages. Each broken
   SQL query gets a focused patch.
2. Common patterns:
   - `IFNULL(a, b)` → `COALESCE(a, b)`
   - `GROUP_CONCAT(...)` → `STRING_AGG(...)`
   - `LIMIT a, b` → `LIMIT b OFFSET a`
   - `ON DUPLICATE KEY UPDATE` → `ON CONFLICT (cols) DO UPDATE`
   - Backtick identifiers → double-quoted
   - `NOW()` works in both — leave alone
   - `DATE_FORMAT()` → `TO_CHAR()`
   - `REGEXP` → `~` / `~*`
3. Each fix is a small commit tagged `// ARTIST-ALLEY:` with the upstream
   pattern referenced.

### Phase 0.5.D — Smoke test the major flows

Branch: `feat/postgres-smoke-tests`

1. Walk through: install → log in → upload an asset → browse → comment →
   search (degraded, that's fine for now) → admin pages → user management.
2. Each broken thing is a small commit.
3. End state: RS works on Postgres for the core flows. Search is the
   known exception, awaiting Phase 1.

### Phase 0.5.E (optional) — Goose for new migrations

Branch: `feat/goose-migrations`

1. Add `infra/db/migrations/` with `0001_extensions.sql` (moved from
   `infra/postgres/init.sql`).
2. Add a goose-runner container or make target.
3. No real artist-alley tables yet — that's Phase 1.

## Open questions

None blocking. The questions from ADR 0004 about workspace hierarchy,
user mirror sync, multi-tenancy, auth bridge, and embedding source
storage all resurface when we get to Phase 1, but they don't shape the
Postgres migration itself.

## Notes

ADR 0004 ("Adjunct Postgres data model") is retired in the same commit
that adds this ADR. Its content is partially absorbed here; the rest
(specific table sketches) will return in a new ADR once we're past the
migration and actually designing artist-alley tables.

ADR 0003 ("Strangler Fig internal") had a paragraph asserting that "RS
keeps its MySQL." That paragraph is now incorrect; this ADR supersedes
it on the database question.
