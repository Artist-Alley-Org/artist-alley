<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface SSOProvider {
    id?: string;
    kind: 'ldap' | 'saml' | 'google' | 'github' | 'x';
    enabled: boolean;
    display_name: string;
    config?: Record<string, unknown>;
  }

  let policy = $state({
    min_length: 0,
    require_upper: false,
    require_number: false,
    require_symbol: false,
    disallow_common: false,
    max_age_days: 0,
  });
  let providers = $state<SSOProvider[]>([]);
  // Phase 1.19.C — self-registration knobs.
  let selfRegistration = $state({
    enabled: false,
    require_email_verification: true,
    default_role: 'Base',
  });
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  onMount(() => { void load(); });

  async function load() {
    loading = true;
    try {
      const { data } = await api.GET('/admin/system/auth');
      if (data) {
        const d = data as {
          password_policy?: typeof policy;
          sso_providers?: SSOProvider[];
          self_registration?: typeof selfRegistration;
        };
        if (d.password_policy) policy = { ...policy, ...d.password_policy };
        providers = d.sso_providers ?? [];
        if (d.self_registration) selfRegistration = { ...selfRegistration, ...d.self_registration };
      }
    } finally {
      loading = false;
    }
  }

  function addProvider() {
    providers = [...providers, { kind: 'ldap', enabled: false, display_name: 'New provider', config: {} }];
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
      const { error: apiErr } = await api.PATCH('/admin/system/auth', {
        body: {
          password_policy: policy,
          sso_providers: providers,
          self_registration: selfRegistration,
        } as never,
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

<svelte:head><title>{t('admin.system.auth.title')} — artist-alley</title></svelte:head>

<h2 class="mb-4 text-xl font-semibold">{t('admin.system.auth.title')}</h2>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else}
  <form onsubmit={(e) => { e.preventDefault(); void save(); }} class="max-w-3xl space-y-6">
    <section class="space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
      <h3 class="text-sm font-medium text-fg">{t('admin.system.auth.password_policy')}</h3>
      <div class="grid grid-cols-2 gap-3">
        <label class="text-sm">
          <span class="block text-xs text-fg-muted">{t('admin.system.auth.min_length')}</span>
          <input type="number" min="0" max="256" bind:value={policy.min_length} class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none" />
        </label>
        <label class="text-sm">
          <span class="block text-xs text-fg-muted">{t('admin.system.auth.max_age_days')}</span>
          <input type="number" min="0" max="36500" bind:value={policy.max_age_days} class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none" />
        </label>
      </div>
      <div class="grid grid-cols-2 gap-2">
        <label class="inline-flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={policy.require_upper} class="h-4 w-4 accent-accent" />{t('admin.system.auth.require_upper')}</label>
        <label class="inline-flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={policy.require_number} class="h-4 w-4 accent-accent" />{t('admin.system.auth.require_number')}</label>
        <label class="inline-flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={policy.require_symbol} class="h-4 w-4 accent-accent" />{t('admin.system.auth.require_symbol')}</label>
        <label class="inline-flex items-center gap-2 text-sm"><input type="checkbox" bind:checked={policy.disallow_common} class="h-4 w-4 accent-accent" />{t('admin.system.auth.disallow_common')}</label>
      </div>
    </section>

    <section class="space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
      <h3 class="text-sm font-medium text-fg">{t('admin.system.auth.self_registration_heading')}</h3>
      <p class="text-xs text-fg-muted">{t('admin.system.auth.self_registration_help')}</p>
      <label class="inline-flex items-center gap-2 text-sm">
        <input type="checkbox" bind:checked={selfRegistration.enabled} class="h-4 w-4 accent-accent" data-testid="auth-selfreg-enabled" />
        {t('admin.system.auth.self_registration_enabled')}
      </label>
      <label class="inline-flex items-center gap-2 text-sm">
        <input type="checkbox" bind:checked={selfRegistration.require_email_verification} class="h-4 w-4 accent-accent" data-testid="auth-selfreg-verify" />
        {t('admin.system.auth.self_registration_require_verify')}
      </label>
      <label class="block">
        <span class="block text-xs text-fg-muted">{t('admin.system.auth.self_registration_default_role')}</span>
        <input type="text" bind:value={selfRegistration.default_role} placeholder="Base" class="mt-1 w-full max-w-xs rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none" />
      </label>
    </section>

    <section class="space-y-3 rounded-lg border border-border bg-surface-elevated p-4">
      <header class="flex items-center justify-between">
        <h3 class="text-sm font-medium text-fg">{t('admin.system.auth.sso_providers')}</h3>
        <button type="button" onclick={addProvider} class="rounded-md border border-border px-2.5 py-1 text-xs text-fg-muted hover:text-fg">
          {t('admin.system.auth.add_provider')}
        </button>
      </header>
      {#if providers.length === 0}
        <p class="rounded-md bg-surface-elevated px-3 py-2 text-xs text-fg-muted">{t('admin.system.auth.no_providers')}</p>
      {:else}
        <div class="space-y-2">
          {#each providers as p, idx (idx)}
            <article class="rounded border border-border bg-surface-elevated p-3">
              <div class="grid grid-cols-1 gap-2 md:grid-cols-[10rem_1fr_auto_auto]">
                <label class="text-sm">
                  <span class="block text-xs text-fg-muted">{t('admin.system.auth.provider_kind')}</span>
                  <select bind:value={providers[idx].kind} class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 text-sm focus-visible:border-border-strong focus:outline-none">
                    <option value="ldap">LDAP</option>
                    <option value="saml">SAML</option>
                    <option value="google">Google</option>
                    <option value="github">GitHub</option>
                    <option value="x">X</option>
                  </select>
                </label>
                <label class="text-sm">
                  <span class="block text-xs text-fg-muted">{t('admin.system.auth.provider_display_name')}</span>
                  <input type="text" bind:value={providers[idx].display_name} class="mt-1 w-full rounded border border-border bg-surface px-2 py-1 text-sm focus-visible:border-border-strong focus:outline-none" />
                </label>
                <label class="inline-flex items-end gap-1 text-sm">
                  <input type="checkbox" bind:checked={providers[idx].enabled} class="h-4 w-4 accent-accent" />
                  <span class="pb-1 text-xs text-fg-muted">{t('admin.system.auth.provider_enabled')}</span>
                </label>
                <button type="button" onclick={() => removeProvider(idx)} class="self-end rounded-md border border-danger/40 px-2 py-1 text-xs text-danger hover:bg-danger-container">
                  {t('admin.system.auth.remove_provider')}
                </button>
              </div>
            </article>
          {/each}
        </div>
      {/if}
    </section>

    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
    {/if}
    {#if saved}
      <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success">{t('admin.system.auth.saved')}</p>
    {/if}

    <button type="submit" disabled={saving} class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40">
      {saving ? t('common.loading') : t('admin.system.auth.save')}
    </button>
  </form>
{/if}
