<script lang="ts">
  // Admin federation directories — Phase 1.22.B-c.
  //
  // Two surfaces in one page:
  //   1. Top: subscribed directories + add-subscription form
  //   2. Bottom: when a directory is selected, the cached
  //      entries from that directory with "Pair" buttons that
  //      pre-fill the existing /admin/federation/peers handshake
  //      flow (the directory is purely advisory — actual trust
  //      is established by the per-peer handshake).
  //
  // Per docs/spec/federation-directory/v1.md the subscriber:
  //   - Trusts the operator pubkey ON FIRST USE (admin reviews
  //     fingerprint after fetch)
  //   - Caches entries locally so directory outages don't drop
  //     discovered peers
  //   - Verifies the signed listing on every poll against the
  //     pinned operator key

  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface Directory {
    id: string;
    directory_url: string;
    operator_name: string;
    operator_public_key: string;
    operator_fingerprint: string;
    operator_contact?: string | null;
    enabled: boolean;
    last_polled_at?: string | null;
    last_poll_status: string;
    last_poll_error: string;
    poll_interval_seconds: number;
    notes: string;
    subscribed_at: string;
    subscribed_by_user_ref: number;
  }

  interface Entry {
    id: string;
    directory_id: string;
    instance_url: string;
    display_name: string;
    instance_public_key: string;
    fingerprint: string;
    region?: string;
    description?: string;
    tags?: string[];
    verified_at: string;
    verified_via: string;
    listing_id?: string;
    cached_at: string;
  }

  let directories = $state<Directory[]>([]);
  let entries = $state<Entry[]>([]);
  let selectedID = $state<string | null>(null);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Subscribe form state.
  let showSubscribe = $state(false);
  let subscribeURL = $state('');
  let subscribeNotes = $state('');
  let subscribeError = $state<string | null>(null);
  let subscribing = $state(false);

  onMount(() => {
    void loadDirectories();
  });

  async function loadDirectories(): Promise<void> {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/admin/federation/directories');
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.federation.dir_load_error');
        return;
      }
      directories = (r.data?.items ?? []) as Directory[];
    } finally {
      loading = false;
    }
  }

  async function loadEntries(dir: Directory): Promise<void> {
    selectedID = dir.id;
    const r = await api.GET('/admin/federation/directories/{id}/entries', {
      params: { path: { id: dir.id } },
    });
    if (!r.error) entries = (r.data?.items ?? []) as Entry[];
  }

  async function submitSubscribe(): Promise<void> {
    subscribeError = null;
    subscribing = true;
    try {
      const body: Record<string, string> = { directory_url: subscribeURL.trim() };
      if (subscribeNotes.trim()) body.notes = subscribeNotes.trim();
      const r = await api.POST('/admin/federation/directories', { body: body as never });
      if (r.error) {
        subscribeError = (r.error as { error?: string } | undefined)?.error ?? t('admin.federation.dir_subscribe_error');
        return;
      }
      subscribeURL = '';
      subscribeNotes = '';
      showSubscribe = false;
      await loadDirectories();
    } finally {
      subscribing = false;
    }
  }

  async function pollNow(dir: Directory): Promise<void> {
    const r = await api.POST('/admin/federation/directories/{id}/poll', {
      params: { path: { id: dir.id } },
    });
    if (!r.error) {
      await loadDirectories();
      if (selectedID === dir.id) await loadEntries(dir);
    }
  }

  async function unsubscribe(dir: Directory): Promise<void> {
    if (!confirm(t('admin.federation.dir_confirm_unsubscribe', { name: dir.operator_name || dir.directory_url }))) return;
    const r = await api.DELETE('/admin/federation/directories/{id}', {
      params: { path: { id: dir.id } },
    });
    if (!r.error) {
      if (selectedID === dir.id) { selectedID = null; entries = []; }
      await loadDirectories();
    }
  }

  function pairFromEntry(entry: Entry): void {
    // Drop into the existing peers page with a hash anchor that
    // tells it to pre-fill the pair form. The peers page reads
    // window.location.hash on mount and opens "Pair by URL"
    // with the encoded URL + display_name (post-shipment we can
    // wire this with query params if simpler).
    const url = encodeURIComponent(entry.instance_url);
    const name = encodeURIComponent(entry.display_name);
    void goto(`/admin/federation/peers#pair=${url}&name=${name}`);
  }

  function statusLabel(s: string): string {
    return t(`admin.federation.dir_status_${s}`, { fallback: s });
  }

  function timeAgo(iso: string | null | undefined): string {
    if (!iso) return t('admin.federation.dir_never_polled');
    return new Date(iso).toLocaleString();
  }
</script>

<svelte:head><title>{t('admin.federation.dir_title')} — artist-alley</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">{t('admin.federation.dir_title')}</h2>
  <p class="text-sm text-fg-muted">{t('admin.federation.dir_intro')}</p>
  <p class="mt-2 rounded border border-warning/40 bg-warning/10 px-3 py-2 text-xs text-warning">
    <strong>{t('admin.federation.dir_banner_label')}</strong>
    {t('admin.federation.dir_banner')}
  </p>
</header>

<section class="mb-4 flex items-center justify-between">
  <h3 class="text-lg font-semibold">{t('admin.federation.dir_subscribed')}</h3>
  <button
    type="button"
    class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90"
    onclick={() => (showSubscribe = !showSubscribe)}
  >
    {showSubscribe ? t('admin.federation.dir_cancel') : t('admin.federation.dir_subscribe')}
  </button>
</section>

