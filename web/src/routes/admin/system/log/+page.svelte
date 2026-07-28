<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin audit log viewer (Phase 1.17.K).
  //
  // Reads from GET /admin/audit with filter + cursor pagination.
  // Each row renders the canonical (event_type, occurred_at, actor →
  // subject) header and a click-to-expand body. For well-known
  // event types we render the metadata as a structured changeset
  // ("active → disabled", grants added/removed, etc.); the long
  // tail falls back to a JSON dump so nothing is hidden.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  interface AuditEvent {
    id: string;
    event_type: string;
    occurred_at: string;
    subject_user_ref?: number | null;
    actor_user_ref?: number | null;
    ip?: string | null;
    user_agent?: string | null;
    metadata: Record<string, unknown>;
  }

  let events = $state<AuditEvent[]>([]);
  let eventTypes = $state<string[]>([]);
  let total = $state<number>(0);
  let nextCursor = $state<string | null>(null);
  let loading = $state(true);
  let loadingMore = $state(false);
  let error = $state<string | null>(null);

  // Filters.
  let filterEventType = $state('');
  let filterActor = $state('');
  let filterSubject = $state('');
  let filterSince = $state('');
  let filterUntil = $state('');

  // UI state.
  let expanded = $state<Record<string, boolean>>({});

  onMount(() => { void load(); void loadTypes(); });

  async function loadTypes() {
    try {
      const r = await api.GET('/admin/audit/event_types');
      if (r.data) {
        eventTypes = ((r.data as unknown as { items?: string[] }).items ?? []).sort();
      }
    } catch {
      // Types are nice-to-have; the dropdown gracefully degrades to
      // a free-text input if the lookup fails.
    }
  }

  function buildQuery(cursor: string | null = null): Record<string, string | number | undefined> {
    const q: Record<string, string | number | undefined> = { limit: 100 };
    if (filterEventType) q.event_type = filterEventType;
    if (filterActor) {
      const n = parseInt(filterActor, 10);
      if (!isNaN(n) && n > 0) q.actor_user_ref = n;
    }
    if (filterSubject) {
      const n = parseInt(filterSubject, 10);
      if (!isNaN(n) && n > 0) q.subject_user_ref = n;
    }
    if (filterSince) q.since = new Date(filterSince).toISOString();
    if (filterUntil) q.until = new Date(filterUntil).toISOString();
    if (cursor) q.cursor = cursor;
    return q;
  }

  async function load() {
    loading = true;
    error = null;
    expanded = {};
    try {
      const r = await api.GET('/admin/audit', { params: { query: buildQuery() } });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.audit.load_error');
        return;
      }
      const page = r.data as unknown as { items: AuditEvent[]; total: number; next_cursor?: string | null };
      events = page.items ?? [];
      total = page.total ?? 0;
      nextCursor = page.next_cursor ?? null;
    } finally {
      loading = false;
    }
  }

  async function loadMore() {
    if (!nextCursor || loadingMore) return;
    loadingMore = true;
    try {
      const r = await api.GET('/admin/audit', { params: { query: buildQuery(nextCursor) } });
      if (r.error || !r.data) {
        error = (r.error as { error?: string } | undefined)?.error ?? t('admin.audit.load_more_error');
        return;
      }
      const page = r.data as unknown as { items: AuditEvent[]; total: number; next_cursor?: string | null };
      events = events.concat(page.items ?? []);
      nextCursor = page.next_cursor ?? null;
    } finally {
      loadingMore = false;
    }
  }

  function applyFilters(e: SubmitEvent) {
    e.preventDefault();
    void load();
  }

  function clearFilters() {
    filterEventType = '';
    filterActor = '';
    filterSubject = '';
    filterSince = '';
    filterUntil = '';
    void load();
  }

  function eventTypeBadgeClass(t: string): string {
    if (t.startsWith('login.')) return 'border-accent/40 bg-accent/10 text-accent';
    if (t === 'logout' || t.startsWith('session.')) return 'border-fg-muted/40 bg-fg-muted/10 text-fg-muted';
    if (t.startsWith('user.password')) return 'border-warning/40 bg-warning/10 text-warning';
    if (t.startsWith('user.capability') || t.startsWith('user.status')) return 'border-danger/40 bg-danger/10 text-danger';
    return 'border-border bg-surface text-fg-muted';
  }

  function statusLabel(n: unknown): string {
    if (n === 1 || n === '1') return 'active';
    if (n === 0 || n === '0') return 'pending';
    if (n === 2 || n === '2') return 'disabled';
    return String(n ?? '?');
  }

  // Returns a structured key/value list for well-known event types,
  // or null when the metadata should be rendered as raw JSON.
  function changesetRows(ev: AuditEvent): Array<{ label: string; value: string; mono?: boolean }> | null {
    const m = ev.metadata ?? {};
    switch (ev.event_type) {
      case 'user.status_changed':
        return [
          { label: t('admin.audit.field_previous'), value: statusLabel(m.previous) },
          { label: t('admin.audit.field_next'), value: statusLabel(m.next) },
          { label: t('admin.audit.field_reason'), value: String(m.reason ?? '') || '—' },
        ];
      case 'user.password_changed':
        return [
          { label: t('admin.audit.field_sessions_revoked'), value: String(m.sessions_revoked ?? 0) },
        ];
      case 'user.password_reset':
        return [
          { label: t('admin.audit.field_reason'), value: String(m.reason ?? '') || '—' },
        ];
      case 'user.capability_granted':
      case 'user.capability_revoked':
        return [
          { label: t('admin.audit.field_capability'), value: String(m.capability ?? ''), mono: true },
          { label: t('admin.audit.field_team_id'), value: String(m.team_id ?? '') || t('admin.audit.field_team_global'), mono: true },
          { label: t('admin.audit.field_note'), value: String(m.note ?? '') || '—' },
        ];
      case 'user.capability_grant_removed':
      case 'user.capability_revoke_removed':
        return [
          { label: t('admin.audit.field_capability'), value: String(m.capability ?? ''), mono: true },
          { label: t('admin.audit.field_team_id'), value: String(m.team_id ?? '') || t('admin.audit.field_team_global'), mono: true },
        ];
      case 'login.succeeded':
      case 'logout':
        return [
          { label: t('admin.audit.field_session_id'), value: String(m.session_id ?? ''), mono: true },
        ];
      case 'login.failed':
        return [
          { label: t('admin.audit.field_attempted_username'), value: String(m.attempted_username ?? '') || '—' },
          { label: t('admin.audit.field_reason'), value: String(m.reason ?? '') || '—' },
        ];
      case 'login.rate_limited':
        return [
          { label: t('admin.audit.field_attempted_username'), value: String(m.attempted_username ?? '') || '—' },
          { label: t('admin.audit.field_key'), value: String(m.key ?? ''), mono: true },
        ];
      case 'session.revoked':
        return [
          { label: t('admin.audit.field_session_id'), value: String(m.session_id ?? ''), mono: true },
          { label: t('admin.audit.field_reason'), value: String(m.reason ?? '') || '—' },
        ];
      default:
        return null;
    }
  }

  function actorSubjectLabel(ev: AuditEvent): string {
    if (ev.actor_user_ref && ev.subject_user_ref && ev.actor_user_ref !== ev.subject_user_ref) {
      return t('admin.audit.actor_to_subject', {
        actor: String(ev.actor_user_ref),
        subject: String(ev.subject_user_ref),
      });
    }
    if (ev.subject_user_ref) {
      return t('admin.audit.subject_only', { subject: String(ev.subject_user_ref) });
    }
    if (ev.actor_user_ref) {
      return t('admin.audit.actor_only', { actor: String(ev.actor_user_ref) });
    }
    return t('admin.audit.no_principal');
  }

  function shortTime(s: string): string {
    return new Date(s).toLocaleString();
  }
