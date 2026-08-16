// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Whiteboard brush-pack registry tests. Focused on the pub/sub
// pattern + the API → internal shape converter — the tinted-stamp
// cache + the URL preloader use HTMLImageElement / canvas and need
// browser-mode vitest projects to test fully.
//
// CAUTION: the registry holds module-level state (the `packs` Map
// keeps the built-in pack between tests). Each test must clean up
// what it registered or the next test sees its leftovers.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  getBrushPack, getStamp, listBrushPacks,
  registerPack, registerPackFromAPI, unregisterPack,
  subscribeBrushPacks,
  type APIPack, type APIStamp,
} from './brushes';
import type { BrushPack, BrushStamp } from './types';

const TEST_PACK_ID = 'test:fixture-pack';

function fixtureStamp(id: string): BrushStamp {
  return {
    id,
    label: 'Fixture',
    // Test source — never actually rendered.
    source: 'test://fake',
    spacing: 0.1,
  };
}

function fixturePack(stamps: BrushStamp[]): BrushPack {
  return { id: TEST_PACK_ID, name: 'Fixture pack', stamps };
}

// ── The preloader must never touch the network ──────────────────
//
// `registerPackFromAPI` kicks off `preloadStamp` for every stamp, and
// that is a FIRE-AND-FORGET real `fetch()` at
// `/api/v1/brush-packs/stamps/<id>`. Nothing here awaits it, so nothing
// here ever caught it: happy-dom resolves the relative URL against its
// default origin (`http://localhost:3000`), no server is listening, and
// the connection failure surfaced as an UNHANDLED `AggregateError:
// connect ECONNREFUSED ::1:3000 / 127.0.0.1:3000` printed alongside this
// file's results.
//
// It has been there since the preloader landed, on `dev` as well as on
// any branch, and sprint 21 recorded it as harmless post-summary noise.
// It is not harmless — it is an unhandled rejection racing the runner's
// own teardown (`AsyncTaskManager.abort` shows up in the same trace), so
// whether it lands as noise or as a fatal depends on timing the suite
// does not control. That is a landmine, and the cost of removing it is
// these ten lines.
//
// `preloadStamp` swallows the REJECTION it awaits (it logs "stamp
// preload error"), which is why the fix is not another try/catch there:
// the escaping error is the socket's, raised outside the awaited promise.
// The only reliable answer is to not open the socket. Stubbing here
// rather than in a shared setup file keeps it next to the one call that
// needs it, and keeps the assertion below — that `source` is still a
// plain URL string — honest.
beforeEach(() => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(new Response(new Blob(), { status: 200 }))),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
  // Drop the fixture pack so the next test starts from the builtin
  // baseline. Safe to call when the pack was never registered.
  unregisterPack(TEST_PACK_ID);
});

describe('registry', () => {
  it('ships the built-in pack out of the box', () => {
    const packs = listBrushPacks();
    expect(packs.length).toBeGreaterThan(0);
    const builtin = packs.find((p) => p.id === 'builtin');
    expect(builtin).toBeDefined();
    // The built-in pack carries soft-round + hard-round at minimum.
    expect(builtin?.stamps.find((s) => s.id === 'builtin:soft-round')).toBeDefined();
    expect(builtin?.stamps.find((s) => s.id === 'builtin:hard-round')).toBeDefined();
  });

  it('registerPack adds the pack + every stamp to the lookup', () => {
    const pack = fixturePack([fixtureStamp('test:a'), fixtureStamp('test:b')]);
    registerPack(pack);
    expect(getBrushPack(TEST_PACK_ID)).toEqual(pack);
    expect(getStamp('test:a')).toBeDefined();
    expect(getStamp('test:b')).toBeDefined();
  });

  it('unregisterPack drops the pack + its stamps', () => {
    registerPack(fixturePack([fixtureStamp('test:a')]));
    unregisterPack(TEST_PACK_ID);
    expect(getBrushPack(TEST_PACK_ID)).toBeUndefined();
    expect(getStamp('test:a')).toBeUndefined();
  });

  it('unregisterPack on an unknown id is a no-op', () => {
    // Must not throw + must not affect existing packs.
    const before = listBrushPacks().length;
    unregisterPack('no-such-pack');
    expect(listBrushPacks()).toHaveLength(before);
  });

  it('getStamp returns undefined for unknown ids', () => {
    expect(getStamp('no-such-stamp')).toBeUndefined();
  });
});

