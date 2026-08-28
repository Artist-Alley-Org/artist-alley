// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1210: a post cover's focal point, and the two things about it that
// are easy to get wrong.
//
// 1. WHERE IT APPLIES. Grid is the only post mode that crops:
//    PostCard sets `fill` there and nowhere else, and `fill` is
//    `object-fit: cover` against a square frame. Masonry takes the
//    picture's own shape and feed / thumbnail / band / list letterbox it
//    whole. A focal point says nothing about a picture shown whole, so
//    those modes must render IDENTICALLY with it set and unset, not
//    "look fine" but identically. The tests below assert that as HTML
//    equality rather than as an absent attribute, because the failure
//    shape is a stray style or a changed `src`, and an assertion aimed
//    at one field would not see the other.
//
// 2. WHAT IT IS PAINTED FROM. The fractions are measured against the
//    ORIGINAL picture, and `col` is a 320x320 square the server already
//    cropped at its centre, so positioning inside it moves a crop of a
//    crop. A framed tile therefore has to draw from a CONTAIN rung, and
//    when it cannot it must drop the framing rather than misapply it.
//
// jsdom has no layout engine, so what is asserted is the DECISION the
// component published: the requested source, the srcset it offered the
// browser, and the object-position it emitted. Those are exactly the
// three inputs the browser's own `object-fit` machinery consumes, and
// they are what changed.

import { render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import PostCard from './PostCard.svelte';
import { previewLadder } from '$stores/previewLadder.svelte';
import type { CardAsset } from './cardAsset';
import type { ViewMode } from '$stores/browseView.svelte';

const ASSET_ID = '3f1b8e2c-0000-4000-8000-0000000010ec';
const POST_ID = '3f1b8e2c-0000-4000-8000-0000000010ed';

function asset(overrides: Partial<CardAsset> = {}): CardAsset {
  return {
    id: ASSET_ID,
    title: 'An off-centre subject',
    asset_type: 1,
    created_at: '2026-08-01T12:00:00.000Z',
    file_hash: 'd'.repeat(64),
    file_extension: 'png',
    thumbhash: null,
    preview_available: true,
    ladder_available: true,
    scrub_available: false,
    pixel_width: 2400,
    pixel_height: 1200,
    ...overrides,
  };
}

function post(focal: { x: number; y: number } | null, over: Partial<CardAsset> = {}) {
  return {
    id: POST_ID,
    title: 'A single picture',
    created_at: '2026-08-01T12:00:00.000Z',
    posted_at: '2026-08-01T12:00:00.000Z',
    author_user_ref: 1,
    description: '',
    visibility: 'public',
    tags: [],
    updated_at: '2026-08-01T12:00:00.000Z',
    like_count: 0,
    comment_count: 0,
    cover_asset_id: ASSET_ID,
    cover_focal_x: focal?.x ?? null,
    cover_focal_y: focal?.y ?? null,
    members: [{ asset_id: ASSET_ID, sort_order: 0, asset: asset(over) }],
  };
}

const card = (mode: ViewMode, focal: { x: number; y: number } | null, over: Partial<CardAsset> = {}) =>
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  render(PostCard, { post: post(focal, over) as any, mode, feed: mode === 'feed' }).container;

function img(c: HTMLElement): HTMLImageElement {
  const el = c.querySelector<HTMLImageElement>('img[data-focal]');
  expect(el, 'the cover image should render').toBeTruthy();
  return el!;
}

/** The install's default ladder (sysconfig DefaultPreviewConfig). */
function defaultLadder() {
  previewLadder.coverRungs = [{ key: 'col', maxDim: 320 }];
  previewLadder.rungs = [
    { key: 'preview', maxDim: 1024 },
    { key: 'screen', maxDim: 1920 },
    { key: 'hires', maxDim: 4096 },
  ];
}

describe('a post cover focal point (#1210)', () => {
  beforeEach(defaultLadder);
  afterEach(() => {
    previewLadder.rungs = [];
    previewLadder.coverRungs = [];
  });

  it('positions the grid tile from the stored fractions', () => {
    const el = img(card('grid', { x: 0.25, y: 0.8 }));
    expect(el.dataset.focal).toBe('on');
    expect(el.getAttribute('style')).toContain('object-position: 25% 80%');
  });

  it('draws the framed tile from a CONTAIN rung and never from col', () => {
    const el = img(card('grid', { x: 0.25, y: 0.8 }));
    // `src` is what a browser ignoring srcset loads, and what the
    // loader uses before it picks a candidate. Neither may be the
    // square the server already cropped.
    expect(el.getAttribute('src')).toBe(`/api/v1/assets/${ASSET_ID}/variants/preview`);
    const srcset = el.getAttribute('srcset') ?? '';
    expect(srcset).not.toContain('/variants/col');
    expect(srcset).toContain('/variants/preview');
    expect(srcset).toContain('/variants/hires');
  });

  it('keeps col in the srcset when the tile is NOT framed', () => {
    // The #1169 arrangement, unchanged: an unframed cropping tile is
    // free to take the cheapest square for a small slot.
    const el = img(card('grid', null));
    expect(el.dataset.focal).toBe('off');
    expect(el.getAttribute('srcset') ?? '').toContain('/variants/col');
    expect(el.getAttribute('style')).toBeNull();
  });

  it('DROPS the framing when there is no contain rung to honour it', () => {
    previewLadder.rungs = [];
    const el = img(card('grid', { x: 0.25, y: 0.8 }, { ladder_available: false }));
    // `col`, centred, which is what every card did before focal points
    // existed. Positioning inside it would crop a crop.
    expect(el.dataset.focal).toBe('off');
    expect(el.getAttribute('src')).toBe(`/api/v1/assets/${ASSET_ID}/variants/col`);
    expect(el.getAttribute('style')).toBeNull();
  });

  it('DROPS the framing when the source dimensions were never measured', () => {
    // Without them the crop fraction of each contain rung is unknown,
    // so no describable candidate is left that is not the square.
    const el = img(card('grid', { x: 0.25, y: 0.8 }, { pixel_width: null, pixel_height: null }));
    expect(el.dataset.focal).toBe('off');
    expect(el.getAttribute('style')).toBeNull();
  });

  for (const mode of ['masonry', 'feed', 'thumbnail', 'band', 'list'] as ViewMode[]) {
    it(`renders ${mode} identically with and without a focal point`, () => {
      const framed = card(mode, { x: 0.05, y: 0.95 }).innerHTML;
      const plain = card(mode, null).innerHTML;
      expect(framed).toBe(plain);
    });
  }
});
