<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/requests — approver-facing pending list (Phase 1.17.E).
  //
  // The queue + inline decision panel live in RequestQueue, shared with
  // the owner-facing queue on /account/requests (#881). This page is the
  // approver's view of the SAME decision UI over a different fetch:
  // every pending request on the instance, gated on requests.read /
  // share.grant / system.admin.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import RequestQueue, { type QueuedRequest } from '$components/RequestQueue.svelte';

  let items = $state<QueuedRequest[]>([]);
  let total = $state(0);
  let loading = $state(true);
  let error = $state<string | null>(null);
  // Page-level, because deciding the last pending request replaces the
  // whole list with the empty state — a confirmation rendered inside the
  // row would vanish with it. See RequestQueue's `ondecided`.
  let decided = $state<string | null>(null);

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
      const data = r.data as { items?: QueuedRequest[]; total?: number };
      items = (data.items ?? []) as QueuedRequest[];
      total = data.total ?? 0;
    } finally {
      loading = false;
    }
  }
</script>

<svelte:head><title>{t('admin.requests.title')} — {site.name}</title></svelte:head>

<section class="space-y-4">
  <header>
    <h1 class="text-2xl font-semibold text-fg">{t('admin.requests.title')}</h1>
    <p class="mt-1 text-sm text-fg-muted">{t('admin.requests.intro', { total })}</p>
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

  {#if loading}
    <p class="text-fg-muted">{t('common.loading')}</p>
  {:else if error}
    <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {:else if items.length === 0}
    <p class="text-fg-muted" data-testid="requests-empty">{t('admin.requests.empty')}</p>
  {:else}
    <RequestQueue
      {items}
      testidPrefix="admin-request"
      ondecided={async (msg) => {
        decided = msg;
        await refresh();
      }}
    />
  {/if}
</section>
