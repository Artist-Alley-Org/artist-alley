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

// ── No test opens a socket ───────────────────────────────────────
//
// THE DEFAULT `fetch` NEVER REACHES THE NETWORK, and this is a real bug
// fix rather than tidying.
//
// happy-dom resolves relative URLs against `http://localhost:3000`, and
// nothing is listening there during a unit run. Any component that
// fetches on mount therefore opened a socket that failed, and because
// the failure arrives on the SOCKET rather than in the awaited promise,
// it surfaced as an unhandled `AggregateError: connect ECONNREFUSED
// ::1:3000 / 127.0.0.1:3000` — printed AFTER the run summary, racing
// vitest's own teardown (`AsyncTaskManager.abort` appears in the same
// trace).
//
// Traced to source: `CardThumb.svelte` calls `previewLadder.init()` in
// an `$effect`, which GETs `/api/v1/previews` through openapi-fetch. So
// EVERY test that renders a card — CardRestricted, CardFallback,
// CardThumb.scrub, and anything mounting AssetCard — emitted it. It has
// been there since the ladder store landed and is present on `dev`.
//
// Sprint 21 recorded it as harmless post-summary noise. It is not: an
// unhandled rejection whose timing depends on how fast teardown wins is
// a coin flip, and a coin flip in CI is a flake waiting to be blamed on
// whatever branch is unlucky. Removing the socket removes the race.
//
// A 503 rather than a 200, deliberately: the components already have a
// "the ladder did not load" path (that is what they take today, via the
// network error), so this keeps their behaviour under test IDENTICAL
// while removing the connection attempt. A synthetic 200 would silently
// put every card test on a code path production rarely takes.
//
// Tests that need real responses stub `fetch` themselves with
// `vi.stubGlobal`, which overrides this and restores back to it on
// `vi.unstubAllGlobals()`. Nothing here prevents that.
if (typeof globalThis.fetch !== 'undefined') {
  globalThis.fetch = (() =>
    Promise.resolve(
      new Response('{}', {
        status: 503,
        headers: { 'content-type': 'application/json' },
      }),
    )) as typeof globalThis.fetch;
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
