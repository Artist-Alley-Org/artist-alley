// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Test stub for SvelteKit's `$app/navigation`. See app-state.ts for the
// rule these stubs follow and why PostCard needs them.
//
// `goto` RESOLVES AND DOES NOTHING, which is the correct behaviour for
// a suite with no router: the component's job is to call it with the
// right target, and asserting the URL it would have gone to is a
// browser test's business. Throwing here would fail component tests
// for exercising a code path they legitimately reach; navigating for
// real is impossible without the kit.

export const goto = async (_url: string | URL, _opts?: unknown): Promise<void> => {};
export const invalidate = async (_dep?: unknown): Promise<void> => {};
export const invalidateAll = async (): Promise<void> => {};
export const preloadData = async (_href: string): Promise<void> => {};
export const preloadCode = async (..._urls: string[]): Promise<void> => {};
export const beforeNavigate = (_fn: unknown): void => {};
export const afterNavigate = (_fn: unknown): void => {};
export const onNavigate = (_fn: unknown): void => {};
export const pushState = (_url: string | URL, _state: unknown): void => {};
export const replaceState = (_url: string | URL, _state: unknown): void => {};
