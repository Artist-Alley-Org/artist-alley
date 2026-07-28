// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Test stub for SvelteKit's `$app/environment`.
//
// vitest.config.ts deliberately loads only the Svelte plugin, not the
// full SvelteKit plugin, so the runner stays fast — which means the
// `$app/*` virtual modules SvelteKit injects at build time do not
// exist. Any component test whose import graph reaches a store that
// asks "am I in the browser?" fails to resolve without this.
//
// `browser: true` is the honest answer here: the suite runs under
// happy-dom with a real document, which is exactly the branch these
// stores take in a browser. Reporting false would send them down the
// SSR path and skip the localStorage / window wiring the components
// under test actually rely on.
//
// Kept a hand-written stub rather than pulling in the SvelteKit plugin
// on purpose: it is four constants, and the alternative is booting the
// whole kit for every pure-logic test in the suite.

export const browser = true;
export const dev = true;
export const building = false;
export const version = 'test';
