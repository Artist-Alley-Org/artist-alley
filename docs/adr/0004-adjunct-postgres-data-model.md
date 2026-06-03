---
id: "0004"
title: Adjunct Postgres data model
status: superseded
date: 2026-05-23
area: infrastructure
phases: []
supersedes: []
superseded_by: "0005"
related: 
  - "0003"
tags:
  - infrastructure
  - ai
  - 3d
excerpt: >-
  ADR 0003 established that we keep RS's MySQL untouched and run a Postgres + pgvector adjunct database for new-feature data, linked to RS by rs_resource_id / rs_user_id. Phase 0 brings up the Postgres container but the database is empty.
---
## Context

ADR 0003 established that we keep RS's MySQL untouched and run a Postgres +
pgvector adjunct database for new-feature data, linked to RS by
`rs_resource_id` / `rs_user_id`. Phase 0 brings up the Postgres container
but the database is empty.

Every sidecar service we build — review sessions, AI gateway, embeddings,
modern annotations, video pipeline — will read or write tables in this
database. Designing the schema *after* services start writing to it
guarantees costly refactors. We need a coherent data model now, before
any sidecar code is written.

This ADR proposes:
- Tooling: migration runner + layout
- Identity strategy: how Postgres rows link back to RS
- Initial table set covering the Phase 1-2 sidecars
- The integration "seam": how PHP publishes events that sidecars consume

## Decision

### Migration runner: **`goose`**

- Go-native, drop-in CLI (no service to deploy)
- Plain `.sql` files with `-- +goose Up` / `-- +goose Down` markers
- Version table embedded in Postgres (`goose_db_version`)
- The same binary will run inside every Go sidecar's startup, so one
  consistent migration story across all services
- Alternatives rejected: **Atlas** (DDL diffing is powerful but overkill at
  our size); **Flyway** (Java dependency, heavy); **bare SQL + shell**
  (no versioning).

### Layout

```
infra/db/
├── migrations/
│   ├── 0001_extensions.sql        # supersedes infra/postgres/init.sql
│   ├── 0002_users_mirror.sql
│   ├── 0003_events_outbox.sql
│   ├── 0004_review_sessions.sql
│   ├── 0005_comments_annotations.sql
│   └── 0006_embeddings.sql
└── README.md
```

`infra/postgres/init.sql` becomes the first migration (`0001_extensions.sql`)
and the file is deleted from its old location.

### Identity strategy

| Kind of ID | Type | Why |
|---|---|---|
| Our primary keys | `UUID` (gen_random_uuid()) | Sidecars often generate IDs before persisting; UUIDs avoid round-trips for sequence values and play well across services |
| RS-owned foreign keys | `BIGINT` | Matches RS's `ref` columns natively; no casting; fast joins inside Postgres queries |
| Time | `TIMESTAMPTZ` always | Never store naive timestamps; UTC by default, display in the app |
| Soft delete | `deleted_at TIMESTAMPTZ NULL` where applicable | We need deleted history for audit and review traceability |

**No foreign-key constraints across the DB boundary** — we cannot
`REFERENCES mysql_resource(ref)`. Application-layer composition is the
contract. We can add referential integrity checks in code or via periodic
sweeps; we will not pretend Postgres can enforce it.

### Tables (initial set)

**`users` — mirror of RS user**

The minimal fields a sidecar needs to render an author label or send a
notification. Synced on demand from PHP (see "Seam" below). Populated lazily
the first time a Postgres-side action references the user.

```
users
  rs_user_id BIGINT PRIMARY KEY
  email TEXT NOT NULL
  display_name TEXT NOT NULL
  avatar_url TEXT NULL
  created_at, updated_at TIMESTAMPTZ
  deleted_at TIMESTAMPTZ NULL
```

**`events` — outbox / integration log**

Single append-only table that PHP writes to on every meaningful action.
Sidecars consume via Postgres `LISTEN/NOTIFY` on a `events_new` channel
plus periodic polling for missed events. No Redis needed for Phase 1-2.

```
events
  id BIGSERIAL PRIMARY KEY
  kind TEXT NOT NULL          -- 'resource.uploaded', 'comment.posted', ...
  subject_type TEXT NOT NULL  -- 'resource', 'session', 'comment'
  subject_id TEXT NOT NULL
  rs_user_id BIGINT NULL      -- actor, if any
  payload JSONB NOT NULL DEFAULT '{}'::jsonb
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
  processed_by JSONB NOT NULL DEFAULT '{}'::jsonb  -- {"ai-gateway": "2026-05-23T...", ...}
```

`processed_by` lets multiple sidecars consume the same event independently
without losing track.

**Review sessions (the killer feature)**

