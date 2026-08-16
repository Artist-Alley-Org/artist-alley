// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1156 — typing must not re-query the feed.
//
// The bar the owner set is a REQUEST COUNT, not a "feels right": type
// five characters and the number of feed queries is zero. `onsearch` is
// the only channel the component has to the feed — the parent turns each
// call into one `goto`, which the browse/search page turns into one fetch
// — so counting `onsearch` calls counts feed queries exactly.
//
// The suggest fetch is counted SEPARATELY and is expected to be non-zero:
// suggestions during input are the half of #1156 that stays.

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import SearchBar from './SearchBar.svelte';

const SUGGEST_DEBOUNCE_MS = 150;

/** A fetch stub that answers /search/suggest with the given values. */
function stubSuggest(values: string[]) {
  return vi.fn(async () => ({
    ok: true,
    json: async () => ({
      suggestions: values.map((v) => ({ value: v, kind: 'tag', similarity: 0.9 })),
    }),
  }));
}

/** Type `text` one character at a time, the way a person does. */
async function typeChars(input: HTMLInputElement, text: string) {
  for (const ch of text) {
    input.value += ch;
    await fireEvent.input(input);
  }
}

/** Wait past every debounce the component owns. */
function settle() {
  return new Promise((r) => setTimeout(r, SUGGEST_DEBOUNCE_MS * 3));
}

describe('SearchBar (#1156 — no live refine)', () => {
  let onsearch: ReturnType<typeof vi.fn> & ((q: string) => void);
  let fetchSpy: ReturnType<typeof stubSuggest>;

  beforeEach(() => {
    onsearch = vi.fn() as unknown as ReturnType<typeof vi.fn> & ((q: string) => void);
    fetchSpy = stubSuggest(['sculpture']);
    vi.stubGlobal('fetch', fetchSpy);
    localStorage.clear();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('fires ZERO feed queries while typing five characters', async () => {
    const { getByTestId } = render(SearchBar, { props: { value: '', onsearch } });
    const input = getByTestId('nav-search') as HTMLInputElement;

    await typeChars(input, 'sculp');
    // Well past the old 250ms commit debounce, so a surviving timer would
    // have fired by now.
    await new Promise((r) => setTimeout(r, 500));

    expect(onsearch).toHaveBeenCalledTimes(0);
  });

  it('still fetches suggestions while typing', async () => {
    const { getByTestId } = render(SearchBar, { props: { value: '', onsearch } });
    const input = getByTestId('nav-search') as HTMLInputElement;

    await typeChars(input, 'scul');
    await settle();

    expect(fetchSpy).toHaveBeenCalled();
    const url = String((fetchSpy.mock.calls[0] as unknown[])[0]);
    expect(url).toContain('/search/suggest');
    // #1155 — the corpus the commit will be executed against rides along.
    expect(url).toContain('scope=browse');
  });

  it('passes the search scope through when the host is /search', async () => {
    const { getByTestId } = render(SearchBar, {
      props: { value: '', onsearch, scope: 'search' as const },
    });
    const input = getByTestId('nav-search') as HTMLInputElement;

    await typeChars(input, 'scul');
    await settle();

    expect(String((fetchSpy.mock.calls[0] as unknown[])[0])).toContain('scope=search');
  });

  it('fires exactly ONE feed query on Enter', async () => {
    const { getByTestId } = render(SearchBar, { props: { value: '', onsearch } });
    const input = getByTestId('nav-search') as HTMLInputElement;

    await typeChars(input, 'scul');
    await settle();
    await fireEvent.keyDown(input, { key: 'Enter' });

    expect(onsearch).toHaveBeenCalledTimes(1);
    expect(onsearch).toHaveBeenCalledWith('scul');
  });

  it('fires exactly ONE feed query when a suggestion is picked', async () => {
    const { getByTestId, findByTestId } = render(SearchBar, {
      props: { value: '', onsearch },
    });
    const input = getByTestId('nav-search') as HTMLInputElement;

    await typeChars(input, 'scul');
    await settle();
    const row = await findByTestId('search-suggestion');
    await fireEvent.mouseDown(row);

    expect(onsearch).toHaveBeenCalledTimes(1);
    expect(onsearch).toHaveBeenCalledWith('sculpture');
  });

  it('Arrow Down highlights the SUGGESTION, not a history row', async () => {
    // The rendered order is suggestions then history, and `highlight`
    // indexes that merged list. Before #1156 it indexed `history` alone,
    // so the first Arrow Down selected a row several places lower.
    localStorage.setItem('search_history', JSON.stringify(['older query']));
    const { getByTestId, findByTestId } = render(SearchBar, {
      props: { value: '', onsearch },
    });
    const input = getByTestId('nav-search') as HTMLInputElement;

    await typeChars(input, 'scul');
    await settle();
    await findByTestId('search-suggestion');
    await fireEvent.keyDown(input, { key: 'ArrowDown' });
    await tick();
    await fireEvent.keyDown(input, { key: 'Enter' });

    expect(onsearch).toHaveBeenCalledTimes(1);
    expect(onsearch).toHaveBeenCalledWith('sculpture');
  });

  it('Escape with the dropdown open closes it and searches nothing', async () => {
    const { getByTestId, findByTestId, queryByTestId } = render(SearchBar, {
      props: { value: '', onsearch },
    });
    const input = getByTestId('nav-search') as HTMLInputElement;

    await typeChars(input, 'scul');
    await settle();
    await findByTestId('search-suggestion');
    await fireEvent.keyDown(input, { key: 'Escape' });
    await tick();

    expect(queryByTestId('search-history')).toBeNull();
    expect(onsearch).toHaveBeenCalledTimes(0);
  });

  it('the clear button commits the empty query', async () => {
    const { getByTestId, getByLabelText } = render(SearchBar, {
      props: { value: 'sculpture', onsearch },
    });

    await fireEvent.click(getByLabelText(/clear/i));
    await tick();

    expect(onsearch).toHaveBeenCalledTimes(1);
    expect(onsearch).toHaveBeenCalledWith('');
    expect((getByTestId('nav-search') as HTMLInputElement).value).toBe('');
  });
});
