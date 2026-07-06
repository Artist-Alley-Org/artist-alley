// Vitest configuration for the web/ frontend.
//
// Why a dedicated config (not the vitest field in vite.config.ts):
// vite.config.ts loads the full SvelteKit plugin which does dev-server
// + adapter wiring we don't want pulled into the test runner. This
// config loads only `@sveltejs/vite-plugin-svelte` so .svelte files
// compile + Svelte 5 runes work, but the SvelteKit boot path stays
// out — pure-logic tests (stores, parsers, utils) run in milliseconds
// without spinning up the full kit.
//
// Component tests that need $app/* or +page.ts can opt into the
// SvelteKit-aware config later; for the test pyramid this branch
// needs (pure stores + brush/sprite/archive parsers + a handful of
// component smokes), the lean setup is the right baseline.

import { svelte } from '@sveltejs/vite-plugin-svelte';
import { defineConfig } from 'vitest/config';
import { fileURLToPath } from 'node:url';

export default defineConfig({
  plugins: [svelte()],
  resolve: {
    // Svelte 5 ships separate server + browser exports; Testing
    // Library needs the browser entry (mount / lifecycle hooks live
    // there). Without this, `render()` throws
    // "lifecycle_function_unavailable: mount(...) is not available
    // on the server". Safe for pure-logic tests too — the browser
    // entry re-exports the same helpers pure-logic imports use.
    conditions: ['browser'],
    alias: {
      // Mirror svelte.config.js + svelte-kit's generated aliases so
      // imports like `$lib/...` in tests resolve the same way they do
      // in app code. svelte-kit normally writes these into
      // .svelte-kit/tsconfig.json; we re-declare them here so vitest
      // resolves them without booting svelte-kit.
      $lib: fileURLToPath(new URL('./src/lib', import.meta.url)),
      $api: fileURLToPath(new URL('./src/lib/api', import.meta.url)),
      $components: fileURLToPath(new URL('./src/lib/components', import.meta.url)),
      $stores: fileURLToPath(new URL('./src/lib/stores', import.meta.url)),
    },
  },
  test: {
    include: ['src/**/*.{test,spec}.{js,ts}'],
    globals: true,
    environment: 'happy-dom',
    setupFiles: ['./vitest-setup.ts'],
    // The Svelte 5 + Testing Library combo needs the browser-ish
    // condition to resolve $state/$effect runes at runtime instead
    // of falling back to the server stub.
    server: { deps: { inline: [/^svelte/, /@testing-library\/svelte/] } },
    // Coverage runs opt-in via `npm run test:coverage`; default test
    // runs stay quick.
    coverage: {
      provider: 'v8',
      reporter: ['text', 'html'],
      include: ['src/lib/**/*.{ts,svelte}'],
      exclude: [
        'src/lib/api/schema.d.ts',
        'src/lib/api/openapi-generated.ts',
        '**/*.test.ts',
      ],
    },
  },
});
