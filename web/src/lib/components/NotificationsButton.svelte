<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Navbar notifications bell (Phase 1.17.I2-b).
  //
  // Two responsibilities:
  //   1. Render the bell + a pill with the caller's unread count.
  //   2. Open a dropdown listing the most recent N notifications
  //      with click-to-mark-read + "See all" link to the full
  //      /account/notifications page.
  //
  // The count comes from /account/notifications/unread-count which
  // is cache-backed server-side (per-recipient LRU + cache.Registry
  // NOTIFY) so polling stays cheap. Initial fetch on mount; 60s
  // poll while the page is visible (Page Visibility API gates so
  // background tabs don't waste cycles).
  //
  // The Notify writer invalidates the per-recipient unread count on
  // every insert; in a future real-time-update phase we'll add an
  // SSE channel so the bell flips instantly instead of waiting for
  // the next poll tick.

  import { onMount, onDestroy } from 'svelte';
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import Pill from '$components/Pill.svelte';
  import Menu from '$components/Menu.svelte';

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

  let unread = $state(0);
  let recent = $state<NotificationItem[]>([]);
  let loadingRecent = $state(false);
  let pollHandle: ReturnType<typeof setInterval> | null = null;

  // 60s polling is the sweet spot — fresh enough that a like /
  // comment shows up before the user wonders why the bell hasn't
  // moved, infrequent enough to be free in aggregate.
  const POLL_MS = 60_000;

  onMount(() => {
    if (!auth.user) return;
    void refresh();
    pollHandle = setInterval(() => {
      if (document.visibilityState === 'visible') void refresh();
    }, POLL_MS);
  });

  // Refresh batch: unread count + recent list together. Both reads
  // are cheap (count is cache-backed; list is the indexed inbox
  // page) so coalescing them is fine and keeps the dropdown fresh
  // even before the user opens it.
  async function refresh(): Promise<void> {
    await Promise.all([fetchUnread(), fetchRecent()]);
  }

  onDestroy(() => {
    if (pollHandle) clearInterval(pollHandle);
  });

  async function fetchUnread(): Promise<void> {
    try {
      const r = await api.GET('/account/notifications/unread-count');
      if (r.data) unread = r.data.count;
    } catch {
      /* swallow — bell stays at last known value */
    }
  }

  async function fetchRecent(): Promise<void> {
    if (loadingRecent) return;
    loadingRecent = true;
    try {
      const r = await api.GET('/account/notifications', {
        params: { query: { limit: 10 } },
      });
      if (r.data?.items) recent = r.data.items as NotificationItem[];
    } finally {
      loadingRecent = false;
    }
  }

  async function markRead(id: string): Promise<void> {
    // Optimistic: drop the unread flair immediately + decrement the
    // pill; if the POST fails the next poll re-syncs.
    const target = recent.find((n) => n.id === id);
    if (target && !target.read_at) {
      target.read_at = new Date().toISOString();
      recent = [...recent]; // trigger reactivity
      if (unread > 0) unread = unread - 1;
    }
    try {
      await api.POST('/account/notifications/{id}/read', {
        params: { path: { id } },
      });
    } catch {
      void fetchUnread();
    }
  }

  async function markAll(): Promise<void> {
    try {
      const r = await api.POST('/account/notifications/read-all');
      if (r.data) {
        unread = 0;
        recent = recent.map((n) => ({ ...n, read_at: n.read_at ?? new Date().toISOString() }));
      }
    } catch {
      void fetchUnread();
    }
  }

  function clickItem(n: NotificationItem): void {
    void markRead(n.id);
    // Route to whatever the notification points at. Same target_kind
    // → route map the inbox page uses.
    if (n.target_kind === 'post' && n.target_id) {
      void goto(`/posts/${n.target_id}`);
    } else if (n.target_kind === 'user' && n.target_id) {
      void goto(`/users/by-ref/${n.target_id}`);
    } else if (n.target_kind === 'request') {
      // #881 — both request surfaces live here: the owner's decision
      // queue and the requester's own list.
      void goto('/account/requests');
    } else if (n.target_kind === 'comment' && n.target_id) {
      // Comments don't have a dedicated route yet — fall through to
      // notifications page where the renderer at least shows the
      // excerpt.
      void goto('/account/notifications');
    } else {
      void goto('/account/notifications');
    }
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

  function verbLabel(n: NotificationItem): string {
    const key = `notifications.verb_${n.verb}`;
    const translated = t(key);
    return translated === key ? n.verb : translated;
  }
</script>

<Menu
  align="right"
  panelClass="w-[24rem]"
  triggerClass="inline-flex rounded-full focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
>
  {#snippet trigger({ open })}
    <!-- #1109 — a SPAN, not a button. `Menu` already renders the real
         `<button aria-haspopup="menu">` around this snippet, and a
         button inside a button is invalid markup that put two focus
         stops on one control. The chip keeps every visual class; the
         wrapper carries the box, the matching radius and the ring. -->
    <span
      title={t('nav.notifications')}
      aria-label={t('nav.notifications')}
      class="relative inline-flex h-9 w-9 items-center justify-center rounded-full text-fg-muted hover:bg-state-hover hover:text-fg"
      class:bg-state-hover={open}
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9" />
        <path d="M13.73 21a2 2 0 0 1-3.46 0" />
      </svg>
      <Pill count={unread} />
    </span>
  {/snippet}

  {#snippet children()}
    <header class="flex items-center justify-between border-b border-border px-3 py-2">
      <span class="text-sm font-medium">{t('notifications.title')}</span>
      {#if unread > 0}
        <button
          type="button"
          class="text-xs text-accent hover:underline"
          onclick={markAll}
        >
          {t('notifications.mark_all_read')}
        </button>
      {/if}
    </header>

    {#if loadingRecent}
      <p class="px-3 py-4 text-center text-xs text-fg-muted">{t('common.loading')}</p>
    {:else if recent.length === 0}
      <p class="px-3 py-6 text-center text-xs text-fg-muted">{t('notifications.empty')}</p>
    {:else}
      <ul class="max-h-[24rem] overflow-y-auto">
        {#each recent as n (n.id)}
          <li>
            <button
              type="button"
              class="flex w-full items-start gap-2 px-3 py-2 text-left hover:bg-state-hover"
              class:bg-accent-container={!n.read_at}
              onclick={() => clickItem(n)}
            >
              <span class="flex-1 text-xs">
                <span class="font-medium">{verbLabel(n)}</span>
                {#if n.payload?.post_title}
                  <span class="text-fg-muted"> · {n.payload.post_title}</span>
                {/if}
                {#if n.payload?.excerpt}
                  <div class="mt-0.5 truncate text-fg-muted">{n.payload.excerpt}</div>
                {/if}
              </span>
              <span class="shrink-0 text-[10px] text-fg-muted">{timeAgo(n.created_at)}</span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}

    <footer class="border-t border-border px-3 py-2">
      <a href="/account/notifications" class="block text-center text-xs text-accent hover:underline">
        {t('notifications.see_all')}
      </a>
    </footer>
  {/snippet}
</Menu>
