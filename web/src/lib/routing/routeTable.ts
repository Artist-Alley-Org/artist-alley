// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// SvelteKit route-table reflection — TEST-ONLY helper.
//
// Imports node:fs, so it must never be imported from app code; it
// exists so more than one suite can resolve a link target against the
// real route tree without each re-implementing the walker.
//
// Consumers:
//   - src/lib/routing/link-integrity.test.ts  (#475, ADR 0068 layer 2)
//     every literal internal href in web/src resolves to SOME route
//   - src/lib/account/sections.test.ts         (#600)
//     every `status: 'live'` account tile resolves to a STATIC route
//     — i.e. is not silently being served by /account/[stub]
//
// The two questions are deliberately different. `resolvable()` accepts
// a dynamic route match ([param]); `hasStaticRoute()` does not. A tile
// marked live whose only match is /account/[stub] is exactly the bug
// class #600 documented, and only the stricter check catches it.

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, relative, sep } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
/** .../web */
export const webRoot = join(here, '..', '..', '..');
export const srcDir = join(webRoot, 'src');
export const routesDir = join(srcDir, 'routes');
export const staticDir = join(webRoot, 'static');

/** Sentinel standing in for an interpolated (dynamic) href segment. */
export const DYN = ' dyn';

export function walk(dir: string, onFile: (path: string) => void): void {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const st = statSync(full);
    if (st.isDirectory()) {
      if (entry === 'node_modules') continue;
      walk(full, onFile);
    } else {
      onFile(full);
    }
  }
}

// Every +page.svelte defines a navigable route. The directory path
// (minus route groups) is the route id, e.g.
//   routes/assets/[id]/+page.svelte  -> /assets/[id]
//   routes/+page.svelte              -> /
export function buildRouteTable(): string[] {
  const routes: string[] = [];
  walk(routesDir, (file) => {
    if (!file.endsWith(`${sep}+page.svelte`) && !file.endsWith('+page.svelte')) return;
    const relDir = relative(routesDir, dirname(file));
    const segs = relDir
      .split(sep)
      .filter((s) => s.length > 0)
      .filter((s) => !/^\(.*\)$/.test(s)); // drop route groups (group)
    routes.push('/' + segs.join('/'));
  });
  return routes;
}

/** Non-route link targets that still resolve, e.g. /favicon.svg. */
export function buildStaticSet(): Set<string> {
  const out = new Set<string>();
  try {
    walk(staticDir, (file) => {
      out.add('/' + relative(staticDir, file).split(sep).join('/'));
    });
  } catch {
    // no static dir — fine
  }
  return out;
}

// A route segment [param] / [...rest] / [[optional]] matches any single
// (or, for rest, remaining) href segment; a static segment must equal.
// A href segment carrying the DYN sentinel is an interpolated value and
// matches ONLY a dynamic route segment — so `/assets/${id}` matches
// `/assets/[id]` but never a static route.
export function routeMatches(routeId: string, hrefSegs: string[]): boolean {
  const rSegs = routeId === '/' ? [] : routeId.slice(1).split('/');
  let ri = 0;
  let hi = 0;
  while (ri < rSegs.length) {
    const r = rSegs[ri];
    if (/^\[\.\.\..+\]$/.test(r)) return true; // rest param eats the rest
    if (/^\[\[.+\]\]$/.test(r)) {
      // optional: try consuming one, else skip
      if (hi < hrefSegs.length) hi++;
      ri++;
      continue;
    }
    if (hi >= hrefSegs.length) return false;
    if (/^\[.+\]$/.test(r)) {
      ri++;
      hi++;
      continue; // dynamic matches any href segment, DYN included
    }
    // static route segment: the href segment must equal it AND must not
    // itself be dynamic (an interpolated segment can't satisfy a static
    // route).
    if (hrefSegs[hi].includes(DYN) || hrefSegs[hi] !== r) return false;
    ri++;
    hi++;
  }
  return hi === hrefSegs.length;
}

// normalizeShape reduces a raw href to its route shape. Collapse every
// interpolation — `${...}` (JS template) and `{...}` (Svelte attribute)
// — to the DYN sentinel FIRST, THEN strip the query/hash: an
// interpolation like `{author?.username ?? ''}` contains a `?` that must
// not be mistaken for the start of a query string.
export function normalizeShape(path: string): string {
  const noInterp = path.replace(/\$?\{[^}]*\}/g, DYN);
  return noInterp.split(/[?#]/)[0];
}

function segmentsOf(normalized: string): string[] {
  return normalized === '/'
    ? []
    : normalized.replace(/^\//, '').replace(/\/$/, '').split('/');
}

/** True when `path` resolves to ANY route (dynamic matches allowed) or
 *  to a file in static/. */
export function resolvable(path: string, routes: string[], staticSet: Set<string>): boolean {
  const normalized = normalizeShape(path);
  if (normalized === '') return true; // pure #anchor / ?query on current page
  const segs = segmentsOf(normalized);
  // Static asset target (only when fully literal).
  if (!normalized.includes(DYN) && staticSet.has(normalized)) return true;
  return routes.some((r) => routeMatches(r, segs));
}

/** True only when a route exists whose segments are ALL literal and
 *  equal `path`'s — i.e. the page is its own `+page.svelte`, not a
 *  `[param]` catch-all standing in for one. */
export function hasStaticRoute(path: string, routes: string[]): boolean {
  const normalized = normalizeShape(path).replace(/\/$/, '') || '/';
  if (normalized.includes(DYN)) return false;
  return routes.some((r) => !r.includes('[') && r === normalized);
}

/** The route(s) that would actually serve `path`, for failure messages. */
export function matchingRoutes(path: string, routes: string[]): string[] {
  const segs = segmentsOf(normalizeShape(path));
  return routes.filter((r) => routeMatches(r, segs));
}
