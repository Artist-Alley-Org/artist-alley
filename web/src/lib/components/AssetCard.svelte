<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Single asset card for the browse / profile / collection grids. The
  // thumbnail itself (thumbhash placeholder, col variant, sprite scrub,
  // typed-doc + icon fallbacks, RS matte frame) lives in the shared
  // CardThumb component (#515 slice 1) so AssetCard and PostCard render
  // one identical treatment. This card adds the link wrapper + the
  // hover title overlay.

  import CardThumb from './CardThumb.svelte';
  import CardToolRow from './CardToolRow.svelte';

  interface Asset {
    id: string;
    title: string;
    file_hash?: string | null;
    file_extension?: string | null;
    asset_type: number;
    created_at: string;
    thumbhash?: string | null;
    preview_available?: boolean;
  }

  interface Props {
    asset: Asset;
  }

  let { asset }: Props = $props();

  // Hover state lives on the interactive <a> and feeds CardThumb's
  // sprite-scrub (keeps hover listeners off the presentation frame).
  let hovering = $state(false);

  const created = $derived(new Date(asset.created_at));
  const createdShort = $derived(
    created.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }),
  );
</script>

<!--
  Stretched-link pattern (#515 slice 2): the card is a container, not an
  <a>, so the tool row's <button>s aren't illegally nested in an anchor.
  A whole-card <a> covers the thumb for navigation (z-[1]); the title
  overlay is pointer-events-none; the tool row (z-20) captures its own
  clicks above the link.
-->
<div
  class="group relative block overflow-hidden rounded-lg bg-surface-elevated border border-border hover:border-fg-muted/60 transition-colors"
>
  <CardThumb
    assetId={asset.id}
    title={asset.title}
    thumbhash={asset.thumbhash}
    fileExtension={asset.file_extension}
    hasFileHash={!!asset.file_hash}
    previewAvailable={asset.preview_available}
    {hovering}
  >
    <!-- Whole-card navigation target. Hover here drives CardThumb's
         sprite-scrub (an interactive element, so no a11y warning). -->
    <a
      href="/assets/{asset.id}"
      onmouseenter={() => (hovering = true)}
      onmouseleave={() => (hovering = false)}
      class="absolute inset-0 z-[1]"
      aria-label={asset.title}
    ></a>

    <!-- Hover overlay with title (non-interactive — clicks fall to the link). -->
    <div
      class="pointer-events-none absolute inset-x-0 bottom-0 z-[2] bg-gradient-to-t from-black/85 via-black/50 to-transparent
             p-3 opacity-0 group-hover:opacity-100 transition-opacity duration-200"
    >
      <p class="text-sm font-medium text-white line-clamp-2">{asset.title}</p>
      <p class="text-xs text-white/70 mt-0.5">{createdShort}</p>
    </div>

    <!-- Quick-action tool row (info / share / add-to-collection). -->
    <CardToolRow assetId={asset.id} detailPath="/assets/{asset.id}" />
  </CardThumb>
</div>
