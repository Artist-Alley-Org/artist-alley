// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Test stub for SvelteKit's `$app/state`.
//
// Added for the same reason `app-environment.ts` exists and under the
// rule vitest.config.ts states beside the alias list: one stub per
// module a test actually reaches, never a blanket shim, so an
// unexpected `$app/*` dependency still fails loudly instead of quietly
// getting a fake.
//
// PostCard is what reaches this — it reads `page.url` to build the
// `/?post={id}` modal target. Only the CARD LAYOUT is under test, and
// that navigation is a Playwright concern (a jsdom `goto` proves
// nothing about whether a route renders), so the honest stub is a
// fixed, valid location rather than a router.
//
// A plain object and NOT a `$state` rune: nothing here is meant to be
// reactive, and a rune in a `.ts` file fails at runtime rather than at
// compile time. If a component under test ever needs the page to
// CHANGE, this becomes a `.svelte.ts` and the alias follows it.

export const page = {
  url: new URL('http://localhost/browse'),
  params: {} as Record<string, string>,
  route: { id: '/browse' as string | null },
  status: 200,
  error: null,
  data: {} as Record<string, unknown>,
  form: null,
  state: {} as Record<string, unknown>,
};

export const navigating = null;
export const updated = { current: false };