{#if showSubscribe}
  <form
    class="mb-6 space-y-3 rounded-lg border border-border bg-surface p-4"
    onsubmit={(e) => { e.preventDefault(); void submitSubscribe(); }}
  >
    <h4 class="text-sm font-semibold">{t('admin.federation.dir_subscribe')}</h4>
    <p class="text-xs text-fg-muted">{t('admin.federation.dir_subscribe_help')}</p>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.dir_url')}</span>
      <input
        bind:value={subscribeURL}
        type="url"
        required
        placeholder={t('admin.federation.dir_url_placeholder')}
        class="mt-1 block w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
      />
    </label>
    <label class="block">
      <span class="text-xs font-medium">{t('admin.federation.dir_notes')}</span>
      <input
        bind:value={subscribeNotes}
        type="text"
        class="mt-1 block w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
      />
    </label>
    {#if subscribeError}
      <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{subscribeError}</p>
    {/if}
    <div class="flex justify-end gap-2">
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-state-hover"
        onclick={() => (showSubscribe = false)}
      >{t('admin.federation.dir_cancel')}</button>
      <button
        type="submit"
        class="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-on-accent hover:bg-accent/90 disabled:opacity-50"
        disabled={subscribing}
      >{subscribing ? t('common.loading') : t('admin.federation.dir_subscribe_submit')}</button>
    </div>
  </form>
{/if}

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>
{:else if directories.length === 0}
  <p class="rounded-md border border-border bg-surface px-4 py-8 text-center text-sm text-fg-muted">
    {t('admin.federation.dir_empty')}
  </p>
{:else}
  <ul class="mb-6 space-y-2">
    {#each directories as d (d.id)}
      <li class="rounded-lg border border-border bg-surface p-3">
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1">
            <div class="font-medium">{d.operator_name || d.directory_url}</div>
            <div class="font-mono text-xs text-fg-muted">{d.directory_url}</div>
            <div class="mt-1 text-[11px] text-fg-muted">
              {t('admin.federation.dir_fingerprint')}: <span class="font-mono">{d.operator_fingerprint}</span>
            </div>
            <div class="mt-1 text-[11px] text-fg-muted">
              {t('admin.federation.dir_last_poll')}: {timeAgo(d.last_polled_at)} · {t('admin.federation.dir_status')}:
              {#if d.last_poll_status === 'ok'}
                <span class="rounded bg-accent-container px-1.5 py-0.5 text-on-accent-container">{statusLabel(d.last_poll_status)}</span>
              {:else if d.last_poll_status === 'never_polled'}
                <span class="text-fg-muted">{statusLabel(d.last_poll_status)}</span>
              {:else}
                <span class="rounded bg-danger/20 px-1.5 py-0.5 text-danger">{statusLabel(d.last_poll_status)}</span>
              {/if}
            </div>
            {#if d.last_poll_error}
              <div class="mt-1 text-[11px] text-danger">{d.last_poll_error}</div>
            {/if}
          </div>
          <div class="flex flex-shrink-0 flex-col gap-2">
            <button
              type="button"
              class="rounded-md border border-border bg-surface px-3 py-1 text-xs hover:bg-state-hover"
              onclick={() => loadEntries(d)}
            >{t('admin.federation.dir_browse')}</button>
            <button
              type="button"
              class="rounded-md border border-border bg-surface px-3 py-1 text-xs hover:bg-state-hover"
              onclick={() => pollNow(d)}
            >{t('admin.federation.dir_poll_now')}</button>
            <button
              type="button"
              class="rounded-md border border-danger/40 bg-danger/10 px-3 py-1 text-xs text-danger hover:bg-danger/20"
              onclick={() => unsubscribe(d)}
            >{t('admin.federation.dir_unsubscribe')}</button>
          </div>
        </div>
      </li>
    {/each}
  </ul>
{/if}

{#if selectedID && entries.length > 0}
  <section class="rounded-lg border border-border bg-surface">
    <header class="border-b border-border p-3">
      <h3 class="text-sm font-semibold">{t('admin.federation.dir_entries_title', { count: entries.length })}</h3>
      <p class="text-xs text-fg-muted">{t('admin.federation.dir_entries_help')}</p>
    </header>
    <ul class="divide-y divide-border">
      {#each entries as e (e.id)}
        <li class="flex items-start justify-between gap-3 p-3">
          <div class="min-w-0 flex-1">
            <div class="font-medium">{e.display_name}</div>
            <div class="font-mono text-xs">{e.instance_url}</div>
            {#if e.description}
              <div class="mt-1 text-xs">{e.description}</div>
            {/if}
            <div class="mt-1 flex flex-wrap gap-2 text-[11px] text-fg-muted">
              {#if e.region}
                <span>{e.region}</span>
              {/if}
              {#if e.tags && e.tags.length > 0}
                {#each e.tags as tag}
                  <span class="rounded bg-surface-elevated px-1.5 py-0.5">{tag}</span>
                {/each}
              {/if}
            </div>
            <div class="mt-1 text-[11px] text-fg-muted">
              {t('admin.federation.dir_fingerprint')}: <span class="font-mono">{e.fingerprint}</span> · {t('admin.federation.dir_verified_via')}: {e.verified_via}
            </div>
          </div>
          <button
            type="button"
            class="flex-shrink-0 rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-on-accent hover:bg-accent/90"
            onclick={() => pairFromEntry(e)}
          >{t('admin.federation.dir_pair_with')}</button>
        </li>
      {/each}
    </ul>
  </section>
{:else if selectedID}
  <p class="text-sm text-fg-muted">{t('admin.federation.dir_entries_empty')}</p>
{/if}
