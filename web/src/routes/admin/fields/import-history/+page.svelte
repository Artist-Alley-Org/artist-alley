<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin: the field schema import history (#1576 S4). Read-only.
  //
  // Every receipt this server recorded, newest first, in pages of 25
  // that move by the server's cursor, never by offset, so a receipt
  // recorded meanwhile neither repeats nor hides one. Older and Newer
  // walk a bounded stack of the cursors already used. A failed read
  // leaves the current page in place and Retry asks again for the same
  // position; nothing silently restarts at the first page. Choosing a
  // receipt shows what was decided and what happened AT APPLY TIME, with
  // the field id as recorded then; a field may have changed or gone since.
  //
  // The page belongs to one signed-in operator: a change of account, a
  // loss of rights or a sign-out clears it and ignores late answers.
  //
  // The source note is what the importing operator typed. It is shown
  // as text and never followed, fetched or trusted.
  import { untrack } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { site } from '$stores/site.svelte';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import { api } from '$api/client';
  import type { components } from '$api/schema';

  type Summary = components['schemas']['FieldSchemaImportReceiptSummary'];
  type Record_ = components['schemas']['FieldSchemaImportReceiptRecord'];

  const x = (k: string, v?: Record<string, string | number>) => t(`admin.fields.history.${k}`, v);

  const PAGE_SIZE = 25;
  // The walk keeps every cursor it used (each at most 512 bytes) so
  // Newer is exact; past this depth Older stops rather than grow the
  // stack without bound.
  const MAX_DEPTH = 1000;

  const allowed = $derived(auth.ready && !auth.capsUnavailable && auth.can('fields.admin'));
  const identity = $derived(allowed ? `user:${auth.user?.ref ?? 'none'}` : 'none');
  let seenIdentity: string | null = null;

  let items = $state<Summary[]>([]);
  let nextCursor = $state<string | null>(null);
  // cursors[i] is the cursor that loaded page i; page 0 has none.
  let cursors = $state<(string | null)[]>([null]);
  let listLoading = $state(false);
  let listError = $state('');
  let listLoaded = $state(false);
  let retryList = $state<(() => void) | null>(null);
  let listSeq = 0;

  let detail = $state<Record_ | null>(null);
  let detailError = $state('');
  let detailLoading = $state(false);
  let detailSeq = 0;

  const selectedId = $derived(page.url.searchParams.get('receipt'));

  function errorOf(e: unknown, status: number | undefined): string {
    const msg = (e as { error?: string } | undefined)?.error;
    return msg ? x('error_with', { msg, status: status ?? '?' }) : x('error_status', { status: status ?? 'network' });
  }

  /** Load the page at `cursor`; on success `commit` updates the stack. */
  async function loadPage(cursor: string | null, commit: () => void): Promise<void> {
    const seq = ++listSeq;
    listLoading = true;
    listError = '';
    retryList = null;
    const failed = (msg: string) => {
      listError = msg;
      retryList = () => void loadPage(cursor, commit);
    };
    try {
      const query: { limit: number; cursor?: string } = { limit: PAGE_SIZE };
      if (cursor) query.cursor = cursor;
      const r = await api.GET('/fields/import/receipts', { params: { query } });
      if (seq !== listSeq) return;
      if (r.error || !r.data) {
        failed(errorOf(r.error, r.response?.status));
        return;
      }
      items = r.data.items;
      nextCursor = r.data.next_cursor ?? null;
      listLoaded = true;
      commit();
    } catch {
      if (seq === listSeq) failed(x('error_network'));
    } finally {
      if (seq === listSeq) listLoading = false;
    }
  }

  function older() {
    if (!nextCursor || listLoading || cursors.length >= MAX_DEPTH) return;
    const c = nextCursor;
    void loadPage(c, () => (cursors = [...cursors, c]));
  }

  function newer() {
    if (cursors.length < 2 || listLoading) return;
    const prev = cursors.slice(0, -1);
    void loadPage(prev[prev.length - 1], () => (cursors = prev));
  }

  function newest() {
    if (listLoading) return;
    void loadPage(null, () => (cursors = [null]));
  }

  async function loadDetail(id: string) {
    const seq = ++detailSeq;
    detailLoading = true;
    detailError = '';
    detail = null;
    try {
      const r = await api.GET('/fields/import/receipts/{receipt_id}', { params: { path: { receipt_id: id } } });
      if (seq !== detailSeq) return;
      if (r.response?.status === 404) {
        detailError = x('not_found');
        return;
      }
      if (r.error || !r.data) {
        detailError = errorOf(r.error, r.response?.status);
        return;
      }
      detail = r.data;
    } catch {
      if (seq === detailSeq) detailError = x('error_network');
    } finally {
      if (seq === detailSeq) detailLoading = false;
    }
  }

  function select(id: string | null) {
    const url = new URL(page.url);
    if (id) url.searchParams.set('receipt', id);
    else url.searchParams.delete('receipt');
    void goto(url, { keepFocus: true, noScroll: true, replaceState: false });
  }

  function resetForIdentity() {
    listSeq++;
    detailSeq++;
    items = [];
    nextCursor = null;
    cursors = [null];
    listLoading = false;
    listError = '';
    listLoaded = false;
    retryList = null;
    detail = null;
    detailError = '';
    detailLoading = false;
  }

  $effect.pre(() => {
    const id = identity;
    if (seenIdentity !== null && id !== seenIdentity) untrack(resetForIdentity);
    seenIdentity = id;
  });

  $effect(() => {
    if (allowed && !listLoaded && !listLoading && !listError) untrack(() => void loadPage(null, () => (cursors = [null])));
  });

  $effect(() => {
    const id = selectedId;
    void identity;
    if (!allowed) return;
    untrack(() => {
      if (id) void loadDetail(id);
      else {
        detailSeq++;
        detail = null;
        detailError = '';
        detailLoading = false;
      }
    });
  });

  let retryingAuth = $state(false);
  async function retryAuth() {
    if (retryingAuth) return;
    retryingAuth = true;
    try {
      await auth.refresh();
    } finally {
      retryingAuth = false;
    }
  }

  function when(s: string): string {
    return new Date(s).toLocaleString();
  }

  function short(d: string): string {
    return d.replace(/^sha256:/, '').slice(0, 12);
  }
</script>

<svelte:head><title>{x('title')} - {site.name}</title></svelte:head>

<header class="mb-4">
  <a href="/admin/fields" class="text-sm text-fg-muted hover:underline">{x('back')}</a>
  <h2 class="text-2xl font-semibold">{x('title')}</h2>
  <p class="text-sm text-fg-muted">
    {x('intro')}
    <a href="/admin/fields/schema-exchange" class="text-accent hover:underline">{x('exchange_link')}</a>
  </p>
</header>

{#if !auth.ready}
  <p class="text-fg-muted" role="status" data-testid="import-history-loading">{x('loading')}</p>
{:else if auth.capsUnavailable}
  <div class="rounded border border-warning/40 bg-warning/10 p-3 text-sm" role="alert" data-testid="import-history-caps-unavailable">
    {x('caps_unavailable')}
    <button type="button" onclick={retryAuth} disabled={retryingAuth} class="ml-2 underline disabled:opacity-50">{x('retry')}</button>
  </div>
{:else if !allowed}
  <div class="rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger" role="alert" data-testid="import-history-denied">
    {x('denied')}
  </div>
{:else}
  {#if listError}
    <div class="mb-3 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger" role="alert" data-testid="import-history-error">
      {listError}
      {#if retryList}
        <button type="button" onclick={() => retryList?.()} disabled={listLoading} class="ml-2 underline" data-testid="import-history-retry">{x('retry')}</button>
      {/if}
    </div>
  {/if}
  <!-- shrink-0: the admin content column is a flex column, and an overflow box there
       would otherwise shrink below its rows. -->
  <section class="mb-6 shrink-0 overflow-x-auto rounded border border-border bg-bg-soft">
    <table class="w-full text-sm" data-testid="import-history-list" aria-busy={listLoading}>
      <caption class="sr-only">{x('title')}</caption>
      <thead class="border-b border-border bg-bg/60 text-fg-muted">
        <tr>
          <th class="px-3 py-2 text-left font-medium">{x('col_applied')}</th>
          <th class="px-3 py-2 text-left font-medium">{x('col_by')}</th>
          <th class="px-3 py-2 text-left font-medium">{x('col_source')}</th>
          <th class="px-3 py-2 text-left font-medium">{x('col_fields')}</th>
          <th class="px-3 py-2 text-left font-medium">{x('col_created')}</th>
          <th class="px-3 py-2 text-left font-medium">{x('col_document')}</th>
          <th class="px-3 py-2"><span class="sr-only">{x('col_open')}</span></th>
        </tr>
      </thead>
      <tbody>
        {#each items as r (r.receipt_id)}
          <tr class="border-b border-border/40 last:border-0" data-testid="import-history-row" data-receipt={r.receipt_id} class:bg-bg={selectedId === r.receipt_id}>
            <td class="px-3 py-2 text-fg-muted">{when(r.applied_at)}</td>
            <td class="px-3 py-2">{r.actor_ref === null ? x('removed_account') : x('user_ref', { ref: r.actor_ref })}</td>
            <td class="max-w-xs truncate px-3 py-2" title={r.source_label ?? ''}>{r.source_label ?? ''}</td>
            <td class="px-3 py-2">{r.field_count}</td>
            <td class="px-3 py-2">{r.changed}</td>
            <td class="px-3 py-2"><code class="text-xs" title={r.document_digest} aria-label={r.document_digest}>{short(r.document_digest)}</code></td>
            <td class="px-3 py-2">
              <button type="button" onclick={() => select(r.receipt_id)} class="rounded border border-border px-2 py-1 text-xs hover:bg-bg" data-testid="import-history-open">{x('details')}</button>
            </td>
          </tr>
        {/each}
        {#if listLoading && items.length === 0}
          <tr><td colspan="7" class="px-3 py-6 text-center text-fg-muted"><span role="status" data-testid="import-history-list-loading">{x('loading')}</span></td></tr>
        {:else if items.length === 0 && listLoaded && !listLoading}
          <tr><td colspan="7" class="px-3 py-6 text-center text-fg-muted" data-testid="import-history-empty">{cursors.length > 1 ? x('empty_past') : x('empty')}</td></tr>
        {/if}
      </tbody>
    </table>
    <div class="flex items-center justify-between gap-2 border-t border-border p-3 text-sm">
      <span class="text-fg-muted" aria-live="polite" data-testid="import-history-page">
        {x('page', { n: cursors.length })}{cursors.length >= MAX_DEPTH && nextCursor ? ' ' + x('page_cap', { max: MAX_DEPTH }) : ''}
      </span>
      <div class="flex gap-2">
        {#if cursors.length > 1}
          <button type="button" onclick={newest} disabled={listLoading} class="rounded border border-border px-3 py-1.5 text-fg-muted hover:bg-bg disabled:opacity-50" data-testid="import-history-newest">{x('newest')}</button>
        {/if}
        <button type="button" onclick={newer} disabled={listLoading || cursors.length < 2} class="rounded border border-border px-3 py-1.5 text-fg-muted hover:bg-bg disabled:opacity-50" data-testid="import-history-newer">{x('newer')}</button>
        <button type="button" onclick={older} disabled={listLoading || !nextCursor || cursors.length >= MAX_DEPTH} class="rounded border border-border px-3 py-1.5 text-fg-muted hover:bg-bg disabled:opacity-50" data-testid="import-history-older">{x('older')}</button>
      </div>
    </div>
  </section>

  {#if selectedId}
    <section class="rounded border border-border bg-bg-soft p-4" data-testid="import-history-detail" aria-busy={detailLoading}>
      <div class="mb-2 flex items-center justify-between">
        <h3 class="text-lg font-semibold">{x('receipt')} <code class="text-sm">{selectedId}</code></h3>
        <button type="button" onclick={() => select(null)} class="text-sm text-fg-muted underline">{x('close')}</button>
      </div>
      {#if detailLoading}
        <p class="text-fg-muted" role="status">{x('loading')}</p>
      {:else if detailError}
        <p class="text-danger" role="alert" data-testid="import-history-detail-error">
          {detailError}
          <button type="button" onclick={() => selectedId && loadDetail(selectedId)} class="ml-2 underline" data-testid="import-history-detail-retry">{x('retry')}</button>
        </p>
      {:else if detail}
        <p class="mb-3 rounded border border-border bg-bg p-2 text-sm" data-testid="import-history-past-note">{x('past_note')}</p>
        <dl class="mb-3 grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[max-content_1fr]">
          <dt class="text-fg-muted">{x('col_applied')}</dt><dd>{when(detail.applied_at)}</dd>
          <dt class="text-fg-muted">{x('col_by')}</dt><dd>{detail.actor_ref === null ? x('removed_account') : x('user_ref', { ref: detail.actor_ref })}</dd>
          <dt class="text-fg-muted">{x('format')}</dt><dd><code>{detail.format}</code></dd>
          <dt class="text-fg-muted">{x('source_label')}</dt><dd data-testid="import-history-source-label">{detail.source_label ?? x('none')}</dd>
          <dt class="text-fg-muted">{x('source_uri')}</dt><dd class="break-all" data-testid="import-history-source-uri">{detail.source_uri ?? x('none')}</dd>
          <dt class="text-fg-muted">{x('document_digest')}</dt><dd><code class="break-all text-xs">{detail.document_digest}</code></dd>
          <dt class="text-fg-muted">{x('decisions_digest')}</dt><dd><code class="break-all text-xs">{detail.decisions_digest}</code></dd>
          <dt class="text-fg-muted">{x('preview')}</dt><dd><code class="text-xs">{detail.preview_id}</code></dd>
          <dt class="text-fg-muted">{x('col_created')}</dt><dd>{detail.changed}</dd>
        </dl>
        <table class="w-full text-sm" data-testid="import-history-items">
          <caption class="mb-1 text-left text-fg-muted">{x('items_caption')}</caption>
          <thead class="text-fg-muted">
            <tr><th class="text-left">{x('code')}</th><th class="text-left">{x('decision')}</th><th class="text-left">{x('outcome')}</th><th class="text-left">{x('field_id_then')}</th><th class="text-left">{x('incoming_digest')}</th></tr>
          </thead>
          <tbody>
            {#each detail.fields as it, i (i)}
              <tr class="align-top" data-testid="import-history-item" data-outcome={it.outcome}>
                <td><code>{it.code}</code></td>
                <td><code>{it.decision}</code></td>
                <td><code>{it.outcome}</code> <span class="text-xs text-fg-muted">{x('outcome_' + it.outcome)}</span></td>
                <td><code class="text-xs">{it.field_id ?? x('none')}</code></td>
                <td><code class="break-all text-xs" data-testid="import-history-item-digest">{it.incoming_field_digest}</code></td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    </section>
  {/if}
{/if}
