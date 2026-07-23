// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Startup-resilience helpers for the setup project (#527 fold).
//
// The `setup` project is a dependency of the WHOLE standalone suite:
// if its handful of raw API calls hit the app a beat before it's
// listening — or catch a single transient blip under CI startup load
// (ECONNRESET / ECONNREFUSED / a socket timeout / "Request context
// disposed") — every downstream test fails as a cascade. These helpers
// make that setup wait for readiness and tolerate a transient blip
// instead of turning one hiccup into a red suite.

import type { APIRequestContext } from '@playwright/test';

// Transient, retry-worthy failures. Connection-level errors that
// Playwright's request context throws as exceptions (not HTTP statuses)
// when the app isn't answering yet or a socket drops mid-flight.
const TRANSIENT =
  /ECONNRESET|ECONNREFUSED|ETIMEDOUT|EPIPE|socket hang up|Request context disposed|context or browser has been closed|Timeout .*exceeded|net::ERR/i;

function isTransient(err: unknown): boolean {
  const msg = err instanceof Error ? err.message : String(err);
  return TRANSIENT.test(msg);
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

/**
 * Run `fn`, retrying only on transient connection blips with a short
 * linear backoff. Non-transient failures (a real 4xx assertion, a bug)
 * throw immediately on the first attempt — retries tolerate the
 * transient, never the persistent.
 */
export async function withTransientRetry<T>(
  label: string,
  fn: () => Promise<T>,
  opts: { attempts?: number; baseDelayMs?: number } = {},
): Promise<T> {
  const attempts = opts.attempts ?? 5;
  const base = opts.baseDelayMs ?? 500;
  let lastErr: unknown;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      return await fn();
    } catch (err) {
      lastErr = err;
      if (!isTransient(err) || attempt === attempts) throw err;
      // eslint-disable-next-line no-console
      console.warn(
        `[setup] ${label}: transient failure (attempt ${attempt}/${attempts}), retrying — ${
          err instanceof Error ? err.message : String(err)
        }`,
      );
      await sleep(base * attempt);
    }
  }
  throw lastErr;
}

/**
 * Poll a cheap public endpoint until the app answers 200, bounded by
 * `timeoutMs`. Returns once ready; throws with the last-seen status /
 * error if the deadline passes.
 *
 * Probes `/api/v1/appearance` — the unauthenticated boot config the
 * frontend fetches on first paint. Chosen over `/healthz` because it's
 * reachable in BOTH suite shapes: through Vite's `/api` proxy locally
 * and directly on the embedded prod build in CI. (`/healthz` lives at
 * the app root, which Vite doesn't proxy.) A 200 here means the HTTP
 * server, routing, and the config DB read are all live — enough to log
 * in against.
 */
export async function waitForAppReady(
  request: APIRequestContext,
  opts: { timeoutMs?: number; intervalMs?: number } = {},
): Promise<void> {
  const timeoutMs = opts.timeoutMs ?? 30_000;
  const interval = opts.intervalMs ?? 750;
  const deadline = Date.now() + timeoutMs;
  let last = 'no response yet';
  let probes = 0;
  while (Date.now() < deadline) {
    probes++;
    try {
      const res = await request.get('/api/v1/appearance', { timeout: 5_000 });
      if (res.ok()) return;
      last = `HTTP ${res.status()}`;
    } catch (err) {
      last = err instanceof Error ? err.message : String(err);
    }
    await sleep(interval);
  }
  throw new Error(
    `app not ready after ${timeoutMs}ms (${probes} probes; last: ${last})`,
  );
}
