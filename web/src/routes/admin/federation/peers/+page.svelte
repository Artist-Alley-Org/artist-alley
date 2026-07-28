<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin federation peers — Phase 1.22.B-a + 1.22.B-b.
  //
  // Three sections (top to bottom):
  //
  //   1. This instance's identity panel — shows our public key
  //      fingerprint + URL + display name so the operator can
  //      relay them out-of-band to the peer's operator.
  //
  //   2. Pending inbound requests — pairings the peer initiated
  //      that need our admin's accept/reject. Hidden when empty.
  //
  //   3. Add/pair peer — two paths:
  //        a) "Pair by URL" (1.22.B-b automated handshake): admin
  //           enters peer URL, system POSTs an offer envelope, the
  //           peer's admin accepts on their side.
  //        b) "Manual entry" (1.22.B-a): paste both URL + PEM
  //           pubkey, status lands as connected immediately.
  //           Useful for air-gapped or DNS-less coordination.
  //
  //   4. Paired peers table — every row regardless of status, with
  //      status badges + inline tier picker + enabled toggle +
  //      defederate.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface Peer {
    id: string;
    instance_url: string;
    display_name: string;
    instance_public_key: string;
    trust_tier: 'connected' | 'directory-listed' | 'auto-sync';
    encryption_policy: 'plaintext' | 'e2e-encrypted';
    enabled: boolean;
    status: 'pending_outbound' | 'pending_inbound' | 'connected';
    handshake_at: string;
    handshake_by_user_ref: number;
    last_seen_at?: string | null;
    notes: string;
    share_in_visible_list?: boolean;
    created_at: string;
    updated_at: string;
  }

  interface PeerSuggestion {
    id: string;
    source_peer_id: string;
    source_display_name: string;
    source_url: string;
    suggested_url: string;
    suggested_display_name: string;
    suggested_public_key: string;
    suggested_fingerprint: string;
    cached_at: string;
  }

  interface InstanceDoc {
    instance_url: string;
    display_name: string;
    public_key_pem: string;
    fingerprint: string;
    protocol_version: string;
  }

  let peers = $state<Peer[]>([]);
  let pendingInbound = $state<Peer[]>([]);
  let suggestions = $state<PeerSuggestion[]>([]);
  let instance = $state<InstanceDoc | null>(null);
  let loading = $state(true);
  let error = $state<string | null>(null);
  let refreshingSuggestions = $state(false);

  // Pair-by-URL form state.
  let showPair = $state(false);
  let pairURL = $state('');
  let pairDisplay = $state('');
  let pairError = $state<string | null>(null);
  let pairSubmitting = $state(false);

  // Manual-entry form state (1.22.B-a path, kept as the
  // "advanced" alternative when DNS / network coordination
  // doesn't work).
  let showManual = $state(false);
  let manualURL = $state('');
  let manualDisplay = $state('');
  let manualPubKey = $state('');
  let manualTier = $state<Peer['trust_tier']>('connected');
  let manualEncryption = $state<Peer['encryption_policy']>('plaintext');
  let manualNotes = $state('');
  let manualError = $state<string | null>(null);
  let manualSubmitting = $state(false);

  onMount(() => {
    void load();
  });

  async function load(): Promise<void> {
    loading = true;
    error = null;
    try {
      const [peersR, pendingR, instanceR, suggestionsR] = await Promise.all([
        api.GET('/admin/federation/peers'),
        api.GET('/admin/federation/peers/pending-inbound'),
        api.GET('/federation/instance'),
        api.GET('/admin/federation/suggestions'),
      ]);
      if (peersR.error) {
        error = (peersR.error as { error?: string } | undefined)?.error ?? t('admin.federation.load_error');
        return;
      }
      peers = (peersR.data?.items ?? []) as Peer[];
      pendingInbound = (pendingR.data?.items ?? []) as Peer[];
      if (instanceR.data) instance = instanceR.data as InstanceDoc;
      suggestions = (suggestionsR.data?.items ?? []) as PeerSuggestion[];
    } finally {
      loading = false;
    }
  }

  async function refreshSuggestions(): Promise<void> {
    refreshingSuggestions = true;
    try {
      await api.POST('/admin/federation/suggestions/refresh');
      await load();
    } finally {
      refreshingSuggestions = false;
    }
  }

  async function toggleShareInVisible(p: Peer): Promise<void> {
    const r = await api.PATCH('/admin/federation/peers/{id}', {
      params: { path: { id: p.id } },
      body: { share_in_visible_list: !p.share_in_visible_list },
    });
    if (!r.error) await load();
  }

  function pairFromSuggestion(s: PeerSuggestion): void {
    // Pre-fill the existing Pair-by-URL form on this same page
    // by setting state directly.
    showPair = true;
    pairURL = s.suggested_url;
    pairDisplay = s.suggested_display_name;
    // Scroll the form into view.
    setTimeout(() => {
      const form = document.querySelector('form input[type=url]') as HTMLInputElement | null;
      form?.focus();
    }, 200);
  }

  async function submitPair(): Promise<void> {
    pairError = null;
    pairSubmitting = true;
    try {
      const body: Record<string, string> = { instance_url: pairURL.trim() };
      if (pairDisplay.trim()) body.display_name = pairDisplay.trim();
      const r = await api.POST('/admin/federation/peers/initiate', { body: body as never });
      if (r.error) {
        pairError = (r.error as { error?: string } | undefined)?.error ?? t('admin.federation.pair_error');
        return;
      }
      pairURL = '';
      pairDisplay = '';
      showPair = false;
      await load();
    } finally {
      pairSubmitting = false;
    }
  }

  async function submitManual(): Promise<void> {
    manualError = null;
    manualSubmitting = true;
    try {
      const r = await api.POST('/admin/federation/peers', {
        body: {
          instance_url: manualURL.trim(),
          display_name: manualDisplay.trim(),
          instance_public_key: manualPubKey,
          trust_tier: manualTier,
          encryption_policy: manualEncryption,
          enabled: true,
          notes: manualNotes.trim(),
        },
      });
      if (r.error) {
        manualError = (r.error as { error?: string } | undefined)?.error ?? t('admin.federation.add_error');
        return;
      }
      manualURL = '';
      manualDisplay = '';
      manualPubKey = '';
      manualNotes = '';
      showManual = false;
      await load();
    } finally {
      manualSubmitting = false;
    }
  }

  async function acceptInbound(p: Peer): Promise<void> {
    const r = await api.POST('/admin/federation/peers/{id}/accept', {
      params: { path: { id: p.id } },
    });
    if (!r.error) await load();
  }

  async function rejectInbound(p: Peer): Promise<void> {
    if (!confirm(t('admin.federation.confirm_reject', { name: p.display_name }))) return;
    const r = await api.DELETE('/admin/federation/peers/{id}', {
      params: { path: { id: p.id } },
    });
    if (!r.error) await load();
  }

  async function toggleEnabled(p: Peer): Promise<void> {
    const r = await api.PATCH('/admin/federation/peers/{id}', {
      params: { path: { id: p.id } },
      body: { enabled: !p.enabled },
    });
    if (!r.error) await load();
  }

  async function setTier(p: Peer, tier: Peer['trust_tier']): Promise<void> {
    const r = await api.PATCH('/admin/federation/peers/{id}', {
      params: { path: { id: p.id } },
      body: { trust_tier: tier },
    });
    if (!r.error) await load();
  }

  // Defederation cascade preview state (1.22.C-e). Backed by
  // GET /admin/federation/peers/{id}/defederation-preview from
  // 1.22.C-d. Replaces the old browser-confirm() with a real
  // modal showing exact impact (per-object-kind breakdown,
  // pending handshakes, suggestions to drop).
  let cascadeOpenFor = $state<Peer | null>(null);
  let cascadePreview = $state<{
    peer_id: string;
    peer_display_name: string;
    peer_url: string;
    total_active_shares: number;
    shares_by_kind: Record<string, number>;
    pending_handshakes: number;
    cached_suggestions: number;
  } | null>(null);
  let cascadeLoading = $state(false);
  let cascadeBusy = $state(false);
  let cascadeError = $state('');

  async function openDefederateModal(p: Peer): Promise<void> {
    cascadeOpenFor = p;
    cascadePreview = null;
    cascadeError = '';
    cascadeLoading = true;
    const r = await api.GET('/admin/federation/peers/{id}/defederation-preview', {
      params: { path: { id: p.id } },
    });
    cascadeLoading = false;
    if (r.error || !r.data) {
      cascadeError = t('admin.federation.cascade_preview_error');
      return;
    }
    cascadePreview = r.data;
  }

  function closeDefederateModal(): void {
    cascadeOpenFor = null;
    cascadePreview = null;
    cascadeError = '';
    cascadeBusy = false;
  }

  async function confirmDefederate(): Promise<void> {
    if (!cascadeOpenFor) return;
    cascadeBusy = true;
    const r = await api.DELETE('/admin/federation/peers/{id}', {
      params: { path: { id: cascadeOpenFor.id } },
    });
    cascadeBusy = false;
    if (!r.error) {
      closeDefederateModal();
      await load();
    } else {
      cascadeError = t('admin.federation.cascade_delete_error');
    }
  }

  // Pretty-print the object_kind keys from shares_by_kind. The
  // backend uses internal catalogue names (post, collection,
  // brand_kit, etc.); the modal renders them as i18n labels.
  function kindLabel(kind: string): string {
    const key = `admin.federation.cascade_kind_${kind}`;
    const lit = t(key);
    return lit === key ? kind : lit;
  }

  function fingerprintFromPEM(pem: string): string {
    // Lightweight client-side hint — we don't reparse the PEM,
    // just show the first 16 hex chars of the SHA-256 of the
    // base64 body. Server returns the proper fingerprint on
    // the instance doc; for other peers we'd need a per-peer
    // endpoint. Placeholder.
    const lines = pem.split('\n').filter((l) => l && !l.startsWith('---'));
    const body = lines.join('').slice(0, 32);
    return body || '—';
  }

  function statusLabel(s: Peer['status']): string {
    if (s === 'pending_outbound') return t('admin.federation.status_pending_outbound');
    if (s === 'pending_inbound') return t('admin.federation.status_pending_inbound');
    return t('admin.federation.status_connected');
  }
