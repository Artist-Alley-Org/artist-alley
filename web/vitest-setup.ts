// Vitest setup — runs once before every test file.
//
// - jest-dom adds the expressive DOM matchers (`toBeInTheDocument`,
//   `toHaveTextContent`, ...) on top of the default vitest assertions.
// - happy-dom doesn't ship a matchMedia implementation; theme.svelte.ts
//   and a few other stores read it at import time, so we stub it here
//   to a permissive default (always non-matching, no listeners).
// - localStorage exists in happy-dom but starts fresh per file —
//   tests that pollute it must clean up themselves (or use beforeEach).

import '@testing-library/jest-dom/vitest';

if (typeof window !== 'undefined' && !window.matchMedia) {
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }),
  });
}
