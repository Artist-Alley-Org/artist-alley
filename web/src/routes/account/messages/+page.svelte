<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/messages — DM inbox (Phase 1.17.I-a).
  //
  // List of every peer the caller has DM'd, latest-message-first.
  // Click a row to open the thread at /account/messages/{peer_ref}.
  // Header shows total unread + "Compose" button (recipient picker
  // is a future commit; for now the user reaches a thread via the
  // FollowButton / profile / post-author chrome on other surfaces
  // that grows a "Message" affordance in I-b).

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';

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

  let threads = $state<ThreadItem[]>([]);
  let unread = $state(0);
  let loading = $state(true);
  let error = $state<string | null>(null);

  onMount(() => {
    void refresh();
  });

  async function refresh(): Promise<void> {
    loading = true;
    error = null;
    try {
      const [tr, u] = await Promise.all([
        api.GET('/account/messages', { params: { query: { limit: 100 } } }),
        api.GET('/account/messages/unread-count'),
      ]);
      if (tr.data?.threads) threads = tr.data.threads as ThreadItem[];
      if (u.data) unread = u.data.count;
    } catch (e) {
      error = e instanceof Error ? e.message : t('messages.load_error');
    } finally {
      loading = false;
    }
  }

  function open(peer: number): void {
    void goto(`/account/messages/${peer}`);
  }

  function initials(name: string | undefined | null): string {
    if (!name) return '?';
    const parts = name.trim().split(/\s+/);
    return (parts[0]?.[0] ?? '?').toUpperCase() + (parts[1]?.[0]?.toUpperCase() ?? '');
  }

  function dateLabel(iso: string): string {
    try {
      const d = new Date(iso);
      const now = new Date();
      const sameDay = d.toDateString() === now.toDateString();
      return sameDay ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleDateString();
    } catch {
      return '';
    }
  }
</script>

<svelte:head><title>{t('messages.title')} — {site.name}</title></svelte:head>

<header class="mb-6 flex items-baseline justify-between gap-3">
  <div>
    <h2 class="text-2xl font-semibold">{t('messages.title')}</h2>
    <p class="text-sm text-fg-muted">
      {#if unread > 0}
        {t('messages.unread_count', { count: String(unread) })}
      {:else}
        {t('messages.all_caught_up')}
      {/if}
    </p>
  </div>
</header>

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>
{:else if threads.length === 0}
  <p class="rounded-md border border-border bg-surface px-4 py-8 text-center text-sm text-fg-muted">
    {t('messages.empty_full')}
  </p>
{:else}
  <ul class="overflow-hidden rounded-lg border border-border bg-surface">
    {#each threads as th (th.peer_user_ref)}
      {@const fromMe = auth.user && th.last_sender_user_ref === auth.user.ref}
      <li class="border-b border-border last:border-b-0">
        <button
          type="button"
          class="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-state-hover"
          class:bg-accent-container={th.unread_count > 0}
          onclick={() => open(th.peer_user_ref)}
        >
          <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-accent/20 text-sm font-semibold text-accent">
            {#if th.peer_avatar_url}
              <img src={th.peer_avatar_url} alt="" class="h-full w-full rounded-full object-cover" />
            {:else}
              {initials(th.peer_display_name ?? th.peer_username)}
            {/if}
          </div>
          <div class="min-w-0 flex-1">
            <div class="flex items-baseline justify-between gap-2">
              <span class="truncate text-sm font-medium">
                {th.peer_display_name ?? '@' + th.peer_username}
              </span>
              <span class="shrink-0 text-xs text-fg-muted">{dateLabel(th.last_sent_at)}</span>
            </div>
            <div class="truncate text-xs text-fg-muted">
              {fromMe ? t('messages.from_me_prefix') : ''}{th.last_body}
            </div>
          </div>
          {#if th.unread_count > 0}
            <span class="shrink-0 rounded-full bg-accent px-2 text-xs font-medium text-on-accent">{th.unread_count}</span>
          {/if}
        </button>
      </li>
    {/each}
  </ul>
{/if}
