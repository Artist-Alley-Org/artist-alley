# artist-alley

A self-hosted art review and archival tool for game studios. Artist-first UX, reviewer-grade workflow, single-binary deploy.

> **Status:** pre-MVP, active development. The architecture is settled; the feature set is still landing. Not production-ready.

---

## Why this exists

Game studios review art and archive it across Slack, Miro, Teams, and shared drives. Knowledge fragments across tools, review is inconsistent, and assets become hard to find a few years later. No good self-hosted tool fills the gap.

artist-alley is built around three pillars:

1. **Artist-first, not archive-first.** Artists upload-and-forget. Metadata happens automatically where possible. UX target: [ArtStation](https://www.artstation.com/) levels of simplicity.
2. **Review mode.** Tracks unreviewed assets since the last session, remembers your spot, supports both async commenting (Frame.io-style) and live presenter sessions over SSE.
3. **Three-click rule.** Any common action reachable in three clicks or fewer.

---

## Architecture

The target shape is intentionally small:

- **One Go binary** serves the JSON API and embeds the SvelteKit SPA via `go:embed`.
- **PostgreSQL** (with `pgvector`) holds everything — assets, metadata, embeddings, sessions, audit log.
- **nginx** in front for TLS termination and static serving.

Three production containers, full stop: `nginx`, `app`, `postgres`. No microservices, no message bus, no sidecars.

Storage is pluggable — filesystem by default, S3-compatible (S3 / R2 / Backblaze / MinIO) optional. The plugin model is WASM via [Extism](https://extism.org/), deferred until external plugin authors arrive.

ADRs in [`docs/adr/`](docs/adr/) are the source of truth for architectural decisions.

### Transitional state

The project began as a fork of [ResourceSpace](https://www.resourcespace.com/) (BSD-3-Clause), a mature DAM with 20 years of battle-tested code. RS PHP still lives at the repo root and currently serves a shrinking set of legacy routes through `/api/v1/legacy/*` while the Go side ports each capability. When the last legacy route is replaced, the `php-fpm` container is removed and the repo collapses to the three-container shape. See [ADR 0003](docs/adr/0003-strangler-fig-internal.md), [ADR 0006](docs/adr/0006-go-as-target-backend.md), and [ADR 0015](docs/adr/0015-php-as-legacy-backend.md).

---

## Tech stack

| Layer | Tech |
|---|---|
| Backend | Go 1.26, `pgx`, `sqlc`, OpenAPI 3 (oapi-codegen) |
| Frontend | SvelteKit, TypeScript, Vite |
| Database | PostgreSQL 16 + pgvector |
| Migrations | [goose](https://github.com/pressly/goose) |
| Storage | filesystem (default), S3-compatible (optional) |
| Search | Postgres `tsvector` (text), pgvector (semantic, future) |
| License | BSD-3-Clause |

---

## Quickstart

Requirements: Docker with Compose v2.

```bash
git clone git@github.com:mscrnt/artist-alley.git
cd artist-alley
./scripts/bootstrap.sh
```

`bootstrap.sh` creates `.env` with random passwords, matches container UIDs to your host user, and brings the stack up. The frontend dev container is opt-in:

```bash
docker compose --profile web up -d
```

Open `http://localhost:8080` (or the port in your `.env`). Stop with `docker compose down`. Persistent data lives in named Docker volumes.

---

## Repository structure

```
artist-alley/
├── app/             # Go backend — the target architecture
│   ├── cmd/         # binaries (server entrypoint)
│   ├── internal/    # handlers, db, storage, auth, ...
│   └── api/         # OpenAPI spec + generated server stubs
├── web/             # SvelteKit frontend
├── aa_api/          # PHP legacy JSON wrappers (transitional — shrinking)
├── infra/           # Dockerfiles, nginx config, postgres init
├── docs/adr/        # Architecture Decision Records
├── scripts/         # bootstrap, test, seed scripts
├── api/ batch/ include/ lib/ pages/ ...   # ResourceSpace fork (transitional)
├── docker-compose.yml
├── LICENSE          # BSD-3-Clause (our additions)
└── license.txt      # ResourceSpace's notice, preserved
```

---

## Roadmap

| Phase | Focus | Status |
|---|---|---|
| 0 | Fork + containerize | done |
| 1 | Backend foundation: auth, assets, metadata, permissions, search, posts | **in progress** |
| 2 | Artist MVP: upload UX, browse, feed, basic comments | next |
| 3 | Review mode: annotations, timecode comments, live presenter sessions | planned |
| 4 | AI + 3D: provider-agnostic AI gateway, semantic search, glTF viewer | planned |
| 5 | Enterprise: OIDC/SAML SSO, audit logs, Perforce / Git LFS integration | planned |

Each phase ends with a working, shippable application.

---

## Contributing

Early days — please open an [issue](https://github.com/mscrnt/artist-alley/issues) or [Discussion](https://github.com/mscrnt/artist-alley/discussions) before starting non-trivial work. See [CONTRIBUTING.md](CONTRIBUTING.md) and the ADRs in [`docs/adr/`](docs/adr/) (especially 0006, 0008, 0010, 0014) before proposing structural changes.

---

## License and attribution

artist-alley is licensed under **BSD-3-Clause** — see [LICENSE](LICENSE).

The project is a fork of [ResourceSpace](https://www.resourcespace.com/) by [Montala Limited](https://www.montala.com/), also BSD-3-Clause. The original notice is preserved in [`license.txt`](license.txt) and [`documentation/licenses/`](documentation/licenses/). Sincere thanks to the RS contributors whose decades of work made this fork viable.
