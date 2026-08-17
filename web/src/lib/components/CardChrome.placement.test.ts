// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// WHERE THUMBNAIL'S CARD CONTROLS LIVE — the owner's ruling, pinned.
//
// This file exists because nothing pinned it. #1136 opened the top
// chrome band, #1144 narrowed what it said, #1158 fixed its order and
// #1171 tightened its spacing — four passes over the same strip of
// markup, and at no point could a test tell you which controls were
// supposed to be in it. The order was re-litigated twice in one week
// on screenshots alone, and a swap was written and then withdrawn.
//
// The ruling these tests hold to, in the owner's words: "I like the
// menu bottom right. Asset type icon and count top left and checkbox
// top right. The padding around the menu button should be smaller and
// rectangle instead of circle."
//
// Two properties are worth a test and the rest are pixels:
//
//   1. WHICH CONTAINER each control is in. The band holds the kind
//      badge and the checkbox; the ⋯ menu is at the end of the
//      metadata stack. That is the ruling, and it is structural.
//   2. THE MENU IS NOT INSIDE AN ANCHOR. This is the one that would
//      ship as a bug rather than as a wrong-looking card: the mock the
//      ruling arrived on nested the trigger inside the post link, which
//      is invalid HTML and navigates the card on the way to opening the
//      menu. A layout tweak that "just" moves the button back inside
//      the <a> would look right in a screenshot and be broken.
//
// The other densities are asserted UNCHANGED in the same breath. The
// ruling is about thumbnail's card layout; grid and masonry keep the
// overlay trigger they have always had, and this file is where a future
// pass finds out it reached too far.

import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import AssetCard from './AssetCard.svelte';
import PostCard from './PostCard.svelte';
import type { CardAsset } from './cardAsset';
import type { ViewMode } from '$stores/browseView.svelte';

const ASSET_ID = '3f1b8e2c-0000-4000-8000-0000000000a1';
const POST_ID = '3f1b8e2c-0000-4000-8000-0000000000b2';

function asset(overrides: Partial<CardAsset> = {}): CardAsset {
  return {
    id: ASSET_ID,
    title: 'Stocking render',
    asset_type: 1,
    created_at: '2026-08-01T12:00:00.000Z',
    file_hash: 'c'.repeat(64),
    file_extension: 'png',
    thumbhash: null,
    preview_available: true,
    ladder_available: true,
    scrub_available: false,
    pixel_width: 1200,
    pixel_height: 1600,
    ...overrides,
  };
}

/** A post holding `n` assets. One is the single-asset shape the
 *  extension rule turns on for; more than one is the set it turns off
 *  for. The members are real rows rather than a `memberCount` override
 *  so the fixture matches what a list endpoint actually ships. */
function post(n = 1) {
  return {
    id: POST_ID,
    title: n > 1 ? 'A set of three' : 'A single picture',
    created_at: '2026-08-01T12:00:00.000Z',
    like_count: 4,
    comment_count: 2,
    members: Array.from({ length: n }, (_, i) => ({
      asset_id: `3f1b8e2c-0000-4000-8000-00000000c${i}0`,
      asset: asset({ id: `3f1b8e2c-0000-4000-8000-00000000c${i}0` }),
    })),
  };
}

const assetCard = (mode: ViewMode) => render(AssetCard, { asset: asset(), mode }).container;
const postCard = (mode: ViewMode, n = 1) =>
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  render(PostCard, { post: post(n) as any, mode }).container;

const band = (c: HTMLElement) => c.querySelector<HTMLElement>('[data-testid="thumb-band-top"]');
const meta = (c: HTMLElement) => c.querySelector<HTMLElement>('[data-testid="thumb-metadata"]');
const triggers = (root: ParentNode) =>
  [...root.querySelectorAll<HTMLElement>('[data-testid="card-menu-trigger"]')];

