import { defineConfig, devices } from '@playwright/test';

// Default to the dev stack + studio-b as configured by
// `scripts/dogfood/up.sh`. Override via env when running against
// a different setup.
const STUDIO_A_HOST = process.env.STUDIO_A_HOST ?? 'http://localhost:5173';
const STUDIO_B_HOST = process.env.STUDIO_B_HOST ?? 'https://studio-b.local:9443';

export default defineConfig({
  testDir: './tests',
  outputDir: '.pw-results',
  reporter: [
    ['list'],
    ['html', { outputFolder: '.pw-report', open: 'never' }],
    ['json', { outputFile: '.pw-results/report.json' }],
  ],
  // Pin Chromium-only; UI dogfood doesn't need cross-browser.
  //
  // Only one project: studio-a. The dogfood profile spawns
  // postgres-b + app-b + nginx-b but NOT a second `web` (Vite)
  // container — the SvelteKit SPA is served by Vite against
  // studio-a only. studio-b is API-only at the HTTP layer.
  //
  // Tests that need to peek at studio-b's state (e.g. "did the
  // Like row land?") drive it through its API, not the browser.
  // The studio-a browser still exercises the full admin surface
  // — which IS what catches the menu / nav / contract bugs.
  projects: [
    {
      name: 'studio-a',
      use: {
        ...devices['Desktop Chrome'],
        baseURL: STUDIO_A_HOST,
        ignoreHTTPSErrors: true,
      },
    },
  ],
  // Allow Playwright to run several test files in parallel — each
  // test file owns its own login + fixture lifecycle, so they don't
  // step on each other.
  fullyParallel: true,
  // Don't auto-spawn a server; the dogfood stack is already up.
  // (CI / pre-merge would have its own wrapper.)
  workers: process.env.CI ? 1 : undefined,
  // 30s per test is plenty for the surfaces we're testing; flag
  // anything slower as a regression.
  timeout: 30_000,
  expect: {
    timeout: 5_000,
  },
});
