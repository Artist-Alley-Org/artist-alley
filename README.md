# artist-alley

[![Go version](https://img.shields.io/github/go-mod/go-version/Artist-Alley-Org/artist-alley?filename=app/go.mod)](app/go.mod)
[![License](https://img.shields.io/badge/license-AGPL--3.0--only-blue)](LICENSE)
[![Latest release](https://img.shields.io/github/v/release/Artist-Alley-Org/artist-alley?include_prereleases&sort=semver)](https://github.com/Artist-Alley-Org/artist-alley/releases)
[![Docs](https://img.shields.io/badge/docs-artist--alley.org-7c3aed)](https://artist-alley.org)

A self-hosted **art review and archival tool for game studios**. Artist-first UX, reviewer-grade workflow, single-binary deploy.

> **Status:** pre-1.0, active development. Releases are tagged and shipping (latest **v0.4.0**); the feature set is still landing. Not production-ready.

---

## Why this exists

Game studios review art and archive it across chat apps, whiteboards, and shared drives. Knowledge fragments across tools, review is inconsistent, and assets become hard to find a few years later. No good self-hosted tool fills the gap — and no DAM at all, open-source or commercial, federates between independently-operated instances. artist-alley is the first that does.

artist-alley is built around three pillars:

1. **Artist-first, not archive-first.** Artists upload-and-forget; metadata happens automatically wherever possible. The UX target is dead-simple — as easy as posting to a gallery, never filing into an archive.
2. **Review mode.** Tracks unreviewed assets since the last session, remembers your spot, supports both async commenting and live presenter sessions over SSE.
3. **Three-click rule.** Any common action reachable in three clicks or fewer.

Supporting all three: artist-alley implements [**ArchivePub**](docs/protocol/archivepub.md), an open federation protocol built on the ActivityPub data model with DAM-shaped extensions for asset sharing, workflow state, and brand workspaces. Studios share work with partners without paying SaaS rent; the protocol is open to any DAM that wants to implement it.

---

## Screenshots

Seeded with the public demo dataset ([ADR 0058](docs/adr/0058-demo-seed-dataset.md)).

![Browse feed (dark)](docs/screenshots/browse-dark.png)

| 3D viewer (live WebGL) | Faceted search |
|---|---|
| ![3D viewer](docs/screenshots/viewer-3d.png) | ![Search](docs/screenshots/search.png) |

<details>
<summary>More surfaces</summary>

![Browse feed (light)](docs/screenshots/browse-light.png)
![HDR viewer with AI variation](docs/screenshots/viewer-image.png)
![Admin overview](docs/screenshots/admin.png)

</details>

Demo content in screenshots is CC0 / CC-BY / CC-BY-SA and public-domain
source material; the aggregate set is CC-BY-SA 4.0 — per-source
attribution is in the dataset attribution list referenced in ADR 0058.

## Architecture

The target shape is intentionally small:

- **One Go binary** serves the JSON API and embeds the SvelteKit SPA via `go:embed`.
- **PostgreSQL** (with `pgvector`) holds everything — assets, metadata, embeddings, sessions, audit log.
- **nginx** in front for TLS termination and static serving.

Three production containers: `nginx`, `app`, `postgres`. No microservices, no message bus, no sidecars.

Storage is pluggable — filesystem by default, S3-compatible optional. Heavier capabilities (semantic image embeddings, audio transcription, document OCR, 3D-render thumbnails, local image-generation runtimes) ship as out-of-band **capability add-ons** that operators install separately — never baked into the binary. The plugin model for third-party extensions is WASM-based, deferred until external authors arrive.

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
| AI add-ons | local embedding / transcription / OCR runtimes, cloud AI-provider bridges — all opt-in |
| License | AGPL-3.0-only (dual-licensed — commercial license available, see [LICENSING.md](LICENSING.md)) |

---

## Quickstart

Requirements: Docker with Compose v2.

```bash
git clone git@github.com:Artist-Alley-Org/artist-alley.git
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

The full roadmap lives at [artist-alley.org/roadmap](https://artist-alley.org/roadmap/) and in [`docs/roadmap.md`](docs/roadmap.md). Every open issue is milestoned — see [**Milestones**](https://github.com/Artist-Alley-Org/artist-alley/milestones) for the release train.

- **Shipped (v0.1.0 – v0.4.0):** single-binary deploy, Postgres + pluggable storage, identity/auth with a full admin surface, upload pipeline, posts + collections, the universal asset viewer (image / video / audio / PDF / fonts / 3D / ebooks / comics / audiobooks / archives / docs / sprite sheets), federation over [ArchivePub](docs/protocol/archivepub.md), media derivatives, responsive UI, and the operator surfaces — jobs admin, storage admin, and storage integrity sweeps.
- **In flight (v0.5.0 — public mode):** anonymous browsing. Content visibility now has a single enforcement point ([ADR 0063](docs/adr/0063-content-visibility-predicate.md)), asset sensitivity gates bytes rather than rows ([ADR 0064](docs/adr/0064-sensitivity-gates-content-not-rows.md)), and public browsing is an operator setting that is **off by default**. Remaining: the public featured rail and audit-log PII gating.
- **Next (v0.6.0 – v0.10.0):** reports and moderation, the audit log, observability, runtime licensing, privacy and consent, share links, bulk operations, the capability add-on layer, external imports, the review-tool arc, commerce, and the plugin ecosystem — sequenced so no milestone depends on a later one.
- **v1.0.0:** feature-complete. Everything currently on the roadmap and in the issue tracker targets GA; new work defaults there unless explicitly labelled `post-v1.0.0`.

---

## Contributing

Early days — please open an [issue](https://github.com/Artist-Alley-Org/artist-alley/issues) or [Discussion](https://github.com/Artist-Alley-Org/artist-alley/discussions) before starting non-trivial work. See [CONTRIBUTING.md](CONTRIBUTING.md) and the [developer docs](https://artist-alley.org/developers/) — particularly [coding standards](https://artist-alley.org/developers/coding-standards/) and [security](https://artist-alley.org/developers/security/) — before opening a PR.

Architectural changes need an ADR per the convention in [ADR 0035](docs/adr/0035-adr-conventions/). Reverse-engineering or interoperability work needs to follow the clean-room methodology in [ADR 0040](docs/adr/0040-clean-room-reverse-engineering-methodology/).

---

## License

artist-alley is **dual-licensed**: **AGPL-3.0-only** for open-source use (see [LICENSE](LICENSE)) **or** a separate **commercial license** for use without the AGPL copyleft obligations — see [LICENSING.md](LICENSING.md). This is the license direction set in [ADR 0016](docs/adr/0016-license-direction.md); the monetization model is [ADR 0017](docs/adr/0017-monetization-and-licensing.md); premium add-ons under a separate EULA per [ADR 0038](docs/adr/0038-premium-add-on-layer.md).
