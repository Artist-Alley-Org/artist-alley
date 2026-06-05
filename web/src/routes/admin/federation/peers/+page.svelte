<script lang="ts">
  // Admin federation peers — Phase 1.22.B-a.
  //
  // Manual peer entry: operator pastes the peer's instance URL,
  // display name, and PEM-encoded Ed25519 instance public key
  // (coordinated out-of-band with the peer's operator). 1.22.B-b
  // adds an automated handshake form on top; for v1-a this is
  // the "manual SSH known_hosts" UX.
  //
  // Per ADR 0043 §"Trust model" pairing alone shares no content
  // — that's federation_shares in 1.22.C. The page banner makes
  // this explicit so operators don't expect content to flow just
  // because they paired.

  import { onMount } from 'svelte';
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
    handshake_at: string;
    handshake_by_user_ref: number;
    last_seen_at?: string | null;
    notes: string;
    created_at: string;
    updated_at: string;
  }

  let peers = $state<Peer[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Add-peer form state.
  let showAdd = $state(false);
  let addInstanceURL = $state('');
  let addDisplayName = $state('');
  let addPublicKey = $state('');
  let addTier = $state<Peer['trust_tier']>('connected');
  let addEncryption = $state<Peer['encryption_policy']>('plaintext');
  let addNotes = $state('');
  let addError = $state<string | null>(null);
  let addSubmitting = $state(false);

  onMount(() => {
    void load();
  });

  async function load(): Promise<void> {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/admin/federation/peers');
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.federation.load_error');
        return;
      }
      peers = (r.data?.items ?? []) as Peer[];
    } finally {
      loading = false;
    }
  }

  async function submitAdd(): Promise<void> {
    addError = null;
    addSubmitting = true;
    try {
      const r = await api.POST('/admin/federation/peers', {
        body: {
          instance_url: addInstanceURL.trim(),
          display_name: addDisplayName.trim(),
          instance_public_key: addPublicKey,
          trust_tier: addTier,
          encryption_policy: addEncryption,
          enabled: true,
          notes: addNotes.trim(),
        },
      });
      if (r.error) {
        addError = (r.error as { error?: string } | undefined)?.error ?? t('admin.federation.add_error');
        return;
      }
      // Clear form + reload.
      addInstanceURL = '';
      addDisplayName = '';
      addPublicKey = '';
      addNotes = '';
      showAdd = false;
      await load();
    } finally {
      addSubmitting = false;
    }
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

  async function defederate(p: Peer): Promise<void> {
    if (!confirm(t('admin.federation.confirm_defederate', { name: p.display_name }))) return;
    const r = await api.DELETE('/admin/federation/peers/{id}', {
      params: { path: { id: p.id } },
    });
    if (!r.error) await load();
  }

  function tierLabel(tier: Peer['trust_tier']): string {
    return t(`admin.federation.tier_${tier.replace('-', '_')}`);
  }
</script>

<svelte:head><title>{t('admin.federation.title')} — artist-alley</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">{t('admin.federation.title')}</h2>
  <p class="text-sm text-fg-muted">{t('admin.federation.intro')}</p>
  <p class="mt-2 rounded border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
    <strong>{t('admin.federation.banner_no_content_yet_label')}</strong>
    {t('admin.federation.banner_no_content_yet')}
  </p>
</header>

<section class="mb-4 flex items-center justify-between">
  <h3 class="text-lg font-semibold">{t('admin.federation.paired_peers')}</h3>
  <button
    type="button"
    class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90"
    onclick={() => (showAdd = !showAdd)}
  >
    {showAdd ? t('admin.federation.add_cancel') : t('admin.federation.add_peer')}
  </button>
</section>

{#if showAdd}
  <form
    class="mb-6 space-y-3 rounded-lg border border-border bg-surface p-4"
    onsubmit={(e) => {
      e.preventDefault();
      void submitAdd();
    }}
  >
    <h4 class="text-sm font-semibold">{t('admin.federation.add_peer')}</h4>
    <p class="text-xs text-fg-muted">{t('admin.federation.add_help')}</p>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.form_instance_url')}</span>
      <input
        bind:value={addInstanceURL}
        type="url"
        required
        placeholder={t('admin.federation.form_instance_url_placeholder')}
        class="mt-1 block w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
      />
    </label>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.form_display_name')}</span>
      <input
        bind:value={addDisplayName}
        type="text"
        required
        maxlength="200"
        class="mt-1 block w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
      />
    </label>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.form_public_key')}</span>
      <textarea
        bind:value={addPublicKey}
        required
        rows="6"
        placeholder={t('admin.federation.form_public_key_placeholder')}
        class="mt-1 block w-full rounded-md border border-border bg-surface px-3 py-2 font-mono text-xs"
      ></textarea>
    </label>
    <div class="flex gap-3">
      <label class="flex-1">
        <span class="text-xs font-medium">{t('admin.federation.form_trust_tier')}</span>
        <select bind:value={addTier} class="mt-1 block w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm">
          <option value="connected">{t('admin.federation.tier_connected')}</option>
          <option value="directory-listed">{t('admin.federation.tier_directory_listed')}</option>
          <option value="auto-sync">{t('admin.federation.tier_auto_sync')}</option>
        </select>
      </label>
      <label class="flex-1">
        <span class="text-xs font-medium">{t('admin.federation.form_encryption_policy')}</span>
        <select bind:value={addEncryption} class="mt-1 block w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm">
          <option value="plaintext">{t('admin.federation.encryption_plaintext')}</option>
          <option value="e2e-encrypted">{t('admin.federation.encryption_e2e')}</option>
        </select>
      </label>
    </div>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.form_notes')}</span>
      <input
        bind:value={addNotes}
        type="text"
        class="mt-1 block w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
      />
    </label>
    {#if addError}
      <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{addError}</p>
    {/if}
    <div class="flex justify-end gap-2">
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-state-hover"
        onclick={() => (showAdd = false)}
      >{t('admin.federation.add_cancel')}</button>
      <button
        type="submit"
        class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90 disabled:opacity-50"
        disabled={addSubmitting}
      >{addSubmitting ? t('common.loading') : t('admin.federation.add_submit')}</button>
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
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_tier')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_enc')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.federation.col_enabled')}</th>
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
              <select
                value={p.trust_tier}
                onchange={(e) => setTier(p, (e.currentTarget as HTMLSelectElement).value as Peer['trust_tier'])}
                class="rounded border border-border bg-surface px-1.5 py-0.5 text-xs"
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
            <td class="px-3 py-2 text-xs text-fg-muted">
              {p.last_seen_at ? new Date(p.last_seen_at).toLocaleString() : t('admin.federation.never_seen')}
            </td>
            <td class="px-3 py-2 text-right">
              <button
                type="button"
                class="rounded-md border border-danger/40 bg-danger/10 px-2 py-0.5 text-xs text-danger hover:bg-danger/20"
                onclick={() => defederate(p)}
              >{t('admin.federation.defederate')}</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
