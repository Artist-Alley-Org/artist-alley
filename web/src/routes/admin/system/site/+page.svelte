<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { auth } from '$stores/auth.svelte';

  let name = $state('');
  let baseUrl = $state('');
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  // Public access (#445). Separate endpoint and separate save from the
  // name/base-URL form on purpose: PATCH /admin/system/site is a
  // whole-object replace, so sharing a submit button would let a stale
  // form flip who can read the install as a side effect of a rename.
  let publicMode = $state(false);
  let publicSaving = $state(false);
  let publicSaved = $state(false);
  let publicError = $state<string | null>(null);
  const canWrite = $derived(auth.can('system.config.write'));

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
      const { data: pm } = await api.GET('/admin/system/public-mode');
      if (pm) publicMode = (pm as { enabled?: boolean }).enabled ?? false;
    } finally {
      loading = false;
    }
  }

  async function savePublicMode(next: boolean) {
    if (publicSaving) return;
    publicSaving = true;
    publicSaved = false;
    publicError = null;
    const previous = publicMode;
    try {
      const { data, error: apiErr } = await api.PATCH('/admin/system/public-mode', {
        body: { enabled: next },
      });
      if (apiErr || !data) {
        // Snap the switch back. Leaving it showing the value the
        // server rejected would misreport whether the install is
        // public, which is the one thing this control must not do.
        publicMode = previous;
        publicError = (apiErr as { error?: string } | undefined)?.error ?? t('errors.save_failed');
        return;
      }
      publicMode = (data as { enabled?: boolean }).enabled ?? next;
      publicSaved = true;
    } finally {
      publicSaving = false;
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

  <section class="mt-8 max-w-xl border-t border-border pt-6">
    <h3 class="text-lg font-semibold">{t('admin.system.site.public_title')}</h3>
    <p class="mt-1 text-sm text-fg-muted">{t('admin.system.site.public_help')}</p>

    <label class="mt-4 flex items-start gap-3">
      <input
        type="checkbox"
        checked={publicMode}
        disabled={!canWrite || publicSaving}
        onchange={(e) => void savePublicMode(e.currentTarget.checked)}
        class="mt-0.5 h-4 w-4 rounded border-border accent-accent disabled:cursor-not-allowed"
      />
      <span class="text-sm">
        {t('admin.system.site.public_toggle')}
        <span class="mt-0.5 block text-xs text-fg-muted">
          {publicMode ? t('admin.system.site.public_on') : t('admin.system.site.public_off')}
        </span>
      </span>
    </label>

    {#if !canWrite}
      <p class="mt-2 text-xs text-fg-muted">{t('admin.system.site.public_readonly')}</p>
    {/if}
    {#if publicError}
      <p role="alert" class="mt-3 rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{publicError}</p>
    {/if}
    {#if publicSaved}
      <p class="mt-3 rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success">{t('admin.system.site.public_saved')}</p>
    {/if}
  </section>
{/if}
