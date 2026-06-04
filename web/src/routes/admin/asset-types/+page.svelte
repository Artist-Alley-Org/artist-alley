<script lang="ts">
  // Admin asset-types index (Phase 1.17.F-bis). Lists every type the
  // caller can see (system.admin sees all; non-admins see what
  // ListAssetTypes' ACL filter allows through) with a click-through
  // to the per-type detail page that hosts the ACL editor.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface AssetType {
    ref: number;
    name?: string | null;
    icon?: string | null;
    allowed_extensions?: string | null;
    order_by?: number | null;
  }

  let types = $state<AssetType[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/asset_types');
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.asset_types.load_error');
        return;
      }
      types = r.data as unknown as AssetType[];
    } finally {
      loading = false;
    }
  }
</script>

<svelte:head><title>{t('admin.asset_types.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.asset_types.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('admin.asset_types.intro')}</p>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
{:else if types.length === 0}
  <p class="rounded-lg border border-border bg-surface-elevated p-4 text-sm text-fg-muted">{t('admin.asset_types.empty')}</p>
{:else}
  <div class="overflow-hidden rounded-lg border border-border bg-surface-elevated">
    <table class="w-full text-sm">
      <thead class="border-b border-border bg-surface text-left text-xs uppercase tracking-wider text-fg-muted">
        <tr>
          <th class="px-3 py-2">{t('admin.asset_types.column_ref')}</th>
          <th class="px-3 py-2">{t('admin.asset_types.column_name')}</th>
          <th class="px-3 py-2">{t('admin.asset_types.column_icon')}</th>
          <th class="px-3 py-2">{t('admin.asset_types.column_extensions')}</th>
          <th class="px-3 py-2">{t('admin.asset_types.column_order')}</th>
        </tr>
      </thead>
      <tbody>
        {#each types as ty (ty.ref)}
          <tr class="border-b border-border last:border-b-0 hover:bg-surface">
            <td class="px-3 py-2 font-mono text-xs text-fg-muted">{ty.ref}</td>
            <td class="px-3 py-2">
              <a href="/admin/asset-types/{ty.ref}" class="text-accent hover:underline">{ty.name ?? '—'}</a>
            </td>
            <td class="px-3 py-2 font-mono text-xs text-fg-muted">{ty.icon ?? '—'}</td>
            <td class="px-3 py-2 font-mono text-xs text-fg-muted">{ty.allowed_extensions ?? '—'}</td>
            <td class="px-3 py-2 text-right text-xs text-fg-muted">{ty.order_by ?? 0}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
