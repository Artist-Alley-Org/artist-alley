<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin audit-log viewer (backend shipped 1.17.K; UI 1.20).
  //
  // Reads GET /admin/audit — cursor-paginated, newest-first, with
  // optional filters (event type, actor, subject, since/until). The
  // event-type dropdown is populated from GET /admin/audit/event_types
  // (the distinct-values facet). Each row expands to show the full
  // metadata blob (the per-event-type changeset).
  //
  // Cap-gated server-side on system.audit.read; the frontend doesn't
  // re-check — a 403 renders a friendly error. Mirrors the activities
  // ledger viewer (admin/system/activities).

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface AuditEvent {
    id: string;
    event_type: string;
    occurred_at: string;
    actor_user_ref?: number | null;
    subject_user_ref?: number | null;
    ip?: string | null;
    user_agent?: string | null;
    metadata?: Record<string, unknown>;
  }

  let items = $state<AuditEvent[]>([]);
  let total = $state(0);
  let nextCursor = $state<string | null>(null);
  let loading = $state(true);
  let loadingMore = $state(false);
  let error = $state<string | null>(null);
  let expanded = $state<Record<string, boolean>>({});

  // Facet: distinct event_type values for the dropdown.
  let eventTypes = $state<string[]>([]);

  // Filter state. Empty string = no filter.
  let filterType = $state('');
  let filterActor = $state('');
  let filterSubject = $state('');
  let filterSince = $state('');
  let filterUntil = $state('');

  onMount(() => {
    void loadEventTypes();
    void refresh();
  });

  // Re-fetch from page 1 whenever any filter changes.
  $effect(() => {
    void filterType;
    void filterActor;
    void filterSubject;
    void filterSince;
    void filterUntil;
    items = [];
    nextCursor = null;
    void refresh();
  });

  async function loadEventTypes(): Promise<void> {
    const r = await api.GET('/admin/audit/event_types');
    if (r.data) eventTypes = (r.data.items ?? []) as string[];
  }

  async function refresh(): Promise<void> {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/admin/audit', { params: { query: buildParams() as never } });
      if (r.error) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.audit.load_error');
        return;
      }
      if (r.data) {
        items = (r.data.items ?? []) as AuditEvent[];
        total = (r.data.total as number) ?? 0;
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
      const r = await api.GET('/admin/audit', { params: { query: params as never } });
      if (r.data) {
        items = [...items, ...((r.data.items ?? []) as AuditEvent[])];
        nextCursor = (r.data.next_cursor as string | null) ?? null;
      }
    } finally {
      loadingMore = false;
    }
  }

  function buildParams(): Record<string, unknown> {
    const p: Record<string, unknown> = { limit: 100 };
    if (filterType) p.event_type = filterType;
    if (filterActor) {
      const n = Number(filterActor);
      if (Number.isFinite(n)) p.actor_user_ref = n;
    }
    if (filterSubject) {
      const n = Number(filterSubject);
      if (Number.isFinite(n)) p.subject_user_ref = n;
    }
    // <input type=datetime-local> yields "YYYY-MM-DDTHH:mm" — turn it
    // into an ISO-8601 instant the API accepts.
    if (filterSince) p.since = new Date(filterSince).toISOString();
    if (filterUntil) p.until = new Date(filterUntil).toISOString();
    return p;
  }

  function clearFilters(): void {
    filterType = '';
    filterActor = '';
    filterSubject = '';
    filterSince = '';
    filterUntil = '';
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
</script>

<svelte:head><title>{t('admin.audit.title')} — {site.name}</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">{t('admin.audit.title')}</h2>
  <p class="text-sm text-fg-muted">{t('admin.audit.intro')}</p>
</header>

<section class="mb-4 flex flex-wrap items-end gap-3 rounded-lg border border-border bg-surface p-3">
  <label class="flex flex-col gap-1">
    <span class="text-xs text-fg-muted">{t('admin.audit.filter_event_type')}</span>
    <select bind:value={filterType} class="rounded-md border border-border bg-surface px-2 py-1 text-sm">
      <option value="">{t('admin.audit.filter_event_type_any')}</option>
      {#each eventTypes as opt (opt)}
        <option value={opt}>{opt}</option>
      {/each}
    </select>
  </label>

  <label class="flex flex-col gap-1">
    <span class="text-xs text-fg-muted">{t('admin.audit.filter_actor')}</span>
    <input
      bind:value={filterActor}
      type="number"
      placeholder={t('admin.audit.filter_user_ref_placeholder')}
      class="w-24 rounded-md border border-border bg-surface px-2 py-1 text-sm"
    />
  </label>

  <label class="flex flex-col gap-1">
    <span class="text-xs text-fg-muted">{t('admin.audit.filter_subject')}</span>
    <input
      bind:value={filterSubject}
      type="number"
      placeholder={t('admin.audit.filter_user_ref_placeholder')}
      class="w-24 rounded-md border border-border bg-surface px-2 py-1 text-sm"
    />
  </label>

  <label class="flex flex-col gap-1">
    <span class="text-xs text-fg-muted">{t('admin.audit.filter_since')}</span>
    <input bind:value={filterSince} type="datetime-local" class="rounded-md border border-border bg-surface px-2 py-1 text-sm" />
  </label>

  <label class="flex flex-col gap-1">
    <span class="text-xs text-fg-muted">{t('admin.audit.filter_until')}</span>
    <input bind:value={filterUntil} type="datetime-local" class="rounded-md border border-border bg-surface px-2 py-1 text-sm" />
  </label>

  <button
    type="button"
    class="rounded-md border border-border bg-surface px-3 py-1 text-xs hover:bg-state-hover"
    onclick={clearFilters}
  >
    {t('admin.audit.clear_filters')}
  </button>
</section>

{#if loading}
  <p class="text-sm text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>
{:else if items.length === 0}
  <p class="rounded-md border border-border bg-surface px-4 py-8 text-center text-sm text-fg-muted">
    {t('admin.audit.empty')}
  </p>
{:else}
  <p class="mb-2 text-xs text-fg-muted">{t('admin.audit.matched', { total })}</p>
  <div class="overflow-hidden rounded-lg border border-border bg-surface">
    <table class="w-full text-sm">
      <thead class="bg-surface-elevated text-xs uppercase tracking-wider text-fg-muted">
        <tr>
          <th class="px-3 py-2 text-left font-medium">{t('admin.audit.col_when')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.audit.col_type')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.audit.col_actor')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.audit.col_subject')}</th>
          <th class="px-3 py-2 text-left font-medium">{t('admin.audit.col_ip')}</th>
        </tr>
      </thead>
      <tbody>
        {#each items as it (it.id)}
          <tr
            class="border-t border-border hover:bg-state-hover/40 cursor-pointer"
            onclick={() => toggle(it.id)}
          >
            <td class="px-3 py-2 text-xs text-fg-muted">{timeLabel(it.occurred_at)}</td>
            <td class="px-3 py-2 font-mono text-xs">{it.event_type}</td>
            <td class="px-3 py-2 text-xs">{it.actor_user_ref != null ? `#${it.actor_user_ref}` : '—'}</td>
            <td class="px-3 py-2 text-xs">{it.subject_user_ref != null ? `#${it.subject_user_ref}` : '—'}</td>
            <td class="px-3 py-2 font-mono text-xs text-fg-muted">{it.ip ?? '—'}</td>
          </tr>
          {#if expanded[it.id]}
            <tr class="border-t border-border bg-surface-elevated/40">
              <td colspan="5" class="px-3 py-3">
                <dl class="grid grid-cols-1 gap-1 text-xs">
                  <div><dt class="inline font-medium">{t('admin.audit.label_event_id')}</dt> <dd class="inline font-mono">{it.id}</dd></div>
                  {#if it.user_agent}
                    <div><dt class="inline font-medium">{t('admin.audit.user_agent')}</dt> <dd class="inline font-mono">{it.user_agent}</dd></div>
                  {/if}
                  <div class="mt-2">
                    <dt class="font-medium">{t('admin.audit.raw_metadata')}</dt>
                    <pre class="mt-1 overflow-x-auto rounded bg-surface p-2 text-[11px]">{JSON.stringify(it.metadata ?? {}, null, 2)}</pre>
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
        {loadingMore ? t('common.loading') : t('admin.audit.load_more')}
      </button>
    </div>
  {/if}
{/if}
