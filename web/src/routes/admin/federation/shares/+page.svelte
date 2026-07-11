<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import RestrictedShareBanner from '$lib/components/federation/RestrictedShareBanner.svelte';
  // Admin federation shares — Phase 1.22.C-e.
  //
  // Lists active federation_shares rows with a mutually-
  // exclusive filter:
  //   - by peer (default — pick a peer, see what we're sharing)
  //   - by grantor (operator audits a user's outbound shares)
  //   - by object (paste object_kind + object_id — "who has
  //     access to this object?")
  //
  // Backed by GET /admin/federation/shares from 1.22.C-c.
  // Revoke action calls DELETE /admin/federation/shares/{id},
  // which emits aa:Unshare + the federation.share.revoked audit
  // row atomically (1.22.C-c write-ahead invariant).
  //
  // The page reuses the existing peer cache via the peers
  // dropdown: a single GET /admin/federation/peers feeds both
  // the picker and the display-name lookup in the table.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface Peer {
    id: string;
    instance_url: string;
    display_name: string;
    enabled: boolean;
    status: string;
  }

  interface Share {
    id: string;
    grantor_user_ref: number;
    object_kind: string;
    object_id: string;
    peer_id: string;
    target_user_url?: string | null;
    scope: string;
    expires_at?: string | null;
    notes: string;
    granted_activity_id: string;
    granted_at: string;
    revoked_at?: string | null;
    revoked_activity_id?: string | null;
    created_at: string;
    updated_at: string;
  }

  type FilterMode = 'by_peer' | 'by_grantor' | 'by_object';

  let mode = $state<FilterMode>('by_peer');
  let peerId = $state('');
  let grantorRef = $state('');
  let objectKind = $state<'asset' | 'post' | 'collection' | 'workspace' | 'brand_kit' | 'user'>('post');
  let objectId = $state('');

  let peers = $state<Peer[]>([]);
  let shares = $state<Share[]>([]);
  let loading = $state(false);
  let error = $state('');
  let revokeBusyId = $state('');

  // peerMap is a memoised id→display lookup so we don't
  // do an O(N*M) inner loop per render. Recomputed only when
  // peers changes, which is the cheapest reactive shape.
  let peerMap = $derived.by(() => {
    const m: Record<string, Peer> = {};
    for (const p of peers) m[p.id] = p;
    return m;
  });

  onMount(async () => {
    await loadPeers();
    // Default load: nothing — the user picks a filter first.
  });

  async function loadPeers(): Promise<void> {
    const r = await api.GET('/admin/federation/peers', {});
    if (!r.error && r.data?.items) peers = r.data.items as Peer[];
  }

  async function loadShares(): Promise<void> {
    error = '';
    loading = true;
    shares = [];
    const params: Record<string, string | number> = { limit: 200 };
    if (mode === 'by_peer') {
      if (!peerId) { error = t('admin.federation.shares.pick_peer'); loading = false; return; }
      params.peer_id = peerId;
    } else if (mode === 'by_grantor') {
      const n = Number.parseInt(grantorRef, 10);
      if (!Number.isFinite(n) || n <= 0) {
        error = t('admin.federation.shares.bad_grantor');
        loading = false;
        return;
      }
      params.grantor_user_ref = n;
    } else if (mode === 'by_object') {
      if (!objectId) { error = t('admin.federation.shares.need_object'); loading = false; return; }
      params.object_kind = objectKind;
      params.object_id = objectId.trim();
    }
    const r = await api.GET('/admin/federation/shares', { params: { query: params } });
    loading = false;
    if (r.error) {
      error = t('admin.federation.shares.load_error');
      return;
    }
    shares = (r.data?.items ?? []) as Share[];
  }

  async function revoke(s: Share): Promise<void> {
    if (!confirm(t('admin.federation.shares.confirm_revoke', { id: s.id.slice(0, 8) }))) return;
    revokeBusyId = s.id;
    const r = await api.DELETE('/admin/federation/shares/{id}', {
      params: { path: { id: s.id } },
    });
    revokeBusyId = '';
    if (r.error) {
      error = t('admin.federation.shares.revoke_error');
      return;
    }
    // Reload the current filter so the just-revoked row drops
    // out (the GET filter is active-only).
    await loadShares();
  }

  function peerLabel(id: string): string {
    const p = peerMap[id];
    if (!p) return id.slice(0, 8) + '…';
    return p.display_name || p.instance_url;
  }

  function scopeLabel(s: string): string {
    const key = `admin.federation.shares.scope_${s}`;
    const lit = t(key);
    return lit === key ? s : lit;
  }

  function kindLabel(k: string): string {
    const key = `admin.federation.cascade_kind_${k}`;
    const lit = t(key);
    return lit === key ? k : lit;
  }

  function expiryLabel(s: Share): string {
    if (!s.expires_at) return t('admin.federation.shares.expiry_none');
    const d = new Date(s.expires_at);
    const now = new Date();
    if (d.getTime() < now.getTime()) return t('admin.federation.shares.expiry_past');
    return d.toLocaleString();
  }
</script>

<svelte:head><title>{t('admin.federation.shares.title')} — artist-alley</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">{t('admin.federation.shares.title')}</h2>
  <p class="text-sm text-fg-muted">{t('admin.federation.shares.intro')}</p>
</header>

<div class="mb-4">
  <RestrictedShareBanner mode="page" />
</div>

