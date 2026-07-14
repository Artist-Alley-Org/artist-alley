<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Asset-type detail with the ACL editor (Phase 1.17.F-bis).
  //
  // The ACL editor is the gold standard for this branch: pick a
  // principal (user / role / team) + a permission (read / write /
  // admin), POST the row, see it appear in the list. Removing is
  // by composite-key DELETE — no surrogate id involved.
  //
  // A type with zero ACL rows is "open" — every caller sees it in
  // ListAssetTypes. First grant flips it to "restricted" (only
  // grantees + admins see it). Removing the last row flips it back.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { page } from '$app/state';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface AssetType {
    ref: number;
    name?: string | null;
    icon?: string | null;
    allowed_extensions?: string | null;
    order_by?: number | null;
  }
  interface AclEntry {
    principal_type: 'user' | 'role' | 'team';
    principal_id: string;
    permission: 'read' | 'write' | 'admin';
    granted_at: string;
    granted_by_user_ref?: number | null;
    expires_at?: string | null;
  }

  const ref = $derived(Number(page.params.ref));

  let type = $state<AssetType | null>(null);
  let acls = $state<AclEntry[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Add-row form state.
  let newPrincipalType = $state<'user' | 'role' | 'team'>('user');
  let newPrincipalId = $state('');
  let newPermission = $state<'read' | 'write' | 'admin'>('read');
  let busy = $state(false);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    error = null;
    try {
      const [tResp, aResp] = await Promise.all([
        // We re-use ListAssetTypes because there's no GET /asset_types/{ref}
        // surface today — the list is short and a client-side find keeps
        // us from inventing a one-off endpoint.
        api.GET('/asset_types'),
        api.GET('/asset_types/{ref}/acls', { params: { path: { ref } } }),
      ]);
      if (tResp.data) {
        const all = tResp.data as unknown as AssetType[];
        type = all.find((x) => x.ref === ref) ?? null;
      }
      if (aResp.error) {
        error = (aResp.error as { error?: string } | undefined)?.error ?? t('admin.asset_type_detail.load_error');
        return;
      }
      acls = (aResp.data as unknown as AclEntry[]) ?? [];
    } finally {
      loading = false;
    }
  }

  async function addAcl() {
    if (busy || !newPrincipalId.trim()) return;
    busy = true;
    error = null;
    try {
      const r = await api.POST('/asset_types/{ref}/acls', {
        params: { path: { ref } },
        body: {
          principal_type: newPrincipalType,
          principal_id: newPrincipalId.trim(),
          permission: newPermission,
        },
      });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.asset_type_detail.add_error');
        return;
      }
      newPrincipalId = '';
      await load();
    } finally {
      busy = false;
    }
  }

  async function removeAcl(row: AclEntry) {
    if (busy) return;
    busy = true;
    error = null;
    try {
      const r = await api.DELETE('/asset_types/{ref}/acls/{principal_type}/{principal_id}/{permission}', {
        params: {
          path: {
            ref,
            principal_type: row.principal_type,
            principal_id: row.principal_id,
            permission: row.permission,
          },
        },
      });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.asset_type_detail.remove_error');
        return;
      }
      await load();
    } finally {
      busy = false;
    }
  }

  function principalPlaceholder(kind: 'user' | 'role' | 'team'): string {
    if (kind === 'user') return t('admin.asset_type_detail.principal_user_placeholder');
    return t('admin.asset_type_detail.principal_uuid_placeholder');
  }

  function principalLabel(kind: 'user' | 'role' | 'team'): string {
    if (kind === 'user') return t('admin.asset_type_detail.principal_kind_user');
    if (kind === 'role') return t('admin.asset_type_detail.principal_kind_role');
    return t('admin.asset_type_detail.principal_kind_team');
  }

  function permissionLabel(p: 'read' | 'write' | 'admin'): string {
    if (p === 'read') return t('admin.asset_type_detail.permission_read');
    if (p === 'write') return t('admin.asset_type_detail.permission_write');
    return t('admin.asset_type_detail.permission_admin');
  }
</script>

<svelte:head>
  <title>{type ? type.name ?? `asset-type ${ref}` : `asset-type ${ref}`} — {site.name}</title>
</svelte:head>

<p class="mb-3 text-xs">
  <a href="/admin/asset-types" class="text-accent hover:underline">{t('admin.asset_type_detail.back')}</a>
