<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin trash — soft-deleted assets / posts / collections with a
  // Restore action (Phase 1.19; backend #320).
  //
  // Each entity's list endpoint accepts ?include_deleted=true (admin);
  // deleted rows now carry deleted_at / deleted_reason, so we fetch
  // include_deleted and keep the rows where deleted_at is set. Restore
  // is POST /admin/{type}/{id}/restore. Cap-gated server-side; a 403
  // renders a friendly error.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  type EntityKey = 'assets' | 'posts' | 'collections';

  interface TrashItem {
    id: string;
    label: string;          // title (assets/posts) or name (collections)
    deleted_at: string;
    deleted_reason?: string | null;
  }

  let active = $state<EntityKey>('assets');
  let items = $state<Record<EntityKey, TrashItem[]>>({ assets: [], posts: [], collections: [] });
  let loading = $state<Record<EntityKey, boolean>>({ assets: false, posts: false, collections: false });
  let loaded = $state<Record<EntityKey, boolean>>({ assets: false, posts: false, collections: false });
  let error = $state<string | null>(null);
  let restoring = $state<string | null>(null);
  let toast = $state<string | null>(null);

  const TABS: EntityKey[] = ['assets', 'posts', 'collections'];

  onMount(() => { void loadTab('assets'); });

  // Load whenever the active tab changes (once per tab; re-fetch on restore).
  $effect(() => {
    const k = active;
    if (!loaded[k] && !loading[k]) void loadTab(k);
  });

  async function loadTab(k: EntityKey): Promise<void> {
    loading = { ...loading, [k]: true };
    error = null;
    try {
      const path = k === 'assets' ? '/assets' : k === 'posts' ? '/posts' : '/collections';
      const r = await api.GET(path as '/assets', { params: { query: { include_deleted: true, limit: 200 } as never } });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.trash.load_error');
        return;
      }
      const rows = ((r.data as { items?: Record<string, unknown>[] } | undefined)?.items) ?? [];
      items = {
        ...items,
        [k]: rows
          .filter((row) => !!row.deleted_at)
          .map((row) => ({
            id: String(row.id),
            label: String((k === 'collections' ? row.name : row.title) ?? row.id),
            deleted_at: String(row.deleted_at),
            deleted_reason: (row.deleted_reason as string | null | undefined) ?? null,
          })),
      };
      loaded = { ...loaded, [k]: true };
    } finally {
      loading = { ...loading, [k]: false };
    }
  }

  async function restore(k: EntityKey, id: string): Promise<void> {
    if (restoring) return;
    restoring = id;
    toast = null;
    error = null;
    try {
      const path = `/admin/${k}/{id}/restore`;
      const r = await api.POST(path as '/admin/assets/{id}/restore', { params: { path: { id } } });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.trash.restore_error');
        return;
      }
      items = { ...items, [k]: items[k].filter((i) => i.id !== id) };
      toast = t('admin.trash.restored');
    } finally {
      restoring = null;
    }
  }

  function timeLabel(iso: string): string {
    try {
      return new Date(iso).toLocaleString();
    } catch {
      return iso;
    }
  }
</script>

<svelte:head><title>{t('admin.trash.title')} — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">{t('admin.trash.title')}</h2>
  <p class="text-sm text-fg-muted">{t('admin.trash.intro')}</p>
</header>

<nav class="mb-4 flex gap-1 border-b border-border">
  {#each TABS as k (k)}
    <button
      type="button"
      class={`rounded-t-md px-4 py-2 text-sm font-medium ${active === k ? 'border-b-2 border-accent text-fg' : 'text-fg-muted hover:text-fg'}`}
      onclick={() => (active = k)}
    >
      {t(`admin.trash.tab_${k}`)}
      {#if loaded[k] && items[k].length > 0}
        <span class="ml-1 rounded-full bg-surface-elevated px-1.5 py-0.5 text-[10px] text-fg-muted">{items[k].length}</span>
      {/if}
    </button>
  {/each}
</nav>

{#if toast}
  <p class="mb-3 rounded border border-success/40 bg-success/10 px-3 py-2 text-sm text-success">{toast}</p>
{/if}
{#if error}
  <p role="alert" class="mb-3 rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>
{/if}

{#if loading[active]}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if items[active].length === 0}
  <p class="rounded-md border border-border bg-surface px-4 py-8 text-center text-sm text-fg-muted">
    {t('admin.trash.empty')}
  </p>
{:else}
  <ul class="space-y-2">
    {#each items[active] as it (it.id)}
      <li class="flex flex-wrap items-center gap-3 rounded-lg border border-border bg-surface-elevated px-4 py-3">
        <div class="min-w-0 flex-1">
          <div class="truncate text-sm font-medium">{it.label}</div>
          <div class="mt-0.5 flex flex-wrap items-center gap-x-3 text-[11px] text-fg-muted">
            <span>{t('admin.trash.deleted_at')}: {timeLabel(it.deleted_at)}</span>
            {#if it.deleted_reason}
              <span>{t('admin.trash.reason')}: {it.deleted_reason}</span>
            {/if}
            <span class="font-mono opacity-60">{it.id}</span>
          </div>
        </div>
        <button
          type="button"
          onclick={() => restore(active, it.id)}
          disabled={restoring === it.id}
          class="rounded border border-accent bg-accent/10 px-3 py-1 text-xs font-medium text-accent hover:bg-accent/20 disabled:opacity-50"
        >
          {restoring === it.id ? t('admin.trash.restoring') : t('admin.trash.restore')}
        </button>
      </li>
    {/each}
  </ul>
{/if}