<section class="mb-6 rounded border border-border bg-bg-soft p-4">
  <h3 class="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-muted">
    {t('admin.federation.shares.filter_heading')}
  </h3>

  <div class="mb-3 flex flex-wrap gap-3">
    <label class="inline-flex items-center gap-2">
      <input type="radio" bind:group={mode} value="by_peer" />
      {t('admin.federation.shares.filter_by_peer')}
    </label>
    <label class="inline-flex items-center gap-2">
      <input type="radio" bind:group={mode} value="by_grantor" />
      {t('admin.federation.shares.filter_by_grantor')}
    </label>
    <label class="inline-flex items-center gap-2">
      <input type="radio" bind:group={mode} value="by_object" />
      {t('admin.federation.shares.filter_by_object')}
    </label>
  </div>

  {#if mode === 'by_peer'}
    <div class="flex flex-wrap items-end gap-3">
      <label class="flex flex-col text-sm">
        <span class="text-xs text-fg-muted">{t('admin.federation.shares.peer_label')}</span>
        <select bind:value={peerId} class="rounded border border-border bg-bg px-3 py-2">
          <option value="">{t('admin.federation.shares.peer_pick')}</option>
          {#each peers as p (p.id)}
            <option value={p.id}>{p.display_name} — {p.instance_url}</option>
          {/each}
        </select>
      </label>
    </div>
  {:else if mode === 'by_grantor'}
    <label class="flex flex-col text-sm">
      <span class="text-xs text-fg-muted">{t('admin.federation.shares.grantor_label')}</span>
      <input
        type="number"
        bind:value={grantorRef}
        class="w-40 rounded border border-border bg-bg px-3 py-2"
        placeholder="123"
      />
    </label>
  {:else}
    <div class="flex flex-wrap items-end gap-3">
      <label class="flex flex-col text-sm">
        <span class="text-xs text-fg-muted">{t('admin.federation.shares.object_kind_label')}</span>
        <select bind:value={objectKind} class="rounded border border-border bg-bg px-3 py-2">
          <option value="post">post</option>
          <option value="collection">collection</option>
          <option value="asset">asset</option>
          <option value="workspace">workspace</option>
          <option value="brand_kit">brand_kit</option>
          <option value="user">user</option>
        </select>
      </label>
      <label class="flex flex-col text-sm">
        <span class="text-xs text-fg-muted">{t('admin.federation.shares.object_id_label')}</span>
        <input
          type="text"
          bind:value={objectId}
          class="w-96 rounded border border-border bg-bg px-3 py-2 font-mono text-xs"
          placeholder="00000000-0000-0000-0000-000000000000"
        />
      </label>
    </div>
  {/if}

  <div class="mt-3 flex gap-2">
    <button
      type="button"
      class="rounded bg-accent px-4 py-2 text-sm text-white hover:bg-accent-strong disabled:opacity-50"
      disabled={loading}
      onclick={loadShares}
    >
      {loading ? t('admin.federation.shares.loading') : t('admin.federation.shares.load')}
    </button>
  </div>

  {#if error}
    <p class="mt-2 text-sm text-danger">{error}</p>
  {/if}
</section>

{#if shares.length === 0 && !loading && !error}
  <p class="text-sm text-fg-muted">{t('admin.federation.shares.empty')}</p>
{:else if shares.length > 0}
  <div class="overflow-x-auto rounded border border-border">
    <table class="min-w-full text-sm">
      <thead class="bg-bg-soft text-left text-xs uppercase tracking-wide text-fg-muted">
        <tr>
          <th class="px-3 py-2">{t('admin.federation.shares.col_object')}</th>
          <th class="px-3 py-2">{t('admin.federation.shares.col_peer')}</th>
          <th class="px-3 py-2">{t('admin.federation.shares.col_recipient')}</th>
          <th class="px-3 py-2">{t('admin.federation.shares.col_scope')}</th>
          <th class="px-3 py-2">{t('admin.federation.shares.col_granted')}</th>
          <th class="px-3 py-2">{t('admin.federation.shares.col_expires')}</th>
          <th class="px-3 py-2 text-right">{t('admin.federation.shares.col_actions')}</th>
        </tr>
      </thead>
      <tbody>
        {#each shares as s (s.id)}
          <tr class="border-t border-border">
            <td class="px-3 py-2">
              <div class="font-medium">{kindLabel(s.object_kind)}</div>
              <div class="font-mono text-xs text-fg-muted">{s.object_id.slice(0, 8)}…</div>
            </td>
            <td class="px-3 py-2">{peerLabel(s.peer_id)}</td>
            <td class="px-3 py-2">
              {#if s.target_user_url}
                <span class="font-mono text-xs">{s.target_user_url}</span>
              {:else}
                <span class="italic text-fg-muted">{t('admin.federation.shares.recipient_broadcast')}</span>
              {/if}
            </td>
            <td class="px-3 py-2">{scopeLabel(s.scope)}</td>
            <td class="px-3 py-2 text-xs text-fg-muted">{new Date(s.granted_at).toLocaleString()}</td>
            <td class="px-3 py-2 text-xs text-fg-muted">{expiryLabel(s)}</td>
            <td class="px-3 py-2 text-right">
              <button
                type="button"
                class="rounded border border-danger/40 px-2 py-1 text-xs text-danger hover:bg-danger/10 disabled:opacity-50"
                disabled={revokeBusyId === s.id}
                onclick={() => revoke(s)}
              >
                {revokeBusyId === s.id ? t('admin.federation.shares.revoking') : t('admin.federation.shares.revoke')}
              </button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
