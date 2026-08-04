// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Account-tile registry invariants (#600).
//
// Why this file exists — the failure it encodes:
//
// `status` on an ACCOUNT_ITEMS entry claims "a static +page.svelte
// answers this href". Nothing checked that claim. The ONLY consumer is
// /account/[stub]/+page.ts, which throws 404 for a `live` slug with no
// static page — but only when a human navigates to it. So three tiles
// carried a wrong label for a full release with no signal:
// `saved-searches` and `messages` were shipped, working pages still
// marked `stub`, and `/account/requests` was a finished page with no
// registry entry at all, which meant nothing in the app linked to it.
//
// The label being wrong in the harmless direction that time is luck.
// The same silence covers the harmful direction: mark a slug `live`
// before its page lands and the tile in the grid — which renders every
// item regardless of status — becomes a 404 nobody notices.
//
// So the invariant is asserted statically, against the real route tree,
// on every run. The sibling net in lib/routing/link-integrity.test.ts
// does NOT cover this: it scrapes `href="..."` attributes out of source,
// and these hrefs live in an object literal (`href: '/account/...'`)
// that is rendered through `href={item.href}`. Neither form matches.

import { describe, expect, it } from 'vitest';

import { ACCOUNT_GROUPS, ACCOUNT_ITEMS, itemBySlug, itemsByGroup } from './sections';
import enDict from '../i18n/en.json';
import { buildRouteTable, hasStaticRoute, matchingRoutes, resolvable } from '../routing/routeTable';

const routes = buildRouteTable();
const STUB_ROUTE = '/account/[stub]';

function lookup(dotted: string): unknown {
  return dotted
    .split('.')
    .reduce<unknown>(
      (acc, k) => (acc && typeof acc === 'object' ? (acc as Record<string, unknown>)[k] : undefined),
      enDict,
    );
}

describe('account section registry', () => {
  it('guards the guard: the route table is real and contains the account routes', () => {
    // Every assertion below is vacuous if the walker found nothing.
    expect(routes.length).toBeGreaterThan(10);
    expect(routes).toContain('/account');
    expect(routes).toContain(STUB_ROUTE);
    expect(ACCOUNT_ITEMS.length).toBeGreaterThan(10);
  });

  it('discriminates a static route from the [stub] catch-all', () => {
    // The robustness proof: `hasStaticRoute` must reject a slug that
    // only /account/[stub] can serve, or the invariant below passes for
    // everything and proves nothing.
    expect(hasStaticRoute('/account/profile', routes)).toBe(true);
    expect(hasStaticRoute('/account/bookmarks', routes)).toBe(false);
    // ...while the looser check (any route, dynamic included) accepts it,
    // because /account/[stub] does answer it.
    expect(resolvable('/account/bookmarks', routes, new Set())).toBe(true);
  });

  it("would flag a 'live' tile whose page is missing (red-first proof)", () => {
    // Encode the regression deterministically: drop /account/following
    // from the table and the entry that claims to be live must fail.
    const without = routes.filter((r) => r !== '/account/following');
    expect(hasStaticRoute('/account/following', without)).toBe(false);
    expect(hasStaticRoute('/account/following', routes)).toBe(true);
  });

  // THE invariant. A `live` tile must be answered by its own page, not
  // by the placeholder.
  it("every status:'live' item has a static route of its own", () => {
    const broken = ACCOUNT_ITEMS.filter((i) => i.status === 'live' && !hasStaticRoute(i.href, routes)).map(
      (i) => `${i.slug} -> ${i.href} (served by: ${matchingRoutes(i.href, routes).join(', ') || 'nothing'})`,
    );
    expect(
      broken,
      "These tiles are marked status:'live' but no static +page.svelte answers their href, so " +
        "/account/[stub] 404s them on click. Either add the page under src/routes, or set status:'stub'.\n  " +
        broken.join('\n  '),
    ).toEqual([]);
  });

  // The converse: a non-live tile must still land somewhere. A stub is
  // allowed to be served by /account/[stub] OR by a placeholder page of
  // its own (`ai` is the latter) — but never by nothing.
  it('every item resolves to some route', () => {
    const dead = ACCOUNT_ITEMS.filter((i) => !resolvable(i.href, routes, new Set())).map(
      (i) => `${i.slug} -> ${i.href}`,
    );
    expect(dead, `Account tiles point at hrefs with no route:\n  ${dead.join('\n  ')}`).toEqual([]);
  });

  // /account/[stub] resolves the placeholder by SLUG, not by href. A
  // stub whose href is nested (e.g. /account/preferences/ai) never
  // reaches it, so it needs a real page — which is the check above.
  // What this one catches is the reverse: a stub that DOES rely on the
  // catch-all must have href === /account/{slug}, or the catch-all
  // looks the wrong slug up.
  it('catch-all stubs have an href the catch-all can actually parse', () => {
    const mismatched = ACCOUNT_ITEMS.filter(
      (i) =>
        i.status !== 'live' &&
        !hasStaticRoute(i.href, routes) &&
        i.href !== `/account/${i.slug}`,
    ).map((i) => `${i.slug} -> ${i.href}`);
    expect(mismatched).toEqual([]);
  });

  it('every item has a group that exists and i18n title + blurb', () => {
    const groupIds = new Set(ACCOUNT_GROUPS.map((g) => g.id));
    const problems: string[] = [];
    for (const i of ACCOUNT_ITEMS) {
      if (!groupIds.has(i.group)) problems.push(`${i.slug}: unknown group '${i.group}'`);
      for (const suffix of ['title', 'blurb']) {
        const key = `account.items.${i.slug}.${suffix}`;
        if (typeof lookup(key) !== 'string') problems.push(`${i.slug}: missing i18n key ${key}`);
      }
    }
    for (const g of ACCOUNT_GROUPS) {
      const key = `account.groups.${g.id}.title`;
      if (typeof lookup(key) !== 'string') problems.push(`group ${g.id}: missing i18n key ${key}`);
      // A group with no items renders an empty heading in the grid.
      if (itemsByGroup(g.id).length === 0) problems.push(`group ${g.id}: has no items`);
    }
    expect(problems).toEqual([]);
  });

  it('slugs are unique and itemBySlug finds each one', () => {
    const slugs = ACCOUNT_ITEMS.map((i) => i.slug);
    expect(new Set(slugs).size).toBe(slugs.length);
    for (const s of slugs) expect(itemBySlug(s)?.slug).toBe(s);
    expect(itemBySlug('definitely-not-a-tile')).toBeUndefined();
  });

  it('links the resource-request page that shipped without a nav entry (#600)', () => {
    // The instance, pinned so a future tidy-up cannot silently orphan
    // /account/requests again.
    const requests = itemBySlug('requests');
    expect(requests).toBeDefined();
    expect(requests?.href).toBe('/account/requests');
    expect(requests?.status).toBe('live');
  });
});
