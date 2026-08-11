// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Static route/link-integrity check (#475, ADR 0068 layer 2).
//
// The class-level net for dead internal links: enumerate every literal
// internal href in web/src — including template-literal hrefs like
// `/assets/${id}` and Svelte attribute interpolation like
// `/assets/{asset.id}` — reduce each to its route SHAPE, and resolve it
// against the SvelteKit route table (src/routes/**/+page, honoring
// [param] / [...rest] / [[optional]] and route groups). Any target with
// no matching route is a dead link.
//
// This catches the entire dead-link CLASS with no running stack —
// #475 (the /assets/{id} tiles pointing at a route that didn't exist)
// is the instance it would have caught in seconds. It runs in CI as a
// plain vitest.

import { readFileSync } from 'node:fs';
import { relative } from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  buildRouteTable,
  buildStaticSet,
  normalizeShape,
  resolvable,
  srcDir,
  walk,
} from './routeTable';

// KNOWN_GAPS — pre-existing dead-link SHAPES that this net surfaced but
// that are NOT #475 and are out of scope for its fix. Each needs its
// own route (a real feature), tracked separately; listed here so the net
// ships LIVE and catches any NEW dead link (and proves #475 is fixed)
// without being held hostage to unrelated debt. Remove an entry the
// moment its route lands. This is the same ratchet the i18n-coverage
// test uses for partially-migrated surfaces.
//
// All three #475-era gaps have now landed as #478 (ADR 0070): the
// user-profile routes (/users/by-username/[username], /users/by-ref/[ref])
// in slice-1, and /posts/by-asset/[id] in slice-2. The set is empty — the
// net is zero-tolerance for every shape again.
const KNOWN_GAPS = new Set<string>([]);

// --- href extraction -----------------------------------------------------

interface Link {
  path: string;
  file: string;
}

// Patterns that yield an internal, `/`-rooted link target:
//   href="/..."            (incl. Svelte {interp}: href="/assets/{id}")
//   href='/...'
//   href={`/...`}          (template literal in an expression attribute)
//   href={"/..."}          (string in an expression attribute)
//   goto('/...') / goto("/...") / goto(`/...`)
const PATTERNS: RegExp[] = [
  /href\s*=\s*"(\/[^"]*)"/g,
  /href\s*=\s*'(\/[^']*)'/g,
  /href\s*=\s*\{\s*`(\/[^`]*)`\s*\}/g,
  /href\s*=\s*\{\s*['"](\/[^'"]*)['"]\s*\}/g,
  /goto\(\s*`(\/[^`]*)`/g,
  /goto\(\s*['"](\/[^'"]*)['"]/g,
];

function extractLinks(): Link[] {
  const links: Link[] = [];
  walk(srcDir, (file) => {
    if (!/\.(svelte|ts)$/.test(file)) return;
    if (file.endsWith('.test.ts') || file.endsWith('.spec.ts')) return;
    const text = readFileSync(file, 'utf8');
    const rel = relative(srcDir, file);
    for (const re of PATTERNS) {
      re.lastIndex = 0;
      let m: RegExpExecArray | null;
      while ((m = re.exec(text)) !== null) {
        links.push({ path: m[1], file: rel });
      }
    }
  });
  return links;
}

// Link targets that are not SvelteKit routes and are validated
// elsewhere: API endpoints (server, not client routes).
function isApiOrExternal(path: string): boolean {
  return path.startsWith('/api/');
}

describe('internal link integrity', () => {
  const routes = buildRouteTable();
  const staticSet = buildStaticSet();
  // Scanned ONCE for the whole describe, alongside the route table.
  //
  // extractLinks() reads every .svelte/.ts file under web/src, and it
  // was being called separately by the guard test and by the main
  // assertion — the same ~330 files walked and read twice per run, for
  // a result that cannot differ between the two calls. That second scan
  // was most of why the last test in this file sat on vitest's 5s
  // default and failed two runs in three (#934).
  const links = extractLinks();

  it('has a non-trivial route table and finds links (guards the guard)', () => {
    // If the scanner silently found nothing, every assertion below would
    // vacuously pass. Fail loudly instead.
    expect(routes.length).toBeGreaterThan(10);
    expect(routes).toContain('/');
    expect(links.length).toBeGreaterThan(20);
  });

  it('rejects a bogus route target (proves the matcher actually discriminates)', () => {
    // The robustness proof ADR 0068 asks for, encoded: a path with no
    // matching route must NOT resolve, and a known-good one must.
    expect(resolvable('/definitely/not/a/route', routes, staticSet)).toBe(false);
    expect(resolvable('/assets/deadbeef', routes, staticSet)).toBe(true);
    expect(resolvable('/', routes, staticSet)).toBe(true);
  });

  it('would flag the #475 dead link if the assets/[id] route were missing (red-first proof)', () => {
    // The regression this test exists for, encoded deterministically:
    // with /assets/[id] absent from the route table, the /assets/{id}
    // links AssetCard renders resolve to nothing. Rebuild the table
    // without that route and confirm the exact #475 target is flagged —
    // so the net is proven to catch #475, not merely to pass today.
    const without = routes.filter((r) => r !== '/assets/[id]');
    expect(resolvable('/assets/{asset.id}', without, staticSet)).toBe(false);
    // ...and adding it back resolves it (this is the fix).
    expect(resolvable('/assets/{asset.id}', routes, staticSet)).toBe(true);
  });

  it('every internal href resolves to a real route', () => {
    const dead: string[] = [];
    for (const { path, file } of links) {
      if (isApiOrExternal(path)) continue;
      if (resolvable(path, routes, staticSet)) continue;
      if (KNOWN_GAPS.has(normalizeShape(path))) continue; // pre-existing, tracked
      dead.push(`${path}   (in ${file})`);
    }
    expect(
      dead,
      `Internal links point at routes that do not exist. Add the route under ` +
        `src/routes/, or fix the href:\n  ${dead.join('\n  ')}`,
    ).toEqual([]);
  });
});
