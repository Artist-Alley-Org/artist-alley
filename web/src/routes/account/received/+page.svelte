<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/received: assets another studio shared with this instance
  // (#1574).
  //
  // Backed by GET /federation/received-assets, whose rule the server
  // evaluates on every request: a complete record, of public tier, from an
  // enabled and connected peer, under a live inbound grant from that peer
  // that names no recipient or names the caller. A revoked or expired
  // share drops the record off this page on the next load.
  //
  // Like /account/shared, this is the ONLY place these appear: received
  // records are not local assets and never reach browse or search. File
  // names and types here are the sender's claims unless labelled detected.
  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { formatBytes, type ReceivedAsset } from '$lib/received/received';

  const PAGE = 50;

  let items = $state<ReceivedAsset[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(true);
  let loadingMore = $state(false);
  let loaded = $state(false);
  let error = $state<string | null>(null);

  onMount(() => {
    void load(null);
  });

  async function load(cursor: string | null): Promise<void> {
    if (cursor) loadingMore = true;
    else loading = true;
    error = null;
    try {
      const query: Record<string, string | number> = { limit: PAGE };
      if (cursor) query.cursor = cursor;
      const r = await api.GET('/federation/received-assets', {
        params: { query: query as never },
      });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('common.failed_to_load');
        return;
      }
      const page = (r.data.items ?? []) as ReceivedAsset[];
      items = cursor ? [...items, ...page] : page;
      nextCursor = (r.data.next_cursor as string | null | undefined) ?? null;
    } catch (e) {
      error = e instanceof Error ? e.message : t('common.failed_to_load');
    } finally {
      loading = false;
      loadingMore = false;
      loaded = true;
    }
  }

  const showEmpty = $derived(loaded && items.length === 0 && !error);
</script>

<svelte:head><title>{t('account.received.title')} — {site.name}</title></svelte:head>

<header class="mb-6">
  <h2 class="text-2xl font-semibold">{t('account.received.title')}</h2>
  <p class="mt-1 text-sm text-fg-muted">{t('account.received.intro')}</p>
</header>

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">
    {error}
  </p>
{:else if showEmpty}
  <div class="rounded-xl border border-dashed border-border p-12 text-center" data-testid="received-empty">
    <p class="font-medium text-fg">{t('account.received.empty')}</p>
    <p class="mt-1 text-sm text-fg-muted">{t('account.received.empty_hint')}</p>
  </div>
{:else}
  <ul class="divide-y divide-border rounded-xl border border-border" data-testid="received-list">
    {#each items as item (item.id)}
      <li>
        <a
          href={`/account/received/${item.id}`}
          class="flex flex-wrap items-baseline justify-between gap-x-6 gap-y-1 px-4 py-3 hover:bg-state-hover"
          data-testid="received-item"
        >
          <span class="min-w-0">
            <span class="block truncate font-medium text-fg">
              {item.original.declared_original_filename ?? item.original.sha256.slice(0, 12)}
            </span>
            <span class="block text-xs text-fg-muted">
              {t('account.received.from')} {item.peer_name ?? item.peer_url}
            </span>
          </span>
          <span class="text-xs text-fg-muted">
            {item.original.detected_media_type ?? item.original.declared_media_type}
            · {formatBytes(item.original.size_bytes)}
            · {t('account.received.received_at')}
            {new Date(item.completed_at).toLocaleString()}
          </span>
        </a>
      </li>
    {/each}
  </ul>

  {#if nextCursor}
    <div class="mt-4 flex justify-center">
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover disabled:opacity-50"
        onclick={() => void load(nextCursor)}
        disabled={loadingMore}
      >
        {loadingMore ? t('common.loading') : t('account.received.load_more')}
      </button>
    </div>
  {/if}
{/if}
