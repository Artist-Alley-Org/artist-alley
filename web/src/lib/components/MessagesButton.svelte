<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Navbar messages bell (Phase 1.17.I-a).
  //
  // Mirror of NotificationsButton: 60s polling + dropdown of recent
  // threads. Click a thread to navigate to /account/messages/{peer_ref};
  // footer link to /account/messages for the full inbox.
  //
  // Unread count comes from /account/messages/unread-count which is
  // cache-backed server-side; polling stays cheap.

  import { onMount, onDestroy } from 'svelte';
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import Pill from '$components/Pill.svelte';
  import Menu from '$components/Menu.svelte';

  interface ThreadItem {
    peer_user_ref: number;
    peer_username: string;
    peer_display_name?: string | null;
    peer_avatar_url?: string | null;
    last_message_id: string;
    last_sender_user_ref: number;
    last_body: string;
    last_sent_at: string;
    last_read_at?: string | null;
    unread_count: number;
  }

  let unread = $state(0);
  let threads = $state<ThreadItem[]>([]);
  let pollHandle: ReturnType<typeof setInterval> | null = null;

  const POLL_MS = 60_000;

  onMount(() => {
    if (!auth.user) return;
    void refresh();
    pollHandle = setInterval(() => {
      if (document.visibilityState === 'visible') void refresh();
    }, POLL_MS);
  });

  onDestroy(() => {
    if (pollHandle) clearInterval(pollHandle);
  });

  async function refresh(): Promise<void> {
    await Promise.all([fetchUnread(), fetchThreads()]);
  }

  async function fetchUnread(): Promise<void> {
    try {
      const r = await api.GET('/account/messages/unread-count');
      if (r.data) unread = r.data.count;
    } catch {
      /* swallow */
    }
  }

  async function fetchThreads(): Promise<void> {
    try {
      const r = await api.GET('/account/messages', { params: { query: { limit: 10 } } });
      if (r.data?.threads) threads = r.data.threads as ThreadItem[];
    } catch {
      /* swallow */
    }
  }

  function openThread(t: ThreadItem): void {
    void goto(`/account/messages/${t.peer_user_ref}`);
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

  function initials(name: string | undefined | null): string {
    if (!name) return '?';
    const parts = name.trim().split(/\s+/);
    return (parts[0]?.[0] ?? '?').toUpperCase() + (parts[1]?.[0]?.toUpperCase() ?? '');
  }
</script>

<Menu align="right" panelClass="w-[24rem]">
  {#snippet trigger({ open })}
    <button
      type="button"
      title={t('nav.messages')}
      aria-label={t('nav.messages')}
      class="relative inline-flex h-9 w-9 items-center justify-center rounded-full text-fg-muted hover:bg-state-hover hover:text-fg"
      class:bg-state-hover={open}
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <path d="M4 4h16c1.1 0 2 .9 2 2v12c0 1.1-.9 2-2 2H4c-1.1 0-2-.9-2-2V6c0-1.1.9-2 2-2z" />
        <polyline points="22,6 12,13 2,6" />
      </svg>
      <Pill count={unread} />
    </button>
  {/snippet}

  {#snippet children()}
    <header class="flex items-center justify-between border-b border-border px-3 py-2">
      <span class="text-sm font-medium">{t('messages.title')}</span>
    </header>

    {#if threads.length === 0}
      <p class="px-3 py-6 text-center text-xs text-fg-muted">{t('messages.empty')}</p>
    {:else}
      <ul class="max-h-[24rem] overflow-y-auto">
        {#each threads as thread (thread.peer_user_ref)}
          {@const fromMe = auth.user && thread.last_sender_user_ref === auth.user.ref}
          <li>
            <button
              type="button"
              class="flex w-full items-start gap-2 px-3 py-2 text-left hover:bg-state-hover"
              class:bg-accent-container={thread.unread_count > 0}
              onclick={() => openThread(thread)}
            >
              <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-accent/20 text-xs font-semibold text-accent">
                {#if thread.peer_avatar_url}
                  <img src={thread.peer_avatar_url} alt="" class="h-full w-full rounded-full object-cover" />
                {:else}
                  {initials(thread.peer_display_name ?? thread.peer_username)}
                {/if}
              </div>
              <div class="min-w-0 flex-1">
                <div class="flex items-baseline justify-between gap-2">
                  <span class="truncate text-xs font-medium">
                    {thread.peer_display_name ?? '@' + thread.peer_username}
                  </span>
                  <span class="shrink-0 text-[10px] text-fg-muted">{timeAgo(thread.last_sent_at)}</span>
                </div>
                <div class="truncate text-xs text-fg-muted">
                  {fromMe ? t('messages.you_prefix') : ''}{thread.last_body}
                </div>
              </div>
              {#if thread.unread_count > 0}
                <span class="ml-1 shrink-0 rounded-full bg-accent px-1.5 text-[10px] font-medium text-on-accent">{thread.unread_count}</span>
              {/if}
            </button>
          </li>
        {/each}
      </ul>
    {/if}

    <footer class="border-t border-border px-3 py-2">
      <a href="/account/messages" class="block text-center text-xs text-accent hover:underline">
        {t('messages.see_all')}
      </a>
    </footer>
  {/snippet}
</Menu>
