<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin federation outbox — Phase 1.22.D-c.
  //
  // Per-recipient queue view across federation_outbox. Filterable
  // by peer + status + activity type + date range; paginated via
  // opaque cursor. The re-queue button on failed rows flips
  // status=queued + attempts=0; the delivery worker picks up
  // within ms via LISTEN/NOTIFY (per spec §3.5 sub-1s contract).
  //
  // Re-queue confirmation: rows whose last_error references a
  // non-retriable §12.1 reason (unknown_object, sig_invalid,
  // encryption_required, envelope_sig_missing) get a stronger
  // warning prompt — those will fail again, so the operator
  // should know before clicking.
  //
  // The cascade-cancel hook (per-peer disable + cancel queued)
  // lives on /admin/federation/peers; this page just surfaces
  // the queue state.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';

  type OutboxRow = {
    id: string;
    activity_id: string;
    peer_id: string;
    target_user_url?: string | null;
    status: 'queued' | 'sent' | 'failed' | 'cancelled';
    attempts: number;
    next_attempt_at: string;
    last_attempt_at?: string | null;
    last_error?: string | null;
    sent_at?: string | null;
    delivered_with_key_id?: string | null;
    created_at: string;
    updated_at: string;
  };
  type Peer = { id: string; instance_url: string; display_name?: string };

  // Non-retriable §12.1 reject reasons. The re-queue confirmation
  // prompts harder when last_error contains any of these — the
  // delivery will fail again unless the underlying state changes
  // on the receiver side.
  const NON_RETRIABLE_REASONS = [
    'unknown_object',
    'sig_invalid',
    'envelope_sig_missing',
    'unsupported_algorithm',
    'invalid_context',
    'invalid_type',
    'encryption_required',
    'encryption_not_supported',
    'unshared_object',
    'unknown_peer',
    'peer_disabled',
  ];

  let peers = $state<Peer[]>([]);
  let peerMap = $derived.by(() => {
    const m: Record<string, Peer> = {};
    for (const p of peers) m[p.id] = p;
    return m;
  });

  let peerFilter = $state('');
  let statusFilter = $state<'' | OutboxRow['status']>('');
  let activityTypeFilter = $state('');
  let rows = $state<OutboxRow[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(false);
  let error = $state('');

  async function loadPeers() {
    const r = await api.GET('/admin/federation/peers', {});
    if (r.data) peers = r.data.items || [];
  }

  async function loadOutbox(append = false) {
    loading = true;
    error = '';
    try {
      const params: Record<string, string | number> = { limit: 100 };
      if (peerFilter) params.peer_id = peerFilter;
      if (statusFilter) params.status = statusFilter;
      if (activityTypeFilter) params.activity_type = activityTypeFilter;
      if (append && nextCursor) params.cursor = nextCursor;
      const r = await api.GET('/admin/federation/outbox', { params: { query: params } });
      if (r.error) {
        error = (r.error as { error?: string }).error || 'load failed';
        return;
      }
      const data = r.data as { items: OutboxRow[]; next_cursor?: string | null };
      rows = append ? [...rows, ...data.items] : data.items;
      nextCursor = data.next_cursor || null;
    } finally {
      loading = false;
    }
  }

  async function requeue(row: OutboxRow) {
    const lastErr = row.last_error || '';
    const nonRetriable = NON_RETRIABLE_REASONS.some(r => lastErr.includes(r));
    const msg = nonRetriable
      ? `WARNING: this row's last_error references a non-retriable reason ("${lastErr.slice(0, 80)}"). The delivery will fail again unless the receiver-side state has changed. Re-queue anyway?`
      : `Re-queue this row for delivery? The delivery worker picks up within seconds.`;
    if (!confirm(msg)) return;

    const r = await api.POST('/admin/federation/outbox/{id}/requeue', {
      params: { path: { id: row.id } },
    });
    if (r.error) {
      const err = (r.error as { error?: string }).error || 'requeue failed';
      alert(err);
      return;
    }
    // Replace the row in-place with the updated state.
    const updated = r.data as OutboxRow;
    rows = rows.map(x => (x.id === row.id ? updated : x));
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

  function statusPillClass(s: OutboxRow['status']): string {
    switch (s) {
      case 'queued':    return 'bg-info/15 text-info border-info/40';
      case 'sent':      return 'bg-success/15 text-success border-success/40';
      case 'failed':    return 'bg-danger/15 text-danger border-danger/40';
      case 'cancelled': return 'bg-fg-muted/15 text-fg-muted border-fg-muted/40';
    }
  }

  onMount(async () => {
    await loadPeers();
    await loadOutbox();
  });
</script>

<svelte:head><title>Federation outbox — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Federation outbox</h2>
  <p class="text-sm text-fg-muted">
    Per-recipient delivery queue. Rows materialise from the activities ledger
    via LISTEN/NOTIFY (sub-100ms) + the delivery worker POSTs to the peer's
    <code>/federation/inbox</code> with HTTP-Signature signed by the
    instance Ed25519 key.
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
        <option value="queued">queued</option>
        <option value="sent">sent</option>
        <option value="failed">failed</option>
        <option value="cancelled">cancelled</option>
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
      onclick={() => loadOutbox(false)}
      disabled={loading}
      class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
    >{loading ? 'Loading…' : 'Apply'}</button>
    <button
      onclick={() => { peerFilter = ''; statusFilter = ''; activityTypeFilter = ''; loadOutbox(false); }}
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
        <th class="px-3 py-2 text-left font-medium">Activity</th>
        <th class="px-3 py-2 text-left font-medium">Attempts</th>
        <th class="px-3 py-2 text-left font-medium">Created</th>
        <th class="px-3 py-2 text-left font-medium">Last error</th>
        <th class="px-3 py-2 text-right font-medium">Actions</th>
      </tr>
    </thead>
    <tbody>
      {#each rows as row (row.id)}
        <tr class="border-b border-border/40 last:border-0">
          <td class="px-3 py-2">
            <span class="inline-flex items-center rounded border px-2 py-0.5 text-xs font-medium {statusPillClass(row.status)}">{row.status}</span>
          </td>
          <td class="px-3 py-2"><code class="text-xs">{peerLabel(row.peer_id)}</code></td>
          <td class="px-3 py-2"><code class="text-xs">{row.activity_id.slice(0, 8)}…</code></td>
          <td class="px-3 py-2">{row.attempts}</td>
          <td class="px-3 py-2 text-fg-muted" title={row.created_at}>{relTime(row.created_at)}</td>
          <td class="max-w-md truncate px-3 py-2 text-fg-muted" title={row.last_error || ''}>{row.last_error || ''}</td>
          <td class="px-3 py-2 text-right">
            {#if row.status === 'failed'}
              <button
                onclick={() => requeue(row)}
                class="rounded border border-accent px-2 py-1 text-xs text-accent hover:bg-accent hover:text-accent-fg"
              >Re-queue</button>
            {/if}
          </td>
        </tr>
      {/each}
      {#if rows.length === 0 && !loading}
        <tr>
          <td colspan="7" class="px-3 py-6 text-center text-fg-muted">No rows match the filter.</td>
        </tr>
      {/if}
    </tbody>
  </table>
  {#if nextCursor}
    <div class="border-t border-border p-3 text-center">
      <button
        onclick={() => loadOutbox(true)}
        disabled={loading}
        class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg disabled:opacity-50"
      >{loading ? 'Loading…' : 'Load more'}</button>
    </div>
  {/if}
</section>
