# Dogfood UI suite (Playwright)

Browser-driven dogfood tests that complement the shell scenarios
under `scripts/dogfood/scenarios/`. Catches:

- Navigation regressions (menu items disabled when they shouldn't be)
- API↔UI contract drift (the API still works but the UI silently no-ops)
- Login + sign-out flow breakage + session lifecycle bugs
- i18n key drift (page renders the raw key instead of the label)
- Capability-gating mistakes (admin can't see a page they should)
- Null-scan crashes on list endpoints
- 404 + error boundary shape

## Two test sets

| Set          | Where                     | Needs                                                                                     |
| ------------ | ------------------------- | ----------------------------------------------------------------------------------------- |
| `standalone` | `tests/standalone/`       | Just the dev stack. Default for PR / pre-merge checks. **133 tests in ~60s.**             |
| `federation` | `tests/federation/`       | Dev stack + `dogfood` profile + `pair.sh` run. Run during dogfood weeks. **4 tests in ~4s.** |

The split is at the file level — each test file is in exactly
one set. Run `all` to run both.

## Running

```bash
# Default — standalone only
./scripts/dogfood/run-ui.sh
./scripts/dogfood/run-ui.sh standalone     # explicit

# Federation only (assumes pair.sh has run)
./scripts/dogfood/run-ui.sh federation

# Both projects
./scripts/dogfood/run-ui.sh all

# Filter by test name (passthrough to playwright test)
./scripts/dogfood/run-ui.sh standalone --grep 'admin menu'

# Interactive UI (debugging)
( cd scripts/dogfood/ui && npm run test:ui )
```

The matching stack-up shortcut:

```bash
./scripts/dogfood/up.sh --standalone       # studio-a only (~30s)
./scripts/dogfood/up.sh                    # studio-a + studio-b (full federation)
```

First run installs `@playwright/test` + Chromium under
`scripts/dogfood/ui/node_modules/` + `~/.cache/ms-playwright/`.
Subsequent runs reuse the install (~1s startup vs ~60s).

## Layout

```
scripts/dogfood/ui/
├── package.json              @playwright/test
├── playwright.config.ts      Two projects: standalone + federation
├── helpers/
│   ├── auth.ts               loginAsAdminViaUI / loginAsAdminViaAPI
│   ├── routes.ts             48-route manifest (anon / user / admin / catch-all)
│   ├── assertions.ts         expectPageRendersCleanly, expectPath
│   └── testids.ts            Canonical `data-testid` catalogue
├── tests/
│   ├── standalone/           (16 files, 133 tests)
│   └── federation/           (2 files, 4 tests)
└── README.md
```

## Selector convention

Prefer `data-testid` for any element a test interacts with that
isn't a stable role + accessible-name pair. Catalogue lives in
`helpers/testids.ts`; component side uses `data-testid="…"` /
the `testId` prop wired through `Button`, `TextField`, and
`Menu`.

Fallback ordering:
1. `tid('foo-bar')` → `data-testid` (most stable)
2. `getByRole('link', { name: 'Foo' })` (i18n-stable when the
   element role + label are durable)
3. `getByText('Foo')` (last resort — copy can change)

## Adding a new test

1. Pick the right set:
   - Does the assertion need studio-b to be running + paired?
     → `tests/federation/`
   - Otherwise → `tests/standalone/`
2. Add a new `ui-NN-name.spec.ts` file. Follow the existing
   file structure (`test.describe` + `loginAsAdminViaUI` in
   `beforeEach`).
3. If you need a stable selector, add a testid:
   - Append the id to `helpers/testids.ts`
   - Wire it in the component (via the `testId` prop on `Button`/
     `TextField`/`Menu`, or `data-testid="…"` for everything else)
   - Use `page.locator(tid('your-id'))` in the test
4. Run `./scripts/dogfood/run-ui.sh --grep ui-NN` while iterating.
5. New testids show up without restarting anything. This used to
   need a `docker compose restart web`, and the reason was never
   HMR: the dev container's bind mount sits on a filesystem that
   drops inotify, so Vite's watcher never heard about the edit and
   kept serving the module it had cached. `web/vite.config.ts` now
   polls instead (#993). Detection takes up to ~0.5s, so a test run
   fired the same instant you saved can still race the watcher —
   give it a beat.

## Pre-reqs

- Node 20+ on PATH (any LTS).
- `scripts/dogfood/up.sh` has run.
- For `--federation`: `scripts/dogfood/pair.sh` has paired studio-a + studio-b.

## CI integration

Not yet wired. When it lands:

- Standalone is the gate for every PR / pre-merge.
- Federation runs nightly + on federation-touching branches.
