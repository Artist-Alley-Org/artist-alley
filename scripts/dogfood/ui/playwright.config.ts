import { defineConfig, devices } from '@playwright/test';
import { ADMIN_STATE_PATH } from './helpers/auth';

// Default to the dev stack as configured by `scripts/dogfood/up.sh`.
// Override via env when running against a different setup.
const STUDIO_A_HOST = process.env.STUDIO_A_HOST ?? 'http://localhost:5173';

// Two top-level test groups under tests/:
//
//   standalone/ — tests that pass against the dev stack alone.
//                 Run these as part of every PR / pre-merge check.
//                 No federation dependency: no pair.sh required,
//                 no studio-b containers required.
//
//   federation/ — tests that ASSUME both studio-a (dev) and
//                 studio-b (dogfood profile) are running AND
//                 paired via scripts/dogfood/pair.sh.
//                 Reads from studio-b via its admin UI through
//                 studio-a's session (peer rows, outbox status,
//                 inbox visibility) plus drives wire flows that
//                 only land cross-instance.
//
// Each is its own Playwright "project" so the CLI --project flag
// filters cleanly:
//   npx playwright test --project standalone     # default work
//   npx playwright test --project federation     # dogfood weeks
//   npx playwright test                          # both
//
// scripts/dogfood/run-ui.sh wraps the common cases.

export default defineConfig({
  // Top-level testDir intentionally omitted — each project pins
  // its own testDir so the standalone vs federation split survives
  // CLI --project filtering.
  outputDir: '.pw-results',
  reporter: [
    ['list'],
    ['html', { outputFolder: '.pw-report', open: 'never' }],
    ['json', { outputFile: '.pw-results/report.json' }],
  ],
  projects: [
    {
      // Shared-session setup (#481): logs in once and writes
      // storageState, so the standalone project below skips ~130
      // per-test logins — the auth-race + mid-suite-ECONNREFUSED source.
      name: 'setup',
      testDir: './tests/setup',
      testMatch: /.*\.setup\.ts/,
      use: {
        ...devices['Desktop Chrome'],
        baseURL: STUDIO_A_HOST,
        ignoreHTTPSErrors: true,
      },
    },
    {
      name: 'standalone',
      testDir: './tests/standalone',
      // Run the setup project first + reuse its authenticated session for
      // every test (#481). Tests whose subject IS the login flow opt out
      // per-file with `test.use({ storageState: undefined })` (ui-16), so
      // the real form stays covered.
      dependencies: ['setup'],
      // Retry transient infra blips on CI as a backstop (#485/#481). The
      // dominant cause — ~130 per-test login round-trips saturating the
      // runner→app connection, surfacing as mid-suite ECONNREFUSED — is
      // removed by the shared session above; retries stay for whatever
      // residual runner-under-load blip slips through. (NOT a Vite issue:
      // CI runs the embedded prod build at app.aa:8080, never Vite —
      // ui-pr.yml.) CI-only (0 locally) so a flake still fails
      // immediately for whoever is fixing it; a test that fails all 3
      // attempts still reds the run (retries tolerate the transient, not
      // the persistent — ADR 0068).
      retries: process.env.CI ? 2 : 0,
      use: {
        ...devices['Desktop Chrome'],
        baseURL: STUDIO_A_HOST,
        ignoreHTTPSErrors: true,
        // Reuse the admin session written by the `setup` project (#481).
        storageState: ADMIN_STATE_PATH,
      },
    },
    {
      name: 'federation',
      testDir: './tests/federation',
      use: {
        ...devices['Desktop Chrome'],
        baseURL: STUDIO_A_HOST,
        ignoreHTTPSErrors: true,
      },
    },
  ],
  fullyParallel: true,
  // Cap at 2 workers — observed flake at 4+ where parallel auth +
  // /admin loads saturate Vite's dev server and tests timeout
  // waiting for hydration. CI still pins to 1 for repeatability.
  workers: process.env.CI ? 1 : 2,
  timeout: 30_000,
  // Assertion polling timeout. Default 5s; CI runs against the
  // embedded prod build (adapter-static + SPA fallback) where
  // initial HTML is the root shell and the client router has to
  // hydrate + navigate + set <title> + re-render <main>
  // asynchronously. 5s is sometimes too short for that chain
  // under load. 15s leaves headroom without slowing the success
  // path (assertions poll, so a fast page passes in milliseconds
  // either way).
  expect: { timeout: 15_000 },
});
