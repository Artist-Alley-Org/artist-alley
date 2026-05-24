# ADR 0006: Go as the target backend; no sidecars

- Date: 2026-05-24
- Status: Accepted
- Supersedes the sidecar-heavy architecture in ADR 0003 (which is
  amended in this commit).

## Context

ADR 0001 established a hard fork from RS. ADR 0003 proposed a Strangler
Fig pattern with multiple Go *sidecar* services (`ai-gateway`,
`review-sessions`, `video-pipeline`, `embeddings`) talking to a forever-
PHP RS backbone over HTTP. The PHP-side glue was to be an `artist_alley`
RS plugin.

After working through Phase 0.5 (the PostgreSQL migration), the author's
view crystallised: **we don't want PHP long-term, and we don't want extra
containers**. The patching-vs-refactor question forced the decision:
either invest months in modernising RS PHP, or commit to leaving PHP and
plan the migration off of it. The latter wins for a passion project
where performance and simplicity matter more than short-term feature
velocity.

## Decision

**The target backend is a single Go server.** No sidecars. No
microservices. One binary, one process, one Postgres, one nginx in front.
Container count stays at three (nginx, app, postgres) throughout the
project's lifetime.

### Container/topology evolution

```
TODAY (Phase 0.5):
  nginx --> php-fpm --> RS PHP --> Postgres

TRANSITION (Phase 1+):
  nginx --> app (Go) --[ported routes]--+
              \                          |
               \--proxy to php-fpm--+    |
                                    |    |
                          RS PHP <--+    |
                              \          |
                               \--> Postgres

EVENTUAL (PHP retired):
  nginx --> app (Go) --> Postgres
```

The Go server is the new front door. For routes it has not yet ported, it
reverse-proxies to PHP-FPM. As we port routes, the proxy fallback
shrinks. When the last PHP route falls, the `php` container is deleted.

### What goes inside the Go binary

What earlier ADRs called "sidecars" become **packages inside the Go
monolith**:

- `internal/ai/` — provider-agnostic AI: Anthropic, OpenAI, Azure, Ollama, vLLM
- `internal/review/` — SSE-based live review sessions, async comments, annotations
- `internal/media/` — HLS transcoding, frame-accurate scrub sprites, thumbnail pipeline
- `internal/search/` — semantic search via pgvector, full-text via tsvector
- `internal/auth/` — sessions, OIDC/SAML when needed
- `internal/storage/` — S3-compatible blob storage abstraction
- `internal/http/` — handlers, routing, middleware, the legacy-PHP proxy

Each package is independently testable. Each can be scaled out to its own
binary later if we ever need it (we won't), so the door stays open
without paying complexity today.

### Why Go specifically

- Solo-dev sustainability: one language across the whole backend.
- Single static binary: trivial deploys, no runtime, no extension hell.
- Performance: 50+ queries/page RS workloads become trivially fast.
- The author is already comfortable with Go (DevProxy project).
- Good DB story for Postgres: `pgx` for the driver, `sqlc` for type-safe
  generated queries, `goose` for migrations (already chosen in ADR 0005).
- Native HTTP, SSE, websocket, concurrency in stdlib — no need to add
  separate processes for streaming or background work.

Rejected alternatives:
- **Stay on PHP** — performance ceiling and dependency hell make it the
  wrong long-term home for this project.
- **Rust** — fastest, but solo-dev iteration speed suffers; not worth the
  trade-off when Go is already fast enough.
- **TypeScript/Node** — better for the frontend; backend tradeoffs vs Go
  don't favour it here.
- **.NET / Java** — more ceremony per line of code than Go offers; runtime
  baggage we don't need.

### Migration strategy

Phase 0.5 (in progress) ends when RS runs cleanly on Postgres. We finish
that — not because we love the PHP, but because:

1. It validates the schema design we'll consume from Go.
2. It gives us a working UI to populate test data while Go is being
   built.
3. We're close — only dialect patches remain.

After Phase 0.5 completes:

- **Phase 1.0 — Go server skeleton.** New `app/` directory with cmd, internal/, migrations, tests. The binary starts, talks to Postgres, serves a single `/health` endpoint. nginx is updated to route to it. PHP still handles all real routes.

- **Phase 1.x — First ported feature.** One self-contained RS endpoint is rewritten in Go. Most likely candidate: artist upload (highest UX leverage and most artist-facing) or auth/users (smallest blast radius, unblocks everything else). Decided at Phase 1.0 sign-off.

- **Phase 2..N — Iterative replacement.** Each iteration: pick a feature, port it, delete the PHP. Keep going until PHP is gone.

- **Frontend.** SvelteKit in `web/` remains the plan (per earlier discussions). It talks to the Go API. PHP UI is replaced page-by-page as Go endpoints exist to back those pages.

### What changes immediately

- `services/` directory (created in Phase 0 scaffolding for the now-rejected sidecar architecture) is **deleted** when Phase 1.0 starts. Its `.gitkeep` files come with it.
- A new `app/` directory will be created at Phase 1.0 with Go layout (`cmd/`, `internal/`, `pkg/` as needed).
- ADR 0003 is amended (in the same commit as this ADR) to remove sidecar language; the Strangler Fig pattern remains in spirit (PHP is being progressively replaced) but the strangler is a single Go server, not a fleet of services.
- No new PHP code is written after Phase 0.5 lands. Bug fixes to RS PHP keep happening only as needed to keep the install usable until the relevant feature is ported to Go.

## Consequences

**Positive**
- One language across the whole backend forever.
- Container count stays small (three) regardless of feature surface.
- Performance ceiling raises dramatically over PHP.
- Solo-dev cognitive load: one mental model, one debugger, one deployment artifact.
- Clean architectural story for outside contributors: "Go on Postgres, with an in-flight legacy PHP proxy until everything is ported."

**Negative**
- Multi-month rewrite of RS features required. Each port is real work.
- During transition we run both PHP and Go. The reverse-proxy layer must be solid.
- Auth/session interop across the boundary needs design (likely: Go owns sessions from day one and gives PHP a header it can trust).
- Existing RS plugin ecosystem is irrelevant to artist-alley after the port — anyone who would have written an RS plugin has to write Go instead.

**Mitigations**
- Port one feature at a time so the blast radius of any single change is
  small.
- Keep PHP runnable throughout so we can fall back to the legacy code
  path if a Go port misbehaves.
- Auth: have Go take over sessions in Phase 1.x; PHP reads them via a
  shared cookie + header that Go signs. Phase 1.x doesn't ship until
  this works.

## Open questions

1. **Auth bridge concrete design** — defer to Phase 1.x kickoff.
2. **Frontend in Phase 1.x** — does the first ported feature ship with
   a SvelteKit page or a server-rendered Go template? My lean: server-
   rendered Go templates for the first one or two features so we
   validate the backend before committing to the frontend stack. The
   user explicitly OK'd SvelteKit being the eventual web framework.
3. **First feature** — see Migration Strategy above; decided at Phase 1.0
   sign-off. Author has not yet picked.

## Notes

This is a passion project; the multi-month timeline is acceptable. The
end state — one Go binary serving everything, no PHP, no sidecars, three
containers total — is worth the journey.
