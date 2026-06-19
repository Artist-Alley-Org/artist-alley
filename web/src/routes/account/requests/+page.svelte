<script lang="ts">
  // /account/requests — the caller's own resource requests
  // (Phase 1.17.E). Shows pending + decided requests with state
  // badges. Read-only; the decision happens at /admin/requests.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface ResourceRequest {
    id: string;
    target_asset_id: string;
    requested_capability: string;
    reason?: string;
    state: 'pending' | 'granted' | 'denied' | 'expired';
    decided_at?: string | null;
    decision_reason?: string;
    expires_at?: string | null;
    requested_at: string;
  }

  let items = $state<ResourceRequest[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  onMount(() => {
    void refresh();
  });

  async function refresh() {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/account/requests', { params: { query: { limit: 100 } } });
      if (r.error) {
        error = (r.error as { error?: string }).error ?? 'Failed to load.';
        return;
      }
      items = ((r.data as { items?: ResourceRequest[] }).items ?? []) as ResourceRequest[];
    } finally {
      loading = false;
    }
  }

  function stateBadge(s: ResourceRequest['state']): string {
    switch (s) {
      case 'pending':  return 'bg-warning/15 text-warning border border-warning/40';
      case 'granted':  return 'bg-success/15 text-success border border-success/40';
      case 'denied':   return 'bg-danger/15 text-danger border border-danger/40';
      case 'expired':  return 'bg-muted/30 text-muted-foreground border border-muted/50';
    }
  }
</script>

<svelte:head><title>{t('account.requests.title')} — artist-alley</title></svelte:head>

<section class="space-y-4">
  <header>
    <h1 class="text-2xl font-semibold text-fg">{t('account.requests.title')}</h1>
    <p class="mt-1 text-sm text-fg-muted">{t('account.requests.intro')}</p>
  </header>

  {#if loading}
    <p class="text-fg-muted">{t('common.loading')}</p>
  {:else if error}
    <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {:else if items.length === 0}
    <p class="text-fg-muted" data-testid="requests-empty">{t('account.requests.empty')}</p>
  {:else}
    <ul class="space-y-2" data-testid="requests-list">
      {#each items as r (r.id)}
        <li class="rounded-lg border border-border bg-surface-elevated p-3">
          <div class="flex items-center justify-between gap-3">
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <span class="rounded px-2 py-0.5 text-xs font-medium {stateBadge(r.state)}" data-testid="request-state-{r.state}">{r.state}</span>
                <code class="truncate text-xs text-fg-muted">{r.requested_capability}</code>
              </div>
              {#if r.reason}
                <p class="mt-1 text-sm text-fg">{r.reason}</p>
              {/if}
              {#if r.decision_reason}
                <p class="mt-1 text-xs italic text-fg-muted">{t('account.requests.decision_reason')}: {r.decision_reason}</p>
              {/if}
              {#if r.expires_at}
                <p class="mt-1 text-xs text-fg-muted">{t('account.requests.expires_at', { ts: r.expires_at })}</p>
              {/if}
            </div>
            <a href={`/assets/${r.target_asset_id}`} class="text-xs text-accent hover:underline">{t('account.requests.view_asset')}</a>
          </div>
        </li>
      {/each}
    </ul>
  {/if}
</section>
