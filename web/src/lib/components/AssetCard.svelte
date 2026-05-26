<script lang="ts">
  // Single asset card for the browse grid (ArtStation-style: square
  // crop, equal-size tiles). Three rendering states:
  //   1. Has file_hash → fetch the `col` (collection) variant; on
  //      404 (likely — variant pipeline lands in a later phase),
  //      fall back to the original file via /assets/{id}/file.
  //   2. Has file_hash but the image fails to load → placeholder.
  //   3. No file_hash (e.g. metadata-only asset) → placeholder.
  //
  // The Masonry view (real aspect ratios) is a separate view mode
  // landing in Phase 1.13.E; this component renders the Grid mode.
  //
  // Hover overlay shows the title (ArtStation-style). Click takes
  // the user to the asset detail page (1.13.F).

  interface Asset {
    id: string;
    title: string;
    file_hash?: string | null;
    file_extension?: string | null;
    resource_type: number;
    created_at: string;
  }

  interface Props {
    asset: Asset;
  }

  let { asset }: Props = $props();

  // Resolve URL chain: collection variant → original file → placeholder.
  // The variant pipeline is deferred, so the original is the practical
  // path right now. The browser handles whatever aspect ratio the
  // original has and object-cover keeps the grid uniform.
  const colUrl = $derived(asset.file_hash ? `/api/v1/assets/${asset.id}/variants/col` : '');
  const fullUrl = $derived(asset.file_hash ? `/api/v1/assets/${asset.id}/file` : '');

  let imgError = $state(false);
  let triedFallback = $state(false);

  function handleImgError(e: Event) {
    const img = e.currentTarget as HTMLImageElement;
    if (!triedFallback && fullUrl) {
      triedFallback = true;
      img.src = fullUrl;
      return;
    }
    imgError = true;
  }

  const created = $derived(new Date(asset.created_at));
  const createdShort = $derived(
    created.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  );
</script>

<a
  href="/assets/{asset.id}"
  class="group block overflow-hidden rounded-lg bg-surface-elevated border border-border hover:border-fg-muted/60 transition-colors"
>
  <div class="relative aspect-square bg-surface">
    {#if asset.file_hash && !imgError}
      <img
        src={colUrl}
        alt={asset.title}
        loading="lazy"
        class="absolute inset-0 h-full w-full object-cover transition-transform duration-300 group-hover:scale-[1.02]"
        onerror={handleImgError}
      />
    {:else}
      <!-- Placeholder: file-less or load failure. Subtle gradient + icon. -->
      <div class="absolute inset-0 flex items-center justify-center text-fg-muted/40">
        <svg
          xmlns="http://www.w3.org/2000/svg"
          width="48"
          height="48"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <rect x="3" y="3" width="18" height="18" rx="2" />
          <circle cx="9" cy="9" r="2" />
          <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
        </svg>
      </div>
    {/if}

    <!-- Hover overlay with title -->
    <div
      class="pointer-events-none absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/85 via-black/50 to-transparent
             p-3 opacity-0 group-hover:opacity-100 transition-opacity duration-200"
    >
      <p class="text-sm font-medium text-white line-clamp-2">{asset.title}</p>
      <p class="text-xs text-white/70 mt-0.5">{createdShort}</p>
    </div>
  </div>
</a>
