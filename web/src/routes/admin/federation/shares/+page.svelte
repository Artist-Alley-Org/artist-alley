<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import RestrictedShareBanner from '$lib/components/federation/RestrictedShareBanner.svelte';
  import { site } from '$stores/site.svelte';
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
  //
  // #1573: the grant form (POST /admin/federation/shares). Before a
  // share is made it reads GET /admin/federation/shares/grant-context
  // for the object: the asset's tier, its local copyright and credit
  // fields (free text, context only), and whether the
  // original-file permission can be set at all. The toggle starts off
  // and stays disabled until that answer says it may be set; the
  // server refuses a disallowed one regardless.

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
    allow_original: boolean;
  }

  interface GrantContext {
    object_kind: string;
    object_id: string;
    title?: string | null;
    sensitivity?: string | null;
    original_allowed: boolean;
    original_refusal?: string | null;
    copyright_notice?: string | null;
    credit?: string | null;
  }

  type GrantKind = 'asset' | 'collection' | 'post';
  type GrantScope = 'view' | 'comment' | 'annotate' | 'remix';

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

  // Grant form state (#1573).
  let grantKind = $state<GrantKind>('asset');
  let grantObjectId = $state('');
  let grantPeerId = $state('');
  let grantScope = $state<GrantScope>('view');
  let grantRecipient = $state('');
  let grantExpires = $state('');
  let grantAllowOriginal = $state(false);
  let grantContext = $state<GrantContext | null>(null);
  let grantLookupBusy = $state(false);
  let grantBusy = $state(false);
  let grantError = $state('');
  let grantNotice = $state('');

  // Only an enabled, connected peer can receive a share (the server
  // refuses any other), so the form offers no other.
  let grantablePeers = $derived(peers.filter((p) => p.enabled && p.status === 'connected'));

  // A context answers for exactly one (kind, id). Every lookup takes a
  // new generation number, and any edit of kind or id bumps it too, so a
  // response is applied only if it is the newest lookup AND still names
  // the object the form names now. A stale success or error is dropped:
  // an answer for one object can never enable the toggle for another.
  let lookupGeneration = 0;

  function sameObject(c: GrantContext | null, kind: string, id: string): boolean {
    return c != null && c.object_kind === kind && c.object_id.toLowerCase() === id.trim().toLowerCase();
  }

  // The context the form may act on: the stored one, only while it still
  // names the object in the form.
  let currentContext = $derived(sameObject(grantContext, grantKind, grantObjectId) ? grantContext : null);

  function resetGrantContext(): void {
    lookupGeneration += 1;
    grantContext = null;
    grantAllowOriginal = false;
    grantLookupBusy = false;
    grantNotice = '';
    grantError = '';
  }

  async function lookupGrantContext(): Promise<void> {
    grantError = '';
    grantNotice = '';
    const kind = grantKind;
    const id = grantObjectId.trim();
    if (!id) {
      grantError = t('admin.federation.shares.need_object');
      return;
    }
    const generation = ++lookupGeneration;
    grantContext = null;
    grantAllowOriginal = false;
    grantLookupBusy = true;
    const r = await api.GET('/admin/federation/shares/grant-context', {
      params: { query: { object_kind: kind, object_id: id } },
    });
    // Stale: a newer lookup started, or the operator edited the object.
    if (generation !== lookupGeneration || kind !== grantKind || id !== grantObjectId.trim()) return;
    grantLookupBusy = false;
    if (r.error) {
      grantError = t('admin.federation.shares.grant_lookup_error', {
        error: (r.error as { error?: string }).error ?? String(r.response.status),
      });
      return;
    }
    const c = r.data as GrantContext;
    // The answer must be about the object that was asked for.
    if (!sameObject(c, kind, id)) return;
    grantContext = c;
  }

  async function submitGrant(e: SubmitEvent): Promise<void> {
    e.preventDefault();
    grantError = '';
    grantNotice = '';
    const id = grantObjectId.trim();
    if (!id) {
      grantError = t('admin.federation.shares.need_object');
      return;
    }
    if (!grantPeerId) {
      grantError = t('admin.federation.shares.pick_peer');
      return;
    }
    grantBusy = true;
    const body = {
      object_kind: grantKind,
      object_id: id,
      peer_id: grantPeerId,
      scope: grantScope,
      // True only when the box is ticked AND the current context, for
      // exactly this kind and id, allowed it. Otherwise false; the server
      // refuses a disallowed one regardless.
      allow_original:
        grantAllowOriginal &&
        sameObject(grantContext, grantKind, id) &&
        grantContext?.original_allowed === true,
      ...(grantRecipient.trim() ? { target_user_url: grantRecipient.trim() } : {}),
      ...(grantExpires ? { expires_at: new Date(grantExpires).toISOString() } : {}),
    };
    const r = await api.POST('/admin/federation/shares', { body });
    grantBusy = false;
    if (r.error) {
      if (r.response.status === 409) {
        grantError = t('admin.federation.shares.grant_conflict');
      } else {
        grantError = t('admin.federation.shares.grant_error', {
          error: (r.error as { error?: string }).error ?? String(r.response.status),
        });
      }
      return;
    }
    grantNotice = t('admin.federation.shares.grant_success');
    // Show the stored share, including the stored original choice.
    mode = 'by_object';
    objectKind = grantKind;
    objectId = id;
    await loadShares();
  }

  function tierLabel(tier: string | null | undefined): string {
    if (!tier) return '';
    const key = `admin.federation.shares.tier_${tier}`;
    const lit = t(key);
    return lit === key ? tier : lit;
  }

  function originalRefusalText(c: GrantContext): string {
    if (c.original_refusal === 'sensitivity_refused') {
      return t('admin.federation.shares.grant_original_refused_sensitivity', { tier: tierLabel(c.sensitivity) });
    }
    return t('admin.federation.shares.grant_original_refused_kind');
  }

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

