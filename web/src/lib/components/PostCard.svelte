<script lang="ts">
  // Grid-mode tile for a Post. Renders the cover asset (or first
  // member when no cover is pinned) as the square-cropped tile;
  // shows post title + author + member count on hover.
  //
  // The masonry / thumbnail / list view modes land in Phase 1.13.E
  // — this card is the Grid mode (default).

  interface AssetSummary {
    id: string;
    file_hash?: string | null;
  }
  interface PostMemberSummary {
    asset_id: string;
    asset: AssetSummary;
  }
  interface Post {
    id: string;
    title: string;
    cover_asset_id?: string | null;
    created_at: string;
    like_count: number;
    comment_count: number;
    members: PostMemberSummary[];
  }

  interface Props {
    post: Post;
  }

  let { post }: Props = $props();

  // Pick the cover asset id. Falls back to the first member; falls
  // back further to nothing (placeholder).
  const coverAssetId = $derived(
    post.cover_asset_id ??
      (post.members.length > 0 ? post.members[0].asset_id : null),
  );
  const hasFile = $derived(
    coverAssetId !== null &&
      post.members.some(
        (m) => m.asset_id === coverAssetId && !!m.asset.file_hash,
      ),
  );

  const colUrl = $derived(
    coverAssetId ? `/api/v1/assets/${coverAssetId}/variants/col` : '',
  );
  const fullUrl = $derived(
    coverAssetId ? `/api/v1/assets/${coverAssetId}/file` : '',
  );

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

  const memberCount = $derived(post.members.length);
  const created = $derived(new Date(post.created_at));
  const createdShort = $derived(
    created.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }),
  );
</script>

<a
  href="/posts/{post.id}"
  class="group block overflow-hidden rounded-lg bg-surface-elevated border border-border hover:border-fg-muted/60 transition-colors"
>
  <div class="relative aspect-square bg-surface">
    {#if hasFile && !imgError}
      <img
        src={colUrl}
        alt={post.title}
        loading="lazy"
        class="absolute inset-0 h-full w-full object-cover transition-transform duration-300 group-hover:scale-[1.02]"
        onerror={handleImgError}
      />
    {:else}
      <div class="absolute inset-0 flex items-center justify-center text-fg-muted/40">
        <svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="3" width="18" height="18" rx="2" />
          <circle cx="9" cy="9" r="2" />
          <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
        </svg>
      </div>
    {/if}

    <!-- Multi-asset indicator badge (top-right). -->
    {#if memberCount > 1}
      <div
        class="absolute top-2 right-2 inline-flex items-center gap-1 rounded-full bg-black/60 px-2 py-0.5 text-xs font-medium text-white backdrop-blur-sm"
        title="{memberCount} assets"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="3" width="14" height="14" rx="2" />
          <path d="M7 21h14a2 2 0 0 0 2-2V8" />
        </svg>
        {memberCount}
      </div>
    {/if}

    <!-- Hover overlay -->
    <div
      class="pointer-events-none absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/85 via-black/50 to-transparent
             p-3 opacity-0 group-hover:opacity-100 transition-opacity duration-200"
    >
      <p class="text-sm font-medium text-white line-clamp-2">{post.title || 'Untitled'}</p>
      <p class="text-xs text-white/70 mt-0.5">
        {createdShort}{post.like_count > 0 ? ` · ♥ ${post.like_count}` : ''}
      </p>
    </div>
  </div>
</a>
