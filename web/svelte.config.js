import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
const config = {
  preprocess: vitePreprocess(),

  kit: {
    // adapter-static — every route is prerendered to a static asset at
    // build time. The Go binary picks up `web/build/` via //go:embed
    // and serves it from /. Dynamic per-user data is fetched client-side
    // after hydration; the prerendered shell + theme + chrome paint
    // instantly. See ADR 0014.
    adapter: adapter({
      pages: 'build',
      assets: 'build',
      // SPA fallback — every unknown path hits index.html and the
      // client router takes over. Makes deep links work without a
      // server-side route table.
      fallback: 'index.html',
      precompress: false,
      strict: true,
    }),

    // Authenticated data is fetched client-side after hydration; no
    // route renders user-specific HTML at build time, so prerendering
    // everything is fine. Pages that need to opt out can set
    // `export const prerender = false` in their +page.ts.
    prerender: {
      handleHttpError: 'warn',
      // Dynamic routes (/posts/[id], /admin/[section], /account/[stub],
      // /collections/[id]) can't be crawled at build time because the
      // shell can't enumerate ids without auth. Skip them — the SPA
      // fallback serves index.html at runtime and the client router
      // resolves the dynamic segment.
      handleUnseenRoutes: 'ignore',
    },

    alias: {
      $api: 'src/lib/api',
      $components: 'src/lib/components',
      $stores: 'src/lib/stores',
    },
  },
};

export default config;
