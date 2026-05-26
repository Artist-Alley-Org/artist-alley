// Universal load. SSR=false to keep everything client-rendered after
// prerendering — the Go binary serves a static SPA shell and the
// client takes over. Per-page +page.ts files can opt back into
// prerender content for SEO surfaces if/when we add them.
export const ssr = false;
export const prerender = true;
export const trailingSlash = 'never';
