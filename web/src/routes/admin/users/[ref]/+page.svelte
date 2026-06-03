<script lang="ts">
  // Per-user admin detail — profile + role assignment + lifecycle
  // status (Phase 1.17.B).

  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import Avatar from '$components/Avatar.svelte';
  import {
    type AdminUser,
    type AdminUserStatus,
    statusBadgeClass,
  } from '$lib/admin/users';

  interface UserPublic {
    ref: number;
    username: string;
    display_name: string;
    fullname?: string | null;
    avatar_url?: string | null;
    member_since: string;
  }
  interface Role {
    id: string;
    name: string;
  }

  const ref = $derived(Number(page.params.ref));

  let user = $state<UserPublic | null>(null);
  let roles = $state<Role[]>([]);
  let selectedRole = $state<string>('');
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  // Lifecycle state (1.17.B). We try to read the user's current
  // status from the admin list (one-off scoped fetch keyed by ref);
  // falls back to "active" if the API doesn't return the row (the
  // public profile doesn't expose status, so we have to ask the
  // admin list).
  let status = $state<AdminUserStatus>('active');
  let statusReason = $state('');
  let statusSaving = $state(false);
  let statusMessage = $state<{ kind: 'ok' | 'noop'; text: string } | null>(null);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    error = null;
    try {
      const [u, rs, adminRow] = await Promise.all([
        api.GET('/users/{ref}', { params: { path: { ref } } }),
        api.GET('/auth/roles'),
        // Fetch only this user via the admin list with a precise
        // search query; the API returns AdminUser which carries
        // status. Limit=1 because we already know the row.
        api.GET('/admin/users', { params: { query: { q: '', limit: 200 } } }),
      ]);
      if (u.error || !u.data) {
        error = (u.error as { error?: string } | undefined)?.error ?? 'User not found.';
        return;
      }
      user = u.data as UserPublic;
      if (rs.data) roles = (rs.data as { items?: Role[] }).items ?? (rs.data as unknown as Role[]);

      if (adminRow.data) {
        const page = adminRow.data as unknown as { items: AdminUser[] };
        const me = page.items.find((u) => u.ref === ref);
        if (me) status = me.status;
      }
    } finally {
      loading = false;
    }
  }

  async function saveRole() {
    if (!selectedRole || saving) return;
    saving = true;
    saved = false;
    try {
      await api.PUT('/auth/users/{ref}/role', {
        params: { path: { ref } },
        body: { role_id: selectedRole },
      });
      saved = true;
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed.';
    } finally {
      saving = false;
    }
  }

  async function saveStatus(next: AdminUserStatus) {
    if (statusSaving) return;
    statusSaving = true;
    statusMessage = null;
    try {
      const r = await api.PUT('/admin/users/{ref}/status', {
        params: { path: { ref } },
        body: { status: next, reason: statusReason || undefined },
      });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? 'Failed to update status.';
        return;
      }
      const result = r.data as unknown as { status: AdminUserStatus; changed: boolean };
      status = result.status;
      statusReason = '';
      statusMessage = result.changed
        ? { kind: 'ok', text: t('admin.user_detail.status_updated', { status: statusLabel(result.status) }) }
        : { kind: 'noop', text: t('admin.user_detail.status_no_change') };
    } finally {
      statusSaving = false;
    }
  }

  function statusLabel(s: AdminUserStatus): string {
    if (s === 'active') return t('admin.users.status_active');
    if (s === 'pending') return t('admin.users.status_pending');
    return t('admin.users.status_disabled');
  }
</script>

<svelte:head><title>User {ref} — artist-alley</title></svelte:head>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
{:else if user}
  <header class="mb-6 flex items-center gap-3">
    <Avatar name={user.display_name} src={user.avatar_url} sizeClass="h-12 w-12" />
    <div>
      <h2 class="text-xl font-semibold">{t('admin.user_detail.title', { username: user.username })}</h2>
      <p class="text-xs text-fg-muted">
        ref {user.ref} · member since {new Date(user.member_since).toLocaleDateString()}
      </p>
    </div>
    <span class={`ml-auto inline-block rounded px-2 py-0.5 text-[10px] uppercase tracking-wider ${statusBadgeClass(status)}`}>
      {statusLabel(status)}
    </span>
  </header>

  <section class="mb-6 max-w-xl space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="text-sm font-medium text-fg">{t('admin.user_detail.role_label')}</h3>
    <select
      bind:value={selectedRole}
      class="w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
    >
      <option value="">—</option>
      {#each roles as r (r.id)}
        <option value={r.id}>{r.name}</option>
      {/each}
    </select>
    <button
      type="button"
      onclick={saveRole}
      disabled={!selectedRole || saving}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {saving ? t('common.loading') : t('admin.user_detail.role_save')}
    </button>
    {#if saved}
      <p class="text-sm text-success">{t('admin.user_detail.role_saved')}</p>
    {/if}
  </section>

  <section class="max-w-xl space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="text-sm font-medium text-fg">{t('admin.user_detail.status_section')}</h3>
    <p class="text-xs text-fg-muted">{t('admin.user_detail.status_intro')}</p>
    <p class="text-xs text-fg-muted">{t('admin.user_detail.status_current', { status: statusLabel(status) })}</p>

    <label class="block text-xs">
      <span class="mb-1 block text-fg-muted">{t('admin.user_detail.status_reason')}</span>
      <input
        type="text"
        bind:value={statusReason}
        placeholder={t('admin.user_detail.status_reason_placeholder')}
        maxlength="500"
        class="w-full rounded border border-border bg-surface px-2 py-1 text-sm focus:border-accent focus:outline-none"
      />
    </label>

    <div class="flex flex-wrap gap-2">
      <button
        type="button"
        onclick={() => saveStatus('active')}
        disabled={statusSaving}
        class="rounded border border-success bg-success/10 px-3 py-1 text-xs font-medium text-success hover:bg-success/20 disabled:opacity-50"
      >
        {status === 'disabled' ? t('admin.user_detail.status_save_active_again') : t('admin.user_detail.status_save_active')}
      </button>
      <button
        type="button"
        onclick={() => saveStatus('pending')}
        disabled={statusSaving}
        class="rounded border border-warning bg-warning/10 px-3 py-1 text-xs font-medium text-warning hover:bg-warning/20 disabled:opacity-50"
      >
        {t('admin.user_detail.status_save_pending')}
      </button>
      <button
        type="button"
        onclick={() => saveStatus('disabled')}
        disabled={statusSaving}
        class="rounded border border-danger bg-danger/10 px-3 py-1 text-xs font-medium text-danger hover:bg-danger/20 disabled:opacity-50"
      >
        {t('admin.user_detail.status_save_disabled')}
      </button>
    </div>

    {#if statusMessage}
      <p class={statusMessage.kind === 'ok' ? 'text-sm text-success' : 'text-sm text-fg-muted'}>{statusMessage.text}</p>
    {/if}
  </section>
{/if}