for (const [name, card] of [
  ['AssetCard', assetCard],
  ['PostCard', postCard],
] as const) {
  describe(`${name} — thumbnail control placement`, () => {
    it('keeps the kind badge in the band', () => {
      const c = card('thumbnail');
      // `card-kind` for a single item, `card-kind-multi` for a set —
      // one badge either way, and it is the band's LEFT-most child.
      const badge = band(c)!.querySelector('[data-testid^="card-kind"]');
      expect(badge).toBeTruthy();
      expect(band(c)!.firstElementChild!.contains(badge)).toBe(true);
    });

    it('has NO ⋯ menu in the top band', () => {
      // The ruling's actual change. The band is content + the checkbox;
      // the action menu left it.
      expect(triggers(band(card('thumbnail'))!)).toHaveLength(0);
    });

    it('puts exactly one ⋯ menu in the metadata stack', () => {
      const found = triggers(meta(card('thumbnail'))!);
      expect(found).toHaveLength(1);
    });

    it('renders the ⋯ menu OUTSIDE every anchor', () => {
      // The half of the ruling that is correctness rather than taste:
      // a <button> inside an <a> is invalid content and would fire the
      // card's navigation before the menu could open.
      const trigger = triggers(meta(card('thumbnail'))!)[0];
      expect(trigger.closest('a')).toBeNull();
    });

    it('draws the ⋯ menu on a RECTANGULAR plate, not a disc', () => {
      // jsdom has no layout, so the assertable fact is the shape the
      // component asks for. `rounded-full` was the old inline chip.
      const plate = triggers(meta(card('thumbnail'))!)[0].querySelector('span')!;
      expect(plate.className).toContain('rounded-md');
      expect(plate.className).not.toContain('rounded-full');
    });

    it('ends the metadata stack with the row the ⋯ menu is on', () => {
      // The trigger belongs to the LAST row, not to some row in the
      // middle — "menu bottom right" is a position, and a stack that
      // grows a new row underneath it would quietly break the ruling.
      const m = meta(card('thumbnail'))!;
      expect(triggers(m.lastElementChild!)).toHaveLength(1);
    });

    it('leaves grid and masonry on the overlay trigger', () => {
      // Scope guard. The ruling names thumbnail's card layout; the
      // other densities own their own chrome and are not part of it.
      for (const mode of ['grid', 'masonry'] as const) {
        const c = card(mode);
        expect(band(c)).toBeNull();
        expect(meta(c)).toBeNull();
        const found = triggers(c);
        expect(found).toHaveLength(1);
        // The overlay keeps its disc — it sits over unknown artwork.
        expect(found[0].querySelector('span')!.className).toContain('rounded-full');
      }
    });
  });
}

