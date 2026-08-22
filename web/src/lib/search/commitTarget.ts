// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * Where a search-box COMMIT navigates, as a pure function of the current
 * URL and what was committed (#1077).
 *
 * # Why this is a module and not four lines inside `handleSearch`
 *
 * Because a commit stopped being one kind of thing. It used to be
 * "whatever the user typed", and free text has exactly one destination
 * per surface. #1077 adds a second kind — a picked suggestion that
 * carries a DIMENSION — and those two kinds go to different parameters
 * on the same surfaces. Written inline that is a branch inside a
 * navigation side-effect, which is the shape you cannot assert on: the
 * acceptance for #1077 is about the resulting URL, not about the
 * dropdown's rendering, so the URL has to be a value somebody can hold.
 */

/** A commit that carries a typed dimension rather than free text.
 *
 *  `tag` is the only dimension today, and it is the one #1077 is about.
 *  The type is left open-ended deliberately: [SuggestKind] already
 *  carries `collection`, `post` and `asset`, and each of those has a
 *  destination that is a PAGE rather than a filter, which is a different
 *  feature. Until one of them is built they commit as free text, exactly
 *  as they did before. */
export type CommitTerm = { dimension: 'tag'; value: string };

/** Does the route at `pathname` render a result feed keyed off the
 *  global `q`?
 *
 *  This is a question about the SURFACE, not a list of paths that happen
 *  to be special, which is why it is a predicate and not a
 *  `pathname === '/' || pathname === '/search'` chain. A surface that
 *  consumes `q` is refined in place; every other surface is left in place
 *  and the user is taken to one that does. Adding the next one — #910's
 *  search-within-a-collection is the queued example — is a line in this
 *  function and nothing else.
 *
 *  `/search` qualifies since #850: it stopped being a different kind of
 *  page (its own text rows beside a facet rail) and became a result feed
 *  rendering the same cards through the same ContentGrid, with its own
 *  chrome on top. Before this it was treated as "some other page", so
 *  typing a refinement into the nav box on the one surface built for
 *  refining a search navigated you AWAY from it to browse (#1053).
 *
 *  It moved out of +layout.svelte in #1077 so that this file and the
 *  layout share ONE definition — `searchScope` and the commit
 *  destination are derived from the same answer, which is the property
 *  that keeps the suggest endpoint's corpus in step with where the
 *  commit lands. */
export function consumesGlobalQuery(pathname: string): boolean {
  return pathname === '/' || pathname === '/search';
}

/**
 * The URL a commit navigates to.
 *
 * # Free text: unchanged
 *
 * On a result surface, stay in place and update `q`; from anywhere else,
 * go to browse carrying it. That is what the nav box has done since the
 * search input went global.
 *
 * # ⭐ A TYPED TERM APPLIES THE STRUCTURED FILTER, AND DROPS `q`
 *
 * This is #1077's second half and the whole point of it. A picked tag
 * used to be committed as `?q=<tag>`, i.e. as free text against a
 * TSVECTOR — and a tag-only word is in no search document anywhere.
 * Measured on the dev seed: `fantasy`, `kit` and `lowpoly` are all FALSE
 * against their own asset's `search_text`, because a tag is not indexed
 * into an asset's document. So the dropdown offered a word and pressing
 * it returned nothing, which is the defect.
 *
 * Each surface gets the structured spelling IT can execute:
 *
 *   - `/search` → `filter=tag:<value>`, the shared grammar's `tag`
 *     dimension, which matches `asset_tag` on assets and `post_tags` on
 *     posts. This is the surface that can answer an ASSET-only tag, and
 *     answering it with the same number the facet rail counts is #1077's
 *     acceptance.
 *   - everywhere else → browse's `?tag=<value>`, which is the same
 *     dimension on the post entity (#1251 slice 2). Browse has a
 *     first-class control for it — the `#tag` heading, the follow
 *     toggle, the rail chip — so a tag commit lands the user on the page
 *     built for it rather than bouncing them to /search.
 *
 * `q` is DELETED rather than left beside the filter, and that is not
 * tidiness. The two would AND server-side, so keeping the typed prefix
 * would intersect the tag's results with a free-text match the tag word
 * cannot satisfy — the empty page this function exists to prevent,
 * arriving one conjunct later.
 *
 * Existing `filter=` tokens on /search are KEPT and the new one is
 * appended without duplicating, because a tag picked on top of a
 * selection is a refinement, exactly like ticking another bucket.
 */
export function commitTarget(current: URL, q: string, term?: CommitTerm): URL {
  const trimmed = q.trim();
  const inPlace = consumesGlobalQuery(current.pathname);

  if (term && term.dimension === 'tag' && term.value !== '') {
    if (current.pathname === '/search') {
      const url = new URL(current);
      url.searchParams.delete('q');
      const token = `tag:${term.value}`;
      if (!url.searchParams.getAll('filter').includes(token)) {
        url.searchParams.append('filter', token);
      }
      return url;
    }
    const url = new URL('/', current);
    url.searchParams.set('tag', term.value);
    return url;
  }

  const url = inPlace ? new URL(current) : new URL('/', current);
  if (trimmed === '') {
    url.searchParams.delete('q');
  } else {
    url.searchParams.set('q', trimmed);
  }
  return url;
}

/** Whether a commit to `target` from `current` should keep focus and
 *  scroll position — true exactly when the user is not being moved to a
 *  different surface. */
export function commitIsInPlace(current: URL, target: URL): boolean {
  return current.pathname === target.pathname;
}
