<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  let name = $state('');
  let baseUrl = $state('');
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/admin/system/site');
      if (data) {
        name = (data as { name?: string }).name ?? '';
        baseUrl = (data as { base_url?: string }).base_url ?? '';
      }
    } finally {
      loading = false;
    }
  }

  async function save() {
    if (saving) return;
    saving = true;
    saved = false;
    error = null;
    try {
      const { data, error: apiErr } = await api.PATCH('/admin/system/site', {
        body: { name, base_url: baseUrl },
      });
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('errors.save_failed');
        return;
      }
      saved = true;
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>{t('admin.system.site.title')} — {site.name}</title></svelte:head>

<h2 class="mb-4 text-xl font-semibold">{t('admin.system.site.title')}</h2>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else}
  <form
    onsubmit={(e) => {
      e.preventDefault();
      void save();
    }}
    class="max-w-xl space-y-4"
  >
    <label class="block">
      <span class="text-sm text-fg-muted">{t('admin.system.site.name')}</span>
      <input
        type="text"
        bind:value={name}
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
        required
      />
    </label>
    <label class="block">
      <span class="text-sm text-fg-muted">{t('admin.system.site.base_url')}</span>
      <input
        type="url"
        bind:value={baseUrl}
        placeholder="https://example.com"
        class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
      />
      <span class="mt-1 block text-xs text-fg-muted">{t('admin.system.site.base_url_help')}</span>
    </label>

    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
    {/if}
    {#if saved}
      <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success">{t('admin.system.site.saved')}</p>
    {/if}

    <button
      type="submit"
      disabled={saving}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {saving ? t('common.loading') : t('admin.system.site.save')}
    </button>
  </form>
{/if}
