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
