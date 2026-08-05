// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #883 + #881 — the restricted placeholder, and the ask it now carries.
//
// The rule under test is the owner's, 2026-08-03: "the placeholder
// should never leak info. Not even title. Only the owner's name."
//
// The interesting assertion is an ALLOW-LIST, not a deny-list. A test
// that says `expect(text).not.toContain(title)` passes forever on the
// day someone adds `filename` or `tags` to the tile, which is the shape
// of failure that let `config` ship SSO secrets and `metadata` ship EXIF
// GPS coordinates. So the tests below take every rendered string in the
// plate — text nodes AND attribute values — and require each one to be
// something explicitly permitted. A new leaked field fails by default.
//
// The fixture deliberately carries a title, a filename extension and
// dimensions even though the SERVER never sends those for a restricted
// asset (assets/withhold.go). That is the point: the component is
// asserted to withhold them even when handed them, so the tile is not
// relying on the API to be the only guard.
//
// jsdom has no container queries, so the plate's size tiers — including
// whether the ask button is displayed at the 60px masonry floor — are
// not assertable here. They are verified in the browser and the
// screenshots are on the PR. What IS assertable is which strings the
// component is willing to put in the DOM at all.

import { render } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import AssetCard from './AssetCard.svelte';
import type { CardAsset } from './cardAsset';
import { auth } from '$stores/auth.svelte';

const SECRET_TITLE = 'Commission WIP — client NDA piece';
const OWNER = 'Rowan Ashgrove';
const ASSET_ID = '3f1b8e2c-0000-4000-8000-000000000881';

function restrictedAsset(overrides: Partial<CardAsset> = {}): CardAsset {
  return {
    id: ASSET_ID,
    // Everything from here down is what the server WITHHOLDS. Present
    // in the fixture so the component is tested on its own restraint.
    title: SECRET_TITLE,
    asset_type: 5,
    created_at: '2026-08-03T02:31:22.000Z',
    file_hash: 'd'.repeat(64),
    file_extension: 'psd',
    thumbhash: null,
    preview_available: true,
    ladder_available: true,
    scrub_available: true,
    pixel_width: 4096,
    pixel_height: 2160,
    // What the server DOES send.
    restricted: true,
    owner_display_name: OWNER,
    ...overrides,
  } as CardAsset;
}

function plate(container: HTMLElement): HTMLElement {
  const el = container.querySelector<HTMLElement>('[data-card-restricted]');
  expect(el, 'a restricted asset should render the restricted plate').toBeTruthy();
  return el!;
}

/** Every string the plate puts in the DOM: text nodes and attribute
 *  values alike. An aria-label or a title attribute is as much a leak
 *  as visible text, and screen-reader users would be the ones leaked
 *  to. */
function renderedStrings(root: HTMLElement): string[] {
  const out: string[] = [];
  const push = (s: string | null) => {
    const v = (s ?? '').trim();
    if (v) out.push(v);
  };
  const walk = (el: Element) => {
    for (const attr of Array.from(el.attributes)) push(attr.value);
    for (const node of Array.from(el.childNodes)) {
      if (node.nodeType === Node.TEXT_NODE) push(node.textContent);
      else if (node.nodeType === Node.ELEMENT_NODE) walk(node as Element);
    }
  };
  walk(root);
  return out;
}

/** Strings the placeholder is permitted to render. Shipped UI copy,
 *  the owner's display name, and the presentational scaffolding
 *  (classes, svg geometry, data-testids) that carries no asset facts.
 *
 *  Adding to this list is a deliberate act. If a rendered string is not
 *  here, the question to answer is "may a viewer who cannot see this
 *  asset be told this?" — not "how do I make the test pass?". */
const PERMITTED_TEXT = new Set([
  'Restricted',
  `by ${OWNER}`,
  'Owner not shown',
  'This item is restricted. You do not have access to it.',
  'Request access',
  'Requested',
  'true',
  'button',
  'request-access-open',
  '0 0 24 24',
  'http://www.w3.org/2000/svg',
  'none',
  'currentColor',
  '1.3',
  'round',
]);

function assertOnlyPermitted(root: HTMLElement) {
  for (const s of renderedStrings(root)) {
    // Presentational-only values: Tailwind class lists, the component's
    // scoped-style hash, and the two SVG path strings. None of them can
    // carry an asset fact, and pinning their exact text would make the
    // test a styling change detector.
    if (/^[-a-z0-9:/\[\]().,%_ ]+$/.test(s) && !/[A-Z]/.test(s) && s.includes(' ')) continue;
    if (/^[\d.\s MLAHVCZmlahvcz-]+$/.test(s)) continue;
    if (/^svelte-[a-z0-9]+$/.test(s)) continue;
    if (/^(plate|stack|glyph|label|owner|ask|sr-only|absolute|inset-0|text-fg-muted|relative|z-10)$/.test(s)) continue;
    expect(
      PERMITTED_TEXT.has(s),
      `the restricted placeholder rendered ${JSON.stringify(s)}, which is not on the allow-list. ` +
        `A restricted tile may say that it is restricted and whose it is, and nothing else ` +
        `(owner's rule, 2026-08-03).`,
    ).toBe(true);
  }
}

describe('CardRestricted (#883, #881)', () => {
  beforeEach(() => {
    auth.user = null;
  });

  it('renders the plate instead of anything derived from the asset', () => {
    const { container } = render(AssetCard, { asset: restrictedAsset(), mode: 'grid' });
    const p = plate(container);
    expect(p.textContent).toContain('Restricted');
    expect(p.textContent).toContain(OWNER);
  });

  it('leaks nothing: every rendered string is on the allow-list', () => {
    const { container } = render(AssetCard, { asset: restrictedAsset(), mode: 'grid' });
    assertOnlyPermitted(plate(container));
  });

  it('leaks nothing once the ask is offered either', () => {
    auth.user = { ref: 42, username: 'viewer' } as never;
    const { container } = render(AssetCard, { asset: restrictedAsset(), mode: 'grid' });
    const p = plate(container);
    expect(p.querySelector('[data-testid="request-access-open"]')).toBeTruthy();
    assertOnlyPermitted(p);
  });

  it('offers no ask to an anonymous viewer, who has no account to ask from', () => {
    const { container } = render(AssetCard, { asset: restrictedAsset(), mode: 'grid' });
    expect(plate(container).querySelector('[data-testid="request-access-open"]')).toBeNull();
  });

  it('renders no link, menu or checkbox on a restricted tile', () => {
    // #883's rule, re-pinned because #881 added the FIRST interactive
    // element to this tile: the ask must not have brought the rest of
    // the card's chrome back with it.
    auth.user = { ref: 42, username: 'viewer' } as never;
    const { container } = render(AssetCard, { asset: restrictedAsset(), mode: 'grid' });
    expect(container.querySelector('a')).toBeNull();
    expect(container.querySelector('input[type="checkbox"]')).toBeNull();
  });

  it('says the owner is unknown rather than rendering a blank where a name goes', () => {
    const { container } = render(AssetCard, {
      asset: restrictedAsset({ owner_display_name: null }),
      mode: 'grid',
    });
    expect(plate(container).textContent).toContain('Owner not shown');
  });
});
