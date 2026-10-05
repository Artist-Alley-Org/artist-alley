// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Teardown for rows a REAL UPLOAD created (#1247).
//
// A spec that drives the upload modal never learns the id of what it
// made: the POST happens in the browser and the response goes to the
// app, not to the test. So specs that upload through the UI had no way
// to delete afterwards and simply did not — the fixture ledger named
// ui-35-open-vocabulary (3 assets a run) and vocabulary-extend-cap-789
// (2 a run) as the whole of the live asset leak.
//
// The id is taken from the modal's own `POST /api/v1/assets` response.
// That is identity recorded at CREATION: this spec deletes exactly the
// rows this spec's browser made, by id. It is not a naming rule, and
// deliberately so — a rule over titles is safe for a report and is the
// one that nearly deleted five real assets when it was used for
// deletion (fixturesweep/rules.go).
//
// Posts are tracked as well as assets because the same modal composes
// one when "create a post" is left ticked, and a leaked post sits at the
// head of the newest-first feed every later spec reads.

import type { APIRequestContext, Page } from '@playwright/test';

const ROUTES: ReadonlyArray<{ path: string; table: 'assets' | 'posts' }> = [
  { path: '/api/v1/assets', table: 'assets' },
  { path: '/api/v1/posts', table: 'posts' },
];

export interface UploadedRows {
  /** Start recording what this page creates. Safe to call per test. */
  watch(page: Page): void;
  /** Delete everything recorded so far and forget it. */
  cleanup(request: APIRequestContext): Promise<void>;
}

export function trackUploadedRows(): UploadedRows {
  // Promises, not ids: the body arrives asynchronously and `cleanup`
  // has to wait for the read rather than race it.
  let pending: Promise<{ table: string; id: string } | undefined>[] = [];

  return {
    watch(page: Page): void {
      page.on('response', (res) => {
        if (res.request().method().toUpperCase() !== 'POST') return;
        if (res.status() !== 201) return;
        let path: string;
        try {
          path = new URL(res.url()).pathname;
        } catch {
          return;
        }
        const hit = ROUTES.find((r) => r.path === path);
        if (!hit) return;
        pending.push(
          res
            .json()
            .then((b) => {
              const id = (b as { id?: string }).id;
              return id ? { table: hit.table, id } : undefined;
            })
            .catch(() => undefined),
        );
      });
    },

    async cleanup(request: APIRequestContext): Promise<void> {
      const rows = (await Promise.all(pending)).filter(Boolean) as {
        table: string;
        id: string;
      }[];
      pending = [];
      // Posts first, then the assets they held: a post delete leaves its
      // members standing, and the reverse order would leave a post
      // pointing at deleted files for as long as the loop runs.
      for (const table of ['posts', 'assets'] as const) {
        for (const row of rows.filter((r) => r.table === table)) {
          await request.delete(`/api/v1/${table}/${row.id}`).catch(() => undefined);
        }
      }
    },
  };
}

// There is deliberately no "did this actually track anything?" assertion
// here. A watcher wired to the wrong page records nothing and cleans up
// nothing, and silence would look like success — but the guard for that
// already exists and is better placed: the fixture ledger names the spec
// in the next run's attribution table the moment its teardown stops
// removing what it made.
