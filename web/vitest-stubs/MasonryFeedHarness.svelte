<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // A wall with a card that READS ITS ROW, for the #1103 feed-swap tests.
  //
  // This cannot be a `createRawSnippet`: a raw snippet's `render` runs
  // once and hands back a string, so it never re-reads its argument —
  // which is exactly the read the bug turns into a throw. Only a real
  // `{#snippet}` re-evaluates when the row underneath it changes, so the
  // harness is a component rather than a helper in the test file.
  //
  // `item.id` with no item is a TypeError, and that is the point, not an
  // oversight: PostCard reads `post.members`, CollectionCard reads its
  // own row, and #1103's uncaught
  // "Cannot read properties of undefined (reading 'members')" is that
  // read landing on an index the current feed no longer has.
  import MasonryColumns from '$components/MasonryColumns.svelte';

  let {
    items,
    tileMin = '22rem',
    loading = false,
  }: { items: Array<{ id: string }>; tileMin?: string; loading?: boolean } = $props();
</script>

{#snippet card(item: { id: string })}
  <span data-card-id={item.id}>{item.id}</span>
{/snippet}

<MasonryColumns {items} {tileMin} {loading} {card} />