</script>

<svelte:head><title>{t('admin.system.log.title')} — {site.name}</title></svelte:head>

<h2 class="mb-2 text-xl font-semibold">{t('admin.audit.title')}</h2>
<p class="mb-4 text-sm text-fg-muted">{t('admin.audit.intro')}</p>

<form onsubmit={applyFilters} class="mb-4 rounded-lg border border-border bg-surface-elevated p-3">
  <div class="grid gap-2 sm:grid-cols-5">
    <label class="block text-[11px]">
      <span class="mb-0.5 block text-fg-muted">{t('admin.audit.filter_event_type')}</span>
      {#if eventTypes.length > 0}
        <select
          bind:value={filterEventType}
          class="w-full rounded border border-border-strong bg-surface px-1.5 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        >
          <option value="">{t('admin.audit.filter_event_type_any')}</option>
          {#each eventTypes as et (et)}
            <option value={et}>{et}</option>
          {/each}
        </select>
      {:else}
        <input
          type="text"
          bind:value={filterEventType}
          placeholder={t('admin.audit.filter_event_type_placeholder')}
          class="w-full rounded border border-border-strong bg-surface px-1.5 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        />
      {/if}
    </label>
    <label class="block text-[11px]">
      <span class="mb-0.5 block text-fg-muted">{t('admin.audit.filter_actor')}</span>
      <input
        type="number"
        bind:value={filterActor}
        min="1"
        placeholder={t('admin.audit.filter_user_ref_placeholder')}
        class="w-full rounded border border-border-strong bg-surface px-1.5 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>
    <label class="block text-[11px]">
      <span class="mb-0.5 block text-fg-muted">{t('admin.audit.filter_subject')}</span>
      <input
        type="number"
        bind:value={filterSubject}
        min="1"
        placeholder={t('admin.audit.filter_user_ref_placeholder')}
        class="w-full rounded border border-border-strong bg-surface px-1.5 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>
    <label class="block text-[11px]">
      <span class="mb-0.5 block text-fg-muted">{t('admin.audit.filter_since')}</span>
      <input
        type="datetime-local"
        bind:value={filterSince}
        class="w-full rounded border border-border-strong bg-surface px-1.5 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>
    <label class="block text-[11px]">
      <span class="mb-0.5 block text-fg-muted">{t('admin.audit.filter_until')}</span>
      <input
        type="datetime-local"
        bind:value={filterUntil}
        class="w-full rounded border border-border-strong bg-surface px-1.5 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>
  </div>
  <div class="mt-3 flex items-center gap-2">
    <button
      type="submit"
      class="rounded bg-accent px-3 py-1 text-xs font-medium text-on-accent"
    >
      {t('admin.audit.apply_filters')}
    </button>
    <button
      type="button"
      onclick={clearFilters}
      class="rounded border border-border bg-surface px-3 py-1 text-xs font-medium"
    >
      {t('admin.audit.clear_filters')}
    </button>
    <span class="ml-auto text-xs text-fg-muted">{t('admin.audit.matched', { total: String(total) })}</span>
  </div>
</form>

{#if loading}
  <p class="text-fg-muted">{t('common.loading')}</p>
{:else if error}
  <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
{:else if events.length === 0}
  <p class="rounded-lg border border-border bg-surface-elevated p-4 text-sm text-fg-muted">{t('admin.audit.empty')}</p>
{:else}
  <ul class="space-y-1.5">
    {#each events as ev (ev.id)}
      {@const rows = changesetRows(ev)}
      <li class="rounded-lg border border-border bg-surface-elevated">
        <button
          type="button"
          onclick={() => (expanded[ev.id] = !expanded[ev.id])}
          class="flex w-full items-center gap-2 px-3 py-2 text-left text-sm hover:bg-surface"
        >
          <span class={'rounded border px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-wider ' + eventTypeBadgeClass(ev.event_type)}>{ev.event_type}</span>
          <span class="text-xs text-fg-muted">{shortTime(ev.occurred_at)}</span>
          <span class="ml-2 text-xs">{actorSubjectLabel(ev)}</span>
          {#if ev.ip}<span class="ml-2 font-mono text-[10px] text-fg-muted">{ev.ip}</span>{/if}
          <span class="ml-auto text-xs text-fg-muted">{expanded[ev.id] ? t('admin.audit.collapse') : t('admin.audit.expand')}</span>
        </button>
        {#if expanded[ev.id]}
          <div class="border-t border-border px-3 py-2">
            {#if rows}
              <dl class="grid grid-cols-[8rem,1fr] gap-x-3 gap-y-1 text-xs">
                {#each rows as row (row.label)}
                  <dt class="text-fg-muted">{row.label}</dt>
                  <dd class={row.mono ? 'break-all font-mono' : ''}>{row.value}</dd>
                {/each}
              </dl>
            {:else}
              <details>
                <summary class="cursor-pointer text-xs text-fg-muted">{t('admin.audit.raw_metadata')}</summary>
                <pre class="mt-1 overflow-x-auto rounded bg-surface p-2 font-mono text-[11px]">{JSON.stringify(ev.metadata, null, 2)}</pre>
              </details>
            {/if}
            {#if ev.user_agent}
              <p class="mt-2 text-[10px] text-fg-muted">{t('admin.audit.user_agent')}: <code class="font-mono">{ev.user_agent}</code></p>
            {/if}
          </div>
        {/if}
      </li>
    {/each}
  </ul>

  {#if nextCursor}
    <div class="mt-4 text-center">
      <button
        type="button"
        onclick={loadMore}
        disabled={loadingMore}
        class="rounded border border-border bg-surface-elevated px-4 py-1.5 text-sm font-medium hover:border-accent disabled:opacity-50"
      >
        {loadingMore ? t('admin.audit.loading_more') : t('admin.audit.load_more')}
      </button>
    </div>
  {/if}
{/if}
