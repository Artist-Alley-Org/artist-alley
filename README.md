# artist-alley

A self-hosted art review and archival tool for game studios.

> **Status:** Phase 1.0 — early development. Forked from [ResourceSpace](https://www.resourcespace.com/) trunk @ r28830 (2026-05-21). RS PHP runs on PostgreSQL (Phase 0.5) and the Go server skeleton is up (Phase 1.0). Not yet ready for production use.

---

## Why this exists

Game studios review art and archive it across Slack, Miro, Teams, and shared drives. Knowledge fragments across mediums, review is inconsistent, and assets become hard to find a few years later. No good self-hosted tool fills the gap. artist-alley is the attempt.

It's built around three pillars:

1. **Artist-first, not archive-first.** Artists upload-and-forget. UX target: [ArtStation](https://www.artstation.com/) levels of simplicity. Metadata happens automatically where possible.
2. **Review mode.** Tracks unreviewed assets since the last session, remembers your spot, supports both async commenting (Frame.io-style) and live presenter sessions (SSE).
3. **Three-click rule.** Any common action reachable in three clicks or fewer.

---

## Architecture

artist-alley is a [Strangler Fig](https://martinfowler.com/bliki/StranglerFigApplication.html) on top of ResourceSpace.

- The forked **ResourceSpace** PHP code at the repo root provides the proven DAM substrate (resources, metadata, permissions, preview pipelines, plugin system, audit logs).
- New capabilities are added as **sidecar services** under `services/` rather than as new PHP modules. Default language is Go; Python where ML stacks demand it.
- A modern frontend in `web/` (SvelteKit) progressively replaces RS's PHP-rendered UI, starting with artist-facing flows.
- A **PostgreSQL + pgvector** adjunct database holds new-feature data (review sessions, embeddings, modern annotations) and is linked to RS resources by `resource_id`. RS keeps its MySQL.

See `docs/adr/` for the full set of architecture decisions.

### Planned sidecars

| Service | Purpose | Status |
|---|---|---|
| `services/ai-gateway` | Provider-agnostic AI: Anthropic, OpenAI, Azure OpenAI, Ollama, vLLM | Not started |
| `services/review-sessions` | SSE-based live review sessions, presenter mode, async threads | Not started |
| `services/video-pipeline` | HLS transcoding, frame-accurate scrub sprites, OG-quality preservation | Not started |
| `services/embeddings` | Vector generation, provider-pluggable, indexed in pgvector | Not started |

---

## Quickstart

Requirements: Docker with Compose v2.

```bash
git clone <this-repo> artist-alley
cd artist-alley
./scripts/bootstrap.sh
```

`bootstrap.sh` will:
- Create `.env` from `.env.example` with random passwords
- Match container UIDs to your host user
- Build and start the stack
- Print the URL to finish ResourceSpace's installer

Open `http://localhost:8080` (or the port in your `.env`) and complete the RS setup. On the database screen, use:

- **MySQL server:** `mysql`
- **MySQL user / password / database:** see the corresponding values in `.env`

Stop with `docker compose down`. Persistent data lives in the `mysql-data`, `postgres-data`, and `filestore` Docker volumes.

---

## Repository structure

```
artist-alley/
├── api/  batch/  css/  dbstruct/  documentation/  gfx/  iccprofiles/
├── include/  js/  languages/  lib/  pages/  plugins/  templates/
├── tests/  upgrade/  index.php  login.php  ...        # ResourceSpace fork
│
├── services/         # Go/Python sidecar services (Strangler Fig replacements)
│   ├── ai-gateway/
│   ├── review-sessions/
│   ├── video-pipeline/
│   └── embeddings/
├── web/              # SvelteKit frontend
├── infra/            # Dockerfiles, nginx config, postgres init, etc.
│   ├── docker/
│   ├── nginx/
│   └── postgres/
├── scripts/
│   ├── bootstrap.sh  # one-shot dev environment setup
│   └── rs-diff.sh    # compare this fork to upstream RS for cherry-picks
├── docs/
│   └── adr/          # Architecture Decision Records
├── docker-compose.yml
├── .env.example
├── LICENSE           # BSD-3-Clause (our additions)
├── license.txt       # ResourceSpace's license, preserved
└── README.md
```

---

## Roadmap

- **Phase 0** — Fork + containerize ResourceSpace. *(in progress)*
- **Phase 1** — Artist MVP: bulk upload, 2D + video pipeline, browse, async comments, basic auth.
- **Phase 2** — Review mode: draw annotations, timecode comments, live sessions over SSE.
- **Phase 3** — AI + 3D: provider-agnostic AI gateway, auto-tagging, semantic search, glTF viewer.
- **Phase 4** — Enterprise: OIDC/SAML SSO, RBAC depth, audit logs, Perforce / Git LFS integration.

Each phase ends with a working, shippable application.

---

## Contributing

Early days. If you've found this and want to help, open an issue first to align on scope. The architecture is documented in `docs/adr/`; please read 0001 through 0003 before proposing structural changes.

---

## License and attribution

artist-alley is licensed under **BSD-3-Clause** — see [LICENSE](LICENSE).

The project is a fork of **ResourceSpace** by [Montala Limited](https://www.montala.com/), also under BSD-3-Clause. Their copyright notice is preserved in [license.txt](license.txt) and [documentation/licenses/](documentation/licenses/). Enormous gratitude to the RS contributors whose 20 years of work made this fork viable.
