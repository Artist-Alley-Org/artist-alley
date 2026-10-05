// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1255 — the accessor's contract, pinned against a THROWING store.
//
// The bug this module exists for is not "getItem returned null". It is
// that `localStorage.getItem` RAISES on a browser with site data
// blocked, so every assertion here stubs a store that throws rather than
// one that is merely empty — an empty store passes on the broken code.
//
// Both directions are pinned per accessor: the throwing store must
// produce the fallback, AND a working store must still produce the
// stored value, because "always return the fallback" passes the first
// half on its own.

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  readStored,
  readStoredJSON,
  removeStored,
  writeStored,
  writeStoredJSON,
} from './storage';

/** A store whose every method raises, which is what a browser with site
 *  data blocked actually does — `SecurityError` on access, not `null`. */
function blockedStore() {
  const boom = () => {
    throw new DOMException('The operation is insecure.', 'SecurityError');
  };
  return { getItem: boom, setItem: boom, removeItem: boom, clear: boom, key: boom, length: 0 };
}

/** Safari's private window, historically: reads work, writes throw.
 *  This is the half-available case a read-only fix would leave broken. */
function readOnlyStore(seed: Record<string, string> = {}) {
  return {
    getItem: (k: string) => (k in seed ? seed[k] : null),
    setItem: () => {
      throw new DOMException('QuotaExceededError', 'QuotaExceededError');
    },
    removeItem: () => {
      throw new DOMException('QuotaExceededError', 'QuotaExceededError');
    },
    clear: () => {},
    key: () => null,
    length: 0,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('readStored', () => {
  it('returns the stored string when the store works', () => {
    vi.stubGlobal('localStorage', readOnlyStore({ k: 'v' }));
    expect(readStored('k')).toBe('v');
  });

  it('returns the fallback — not a throw — when the store raises', () => {
    vi.stubGlobal('localStorage', blockedStore());
    expect(readStored('k')).toBeNull();
    expect(readStored('k', 'default')).toBe('default');
  });

  it('returns the fallback for an ABSENT key, which is a different fact', () => {
    vi.stubGlobal('localStorage', readOnlyStore({}));
    expect(readStored('k', 'default')).toBe('default');
  });

  it('⭐ keeps answering after one read throws', () => {
    // The plural case. Four reads run back to back in AssetPlaylist's
    // onMount; a helper that let the first exception escape would take
    // the other three — and the component — with it.
    vi.stubGlobal('localStorage', blockedStore());
    expect([
      readStored('a', 'A'),
      readStored('b', 'B'),
      readStored('c', 'C'),
      readStored('d', 'D'),
    ]).toEqual(['A', 'B', 'C', 'D']);
  });
});

describe('writeStored / removeStored', () => {
  it('writes through when the store works', () => {
    const seed: Record<string, string> = {};
    vi.stubGlobal('localStorage', {
      getItem: (k: string) => seed[k] ?? null,
      setItem: (k: string, v: string) => {
        seed[k] = v;
      },
      removeItem: (k: string) => {
        delete seed[k];
      },
    });
    writeStored('k', 'v');
    expect(seed.k).toBe('v');
    removeStored('k');
    expect('k' in seed).toBe(false);
  });

  it('⭐ swallows a write that cannot land, rather than raising', () => {
    // A control whose write is dropped still works for the session; a
    // control whose write THROWS takes the component down.
    vi.stubGlobal('localStorage', readOnlyStore());
    expect(() => writeStored('k', 'v')).not.toThrow();
    expect(() => removeStored('k')).not.toThrow();
  });
});

describe('readStoredJSON / writeStoredJSON', () => {
  it('round-trips a value through a working store', () => {
    const seed: Record<string, string> = {};
    vi.stubGlobal('localStorage', {
      getItem: (k: string) => seed[k] ?? null,
      setItem: (k: string, v: string) => {
        seed[k] = v;
      },
    });
    writeStoredJSON('k', { a: 1 });
    expect(readStoredJSON('k', null)).toEqual({ a: 1 });
  });

  it('falls back when the store raises', () => {
    vi.stubGlobal('localStorage', blockedStore());
    expect(readStoredJSON('k', 'fallback')).toBe('fallback');
    expect(() => writeStoredJSON('k', { a: 1 })).not.toThrow();
  });

  it('falls back on CORRUPT stored text rather than crashing the caller', () => {
    // A half-written entry is no more of an answer than an unreadable
    // store, and a component that threw on it would stay broken until
    // the reader cleared their site data by hand.
    vi.stubGlobal('localStorage', readOnlyStore({ k: '{not json' }));
    expect(readStoredJSON('k', 42)).toBe(42);
  });

  it('drops an UNSERIALISABLE value instead of raising', () => {
    const cyclic: Record<string, unknown> = {};
    cyclic.self = cyclic;
    vi.stubGlobal('localStorage', {
      getItem: () => null,
      setItem: () => {},
    });
    expect(() => writeStoredJSON('k', cyclic)).not.toThrow();
  });
});
