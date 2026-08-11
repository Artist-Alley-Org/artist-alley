// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// /account overview grid — RENDERED-DOM assertions (#600).
//
// The registry invariants live in lib/account/sections.test.ts and are
// pure data. This file exists because reading the source proves the
// wrong thing: the tile grid renders `href={item.href}` and
// `t('account.items.' + slug + '.title')`, so a missing i18n key or a
// group whose items never reach the template shows up as a broken tile
// in the browser while every source-level grep still looks correct.
//
// So: mount the real page component and read the anchors back out.

import { render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';

import AccountOverview from './+page.svelte';
import { ACCOUNT_GROUPS, ACCOUNT_ITEMS, itemsByGroup } from '$lib/account/sections';
import { t } from '$stores/lang.svelte';

function tileAnchors(container: HTMLElement): HTMLAnchorElement[] {
  return Array.from(container.querySelectorAll<HTMLAnchorElement>('a[href^="/account"]'));
}

describe('/account tile grid (rendered)', () => {
  it('renders one tile per registry entry, at its registered href', () => {
    const { container } = render(AccountOverview);
    const hrefs = tileAnchors(container).map((a) => a.getAttribute('href'));
    expect(hrefs.length).toBe(ACCOUNT_ITEMS.length);
    for (const item of ACCOUNT_ITEMS) {
      expect(hrefs, `no tile rendered for '${item.slug}'`).toContain(item.href);
    }
  });

  it('shows the access-requests tile, linking the page that had no nav entry (#600)', () => {
    // The concrete gap: /account/requests was a finished page (Phase
    // 1.17.E) that nothing in the app linked to. Assert the tile is on
    // the grid AND points at the real page — not at a placeholder.
    const { container } = render(AccountOverview);
    const tile = container.querySelector<HTMLAnchorElement>('a[href="/account/requests"]');
    expect(tile).not.toBeNull();
    expect(tile?.textContent).toContain(t('account.items.requests.title'));
  });

  it('renders every tile label + blurb as real copy, never a raw i18n key', () => {
    // `t()` falls back to echoing the key when it misses, so a missing
    // en.json entry renders as the literal "account.items.foo.title".
    // That is invisible to a source grep and obvious here.
    const { container } = render(AccountOverview);
    const raw = tileAnchors(container)
      .map((a) => a.textContent ?? '')
      .filter((text) => /account\.(items|groups)\./.test(text));
    expect(raw, `tiles rendered untranslated i18n keys:\n  ${raw.join('\n  ')}`).toEqual([]);
  });

  it('renders every group heading, each with its items beneath it', () => {
    render(AccountOverview);
    for (const g of ACCOUNT_GROUPS) {
      const heading = screen.getByRole('heading', { name: t(`account.groups.${g.id}.title`) });
      expect(heading).toBeTruthy();
      const section = heading.closest('section');
      const inSection = Array.from(
        section?.querySelectorAll<HTMLAnchorElement>('a[href^="/account"]') ?? [],
      ).map((a) => a.getAttribute('href'));
      expect(inSection.sort()).toEqual(itemsByGroup(g.id).map((i) => i.href).sort());
    }
  });

  it("the pages this sprint added are on the grid and reachable by href", () => {
    const { container } = render(AccountOverview);
    for (const href of ['/account/following', '/account/help', '/account/shortcuts']) {
      expect(container.querySelector(`a[href="${href}"]`), `${href} tile missing`).not.toBeNull();
    }
  });
});
