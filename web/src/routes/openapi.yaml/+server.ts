// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Serves the canonical OpenAPI spec to the in-app Scalar reference at
// /admin/integrations/api.
//
// The spec file is mounted into the web container at /opt/openapi.yaml
// (see docker-compose.yml). The SvelteKit dev server runs as Node so
// it can read the file directly — no Vite static-asset path is set up
// because the file lives outside /app on purpose (avoids creating a
// stub file in the bind-mount).
//
// In a production build of the SPA, the Go backend would expose the
// spec at /api/v1/openapi.yaml directly. This endpoint exists for the
// dev container; the page that consumes it picks the right URL.

import { error } from '@sveltejs/kit';
import { readFile } from 'node:fs/promises';
import type { RequestHandler } from './$types';

const SPEC_PATHS = [
  process.env.AA_OPENAPI_SPEC,
  '/opt/openapi.yaml',
  '../app/api/openapi.yaml',
].filter((p): p is string => !!p);

export const GET: RequestHandler = async () => {
  for (const path of SPEC_PATHS) {
    try {
      const body = await readFile(path, 'utf8');
      return new Response(body, {
        headers: {
          'content-type': 'application/yaml; charset=utf-8',
          'cache-control': 'no-cache',
        },
      });
    } catch {
      // try next candidate
    }
  }
  throw error(500, `OpenAPI spec not found. Tried: ${SPEC_PATHS.join(', ')}`);
};
