# artist-alley.org — docs & whitepaper site

Static site rendered with [Astro](https://astro.build) +
[Starlight](https://starlight.astro.build), deployed to
[Cloudflare Pages](https://pages.cloudflare.com). Lives in the same
monorepo as the app so docs travel with code changes — one PR, one truth.

## What's here

```
site/
├── astro.config.mjs              ← Starlight config, sidebar, plugins
├── package.json                  ← prebuild step regenerates auto-docs
├── scripts/
│   ├── sync-adrs.mjs             ← docs/adr/*.md → src/content/docs/adr/
│   ├── sync-openapi.mjs          ← app/api/openapi.yaml → public/openapi.yaml
│   └── generate-schema-docs.mjs  ← app/schema.sql    → reference/schema.mdx
├── src/
│   ├── assets/logo.svg
│   ├── content.config.ts
│   ├── content/docs/             ← hand-authored MDX
│   │   ├── index.mdx
│   │   ├── whitepaper/
│   │   ├── guides/
│   │   ├── reference/            ← schema.mdx auto-generated, gitignored
│   │   └── adr/                  ← auto-synced, gitignored
│   └── styles/custom.css
└── public/                       ← openapi.yaml auto-synced, gitignored
```

## Local development

```bash
cd site
pnpm install
pnpm dev          # http://localhost:4321
```

`predev` and `prebuild` both run the three sync scripts, so the auto-generated
pages stay in step with `app/`, `app/api/`, and `docs/adr/` automatically.

## Auto-generated content

| Output page                       | Source                              | Script                           |
|-----------------------------------|-------------------------------------|----------------------------------|
| `/api/reference/*` (sidebar group)| `app/api/openapi.yaml`              | `sync-openapi.mjs` + starlight-openapi |
| `/reference/schema/`              | `app/schema.sql`                    | `generate-schema-docs.mjs`       |
| `/adr/*`                          | `docs/adr/*.md`                     | `sync-adrs.mjs`                  |

Edit the source, not the generated MDX. Generated files are in `.gitignore`.

## Deployment — Cloudflare Pages

The site is built and served by Cloudflare Pages' native Git integration.
**No GitHub Actions required for deploy** — CF Pages clones the repo, runs
the build, and ships the artifact on every push.

### One-time setup (manual)

1. **Add `artist-alley.org` to your existing Cloudflare account** (same
   account as `mscrnt.com`). Free — zones are unlimited on the Free plan.
2. At the registrar where you bought `artist-alley.org`, change the
   nameservers to the two CF gave you. Propagation takes 1-24h.
3. In Cloudflare → **Workers & Pages → Create → Pages → Connect to Git**:
   - Repository: `mscrnt/artist-alley`
   - **Production branch**: `main`
   - **Build command**: `cd site && pnpm install --frozen-lockfile && pnpm build`
   - **Build output directory**: `site/dist`
   - **Root directory**: leave empty (we cd into `site/` ourselves)
   - **Environment variables**:
     - `NODE_VERSION` = `20`
     - `PNPM_VERSION` = `10`
     - `PUBLIC_SITE_URL` = `https://artist-alley.org`
4. In the Pages project → **Custom domains** → add:
   - `artist-alley.org` and `www.artist-alley.org` → bound to **main** branch
   - (Optional) `dev.artist-alley.org` → bound to **dev** branch preview
5. Push to `main` → production at https://artist-alley.org.
   Push to `dev` → preview at https://dev.<project>.pages.dev (and
   https://dev.artist-alley.org if you added the custom domain above).
   Push to any other branch → preview at https://<branch>.<project>.pages.dev.

### What lives where after setup

- **Production** (main branch) → `https://artist-alley.org`
- **Dev preview** (dev branch) → `https://dev.<project>.pages.dev`
- **PR previews** (every other branch) → `https://<branch>.<project>.pages.dev`

Cloudflare Pages keeps the last 100 deployments around, so you can
roll back to any prior commit from the dashboard.

## Adding content

- **Whitepaper / guides / hand-authored pages** — add an `.mdx` file
  under `src/content/docs/<section>/` and update the sidebar in
  `astro.config.mjs` if you want a stable nav slot. Starlight will
  pick it up automatically otherwise.
- **API surface** — edit `app/api/openapi.yaml`. The rendered
  reference rebuilds on next site build.
- **Database table** — edit `app/schema.sql`. Add a `-- comment`
  block above the `CREATE TABLE` to give the table a description on
  its reference page.
- **Architecture decision** — drop a new `docs/adr/NNNN-<slug>.md`
  in the repo root's ADR folder. It surfaces in the sidebar
  automatically on next build.

## Search

Pagefind builds the full-text index at deploy time. No Algolia,
no DocSearch, nothing external. Free and works offline.

## Notes on the prebuild scripts

- All three scripts run on `predev` *and* `prebuild`, so local
  dev and production builds always see fresh generated content.
- Each script writes into the docs collection and is **gitignored**.
  Generated artifacts never end up in commits.
- Scripts fail soft: if `app/schema.sql` or `app/api/openapi.yaml`
  is missing, the build still succeeds with a placeholder page.
- Add more sync scripts (e.g. Go doc extraction) by:
  1. Dropping the script in `site/scripts/`,
  2. Chaining it into `prebuild` + `predev` in `package.json`,
  3. Adding its output to `.gitignore`.
