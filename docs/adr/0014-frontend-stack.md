---
id: "0014"
title: Frontend stack — SvelteKit, Tailwind, embedded in the Go binary
status: accepted
date: 2026-05-25
area: ux
phases: 
  - "1.11"
supersedes: []
related: 
  - "0003"
  - "0015"
tags:
  - ux
  - ai
  - 3d
excerpt: >-
  Phases 1.1–1.11 built the Go backend with RS PHP as the temporary frontend (strangler-fig per ADR 0003). The plan was that RS would render against our Postgres + Go API until each feature got its own native UI.
---
## Context

Phases 1.1–1.11 built the Go backend with RS PHP as the temporary
frontend (strangler-fig per ADR 0003). The plan was that RS would
render against our Postgres + Go API until each feature got its own
native UI.

That plan broke down. The recent Phase 1.11 work surfaced an open-
ended series of MySQL-vs-Postgres semantic gaps in RS's queries
(strict GROUP BY, implicit type coercion, DISTINCT + ORDER BY
rules). Every RS page we exercise costs hours of patching, and
every RS upgrade we ever do reintroduces the gaps. The locked target
architecture (per memory: "one Go binary, three containers, no
sidecars, no new PHP") already excludes PHP from prod — but we'd
never set a hard date for the cutover.

The user's decision: stop building against RS as the frontend. Treat
RS as a blueprint for *what* features need to exist, build our own
UI to render them. RS's PHP business-logic functions (`do_search`,
`save_resource_data`, etc.) remain available as a backend-gap-filler
via a separate mechanism (ADR 0015) — but the user-facing surface
is ours.

This ADR locks the frontend stack, the integration with the Go
binary, and the dev / prod build pipeline.

## Decision

### Stack

| Layer | Choice | Rationale |
|---|---|---|
| Framework | **SvelteKit 2 + Svelte 5 (runes)** | Smallest bundles of mainstream frameworks; file-based routing; runes give predictable reactivity; small enough mental model that one person can own it |
| Language | TypeScript (strict) | Type-safe; shares the OpenAPI contract with Go via codegen |
| Styling | **Tailwind CSS v4** | Image-grid + dark-mode + masonry are straightforward; `@theme` blocks + OKLCH palette gives clean token-driven theming |
| 3D | **Threlte** | Established Svelte binding for three.js, runes-compatible |
| Video / audio | **Vidstack** | Modern, framework-agnostic player with Svelte adapter; HLS / DASH support via hls.js when streaming lands |
| Icons | Lucide (Svelte port) | RS already uses it; visual consistency through the strangler-fig phase |
| API client | `openapi-typescript` + `openapi-fetch` | Generated from the same `app/api/openapi.yaml` that drives `oapi-codegen` on the Go side — one contract, two languages |
| Build | Vite (bundled with SvelteKit) | Standard |

Explicitly *not* adopted: React / Next.js (heavier, no benefit here),
Redux / Pinia / Zustand (Svelte runes are sufficient), Tailwind UI /
shadcn-svelte (defer until we hit a non-trivial primitive we don't
want to hand-roll).

### Integration with the Go binary

SvelteKit's `adapter-static` outputs a fully prerendered static
bundle (`web/build/`) at build time. The Go binary embeds the bundle
via `//go:embed` (gated on the `embed_web` build tag) and serves it
at `/`. The same Go binary handles `/api/v1/*` via the existing
handler tree. Routing precedence: API and health probes win over
the static catch-all (SPA fallback), so unknown paths return the
SvelteKit index and the client router takes over.

```
┌─────────────────────── prod (one binary, three containers) ────────────┐
│                                                                        │
│  Browser → nginx → Go binary :8080                                     │
│                       │                                                │
│                       ├── /api/v1/*       → Go handlers                │
│                       ├── /healthz        → Go handlers                │
│                       └── /*              → embedded SvelteKit bundle  │
│                                                                        │
└────────────────────────────────────────────────────────────────────────┘

┌─────────────────────── dev (four containers, +1 for HMR) ──────────────┐
│                                                                        │
│  Browser → web :5173 (Vite + HMR)                                      │
│              │                                                         │
│              └── /api/* proxy to ───→ app :8080 → Go handlers          │
│                                                                        │
│  Backend-only iterations skip the `web` profile and continue to use    │
│  RS at :8088 as before.                                                │
│                                                                        │
└────────────────────────────────────────────────────────────────────────┘
```

