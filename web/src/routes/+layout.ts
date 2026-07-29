// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Root layout load.
//
// adapter-static prerenders this layout at build time, but the load
// function runs again on every client-side navigation. We use it to
// decide where the user is allowed to go based on auth + setup state.
//
// Flow:
//   1. /api/v1/setup/status — if `needs_setup`, force the user onto
//      /setup regardless of where they were trying to go.
//   2. /api/v1/auth/me — populates the auth store.
//   3. If the route requires auth and there's no session, redirect
//      to /login with a `?next=` carrying the original URL. The
//      login form posts back and routes the user where they meant
//      to go.
//
// Public routes skip the auth requirement; the gate applies to
// everything else.

import { redirect } from '@sveltejs/kit';
import { api } from '$api/client';
import { auth } from '$stores/auth.svelte';
import { browser } from '$app/environment';

// Always reachable without a session, in BOTH public-mode states.
// These are the routes an operator needs before they can possibly
// have one.
const ALWAYS_PUBLIC_ROUTE_IDS = new Set(['/login', '/setup']);

// Reachable without a session ONLY when the install has public mode
// turned on (#416, #445).
//
// Matched against `route.id`, not `url.pathname`. That is what lets an
// EXACT-MATCH set express a parameterised route: SvelteKit hands the
// load function the route id (`/collections/[id]`), while the pathname
// is the concrete URL (`/collections/9f3c…`). Prefix matching on the
// pathname was the alternative and is worse in the way that matters —
// a `/collections` prefix silently grandfathers every route somebody
// mounts underneath it later, which is precisely how a route gets
// published by accident. With exact route ids a new route is
// authenticated until someone adds a line here, so the failure mode
// is a route that should be public not being public: visible, and
// harmless. Same reasoning as the backend deny-list in
// auth/publicmode.go.
//
// DELIBERATELY ABSENT: `/posts/[id]`. The post visibility rule has a
// followers tier that the predicate does not model, so posts stay
// authenticated until it does. Do not add it here to "finish the
// set".
//
// `/assets/[id]` is the standalone asset page (#475). It is safe to
// make public for the same reason `/collections/[id]` is: the getAsset
// endpoint applies the visibility predicate (ADR 0064) and returns 404
// for any asset the caller may not see, so the page can never confirm
// a hidden asset exists — the gate lives on the server, not here.
const PUBLIC_MODE_ROUTE_IDS = new Set([
  '/',
  '/collections',
  '/collections/[id]',
  '/assets/[id]',
  '/search',
  '/search/advanced',
]);

// Reachable without a session ONLY when the install accepts
// self-service signups (#712).
//
// Deliberately its own set rather than a line in either of the two
// above. It cannot go in ALWAYS_PUBLIC_ROUTE_IDS — a closed install
// should not serve a signup form — and it must not ride on public
// mode, because those are unrelated settings: public mode is "may
// strangers read this library", self-registration is "may strangers
// open an account here", and an operator can plausibly want either
// without the other. Folding /register into PUBLIC_MODE_ROUTE_IDS is
// what made this bug hard to see coming, so the two stay separate.
//
// Same exact-match-on-`route.id` discipline as above.
const SELF_REGISTRATION_ROUTE_IDS = new Set(['/register']);

export const ssr = false;
export const prerender = true;
export const trailingSlash = 'never';

export async function load({ url, route, fetch }) {
  if (!browser) {
    return { needsSetup: false, authed: false };
  }

  // Setup gate — short-circuits everything when the system isn't
  // configured yet. Uses load()'s `fetch` so SvelteKit can do its
  // request-deduplication and serialise responses across SSR/CSR
  // boundaries cleanly (avoids the "Loading using window.fetch"
  // hydration warning).
  const setupRes = await fetch('/api/v1/setup/status').catch(() => null);
  const setup = setupRes && setupRes.ok ? await setupRes.json() : null;
  const needsSetup = !!setup?.needs_setup;
  // Public mode rides on the setup payload (#416) — this call already
  // happens first on every navigation, so the flag is available before
  // the gate below runs without adding a request to the hot path.
  // Absent or unreadable reads as OFF: the server enforces the setting
  // itself, so the worst a false here can do is send a visitor to the
  // sign-in page on an install that would have let them browse.
  const publicMode = !!setup?.public_mode;
  // Same channel, same reasoning (#712). Absent or unreadable reads as
  // OFF; POST /auth/register enforces the setting server-side and 403s
  // when it is off, so a false here can only send a would-be signup to
  // the sign-in page, never open a closed install.
  const selfRegistration = !!setup?.self_registration_enabled;

  if (needsSetup && url.pathname !== '/setup') {
    throw redirect(302, '/setup');
  }
  if (!needsSetup && url.pathname === '/setup') {
    throw redirect(302, '/');
  }

  // Resolve current session — also via load's fetch. The auth store
  // is populated from the response so component code reads from one
  // place. We deliberately avoid auth.refresh() here because that
  // path goes through openapi-fetch + global fetch, which trips the
  // SvelteKit hydration warning.
  if (!auth.ready) {
    const meRes = await fetch('/api/v1/auth/me').catch(() => null);
    if (meRes && meRes.ok) {
      const u = await meRes.json();
      auth.hydrateFrom(u);
    } else {
      auth.clear();
    }
    auth.markReady();
  }

  const isPublic =
    ALWAYS_PUBLIC_ROUTE_IDS.has(route.id ?? '') ||
    (publicMode && PUBLIC_MODE_ROUTE_IDS.has(route.id ?? '')) ||
    (selfRegistration && SELF_REGISTRATION_ROUTE_IDS.has(route.id ?? ''));
  if (!isPublic && !auth.user) {
    const next = url.pathname + url.search;
    const dest = next === '/' ? '/login' : `/login?next=${encodeURIComponent(next)}`;
    throw redirect(302, dest);
  }
  if (url.pathname === '/login' && auth.user) {
    throw redirect(302, '/');
  }

  return {
    needsSetup,
    publicMode,
    selfRegistration,
    authed: !!auth.user,
  };
}

// silence unused-import warning when used by routes only.
void api;
