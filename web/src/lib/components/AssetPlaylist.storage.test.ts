// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1255 — THE VIEWER SHELL RENDERS ON A BROWSER THAT BLOCKS SITE DATA.
//
// ⛔ WHAT THIS FILE ASSERTS IS THE RENDER, NOT THE ABSENCE OF A THROW.
//
// "It doesn't throw" is the weaker claim and it passes on code that
// swallows the exception and then renders nothing — which is exactly the
// shape of the bug: `AssetPlaylist`'s four `localStorage.getItem` calls
// ran inside `onMount`, so the first `SecurityError` escaped the mount
// and the shell never appeared. The user did not see a viewer with
// forgotten preferences; they saw no viewer. So every case below reaches
// into the rendered DOM for the DEFAULT state the shell is supposed to
// fall back to.
//
// Pinned in BOTH directions. "The strip is expanded when storage
// throws" passes on a component that ignores storage entirely, so the
// control case — a store that really does hold `'1'` — asserts the
// COLLAPSED render from the same code path. Only the pair distinguishes
// "falls back correctly" from "never reads at all".
//
// The third case is the one a read-only fix would have missed: a store
// whose reads work and whose WRITES throw. `$effect` writes
// `assetPlaylist.paneCollapsed` at mount, so an unguarded setItem takes
// the shell down on Safari's private window even with every read fixed.

import { fireEvent, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import AssetPlaylist from './AssetPlaylist.svelte';
import type { PlaylistItem, PlaylistSource } from '$lib/playlist/types';

const A1 = '3f1b8e2c-0000-4000-8000-0000000000e1';
const A2 = '3f1b8e2c-0000-4000-8000-0000000000e2';

function item(id: string): PlaylistItem {
  return {
    id,
    asset: {
      id,
      title: 'A picture',
      file_extension: 'png',
      file_hash: 'd'.repeat(64),
      asset_type: 1,
      metadata: null,
      preview_available: true,
    },
  };
}

/** Two items, so the thumb strip renders — it is the surface the
 *  stripCollapsed / stripHeight preferences drive, and a playlist of one
 *  hides it entirely. */
function source(): PlaylistSource {
  return {
    kind: 'post',
    id: 'p1',
    title: 'A set',
    items: [item(A1), item(A2)],
    cursor: 0,
    loading: false,
    error: null,
    removeItem: () => 1,
  };
}

/** Every method raises — a browser with site data blocked. */
function blockedStore() {
  const boom = () => {
    throw new DOMException('The operation is insecure.', 'SecurityError');
  };
  return { getItem: boom, setItem: boom, removeItem: boom, clear: boom, key: boom, length: 0 };
}

/** Reads answer, writes raise — Safari's private window. */
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

/** happy-dom has no `<dialog>` modal behaviour; the shell calls one of
 *  these on mount, so without the stubs nothing renders for any reason
 *  and the storage assertions would be vacuous. */
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.show = function show(this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement) {
    this.open = false;
  };
});

afterEach(() => {
  vi.unstubAllGlobals();
});

const shell = (c: HTMLElement) => c.querySelector<HTMLElement>('[data-testid="asset-playlist"]');
/** The strip's collapse chevron reports the strip's state on itself. */
const stripToggle = (c: HTMLElement) =>
  [...c.querySelectorAll<HTMLElement>('button[aria-expanded]')].at(-1) ?? null;
/** The wrapper the strip height is written onto — the chevron's parent. */
const stripBox = (c: HTMLElement) => stripToggle(c)?.parentElement ?? null;

describe('AssetPlaylist — storage that throws', () => {
  it('⭐ RENDERS THE SHELL, with the strip at its default height', () => {
    vi.stubGlobal('localStorage', blockedStore());
    const c = render(AssetPlaylist, { source: source(), onClose: () => {} }).container;

    // The shell is on the page at all — the assertion the old code
    // fails, because its `onMount` never finished.
    expect(shell(c)).toBeTruthy();
    // ...and it is on its DEFAULTS: the strip is expanded, at 96px.
    expect(stripToggle(c)!.getAttribute('aria-expanded')).toBe('true');
    expect(stripBox(c)!.getAttribute('style')).toContain('height: 96px');
    // The right pane is open, which is what `paneCollapsed = false`
    // renders — the nav arrows sit clear of it.
    expect(c.querySelector('.right-\\[25rem\\]')).toBeTruthy();
  });

  it('⭐ RENDERS THE SHELL when reads work and WRITES throw', () => {
    // The half-available store. Fixing only the four reads leaves the
    // mount-time `$effect` write to take the shell down here.
    vi.stubGlobal('localStorage', readOnlyStore());
    const c = render(AssetPlaylist, { source: source(), onClose: () => {} }).container;
    expect(shell(c)).toBeTruthy();
    expect(stripToggle(c)!.getAttribute('aria-expanded')).toBe('true');
  });

  it('⭐ the control: a store that ANSWERS still restores the preference', () => {
    // Without this the suite passes on a component that never reads
    // storage at all, which is a different bug wearing the same green.
    vi.stubGlobal(
      'localStorage',
      readOnlyStore({
        'assetPlaylist.stripCollapsed': '1',
        'assetPlaylist.paneCollapsed': '1',
        'assetPlaylist.stripHeight': '180',
      }),
    );
    const c = render(AssetPlaylist, { source: source(), onClose: () => {} }).container;
    expect(shell(c)).toBeTruthy();
    expect(stripToggle(c)!.getAttribute('aria-expanded')).toBe('false');
    // A collapsed strip carries no inline height, and the pane is shut.
    expect(stripBox(c)!.getAttribute('style') ?? '').not.toContain('height:');
    expect(c.querySelector('.right-3')).toBeTruthy();
  });

  it('a blocked store FORGETS rather than persisting — the control still works', async () => {
    // "Storage blocked ⇒ the control works for the session and forgets."
    // The toggle flips the rendered state even though nothing can be
    // written, which is the whole degradation contract.
    vi.stubGlobal('localStorage', blockedStore());
    const c = render(AssetPlaylist, { source: source(), onClose: () => {} }).container;
    const toggle = stripToggle(c)!;
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    await fireEvent.click(toggle);
    expect(stripToggle(c)!.getAttribute('aria-expanded')).toBe('false');
  });
});
