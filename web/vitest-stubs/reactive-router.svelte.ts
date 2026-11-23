// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// A reactive `$app/state` + `$app/navigation` pair for component tests
// whose component READS the URL it navigates to (the import history
// page keeps the selected receipt in `?receipt=`). The default stubs in
// app-state.ts and app-navigation.ts are deliberately inert; a test
// that needs the page to change opts in with:
//
//   vi.mock('$app/state', async () => await import('../../../../../vitest-stubs/reactive-router.svelte'));
//   vi.mock('$app/navigation', async () => await import('../../../../../vitest-stubs/reactive-router.svelte'));
//
// `goto` records each target and moves `page.url` there, which is all a
// component can observe of a client-side navigation.

class ReactivePage {
  url = $state(new URL('http://localhost/'));
  params = {} as Record<string, string>;
  route = { id: null as string | null };
  status = 200;
  error = null;
  data = {} as Record<string, unknown>;
  form = null;
  state = {} as Record<string, unknown>;
}

export const page = new ReactivePage();
export const navigating = null;
export const updated = { current: false };

export const visits: string[] = [];

export function resetRouter(href: string): void {
  visits.length = 0;
  page.url = new URL(href);
}

export const goto = async (url: string | URL, _opts?: unknown): Promise<void> => {
  const next = new URL(String(url), page.url);
  visits.push(next.pathname + next.search);
  page.url = next;
};
export const invalidate = async (_dep?: unknown): Promise<void> => {};
export const invalidateAll = async (): Promise<void> => {};
export const preloadData = async (_href: string): Promise<void> => {};
export const preloadCode = async (..._urls: string[]): Promise<void> => {};
export const beforeNavigate = (_fn: unknown): void => {};
export const afterNavigate = (_fn: unknown): void => {};
export const onNavigate = (_fn: unknown): void => {};
export const pushState = (_url: string | URL, _state: unknown): void => {};
export const replaceState = (_url: string | URL, _state: unknown): void => {};
