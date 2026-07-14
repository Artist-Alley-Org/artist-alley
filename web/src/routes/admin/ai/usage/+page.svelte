<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Phase 1.14.A — /admin/ai/usage cost dashboard.
  //
  // Per-provider call count + spend + status breakdown for one
  // billing period. The backend (GET /admin/ai/usage) defaults to
  // the current UTC month; this page lets the operator scrub via
  // a month picker.
  //
  // Cost is integer micros on the wire; we format to USD here.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface UsageRow {
    provider: string;
    call_count: number;
    cost_usd_micros: number;
    status_counts: Record<string, number>;
  }
  interface UsageReport {
    billing_period: string;
    total_cost_usd_micros: number;
    providers: UsageRow[];
  }

  // Default to the current YYYY-MM in UTC. Operators outside UTC
  // still see consistent month boundaries — matches the backend
  // currentBillingPeriod helper.
  function currentPeriod(): string {
    const d = new Date();
    return d.getUTCFullYear() + '-' + String(d.getUTCMonth() + 1).padStart(2, '0');
  }

  let period = $state(currentPeriod());
  let report = $state<UsageReport | null>(null);
  let loading = $state(true);
  let error = $state<string | null>(null);

  onMount(() => void load());

  async function load() {
    loading = true;
    error = null;
    try {
      const { data } = await api.GET('/admin/ai/usage', {
        params: { query: { billing_period: period } },
      });
      if (!data) {
        error = t('admin.ai_usage.load_error');
        return;
      }
      report = data as UsageReport;
    } finally {
      loading = false;
    }
  }

  function microsToUSD(micros: number): string {
    return '$' + (micros / 1_000_000).toFixed(2);
  }

  async function reloadAfterChange() {
    await load();
  }
</script>

<svelte:head><title>{t('admin.ai_usage.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.ai_usage.title')}</h2>
<p class="mb-4 max-w-2xl text-sm text-fg-muted">{t('admin.ai_usage.intro')}</p>

<div class="mb-4 flex items-end gap-3">
  <label class="block">
    <span class="text-xs text-fg-muted">{t('admin.ai_usage.period_label')}</span>
    <input
      type="month"
      bind:value={period}
      onchange={() => reloadAfterChange()}
      data-testid="ai-usage-period"
      class="mt-1 rounded border border-border bg-surface px-3 py-1.5 text-sm focus-visible:border-border-strong focus:outline-none"
    />
  </label>
</div>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if error || !report}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger" data-testid="ai-usage-error">
    {error ?? t('admin.ai_usage.load_error')}
  </p>
{:else}
  <section class="mb-4 rounded border border-border bg-surface px-4 py-3" data-testid="ai-usage-total">
    <span class="text-xs text-fg-muted">{t('admin.ai_usage.total_label')}</span>
    <p class="mt-1 text-2xl font-semibold tabular-nums">{microsToUSD(report.total_cost_usd_micros)}</p>
  </section>

  {#if report.providers.length === 0}
    <p class="rounded-md bg-surface-elevated px-4 py-6 text-center text-fg-muted" data-testid="ai-usage-empty">
      {t('admin.ai_usage.no_providers')}
    </p>
  {:else}
    <table class="w-full text-sm" data-testid="ai-usage-table">
      <thead class="text-left text-xs uppercase tracking-wider text-fg-muted">
        <tr>
          <th class="py-2">{t('admin.ai_usage.column_provider')}</th>
          <th class="py-2 text-right">{t('admin.ai_usage.column_calls')}</th>
          <th class="py-2 text-right">{t('admin.ai_usage.column_cost')}</th>
          <th class="py-2">{t('admin.ai_usage.column_status')}</th>
        </tr>
      </thead>
      <tbody>
        {#each report.providers as row (row.provider)}
          <tr class="border-t border-border" data-testid="ai-usage-row-{row.provider}">
            <td class="py-2 font-mono text-xs">{row.provider}</td>
            <td class="py-2 text-right tabular-nums">{row.call_count}</td>
            <td class="py-2 text-right tabular-nums">{microsToUSD(row.cost_usd_micros)}</td>
            <td class="py-2 text-fg-muted text-xs">
              {#each Object.entries(row.status_counts) as [status, count] (status)}
                <span class="mr-2">{status}: {count}</span>
              {/each}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
{/if}