`adapter-static` was chosen over `adapter-node` because the latter
would require an additional Node container in prod, breaking the
locked three-container target. Per-request SSR is not load-bearing
for this app — image lazy-loading carries the perceived-perf win;
SEO surfaces (anonymous browse) can be served as a separate
prerendered route set if/when that matters.

### Build pipeline

- `web/package.json` declares the SvelteKit project. `npm install`
  runs `postinstall` which calls `svelte-kit sync` and (if
  `app/api/openapi.yaml` is present) regenerates
  `web/src/lib/api/schema.d.ts` via `openapi-typescript`.
- The generated TS schema is **not** committed (gitignored). It's
  regenerated on every install and on every `openapi.yaml` change.
  This differs from the Go side where sqlc/oapi-codegen output IS
  committed — the difference: TS regen is cheap and contained to a
  postinstall hook, whereas Go source files need to be present for
  `go build` and `go test` to pass on a fresh clone.
- Prod build: `npm run build` produces `web/build/`. The app
  Dockerfile (extended in a follow-up sub-phase) copies that output
  to `app/internal/http/static_assets/` and compiles Go with
  `-tags embed_web`.
- Dev build: no `-tags`, the Go binary skips static serving entirely
  (see `static_dev.go`); the `web` docker-compose service runs Vite
  on `:5173` with `/api/*` proxied to the Go binary.

### Repo layout

```
web/
├── src/
│   ├── routes/              # SvelteKit file-based routes
│   ├── lib/
│   │   ├── api/             # generated schema + thin openapi-fetch wrapper
│   │   ├── components/      # hand-rolled UI primitives
│   │   └── stores/          # runes-backed stores (theme, etc.)
│   ├── app.html             # HTML shell with no-FOUC theme init
│   ├── app.css              # Tailwind entrypoint + theme tokens
│   └── app.d.ts             # ambient types
├── static/
├── package.json
├── svelte.config.js
└── vite.config.ts

app/internal/http/
├── static_dev.go            # //go:build !embed_web — stub
├── static_embed.go          # //go:build embed_web   — //go:embed
└── static_assets/           # populated by Docker build (gitignored)
```

## Consequences

**Wins**

- The strict-GROUP-BY whack-a-mole stops. No more patching RS
  HTML-rendering pages; we touch RS PHP only for the backend gap-
  fillers (ADR 0015), which return arrays we shape into JSON.
- Prod stays at one Go binary, three containers — the locked target
  architecture survives intact.
- Modern frontend stack (SvelteKit, Tailwind v4, Threlte, Vidstack)
  unlocks the multimedia features (3D model viewer, video playback)
  RS could never offer cleanly.
- Single contract: the OpenAPI spec drives both Go (`oapi-codegen`)
  and TS (`openapi-typescript`). Drift between server and client is
  caught at compile time on both sides.

**Costs**

- A SvelteKit codebase to maintain. Several phases of frontend work
  (1.13.A–H) before users see the new UI light up.
- One extra dev container (the Vite HMR server). Backend-only days
  skip it via profile.
- Server-rendered HTML for first paint is lost. Mitigated by image
  lazy-loading + small JS bundle; SEO not load-bearing for an
  authenticated tool.
- New JS/TS toolchain in the build pipeline. `npm` enters the CI
  story. Acceptable: it stays scoped to `web/`, doesn't leak into
  the Go module.

## References

- ADR 0003 — Strangler fig (the original "RS as frontend" plan that
  this ADR retires)
- ADR 0015 — PHP as legacy backend (the companion piece — how RS's
  business logic stays available without RS's UI)
- Memory: "Target architecture — one Go binary, three containers"
  (locked 2026-05-24)
