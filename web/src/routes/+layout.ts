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
// Public routes (/login, /setup) skip the auth requirement; the
// gate only applies to everything else.

import { redirect } from '@sveltejs/kit';
import { api } from '$api/client';
import { auth } from '$stores/auth.svelte';
import { browser } from '$app/environment';

// Routes that are reachable without a session.
const PUBLIC_ROUTES = new Set(['/login', '/setup']);

export const ssr = false;
export const prerender = true;
export const trailingSlash = 'never';

export async function load({ url, fetch }) {
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

  const isPublic = PUBLIC_ROUTES.has(url.pathname);
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
    authed: !!auth.user,
  };
}

// silence unused-import warning when used by routes only.
void api;
