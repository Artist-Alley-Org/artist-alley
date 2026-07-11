<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Phase 1.14.A — /admin/ai/config page.
  //
  // Routing + fallback chains + privacy + budget defaults in one
  // form (the brief proposed split pages; coalesced here because
  // the backend treats them as one config object and the operator
  // typically edits them together). The Phase 1.16 /admin/system/ai
  // provider-list stub still owns the per-provider config — this
  // surface is the typed inference config that drives the router.
  //
  // Validator findings (returned by GET + included in 422 PUT
  // responses) render at the top of the page so the operator sees
  // structural issues — e.g. "routing names 'ollama' but no
  // provider is registered" — without having to dig into logs.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  type ConcernKey = 'complete' | 'embed' | 'transcribe' | 'tag' | 'caption';
  const CONCERNS: ConcernKey[] = ['complete', 'embed', 'transcribe', 'tag', 'caption'];

  interface Finding {
    code: string;
    concern?: ConcernKey;
    message: string;
  }
  interface InferenceConfig {
    enabled: boolean;
    routing: Record<ConcernKey, string>;
    fallback_chains: Record<string, string[]>;
    privacy: { lock_sensitive_to_local: boolean; local_providers: string[] };
    default_budget: { soft_warning_usd: number; hard_cap_usd: number };
    findings?: Finding[];
  }

  let cfg = $state<InferenceConfig | null>(null);
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  // Pre-flatten the fallback chains for textarea editing. Each
  // concern's chain renders as a comma-separated string; we
  // re-explode on save.
  let fallbackText = $state<Record<string, string>>({});
  let privacyLocalText = $state('');

  onMount(() => void load());

  async function load() {
    loading = true;
    error = null;
    try {
      const { data } = await api.GET('/admin/ai/config');
      if (!data) {
        error = t('admin.ai_inference.load_error');
        return;
      }
      cfg = data as InferenceConfig;
      fallbackText = {};
      for (const c of CONCERNS) {
        fallbackText[c] = (cfg.fallback_chains?.[c] ?? []).join(', ');
      }
      privacyLocalText = (cfg.privacy?.local_providers ?? []).join(', ');
    } finally {
      loading = false;
    }
  }

  function parseList(input: string): string[] {
    return input
      .split(',')
      .map((s) => s.trim())
      .filter((s) => s.length > 0);
  }

  async function save() {
    if (!cfg || saving) return;
    saving = true;
    saved = false;
    error = null;
    try {
      const fallbackChains: Record<string, string[]> = {};
      for (const c of CONCERNS) {
        const list = parseList(fallbackText[c] ?? '');
        if (list.length > 0) fallbackChains[c] = list;
      }
      const body = {
        enabled: cfg.enabled,
        routing: cfg.routing,
        fallback_chains: fallbackChains,
        privacy: {
          lock_sensitive_to_local: cfg.privacy.lock_sensitive_to_local,
          local_providers: parseList(privacyLocalText),
        },
        default_budget: {
          soft_warning_usd: Number(cfg.default_budget.soft_warning_usd) || 0,
          hard_cap_usd: Number(cfg.default_budget.hard_cap_usd) || 0,
        },
      };
      const { data, error: apiErr, response } = await api.PUT('/admin/ai/config', { body: body as never });
      if (apiErr || !data) {
        // 422 carries findings; show inline.
        if (response?.status === 422) {
          const apiErrObj = apiErr as { error?: string; findings?: Finding[] } | undefined;
          error = apiErrObj?.error ?? t('admin.ai_inference.save_error');
          if (apiErrObj?.findings && cfg) {
            cfg = { ...cfg, findings: apiErrObj.findings };
          }
          return;
        }
        error = (apiErr as { error?: string } | undefined)?.error ?? t('admin.ai_inference.save_error');
        return;
      }
      cfg = data as InferenceConfig;
      saved = true;
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>{t('admin.ai_inference.title')} — artist-alley</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.ai_inference.title')}</h2>
<p class="mb-4 max-w-2xl text-sm text-fg-muted">{t('admin.ai_inference.intro')}</p>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if !cfg}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger" data-testid="ai-config-load-error">
    {error ?? t('admin.ai_inference.load_error')}
  </p>
{:else}
  <form
    onsubmit={(e) => {
      e.preventDefault();
      void save();
    }}
    class="max-w-3xl space-y-6"
    data-testid="ai-config-form"
  >
    {#if cfg.findings && cfg.findings.length > 0}
      <section class="rounded border border-warn/40 bg-warn-container px-3 py-2" data-testid="ai-config-findings">
        <h3 class="mb-1 text-sm font-semibold text-warn">{t('admin.ai_inference.findings_heading')}</h3>
        <p class="mb-2 text-xs text-fg-muted">{t('admin.ai_inference.findings_intro')}</p>
        <ul class="space-y-1 text-xs">
          {#each cfg.findings as finding (finding.code + (finding.concern ?? ''))}
            <li>
              <span class="font-mono">[{finding.code}]</span>
              {#if finding.concern}<span class="text-fg-muted">({finding.concern})</span>{/if}
              {finding.message}
            </li>
          {/each}
        </ul>
      </section>
    {/if}

    <label class="flex items-center gap-2 text-sm">
      <input type="checkbox" bind:checked={cfg.enabled} data-testid="ai-config-enabled" class="h-4 w-4 rounded border-border" />
      <span class="font-medium">{t('admin.ai_inference.enabled_label')}</span>
    </label>
    <p class="-mt-4 text-xs text-fg-muted">{t('admin.ai_inference.enabled_help')}</p>

    <section class="space-y-3 rounded border border-border bg-surface p-4">
      <h3 class="text-sm font-semibold">{t('admin.ai_inference.routing_section')}</h3>
      <p class="text-xs text-fg-muted">{t('admin.ai_inference.routing_help')}</p>
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        {#each CONCERNS as concern (concern)}
          <label class="block">
            <span class="text-xs text-fg-muted">{concern}</span>
            <input
              type="text"
              bind:value={cfg.routing[concern]}
              data-testid="ai-config-routing-{concern}"
              class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 font-mono text-sm focus-visible:border-border-strong focus:outline-none"
            />
          </label>
        {/each}
      </div>
    </section>

    <section class="space-y-3 rounded border border-border bg-surface p-4">
      <h3 class="text-sm font-semibold">{t('admin.ai_inference.fallback_section')}</h3>
      <p class="text-xs text-fg-muted">{t('admin.ai_inference.fallback_help')}</p>
      {#each CONCERNS as concern (concern)}
        <label class="block">
          <span class="text-xs text-fg-muted">{concern}</span>
          <input
            type="text"
            bind:value={fallbackText[concern]}
            placeholder="e.g. claude, openai, ollama"
            data-testid="ai-config-fallback-{concern}"
            class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 font-mono text-sm focus-visible:border-border-strong focus:outline-none"
          />
        </label>
      {/each}
    </section>

    <section class="space-y-3 rounded border border-border bg-surface p-4">
      <h3 class="text-sm font-semibold">{t('admin.ai_inference.privacy_section')}</h3>
      <label class="flex items-center gap-2 text-sm">
        <input type="checkbox" bind:checked={cfg.privacy.lock_sensitive_to_local} data-testid="ai-config-privacy-lock" class="h-4 w-4 rounded border-border" />
        <span>{t('admin.ai_inference.privacy_lock_label')}</span>
      </label>
      <p class="text-xs text-fg-muted">{t('admin.ai_inference.privacy_lock_help')}</p>
      <label class="block">
        <span class="text-xs text-fg-muted">{t('admin.ai_inference.privacy_local_label')}</span>
        <input
          type="text"
          bind:value={privacyLocalText}
          data-testid="ai-config-privacy-local"
          class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 font-mono text-sm focus-visible:border-border-strong focus:outline-none"
        />
        <span class="mt-1 block text-xs text-fg-muted">{t('admin.ai_inference.privacy_local_help')}</span>
      </label>
    </section>

    <section class="space-y-3 rounded border border-border bg-surface p-4">
      <h3 class="text-sm font-semibold">{t('admin.ai_inference.budget_section')}</h3>
      <div class="grid grid-cols-2 gap-3">
        <label class="block">
          <span class="text-xs text-fg-muted">{t('admin.ai_inference.budget_soft_label')}</span>
          <input
            type="number"
            bind:value={cfg.default_budget.soft_warning_usd}
            min="0"
            data-testid="ai-config-budget-soft"
            class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
          />
          <span class="mt-1 block text-xs text-fg-muted">{t('admin.ai_inference.budget_soft_help')}</span>
        </label>
        <label class="block">
          <span class="text-xs text-fg-muted">{t('admin.ai_inference.budget_hard_label')}</span>
          <input
            type="number"
            bind:value={cfg.default_budget.hard_cap_usd}
            min="0"
            data-testid="ai-config-budget-hard"
            class="mt-1 w-full rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
          />
          <span class="mt-1 block text-xs text-fg-muted">{t('admin.ai_inference.budget_hard_help')}</span>
        </label>
      </div>
    </section>

    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger" data-testid="ai-config-error">{error}</p>
    {/if}
    {#if saved}
      <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success" data-testid="ai-config-saved">
        {t('admin.ai_inference.saved')}
      </p>
    {/if}

    <button
      type="submit"
      disabled={saving}
      data-testid="ai-config-save"
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {saving ? t('common.loading') : t('admin.ai_inference.save')}
    </button>
  </form>
{/if}