</p>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if !type}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
    {t('admin.asset_type_detail.not_found')}
  </p>
{:else}
  <header class="mb-6">
    <h2 class="text-xl font-semibold">{type.name ?? '—'}</h2>
    <p class="mt-1 font-mono text-xs text-fg-muted">
      {t('admin.asset_type_detail.header_meta', {
        ref: String(type.ref),
        icon: type.icon ?? '—',
        extensions: type.allowed_extensions ?? '—',
      })}
    </p>
  </header>

  {#if error}
    <p role="alert" class="mb-4 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {/if}

  <section class="max-w-3xl rounded-lg border border-border bg-surface-elevated p-4">
    <h3 class="mb-2 text-sm font-medium text-fg">{t('admin.asset_type_detail.acl_section')}</h3>
    <p class="mb-3 text-xs text-fg-muted">{t('admin.asset_type_detail.acl_intro')}</p>

    {#if acls.length === 0}
      <div class="mb-3 rounded border border-warning/30 bg-warning/5 px-3 py-2 text-xs text-fg">
        {t('admin.asset_type_detail.acl_open')}
      </div>
    {:else}
      <div class="mb-3 rounded border border-success/30 bg-success/5 px-3 py-2 text-xs text-fg">
        {t('admin.asset_type_detail.acl_restricted', { count: String(acls.length) })}
      </div>
      <ul class="mb-3 space-y-1">
        {#each acls as row, i (row.principal_type + row.principal_id + row.permission + i)}
          <li class="flex items-center gap-2 rounded border border-border bg-surface px-2 py-1.5 text-xs">
            <span class="inline-block w-12 text-[10px] uppercase tracking-wider text-fg-muted">{principalLabel(row.principal_type)}</span>
            <code class="flex-1 font-mono">{row.principal_id}</code>
            <span
              class={'rounded px-1.5 py-0.5 text-[10px] uppercase tracking-wider ' + (
                row.permission === 'admin'
                  ? 'border border-danger/40 bg-danger/10 text-danger'
                  : row.permission === 'write'
                  ? 'border border-warning/40 bg-warning/10 text-warning'
                  : 'border border-accent/40 bg-accent/10 text-accent'
              )}
            >{permissionLabel(row.permission)}</span>
            <button
              type="button"
              onclick={() => removeAcl(row)}
              disabled={busy}
              class="rounded border border-border bg-surface-elevated px-2 py-0.5 text-[11px] hover:border-danger hover:text-danger disabled:opacity-50"
            >
              {t('admin.asset_type_detail.acl_remove')}
            </button>
          </li>
        {/each}
      </ul>
    {/if}

    <div class="grid gap-2 rounded border border-border bg-surface p-3 sm:grid-cols-4">
      <label class="block text-[11px]">
        <span class="mb-0.5 block text-fg-muted">{t('admin.asset_type_detail.acl_principal_type')}</span>
        <select
          bind:value={newPrincipalType}
          class="w-full rounded border border-border bg-surface-elevated px-1.5 py-1 text-xs focus:border-accent focus:outline-none"
        >
          <option value="user">{principalLabel('user')}</option>
          <option value="role">{principalLabel('role')}</option>
          <option value="team">{principalLabel('team')}</option>
        </select>
      </label>
      <label class="block text-[11px] sm:col-span-2">
        <span class="mb-0.5 block text-fg-muted">{t('admin.asset_type_detail.acl_principal_id')}</span>
        <input
          type="text"
          bind:value={newPrincipalId}
          placeholder={principalPlaceholder(newPrincipalType)}
          class="w-full rounded border border-border bg-surface-elevated px-1.5 py-1 font-mono text-[11px] focus:border-accent focus:outline-none"
        />
      </label>
      <label class="block text-[11px]">
        <span class="mb-0.5 block text-fg-muted">{t('admin.asset_type_detail.acl_permission')}</span>
        <select
          bind:value={newPermission}
          class="w-full rounded border border-border bg-surface-elevated px-1.5 py-1 text-xs focus:border-accent focus:outline-none"
        >
          <option value="read">{permissionLabel('read')}</option>
          <option value="write">{permissionLabel('write')}</option>
          <option value="admin">{permissionLabel('admin')}</option>
        </select>
      </label>
      <button
        type="button"
        onclick={addAcl}
        disabled={busy || !newPrincipalId.trim()}
        class="rounded border border-accent bg-accent/15 px-3 py-1 text-xs font-medium text-fg hover:bg-accent/25 disabled:opacity-50 sm:col-span-4"
      >
        {t('admin.asset_type_detail.acl_add_button')}
      </button>
    </div>
  </section>
{/if}
