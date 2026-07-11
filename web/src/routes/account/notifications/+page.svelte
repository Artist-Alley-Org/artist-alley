<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/notifications — full inbox (Phase 1.17.I2-b).
  //
  // Cursor-paginated; "All / Unread" filter tabs; click an item to
  // mark-read + navigate to its target (post, user, etc.). Header
  // shows the unread count + a "Mark all read" button.

  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface NotificationItem {
    id: string;
    actor_user_ref?: number | null;
    verb: string;
    target_kind?: string | null;
    target_id?: string | null;
    payload?: Record<string, unknown>;
    read_at?: string | null;
    delivered_at: string;
    created_at: string;
  }

  let items = $state<NotificationItem[]>([]);
  let nextCursor = $state<string | null>(null);
  let unread = $state(0);
  let onlyUnread = $state(false);
  let loading = $state(true);
  let loadingMore = $state(false);
  let error = $state<string | null>(null);

  onMount(() => {
    void refresh();
  });

  // Re-fetch from page 1 whenever the unread-only toggle flips. The
  // $effect-of-onlyUnread pattern matches BrowsePage's approach so
  // a future filter pill set composes the same way.
  $effect(() => {
    void onlyUnread;
    items = [];
    nextCursor = null;
    void refresh();
  });

  async function refresh(): Promise<void> {
    loading = true;
    error = null;
    try {
      const params: Record<string, unknown> = { limit: 50 };
      if (onlyUnread) params.only_unread = true;
      const r = await api.GET('/account/notifications', { params: { query: params as never } });
      if (r.data) {
        items = (r.data.items ?? []) as NotificationItem[];
        nextCursor = (r.data.next_cursor as string | null) ?? null;
      }
      const c = await api.GET('/account/notifications/unread-count');
      if (c.data) unread = c.data.count;
    } catch (e) {
      error = e instanceof Error ? e.message : t('notifications.load_error');
    } finally {
      loading = false;
    }
  }

  async function loadMore(): Promise<void> {
    if (!nextCursor || loadingMore) return;
    loadingMore = true;
    try {
      const params: Record<string, unknown> = { limit: 50, cursor: nextCursor };
      if (onlyUnread) params.only_unread = true;
      const r = await api.GET('/account/notifications', { params: { query: params as never } });
      if (r.data) {
        items = [...items, ...((r.data.items ?? []) as NotificationItem[])];
        nextCursor = (r.data.next_cursor as string | null) ?? null;
      }
    } finally {
      loadingMore = false;
    }
  }

  async function markRead(id: string): Promise<void> {
    const target = items.find((n) => n.id === id);
    if (target && !target.read_at) {
      target.read_at = new Date().toISOString();
      items = [...items];
      if (unread > 0) unread = unread - 1;
    }
    try {
      await api.POST('/account/notifications/{id}/read', { params: { path: { id } } });
    } catch {
      void refresh();
    }
  }

  async function markAll(): Promise<void> {
    try {
      await api.POST('/account/notifications/read-all');
      unread = 0;
      items = items.map((n) => ({ ...n, read_at: n.read_at ?? new Date().toISOString() }));
    } catch {
      void refresh();
    }
  }

  function clickItem(n: NotificationItem): void {
    void markRead(n.id);
    if (n.target_kind === 'post' && n.target_id) {
      void goto(`/posts/${n.target_id}`);
    } else if (n.target_kind === 'user' && n.target_id) {
      void goto(`/users/by-ref/${n.target_id}`);
    }
    // Comments + license + request target_kinds stay on this page
    // until those domains get dedicated routes; the inbox card still
    // shows the excerpt + payload so the user knows what happened.
  }

  function verbLabel(n: NotificationItem): string {
    const key = `notifications.verb_${n.verb}`;
    const translated = t(key);
    return translated === key ? n.verb : translated;
  }

  function timeAgo(iso: string): string {
    try {
      const ms = Date.now() - new Date(iso).getTime();
      const m = Math.floor(ms / 60_000);
      if (m < 1) return t('common.just_now');
      if (m < 60) return `${m}m`;
      const h = Math.floor(m / 60);
      if (h < 24) return `${h}h`;
      const d = Math.floor(h / 24);
      return `${d}d`;
    } catch {
      return '';
    }
  }
</script>

<svelte:head><title>{t('notifications.title')} — artist-alley</title></svelte:head>

<header class="mb-6 flex flex-wrap items-baseline justify-between gap-3">
  <div>
    <h2 class="text-2xl font-semibold">{t('notifications.title')}</h2>
    <p class="text-sm text-fg-muted">
      {#if unread > 0}
        {t('notifications.unread_count', { count: String(unread) })}
      {:else}
        {t('notifications.all_caught_up')}
      {/if}
    </p>
  </div>
  <div class="flex items-center gap-2">
    <div class="inline-flex overflow-hidden rounded-md border border-border">
      <button
        type="button"
        class={`px-3 py-1.5 text-xs ${!onlyUnread ? 'bg-accent-container text-on-accent-container font-medium' : 'bg-surface text-fg-muted hover:bg-state-hover'}`}
        onclick={() => (onlyUnread = false)}
      >
        {t('notifications.tab_all')}
      </button>
      <button
        type="button"
        class={`px-3 py-1.5 text-xs ${onlyUnread ? 'bg-accent-container text-on-accent-container font-medium' : 'bg-surface text-fg-muted hover:bg-state-hover'}`}
        onclick={() => (onlyUnread = true)}
      >
        {t('notifications.tab_unread')}
      </button>
    </div>
    {#if unread > 0}
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-3 py-1.5 text-xs hover:bg-state-hover"
        onclick={markAll}
      >
        {t('notifications.mark_all_read')}
      </button>
    {/if}
  </div>
</header>

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>
{:else if items.length === 0}
  <p class="rounded-md border border-border bg-surface px-4 py-8 text-center text-sm text-fg-muted">
    {onlyUnread ? t('notifications.empty_unread') : t('notifications.empty')}
  </p>
{:else}
  <ul class="overflow-hidden rounded-lg border border-border bg-surface">
    {#each items as n (n.id)}
      <li class="border-b border-border last:border-b-0">
        <button
          type="button"
          class="flex w-full items-start gap-3 px-4 py-3 text-left hover:bg-state-hover"
          class:bg-accent-container={!n.read_at}
          onclick={() => clickItem(n)}
        >
          <div class="flex-1 min-w-0">
            <div class="flex items-baseline gap-2 text-sm">
              <span class="font-medium">{verbLabel(n)}</span>
              {#if n.payload?.post_title}
                <span class="truncate text-fg-muted">· {n.payload.post_title}</span>
              {/if}
            </div>
            {#if n.payload?.excerpt}
              <p class="mt-1 line-clamp-2 text-xs text-fg-muted">{n.payload.excerpt}</p>
            {/if}
          </div>
          <span class="shrink-0 text-xs text-fg-muted">{timeAgo(n.created_at)}</span>
        </button>
      </li>
    {/each}
  </ul>

  {#if nextCursor}
    <div class="mt-4 flex justify-center">
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover disabled:opacity-50"
        onclick={loadMore}
        disabled={loadingMore}
      >
        {loadingMore ? t('common.loading') : t('notifications.load_more')}
      </button>
    </div>
  {/if}
{/if}
