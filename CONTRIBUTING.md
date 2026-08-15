# Contributing to artist-alley

artist-alley is early — the architecture is settling and the feature set is still landing. Contributions are welcome, but please **open an issue or [Discussion](https://github.com/mscrnt/artist-alley/discussions) before starting non-trivial work** so we can align on scope and approach.

## Development setup

Requirements: Docker with Compose v2.

```bash
git clone git@github.com:mscrnt/artist-alley.git
cd artist-alley
./scripts/bootstrap.sh
```

This creates `.env` with random passwords, matches container UIDs to your host, and brings the stack up.

The frontend dev container is opt-in:

```bash
docker compose --profile web up -d
```

## Tests

Run the full suite locally before pushing:

```bash
./scripts/test.sh
```

CI must be green before merge. The script runs Go unit and integration tests against a real Postgres — it does not mock the database.

The suite never touches your dev database. It resets and runs against a disposable `<POSTGRES_DB>_test` in the same Postgres container. In a `git worktree` the name gets a short hash of the checkout path appended, so runs from several worktrees can't reset each other's database mid-suite; that per-worktree database is dropped when the run exits (`AA_TEST_KEEP_DB=1` keeps it for a post-mortem). If two runs somehow land on the same database name, the second exits immediately with an explanation rather than force-dropping the first one's.

## Branches

- `main` — stable. Only tested, working checkpoints land here.
- `dev` — integration. Feature branches merge here first.
- `feat/<name>` — short-lived feature branches off `dev`. Delete after merge.

## Architectural changes

Anything that touches the public API, database schema, on-disk format, storage interface, or container topology requires an Architecture Decision Record. See [`docs/adr/`](docs/adr/) for examples — ADRs 0006 (Go backend), 0008 (storage), 0010 (permissions), and 0014 (frontend) are the most load-bearing.

ADR frontmatter follows the schema in [ADR 0035](docs/adr/0035-adr-conventions.md). It is machine-read by the docs site, so run `./scripts/check-adr-frontmatter.sh` after adding or editing one — CI runs the same check on every PR that touches `docs/adr/**`.

## Code style

- **Go:** `gofmt`, `go vet`, idiomatic Go. No clever metaprogramming.
- **TypeScript / Svelte:** project Prettier and ESLint configs.
- **Migrations:** goose, in [`app/internal/db/migrations/`](app/internal/db/migrations/). PL/pgSQL functions need `-- +goose StatementBegin` / `-- +goose StatementEnd` markers — fresh-DB CI runs fail without them.
- **OpenAPI:** the spec at [`app/api/openapi.yaml`](app/api/openapi.yaml) is the contract. After editing it, regenerate the frontend client with `npm run generate:api` from `web/`.
- **Icons:** we use [lucide](https://lucide.dev) via the `@lucide/svelte` package (`lucide-svelte` is the deprecated Svelte-4-era name — don't install it). Import each icon **by its own path** and never from the package root: `import Shapes from '@lucide/svelte/icons/shapes';`, not `import { Shapes } from '@lucide/svelte';`. The barrel re-exports every icon, so one glyph pulls the whole set through Vite's dev server; the production build tree-shakes either way, which means the barrel's cost is invisible in CI and lands entirely on you. Shared kind→icon mappings belong in [`web/src/lib/components/kindIcon.ts`](web/src/lib/components/kindIcon.ts). Some older components still carry inline lucide path data ( `CardFallback`, `ViewControls`); those migrate when the component is next touched, not in a sweep.

## Pull requests

- Use the PR template.
- Keep PRs focused — one logical change per PR.
- Reference related issues and ADRs.
- Squash if the commit history is noisy.

## Reporting security issues

A formal disclosure channel is not yet set up. For now, please open a private security advisory via GitHub (`Security` tab → `Report a vulnerability`) rather than filing a public issue. A `SECURITY.md` with a stable reporting path will land before the repo goes public.
