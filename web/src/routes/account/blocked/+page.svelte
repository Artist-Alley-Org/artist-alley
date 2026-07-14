<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/blocked — caller's block list (Phase 1.17.G2).
  //
  // Private to the caller; the reverse direction ("who blocked me")
  // is deliberately NOT exposed per the standard social-platform
  // privacy convention. Lists the users the caller has blocked with
  // the optional private reason + an inline Unblock button.
  //
  // Each Unblock click hits DELETE /users/{ref}/block + drops the
  // row optimistically. We don't surface re-following from this
  // surface — the block auto-unfollowed both directions at block
  // time, so the user has to re-follow explicitly from the
  // profile / post-author header.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface BlockedUser {
    ref: number;
    username: string;
    display_name?: string;
    avatar_url?: string;
    reason?: string;
    since: string;
  }

  let users = $state<BlockedUser[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);
  let pendingRefs = $state<Set<number>>(new Set());

  onMount(() => {
    void load();
  });

  async function load(): Promise<void> {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/account/blocked', {
        params: { query: { limit: 200 } },
      });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('account.blocked.load_error');
        return;
      }
      users = (r.data?.users ?? []) as BlockedUser[];
    } finally {
      loading = false;
    }
  }

  async function unblock(ref: number): Promise<void> {
    const next = new Set(pendingRefs);
    next.add(ref);
    pendingRefs = next;
    try {
      const r = await api.DELETE('/users/{ref}/block', {
        params: { path: { ref } },
      });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('account.blocked.unblock_error');
        return;
      }
      // Drop the row optimistically; the server returned 204 so the
      // edge is gone. No need to re-fetch the whole list.
      users = users.filter((u) => u.ref !== ref);
    } finally {
      const after = new Set(pendingRefs);
      after.delete(ref);
      pendingRefs = after;
    }
  }

  function initials(name: string | undefined): string {
    if (!name) return '?';
    const parts = name.trim().split(/\s+/);
    return (parts[0]?.[0] ?? '?').toUpperCase() + (parts[1]?.[0]?.toUpperCase() ?? '');
  }

  function formatDate(iso: string): string {
    try {
      return new Date(iso).toLocaleDateString();
    } catch {
      return iso;
    }
  }
</script>

<svelte:head><title>{t('account.blocked.title')} — {site.name}</title></svelte:head>

<header class="mb-6">
  <h2 class="text-2xl font-semibold">{t('account.blocked.title')}</h2>
  <p class="text-sm text-fg-muted">{t('account.blocked.intro')}</p>
</header>

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>
{:else if users.length === 0}
  <p class="rounded-md border border-border bg-surface px-4 py-6 text-center text-sm text-fg-muted">
    {t('account.blocked.empty')}
  </p>
{:else}
  <div class="overflow-hidden rounded-lg border border-border bg-surface">
    <table class="w-full text-sm">
      <thead class="bg-surface-elevated text-xs uppercase tracking-wider text-fg-muted">
        <tr>
          <th class="px-4 py-2 text-left font-medium">{t('account.blocked.col_user')}</th>
          <th class="px-4 py-2 text-left font-medium">{t('account.blocked.col_reason')}</th>
          <th class="px-4 py-2 text-left font-medium">{t('account.blocked.col_since')}</th>
          <th class="px-4 py-2 text-right font-medium">{t('account.blocked.col_actions')}</th>
        </tr>
      </thead>
      <tbody>
        {#each users as u (u.ref)}
          {@const isPending = pendingRefs.has(u.ref)}
          <tr class="border-t border-border hover:bg-surface-elevated/40">
            <td class="px-4 py-3">
              <div class="flex items-center gap-3">
                <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-accent/20 text-xs font-semibold text-accent">
                  {#if u.avatar_url}
                    <img src={u.avatar_url} alt="" class="h-full w-full rounded-full object-cover" />
                  {:else}
                    {initials(u.display_name ?? u.username)}
                  {/if}
                </div>
                <div class="min-w-0">
                  <div class="truncate font-medium">{u.display_name ?? u.username}</div>
                  <div class="truncate text-xs text-fg-muted">@{u.username}</div>
                </div>
              </div>
            </td>
            <td class="px-4 py-3 text-xs text-fg-muted">
              {u.reason ?? '—'}
            </td>
            <td class="px-4 py-3 text-xs text-fg-muted">
              {formatDate(u.since)}
            </td>
            <td class="px-4 py-3 text-right">
              <button
                type="button"
                class="rounded-md border border-border bg-surface px-3 py-1 text-xs hover:bg-state-hover disabled:opacity-50"
                onclick={() => unblock(u.ref)}
                disabled={isPending}
              >
                {isPending ? t('common.loading') : t('account.blocked.unblock')}
              </button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
