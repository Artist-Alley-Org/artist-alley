<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin: received assets and their transfer events (#1574 slice 2).
  //
  // Two read-only views, system.admin only:
  //   - the receiver's records of assets peers announced to it, in every
  //     state and tier (team included), without bytes;
  //   - the transfer and fixity event ledger. Its rows are snapshots kept
  //     after the record or the peer is deleted (the page marks those), and
  //     a mismatched file's row shows what was listed against what arrived.
  // Choosing a record shows its events; the filters narrow either list.
  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import type { components } from '$api/schema';

  type Record_ = components['schemas']['AdminReceivedAsset'];
  type Event_ = components['schemas']['FederationTransferEvent'];
  type Peer = { id: string; instance_url: string; display_name?: string };

  let peers = $state<Peer[]>([]);

  let stateFilter = $state('');
  let peerFilter = $state('');
  let records = $state<Record_[]>([]);
  let recordsCursor = $state<string | null>(null);
  let recordsLoading = $state(false);
  let recordsError = $state('');

  let selectedRecord = $state<string | null>(null);
  let outcomeFilter = $state('');
  let typeFilter = $state('');
  let events = $state<Event_[]>([]);
  let eventsCursor = $state<string | null>(null);
  let eventsLoading = $state(false);
  let eventsError = $state('');

  function errorOf(e: unknown): string {
    return (e as { error?: string } | undefined)?.error || 'load failed';
  }

  async function loadPeers() {
    const r = await api.GET('/admin/federation/peers', {});
    if (r.data) peers = (r.data as { items?: Peer[] }).items || [];
  }

  async function loadRecords(append = false) {
    recordsLoading = true;
    recordsError = '';
    try {
      const query: Record<string, string | number> = { limit: 100 };
      if (stateFilter) query.state = stateFilter;
      if (peerFilter) query.peer_id = peerFilter;
      if (append && recordsCursor) query.cursor = recordsCursor;
      const r = await api.GET('/admin/federation/received-assets', { params: { query } });
      if (r.error || !r.data) {
        recordsError = errorOf(r.error);
        return;
      }
      records = append ? [...records, ...r.data.items] : r.data.items;
      recordsCursor = r.data.next_cursor ?? null;
    } finally {
      recordsLoading = false;
    }
  }

  async function loadEvents(append = false) {
    eventsLoading = true;
    eventsError = '';
    try {
      const query: Record<string, string | number> = { limit: 100 };
      if (selectedRecord) query.received_asset_id = selectedRecord;
      if (peerFilter) query.peer_id = peerFilter;
      if (outcomeFilter) query.outcome = outcomeFilter;
      if (typeFilter) query.event_type = typeFilter;
      if (append && eventsCursor) query.cursor = eventsCursor;
      const r = await api.GET('/admin/federation/transfer-events', { params: { query } });
      if (r.error || !r.data) {
        eventsError = errorOf(r.error);
        return;
      }
      events = append ? [...events, ...r.data.items] : r.data.items;
      eventsCursor = r.data.next_cursor ?? null;
    } finally {
      eventsLoading = false;
    }
  }

  function showEventsFor(id: string | null) {
    selectedRecord = id;
    void loadEvents(false);
  }

  function applyFilters() {
    void loadRecords(false);
    void loadEvents(false);
  }

  function clearFilters() {
    stateFilter = '';
    peerFilter = '';
    outcomeFilter = '';
    typeFilter = '';
    selectedRecord = null;
    applyFilters();
  }

  function peerName(id: string, url?: string, name?: string | null): string {
    const p = peers.find((x) => x.id === id);
    return name || p?.display_name || url || p?.instance_url || id.slice(0, 8);
  }

  function short(h?: string | null): string {
    return h ? h.slice(0, 12) : '';
  }

  function size(n?: number | null): string {
    return n === undefined || n === null ? '' : `${n} B`;
  }

  function pill(kind: string): string {
    switch (kind) {
      case 'complete':
      case 'success':
        return 'bg-success/15 text-success border-success/40';
      case 'rejected':
      case 'failure':
        return 'bg-danger/15 text-danger border-danger/40';
      default:
        return 'bg-warning/15 text-warning border-warning/40';
    }
  }

  onMount(async () => {
    await loadPeers();
    await Promise.all([loadRecords(), loadEvents()]);
  });
</script>

<svelte:head><title>Received assets - {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Received assets</h2>
  <p class="text-sm text-fg-muted">
    Assets other instances announced to this one, in every state, and the history of each transfer and
    size and checksum check. The history is kept after a record or its peer is removed, for the retention
    period set by <code>AA_FEDERATION_EVENT_RETENTION_DAYS</code>. Mismatched bytes are never kept: an
    event shows what was listed and what arrived.
  </p>
</header>

<section class="mb-6 rounded border border-border bg-bg-soft p-4">
  <h3 class="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-muted">Filters</h3>
  <div class="grid gap-3 sm:grid-cols-4">
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Peer</span>
      <select bind:value={peerFilter} class="rounded border border-border-strong bg-bg p-2 text-fg" data-testid="received-admin-peer">
        <option value="">All</option>
        {#each peers as p (p.id)}
          <option value={p.id}>{p.display_name || p.instance_url}</option>
        {/each}
      </select>
    </label>
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Record state</span>
      <select bind:value={stateFilter} class="rounded border border-border-strong bg-bg p-2 text-fg">
        <option value="">All</option>
        <option value="pending">pending</option>
        <option value="complete">complete</option>
        <option value="rejected">rejected</option>
      </select>
    </label>
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Event outcome</span>
      <select bind:value={outcomeFilter} class="rounded border border-border-strong bg-bg p-2 text-fg">
        <option value="">All</option>
        <option value="success">success</option>
        <option value="warning">warning</option>
        <option value="failure">failure</option>
      </select>
    </label>
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Event type</span>
      <select bind:value={typeFilter} class="rounded border border-border-strong bg-bg p-2 text-fg">
        <option value="">All</option>
        <option value="transfer">transfer</option>
        <option value="fixity-check">fixity-check</option>
        <option value="ingestion">ingestion</option>
      </select>
    </label>
  </div>
  <div class="mt-3 flex gap-2">
    <button
      onclick={applyFilters}
      disabled={recordsLoading || eventsLoading}
      class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
    >Apply</button>
    <button onclick={clearFilters} class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg-soft">Clear</button>
  </div>
</section>

<h3 class="mb-2 text-lg font-semibold">Records</h3>
{#if recordsError}
  <div class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{recordsError}</div>
{/if}
<section class="mb-8 overflow-x-auto rounded border border-border bg-bg-soft">
  <table class="w-full text-sm" data-testid="received-admin-records">
    <thead class="border-b border-border bg-bg/60 text-fg-muted">
      <tr>
        <th class="px-3 py-2 text-left font-medium">State</th>
        <th class="px-3 py-2 text-left font-medium">Peer</th>
        <th class="px-3 py-2 text-left font-medium">Source</th>
        <th class="px-3 py-2 text-left font-medium">Tier</th>
        <th class="px-3 py-2 text-left font-medium">Originals</th>
        <th class="px-3 py-2 text-left font-medium">Attempts</th>
        <th class="px-3 py-2 text-left font-medium">Received</th>
        <th class="px-3 py-2"></th>
      </tr>
    </thead>
    <tbody>
      {#each records as r (r.id)}
        <tr class="border-b border-border/40 last:border-0" data-testid="received-admin-record" data-state={r.state}>
          <td class="px-3 py-2">
            <span class="inline-flex rounded border px-2 py-0.5 text-xs font-medium {pill(r.state)}">{r.state}</span>
            {#if r.reason}<code class="ml-1 text-xs text-danger">{r.reason}</code>{/if}
          </td>
          <td class="px-3 py-2">{peerName(r.peer_id, r.peer_url, r.peer_name)}</td>
          <td class="max-w-xs truncate px-3 py-2" title={r.origin_asset_uri}><code class="text-xs">{r.origin_asset_uri}</code></td>
          <td class="px-3 py-2">{r.sensitivity}</td>
          <td class="px-3 py-2">{r.original_count} ({size(r.original_bytes)})</td>
          <td class="px-3 py-2">{r.attempts}</td>
          <td class="px-3 py-2 text-fg-muted">{new Date(r.received_at).toLocaleString()}</td>
          <td class="px-3 py-2">
            <button
              onclick={() => showEventsFor(r.id)}
              class="rounded border border-border px-2 py-1 text-xs hover:bg-bg"
              data-testid="received-admin-show-events"
            >Events</button>
          </td>
        </tr>
      {/each}
      {#if records.length === 0 && !recordsLoading}
        <tr><td colspan="8" class="px-3 py-6 text-center text-fg-muted">No records match the filter.</td></tr>
      {/if}
    </tbody>
  </table>
  {#if recordsCursor}
    <div class="border-t border-border p-3 text-center">
      <button onclick={() => loadRecords(true)} disabled={recordsLoading} class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg disabled:opacity-50">Load more</button>
    </div>
  {/if}
</section>

<h3 class="mb-2 text-lg font-semibold">
  Transfer events
  {#if selectedRecord}
    <span class="ml-2 text-sm font-normal text-fg-muted" data-testid="received-admin-event-scope">
      for record <code>{selectedRecord.slice(0, 8)}</code>
      <button onclick={() => showEventsFor(null)} class="ml-1 underline">show all</button>
    </span>
  {/if}
</h3>
{#if eventsError}
  <div class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{eventsError}</div>
{/if}
<section class="overflow-x-auto rounded border border-border bg-bg-soft">
  <table class="w-full text-sm" data-testid="received-admin-events">
    <thead class="border-b border-border bg-bg/60 text-fg-muted">
      <tr>
        <th class="px-3 py-2 text-left font-medium">When</th>
        <th class="px-3 py-2 text-left font-medium">Event</th>
        <th class="px-3 py-2 text-left font-medium">Peer</th>
        <th class="px-3 py-2 text-left font-medium">Record</th>
        <th class="px-3 py-2 text-left font-medium">File</th>
        <th class="px-3 py-2 text-left font-medium">Listed</th>
        <th class="px-3 py-2 text-left font-medium">Arrived</th>
        <th class="px-3 py-2 text-left font-medium">Attempt</th>
      </tr>
    </thead>
    <tbody>
      {#each events as e (e.id)}
        <tr class="border-b border-border/40 align-top last:border-0" data-testid="received-admin-event" data-outcome={e.outcome}>
          <td class="px-3 py-2 text-fg-muted">{new Date(e.occurred_at).toLocaleString()}</td>
          <td class="px-3 py-2">
            <code class="text-xs">{e.event_type}</code>
            <span class="ml-1 inline-flex rounded border px-2 py-0.5 text-xs font-medium {pill(e.outcome)}">{e.outcome}</span>
            {#if e.reason}<div><code class="text-xs text-danger">{e.reason}</code></div>{/if}
            {#if e.detail}<div class="max-w-xs truncate text-xs text-fg-muted" title={e.detail}>{e.detail}</div>{/if}
          </td>
          <td class="px-3 py-2">
            {e.peer_display_name || e.peer_instance_url}
            {#if !e.peer_exists}<span class="ml-1 text-xs text-warning" data-testid="received-admin-peer-gone">peer removed</span>{/if}
          </td>
          <td class="px-3 py-2">
            <code class="text-xs">{e.received_asset_id.slice(0, 8)}</code>
            <span class="text-xs text-fg-muted">then {e.record_state_after}</span>
            {#if !e.record_exists}<span class="ml-1 text-xs text-warning" data-testid="received-admin-record-gone">record removed</span>{/if}
          </td>
          <td class="px-3 py-2">
            {#if e.file_role}{e.file_role} #{e.file_position}{/if}
          </td>
          <td class="px-3 py-2 font-mono text-xs" data-testid="received-admin-expected">
            {#if e.expected_sha256}{short(e.expected_sha256)}<br />{size(e.expected_size_bytes)}{/if}
          </td>
          <td class="px-3 py-2 font-mono text-xs" data-testid="received-admin-observed">
            {#if e.observed_basis}
              {#if e.observed_sha256}{short(e.observed_sha256)}<br />{/if}
              {#if e.observed_size_bytes !== undefined && e.observed_size_bytes !== null}
                {e.observed_truncated ? `more than ${size(e.expected_size_bytes)}` : size(e.observed_size_bytes)}<br />
              {/if}
              {#if e.mismatch_algorithm}{e.mismatch_algorithm}: {short(e.mismatch_observed)}<br />{/if}
              <span class="font-sans text-fg-muted">{e.observed_basis}</span>
            {/if}
          </td>
          <td class="px-3 py-2">{e.attempt}</td>
        </tr>
      {/each}
      {#if events.length === 0 && !eventsLoading}
        <tr><td colspan="8" class="px-3 py-6 text-center text-fg-muted">No events match the filter.</td></tr>
      {/if}
    </tbody>
  </table>
  {#if eventsCursor}
    <div class="border-t border-border p-3 text-center">
      <button onclick={() => loadEvents(true)} disabled={eventsLoading} class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg disabled:opacity-50">Load more</button>
    </div>
  {/if}
</section>
