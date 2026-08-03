<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import PasswordInput from '$components/PasswordInput.svelte';

  // api_key is WRITE-ONLY (#711). The API returns api_key_set, never
  // the key, so this box always starts empty and only ever holds what
  // the admin just typed — which is what makes the reveal toggle safe
  // here, by the same argument that made it safe on SMTP.
  //
  // Empty on save means "keep the stored key": the server merges the
  // on-file value in per provider id. Sending '' would otherwise wipe
  // every key on an unrelated edit (#708's shape).
  interface Provider {
    id?: string;
    kind: 'openai' | 'anthropic' | 'google' | 'local';
    enabled: boolean;
    display_name: string;
    model?: string;
    base_url?: string;
    /** Always a string, never undefined — see load()/addProvider. */
    api_key: string;
    api_key_set?: boolean;
  }

  let providers = $state<Provider[]>([]);
  let defaultProviderId = $state('');
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/admin/system/ai');
      if (data) {
        const d = data as { default_provider_id?: string; providers?: Omit<Provider, 'api_key'>[] };
        // api_key is always blank after a load — the response has no
        // key to restore, and a stale one left over from the previous
        // save must not be re-sent.
        providers = (d.providers ?? []).map((p) => ({ ...p, api_key: '' }));
        defaultProviderId = d.default_provider_id ?? '';
      }
    } finally {
      loading = false;
    }
  }

  function addProvider() {
    providers = [...providers, { kind: 'openai', enabled: false, display_name: 'New provider', model: '', base_url: '', api_key: '', api_key_set: false }];
  }

  function removeProvider(idx: number) {
    providers = providers.filter((_, i) => i !== idx);
  }

  async function save() {
    if (saving) return;
    saving = true;
    saved = false;
    error = null;
    try {
      // Drop the read-only marker, and omit api_key entirely unless
      // the admin typed one — the body must say "no opinion about the
      // key", not "set it to empty".
      const payload = providers.map((p) => ({
        id: p.id,
        kind: p.kind,
        enabled: p.enabled,
        display_name: p.display_name,
        model: p.model,
        base_url: p.base_url,
        ...(p.api_key ? { api_key: p.api_key } : {}),
      }));
      const { error: apiErr } = await api.PATCH('/admin/system/ai', {
        body: { default_provider_id: defaultProviderId, providers: payload } as never,
      });
      if (apiErr) {
        error = (apiErr as { error?: string }).error ?? t('errors.save_failed');
        return;
      }
      saved = true;
      await load();
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>{t('admin.system.ai_providers.title')} — {site.name}</title></svelte:head>

<nav class="mb-3 text-xs text-fg-muted">
  <a href="/admin/system/ai" class="hover:underline">{t('admin.system.ai_landing.title')}</a>
  <span aria-hidden="true">/</span>
  <span>{t('admin.system.ai_providers.title')}</span>
</nav>

<h2 class="mb-4 text-xl font-semibold">{t('admin.system.ai_providers.title')}</h2>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else}
  <form onsubmit={(e) => { e.preventDefault(); void save(); }} class="max-w-3xl space-y-6">
    <section class="space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
      <header class="flex items-center justify-between">
        <h3 class="text-sm font-medium text-fg">{t('admin.system.ai.providers')}</h3>
        <button type="button" onclick={addProvider} class="rounded-md border border-border px-2.5 py-1 text-xs text-fg-muted hover:text-fg">
          {t('admin.system.ai.add_provider')}
        </button>
      </header>

      {#if providers.length === 0}
        <p class="rounded-md bg-surface-elevated px-3 py-2 text-xs text-fg-muted">{t('admin.system.ai.no_providers')}</p>
      {:else}
        <div class="space-y-2">
          {#each providers as p, idx (idx)}
            <article class="rounded border border-border bg-surface-elevated p-3 text-sm">
              <div class="grid grid-cols-1 gap-2 md:grid-cols-[10rem_1fr_auto_auto]">
                <label>
                  <span class="block text-xs text-fg-muted">{t('admin.system.ai.kind')}</span>
                  <select bind:value={providers[idx].kind} class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none">
                    <option value="openai">OpenAI</option>
                    <option value="anthropic">Anthropic</option>
                    <option value="google">Google</option>
                    <option value="local">Local</option>
                  </select>
                </label>
                <label>
                  <span class="block text-xs text-fg-muted">{t('admin.system.ai.display_name')}</span>
                  <input type="text" bind:value={providers[idx].display_name} class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                </label>
                <label class="inline-flex items-end gap-1">
                  <input type="checkbox" bind:checked={providers[idx].enabled} class="h-4 w-4 accent-accent" />
                  <span class="pb-1 text-xs text-fg-muted">{t('admin.system.ai.enabled')}</span>
                </label>
                <button type="button" onclick={() => removeProvider(idx)} class="self-end rounded-md border border-danger/40 px-2 py-1 text-xs text-danger hover:bg-danger-container">
                  {t('admin.system.ai.remove_provider')}
                </button>
              </div>
              <div class="mt-2 grid grid-cols-1 gap-2 md:grid-cols-3">
                <label>
                  <span class="block text-xs text-fg-muted">{t('admin.system.ai.model')}</span>
                  <input type="text" bind:value={providers[idx].model} placeholder="gpt-4o" class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                </label>
                <label>
                  <span class="block text-xs text-fg-muted">{t('admin.system.ai.base_url')}</span>
                  <input type="url" bind:value={providers[idx].base_url} class="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none" />
                </label>
                <!--
                  The reveal toggle #692 deliberately withheld is now
                  here, because #711 made the read path write-only.
                  The box starts empty on every load and can only ever
                  contain what this admin just typed, so revealing it
                  discloses nothing that is not already on their
                  screen — the same argument that made it safe on the
                  SMTP password field.
                -->
                <label>
                  <span class="block text-xs text-fg-muted">{t('admin.system.ai.api_key')}</span>
                  <!-- Short placeholder deliberately: this is a
                       third-width column at 1440 and a full-width one
                       at 390, and SMTP's longer phrasing truncates in
                       both. The help line below carries the meaning
                       the placeholder can't. -->
                  <PasswordInput
                    bind:value={providers[idx].api_key}
                    placeholder={providers[idx].api_key_set ? t('admin.system.ai.api_key_on_file') : t('admin.system.ai.api_key_unset')}
                    autocomplete="new-password"
                    testId="ai-api-key-{idx}"
                    inputClass="mt-1 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
                  />
                  <span class="mt-0.5 block text-[11px] text-fg-muted">{t('admin.system.ai.api_key_help')}</span>
                </label>
              </div>
            </article>
          {/each}
        </div>
      {/if}
    </section>

    <section class="space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
      <h3 class="text-sm font-medium text-fg">{t('admin.system.ai.default_provider')}</h3>
      <select bind:value={defaultProviderId} class="w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none">
        <option value="">{t('admin.system.ai.no_default')}</option>
        {#each providers as p (p.id ?? p.display_name)}
          {#if p.id}
            <option value={p.id}>{p.display_name}</option>
          {/if}
        {/each}
      </select>
    </section>

    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
    {/if}
    {#if saved}
      <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success">{t('admin.system.ai.saved')}</p>
    {/if}

    <button type="submit" disabled={saving} class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40">
      {saving ? t('common.loading') : t('admin.system.ai.save')}
    </button>
  </form>
{/if}
