<script lang="ts">
  // Admin metadata-extraction failures queue — Phase 1.18.A-2 follow-up B.
  //
  // Per-row review surface for extraction_failure rows the upload-
  // side or backfill-side jobs landed. Operator can filter by
  // error_kind + format + dismiss rows one-by-one. Dismiss is soft —
  // rows stay for audit. Counts above the table power the nav badge.

  import { onMount } from 'svelte';
  import { api } from '$api/client';

  type FailureRow = {
    id: string;
    asset_id: string;
    format: string;
    error_kind: string;
    message: string;
    field_key?: string;
    raw_value?: unknown;
    occurred_at: string;
    dismissed_at?: string | null;
  };

  // The full enum of error_kinds the job handler + applier can
  // emit. Kept inline (no remote enum endpoint) so the filter
  // dropdown is one click — these are stable per docs/observability.
  const ERROR_KINDS = [
    { value: '',                   label: '— all —' },
    { value: 'unsupported_format', label: 'unsupported_format' },
    { value: 'malformed_file',     label: 'malformed_file' },
    { value: 'library_panic',      label: 'library_panic' },
    { value: 'validation',         label: 'validation' },
  ];

  let errorKindFilter = $state('');
  let formatFilter = $state('');
  let rows = $state<FailureRow[]>([]);
  let total = $state(0);
  let offset = $state(0);
  const PAGE_SIZE = 50;
  let loading = $state(false);
  let error = $state('');

  async function load(reset = true) {
    loading = true;
    error = '';
    if (reset) offset = 0;
    try {
      const params: Record<string, string | number> = { limit: PAGE_SIZE, offset };
      if (errorKindFilter) params.error_kind = errorKindFilter;
      if (formatFilter) params.format = formatFilter;
      const r = await api.GET('/admin/metadata-extraction/failures', { params: { query: params } });
      if (r.error) {
        error = (r.error as { error?: string }).error || 'load failed';
        return;
      }
      const data = r.data as { items: FailureRow[]; total: number };
      rows = reset ? data.items : [...rows, ...data.items];
      total = data.total;
    } finally {
      loading = false;
    }
  }

  async function dismiss(row: FailureRow) {
    if (!confirm(`Dismiss this failure?\n\nasset: ${row.asset_id.slice(0, 8)}…\nkind: ${row.error_kind}\nmessage: ${row.message}`)) return;
    const r = await api.POST('/admin/metadata-extraction/failures/{id}/dismiss', {
      params: { path: { id: row.id } },
    });
    if (r.error) {
      const err = (r.error as { error?: string }).error || 'dismiss failed';
      alert(err);
      return;
    }
    rows = rows.filter(x => x.id !== row.id);
    total = Math.max(0, total - 1);
  }

  function pageInfo(): string {
    const start = total === 0 ? 0 : offset + 1;
    const end = Math.min(offset + rows.length, total);
    return `${start}–${end} of ${total}`;
  }

  function nextPage() {
    offset += PAGE_SIZE;
    void load(true);
  }
  function prevPage() {
    offset = Math.max(0, offset - PAGE_SIZE);
    void load(true);
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

  function kindPillClass(k: string): string {
    switch (k) {
      case 'validation':         return 'bg-warning/15 text-warning border-warning/40';
      case 'malformed_file':     return 'bg-danger/15 text-danger border-danger/40';
      case 'unsupported_format': return 'bg-info/15 text-info border-info/40';
      case 'library_panic':      return 'bg-danger/15 text-danger border-danger/40';
      default:                   return 'bg-fg-muted/15 text-fg-muted border-fg-muted/40';
    }
  }

  function rawDisplay(v: unknown): string {
    if (v === null || v === undefined) return '';
    if (typeof v === 'string') return v;
    try { return JSON.stringify(v); } catch { return String(v); }
  }

  onMount(() => { void load(true); });
</script>

<svelte:head><title>Metadata extraction failures — artist-alley</title></svelte:head>

<header class="mb-4">
  <h2 class="text-2xl font-semibold">Metadata extraction failures</h2>
  <p class="text-sm text-fg-muted">
    Per-asset failures the EXIF extract + applier pipeline recorded.
    Validation failures carry the rejected raw value; library /
    format failures carry the parser's message. Dismiss is soft —
    rows stay queryable for audit.
    Re-extract everything via the
    <a class="text-accent hover:underline" href="/admin/metadata-extraction/backfills">backfill page</a>.
  </p>
</header>

<section class="mb-6 rounded border border-border bg-bg-soft p-4">
  <h3 class="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-muted">Filters</h3>
  <div class="grid gap-3 sm:grid-cols-3">
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Error kind</span>
      <select bind:value={errorKindFilter} class="rounded border border-border bg-bg p-2 text-fg">
        {#each ERROR_KINDS as k}
          <option value={k.value}>{k.label}</option>
        {/each}
      </select>
    </label>
    <label class="flex flex-col gap-1 text-sm">
      <span class="text-fg-muted">Format (MIME)</span>
      <input
        bind:value={formatFilter}
        placeholder="image/jpeg, image/png, …"
        class="rounded border border-border bg-bg p-2 text-fg"
      />
    </label>
    <div class="flex items-end gap-2">
      <button
        onclick={() => load(true)}
        disabled={loading}
        class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
      >{loading ? 'Loading…' : 'Apply'}</button>
      <button
        onclick={() => { errorKindFilter = ''; formatFilter = ''; load(true); }}
        class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg"
      >Clear</button>
    </div>
  </div>
</section>

{#if error}
  <div class="mb-4 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger">{error}</div>
{/if}

<section class="rounded border border-border bg-bg-soft">
  <header class="flex items-center justify-between border-b border-border px-3 py-2 text-sm text-fg-muted">
    <div>{pageInfo()}</div>
    <div class="flex gap-2">
      <button
        onclick={prevPage}
        disabled={loading || offset === 0}
        class="rounded border border-border px-2 py-1 text-xs hover:bg-bg disabled:opacity-40"
      >‹ Prev</button>
      <button
        onclick={nextPage}
        disabled={loading || offset + rows.length >= total}
        class="rounded border border-border px-2 py-1 text-xs hover:bg-bg disabled:opacity-40"
      >Next ›</button>
    </div>
  </header>
  <table class="w-full text-sm">
    <thead class="border-b border-border bg-bg/60 text-fg-muted">
      <tr>
        <th class="px-3 py-2 text-left font-medium">Kind</th>
        <th class="px-3 py-2 text-left font-medium">Asset</th>
        <th class="px-3 py-2 text-left font-medium">Format</th>
        <th class="px-3 py-2 text-left font-medium">Field</th>
        <th class="px-3 py-2 text-left font-medium">Message</th>
        <th class="px-3 py-2 text-left font-medium">Raw</th>
        <th class="px-3 py-2 text-left font-medium">Occurred</th>
        <th class="px-3 py-2 text-right font-medium">Actions</th>
      </tr>
    </thead>
    <tbody>
      {#each rows as row (row.id)}
        <tr class="border-b border-border/40 last:border-0">
          <td class="px-3 py-2">
            <span class="inline-flex items-center rounded border px-2 py-0.5 text-xs font-medium {kindPillClass(row.error_kind)}">{row.error_kind}</span>
          </td>
          <td class="px-3 py-2">
            <a class="text-accent hover:underline" href={`/assets/${row.asset_id}`}>
              <code class="text-xs">{row.asset_id.slice(0, 8)}…</code>
            </a>
          </td>
          <td class="px-3 py-2"><code class="text-xs">{row.format}</code></td>
          <td class="px-3 py-2 text-fg-muted">{row.field_key || ''}</td>
          <td class="max-w-md truncate px-3 py-2 text-fg-muted" title={row.message}>{row.message}</td>
          <td class="max-w-xs truncate px-3 py-2 text-fg-muted" title={rawDisplay(row.raw_value)}>{rawDisplay(row.raw_value)}</td>
          <td class="px-3 py-2 text-fg-muted" title={row.occurred_at}>{relTime(row.occurred_at)}</td>
          <td class="px-3 py-2 text-right">
            <button
              onclick={() => dismiss(row)}
              class="rounded border border-accent px-2 py-1 text-xs text-accent hover:bg-accent hover:text-accent-fg"
            >Dismiss</button>
          </td>
        </tr>
      {/each}
      {#if rows.length === 0 && !loading}
        <tr>
          <td colspan="8" class="px-3 py-6 text-center text-fg-muted">No pending failures match the filter.</td>
        </tr>
      {/if}
    </tbody>
  </table>
</section>
