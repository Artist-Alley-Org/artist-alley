// Vitest setup — runs once before every test file.
//
// - jest-dom adds the expressive DOM matchers (`toBeInTheDocument`,
//   `toHaveTextContent`, ...) on top of the default vitest assertions.
// - happy-dom doesn't ship a matchMedia implementation; theme.svelte.ts
//   and a few other stores read it at import time, so we stub it here
//   to a permissive default (always non-matching, no listeners).
// - localStorage exists in happy-dom but starts fresh per file —
//   tests that pollute it must clean up themselves (or use beforeEach).

/// <reference types="@testing-library/jest-dom" />
import '@testing-library/jest-dom/vitest';

// happy-dom returns null for canvas.getContext — that breaks any
// module that builds a Path2D or gradient at import time (the
// whiteboard built-in brush stamps do, since the soft-round stamp
// is pre-rendered into an offscreen canvas at module load). The
// stub here lets those modules load; tests that actually need real
// canvas pixel output should move to a browser-mode vitest project
// when we add one.
if (typeof HTMLCanvasElement !== 'undefined') {
  const fakeCtx = new Proxy(
    {},
    {
      get(_, prop) {
        if (prop === 'canvas') return null;
        // Methods commonly chained off the returned gradient / pattern.
        if (prop === 'createRadialGradient' || prop === 'createLinearGradient' || prop === 'createPattern') {
          return () => ({ addColorStop: () => {} });
        }
        // Default any method to a noop, any property read to 0/false.
        return () => undefined;
      },
    },
  );
  HTMLCanvasElement.prototype.getContext = (() => fakeCtx) as never;
}

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
