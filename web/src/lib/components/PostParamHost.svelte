<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The `?post={uuid}` viewer host — ONE mechanism, mounted by every
  // surface that shows post cards (#1130).
  //
  // # What it is for
  //
  // `PostCard`'s primary click does not navigate. It calls
  // `goto()` with `?post={id}` appended to WHATEVER URL it is currently
  // on, and something on that page is expected to notice and overlay the
  // post. Nothing about the card knows which page it is on, which is the
  // point — one card, every surface.
  //
  // The corollary is the bug: a surface that renders `PostCard` and does
  // NOT host the param has cards that write a query string and do
  // nothing else. No error, no dialog, no console output — the URL
  // changes and the page sits there. That was `/collections/{id}` (the
  // reported case), `/teams/{id}`, the user profile and
  // `/posts/by-asset/{id}`. It was never a regression on any of them:
  // the cards arrived without a host and the gap shipped with them.
  //
  // Three surfaces DID host it (browse, `/search`, `/account/shared`),
  // each with its own copy of the same eleven lines. Adding a fourth
  // copy is how the count reaches eight and how the close policy drifts
  // between them, so the watcher, the close policy and the sibling
  // navigation live here now and each host is one tag.
  //
  // # Where it must be DECLARED
  //
  // At the top level of a route's markup, NOT inside another dialog.
  // ADR 0067's amendment records why in full: `Modal` portals to the
  // nearest open `<dialog>` RESOLVED FROM WHERE IT IS DECLARED, so a
  // viewer declared inside some other dialog renders underneath that
  // dialog's top layer — present in the DOM, invisible on screen, and
  // passing every test that asserts on the DOM.
  //
  // `UserProfile.svelte` is a component rather than a route and mounts
  // this anyway: it IS the whole body of both profile permalinks and has
  // no dialog ancestor, so declaring it there is declaring it at route
  // level, once, for two routes.
  //
  // # The close policy
  //
  // Deleting the param and `goto`ing is the whole of it, and it is what
  // makes Back work for free: the open was a history entry, so the
  // browser's own Back lands on the URL without `?post=` and this host
  // unmounts, leaving the reader on the collection / team / profile they
  // opened from. That is #994's close-to-origin family, obtained rather
  // than re-implemented per surface.
  //
  // `keepFocus` keeps the browser from dropping focus on <body> during
  // the navigation; `restoreFocus` below is what puts it back on the
  // CARD, which is the part `keepFocus` cannot do because the element
  // that had focus is inside the dialog being torn down.

  import { tick } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import PostHost from './PostHost.svelte';

  interface Props {
    /** The post ids this surface currently shows, in display order.
     *  Supplied by hosts with a sibling concept (a feed, a member grid);
     *  ← / → inside the viewer then walk it. Omit and the arrows are
     *  inert, which is the right answer for a surface with no order. */
    ordered?: () => string[];
    /** Called when ← / → runs past the end of what is loaded. A paged
     *  host uses it to fetch the next page; the post does not auto-open,
     *  because which id is "next" is not known until the fetch lands. */
    onEndReached?: (dir: 'prev' | 'next') => void;
  }

  let { ordered, onEndReached }: Props = $props();

  const postId = $derived(page.url.searchParams.get('post'));

  // The element that had focus when the overlay opened, so Escape can
  // hand it back rather than dropping the reader at the top of the
  // document. Captured on the transition into an open state — reading
  // `document.activeElement` at close time is too late, the dialog owns
  // focus by then.
  //
  // `Modal` does exactly this for its own dialogs (see its header); the
  // viewer's dialog is opened by a URL change rather than by a click, so
  // the capture belongs on the thing that watches the URL.
  let opener: HTMLElement | null = null;
  let wasOpen = false;
  $effect(() => {
    const open = postId !== null;
    if (open && !wasOpen) {
      opener = document.activeElement as HTMLElement | null;
    }
    wasOpen = open;
  });

  async function close(): Promise<void> {
    const target = new URL(page.url);
    target.searchParams.delete('post');
    const restore = opener;
    opener = null;
    await goto(target.pathname + target.search, { keepFocus: true, noScroll: true });
    // After the navigation AND after Svelte has torn the dialog down —
    // focusing an element while a <dialog> is still up is a no-op, and
    // the reader silently loses their place in the grid.
    await tick();
    if (restore?.isConnected) restore.focus();
  }

  async function navigateSibling(dir: 'prev' | 'next'): Promise<void> {
    if (!postId || !ordered) return;
    const ids = ordered();
    const idx = ids.indexOf(postId);
    // Deep-linked from outside this surface: the open post is not in
    // the list, so there is no "next" to guess at. No-op rather than
    // jumping to an arbitrary end.
    if (idx < 0) return;
    const next = dir === 'next' ? idx + 1 : idx - 1;
    if (next < 0) return;
    if (next >= ids.length) {
      onEndReached?.(dir);
      return;
    }
    const target = new URL(page.url);
    target.searchParams.set('post', ids[next]);
    await goto(target.pathname + target.search, { keepFocus: true, noScroll: true });
  }
</script>

{#if postId}
  <PostHost
    {postId}
    onClose={close}
    onNavigateSibling={ordered ? navigateSibling : undefined}
  />
{/if}
