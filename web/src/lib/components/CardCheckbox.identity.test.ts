// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The card affordance contributes a TYPED identity (#1119, #1173).
//
// The store test proves the store can hold `{kind, id}`. This proves
// the MOUNTED CONTROL supplies both halves, which is the half that
// actually shipped broken. `CardCheckbox` took `id: string` and nothing
// else, so the one place in the app that KNOWS whether a card is a post
// or an asset threw that knowledge away at the point of contact, and no
// amount of store work downstream could recover it.
//
// The load-bearing case is the SAME UUID mounted twice, once as an
// asset and once as a post. Nothing in the schema forbids that pair,
// both cards are separately visible and separately tickable, and the
// shipped control could not tell them apart: it asked `selection.has(id)`
// and both boxes answered together.
//
// Against the shipped `CardCheckbox` every test here fails: it has no
// `kind` prop at all, so the two instances collapse onto one id.

import { render, fireEvent } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import CardCheckbox from './CardCheckbox.svelte';
import { selection } from '$stores/selection.svelte';
import { auth } from '$stores/auth.svelte';

/** One uuid, worn by an asset and by a post. */
const SHARED = '9f2c1a44-0000-4000-8000-000000001119';

function box(container: HTMLElement): HTMLElement {
  const el = container.querySelector<HTMLElement>('[role="checkbox"]');
  if (!el) throw new Error('the checkbox did not render');
  return el;
}

beforeEach(() => {
  selection.clear();
  auth.user = { ref: 42, username: 'operator' } as never;
});

describe('a card contributes its KIND along with its id', () => {
  it('an asset checkbox stores an asset entry', async () => {
    const { container } = render(CardCheckbox, { kind: 'asset', id: SHARED });

    await fireEvent.click(box(container));

    expect(selection.entries).toEqual([{ kind: 'asset', id: SHARED }]);
  });

  it('a post checkbox stores a post entry', async () => {
    const { container } = render(CardCheckbox, { kind: 'post', id: SHARED });

    await fireEvent.click(box(container));

    expect(selection.entries).toEqual([{ kind: 'post', id: SHARED }]);
  });
});

describe('the same uuid as an asset AND as a post', () => {
  it('two mounted checkboxes tick independently', async () => {
    const a = render(CardCheckbox, { kind: 'asset', id: SHARED });
    const p = render(CardCheckbox, { kind: 'post', id: SHARED });

    await fireEvent.click(box(a.container));

    // The ASSET box is checked and the POST box is NOT. On the shipped
    // control both read `selection.has(SHARED)` and both would be
    // checked here.
    expect(box(a.container).getAttribute('aria-checked')).toBe('true');
    expect(box(p.container).getAttribute('aria-checked')).toBe('false');

    await fireEvent.click(box(p.container));

    expect(selection.count).toBe(2);
    expect(selection.entries).toEqual([
      { kind: 'asset', id: SHARED },
      { kind: 'post', id: SHARED },
    ]);
  });

  it('unticking one leaves the other selected', async () => {
    const a = render(CardCheckbox, { kind: 'asset', id: SHARED });
    const p = render(CardCheckbox, { kind: 'post', id: SHARED });
    await fireEvent.click(box(a.container));
    await fireEvent.click(box(p.container));

    await fireEvent.click(box(a.container));

    expect(selection.entries).toEqual([{ kind: 'post', id: SHARED }]);
    expect(box(a.container).getAttribute('aria-checked')).toBe('false');
    expect(box(p.container).getAttribute('aria-checked')).toBe('true');
  });
});

describe('Shift+click ranges over a MIXED ordered list', () => {
  const A = 'aaaaaaaa-0000-4000-8000-000000001119';
  const B = 'bbbbbbbb-0000-4000-8000-000000001119';

  it('selects the run and keeps every entry\'s kind', async () => {
    const ordered = () => [
      { kind: 'post' as const, id: A },
      { kind: 'post' as const, id: SHARED },
      { kind: 'asset' as const, id: SHARED },
      { kind: 'asset' as const, id: B },
    ];
    const first = render(CardCheckbox, { kind: 'post', id: A, ordered });
    const last = render(CardCheckbox, { kind: 'asset', id: B, ordered });

    await fireEvent.click(box(first.container));
    await fireEvent.click(box(last.container), { shiftKey: true });

    expect(selection.entries).toEqual([
      { kind: 'post', id: A },
      { kind: 'post', id: SHARED },
      { kind: 'asset', id: SHARED },
      { kind: 'asset', id: B },
    ]);
  });
});
