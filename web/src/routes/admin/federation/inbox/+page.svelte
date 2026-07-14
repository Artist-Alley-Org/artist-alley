<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin federation inbox — Phase 1.22.D-c.
  //
  // Per-peer inbound activity view across federation_inbox.
  // Read-only — inbox rows can't be re-queued the way outbox
  // rows can (activity_uri UNIQUE means re-delivery is
  // idempotent on the existing row). Admins investigating
  // rejections need to coordinate with the sender's outbox.
  //
  // For rejected rows, the reject_reason maps to spec §12.1;
  // the table surfaces it prominently so operators can
  // recognize patterns (envelope_sig_missing → spec-noncompliant
  // peer; unshared_object → sender's local share-cache drift).

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';

  type InboxRow = {
    id: string;
    activity_uri: string;
    peer_id: string;
    actor_uri: string;
    activity_type: string;
    object_kind?: string | null;
    object_id?: string | null;
    http_sig_key?: string | null;
    status: 'pending' | 'processed' | 'rejected' | 'failed';
    reject_reason?: string | null;
    dispatch_attempts: number;
    last_attempt_at?: string | null;
    last_error?: string | null;
    received_at: string;
    processed_at?: string | null;
    correlation_activity_id?: string | null;
  };
  type Peer = { id: string; instance_url: string; display_name?: string };

  let peers = $state<Peer[]>([]);
  let peerMap = $derived.by(() => {
    const m: Record<string, Peer> = {};
    for (const p of peers) m[p.id] = p;
    return m;
  });

  let peerFilter = $state('');
  let statusFilter = $state<'' | InboxRow['status']>('');
  let activityTypeFilter = $state('');
  let rows = $state<InboxRow[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(false);
  let error = $state('');

  async function loadPeers() {
    const r = await api.GET('/admin/federation/peers', {});
    if (r.data) peers = r.data.items || [];
  }

  async function loadInbox(append = false) {
    loading = true;
    error = '';
    try {
      const params: Record<string, string | number> = { limit: 100 };
      if (peerFilter) params.peer_id = peerFilter;
      if (statusFilter) params.status = statusFilter;
      if (activityTypeFilter) params.activity_type = activityTypeFilter;
      if (append && nextCursor) params.cursor = nextCursor;
      const r = await api.GET('/admin/federation/inbox', { params: { query: params } });
      if (r.error) {
        error = (r.error as { error?: string }).error || 'load failed';
        return;
      }
      const data = r.data as { items: InboxRow[]; next_cursor?: string | null };
      rows = append ? [...rows, ...data.items] : data.items;
      nextCursor = data.next_cursor || null;
    } finally {
      loading = false;
    }
  }

  function peerLabel(id: string): string {
    const p = peerMap[id];
    if (!p) return id.slice(0, 8) + '…';
    return p.display_name || p.instance_url;
  }

  function relTime(s: string): string {
    const d = new Date(s);
    const ms = Date.now() - d.getTime();
    if (ms < 0) return 'in ' + relUnits(-ms);
    return relUnits(ms) + ' ago';
  }
  function relUnits(ms: number): string {
    const s = Math.floor(ms / 1000);
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m}m`;
    const h = Math.floor(m / 60);
    if (h < 24) return `${h}h`;
    return `${Math.floor(h / 24)}d`;
  }

  function statusPillClass(s: InboxRow['status']): string {
    switch (s) {
      case 'pending':   return 'bg-info/15 text-info border-info/40';
      case 'processed': return 'bg-success/15 text-success border-success/40';
      case 'rejected':  return 'bg-danger/15 text-danger border-danger/40';
      case 'failed':    return 'bg-warning/15 text-warning border-warning/40';
    }
  }

  onMount(async () => {
    await loadPeers();
    await loadInbox();
  });
</script>

<svelte:head><title>Federation inbox — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Federation inbox</h2>
  <p class="text-sm text-fg-muted">
    Inbound activities from paired peers. Read-only — the
    <code>activity_uri</code> UNIQUE constraint means re-delivered
    envelopes land on the existing row idempotently. For rejected
    rows, the reason maps to spec §12.1; coordinate with the
    sender's outbox to investigate recurring failures.
  </p>
</header>

<section class="mb-6 rounded border border-border bg-bg-soft p-4">
  <h3 class="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-muted">Filters</h3>
  <div class="grid gap-3 sm:grid-cols-3">
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Peer</span>
      <select bind:value={peerFilter} class="rounded border border-border bg-bg p-2 text-fg">
        <option value="">— all —</option>
        {#each peers as p}
          <option value={p.id}>{p.display_name || p.instance_url}</option>
        {/each}
      </select>
    </label>
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Status</span>
      <select bind:value={statusFilter} class="rounded border border-border bg-bg p-2 text-fg">
        <option value="">— all —</option>
        <option value="pending">pending</option>
        <option value="processed">processed</option>
        <option value="rejected">rejected</option>
        <option value="failed">failed</option>
      </select>
    </label>
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Activity type</span>
      <input
        bind:value={activityTypeFilter}
        placeholder="Like / Create / aa:Share / …"
        class="rounded border border-border bg-bg p-2 text-fg"
      />
    </label>
  </div>
  <div class="mt-3 flex gap-2">
    <button
      onclick={() => loadInbox(false)}
      disabled={loading}
      class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
    >{loading ? 'Loading…' : 'Apply'}</button>
    <button
      onclick={() => { peerFilter = ''; statusFilter = ''; activityTypeFilter = ''; loadInbox(false); }}
      class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg-soft"
    >Clear</button>
  </div>
</section>

{#if error}
  <div class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{error}</div>
{/if}

<section class="rounded border border-border bg-bg-soft">
  <table class="w-full text-sm">
    <thead class="border-b border-border bg-bg/60 text-fg-muted">
      <tr>
        <th class="px-3 py-2 text-left font-medium">Status</th>
        <th class="px-3 py-2 text-left font-medium">Peer</th>
        <th class="px-3 py-2 text-left font-medium">Verb</th>
        <th class="px-3 py-2 text-left font-medium">Actor</th>
        <th class="px-3 py-2 text-left font-medium">Received</th>
        <th class="px-3 py-2 text-left font-medium">Reject reason</th>
      </tr>
    </thead>
    <tbody>
      {#each rows as row (row.id)}
        <tr class="border-b border-border/40 last:border-0">
          <td class="px-3 py-2">
            <span class="inline-flex items-center rounded border px-2 py-0.5 text-xs font-medium {statusPillClass(row.status)}">{row.status}</span>
          </td>
          <td class="px-3 py-2"><code class="text-xs">{peerLabel(row.peer_id)}</code></td>
          <td class="px-3 py-2"><code class="text-xs">{row.activity_type}</code></td>
          <td class="max-w-xs truncate px-3 py-2 text-fg-muted" title={row.actor_uri}><code class="text-xs">{row.actor_uri}</code></td>
          <td class="px-3 py-2 text-fg-muted" title={row.received_at}>{relTime(row.received_at)}</td>
          <td class="max-w-xs truncate px-3 py-2 text-fg-muted" title={row.reject_reason || row.last_error || ''}>
            {#if row.reject_reason}<code class="text-xs text-danger">{row.reject_reason}</code>{:else}{row.last_error || ''}{/if}
          </td>
        </tr>
      {/each}
      {#if rows.length === 0 && !loading}
        <tr>
          <td colspan="6" class="px-3 py-6 text-center text-fg-muted">No rows match the filter.</td>
        </tr>
      {/if}
    </tbody>
  </table>
  {#if nextCursor}
    <div class="border-t border-border p-3 text-center">
      <button
        onclick={() => loadInbox(true)}
        disabled={loading}
        class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg disabled:opacity-50"
      >{loading ? 'Loading…' : 'Load more'}</button>
    </div>
  {/if}
</section>
