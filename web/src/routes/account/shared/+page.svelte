<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/shared — "Shared with me" (#875).
  //
  // Where posts other people granted you access to accumulate. Backed
  // by GET /account/shared-posts, whose predicate is the SAME SQL
  // fragment the post read rule uses for its ACL disjunct — so a post
  // on this page is always one the caller can actually open, and a
  // revoked or expired grant drops off here the moment it stops
  // granting.
  //
  // This page exists because the browse feed deliberately does NOT
  // surface shares: `/posts` defaults to the org-only tier, and
  // widening that default would push low-volume, high-salience content
  // into the busiest query in the app. A share announces itself with a
  // notification and lands here instead.
  //
  // Rendering is the shared ContentGrid + PostCard, driven by the same
  // global browseView store as browse and the profile grids, so a post
  // looks and behaves identically wherever it is listed — including
  // PostCard's `?post={id}` overlay, which is why PostHost is mounted
  // below rather than letting the click dead-end on a page with no
  // modal host.

  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import ContentGrid from '$components/ContentGrid.svelte';
  import PostCard from '$components/PostCard.svelte';
  import PostListTable from '$components/PostListTable.svelte';
  import PostHost from '$components/PostHost.svelte';
  import type { CardCoverAsset } from '$components/cardAsset';

  // Same member shape browse declares (#595): the presentation fields
  // are part of the card feed contract, and a narrower local type is
  // how a surface silently loses the media-type badge and the sprite
  // scrub.
  interface PostMember {
    asset_id: string;
    sort_order: number;
    asset: CardCoverAsset;
  }
  interface SharedPost {
    id: string;
    author_user_ref: number;
    title: string;
    description: string;
    visibility: string;
    cover_asset_id?: string | null;
    posted_at: string;
    like_count: number;
    comment_count: number;
    tags: string[];
    members: PostMember[];
    created_at: string;
    updated_at: string;
  }

  const PAGE = 36;

  let items = $state<SharedPost[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(true);
  let loadingMore = $state(false);
  let loaded = $state(false);
  let error = $state<string | null>(null);

  onMount(() => {
    browseView.init();
    void load(null);
  });

  async function load(cursor: string | null): Promise<void> {
    if (cursor) loadingMore = true;
    else loading = true;
    error = null;
    try {
      const query: Record<string, string | number> = { limit: PAGE };
      if (cursor) query.cursor = cursor;
      const r = await api.GET('/account/shared-posts', {
        params: { query: query as never },
      });
      if (r.error || !r.data) {
        error =
          (r.error as { error?: string } | undefined)?.error ?? t('common.failed_to_load');
        return;
      }
      const pageItems = (r.data.items ?? []) as SharedPost[];
      items = cursor ? [...items, ...pageItems] : pageItems;
      nextCursor = (r.data.next_cursor as string | null) ?? null;
    } catch (e) {
      error = e instanceof Error ? e.message : t('common.failed_to_load');
    } finally {
      loading = false;
      loadingMore = false;
      loaded = true;
    }
  }

  // ?post={uuid} → overlay the post, same as browse. PostCard's click
  // handler writes the param onto whatever URL it is on, so without a
  // host here the primary click on this page would do nothing visible.
  const modalPostId = $derived(page.url.searchParams.get('post'));

  async function closeModal(): Promise<void> {
    const target = new URL(page.url);
    target.searchParams.delete('post');
    await goto(target.pathname + target.search, { keepFocus: true, noScroll: true });
  }

  const showEmpty = $derived(loaded && items.length === 0 && !error);
</script>

<svelte:head><title>{t('account.shared.title')} — {site.name}</title></svelte:head>

<header class="mb-6">
  <h2 class="text-2xl font-semibold">{t('account.shared.title')}</h2>
  <p class="mt-1 text-sm text-fg-muted">{t('account.shared.intro')}</p>
</header>

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
    {error}
  </p>
{:else if showEmpty}
  <div class="rounded-xl border border-dashed border-border p-12 text-center" data-testid="shared-empty">
    <p class="font-medium text-fg">{t('account.shared.empty')}</p>
    <p class="mt-1 text-sm text-fg-muted">{t('account.shared.empty_hint')}</p>
  </div>
{:else}
  <ContentGrid mode={browseView.mode} {items} tileMin={browseView.tileMin} {loading}>
    {#snippet card(item, mode)}
      {@const post = item as SharedPost}
      <PostCard {post} {mode} feed={mode === 'feed'} tileSizes={browseView.tileSizes} />
    {/snippet}
    {#snippet list()}
      <PostListTable {items} loading={false} />
    {/snippet}
  </ContentGrid>

  {#if nextCursor}
    <div class="mt-4 flex justify-center">
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover disabled:opacity-50"
        onclick={() => void load(nextCursor)}
        disabled={loadingMore}
      >
        {loadingMore ? t('common.loading') : t('account.shared.load_more')}
      </button>
    </div>
  {/if}
{/if}

{#if modalPostId}
  <PostHost postId={modalPostId} onClose={closeModal} />
{/if}
