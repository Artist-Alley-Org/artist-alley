<script lang="ts">
  // Admin audit view of the activities ledger (Phase 1.22.A-bis-3b).
  //
  // Reads from GET /admin/activities with filter + cursor
  // pagination. Each row shows the canonical (type, actor →
  // object, time) header and a click-to-expand JSON payload
  // dump. Operators use this surface to:
  //   - Verify federation outbox emissions are being recorded
  //     (the gold-standard ADR-0044 invariant)
  //   - Investigate per-actor activity bursts (abuse triage)
  //   - Audit inbound peer activities once 1.22.D wires inbox
  //
  // Cap-gated server-side on system.admin; the frontend doesn't
  // re-check (we get a 403 + render a friendly error if we don't
  // have the cap).

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface Activity {
    id: string;
    activity_uri: string;
    activity_type: string;
    actor_uri: string;
    actor_user_ref?: number | null;
    object_uri?: string | null;
    object_kind?: string | null;
    object_local_id?: string | null;
    target_uri?: string | null;
    to?: string[];
    cc?: string[];
    payload?: Record<string, unknown>;
    signature_value?: string | null;
    signature_pubkey?: string | null;
    source: string;
    published_at: string;
    created_at: string;
  }

  let items = $state<Activity[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(true);
  let loadingMore = $state(false);
  let error = $state<string | null>(null);
  let expanded = $state<Record<string, boolean>>({});

  // Filter state. Empty string = no filter.
  let filterType = $state('');
  let filterSource = $state('');
  let filterKind = $state('');
  let filterActor = $state('');

  // Closed catalogue of activity types — matches
  // app/internal/federation/vocab.go's KnownActivityTypes.
  const ACTIVITY_TYPES = [
    '', 'Create', 'Update', 'Delete', 'Follow', 'Accept', 'Reject',
    'Undo', 'Like', 'Announce', 'Block', 'Add', 'Remove',
    'aa:Share', 'aa:Unshare', 'aa:Approve', 'aa:RequestChanges',
    'aa:MarkReviewed', 'aa:Annotation', 'aa:WorkflowTransition',
    'aa:AssetVersion', 'aa:Subscribe', 'aa:Mention',
  ];

  // Object kinds — matches activities.ActivityObjectKind.
  const OBJECT_KINDS = [
    '', 'post', 'comment', 'asset', 'user', 'collection',
    'workspace', 'brand_kit', 'message', 'activity',
  ];

  onMount(() => {
    void refresh();
  });

  // Re-fetch from page 1 whenever any filter changes.
  $effect(() => {
    void filterType;
    void filterSource;
    void filterKind;
    void filterActor;
    items = [];
    nextCursor = null;
    void refresh();
  });

  async function refresh(): Promise<void> {
    loading = true;
    error = null;
    try {
      const params = buildParams();
      const r = await api.GET('/admin/activities', { params: { query: params as never } });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.activities.load_error');
        return;
      }
      if (r.data) {
        items = (r.data.items ?? []) as Activity[];
        nextCursor = (r.data.next_cursor as string | null) ?? null;
      }
    } finally {
      loading = false;
    }
  }

  async function loadMore(): Promise<void> {
    if (!nextCursor || loadingMore) return;
    loadingMore = true;
    try {
      const params = buildParams();
      params.cursor = nextCursor;
      const r = await api.GET('/admin/activities', { params: { query: params as never } });
      if (r.data) {
        items = [...items, ...((r.data.items ?? []) as Activity[])];
        nextCursor = (r.data.next_cursor as string | null) ?? null;
      }
    } finally {
      loadingMore = false;
    }
  }

  function buildParams(): Record<string, unknown> {
    const p: Record<string, unknown> = { limit: 50 };
    if (filterType) p.activity_type = filterType;
    if (filterSource) p.source = filterSource;
    if (filterKind) p.object_kind = filterKind;
    if (filterActor) {
      const n = Number(filterActor);
      if (Number.isFinite(n)) p.actor_user_ref = n;
    }
    return p;
  }

  function clearFilters(): void {
    filterType = '';
    filterSource = '';
    filterKind = '';
    filterActor = '';
  }

  function toggle(id: string): void {
    expanded = { ...expanded, [id]: !expanded[id] };
  }

  function timeLabel(iso: string): string {
    try {
      return new Date(iso).toLocaleString();
    } catch {
      return iso;
    }
  }

  function shortUri(uri: string): string {
    // Strip the instance prefix for readability — the activity_uri
    // is always {baseURL}/activities/{uuid}; the UUID is the
    // interesting part.
    try {
      const u = new URL(uri);
      return u.pathname;
    } catch {
      return uri;
    }
  }
</script>

<svelte:head><title>{t('admin.activities.title')} — artist-alley</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">{t('admin.activities.title')}</h2>
  <p class="text-sm text-fg-muted">{t('admin.activities.intro')}</p>
</header>