```
review_sessions
  id UUID PRIMARY KEY
  name TEXT NOT NULL
  mode TEXT NOT NULL          -- 'async' | 'sync'
  status TEXT NOT NULL        -- 'open' | 'closed'
  created_by_rs_user_id BIGINT NOT NULL
  presenter_rs_user_id BIGINT NULL  -- nullable; meaningful only for sync mode
  created_at, updated_at, closed_at

review_session_resources
  session_id UUID
  rs_resource_id BIGINT
  sort_order INT NOT NULL
  status TEXT NOT NULL DEFAULT 'pending'  -- pending | approved | rejected | needs_changes
  PRIMARY KEY (session_id, rs_resource_id)

review_session_participants
  session_id UUID
  rs_user_id BIGINT
  role TEXT NOT NULL          -- 'presenter' | 'reviewer' | 'observer'
  joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
  last_position INT NULL      -- which resource they were last looking at — drives "resume your spot"
  PRIMARY KEY (session_id, rs_user_id)

review_session_events  -- only populated for sync sessions; high-cardinality
  id BIGSERIAL PRIMARY KEY
  session_id UUID NOT NULL
  rs_user_id BIGINT NOT NULL
  kind TEXT NOT NULL          -- 'cursor_move' | 'next_resource' | 'annotation_added' | ...
  payload JSONB NOT NULL DEFAULT '{}'::jsonb
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
```

**Comments and annotations**

Modern comments live in Postgres. RS's own `comment` table is left alone
for legacy data. We do NOT migrate RS comments forward unless/until we
decide to.

```
annotations
  id UUID PRIMARY KEY
  rs_resource_id BIGINT NOT NULL
  created_by_rs_user_id BIGINT NOT NULL
  kind TEXT NOT NULL          -- 'pin' | 'rect' | 'path' | 'timestamp'
  data JSONB NOT NULL         -- shape-specific: {x, y} | {x, y, w, h} | {points: [...]}
  time_ms INT NULL            -- video annotations: frame-aligned offset
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
  deleted_at TIMESTAMPTZ NULL

comments
  id UUID PRIMARY KEY
  parent_id UUID NULL REFERENCES comments(id) ON DELETE CASCADE
  rs_resource_id BIGINT NOT NULL
  annotation_id UUID NULL REFERENCES annotations(id) ON DELETE SET NULL
  author_rs_user_id BIGINT NOT NULL
  body TEXT NOT NULL          -- markdown
  created_at, updated_at, resolved_at, deleted_at TIMESTAMPTZ
  resolved_by_rs_user_id BIGINT NULL

comment_reactions
  comment_id UUID
  rs_user_id BIGINT
  emoji TEXT NOT NULL
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
  PRIMARY KEY (comment_id, rs_user_id, emoji)

comment_mentions
  comment_id UUID
  mentioned_rs_user_id BIGINT
  PRIMARY KEY (comment_id, mentioned_rs_user_id)
```

**Embeddings (one table per dim)**

pgvector's `VECTOR(N)` requires a fixed dim at column definition. Different
models output different dims (768 / 1024 / 1536 / 3072). One table per dim
is the cleanest path.

```
embeddings_1536
  id UUID PRIMARY KEY DEFAULT gen_random_uuid()
  rs_resource_id BIGINT NOT NULL
  source_kind TEXT NOT NULL    -- 'image' | 'filename' | 'caption' | 'metadata' | 'comments_aggregate'
  source_ref TEXT NULL         -- e.g., for 'caption' the caption text hash; for 'image' null
  provider TEXT NOT NULL       -- 'openai' | 'anthropic' | 'voyage' | 'ollama'
  model TEXT NOT NULL          -- 'text-embedding-3-large' | 'voyage-3' | ...
  embedding VECTOR(1536) NOT NULL
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
  UNIQUE (rs_resource_id, source_kind, source_ref, provider, model)

CREATE INDEX ON embeddings_1536 USING hnsw (embedding vector_cosine_ops);
CREATE INDEX ON embeddings_1536 (rs_resource_id);
```

Initial dims to ship: `768`, `1024`, `1536`. Add others on demand.

### The "seam": how PHP publishes events to Go

PHP writes a row to `events` on every meaningful action. Hooked into RS at
known extension points (we'll add `// ARTIST-ALLEY:` calls to small list of
RS PHP files — upload, comment, login, resource edit). Go sidecars run a
single goroutine that:

1. Issues `LISTEN events_new` on a dedicated Postgres connection
2. On wake-up (or every ~5 seconds as a safety poll), processes rows where
   `processed_by ? '<service-name>' IS NULL`
3. Marks `processed_by['<service-name>'] = NOW()` after handling