describe('pub/sub', () => {
  it('notifies subscribers on register + unregister', () => {
    const listener = vi.fn();
    const unsub = subscribeBrushPacks(listener);
    registerPack(fixturePack([fixtureStamp('test:a')]));
    unregisterPack(TEST_PACK_ID);
    expect(listener).toHaveBeenCalledTimes(2);
    unsub();
    // After unsubscribe, no more calls.
    registerPack(fixturePack([fixtureStamp('test:c')]));
    expect(listener).toHaveBeenCalledTimes(2);
  });

  it('unsubscribe handle is idempotent', () => {
    const listener = vi.fn();
    const unsub = subscribeBrushPacks(listener);
    unsub();
    unsub(); // must not throw
    registerPack(fixturePack([fixtureStamp('test:a')]));
    expect(listener).not.toHaveBeenCalled();
  });
});

describe('registerPackFromAPI', () => {
  // Shape conversion is what desyncs when the backend struct grows
  // a field — pin it so changes are explicit on both sides.
  let registered: BrushPack | undefined;
  beforeEach(() => {
    registered = undefined;
  });

  it('maps API snake_case fields to internal camelCase fields', () => {
    const api: APIPack = {
      id: TEST_PACK_ID,
      name: 'API pack',
      source_file: 'photoshop-set.abr',
      stamps: [{
        id: 'test:api-stamp',
        label: 'API stamp',
        width: 64,
        height: 64,
        spacing: 0.25,
        align_to_path: true,
        size_jitter: 0.1,
        opacity_jitter: 0.2,
        angle_jitter: 45,
      }],
    };
    registerPackFromAPI(api);
    registered = getBrushPack(TEST_PACK_ID);
    expect(registered).toBeDefined();
    expect(registered!.name).toBe('API pack');
    expect(registered!.stamps).toHaveLength(1);
    const s = registered!.stamps[0];
    expect(s.id).toBe('test:api-stamp');
    expect(s.label).toBe('API stamp');
    expect(s.spacing).toBe(0.25);
    expect(s.alignToPath).toBe(true);
    expect(s.sizeJitter).toBe(0.1);
    expect(s.opacityJitter).toBe(0.2);
    expect(s.angleJitter).toBe(45);
    // Source must be a string URL pointing at the stamp endpoint —
    // not a fetched Image yet (preload kicks off async).
    expect(typeof s.source).toBe('string');
    expect(s.source).toContain('/api/v1/brush-packs/stamps/test:api-stamp');
  });

  it('supplies sensible defaults for nullable API fields', () => {
    // `label` is the most visible one — a null label must not render
    // as `null` in the panel; the converter substitutes "Stamp".
    const api: APIPack = {
      id: TEST_PACK_ID,
      name: 'Minimal API pack',
      source_file: null,
      stamps: [{
        id: 'test:bare-stamp',
        label: null,
        width: 32,
        height: 32,
        spacing: 0.1,
        align_to_path: false,
        // Nullable jitter fields omitted.
      } as APIStamp],
    };
    registerPackFromAPI(api);
    const stamp = getStamp('test:bare-stamp');
    expect(stamp).toBeDefined();
    expect(stamp!.label).toBe('Stamp');
    expect(stamp!.sizeJitter).toBeUndefined();
    expect(stamp!.opacityJitter).toBeUndefined();
    expect(stamp!.angleJitter).toBeUndefined();
  });
});
