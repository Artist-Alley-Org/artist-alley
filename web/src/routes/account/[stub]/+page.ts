import { error } from '@sveltejs/kit';
import type { PageLoad } from './$types';
import { itemBySlug } from '$lib/account/sections';

export const load: PageLoad = ({ params }) => {
  const item = itemBySlug(params.stub);
  // Dynamic route catches anything not statically defined. We only
  // serve stubs for slugs known to our registry — bare typos return
  // a real 404 so the user understands they've wandered off-map.
  if (!item) {
    throw error(404, `Unknown account page: ${params.stub}`);
  }
  // 'live' items must have a static route — if we got here for a
  // live slug it means the static page is missing. Surface that as
  // a 404 too rather than silently falling back to the stub layout.
  if (item.status === 'live') {
    throw error(404, `${params.stub} is marked live but has no static page`);
  }
  return { item };
};
