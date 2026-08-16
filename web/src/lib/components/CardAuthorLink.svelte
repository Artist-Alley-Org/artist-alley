<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // THE artist block on a card: avatar + name, clickable through to the
  // profile (#1047, extracted from #1111 + #1126's grid overlay).
  //
  // The owner's density table gives thumbnail "grid's visual language as
  // persistent chrome … artist (avatar+name, clickable → profile)". That
  // is the same block #1111 drew inside the grid overlay and #1126 made
  // into a link, so it moves here rather than being written a second
  // time — the #1063 rule, and the reason PR #1124 had two multi-asset
  // notations to complain about.
  //
  // TWO VARIANTS, ONE BLOCK. `overlay` sits ON artwork (white type, a
  // translucent disc, a hover wash) and `plain` sits on card chrome
  // (theme tokens). They differ in colour and nothing else — the same
  // markup, the same link target, the same avatar fallback — so a change
  // to what an artist block IS lands in both densities at once.
  //
  // WHAT IS NOT DECIDED HERE: whether there is an author at all. An
  // absent author is the ADR 0024 opt-out working (the server omits the
  // identity for an anonymous reader rather than sending a redacted
  // one), so the caller renders nothing — see PostCard's `author` prop
  // and users.LookupAuthors. This component is never handed a
  // placeholder identity to draw.
  //
  // `display_name` is the SERVER's resolution and is printed verbatim
  // (#1023): the real-name-vs-username ladder is authenticated-only at
  // rung 2, and re-deriving it in the client is how the anonymous rung
  // gets skipped.

  import { t } from '$stores/lang.svelte';

  interface Props {
    /** The renderable identity, exactly as the payload carries it. */
    author: {
      username: string;
      display_name: string;
      avatar_url?: string | null;
    };
    /** `overlay` = over artwork, `plain` = on card chrome. */
    variant?: 'overlay' | 'plain';
    /** Avatar edge in Tailwind sizing units. 40px over artwork (#1111's
     *  measurement); the persistent chrome densities run smaller because
     *  the block sits in a text row rather than over a picture. */
    size?: 'sm' | 'md';
  }

  let { author, variant = 'plain', size = 'md' }: Props = $props();

  function initialsOf(name: string): string {
    const parts = name.trim().split(/\s+/).filter(Boolean);
    if (parts.length === 0) return '?';
    if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
    return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
  }
  const initials = $derived(initialsOf(author.display_name ?? ''));

  const avatarClass = $derived(size === 'sm' ? 'h-6 w-6' : 'h-10 w-10');
</script>

<!-- `w-fit` is load-bearing, not tidying (#1126): without it the anchor
     is a block filling its column, so the clickable region reaches
     across empty space beside a short name and swallows clicks meant for
     the card underneath. `pointer-events-auto` re-enables the link
     inside an overlay that is otherwise `pointer-events-none`; it is
     harmless in the `plain` variant, where nothing switched them off. -->
<a
  href="/users/by-username/{author.username}"
  class="pointer-events-auto flex w-fit max-w-full items-center gap-2 rounded-full transition-colors
         focus-visible:outline-none focus-visible:ring-2
         {variant === 'overlay'
    ? 'hover:bg-white/15 focus-visible:ring-white/90'
    : 'hover:bg-surface focus-visible:ring-ring'}"
  title={t('card.feed.author_profile', { name: author.display_name })}
  data-testid="card-author-link"
  onclick={(e) => e.stopPropagation()}
>
  <!-- The initials disc is the COMMON case, not a broken state:
       avatar_url is null for every account that never uploaded one,
       which on a fresh install is all of them. -->
  <span
    class="flex shrink-0 items-center justify-center overflow-hidden rounded-full text-xs font-semibold
           {avatarClass}
           {variant === 'overlay'
      ? 'bg-white/15 text-white backdrop-blur-sm'
      : 'bg-accent/20 text-accent'}"
    data-testid="card-author-avatar"
  >
    {#if author.avatar_url}
      <img src={author.avatar_url} alt="" class="h-full w-full object-cover" />
    {:else}
      {initials}
    {/if}
  </span>
  <span
    class="truncate pr-2 text-xs {variant === 'overlay'
      ? 'text-white/80 group-hover:text-white'
      : 'text-fg-muted'}"
    data-testid="card-author">{author.display_name}</span
  >
</a>
