// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1077 — WHERE A COMMIT LANDS, ASSERTED ON THE URL.
//
// The acceptance for #1077 is deliberately not about the dropdown: a
// suggestion that RENDERS its kind as a chip and then commits as free
// text is exactly the bug, and the chip has been there since the
// dropdown was built. What changed is the destination, so the
// destination is what is asserted.
//
// The measured fact underneath: a tag-only word is in NO search
// document. `fantasy`, `kit` and `lowpoly` are each FALSE against their
// own asset's `assets.search_text`, because a tag is not indexed into an
// asset's document. So `?q=<tag>` returns nothing for exactly the values
// #1077 is about, and only the structured filter can answer them.

import { describe, it, expect } from 'vitest';

import { commitTarget, commitIsInPlace, consumesGlobalQuery } from './commitTarget';

const at = (path: string, search = '') => new URL(`https://alley.test${path}${search}`);
const tag = (value: string) => ({ dimension: 'tag' as const, value });

describe('commitTarget — free text (unchanged)', () => {
  it('refines in place on a result surface', () => {
    expect(commitTarget(at('/'), 'dragon').search).toBe('?q=dragon');
    expect(commitTarget(at('/search', '?q=old'), 'dragon').search).toBe('?q=dragon');
  });

  it('lands on browse from a surface that renders no feed', () => {
    const target = commitTarget(at('/account/settings'), 'dragon');
    expect(target.pathname).toBe('/');
    expect(target.search).toBe('?q=dragon');
  });

  it('an empty commit clears the query rather than searching for ""', () => {
    expect(commitTarget(at('/', '?q=dragon'), '').search).toBe('');
    expect(commitTarget(at('/', '?q=dragon'), '   ').search).toBe('');
  });

  it('keeps the other parameters of the surface it is refining', () => {
    const target = commitTarget(at('/', '?tag=fantasy&team=7'), 'dragon');
    expect(target.searchParams.get('tag')).toBe('fantasy');
    expect(target.searchParams.get('team')).toBe('7');
    expect(target.searchParams.get('q')).toBe('dragon');
  });
});

describe('commitTarget — a picked TAG applies the structured filter (#1077)', () => {
  it('on /search it becomes filter=tag:<value>, NOT q', () => {
    const target = commitTarget(at('/search'), 'lowpoly', tag('lowpoly'));
    expect(target.pathname).toBe('/search');
    expect(target.searchParams.getAll('filter')).toEqual(['tag:lowpoly']);
    // ⛔ The half that matters. `q=lowpoly` beside the filter would AND
    // with a TSVECTOR match the word cannot satisfy, so the page would be
    // empty for a different reason than before — the same defect, one
    // conjunct later.
    expect(target.searchParams.has('q')).toBe(false);
  });

  it('on browse it becomes ?tag=<value>, NOT q', () => {
    const target = commitTarget(at('/'), 'lowpoly', tag('lowpoly'));
    expect(target.pathname).toBe('/');
    expect(target.searchParams.get('tag')).toBe('lowpoly');
    expect(target.searchParams.has('q')).toBe(false);
  });

  it('from a non-feed surface it lands on browse with the tag applied', () => {
    const target = commitTarget(at('/admin/users'), 'lowpoly', tag('lowpoly'));
    expect(target.pathname).toBe('/');
    expect(target.searchParams.get('tag')).toBe('lowpoly');
  });

  it('drops the typed prefix that produced the suggestion', () => {
    // The user typed "lowp" and picked "lowpoly". Leaving `q=lowp` in
    // place is the free-text commit wearing the filter's clothes.
    const target = commitTarget(at('/search', '?q=lowp'), 'lowpoly', tag('lowpoly'));
    expect(target.searchParams.has('q')).toBe(false);
    expect(target.searchParams.getAll('filter')).toEqual(['tag:lowpoly']);
  });

  it('REFINES an existing selection rather than replacing it', () => {
    const target = commitTarget(
      at('/search', '?filter=extension%3Apng'),
      'lowpoly',
      tag('lowpoly'),
    );
    expect(target.searchParams.getAll('filter')).toEqual(['extension:png', 'tag:lowpoly']);
  });

  it('picking the same tag twice does not double the token', () => {
    const target = commitTarget(at('/search', '?filter=tag%3Alowpoly'), 'lowpoly', tag('lowpoly'));
    expect(target.searchParams.getAll('filter')).toEqual(['tag:lowpoly']);
  });

  it('replaces the browse tag rather than stacking an unreadable one', () => {
    // Browse's control is single-select and its heading renders ONE
    // `#tag`, so a second pick means "that one instead".
    const target = commitTarget(at('/', '?tag=fantasy'), 'lowpoly', tag('lowpoly'));
    expect(target.searchParams.getAll('tag')).toEqual(['lowpoly']);
  });

  it('a tag whose value needs escaping survives the round trip', () => {
    const target = commitTarget(at('/search'), 'a:b c', tag('a:b c'));
    // Encoded on the wire, exact on the way out — the grammar cuts a
    // `filter=` token at its FIRST colon, so `tag:a:b c` is the `tag`
    // dimension with the value `a:b c`.
    expect(target.search).toContain('filter=tag%3Aa%3Ab+c');
    expect(target.searchParams.getAll('filter')).toEqual(['tag:a:b c']);
  });

  it('an empty term value falls back to the free-text commit', () => {
    // Nothing produces this, and the fallback is the safe direction: a
    // `filter=tag:` token would be a 400 out of ParseSelection.
    const target = commitTarget(at('/'), 'lowpoly', tag(''));
    expect(target.searchParams.get('q')).toBe('lowpoly');
    expect(target.searchParams.has('tag')).toBe(false);
  });
});

describe('commitIsInPlace + consumesGlobalQuery', () => {
  it('keeps focus when the surface does not change', () => {
    expect(commitIsInPlace(at('/'), commitTarget(at('/'), 'dragon'))).toBe(true);
    expect(commitIsInPlace(at('/search'), commitTarget(at('/search'), 'x', tag('x')))).toBe(true);
  });

  it('does NOT keep focus when the commit moves the user to browse', () => {
    const current = at('/account');
    expect(commitIsInPlace(current, commitTarget(current, 'dragon'))).toBe(false);
    expect(commitIsInPlace(current, commitTarget(current, 'x', tag('x')))).toBe(false);
  });

  it('names the two surfaces that render a feed keyed off q', () => {
    expect(consumesGlobalQuery('/')).toBe(true);
    expect(consumesGlobalQuery('/search')).toBe(true);
    expect(consumesGlobalQuery('/search/advanced')).toBe(false);
    expect(consumesGlobalQuery('/collections')).toBe(false);
  });
});
