<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/requests — approver-facing pending list (Phase 1.17.E).
  // Inline decision dialog (grant / deny + reason + optional
  // expires_at on grant) per row. 409 on race-decide.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface ResourceRequest {
    id: string;
    requester_user_ref: number;
    target_asset_id: string;
    requested_capability: string;
    reason?: string;
    state: string;
    requested_at: string;
  }

  let items = $state<ResourceRequest[]>([]);
  let total = $state(0);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Per-row decision form state — keyed by request id.
  let openId = $state<string | null>(null);
  let decisionReason = $state('');
  let decisionExpires = $state('');
  let deciding = $state(false);
  let decideMsg = $state<{ kind: 'ok' | 'err'; text: string } | null>(null);

  onMount(() => { void refresh(); });

  async function refresh() {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/admin/requests', { params: { query: { limit: 100, offset: 0 } } });
      if (r.error) {
        error = (r.error as { error?: string }).error ?? 'Failed to load.';
        return;
      }
      const data = r.data as { items?: ResourceRequest[]; total?: number };
      items = (data.items ?? []) as ResourceRequest[];
      total = data.total ?? 0;
    } finally {
      loading = false;
    }
  }

  function openDecide(id: string) {
    openId = id;
    decisionReason = '';
    decisionExpires = '';
    decideMsg = null;
  }

  async function decide(id: string, decision: 'granted' | 'denied') {
    if (deciding) return;
    deciding = true;
    decideMsg = null;
    try {
      const body: Record<string, unknown> = { decision };
      if (decisionReason) body.reason = decisionReason;
      if (decision === 'granted' && decisionExpires) body.expires_at = new Date(decisionExpires).toISOString();
      const r = await api.POST('/admin/requests/{id}/decide', { params: { path: { id } }, body: body as never });
      if (r.error || !r.data) {
        const err = (r.error as { error?: string }).error ?? 'Failed.';
        decideMsg = { kind: 'err', text: err };
        return;
      }
      decideMsg = { kind: 'ok', text: t('admin.requests.decided', { decision }) };
      openId = null;
      await refresh();
    } finally {
      deciding = false;
    }
  }
</script>

<svelte:head><title>{t('admin.requests.title')} — {site.name}</title></svelte:head>

<section class="space-y-4">
  <header>
    <h1 class="text-2xl font-semibold text-fg">{t('admin.requests.title')}</h1>
    <p class="mt-1 text-sm text-fg-muted">{t('admin.requests.intro', { total })}</p>
  </header>

  {#if loading}
    <p class="text-fg-muted">{t('common.loading')}</p>
  {:else if error}
    <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {:else if items.length === 0}
    <p class="text-fg-muted" data-testid="requests-empty">{t('admin.requests.empty')}</p>
  {:else}
    <ul class="space-y-2" data-testid="admin-requests-list">
      {#each items as r (r.id)}
        <li class="rounded-lg border border-border bg-surface-elevated p-3" data-testid="admin-request-row-{r.id}">
          <div class="flex items-center justify-between gap-3">
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <code class="text-xs text-fg">{r.requested_capability}</code>
                <span class="text-xs text-fg-muted">user_ref={r.requester_user_ref}</span>
              </div>
              {#if r.reason}
                <p class="mt-1 text-sm text-fg">{r.reason}</p>
              {/if}
              <p class="mt-1 text-xs text-fg-muted">asset: <a href={`/assets/${r.target_asset_id}`} class="text-accent hover:underline">{r.target_asset_id.slice(0, 8)}…</a></p>
            </div>
            {#if openId === r.id}
              <button type="button" class="text-xs text-fg-muted hover:underline" onclick={() => (openId = null)}>{t('common.cancel')}</button>
            {:else}
              <button type="button" class="rounded border border-border bg-surface px-3 py-1 text-xs hover:border-accent" onclick={() => openDecide(r.id)} data-testid="admin-decide-{r.id}">{t('admin.requests.decide')}</button>
            {/if}
          </div>

          {#if openId === r.id}
            <div class="mt-3 space-y-2 border-t border-border pt-3">
              <label class="block text-xs">
                <span class="mb-1 block text-fg-muted">{t('admin.requests.reason')}</span>
                <input type="text" bind:value={decisionReason} maxlength="1000" class="w-full rounded border border-border bg-surface px-2 py-1 text-sm" />
              </label>
              <label class="block text-xs">
                <span class="mb-1 block text-fg-muted">{t('admin.requests.expires_at')}</span>
                <input type="datetime-local" bind:value={decisionExpires} class="w-full rounded border border-border bg-surface px-2 py-1 text-sm" />
              </label>
              <div class="flex flex-wrap gap-2">
                <button type="button" disabled={deciding} onclick={() => decide(r.id, 'granted')} class="rounded border border-success bg-success/10 px-3 py-1 text-xs font-medium text-success hover:bg-success/20 disabled:opacity-50" data-testid="admin-grant-{r.id}">{t('admin.requests.grant')}</button>
                <button type="button" disabled={deciding} onclick={() => decide(r.id, 'denied')} class="rounded border border-danger bg-danger/10 px-3 py-1 text-xs font-medium text-danger hover:bg-danger/20 disabled:opacity-50" data-testid="admin-deny-{r.id}">{t('admin.requests.deny')}</button>
              </div>
              {#if decideMsg}
                <p class={decideMsg.kind === 'ok' ? 'text-sm text-success' : 'text-sm text-danger'}>{decideMsg.text}</p>
              {/if}
            </div>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</section>
