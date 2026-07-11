// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Thin wrapper around openapi-fetch that pins the artist-alley base
// URL and forwards the session cookie. Component code imports `api`
// from here and gets fully typed paths + methods.
//
// In dev, Vite proxies /api/v1 to the Go binary (see vite.config.ts).
// In prod, the Go binary serves /api/v1 directly alongside the
// embedded static frontend, so the same relative base URL works.

import createClient from 'openapi-fetch';
import type { paths } from './schema';

export const api = createClient<paths>({
  baseUrl: '/api/v1',
  credentials: 'same-origin',
});

export type { paths } from './schema';