Postgres `LISTEN/NOTIFY` gives sub-second delivery; the polling fallback
covers gaps (e.g., listener was offline). No Redis, no Kafka, no extra
infrastructure.

PHP side: a `// ARTIST-ALLEY:` helper function `aa_publish_event($kind,
$subject_type, $subject_id, $payload)` opens a Postgres connection (via
the existing `pdo_pgsql` extension we baked in) and inserts. Wrapped so
one call per RS hook site.

## Consequences

**Positive**
- Single source of truth for new-feature data; no schema sprawl across
  sidecars.
- LISTEN/NOTIFY gives us event-driven sidecars without Redis.
- pgvector ANN search is fast enough for "millions of assets" with HNSW
  indexes.
- BIGINT-for-RS-IDs makes joins inside Postgres cheap when we need to
  fetch related data.

**Negative**
- Cross-DB referential integrity is a discipline, not a constraint —
  orphaned rows are possible (e.g., RS resource deleted but comments remain).
  We will run a periodic sweep.
- pgvector one-table-per-dim creates schema duplication if we add many
  dims. Acceptable at 3-4 dims; revisit if we hit 8+.
- Polling fallback every 5 s on `events` adds light idle load. Negligible
  for our scale.

## Open questions (need user input)

1. **Workspace / Project hierarchy.** Game studios have a natural
   `Studio → Game → Milestone → Discipline → Asset` structure. RS has
   collections (flat-ish with parents). Three options:
   - **(a)** Use RS collections for everything; no `workspaces` table in
     Postgres. Simpler.
   - **(b)** Add `workspaces`/`projects` tables in Postgres as a top-level
     grouping for dashboard/reporting, but actual asset organization stays
     in RS collections. Hybrid.
   - **(c)** Full hierarchy in Postgres, RS collections become an
     implementation detail.

   **My lean: (b).** Lets us model "what game is this for" without
   reinventing RS's collection ACLs. But this is a real call you should
   weigh in on.

2. **User mirror — when does it sync?**
   - **(a) Lazy:** First time a Postgres action references a user, the
     sidecar fetches their RS profile via PHP API and inserts.
   - **(b) Eager event:** PHP fires `user.created` / `user.updated` events;
     sidecars upsert from event payload.
   - **(c) Periodic full sync:** Nightly job copies RS users into Postgres.

   **My lean: (b)** with `(a)` as a fallback for users that predate the
   event hook. Eager keeps the mirror near-real-time without a separate cron.

3. **Multi-tenancy: do we add `tenant_id` to every table now?** If
   artist-alley ever runs as a hosted service (multiple studios on one
   install), retrofitting tenant scoping is painful. Adding it day-one
   costs nothing. **My lean: yes — add `tenant_id UUID NOT NULL` on every
   "user data" table** (review_sessions, comments, annotations, embeddings),
   default to a single seed tenant in the migration. Free option value.

4. **Auth bridge — how does a Go sidecar know who's calling?** Two paths:
   - **(a)** Sidecar trusts a header (`X-User-Id`) set by nginx after
     validating RS's PHP session cookie. PHP middleware does the auth;
     sidecars trust the perimeter.
   - **(b)** Sidecar verifies the session cookie itself by calling a PHP
     endpoint or reading RS's `user_session` table directly.

   **My lean: (a)**. Cheap, fast, requires that nothing reaches the
   sidecar without passing through nginx + PHP — which is our deployment
   topology anyway.

5. **Embeddings: also store the raw text/file that was embedded?**
   Storing it lets us re-embed later without re-fetching from RS or
   regenerating captions. Costs a few KB per row. **My lean: yes — store
   `source_text TEXT NULL`** (truncate to 8KB if longer).

## Alternatives considered

- **MySQL-only** (extend RS's schema): no pgvector path. Rejected.
- **Single Postgres for everything** (migrate RS to Postgres): multi-month
  project, blocks all feature work. Already rejected in ADR 0003.
- **Document DB (Mongo / DynamoDB)**: loses transactional integrity and
  the SQL ergonomic. No.
- **Separate vector DB (Qdrant, Weaviate)**: more moving parts. pgvector
  is sufficient at our scale (10M+ vectors before we'd consider migrating).

## Next steps after acceptance

1. Move `infra/postgres/init.sql` → `infra/db/migrations/0001_extensions.sql`.
2. Add `goose` to the developer toolchain (a Go binary in the php container
   or a make target that runs it via `docker run`).
3. Write migrations 0002 through 0006 implementing the tables above.
4. Wire `bootstrap.sh` to run `goose up` after Postgres is healthy.
5. Add a smoke test: `docker compose run --rm goose-runner status`.
6. *Then* — and only then — start the first sidecar (probably
   `services/ai-gateway` to validate the seam end-to-end).