// The two cards are NOT twins here, and that is the point of these
// tests rather than an inconsistency to be tidied away later.
//
// #1158 pulled the extension off both bands. The owner has since split
// the ruling by SURFACE: a post wall is discovery, where the icon
// already answers "what kind of thing is this?", and an asset wall is
// somebody's own uploads, where the file is the unit and "which of
// these is the TXT" is the question being asked. So the extension comes
// back on the asset band only, and the asset stack grows the date row
// the post stack always had.
describe('AssetCard — the asset band says which FILE', () => {
  it('shows the extension after the kind icon', () => {
    const c = assetCard('thumbnail');
    const ext = c.querySelector('[data-testid="thumb-band-extension"]');
    expect(ext).toBeTruthy();
    expect(ext!.textContent!.trim()).toBe('png');
    // Icon first, word second — the glyph is the notation and the
    // extension qualifies it.
    const badge = band(c)!.querySelector('[data-testid^="card-kind"]')!;
    expect(badge.compareDocumentPosition(ext!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('renders NOTHING rather than an empty slot with no extension', () => {
    const c = render(AssetCard, {
      asset: asset({ file_extension: '' }),
      mode: 'thumbnail' as ViewMode,
    }).container;
    expect(c.querySelector('[data-testid="thumb-band-extension"]')).toBeNull();
  });

  it('strips a leading dot', () => {
    const c = render(AssetCard, {
      asset: asset({ file_extension: '.txt' }),
      mode: 'thumbnail' as ViewMode,
    }).container;
    expect(c.querySelector('[data-testid="thumb-band-extension"]')!.textContent!.trim()).toBe('txt');
  });

  it('carries the date on the same row as the ⋯ menu', () => {
    const c = assetCard('thumbnail');
    const date = c.querySelector('[data-testid="card-date"]');
    expect(date).toBeTruthy();
    expect(date!.textContent!.trim()).toBeTruthy();
    // One row, two siblings — and the date's link is not the menu's
    // parent, which is the invalid-nesting trap this file exists for.
    const row = meta(c)!.lastElementChild!;
    expect(row.contains(date!)).toBe(true);
    expect(triggers(row)).toHaveLength(1);
    expect(triggers(row)[0].closest('a')).toBeNull();
  });

  it('keeps the band facts off grid, which has no band at all', () => {
    expect(assetCard('grid').querySelector('[data-testid="thumb-band-extension"]')).toBeNull();
    expect(assetCard('grid').querySelector('[data-testid="card-date"]')).toBeNull();
  });
});

// THE COUNT DECIDES, on a post card.
//
// The owner's refinement: "For thumbnails on posts with only one asset,
// it can show the extension, but not if there is more than one asset in
// the post." Both directions are pinned, because this rule is only
// meaningful as a pair — a test that a single-asset post shows "png"
// passes just as well on a card that shows it unconditionally, which is
// precisely the bug the ruling exists to prevent.
//
// The reason it is a count: on a set, the cover's extension is not the
// post's fact. A carousel of a PNG, a PSD and an MP4 labelled "png" by
// whichever member happens to be the cover says something false about
// the other two — the failure #1111 named when it made the badge state
// the SET rather than any one member's kind.
describe('PostCard — the extension is a SINGLE-asset fact', () => {
  const ext = (c: HTMLElement) => c.querySelector('[data-testid="thumb-band-extension"]');

  it('SHOWS the extension when the post holds exactly one asset', () => {
    const e = ext(postCard('thumbnail', 1));
    expect(e).toBeTruthy();
    expect(e!.textContent!.trim()).toBe('png');
  });

  it('HIDES it as soon as the post holds two', () => {
    expect(ext(postCard('thumbnail', 2))).toBeNull();
  });

  it('stays hidden on a larger set', () => {
    expect(ext(postCard('thumbnail', 5))).toBeNull();
  });

  it('leaves the multi-asset badge to say the count instead', () => {
    // What a set shows in place of the extension, so the "no extension"
    // assertion above is not passing on an empty band.
    const c = postCard('thumbnail', 3);
    expect(band(c)!.querySelector('[data-testid="card-kind-multi"]')).toBeTruthy();
    // ...and a single-asset post gets the plain kind glyph beside its
    // extension, never the multi badge.
    const one = postCard('thumbnail', 1);
    expect(band(one)!.querySelector('[data-testid="card-kind"]')).toBeTruthy();
    expect(band(one)!.querySelector('[data-testid="card-kind-multi"]')).toBeNull();
  });

  it('puts the extension after the icon, as the asset band does', () => {
    const c = postCard('thumbnail', 1);
    const badge = band(c)!.querySelector('[data-testid^="card-kind"]')!;
    expect(badge.compareDocumentPosition(ext(c)!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('shows no extension in grid or masonry, which have no band', () => {
    expect(ext(postCard('grid', 1))).toBeNull();
    expect(ext(postCard('masonry', 1))).toBeNull();
  });

  it('WITHHOLDS the extension when the cover is restricted', () => {
    // A withheld value has derived copies, and each one has to be
    // withheld too. The band already suppresses the kind badge for a
    // restricted cover; a card that hides the icon and then prints
    // "png" beside the gap has disclosed the thing it just withheld.
    const p = post(1);
    p.members[0] = { ...p.members[0], restricted: true } as (typeof p.members)[0];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const c = render(PostCard, { post: p as any, mode: 'thumbnail' as ViewMode }).container;
    expect(ext(c)).toBeNull();
    // The badge is gone too — i.e. the test is observing the restricted
    // branch and not simply a card that failed to render a band.
    expect(band(c)!.querySelector('[data-testid^="card-kind"]')).toBeNull();
  });
});
