<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/requests — both directions of the request workflow.
  //
  // OUTGOING (Phase 1.17.E): the caller's own requests with state
  // badges. Read-only — someone else decides these.
  //
  // INCOMING (#881): pending requests against assets the caller OWNS,
  // with the same decision panel /admin/requests uses. This half exists
  // because the person with the strongest claim to decide had no route
  // to: /admin/requests is gated on requests.read / share.grant, which
  // an artist has no reason to hold, so every request on their own work
  // needed an administrator. The section renders only when there is
  // something to decide — an account with no incoming requests should
  // not grow a permanent empty panel about a workflow it never uses.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import RequestQueue, { type QueuedRequest } from '$components/RequestQueue.svelte';

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
  let incoming = $state<QueuedRequest[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);
  // Survives the incoming section emptying out — see RequestQueue's
  // `ondecided` note. Deciding the last request removes the panel, and
  // a confirmation rendered inside it would go with it.
  let decided = $state<string | null>(null);

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
      await refreshIncoming();
    } finally {
      loading = false;
    }
  }

  // Kept separate from refresh() so deciding a row re-reads the queue
  // without blanking the outgoing list underneath it.
  async function refreshIncoming() {
    const r = await api.GET('/account/requests/incoming', {
      params: { query: { limit: 100, offset: 0 } },
    });
    if (r.error) return;
    incoming = ((r.data as { items?: QueuedRequest[] }).items ?? []) as QueuedRequest[];
  }

  // The expiry was interpolated raw, so the page rendered
  // "Access expires: 2026-08-24T05:12:14.633384Z" at a user. It went
  // unnoticed because nothing linked here until #600.
  function formatDate(iso: string): string {
    try {
      return new Date(iso).toLocaleDateString();
    } catch {
      return iso;
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

<svelte:head><title>{t('account.requests.title')} — {site.name}</title></svelte:head>

<section class="space-y-4">
  <header>
    <h1 class="text-2xl font-semibold text-fg">{t('account.requests.title')}</h1>
    <p class="mt-1 text-sm text-fg-muted">{t('account.requests.intro')}</p>
  </header>

  {#if decided}
    <p
      role="status"
      data-testid="decision-recorded"
      class="rounded border border-success/40 bg-success/10 px-3 py-2 text-sm text-success"
    >
      {decided}
    </p>
  {/if}

  {#if incoming.length > 0}
    <section class="space-y-2" data-testid="incoming-requests">
      <h2 class="text-lg font-semibold text-fg">{t('account.requests.incoming_title')}</h2>
      <p class="text-sm text-fg-muted">
        {t('account.requests.incoming_intro', { total: incoming.length })}
      </p>
      <RequestQueue
        items={incoming}
        testidPrefix="incoming-request"
        ondecided={async (msg) => {
          decided = msg;
          await refreshIncoming();
        }}
      />
    </section>
    <h2 class="pt-2 text-lg font-semibold text-fg">{t('account.requests.outgoing_title')}</h2>
  {/if}

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
                <p class="mt-1 text-xs text-fg-muted">{t('account.requests.expires_at', { ts: formatDate(r.expires_at) })}</p>
              {/if}
            </div>
            <a href={`/assets/${r.target_asset_id}`} class="text-xs text-accent hover:underline">{t('account.requests.view_asset')}</a>
          </div>
        </li>
      {/each}
    </ul>
  {/if}
</section>
