# Dogfood UI suite (Playwright)

Browser-driven dogfood tests that complement the shell scenarios
under `scripts/dogfood/scenarios/`. The shell scenarios catch
API + wire-protocol bugs; the UI suite catches:

- Navigation regressions (menu items disabled when they shouldn't be)
- API↔UI contract drift (the API still works but the UI silently no-ops)
- i18n key drift (admin page renders the raw key instead of a label)
- Capability-gating mistakes (admin can't see a page they should)
- Login flow breakage

## Layout

```
scripts/dogfood/ui/
├── package.json              @playwright/test
├── playwright.config.ts      two projects: studio-a + studio-b
├── helpers/
│   └── auth.ts               loginAsAdminViaUI / loginAsAdminViaAPI
├── tests/
│   ├── ui-01-login-and-overview.spec.ts
│   ├── ui-02-federation-menu.spec.ts
│   └── ui-03-peers-page-cross-instance.spec.ts
└── README.md                 this file
```

## Running

```bash
# Run everything (both stacks). Installs deps + Chromium on first run.
./scripts/dogfood/run-ui.sh

# Run against one stack only
./scripts/dogfood/run-ui.sh --project studio-a

# Grep filter
./scripts/dogfood/run-ui.sh --grep federation

# Interactive UI (debugging)
( cd scripts/dogfood/ui && npm run test:ui )
```

## Conventions

- One file per "scenario." File naming mirrors the shell scenarios
  (`ui-01-…`, `ui-02-…`) so the catalogue is easy to scan.
- Each file is self-contained — its own login + fixture setup.
  Tests inside one file may share state via `beforeEach`, but
  files don't share state.
- Use `test.skip(test.info().project.name !== 'studio-a', ...)`
  to scope a test to one project (= one stack).
- Prefer `getByRole`, `getByLabel`, `getByText` over CSS selectors.
  When you must use a CSS selector, write it close to a stable
  attribute (`data-testid`, `aria-*`, `role`).

## Adding a new test

1. Add a `tests/ui-NN-name.spec.ts` file using the same shape as the
   existing ones.
2. If it needs new helpers, drop them under `helpers/`.
3. Run `./scripts/dogfood/run-ui.sh --grep ui-NN` while writing it.
4. When green, leave it in place — it joins the regression catalogue
   automatically.

## Pre-reqs

- Node 20+ on PATH (any LTS).
- `scripts/dogfood/up.sh` has run.
- `scripts/dogfood/pair.sh` has run (UI-03 asserts cross-instance
  peer visibility).

## CI integration

Not yet wired. When it lands:

- Boot the dogfood profile in CI's compose stack.
- Run `up.sh + pair.sh` headlessly.
- Run `./scripts/dogfood/run-all.sh && ./scripts/dogfood/run-ui.sh`.
- Upload the JSON report + HTML report as build artifacts.
