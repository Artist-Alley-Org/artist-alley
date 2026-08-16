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

function post() {
  return {
    id: POST_ID,
    title: 'A set of three',
    created_at: '2026-08-01T12:00:00.000Z',
    like_count: 4,
    comment_count: 2,
    members: [{ asset_id: ASSET_ID, asset: asset() }],
  };
}

const assetCard = (mode: ViewMode) => render(AssetCard, { asset: asset(), mode }).container;
// eslint-disable-next-line @typescript-eslint/no-explicit-any
const postCard = (mode: ViewMode) => render(PostCard, { post: post() as any, mode }).container;

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
