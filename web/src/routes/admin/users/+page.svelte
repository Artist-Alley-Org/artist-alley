<script lang="ts">
  // Admin user list (Phase 1.17.A).
  //
  // Paginated table backed by GET /admin/users. Search + status
  // filter debounce 250ms; cursor pagination via the "Load more"
  // button at the bottom. Click any row → /admin/users/{ref} for
  // role assignment (existing).

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import Avatar from '$components/Avatar.svelte';
  import {
    type AdminUser,
    type AdminUserStatus,
    statusBadgeClass,
    relativeAgo,
    buildListQuery,
  } from '$lib/admin/users';

  let users = $state<AdminUser[]>([]);
  let total = $state(0);
  let nextCursor = $state<string | null>(null);
  let loading = $state(true);
  let loadingMore = $state(false);
  let error = $state<string | null>(null);

  let query = $state('');
  let status = $state<AdminUserStatus | ''>('');

  let debounceTimer: ReturnType<typeof setTimeout> | null = null;

  onMount(() => {
    void load(true);
  });

  function onFilterChange() {
    if (debounceTimer) clearTimeout(debounceTimer);
    debounceTimer = setTimeout(() => { void load(true); }, 250);
  }

  async function load(reset: boolean) {
    if (reset) {
      loading = true;
      nextCursor = null;
    } else {
      loadingMore = true;
    }
    error = null;
    try {
      const params = buildListQuery({
        q: query,
        status: status,
        cursor: reset ? null : nextCursor,
        limit: 50,
      });
      const r = await api.GET('/admin/users', { params: { query: params } });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to load users.';
        return;
      }
      const page = r.data as unknown as { items: AdminUser[]; total: number; next_cursor?: string | null };
      users = reset ? page.items : [...users, ...page.items];
      total = page.total;
      nextCursor = page.next_cursor ?? null;
    } finally {
      loading = false;
      loadingMore = false;
    }
  }

  function statusLabel(s: AdminUserStatus): string {
    if (s === 'active') return t('admin.users.status_active');
    if (s === 'pending') return t('admin.users.status_pending');
    return t('admin.users.status_disabled');
  }
</script>

<svelte:head><title>{t('admin.users.title')} — artist-alley</title></svelte:head>

<header class="mb-4 flex flex-wrap items-baseline justify-between gap-2">
  <h2 class="text-xl font-semibold">{t('admin.users.title')}</h2>
  {#if !loading}
    <p class="text-xs text-fg-muted">{t('admin.users.subtitle_count', { count: total })}</p>
  {/if}
</header>

<div class="mb-4 flex flex-wrap items-center gap-2">
  <input
    type="search"
    bind:value={query}
    oninput={onFilterChange}
    placeholder={t('admin.users.search_placeholder')}
    class="flex-1 min-w-[14rem] rounded border border-border bg-surface px-3 py-1.5 text-sm focus:border-accent focus:outline-none"
  />
  <label class="flex items-center gap-2 text-xs text-fg-muted">
    <span>{t('admin.users.filter_status_label')}</span>
    <select
      bind:value={status}
      onchange={onFilterChange}
      class="rounded border border-border bg-surface px-2 py-1 text-xs"
    >
      <option value="">{t('admin.users.filter_status_all')}</option>
      <option value="active">{t('admin.users.filter_status_active')}</option>
      <option value="pending">{t('admin.users.filter_status_pending')}</option>
      <option value="disabled">{t('admin.users.filter_status_disabled')}</option>
    </select>
  </label>
</div>

{#if loading}
  <p class="text-fg-muted">{t('admin.users.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
{:else if users.length === 0}
  <p class="rounded-md bg-surface-elevated px-4 py-6 text-center text-sm text-fg-muted">
    {t('admin.users.no_users')}
  </p>
{:else}
  <div class="overflow-x-auto rounded-md border border-border">
    <table class="w-full text-sm">
      <thead class="bg-surface-elevated text-left text-xs uppercase tracking-wider text-fg-muted">
        <tr>
          <th class="px-2 py-2"></th>
          <th class="px-2 py-2">{t('admin.users.username')}</th>
          <th class="px-2 py-2">{t('admin.users.email')}</th>
          <th class="px-2 py-2">{t('admin.users.status')}</th>
          <th class="px-2 py-2">{t('admin.users.role')}</th>
          <th class="px-2 py-2">{t('admin.users.last_active')}</th>
          <th class="px-2 py-2">{t('admin.users.joined')}</th>
        </tr>
      </thead>
      <tbody>
        {#each users as u (u.ref)}
          <tr class="border-t border-border hover:bg-surface-elevated/60">
            <td class="px-2 py-2"><Avatar name={u.display_name} src={u.avatar_url} sizeClass="h-7 w-7" /></td>
            <td class="px-2 py-2">
              <a
                href="/admin/users/{u.ref}"
                class="font-medium text-accent hover:underline"
                aria-label={t('admin.users.open_user', { username: u.username })}
              >
                @{u.username}
              </a>
              {#if u.fullname}
                <span class="ml-2 text-xs text-fg-muted">{u.fullname}</span>
              {/if}
            </td>
            <td class="px-2 py-2 text-fg-muted">{u.email ?? ''}</td>
            <td class="px-2 py-2">
              <span class={`inline-block rounded px-2 py-0.5 text-[10px] uppercase tracking-wider ${statusBadgeClass(u.status)}`}>
                {statusLabel(u.status)}
              </span>
            </td>
            <td class="px-2 py-2 text-fg-muted">{(u.primary_role ?? '') || t('admin.users.no_role')}</td>
            <td class="px-2 py-2 text-fg-muted" title={u.last_active ?? ''}>
              {u.last_active ? relativeAgo(u.last_active) : t('admin.users.never_active')}
            </td>
            <td class="px-2 py-2 text-fg-muted" title={u.created_at}>{relativeAgo(u.created_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>

  {#if nextCursor}
    <div class="mt-4 text-center">
      <button
        type="button"
        onclick={() => load(false)}
        disabled={loadingMore}
        class="rounded border border-border bg-surface px-4 py-2 text-sm hover:border-accent disabled:opacity-50"
      >
        {loadingMore ? t('admin.users.loading') : t('admin.users.load_more')}
      </button>
    </div>
  {/if}
{/if}
