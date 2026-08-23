// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// WHICH SPEC LEFT THE ROW (#1247).
//
// # The gap this closes
//
// The corpus census in run-ui.sh reports drift per TABLE — "assets +54"
// — and #1247's repair plan assumed an attribution table underneath it
// that names the offending specs. There was none, and neither
// `aa sweep-fixtures` nor the census can produce one after the fact:
// the sweep's rule is collective (a row is a fixture or it is not; it
// does not know whose), and the census is two integers taken twenty
// minutes apart. So "fix a few of the ~25 leaking specs" had no way to
// choose which few.
//
// # Identity recorded at creation, not inferred later
//
// The alternative — read the leaked rows back afterwards and guess the
// spec from their titles — is the heuristic ADR 0095 exists to avoid,
// and it is unsound here for a specific reason: not every fixture
// carries a per-spec stamp, timestamps overlap under two workers, and a
// naming rule that is fine for a REPORT is the exact rule that nearly
// deleted five real assets when it was used for deletion (see
// fixturesweep/rules.go). Recording the creation as it happens is exact
// and needs no rule at all.
//
// # How it is universal without every spec opting in
//
// The leak always comes from the spec that did not remember to opt in,
// so this cannot be something a spec calls. It hangs off the two
// fixtures every spec already uses — `request` and `page` — and watches
// their traffic. A spec creates a row by POSTing to the API, whether it
// does that directly or by driving a form; both go past here.
//
// The record is append-only JSONL, one line per create or delete, in the
// project's output dir. `fixture-ledger-report.mjs` nets creates against
// deletes per spec and prints the table.
//
// # What it deliberately does NOT do
//
// It never deletes anything, and nothing consumes it as a deletion rule.
// Attribution may use names and identity; deletion may not.
//
// # ⛔ THE ONE THING IT CANNOT SEE, and why the census stays
//
// It records what the browser REPORTED. A request still in flight when
// the page is torn down is never reported, so a spec that ends while its
// own creation is on the wire is invisible here — and will be reported
// as having cleaned up everything it made, because as far as this file
// knows it made nothing.
//
// That is not hypothetical. ui-14-upload-flow's AI-control case picked a
// file (which the modal uploads immediately) and asserted only on a
// control, so it raced its own `POST /api/v1/assets`: sometimes the row
// landed, sometimes the page closed first. The run where it landed
// showed `assets +1` in the corpus census and "every spec removed
// everything it created" here, in the same output.
//
// The census is what caught it, and that is the point of having both:
// this file answers WHOSE, the census answers WHETHER. When the two
// disagree, the census is right and the disagreement is itself the
// finding — a spec that ends with writes outstanding.

import { appendFileSync, mkdirSync } from 'node:fs';
import { dirname, basename, join } from 'node:path';
import { test as pwTest } from '@playwright/test';
import type { APIResponse, Browser, BrowserContext, Page, TestInfo } from '@playwright/test';

/** The tables the corpus census counts, keyed by the route that fills them. */
const CREATE_ROUTES: ReadonlyArray<{ re: RegExp; table: string }> = [
  { re: /^\/api\/v1\/assets$/, table: 'assets' },
  { re: /^\/api\/v1\/posts$/, table: 'posts' },
  { re: /^\/api\/v1\/collections$/, table: 'collections' },
  { re: /^\/api\/v1\/fields$/, table: 'field_definition' },
  { re: /^\/api\/v1\/auth\/register$/, table: 'user' },
  { re: /^\/api\/v1\/admin\/users$/, table: 'user' },
];

const DELETE_ROUTES: ReadonlyArray<{ re: RegExp; table: string }> = [
  { re: /^\/api\/v1\/assets\/[^/]+$/, table: 'assets' },
  { re: /^\/api\/v1\/posts\/[^/]+$/, table: 'posts' },
  { re: /^\/api\/v1\/collections\/[^/]+$/, table: 'collections' },
  { re: /^\/api\/v1\/fields\/[^/]+$/, table: 'field_definition' },
];

export interface LedgerIdent {
  spec: string;
  title: string;
  file: string;
}

let ledgerFile: string | undefined;

