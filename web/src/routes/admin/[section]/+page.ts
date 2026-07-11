// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

import { error } from '@sveltejs/kit';
import type { PageLoad } from './$types';
import { sectionBySlug } from '$lib/admin/sections';

export const load: PageLoad = ({ params }) => {
  const section = sectionBySlug(params.section);
  if (!section) {
    throw error(404, `Unknown admin section: ${params.section}`);
  }
  return { section };
};
