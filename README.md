# artist-alley

[![Go Report Card](https://goreportcard.com/badge/github.com/mscrnt/artist-alley)](https://goreportcard.com/report/github.com/mscrnt/artist-alley)
[![Go version](https://img.shields.io/github/go-mod/go-version/mscrnt/artist-alley?filename=app/go.mod)](app/go.mod)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![Latest release](https://img.shields.io/github/v/release/mscrnt/artist-alley?include_prereleases&sort=semver)](https://github.com/mscrnt/artist-alley/releases)
[![Docs](https://img.shields.io/badge/docs-artist--alley.org-7c3aed)](https://artist-alley.org)

A self-hosted **art review and archival tool for game studios**. Artist-first UX, reviewer-grade workflow, single-binary deploy.

> **Status:** pre-MVP, active development. The architecture is settled; the feature set is still landing. Not production-ready.

---

## Why this exists

Game studios review art and archive it across Slack, Miro, Teams, and shared drives. Knowledge fragments across tools, review is inconsistent, and assets become hard to find a few years later. No good self-hosted tool fills the gap — and no DAM at all, open-source or commercial, federates between independently-operated instances. artist-alley is the first that does.

artist-alley is built around three pillars:

1. **Artist-first, not archive-first.** Artists upload-and-forget. Metadata happens automatically where possible. UX target: [ArtStation](https://www.artstation.com/) levels of simplicity.
2. **Review mode.** Tracks unreviewed assets since the last session, remembers your spot, supports both async commenting (Frame.io-style) and live presenter sessions over SSE.
3. **Three-click rule.** Any common action reachable in three clicks or fewer.

Supporting all three: artist-alley implements [**ArchivePub**](docs/protocol/archivepub.md), an open federation protocol built on the ActivityPub data model with DAM-shaped extensions for asset sharing, workflow state, and brand workspaces. Studios share work with partners without paying SaaS rent; the protocol is open to any DAM that wants to implement it.

---

## Architecture

The target shape is intentionally small:

- **One Go binary** serves the JSON API and embeds the SvelteKit SPA via `go:embed`.
- **PostgreSQL** (with `pgvector`) holds everything — assets, metadata, embeddings, sessions, audit log.
- **nginx** in front for TLS termination and static serving.

Three production containers: `nginx`, `app`, `postgres`. No microservices, no message bus, no sidecars.

Storage is pluggable — filesystem by default, S3-compatible (S3 / R2 / Backblaze / MinIO) optional. Heavier capabilities (CLIP embeddings, Whisper transcription, Tesseract OCR, Blender-rendered thumbnails, Stable Diffusion / Flux / ComfyUI runtimes) ship as out-of-band **capability add-ons** that operators install separately — never baked into the binary. The plugin model for third-party extensions is WASM via [Extism](https://extism.org/), deferred until external authors arrive.

ADRs in [`docs/adr/`](docs/adr/) are the source of truth for architectural decisions. Start with [ADR 0006](docs/adr/0006-go-as-target-backend.md) (architecture), [ADR 0008](docs/adr/0008-storage-architecture.md) (storage), [ADR 0017](docs/adr/0017-monetization-and-licensing.md) (licensing + enterprise gates), [ADR 0034](docs/adr/0034-capability-add-ons.md) (add-on layer), [ADR 0038](docs/adr/0038-premium-add-on-layer.md) (commercial model), and [ADR 0043](docs/adr/0043-federation-walled-garden-protocol.md) (federation — the ArchivePub reference spec lives at [`docs/protocol/archivepub.md`](docs/protocol/archivepub.md)).

---

## Tech stack

| Layer | Tech |
|---|---|
| Backend | Go 1.26, `pgx`, `sqlc`, OpenAPI 3 (oapi-codegen) |
| Frontend | SvelteKit (Svelte 5 runes), TypeScript, Vite |
| Database | PostgreSQL 16 + pgvector |
| Migrations | [goose](https://github.com/pressly/goose) |
| Storage | filesystem (default), S3-compatible (optional) |
| Search | Postgres `tsvector` (text), pgvector (semantic) |
| AI add-ons | CLIP / Whisper / Tesseract (local), OpenAI / Anthropic / Stability (cloud bridge) — all opt-in |
| License | BSD-3-Clause (relicense to AGPL + commercial planned, Phase 1.24) |

---

## Quickstart

Requirements: Docker with Compose v2.

```bash
git clone git@github.com:mscrnt/artist-alley.git
cd artist-alley
./scripts/bootstrap.sh
```

`bootstrap.sh` creates `.env` with random passwords, matches container UIDs to your host user, and brings the stack up.

```bash
docker compose up -d            # core stack
docker compose --profile workers up -d   # + Blender worker for 3D thumbnails
docker compose --profile storage-s3 up -d  # + MinIO for S3-backend testing
```

Open `http://localhost:8080` (or the port in your `.env`). Stop with `docker compose down`. Persistent data lives in named Docker volumes.

---

## Repository structure

```
artist-alley/
├── app/             # Go backend — the runtime
│   ├── cmd/         # binaries (server entrypoint)
│   ├── internal/    # per-domain handlers, db, storage, auth, …
│   ├── api/         # OpenAPI spec + generated server stubs
│   └── schema.sql   # Postgres schema (mirrors goose migrations)
├── web/             # SvelteKit frontend
├── infra/           # Dockerfiles, nginx config, postgres init
├── docs/            # doc source (ADRs, roadmap, install) — the public
│   │                #   site at artist-alley.org renders these at build
│   ├── adr/         # Architecture Decision Records (source of truth)
│   └── research/    # Investigation notes, not yet decisions
├── scripts/         # bootstrap, test, seed
├── docker-compose.yml
├── Dockerfile
├── LICENSE
├── CONTRIBUTING.md
└── RELEASING.md
```

---

## Roadmap

The full roadmap lives at [artist-alley.org/roadmap](https://artist-alley.org/roadmap/) and in [`docs/roadmap.md`](docs/roadmap.md). Highlights:

- **Foundations (shipped):** single-binary deploy, Postgres + pluggable storage, identity & auth (full admin surface — paginated user list, lifecycle states, multi-device session management, password change/reset/history, teams admin UI, per-user capability grants/revokes, per-asset-type ACLs, audit-log viewer), upload pipeline, posts + collections, browse feed, post-detail modal, admin shell, theming, i18n, universal asset viewer with format coverage across image / video / audio / PDF / fonts / 3D / ebooks / comics / audiobooks / archives / docs / sprite sheets, whiteboard / brush surface, **license verifier + admin upload UI + capability-level enterprise-feature gating + identity-provider registry** (Phase 1.17.O/P — see [ADR 0017 § Status](docs/adr/0017-monetization-and-licensing.md)).
- **In flight:** first tagged release (`v0.1.0`), image + video processing pipelines, AI auto-tagging, the load-bearing review tool arc (Phase 1.18.B) — video player → polish → captions → image sequences → presentation rooms → annotation system → timeline assembly → A/B compare → DCC integrations → native 3D viewer. Real LDAP / SAML impls plug into the existing provider registry slots.
- **On the map:** advanced search, real LDAP/SAML/OAuth impls (foundation in place), tenant CRUD + per-tenant routing, storage tooling, reports, moderation, brand workspace, AI creative editing, federation, plugin ecosystem, observability, capability add-ons, premium add-on layer (commerce / ads / DCC plugins / cloud-bridge AI), external imports framework, caption / subtitle artifacts, native viewers for proprietary DCC formats.

---

## Contributing

Early days — please open an [issue](https://github.com/mscrnt/artist-alley/issues) or [Discussion](https://github.com/mscrnt/artist-alley/discussions) before starting non-trivial work. See [CONTRIBUTING.md](CONTRIBUTING.md) and the [developer docs](https://artist-alley.org/developers/) — particularly [coding standards](https://artist-alley.org/developers/coding-standards/) and [security](https://artist-alley.org/developers/security/) — before opening a PR.

Architectural changes need an ADR per the convention in [ADR 0035](docs/adr/0035-adr-conventions/). Reverse-engineering or interoperability work needs to follow the clean-room methodology in [ADR 0040](docs/adr/0040-clean-room-reverse-engineering-methodology/).

---

## License

artist-alley is currently licensed under **BSD-3-Clause** — see [LICENSE](LICENSE). A relicense to **AGPL + commercial** is planned at Phase 1.24 per [ADR 0016](docs/adr/0016-license-direction.md); premium add-ons under a separate EULA per [ADR 0038](docs/adr/0038-premium-add-on-layer.md).
