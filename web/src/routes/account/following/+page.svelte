<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/following — the caller's social graph, both directions (#600).
  //
  // Read-only over two endpoints that already existed:
  //   GET /users/{ref}/following   listUserFollowing
  //   GET /users/{ref}/followers   listUserFollowers
  // plus DELETE /users/{ref}/follow (unfollowUser) for the inline
  // action. Nothing new server-side.
  //
  // Both lists are anchored on `auth.user.ref` — the endpoints are
  // per-user, not per-caller, so there is no "me" alias to use. The
  // page therefore waits for `auth.ready` before fetching.
  //
  // Unfollow appears only on the Following tab. The Followers tab is
  // informational: there is no "remove a follower" endpoint (and no
  // product decision that there should be one) — blocking is the
  // sanctioned way to sever an incoming edge, and that lives on the
  // profile page.
  //
  // Scope note: the account tile used to promise "people and tags you
  // follow". There is no tag-follow endpoint anywhere in the API, so
  // the blurb was corrected rather than the page faking it.

  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';

  /** components/schemas/SocialUserSummary */
  interface SocialUser {
    ref: number;
    username: string;
    display_name?: string | null;
    avatar_url?: string | null;
    since?: string;
  }

  type Tab = 'following' | 'followers';

  let tab = $state<Tab>('following');
  let following = $state<SocialUser[]>([]);
  let followers = $state<SocialUser[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);
  let pendingRefs = $state<Set<number>>(new Set());

  const rows = $derived(tab === 'following' ? following : followers);

  // auth.refresh() runs in the root layout; `ready` flips once /auth/me
  // has resolved. Fetching before that would send ref=undefined.
  $effect(() => {
    if (!auth.ready) return;
    const me = auth.user?.ref;
    if (me == null) {
      loading = false;
      return;
    }
    void load(me);
  });

  async function load(me: number): Promise<void> {
    loading = true;
    error = null;
    try {
      const [f, fo] = await Promise.all([
        api.GET('/users/{ref}/following', {
          params: { path: { ref: me }, query: { limit: 200 } },
        }),
        api.GET('/users/{ref}/followers', {
          params: { path: { ref: me }, query: { limit: 200 } },
        }),
      ]);
      const firstErr = f.error ?? fo.error;
      if (firstErr) {
        error = (firstErr as { error?: string } | undefined)?.error ?? t('account.following.load_error');
        return;
      }
      following = (f.data?.users ?? []) as SocialUser[];
      followers = (fo.data?.users ?? []) as SocialUser[];
    } finally {
      loading = false;
    }
  }

  async function unfollow(ref: number): Promise<void> {
    const next = new Set(pendingRefs);
    next.add(ref);
    pendingRefs = next;
    try {
      const r = await api.DELETE('/users/{ref}/follow', { params: { path: { ref } } });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('account.following.unfollow_error');
        return;
      }
      // 204 means the edge is gone; drop the row rather than refetch.
      following = following.filter((u) => u.ref !== ref);
    } finally {
      const after = new Set(pendingRefs);
      after.delete(ref);
      pendingRefs = after;
    }
  }

  function initials(name: string | null | undefined): string {
    if (!name) return '?';
    const parts = name.trim().split(/\s+/);
    return (parts[0]?.[0] ?? '?').toUpperCase() + (parts[1]?.[0]?.toUpperCase() ?? '');
  }

  function formatDate(iso: string | undefined): string {
    if (!iso) return '—';
    try {
      return new Date(iso).toLocaleDateString();
    } catch {
      return iso;
    }
  }
</script>

<svelte:head><title>{t('account.following.title')} — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">{t('account.following.title')}</h2>
  <p class="text-sm text-fg-muted">{t('account.following.intro')}</p>
</header>

<div class="mb-4 flex gap-1 border-b border-border" role="tablist">
  {#each [{ id: 'following' as Tab, count: following.length }, { id: 'followers' as Tab, count: followers.length }] as tb (tb.id)}
    <button
      type="button"
      role="tab"
      aria-selected={tab === tb.id}
      data-testid="following-tab-{tb.id}"
      class={`-mb-px border-b-2 px-4 py-2 text-sm ${
        tab === tb.id
          ? 'border-accent font-medium text-fg'
          : 'border-transparent text-fg-muted hover:text-fg'
      }`}
      onclick={() => (tab = tb.id)}
    >
      {t(`account.following.tab_${tb.id}`)}
      <span class="ml-1 text-xs text-fg-muted">{tb.count}</span>
    </button>
  {/each}
</div>

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>
{:else if rows.length === 0}
  <p
    class="rounded-md border border-border bg-surface px-4 py-6 text-center text-sm text-fg-muted"
    data-testid="following-empty"
  >
    {tab === 'following'
      ? t('account.following.empty_following')
      : t('account.following.empty_followers')}
  </p>
{:else}
  <!-- overflow-x-auto, not overflow-hidden: at 390px the row is wider
       than the viewport and the Unfollow button would be clipped off
       the edge with no way to reach it. `Since` also drops below `sm`
       so the common case needs no sideways scroll at all. -->
  <div class="overflow-x-auto rounded-lg border border-border bg-surface">
    <table class="w-full text-sm" data-testid="following-table">
      <thead class="bg-surface-elevated text-xs uppercase tracking-wider text-fg-muted">
        <tr>
          <th class="px-4 py-2 text-left font-medium">{t('account.following.col_user')}</th>
          <th class="hidden px-4 py-2 text-left font-medium sm:table-cell"
            >{t('account.following.col_since')}</th
          >
          {#if tab === 'following'}
            <th class="px-4 py-2 text-right font-medium">{t('account.following.col_actions')}</th>
          {/if}
        </tr>
      </thead>
      <tbody>
        {#each rows as u (u.ref)}
          {@const isPending = pendingRefs.has(u.ref)}
          <tr class="border-t border-border hover:bg-surface-elevated/40">
            <td class="px-4 py-3">
              <a
                href={`/users/by-username/${u.username}`}
                class="flex items-center gap-3 hover:underline"
              >
                <div
                  class="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-accent/20 text-xs font-semibold text-accent"
                >
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
              </a>
            </td>
            <td class="hidden px-4 py-3 text-xs text-fg-muted sm:table-cell">{formatDate(u.since)}</td>
            {#if tab === 'following'}
              <td class="px-4 py-3 text-right">
                <button
                  type="button"
                  class="rounded-md border border-border bg-surface px-3 py-1 text-xs hover:bg-state-hover disabled:opacity-50"
                  onclick={() => unfollow(u.ref)}
                  disabled={isPending}
                >
                  {isPending ? t('common.loading') : t('account.following.unfollow')}
                </button>
              </td>
            {/if}
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
