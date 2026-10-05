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
| `standalone` | `tests/standalone/`       | Just the dev stack. Default for PR / pre-merge checks. **~364 tests in ~5 min at 2 workers.** |
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
├── fixture-ledger-report.mjs Which spec left which rows (#1247)
├── instance-lock-audit.mjs   Proof the config windows did not overlap (#1248)
├── helpers/
│   ├── test.ts               THE `test` EVERY SPEC IMPORTS — hydration + ledger
│   ├── auth.ts               loginAsAdminViaUI / loginAsAdminViaAPI
│   ├── fixture-ledger.ts     Records creates + deletes against their spec
│   ├── instance-lock.ts      Cross-process mutex over shared instance state
│   ├── public-mode.ts        `system.public_mode`, borrowed under that lock
│   ├── seeded-principal.ts   The principals the SEED owns (#1270)
│   ├── routes.ts             48-route manifest (anon / user / admin / catch-all)
│   ├── assertions.ts         expectPageRendersCleanly, expectPath
│   └── testids.ts            Canonical `data-testid` catalogue
├── tests/
│   ├── standalone/           (59 files, ~364 tests)
│   └── federation/           (2 files, 4 tests)
└── README.md
```

## Two rules the suite enforces on itself

The dogfood stack is persistent and the suite runs with two workers, so
a spec can change what a LATER spec sees — in the database, or in the
instance's own configuration. Both are guarded, and both guards report
from the run's own evidence rather than from a claim.

### 1. Import `test` from `helpers/test`, never from `@playwright/test`

```ts
import { test, expect } from '../../helpers/test';
import type { APIRequestContext, Page } from '@playwright/test'; // types are fine
```

That fixture wraps `page.goto()` to wait for hydration, and it is also
where the **fixture ledger** (#1247) hangs: every row a spec creates
through the API — directly, or by driving a form — is recorded against
the spec that created it, and every delete against the spec that removed
it. After each run `fixture-ledger-report.mjs` nets the two and prints
which spec left which rows, so the corpus census's "assets +12" has an
owner instead of being a number nobody can act on.

A spec that imports `test` from `@playwright/test` is invisible to that
accounting — which is exactly the spec a leak tends to come from.

### 2. Take shared instance config through `helpers/public-mode.ts`

`system.public_mode` has four writers, and each used to read the prior
value, set what it needed and put the prior back. That is a lost update
the moment two of them run at once: the second reads the first's
TEMPORARY value as the instance's, asserts against a mode it did not ask
for, and then publishes that temporary value as its "restore".

`test.describe.configure({ mode: 'serial' })` does not help — it orders
tests inside one describe block, not files across workers.
`helpers/instance-lock.ts` is a cross-process mutex (an O_EXCL lock file
keyed by the instance's base URL, so two checkouts driving one stack
still exclude each other), and `helpers/public-mode.ts` reads the prior
value **inside** the lock, which is the actual fix.

Every hold records its window; `instance-lock-audit.mjs` checks after
each run that no two windows on one lock overlapped, and says whether
anything ever had to wait — a run with no contention proves nothing, so
it is reported as such rather than counted as a pass.

If you add a spec that writes shared instance state, give it a lock and
say in the file what it contends over.

## What "clean up after yourself" means here

`DELETE` is a **soft** delete on `assets`, `posts` and `collections`
(`deleted_at`), and an **archive** on `field_definition` (`status`).
`user` has no delete at all. So a spec that deletes its fixtures has
cleaned up correctly and the ROW is still in the table — which is why
the corpus census counts live rows, and reports the raw totals beside
them as the sweep's backlog. `aa sweep-fixtures` is what removes rows;
it is dry-run by default.

### The rows a spec must NOT create (#1270)

Some fixtures cannot be cleaned up at all: **there is no user-delete
endpoint**, and a soft-deleted asset or post still counts against the
raw totals. Those belong to the SEED, not to a spec:

| what | who owns it |
|---|---|
| a principal that is not the bootstrap admin | `helpers/seeded-principal.ts` → `seed/profiles/dataset.fixtures.json` |
| assets + posts the bootstrap admin OWNS | `aa seed --fixtures` (`app/internal/seed/fixtures.go`) |

Seed them with:

```bash
aa seed --site <site> --catalogue seed/profiles --fixtures
```

⛔ **A spec that creates one of these on a miss is worse than one that
fails.** The fallback works forever, and quietly reintroduces a
permanent per-instance leak on every database that was seeded without
the flag — which is what a fresh CI database is. `requireSeededPrincipal`
and `requireAdminFixture` have no create arm on purpose; they fail and
name the command.

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
- The stack was seeded with `--fixtures` (see above). Without it, five
  specs fail in setup with a message naming the command.
- For `--federation`: `scripts/dogfood/pair.sh` has paired studio-a + studio-b.

## CI integration

Not yet wired. When it lands:

- Standalone is the gate for every PR / pre-merge.
- Federation runs nightly + on federation-touching branches.