/** Absolute path of the ledger, derived from the config root so it does
 *  not depend on the caller's cwd. Takes a TestInfo or a WorkerInfo —
 *  both carry `config.rootDir`.
 *
 *  ⛔ NOT under `.pw-results`. That is Playwright's `outputDir`, and
 *  Playwright DELETES it at the start of every run — proved with a
 *  canary file, which did not survive. It happens to be harmless for a
 *  single run (the wipe precedes the first test) and it silently
 *  destroyed the ledger of every run but the last when three ran back to
 *  back. A file whose whole job is to be complete does not live in a
 *  directory somebody else empties. */
export function ledgerPath(info: { config: { rootDir: string } }): string {
  if (!ledgerFile) {
    ledgerFile =
      process.env.AA_FIXTURE_LEDGER ??
      join(info.config.rootDir, '.pw-artifacts', 'fixture-ledger.jsonl');
  }
  return ledgerFile;
}

export function identFor(testInfo: TestInfo): LedgerIdent {
  return {
    spec: basename(testInfo.file ?? 'unknown'),
    title: testInfo.titlePath?.slice(1).join(' › ') || testInfo.title || '(hook)',
    file: testInfo.file ?? '',
  };
}

/**
 * Who is running RIGHT NOW.
 *
 * Contexts made inside a test (`browser.newContext()`) outlive no test
 * but belong to whichever one made them, and the `browser` fixture that
 * hands them out is worker-scoped — so the spec cannot be captured when
 * the watch is installed. It is read at the moment of the write instead.
 * `test.info()` throws outside a running test, which is what `fallback`
 * is for.
 */
function identNow(fallback: LedgerIdent): LedgerIdent {
  try {
    const info = pwTest.info();
    return info ? identFor(info) : fallback;
  } catch {
    return fallback;
  }
}

function write(file: string, row: Record<string, unknown>): void {
  try {
    mkdirSync(dirname(file), { recursive: true });
    // One JSON object per line, opened O_APPEND: short lines from
    // several worker PROCESSES interleave without tearing.
    appendFileSync(file, `${JSON.stringify(row)}\n`);
  } catch {
    // The ledger is a diagnostic. Never fail a test because it could
    // not be written.
  }
}

function classify(method: string, urlStr: string): { op: string; table: string } | undefined {
  let path: string;
  try {
    path = new URL(urlStr, 'http://x').pathname;
  } catch {
    return undefined;
  }
  if (method === 'POST') {
    const hit = CREATE_ROUTES.find((r) => r.re.test(path));
    return hit ? { op: 'create', table: hit.table } : undefined;
  }
  if (method === 'DELETE') {
    const hit = DELETE_ROUTES.find((r) => r.re.test(path));
    return hit ? { op: 'delete', table: hit.table } : undefined;
  }
  return undefined;
}

function idFromPath(urlStr: string): string | undefined {
  try {
    const parts = new URL(urlStr, 'http://x').pathname.split('/');
    return parts[parts.length - 1] || undefined;
  } catch {
    return undefined;
  }
}

async function idFromBody(read: () => Promise<unknown>): Promise<string | undefined> {
  try {
    const body = (await read()) as Record<string, unknown> | undefined;
    const id = body?.id ?? body?.ref;
    return id === undefined || id === null ? undefined : String(id);
  } catch {
    return undefined;
  }
}

async function record(
  file: string,
  ident: LedgerIdent,
  via: 'api' | 'page',
  method: string,
  url: string,
  status: number,
  readBody: () => Promise<unknown>,
): Promise<void> {
  const what = classify(method, url);
  if (!what) return;
  // A create that failed created nothing; a delete that failed deleted
  // nothing. Only successes are recorded, so the net is the net.
  //
  // ⛔ 404 IS NOT A DELETE, and treating it as one was a real mistake in
  // the first version. A `DELETE` that 404s removed nothing — the row is
  // either already gone (in which case the call that removed it was
  // recorded) or invisible to that caller (in which case it is still
  // there). Crediting it either way lets a spec book a cleanup it did
  // not perform, which is the exact failure this file exists to catch.
  if (!(status >= 200 && status < 300)) return;

  const id = what.op === 'create' ? await idFromBody(readBody) : idFromPath(url);
  write(file, {
    ts: new Date().toISOString(),
    spec: ident.spec,
    test: ident.title,
    via,
    op: what.op,
    table: what.table,
    id: id ?? null,
    status,
  });
}

