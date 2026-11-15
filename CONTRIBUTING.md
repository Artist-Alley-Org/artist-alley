# Contributing to artist-alley

artist-alley is early — the architecture is settling and the feature set is still landing. Contributions are welcome, but please **open an issue or [Discussion](https://github.com/Artist-Alley-Org/artist-alley/discussions) before starting non-trivial work** so we can align on scope and approach.

## Development setup

Requirements: Docker with Compose v2.

```bash
git clone git@github.com:Artist-Alley-Org/artist-alley.git
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

## Public CI datasets (optional, for UI tests)

The UI suites seed from public dataset editions that anyone can download. These are GitHub Releases of `Artist-Alley-Org/artist-alley-dataset-site-a` and `-site-b` ([ADR 0100](docs/adr/0100-public-ci-datasets-via-versioned-release-payloads.md)). CI pins each one in [`seed/contracts/ci-datasets.v1.json`](seed/contracts/ci-datasets.v1.json), and the same helper fetches and verifies them on your machine. You need Docker, `git`, Python 3 and network access; no token or private path is involved.

```bash
mkdir -p ~/aa-datasets
export AA_CI_DATASET_PARENTS=~/aa-datasets              # where the dataset root is created
./scripts/ci/public-datasets.sh prepare ui-pr           # checks free space, creates ~/aa-datasets/aa-ci-datasets
export AA_CI_DATASET_ROOT=~/aa-datasets/aa-ci-datasets  # `prepare` cannot export it to your shell
./scripts/ci/public-datasets.sh fetch site_a_sm         # pinned tag and commit, verified download, sha256sum -c
```

**Keys.** `site_a_sm` is the small PR subset, about 0.7 GB. `site_a` (about 2.5 GB) and `site_b` (about 3 MB) are the full editions; for those, `prepare` with `ui-scale-site_a` or `ui-scale-site_b`.

**Failures stop.** A pin, download or checksum failure stops with an error. There is no fallback and nothing is retried.

**Seed and test.** Seed a running dev stack from `./scripts/bootstrap.sh` the way `ui-pr` does. Then run the standalone UI suite ([`scripts/dogfood/ui/README.md`](scripts/dogfood/ui/README.md)):

```bash
docker compose run --rm --no-deps \
  -v "$AA_CI_DATASET_ROOT/sites/site_a_sm:/seed/site:ro" \
  -v "$PWD/seed/profiles:/seed/profiles:ro" \
  app seed --site /seed/site --catalogue /seed/profiles --profile ci --fixtures
./scripts/dogfood/run-ui.sh standalone
./scripts/ci/public-datasets.sh release                 # deletes the downloaded dataset root
```

- **Re-seeding:** on a stack that already holds data, add `--reset`.
- **`--fixtures`:** creates test accounts with committed passwords, so use it only on a local or throwaway stack ([`seed/SEED_INSTRUCTIONS.md`](seed/SEED_INSTRUCTIONS.md) lists every flag).

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
