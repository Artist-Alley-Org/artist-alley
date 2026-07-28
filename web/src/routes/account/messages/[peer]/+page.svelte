<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/messages/[peer] — DM thread view (Phase 1.17.I-a).
  //
  // Bottom-anchored chat shape: newest messages at the bottom,
  // compose box pinned. Scrolls to the latest on load + after
  // sending. The server returns newest-first; we reverse for
  // display so the natural reading order matches a chat client.
  //
  // Marks the thread as read on mount via POST .../read so the
  // envelope pill + inbox unread counter decrement immediately.

  import { onMount, tick } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';

  interface DirectMessage {
    id: string;
    sender_user_ref: number;
    recipient_user_ref: number;
    body: string;
    sent_at: string;
    read_at?: string | null;
  }

  const peerRef = $derived(Number(page.params.peer));

  let messages = $state<DirectMessage[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(true);
  let sending = $state(false);
  let error = $state<string | null>(null);
  let draft = $state('');
  let scrollEl: HTMLDivElement | undefined = $state();
  // Peer display info — we don't have a dedicated /users/{ref} surface
  // yet, so we lift it off the inbox thread row.
  let peerName = $state('');

  onMount(() => {
    void refresh();
  });

  // Re-fetch when the route param changes (in-place navigation
  // between threads via the sidebar would otherwise stay stale).
  $effect(() => {
    void peerRef;
    if (peerRef > 0) {
      messages = [];
      nextCursor = null;
      void refresh();
    }
  });

  async function refresh(): Promise<void> {
    if (!peerRef) return;
    loading = true;
    error = null;
    try {
      const r = await api.GET('/account/messages/{peer_ref}', {
        params: { path: { peer_ref: peerRef }, query: { limit: 100 } },
      });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('messages.load_error');
        return;
      }
      if (r.data) {
        // Server returns newest-first; reverse for chat-style display.
        messages = ((r.data.items ?? []) as DirectMessage[]).slice().reverse();
        nextCursor = (r.data.next_cursor as string | null) ?? null;
      }
      // Mark-read in the background. We don't await — the user
      // shouldn't wait on the count update to read.
      void api.POST('/account/messages/{peer_ref}/read', {
        params: { path: { peer_ref: peerRef } },
      });
      // Resolve peer display name via the inbox row (no /users/{ref}
      // surface yet that gives username + display_name in one shot).
      try {
        const t = await api.GET('/account/messages', { params: { query: { limit: 200 } } });
        const match = ((t.data?.threads ?? []) as Array<{ peer_user_ref: number; peer_display_name?: string | null; peer_username: string }>).find((x) => x.peer_user_ref === peerRef);
        if (match) peerName = match.peer_display_name ?? '@' + match.peer_username;
      } catch {
        /* swallow */
      }
      await tick();
      scrollToBottom();
    } finally {
      loading = false;
    }
  }

  function scrollToBottom(): void {
    if (scrollEl) scrollEl.scrollTop = scrollEl.scrollHeight;
  }

  async function send(): Promise<void> {
    const body = draft.trim();
    if (!body || sending) return;
    sending = true;
    error = null;
    try {
      const r = await api.POST('/account/messages/{peer_ref}', {
        params: { path: { peer_ref: peerRef } },
        body: { body },
      });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('messages.send_error');
        return;
      }
      if (r.data) {
        messages = [...messages, r.data as DirectMessage];
        draft = '';
        await tick();
        scrollToBottom();
      }
    } finally {
      sending = false;
    }
  }

  function onKey(e: KeyboardEvent): void {
    // Enter sends; Shift+Enter newlines.
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      void send();
    }
  }

  function timeStamp(iso: string): string {
    try {
      const d = new Date(iso);
      return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    } catch {
      return '';
    }
  }
</script>

<svelte:head><title>{peerName || t('messages.thread')} — {site.name}</title></svelte:head>

<header class="mb-4 flex items-center justify-between gap-3">
  <div>
    <button
      type="button"
      class="text-xs text-accent hover:underline"
      onclick={() => goto('/account/messages')}
    >
      &larr; {t('messages.back_to_inbox')}
    </button>
    <h2 class="mt-1 text-xl font-semibold">{peerName || `Peer #${peerRef}`}</h2>
  </div>
</header>

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>
{:else}
  <div
    bind:this={scrollEl}
    class="mb-3 max-h-[60vh] overflow-y-auto rounded-lg border border-border bg-surface p-3"
  >
    {#if messages.length === 0}
      <p class="py-8 text-center text-sm text-fg-muted">{t('messages.thread_empty')}</p>
    {:else}
      <ul class="space-y-2">
        {#each messages as m (m.id)}
          {@const mine = auth.user && m.sender_user_ref === auth.user.ref}
          <li class="flex" class:justify-end={mine}>
            <div
              class={mine
                ? 'max-w-[70%] rounded-lg rounded-br-sm bg-accent px-3 py-2 text-sm text-on-accent'
                : 'max-w-[70%] rounded-lg rounded-bl-sm bg-surface-elevated px-3 py-2 text-sm text-fg'}
            >
              <p class="whitespace-pre-wrap break-words">{m.body}</p>
              <div class="mt-1 text-right text-[10px] opacity-70">{timeStamp(m.sent_at)}</div>
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  </div>

  <form
    class="flex items-end gap-2"
    onsubmit={(e) => {
      e.preventDefault();
      void send();
    }}
  >
    <textarea
      class="min-h-[2.5rem] flex-1 resize-y rounded-md border border-border-strong bg-surface px-3 py-2 text-sm focus:outline-none focus:ring-1 focus:ring-accent"
      placeholder={t('messages.compose_placeholder')}
      bind:value={draft}
      onkeydown={onKey}
      maxlength="5000"
      disabled={sending}
      rows="2"
    ></textarea>
    <button
      type="submit"
      class="shrink-0 rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent hover:bg-accent/90 disabled:opacity-50"
      disabled={sending || draft.trim() === ''}
    >
      {sending ? t('common.loading') : t('messages.send')}
    </button>
  </form>
{/if}
