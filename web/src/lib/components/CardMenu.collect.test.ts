// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// WHO MAY BE PUT IN A COLLECTION — #1185, pinned in both directions.
//
// The owner's model, stated three times: non-post assets belong only to
// their uploader; collections and browse contain posts only. #1185 acted
// on the display half of that, and this file holds the affordance half.
//
// Until then every ASSET card offered "Add to collection…", and it
// worked: the click POSTed to `/collections/{id}/resources`, the server
// wrote the row, the picker reported success — and nothing anywhere
// rendered it, because the collection page shows `collection_posts` and
// only that. A write that reports success and changes nothing the user
// can see is worse than a missing feature, and it is invisible to every
// test that checks the request rather than the wall.
//
// BOTH directions are asserted because either alone is a trap:
//
//   - "the asset card offers no add-to-collection" passes just as well
//     when the menu failed to open, when the user is signed out, or when
//     the whole item was deleted for posts too. So the post card's item
//     is asserted in the same breath, from the same fixture and the same
//     gate — it is the control.
//   - "the post card still offers it" is the thing that must NOT be
//     collateral damage. `collection_posts` is the unit; saving someone
//     else's post is the point of #882 and it survives untouched.
//
// The read-only actions are asserted present on the asset card too. That
// is the guard against the cheapest false pass of all: an asset menu
// that renders nothing at all would satisfy the absence assertion while
// being a much bigger bug.

import { fireEvent, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import AssetCard from './AssetCard.svelte';
import PostCard from './PostCard.svelte';
import type { CardAsset } from './cardAsset';
import { auth } from '$stores/auth.svelte';

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
    title: 'A single picture',
    created_at: '2026-08-01T12:00:00.000Z',
    like_count: 4,
    comment_count: 2,
    members: [{ asset_id: ASSET_ID, asset: asset() }],
  };
}

/** Render a card, open its ⋯ menu, and hand back the PORTALED panel.
 *
 *  The panel is portaled to <body> (it has to be — the card is
 *  `overflow-hidden` and gains a transform on hover), so querying the
 *  render container finds nothing whether the item is there or not.
 *  Every assertion below reads the panel this returns. */
async function openMenu(kind: 'asset' | 'post'): Promise<HTMLElement> {
  const { container } =
    kind === 'asset'
      ? render(AssetCard, { asset: asset(), mode: 'grid' as const })
      : // eslint-disable-next-line @typescript-eslint/no-explicit-any
        render(PostCard, { post: post() as any, mode: 'grid' as const });

  const trigger = container.querySelector<HTMLElement>('[data-testid="card-menu-trigger"]');
  expect(trigger, `${kind} card should render a ⋯ trigger`).toBeTruthy();
  await fireEvent.click(trigger!);
  await tick();

  const panel = document.body.querySelector<HTMLElement>('[data-testid="card-menu-panel"]');
  expect(panel, `${kind} card's ⋯ menu should have opened`).toBeTruthy();
  return panel!;
}

const collectItem = (panel: HTMLElement) =>
  panel.querySelector('[data-testid="card-add-to-collection"]');

describe('add-to-collection is a POST affordance only (#1185)', () => {
  beforeEach(() => {
    // The item is gated on a signed-in, non-demo caller. Signed in is
    // the ONLY state in which its absence on an asset card means what
    // this file says it means.
    auth.user = { ref: 42, username: 'viewer' } as never;
  });
  afterEach(() => {
    auth.user = null;
    document.body.innerHTML = '';
  });

  it('an ASSET card offers no add-to-collection', async () => {
    expect(collectItem(await openMenu('asset'))).toBeNull();
  });

  it('a POST card still offers it', async () => {
    // The control for the assertion above, and the thing that must not
    // become collateral damage: collection_posts is the unit (#882).
    expect(collectItem(await openMenu('post'))).toBeTruthy();
  });

  it('the ASSET card menu is otherwise intact', async () => {
    // Guards the cheapest false pass: an asset menu that rendered
    // nothing would satisfy the absence assertion above.
    const panel = await openMenu('asset');
    expect(panel.querySelectorAll('[role="menuitem"]').length).toBeGreaterThan(0);
    expect(panel.querySelector(`a[href="/assets/${ASSET_ID}"]`)).toBeTruthy();
  });

  it('offers it to nobody while signed out', async () => {
    // The pre-existing write gate still decides; #1185 narrowed WHICH
    // cards may ever show the item, not who may use it.
    auth.user = null;
    expect(collectItem(await openMenu('post'))).toBeNull();
    document.body.innerHTML = '';
    expect(collectItem(await openMenu('asset'))).toBeNull();
  });
});