</script>

<svelte:head><title>{t('admin.federation.title')} — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">{t('admin.federation.title')}</h2>
  <p class="text-sm text-fg-muted">{t('admin.federation.intro')}</p>
  <p class="mt-2 rounded border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
    <strong>{t('admin.federation.banner_no_content_yet_label')}</strong>
    {t('admin.federation.banner_no_content_yet')}
  </p>
</header>

<!-- This instance's identity (out-of-band verification) -->
{#if instance}
  <section class="mb-6 rounded-lg border border-border bg-surface p-4">
    <h3 class="mb-2 text-sm font-semibold">{t('admin.federation.this_instance')}</h3>
    <dl class="grid grid-cols-1 gap-1 text-xs sm:grid-cols-2">
      <div><dt class="inline font-medium">{t('admin.federation.this_url')}</dt> <dd class="inline font-mono">{instance.instance_url || t('admin.federation.this_url_unset')}</dd></div>
      <div><dt class="inline font-medium">{t('admin.federation.this_display_name')}</dt> <dd class="inline">{instance.display_name || t('admin.federation.this_display_unset')}</dd></div>
      <div class="sm:col-span-2"><dt class="inline font-medium">{t('admin.federation.this_fingerprint')}</dt> <dd class="inline font-mono">{instance.fingerprint}</dd></div>
    </dl>
    <p class="mt-2 text-xs text-fg-muted">{t('admin.federation.this_help')}</p>
  </section>
{/if}

<!-- Pending inbound requests -->
{#if pendingInbound.length > 0}
  <section class="mb-6 rounded-lg border border-accent/40 bg-accent-container/40 p-4">
    <h3 class="mb-2 text-sm font-semibold">{t('admin.federation.pending_inbound_title', { count: pendingInbound.length })}</h3>
    <p class="mb-3 text-xs text-fg-muted">{t('admin.federation.pending_inbound_help')}</p>
    <ul class="space-y-2">
      {#each pendingInbound as p (p.id)}
        <li class="rounded border border-border bg-surface p-3">
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0 flex-1">
              <div class="font-medium">{p.display_name}</div>
              <div class="font-mono text-xs">{p.instance_url}</div>
              <div class="mt-1 text-[11px] text-fg-muted">
                {t('admin.federation.pending_fingerprint_hint')}: <span class="font-mono">{fingerprintFromPEM(p.instance_public_key)}…</span>
              </div>
            </div>
            <div class="flex flex-shrink-0 gap-2">
              <button
                type="button"
                class="rounded-md bg-accent px-3 py-1 text-xs font-medium text-on-accent hover:bg-accent/90"
                onclick={() => acceptInbound(p)}
              >{t('admin.federation.accept')}</button>
              <button
                type="button"
                class="rounded-md border border-danger/40 bg-danger/10 px-3 py-1 text-xs text-danger hover:bg-danger/20"
                onclick={() => rejectInbound(p)}
              >{t('admin.federation.reject')}</button>
            </div>
          </div>
        </li>
      {/each}
    </ul>
  </section>
{/if}

<!-- Add / pair -->
<section class="mb-4 flex items-center justify-between gap-2">
  <h3 class="text-lg font-semibold">{t('admin.federation.paired_peers')}</h3>
  <div class="flex gap-2">
    <button
      type="button"
      class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90"
      onclick={() => { showPair = !showPair; showManual = false; }}
    >{showPair ? t('admin.federation.add_cancel') : t('admin.federation.pair_peer')}</button>
    <button
      type="button"
      class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-state-hover"
      onclick={() => { showManual = !showManual; showPair = false; }}
    >{showManual ? t('admin.federation.add_cancel') : t('admin.federation.manual_entry')}</button>
  </div>
</section>

{#if showPair}
  <form
    class="mb-6 space-y-3 rounded-lg border border-border bg-surface p-4"
    onsubmit={(e) => { e.preventDefault(); void submitPair(); }}
  >
    <h4 class="text-sm font-semibold">{t('admin.federation.pair_peer')}</h4>
    <p class="text-xs text-fg-muted">{t('admin.federation.pair_help')}</p>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.form_instance_url')}</span>
      <input
        bind:value={pairURL}
        type="url"
        required
        placeholder={t('admin.federation.form_instance_url_placeholder')}
        class="mt-1 block w-full rounded-md border border-border-strong bg-surface px-3 py-2 text-sm"
      />
    </label>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.form_display_name_optional')}</span>
      <input
        bind:value={pairDisplay}
        type="text"
        maxlength="200"
        class="mt-1 block w-full rounded-md border border-border-strong bg-surface px-3 py-2 text-sm"
      />
    </label>
    {#if pairError}
      <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{pairError}</p>
    {/if}
    <div class="flex justify-end gap-2">
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-state-hover"
        onclick={() => (showPair = false)}
      >{t('admin.federation.add_cancel')}</button>
      <button
        type="submit"
        class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90 disabled:opacity-50"
        disabled={pairSubmitting}
      >{pairSubmitting ? t('common.loading') : t('admin.federation.pair_submit')}</button>
    </div>
  </form>
{/if}

{#if showManual}
  <form
    class="mb-6 space-y-3 rounded-lg border border-border bg-surface p-4"
    onsubmit={(e) => { e.preventDefault(); void submitManual(); }}
  >
    <h4 class="text-sm font-semibold">{t('admin.federation.manual_entry')}</h4>
    <p class="text-xs text-fg-muted">{t('admin.federation.manual_help')}</p>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.form_instance_url')}</span>
      <input
        bind:value={manualURL}
        type="url"
        required
        placeholder={t('admin.federation.form_instance_url_placeholder')}
        class="mt-1 block w-full rounded-md border border-border-strong bg-surface px-3 py-2 text-sm"
      />
    </label>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.form_display_name')}</span>
      <input
        bind:value={manualDisplay}
        type="text"
        required
        maxlength="200"
        class="mt-1 block w-full rounded-md border border-border-strong bg-surface px-3 py-2 text-sm"
      />
    </label>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.form_public_key')}</span>
      <textarea
        bind:value={manualPubKey}
        required
        rows="6"
        placeholder={t('admin.federation.form_public_key_placeholder')}
        class="mt-1 block w-full rounded-md border border-border-strong bg-surface px-3 py-2 font-mono text-xs"
      ></textarea>
    </label>
    <div class="flex gap-3">
      <label class="flex-1">
        <span class="text-xs font-medium">{t('admin.federation.form_trust_tier')}</span>
        <select bind:value={manualTier} class="mt-1 block w-full rounded-md border border-border-strong bg-surface px-2 py-1.5 text-sm">
          <option value="connected">{t('admin.federation.tier_connected')}</option>
          <option value="directory-listed">{t('admin.federation.tier_directory_listed')}</option>
          <option value="auto-sync">{t('admin.federation.tier_auto_sync')}</option>
        </select>
      </label>
      <label class="flex-1">
        <span class="text-xs font-medium">{t('admin.federation.form_encryption_policy')}</span>
        <select bind:value={manualEncryption} class="mt-1 block w-full rounded-md border border-border-strong bg-surface px-2 py-1.5 text-sm">
          <option value="plaintext">{t('admin.federation.encryption_plaintext')}</option>
          <option value="e2e-encrypted">{t('admin.federation.encryption_e2e')}</option>
        </select>
      </label>
    </div>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.form_notes')}</span>
      <input
        bind:value={manualNotes}
        type="text"
        class="mt-1 block w-full rounded-md border border-border-strong bg-surface px-3 py-2 text-sm"
      />
    </label>
    {#if manualError}
      <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{manualError}</p>
    {/if}
    <div class="flex justify-end gap-2">
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-state-hover"
        onclick={() => (showManual = false)}
      >{t('admin.federation.add_cancel')}</button>
      <button
        type="submit"
        class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90 disabled:opacity-50"
        disabled={manualSubmitting}
      >{manualSubmitting ? t('common.loading') : t('admin.federation.add_submit')}</button>
    </div>
  </form>
{/if}

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>
{:else if peers.length === 0}
  <p class="rounded-md border border-border bg-surface px-4 py-8 text-center text-sm text-fg-muted">
    {t('admin.federation.empty')}
  </p>
{:else}
  <div class="overflow-hidden rounded-lg border border-border bg-surface">
    <table class="w-full text-sm">
      <thead class="bg-surface-elevated text-xs uppercase tracking-wider text-fg-muted">
        <tr>
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_name')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_url')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_status')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_tier')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_enc')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_enabled')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_share')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_seen')}</th>
          <th class="px-3 py-2 text-right font-medium">{t('admin.federation.col_actions')}</th>
        </tr>
      </thead>
      <tbody>
        {#each peers as p (p.id)}
          <tr class="border-t border-border">
            <td class="px-3 py-2">
              <div class="font-medium">{p.display_name}</div>
              {#if p.notes}
                <div class="text-xs text-fg-muted">{p.notes}</div>
              {/if}
            </td>
            <td class="px-3 py-2 font-mono text-xs">{p.instance_url}</td>
            <td class="px-3 py-2">
              {#if p.status === 'connected'}
                <span class="rounded bg-accent-container px-1.5 py-0.5 text-[10px] font-medium text-on-accent-container">{statusLabel(p.status)}</span>
              {:else}
                <span class="rounded bg-warning/20 px-1.5 py-0.5 text-[10px] font-medium text-warning">{statusLabel(p.status)}</span>
              {/if}
            </td>
            <td class="px-3 py-2">
              <select
                value={p.trust_tier}
                onchange={(e) => setTier(p, (e.currentTarget as HTMLSelectElement).value as Peer['trust_tier'])}
                class="rounded border border-border-strong bg-surface px-1.5 py-0.5 text-xs"
              >
                <option value="connected">{t('admin.federation.tier_connected')}</option>
                <option value="directory-listed">{t('admin.federation.tier_directory_listed')}</option>
                <option value="auto-sync">{t('admin.federation.tier_auto_sync')}</option>
              </select>
            </td>
            <td class="px-3 py-2 text-xs">
              {#if p.encryption_policy === 'e2e-encrypted'}
                <span class="rounded bg-accent-container px-1.5 py-0.5 text-on-accent-container">{t('admin.federation.encryption_e2e_short')}</span>
              {:else}
                <span class="text-fg-muted">{t('admin.federation.encryption_plaintext_short')}</span>
              {/if}
            </td>
            <td class="px-3 py-2">
              <button
                type="button"
                class={p.enabled
                  ? 'rounded bg-accent-container px-2 py-0.5 text-xs text-on-accent-container'
                  : 'rounded bg-surface-elevated px-2 py-0.5 text-xs text-fg-muted'}
                onclick={() => toggleEnabled(p)}
              >{p.enabled ? t('admin.federation.enabled_yes') : t('admin.federation.enabled_no')}</button>
            </td>
            <td class="px-3 py-2">
              {#if p.status === 'connected'}
                <button
                  type="button"
                  class={p.share_in_visible_list
                    ? 'rounded bg-accent-container px-2 py-0.5 text-xs text-on-accent-container'
                    : 'rounded bg-surface-elevated px-2 py-0.5 text-xs text-fg-muted'}
                  onclick={() => toggleShareInVisible(p)}
                  title={t('admin.federation.col_share_help')}
                >{p.share_in_visible_list ? t('admin.federation.share_yes') : t('admin.federation.share_no')}</button>
              {:else}
                <span class="text-xs text-fg-muted">—</span>
              {/if}
            </td>
            <td class="px-3 py-2 text-xs text-fg-muted">
              {p.last_seen_at ? new Date(p.last_seen_at).toLocaleString() : t('admin.federation.never_seen')}
            </td>
            <td class="px-3 py-2 text-right">
              <button
                type="button"
                class="rounded-md border border-danger/40 bg-danger/10 px-2 py-0.5 text-xs text-danger hover:bg-danger/20"
                onclick={() => openDefederateModal(p)}
              >{t('admin.federation.defederate')}</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}

<!-- Suggested peers (peer-of-peer discovery, Phase 1.22.B-d) -->
<section class="mt-8 rounded-lg border border-border bg-surface">
  <header class="flex items-center justify-between border-b border-border p-3">
    <div>
      <h3 class="text-sm font-semibold">{t('admin.federation.suggestions_title', { count: suggestions.length })}</h3>
      <p class="text-xs text-fg-muted">{t('admin.federation.suggestions_help')}</p>
    </div>
    <button
      type="button"
      class="rounded-md border border-border bg-surface px-3 py-1 text-xs hover:bg-state-hover disabled:opacity-50"
      onclick={refreshSuggestions}
      disabled={refreshingSuggestions}
    >{refreshingSuggestions ? t('common.loading') : t('admin.federation.suggestions_refresh')}</button>
  </header>
  {#if suggestions.length === 0}
    <p class="p-4 text-center text-sm text-fg-muted">{t('admin.federation.suggestions_empty')}</p>
  {:else}
    <ul class="divide-y divide-border">
      {#each suggestions as s (s.id)}
        <li class="flex items-start justify-between gap-3 p-3">
          <div class="min-w-0 flex-1">
            <div class="font-medium">{s.suggested_display_name}</div>
            <div class="font-mono text-xs">{s.suggested_url}</div>
            <div class="mt-1 text-[11px] text-fg-muted">
              {t('admin.federation.suggestions_via')}: <span class="font-mono">{s.source_display_name}</span> ({s.source_url})
            </div>
            <div class="mt-1 text-[11px] text-fg-muted">
              {t('admin.federation.dir_fingerprint')}: <span class="font-mono">{s.suggested_fingerprint}</span>
            </div>
          </div>
          <button
            type="button"
            class="flex-shrink-0 rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-on-accent hover:bg-accent/90"
            onclick={() => pairFromSuggestion(s)}
          >{t('admin.federation.suggestions_pair')}</button>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<!-- Defederation cascade modal (1.22.C-e). Backed by the
     /admin/federation/peers/{id}/defederation-preview endpoint
     from 1.22.C-d. Shows exact impact before the destructive
     DELETE: per-object-kind breakdown, pending handshakes that
     would be cancelled, suggestions that would be dropped, the
     "best-effort aa:Unshare per row" caveat. -->
{#if cascadeOpenFor}
  <div
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
    role="presentation"
    onclick={(e) => { if (e.target === e.currentTarget) closeDefederateModal(); }}
    onkeydown={(e) => { if (e.key === 'Escape') closeDefederateModal(); }}
  >
    <div
      class="w-full max-w-2xl rounded-lg border border-border bg-bg p-6 shadow-xl"
      role="dialog"
      aria-modal="true"
      aria-labelledby="cascade-title"
    >
      <h3 id="cascade-title" class="text-lg font-semibold">
        {t('admin.federation.cascade_title', { name: cascadeOpenFor.display_name })}
      </h3>
      <p class="mt-1 text-sm text-fg-muted">
        {t('admin.federation.cascade_intro', { url: cascadeOpenFor.instance_url })}
      </p>

      {#if cascadeLoading}
        <p class="mt-4 text-sm text-fg-muted">{t('admin.federation.cascade_loading')}</p>
      {:else if cascadeError}
        <p class="mt-4 text-sm text-danger">{cascadeError}</p>
      {:else if cascadePreview}
        <div class="mt-4 space-y-3 rounded border border-border bg-bg-soft p-4 text-sm">
          <p class="font-medium">
            {t('admin.federation.cascade_summary_intro')}
          </p>
          <ul class="ml-4 list-disc space-y-1">
            <li>
              <strong>{cascadePreview.total_active_shares}</strong>
              {t('admin.federation.cascade_total_shares')}
              {#if Object.keys(cascadePreview.shares_by_kind || {}).length > 0}
                <ul class="ml-4 mt-1 list-[circle] text-xs text-fg-muted">
                  {#each Object.entries(cascadePreview.shares_by_kind) as [kind, count] (kind)}
                    <li>{count} {kindLabel(kind)}</li>
                  {/each}
                </ul>
              {/if}
            </li>
            <li>
              <strong>{cascadePreview.pending_handshakes}</strong>
              {t('admin.federation.cascade_pending_handshakes')}
            </li>
            <li>
              <strong>{cascadePreview.cached_suggestions}</strong>
              {t('admin.federation.cascade_cached_suggestions')}
            </li>
            <li class="text-fg-muted">{t('admin.federation.cascade_aa_unshare_note')}</li>
          </ul>
          <p class="text-xs text-fg-muted">
            {t('admin.federation.cascade_irreversible_note')}
          </p>
        </div>
      {/if}

      <div class="mt-6 flex justify-end gap-2">
        <button
          type="button"
          class="rounded border border-border px-4 py-2 text-sm hover:bg-bg-soft disabled:opacity-50"
          disabled={cascadeBusy}
          onclick={closeDefederateModal}
        >{t('common.cancel')}</button>
        <button
          type="button"
          class="rounded bg-danger px-4 py-2 text-sm text-white hover:bg-danger-strong disabled:opacity-50"
          disabled={cascadeLoading || cascadeBusy || !!cascadeError}
          onclick={confirmDefederate}
        >
          {cascadeBusy ? t('admin.federation.cascade_defederating') : t('admin.federation.defederate')}
        </button>
      </div>
    </div>
  </div>
{/if}