<svelte:head><title>{t('admin.federation.shares.title')} — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">{t('admin.federation.shares.title')}</h2>
  <p class="text-sm text-fg-muted">{t('admin.federation.shares.intro')}</p>
</header>

<div class="mb-4">
  <RestrictedShareBanner mode="page" />
</div>

<section class="mb-6 rounded border border-border bg-bg-soft p-4" aria-labelledby="share-grant-heading">
  <h3 id="share-grant-heading" class="mb-1 text-sm font-semibold uppercase tracking-wide text-fg-muted">
    {t('admin.federation.shares.grant_heading')}
  </h3>
  <p class="mb-3 text-sm text-fg-muted">{t('admin.federation.shares.grant_intro')}</p>

  <form class="flex flex-col gap-3" onsubmit={submitGrant} data-testid="share-grant-form">
    <div class="flex flex-wrap items-end gap-3">
      <label class="flex flex-col text-sm">
        <span class="text-xs text-fg-muted">{t('admin.federation.shares.grant_kind_label')}</span>
        <select
          bind:value={grantKind}
          onchange={resetGrantContext}
          class="rounded border border-border-strong bg-bg px-3 py-2"
          name="grant_kind"
        >
          <option value="asset">{t('admin.federation.shares.grant_kind_asset')}</option>
          <option value="collection">{t('admin.federation.shares.grant_kind_collection')}</option>
          <option value="post">{t('admin.federation.shares.grant_kind_post')}</option>
        </select>
      </label>
      <label class="flex flex-col text-sm">
        <span class="text-xs text-fg-muted">{t('admin.federation.shares.grant_object_label')}</span>
        <input
          type="text"
          bind:value={grantObjectId}
          oninput={resetGrantContext}
          class="w-96 max-w-full rounded border border-border-strong bg-bg px-3 py-2 font-mono text-xs"
          placeholder="00000000-0000-0000-0000-000000000000"
          name="grant_object_id"
          autocomplete="off"
        />
      </label>
      <button
        type="button"
        class="rounded border border-border-strong px-3 py-2 text-sm hover:bg-bg disabled:opacity-50"
        disabled={grantLookupBusy}
        onclick={lookupGrantContext}
      >
        {grantLookupBusy ? t('admin.federation.shares.grant_looking_up') : t('admin.federation.shares.grant_lookup')}
      </button>
    </div>

    {#if currentContext}
      <div class="rounded border border-border bg-bg p-3 text-sm" data-testid="share-grant-context" aria-live="polite">
        <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
          {#if currentContext.title != null}
            <dt class="text-fg-muted">{t('admin.federation.shares.grant_title_label')}</dt>
            <dd>{currentContext.title}</dd>
          {/if}
          {#if currentContext.sensitivity}
            <dt class="text-fg-muted">{t('admin.federation.shares.grant_tier_label')}</dt>
            <dd>{tierLabel(currentContext.sensitivity)}</dd>
          {/if}
          {#if currentContext.object_kind === 'asset'}
            {#if currentContext.copyright_notice || currentContext.credit}
              {#if currentContext.copyright_notice}
                <dt class="text-fg-muted">{t('admin.federation.shares.grant_copyright_label')}</dt>
                <dd>{currentContext.copyright_notice}</dd>
              {/if}
              {#if currentContext.credit}
                <dt class="text-fg-muted">{t('admin.federation.shares.grant_credit_label')}</dt>
                <dd>{currentContext.credit}</dd>
              {/if}
            {:else}
              <dt class="sr-only">{t('admin.federation.shares.grant_copyright_label')}</dt>
              <dd class="col-span-2 text-fg-muted">{t('admin.federation.shares.grant_copyright_none')}</dd>
            {/if}
          {/if}
        </dl>
        {#if currentContext.object_kind === 'asset'}
          <p class="mt-2 text-xs text-fg-muted">{t('admin.federation.shares.grant_copyright_note')}</p>
        {/if}
      </div>
    {/if}

    <div class="flex flex-wrap items-end gap-3">
      <label class="flex flex-col text-sm">
        <span class="text-xs text-fg-muted">{t('admin.federation.shares.grant_peer_label')}</span>
        <select bind:value={grantPeerId} class="rounded border border-border-strong bg-bg px-3 py-2" name="grant_peer">
          <option value="">{t('admin.federation.shares.grant_peer_pick')}</option>
          {#each grantablePeers as p (p.id)}
            <option value={p.id}>{p.display_name} ({p.instance_url})</option>
          {/each}
        </select>
      </label>
      <label class="flex flex-col text-sm">
        <span class="text-xs text-fg-muted">{t('admin.federation.shares.grant_scope_label')}</span>
        <select bind:value={grantScope} class="rounded border border-border-strong bg-bg px-3 py-2" name="grant_scope">
          <option value="view">{scopeLabel('view')}</option>
          <option value="comment">{scopeLabel('comment')}</option>
          <option value="annotate">{scopeLabel('annotate')}</option>
          <option value="remix">{scopeLabel('remix')}</option>
        </select>
      </label>
      <label class="flex flex-col text-sm">
        <span class="text-xs text-fg-muted">{t('admin.federation.shares.grant_expires_label')}</span>
        <input
          type="datetime-local"
          bind:value={grantExpires}
          class="rounded border border-border-strong bg-bg px-3 py-2"
          name="grant_expires"
        />
      </label>
    </div>
    {#if grantablePeers.length === 0}
      <p class="text-xs text-fg-muted">{t('admin.federation.shares.grant_peer_none')}</p>
    {/if}

    <label class="flex flex-col text-sm">
      <span class="text-xs text-fg-muted">{t('admin.federation.shares.grant_recipient_label')}</span>
      <input
        type="url"
        bind:value={grantRecipient}
        class="w-full max-w-xl rounded border border-border-strong bg-bg px-3 py-2 font-mono text-xs"
        placeholder="https://peer.example/users/name"
        name="grant_recipient"
        aria-describedby="share-grant-recipient-hint"
        autocomplete="off"
      />
      <span id="share-grant-recipient-hint" class="mt-1 text-xs text-fg-muted">
        {t('admin.federation.shares.grant_recipient_hint')}
      </span>
    </label>

    <div class="rounded border border-border p-3">
      <label class="inline-flex items-start gap-2 text-sm">
        <input
          type="checkbox"
          bind:checked={grantAllowOriginal}
          disabled={!currentContext?.original_allowed}
          aria-describedby="share-grant-original-help"
          class="mt-1"
          name="grant_allow_original"
        />
        <span class="font-medium">{t('admin.federation.shares.grant_original_label')}</span>
      </label>
      <div id="share-grant-original-help" class="mt-1 flex flex-col gap-1 text-xs text-fg-muted">
        <span>{t('admin.federation.shares.grant_original_hint')}</span>
        {#if !currentContext}
          <span>{t('admin.federation.shares.grant_original_needs_lookup')}</span>
        {:else if !currentContext.original_allowed}
          <span class="text-danger" data-testid="share-grant-original-refusal">{originalRefusalText(currentContext)}</span>
        {:else if currentContext.object_kind === 'collection'}
          <span>{t('admin.federation.shares.grant_original_collection_note')}</span>
        {/if}
        <span>{t('admin.federation.shares.grant_original_not_live')}</span>
      </div>
    </div>

    <p class="text-xs text-fg-muted">{t('admin.federation.shares.grant_no_recall')}</p>

    <div class="flex flex-wrap items-center gap-3">
      <button
        type="submit"
        class="rounded bg-accent px-4 py-2 text-sm text-on-accent hover:bg-accent-strong disabled:opacity-50"
        disabled={grantBusy}
      >
        {grantBusy ? t('admin.federation.shares.grant_submitting') : t('admin.federation.shares.grant_submit')}
      </button>
      {#if grantNotice}
        <p class="text-sm text-success" role="status">{grantNotice}</p>
      {/if}
    </div>
    {#if grantError}
      <p class="text-sm text-danger" role="alert">{grantError}</p>
    {/if}
  </form>
</section>

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
        <select bind:value={peerId} class="rounded border border-border-strong bg-bg px-3 py-2">
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
        class="w-40 rounded border border-border-strong bg-bg px-3 py-2"
        placeholder="123"
      />
    </label>
  {:else}
    <div class="flex flex-wrap items-end gap-3">
      <label class="flex flex-col text-sm">
        <span class="text-xs text-fg-muted">{t('admin.federation.shares.object_kind_label')}</span>
        <select bind:value={objectKind} class="rounded border border-border-strong bg-bg px-3 py-2">
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
          class="w-96 rounded border border-border-strong bg-bg px-3 py-2 font-mono text-xs"
          placeholder="00000000-0000-0000-0000-000000000000"
        />
      </label>
    </div>
  {/if}

  <div class="mt-3 flex gap-2">
    <button
      type="button"
      class="rounded bg-accent px-4 py-2 text-sm text-on-accent hover:bg-accent-strong disabled:opacity-50"
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
          <th class="px-3 py-2">{t('admin.federation.shares.col_original')}</th>
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
            <td class="px-3 py-2" data-testid="share-original-cell">
              {s.allow_original ? t('admin.federation.shares.original_yes') : t('admin.federation.shares.original_no')}
            </td>
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
