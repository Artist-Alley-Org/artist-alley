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

/** A post holding `n` assets, all of them PNGs — the uniform shape.
 *  The members are real rows rather than a `memberCount` override so the
 *  fixture matches what a list endpoint actually ships. */
function post(n = 1) {
  return postOf(Array.from({ length: n }, () => 'png'));
}

/** A post whose members carry exactly these extensions, in order.
 *  `null` plants a RESTRICTED member — the #883 placeholder shape, with
 *  no `asset` at all — which is what a member this reader may not see
 *  actually looks like on the wire. */
function postOf(exts: Array<string | null>) {
  return {
    id: POST_ID,
    title: exts.length > 1 ? 'A set of three' : 'A single picture',
    created_at: '2026-08-01T12:00:00.000Z',
    like_count: 4,
    comment_count: 2,
    members: exts.map((ext, i) => {
      const id = `3f1b8e2c-0000-4000-8000-00000000c${i}0`;
      if (ext === null) return { asset_id: id, restricted: true };
      return { asset_id: id, asset: asset({ id, file_extension: ext }) };
    }),
  };
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const cardOf = (exts: Array<string | null>, extra: Record<string, any> = {}) =>
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  render(PostCard, { post: postOf(exts) as any, mode: 'thumbnail' as ViewMode, ...extra })
    .container;

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

// ONE ANSWER DECIDES, on a post card — not one asset.
//
// #1158's rule was a COUNT: a single-asset post showed its extension and
// a set showed nothing, because the COVER's extension is not the SET's
// fact. A carousel of a PNG, a PSD and an MP4 labelled "png" by
// whichever member happens to be the cover says something false about
// the other two — the failure #1111 named when it made the badge state
// the SET rather than any one member's kind.
//
// #1190 keeps that reasoning and finds the count was a proxy for it. The
// owner: "if a multi asset post contains all the same extension (glb,
// png, etc...) we can place the extension on the thumbnail. Not if it's
// mixed. Maybe we can put (mixed) for the extension instead?" A pack of
// six .glb files has a true answer — every member is a glb — and the old
// rule suppressed it for a reason that did not apply. So the band shows
// the shared extension when there is one and the WORD when there is not.
//
// Both directions are pinned, because the rule is only meaningful as a
// pair: "a uniform post shows glb" passes just as well on a card that
// prints the cover's extension unconditionally, which is the bug the
// original ruling exists to prevent.
describe('PostCard — the extension is a ONE-ANSWER fact', () => {
  const ext = (c: HTMLElement) => c.querySelector('[data-testid="thumb-band-extension"]');
  const extText = (c: HTMLElement) => ext(c)?.textContent?.trim() ?? null;

  it('SHOWS the extension when the post holds exactly one asset', () => {
    expect(extText(postCard('thumbnail', 1))).toBe('png');
  });

  it('⭐ SHOWS the shared extension when every member agrees', () => {
    expect(extText(cardOf(['glb', 'glb', 'glb']))).toBe('glb');
    expect(extText(postCard('thumbnail', 5))).toBe('png');
  });

  it('⭐ says "mixed" instead when the members disagree', () => {
    const c = cardOf(['png', 'psd', 'mp4']);
    expect(extText(c)).toBe('mixed');
    // The word is marked as a word. The span reads as an extension by
    // position and by casing, so a screen reader gets the sentence and
    // a future pass gets something to assert that is not the English.
    expect(ext(c)!.getAttribute('data-mixed')).toBe('true');
    expect(ext(c)!.getAttribute('aria-label')).toBeTruthy();
    // ...and a uniform set is NOT marked, or the flag would be noise.
    expect(ext(cardOf(['glb', 'glb']))!.getAttribute('data-mixed')).toBeNull();
  });

  it('normalises before comparing, so ".PNG" and "png" are one answer', () => {
    expect(extText(cardOf(['.PNG', 'png']))).toBe('png');
  });

  it('⭐⭐ computes uniformity over the members this READER can see', () => {
    // The leak this rule is written to avoid. Three visible PNGs beside
    // a member this caller may not read: the band says "png", because
    // the alternative — recomputing over all members and flipping to
    // "mixed" — would announce the existence and the foreignness of a
    // file that was deliberately withheld (#902/#1066's class, on a
    // card instead of in a query).
    expect(extText(cardOf(['png', 'png', null, 'png']))).toBe('png');
    // The owner of that member sees their own truth: with the same
    // member readable and a different format, the set IS mixed.
    expect(extText(cardOf(['png', 'png', 'glb', 'png']))).toBe('mixed');
  });

  it('says nothing when no member is readable', () => {
    // Nothing to compare is not "mixed" — it is no answer, and the band
    // prints no answer rather than a guess.
    expect(ext(cardOf([null, null]))).toBeNull();
  });

  it('says nothing when the payload carries only part of the membership', () => {
    // A search hit ships ONE member with the real total beside it, so
    // uniformity is unknowable there. Three of the four members are
    // absent from this payload — inferring "png" from the one that came
    // would be a sentence about a set this card never received.
    expect(ext(cardOf(['png'], { memberCount: 4 }))).toBeNull();
  });

  it('leaves the multi-asset badge to say the count as well', () => {
    // The count and the format are two facts, not alternatives — the
    // badge still states the SET while the extension states the format.
    const c = postCard('thumbnail', 3);
    expect(band(c)!.querySelector('[data-testid="card-kind-multi"]')).toBeTruthy();
    expect(extText(c)).toBe('png');
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

  // The owner: "Move the extension closer to the asset type icon. Maybe
  // half the space between." The measured 14px of air was the badge
  // pill's 6px trailing padding plus the BAND's own `gap-2`, so the fix
  // is that the glyph and the word stop being siblings of that gap and
  // become one unit with no gap of its own. jsdom computes no layout, so
  // what is pinned here is the STRUCTURE that produces the spacing —
  // which is the half a future pass would undo without noticing.
  it('⭐ keeps the glyph and the word in ONE gapless container', () => {
    for (const c of [postCard('thumbnail', 1), assetCard('thumbnail'), cardOf(['glb', 'glb'])]) {
      const badge = band(c)!.querySelector('[data-testid^="card-kind"]')!;
      const word = c.querySelector('[data-testid="thumb-band-extension"]')!;
      expect(word.parentElement, 'the word sits beside the glyph, not beside the band').toBe(
        badge.parentElement,
      );
      // ...and that shared parent is not the band itself, which still
      // needs its gap to hold the checkbox off at the far edge.
      expect(word.parentElement).not.toBe(band(c));
      expect(band(c)!.className).toContain('gap-2');
      expect(word.parentElement!.className).not.toMatch(/\bgap-/);
    }
  });

  // #1191 follow-up, the owner's phrasing: "7 mixed assets in this
  // post", "4 glb assets in this post". The badge already knew the
  // count; the format is what the band prints beside it.
  describe('the pack badge names the format as well as the count', () => {
    const badgeLabel = (c: HTMLElement) =>
      band(c)!.querySelector('[data-testid^="card-kind"]')!.getAttribute('aria-label');

    it('⭐ says the shared extension on a uniform pack', () => {
      expect(badgeLabel(cardOf(['glb', 'glb', 'glb', 'glb']))).toBe(
        '4 glb assets in this post',
      );
    });

    it('⭐ says the WORD on a mixed pack, matching what the band drew', () => {
      const c = cardOf(['png', 'psd', 'mp4', 'glb', 'png', 'psd', 'mp4']);
      expect(badgeLabel(c)).toBe('7 mixed assets in this post');
      // The sentence and the label beside it are the same value, never
      // two derivations that could drift apart.
      expect(extText(c)).toBe('mixed');
    });

    it('falls back to the count alone when the format is unknowable', () => {
      // A truncated payload: uniformity cannot be computed, so the band
      // prints nothing and the badge must not invent a format either.
      const c = cardOf(['png'], { memberCount: 4 });
      expect(ext(c)).toBeNull();
      expect(badgeLabel(c)).toBe('4 assets in this post');
    });

    it('leaves a SINGLE-asset post on the kind tooltip', () => {
      // Deliberate, and the reason is that the band is already printing
      // "PNG" two pixels to the right: "1 png asset in this post" would
      // say that back and drop the one word the reader could not
      // otherwise get. #1144 built this tooltip to spell the type out.
      const c = postCard('thumbnail', 1);
      expect(badgeLabel(c)).not.toMatch(/in this post/);
      expect(badgeLabel(c)).toBeTruthy();
    });

    it('says nothing at all when the cover is restricted', () => {
      // No badge, so no tooltip — the withholding is upstream of both.
      const c = cardOf([null, 'png']);
      expect(band(c)!.querySelector('[data-testid^="card-kind"]')).toBeNull();
    });
  });

  it('shows no extension in grid or masonry, which have no band', () => {
    expect(ext(postCard('grid', 1))).toBeNull();
    expect(ext(postCard('masonry', 1))).toBeNull();
  });

  it('WITHHOLDS the extension when the cover is restricted', () => {
    // A withheld value has derived copies, and each one has to be
    // withheld too. The band already suppresses the kind badge for a
    // restricted cover; a card that hides the icon and then prints
    // "png" — or "mixed" — beside the gap has disclosed something about
    // the thing it just withheld.
    const c = cardOf([null, 'png']);
    expect(ext(c)).toBeNull();
    // The badge is gone too — i.e. the test is observing the restricted
    // branch and not simply a card that failed to render a band.
    expect(band(c)!.querySelector('[data-testid^="card-kind"]')).toBeNull();
  });
});