<section class="mb-4 flex flex-wrap items-end gap-3 rounded-lg border border-border bg-surface p-3">
  <label class="flex flex-col gap-1">
    <span class="text-xs text-fg-muted">{t('admin.activities.filter_type')}</span>
    <select bind:value={filterType} class="rounded-md border border-border bg-surface px-2 py-1 text-sm">
      {#each ACTIVITY_TYPES as opt}
        <option value={opt}>{opt || t('admin.activities.filter_any')}</option>
      {/each}
    </select>
  </label>

  <label class="flex flex-col gap-1">
    <span class="text-xs text-fg-muted">{t('admin.activities.filter_kind')}</span>
    <select bind:value={filterKind} class="rounded-md border border-border bg-surface px-2 py-1 text-sm">
      {#each OBJECT_KINDS as opt}
        <option value={opt}>{opt || t('admin.activities.filter_any')}</option>
      {/each}
    </select>
  </label>

  <label class="flex flex-col gap-1">
    <span class="text-xs text-fg-muted">{t('admin.activities.filter_source')}</span>
    <input
      bind:value={filterSource}
      type="text"
      placeholder={t('admin.activities.source_placeholder')}
      class="w-48 rounded-md border border-border bg-surface px-2 py-1 text-sm"
    />
  </label>

  <label class="flex flex-col gap-1">
    <span class="text-xs text-fg-muted">{t('admin.activities.filter_actor')}</span>
    <input
      bind:value={filterActor}
      type="number"
      placeholder={t('admin.activities.actor_placeholder')}
      class="w-24 rounded-md border border-border bg-surface px-2 py-1 text-sm"
    />
  </label>

  <button
    type="button"
    class="rounded-md border border-border bg-surface px-3 py-1 text-xs hover:bg-state-hover"
    onclick={clearFilters}
  >
    {t('admin.activities.clear_filters')}
  </button>
</section>

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>
{:else if items.length === 0}
  <p class="rounded-md border border-border bg-surface px-4 py-8 text-center text-sm text-fg-muted">
    {t('admin.activities.empty')}
  </p>
{:else}
  <div class="overflow-hidden rounded-lg border border-border bg-surface">
    <table class="w-full text-sm">
      <thead class="bg-surface-elevated text-xs uppercase tracking-wider text-fg-muted">
        <tr>
          <th class="px-3 py-2 text-left font-medium">{t('admin.activities.col_when')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.activities.col_type')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.activities.col_actor')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.activities.col_object')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.activities.col_source')}</th>
        </tr>
      </thead>
      <tbody>
        {#each items as it (it.id)}
          <tr
            class="border-t border-border hover:bg-state-hover/40 cursor-pointer"
            onclick={() => toggle(it.id)}
          >
            <td class="px-3 py-2 text-xs text-fg-muted">{timeLabel(it.published_at)}</td>
            <td class="px-3 py-2 font-mono text-xs">{it.activity_type}</td>
            <td class="px-3 py-2 text-xs">
              <span class="font-mono">{shortUri(it.actor_uri)}</span>
              {#if it.actor_user_ref}
                <span class="text-fg-muted">(#{it.actor_user_ref})</span>
              {/if}
            </td>
            <td class="px-3 py-2 text-xs">
              {#if it.object_kind}
                <span class="rounded bg-surface-elevated px-1.5 py-0.5 text-[10px] font-medium text-fg-muted">{it.object_kind}</span>
              {/if}
              {#if it.object_uri}
                <span class="ml-1 font-mono">{shortUri(it.object_uri)}</span>
              {/if}
            </td>
            <td class="px-3 py-2 text-xs">
              {#if it.source === 'local'}
                <span class="rounded bg-accent-container px-1.5 py-0.5 text-[10px] font-medium text-on-accent-container">{t('admin.activities.source_local')}</span>
              {:else}
                <span class="rounded bg-warning/20 px-1.5 py-0.5 text-[10px] font-medium text-warning">{t('admin.activities.source_peer')}</span>
                <span class="ml-1 text-fg-muted">{it.source}</span>
              {/if}
            </td>
          </tr>
          {#if expanded[it.id]}
            <tr class="border-t border-border bg-surface-elevated/40">
              <td colspan="5" class="px-3 py-3">
                <dl class="grid grid-cols-1 gap-1 text-xs">
                  <div><dt class="inline font-medium">{t('admin.activities.label_activity_uri')}</dt> <dd class="inline font-mono">{it.activity_uri}</dd></div>
                  {#if it.target_uri}
                    <div><dt class="inline font-medium">{t('admin.activities.label_target_uri')}</dt> <dd class="inline font-mono">{it.target_uri}</dd></div>
                  {/if}
                  {#if it.to && it.to.length}
                    <div><dt class="inline font-medium">{t('admin.activities.label_to')}</dt> <dd class="inline font-mono">{it.to.join(', ')}</dd></div>
                  {/if}
                  {#if it.cc && it.cc.length}
                    <div><dt class="inline font-medium">{t('admin.activities.label_cc')}</dt> <dd class="inline font-mono">{it.cc.join(', ')}</dd></div>
                  {/if}
                  {#if it.signature_value}
                    <div><dt class="inline font-medium">{t('admin.activities.label_signed')}</dt> <dd class="inline">{t('admin.activities.label_signed_yes', { key: it.signature_pubkey ?? '' })}</dd></div>
                  {:else}
                    <div><dt class="inline font-medium">{t('admin.activities.label_signed')}</dt> <dd class="inline text-fg-muted">{t('admin.activities.label_signed_no')}</dd></div>
                  {/if}
                  <div class="mt-2">
                    <dt class="font-medium">{t('admin.activities.label_payload')}</dt>
                    <pre class="mt-1 overflow-x-auto rounded bg-surface p-2 text-[11px]">{JSON.stringify(it.payload ?? {}, null, 2)}</pre>
                  </div>
                </dl>
              </td>
            </tr>
          {/if}
        {/each}
      </tbody>
    </table>
  </div>

  {#if nextCursor}
    <div class="mt-4 flex justify-center">
      <button
        type="button"
        class="rounded-md border border-border bg-surface px-4 py-2 text-sm hover:bg-state-hover disabled:opacity-50"
        onclick={loadMore}
        disabled={loadingMore}
      >
        {loadingMore ? t('common.loading') : t('admin.activities.load_more')}
      </button>
    </div>
  {/if}
{/if}