const watchedContexts = new WeakSet<object>();

/**
 * Watch an APIRequestContext's writes.
 *
 * Patches the instance rather than wrapping the fixture, the same shape
 * helpers/test.ts already uses for `page.goto` — a wrapper would have to
 * re-create the context and re-apply its storageState, and the fixture's
 * own disposal semantics with it.
 *
 * ONLY `fetch` is patched, and that is not a shortcut: every other verb
 * on APIRequestContext is `fetch(url, {...options, method})` (Playwright
 * 1.62, coreBundle.js — `get`, `post`, `put`, `patch`, `delete` and
 * `head` all delegate). Patching `post` as well recorded every creation
 * TWICE, because the wrapped `post` called the wrapped `fetch`.
 */
export function watchRequestContext(
  request: { fetch: Function },
  ident: LedgerIdent,
  file: string,
): void {
  if (watchedContexts.has(request)) return;
  watchedContexts.add(request);

  const orig = request.fetch.bind(request);
  request.fetch = async (url: string, options?: { method?: string }) => {
    const res: APIResponse = await orig(url, options);
    const method = (options?.method ?? 'GET').toUpperCase();
    await record(
      file,
      identNow(ident),
      'api',
      method,
      res.url(),
      res.status(),
      () => res.json(),
    ).catch(() => undefined);
    return res;
  };
}

/**
 * Watch what the BROWSER creates — a spec that drives the upload modal
 * or the create form leaks exactly the same rows as one that POSTs
 * directly, and used to be invisible to any per-spec accounting.
 */
export function watchPage(page: Page, ident: LedgerIdent, file: string): void {
  if (watchedContexts.has(page)) return;
  watchedContexts.add(page);
  page.on('response', (res) => {
    const method = res.request().method().toUpperCase();
    if (method !== 'POST' && method !== 'DELETE') return;
    void record(
      file,
      identNow(ident),
      'page',
      method,
      res.url(),
      res.status(),
      () => res.json(),
    ).catch(() => undefined);
  });
}

/**
 * Watch a browser context: its API request context AND every page it
 * opens.
 *
 * ⛔ `page.request` IS NOT THE `request` FIXTURE and does not raise
 * `page.on('response')` either — it is the CONTEXT's APIRequestContext,
 * a third path entirely. Missing it made the first version of this
 * ledger report four clean specs as leaking: ui-39 and
 * upload-visibility-1240 delete their fixtures through `page.request`,
 * so their creates were seen and their deletes were not. A ledger that
 * accuses a spec that cleaned up is worse than no ledger, because
 * somebody then "fixes" a teardown that was already right.
 */
export function watchContext(context: BrowserContext, ident: LedgerIdent, file: string): void {
  if (watchedContexts.has(context)) return;
  watchedContexts.add(context);
  watchRequestContext(context.request as unknown as { fetch: Function }, ident, file);
  for (const p of context.pages()) watchPage(p, ident, file);
  context.on('page', (p) => watchPage(p, ident, file));
}

/**
 * Watch every context a spec opens for itself.
 *
 * `browser.newContext()` is how a spec gets a second principal — an
 * anonymous visitor, a grantee — and those contexts create and delete
 * rows like any other. The browser is worker-scoped, so the spec that
 * owns a given context is only knowable when the write happens; see
 * identNow().
 */
export function watchBrowser(browser: Browser, ident: LedgerIdent, file: string): void {
  if (watchedContexts.has(browser)) return;
  watchedContexts.add(browser);

  const origContext = browser.newContext.bind(browser);
  browser.newContext = async (...args: Parameters<Browser['newContext']>) => {
    const ctx = await origContext(...args);
    watchContext(ctx, ident, file);
    return ctx;
  };

  const origPage = browser.newPage.bind(browser);
  browser.newPage = async (...args: Parameters<Browser['newPage']>) => {
    const page = await origPage(...args);
    watchContext(page.context(), ident, file);
    return page;
  };
}
