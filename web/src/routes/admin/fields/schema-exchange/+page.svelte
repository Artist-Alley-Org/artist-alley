<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Admin: export and import portable field schemas (#1576 S4, ADR 0083).
  //
  // Export: the operator picks 1 to 100 active, non-mirrored fields, and
  // the browser saves the server's response exactly as it arrived.
  //
  // Import is three deliberate steps, and none of them guesses:
  //   1. preview  - the document text is sent as typed or read from the
  //                 file (exact UTF-8, bounded before it is read), never
  //                 re-encoded; schemaExchange.ts says why;
  //   2. review   - every incoming member of every field is shown, with
  //                 the differences, and one explicit choice is made per
  //                 code from the choices the preview allows;
  //   3. apply    - the request is built once and frozen.
  //
  // Two rules hold the page's state together:
  //   - An apply whose outcome is UNKNOWN (no usable answer) locks the
  //     page on that exact request: the document, file, source note,
  //     decisions and Preview are disabled, and only "retry the same
  //     request", the history link and a deliberate, acknowledged
  //     "discard and start over" remain. The server applies a request at
  //     most once and answers a repeat with the recorded receipt.
  //     Otherwise any edit or new preview discards the preview, its
  //     decisions, the frozen request and the previous result.
  //   - Everything on the page belongs to ONE signed-in operator. A
  //     change of account, a loss of rights or a sign-out clears it all
  //     and ignores every answer still in flight. An unknown outcome is
  //     never retried under another account; it is reported as possibly
  //     applied by the operator who sent it.
  import { onDestroy, onMount, untrack } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import { api } from '$api/client';
  import {
    DEAD_PREVIEW_REASONS,
    ENDPOINTS,
    EXPORT_MAX_CODES,
    PORTABLE_KEYS,
    PrepareError,
    SOURCE_LABEL_MAX_CHARS,
    allDecided,
    buildApplyBody,
    buildPreviewBody,
    checkSource,
    postText,
    readDocumentFile,
    refusalKey,
    type ExportOption,
    type ExportRefusal,
    type Preview,
    type Receipt,
    type Refusal,
  } from '$lib/admin/schemaExchange';

  const x = (k: string, v?: Record<string, string | number>) => t(`admin.fields.exchange.${k}`, v);

  type FieldRow = { code: string; label: string; type: string; mirrors_column?: string | null };
  type Problem = { path: string; problem: string };

  const allowed = $derived(auth.ready && !auth.capsUnavailable && auth.can('fields.admin'));
  // Whose page this is. Any change starts the page over.
  const identity = $derived(allowed ? `user:${auth.user?.ref ?? 'none'}` : 'none');
  let epoch = 0;
  let seenIdentity: string | null = null;

  // ---- export ----
  let fields = $state<FieldRow[]>([]);
  let fieldsLoaded = $state(false);
  let fieldsLoading = $state(false);
  let fieldsError = $state('');
  let fieldFilter = $state('');
  let selected = $state<string[]>([]);
  let exporting = $state(false);
  let exportError = $state('');
  let exportedName = $state('');

  const exportable = $derived(fields.filter((f) => !f.mirrors_column));
  const shown = $derived(
    exportable.filter((f) => {
      const q = fieldFilter.trim().toLowerCase();
      return q === '' || f.code.includes(q) || f.label.toLowerCase().includes(q);
    }),
  );
  const canExport = $derived(selected.length >= 1 && selected.length <= EXPORT_MAX_CODES && !exporting);

  async function loadFields() {
    if (fieldsLoading) return;
    const ep = epoch;
    fieldsLoading = true;
    fieldsError = '';
    try {
      const r = await api.GET('/fields', { params: { query: { status: 'active' } } });
      if (ep !== epoch) return;
      if (r.error || !r.data) {
        fieldsError = x('fields_failed');
        return;
      }
      fields = r.data as FieldRow[];
      fieldsLoaded = true;
    } catch {
      if (ep === epoch) fieldsError = x('fields_failed');
    } finally {
      if (ep === epoch) fieldsLoading = false;
    }
  }

  function toggle(code: string) {
    selected = selected.includes(code) ? selected.filter((c) => c !== code) : [...selected, code];
  }

  async function exportSelected() {
    if (!canExport || !allowed) return;
    const ep = epoch;
    exporting = true;
    exportError = '';
    exportedName = '';
    try {
      const res = await fetch(ENDPOINTS.export, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ codes: [...selected].sort() }),
      });
      if (ep !== epoch) return;
      if (res.status !== 200) {
        let detail = `HTTP ${res.status}`;
        try {
          const ref = (await res.json()) as Partial<ExportRefusal>;
          if (ref.reason) detail = `${ref.reason}${ref.codes?.length ? ': ' + ref.codes.join(', ') : ''}`;
        } catch {
          /* the status alone is all there is */
        }
        if (ep === epoch) exportError = x('export_refused', { detail });
        return;
      }
      // The bytes the server sent, unparsed.
      const blob = await res.blob();
      if (ep !== epoch) return;
      const url = URL.createObjectURL(blob);
      const name = `field-schema-${new Date().toISOString().slice(0, 10)}.json`;
      const a = document.createElement('a');
      a.href = url;
      a.download = name;
      document.body.appendChild(a);
      try {
        a.click();
      } finally {
        a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 0);
      }
      exportedName = name;
    } catch {
      if (ep === epoch) exportError = x('export_failed');
    } finally {
      if (ep === epoch) exporting = false;
    }
  }

  // ---- import ----
  let rawText = $state('');
  let fileName = $state('');
  let fileLoading = $state(false);
  let fileError = $state('');
  let sourceLabel = $state('');
  let sourceUri = $state('');

  // Every discarding change and every preview takes a new generation; an
  // answer is used only while both its generation and epoch are current.
  let generation = 0;
  // A file read is owned by its own token, not by `generation`: a
  // source-note edit discards the review but must not orphan the read
  // (which would leave the page loading forever). Only a newer file
  // choice or a change of operator takes the read away.
  let fileToken = 0;
  let previewing = $state(false);
  let previewError = $state('');
  let previewProblems = $state<Problem[]>([]);
  let preview = $state<Preview | null>(null);
  let decisions = $state<Record<string, string>>({});
  // The one apply request for this preview, once built.
  let frozen = $state<{ previewId: string; body: string; operator: string } | null>(null);
  // True from an apply with no usable answer until a definite answer.
  let unknown = $state(false);
  let ackAbandon = $state(false);
  let applying = $state(false);
  let applyError = $state('');
  let applyProblems = $state<Problem[]>([]);
  let previewDead = $state(false);
  let receipt = $state<Receipt | null>(null);
  // An unknown outcome that a change of account cut off.
  let lostUnknown = $state<{ operator: string } | null>(null);

  const locked = $derived(unknown || applying);
  const labelChars = $derived([...sourceLabel].length);

  let now = $state(Date.now());
  let ticker: ReturnType<typeof setInterval> | undefined;

  function resetForIdentity() {
    // An apply still in flight may commit just as surely as one whose
    // answer was lost: warn for both, before anything is cleared.
    if ((unknown || applying) && frozen) lostUnknown = { operator: frozen.operator };
    epoch++;
    generation++;
    fileToken++;
    fields = [];
    fieldsLoaded = false;
    fieldsLoading = false;
    fieldsError = '';
    fieldFilter = '';
    selected = [];
    exporting = false;
    exportError = '';
    exportedName = '';
    rawText = '';
    fileName = '';
    fileLoading = false;
    fileError = '';
    sourceLabel = '';
    sourceUri = '';
    discardReview();
  }

  function discardReview() {
    generation++;
    previewing = false;
    previewError = '';
    previewProblems = [];
    preview = null;
    decisions = {};
    frozen = null;
    unknown = false;
    ackAbandon = false;
    applying = false;
    applyError = '';
    applyProblems = [];
    previewDead = false;
    receipt = null;
  }

  $effect.pre(() => {
    const id = identity;
    if (seenIdentity !== null && id !== seenIdentity) untrack(resetForIdentity);
    seenIdentity = id;
  });

  // Inputs are controlled by hand rather than bound, so a locked page
  // puts the value back instead of letting an event change it.
  function onText(e: Event) {
    const el = e.currentTarget as HTMLTextAreaElement;
    if (locked || fileLoading) {
      el.value = rawText;
      return;
    }
    rawText = el.value;
    fileName = '';
    fileError = '';
    discardReview();
  }

  function onSource(which: 'label' | 'uri', e: Event) {
    const el = e.currentTarget as HTMLInputElement;
    if (locked) {
      el.value = which === 'label' ? sourceLabel : sourceUri;
      return;
    }
    if (which === 'label') sourceLabel = el.value;
    else sourceUri = el.value;
    discardReview();
  }

  async function chooseFile(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    const file = input.files?.[0];
    if (locked) {
      input.value = '';
      return;
    }
    if (!file) return;
    // The previous document is gone the moment another file is chosen.
    discardReview();
    rawText = '';
    fileName = '';
    fileError = '';
    fileLoading = true;
    const tok = ++fileToken;
    const ep = epoch;
    try {
      const text = await readDocumentFile(file);
      if (tok !== fileToken || ep !== epoch) return;
      rawText = text;
      fileName = file.name;
    } catch (err) {
      if (tok !== fileToken || ep !== epoch) return;
      fileError = err instanceof PrepareError ? x('prepare.' + err.key, err.vars) : x('prepare.file_unreadable');
    } finally {
      if (tok === fileToken && ep === epoch) fileLoading = false;
      input.value = '';
    }
  }

  async function runPreview() {
    if (locked || previewing || fileLoading || !allowed) return;
    discardReview();
    const gen = generation;
    const ep = epoch;
    let body: string;
    try {
      body = buildPreviewBody(rawText, checkSource(sourceLabel, sourceUri));
    } catch (err) {
      previewError = err instanceof PrepareError ? x('prepare.' + err.key, err.vars) : x('prepare.document_not_json');
      return;
    }
    previewing = true;
    const r = await postText(ENDPOINTS.preview, body);
    if (gen !== generation || ep !== epoch) return;
    previewing = false;
    if (r.kind === 'ambiguous') {
      previewError = x('preview_no_answer', { status: r.status ?? 'network', detail: r.detail });
      return;
    }
    if (r.status === 200) {
      preview = r.body as Preview;
      return;
    }
    const ref = r.body as Partial<Refusal>;
    previewError = `${t(refusalKey(ref.reason ?? ''))} (${ref.reason ?? 'HTTP ' + r.status})`;
    previewProblems = ref.problems ?? [];
  }

  function decide(code: string, choice: string) {
    if (frozen || locked || previewDead) return;
    decisions = { ...decisions, [code]: choice };
  }

  const ready = $derived(preview !== null && allDecided(preview.fields, decisions));
  const expired = $derived(preview !== null && Date.parse(preview.expires_at) <= now);
  const decisionCounts = $derived.by(() => {
    const c: Record<string, number> = {};
    for (const v of Object.values(decisions)) c[v] = (c[v] ?? 0) + 1;
    return c;
  });

  async function runApply() {
    if (!preview || applying || !allowed) return;
    if (!frozen) {
      if (!ready || previewDead) return;
      frozen = {
        previewId: preview.preview_id,
        body: buildApplyBody(preview, decisions),
        operator: auth.user?.username ?? String(auth.user?.ref ?? ''),
      };
    }
    const ep = epoch;
    const sent = frozen;
    applying = true;
    applyError = '';
    applyProblems = [];
    const r = await postText(ENDPOINTS.apply, sent.body);
    if (ep !== epoch) return;
    applying = false;
    if (r.kind === 'ambiguous') {
      // Keep `frozen`: the retry must be this same request.
      unknown = true;
      applyError = x('apply_unknown', { status: r.status ?? 'network', detail: r.detail });
      return;
    }
    if (r.status === 200) {
      receipt = r.body as Receipt;
      frozen = null;
      unknown = false;
      previewDead = true;
      return;
    }
    const ref = r.body as Partial<Refusal>;
    const reason = ref.reason ?? '';
    const definite = reason !== '' && r.status !== 401 && r.status !== 403 && r.status !== 429;
    applyError = `${reason ? t(refusalKey(reason)) : x('apply_refused_status', { status: r.status })} (${reason || 'HTTP ' + r.status})`;
    applyProblems = ref.problems ?? [];
    if (!definite) {
      // Not processed (signed out, refused rights, rate limited): the
      // same request stays ready, and an unknown outcome stays unknown.
      return;
    }
    unknown = false;
    if (DEAD_PREVIEW_REASONS.has(reason)) {
      previewDead = true;
      frozen = null;
    } else {
      // Refused before any change: the decisions may be revised.
      frozen = null;
    }
  }

  function abandon() {
    if (!ackAbandon || applying) return;
    discardReview();
  }

  function valueText(v: unknown): string {
    return v === undefined ? x('absent_value') : JSON.stringify(v);
  }

  onMount(() => {
    ticker = setInterval(() => (now = Date.now()), 5000);
  });
  onDestroy(() => clearInterval(ticker));

  $effect(() => {
    if (allowed && !fieldsLoaded && !fieldsLoading && !fieldsError) void untrack(loadFields);
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
</script>

{#snippet optionTree(opts: ExportOption[])}
  <ul class="list-disc pl-5">
    {#each opts as o, i (i)}
      <li>
        <code>{o.value}</code>
        {#if o.label === null}<span class="text-fg-muted">label <code>null</code></span>{:else}<span>{o.label}</span>{/if}
        {#if o.children.length}{@render optionTree(o.children)}{/if}
      </li>
    {/each}
  </ul>
{/snippet}

<svelte:head><title>{x('title')} - {site.name}</title></svelte:head>

<header class="mb-4">
  <a href="/admin/fields" class="text-sm text-fg-muted hover:underline">{x('back')}</a>
  <h2 class="text-2xl font-semibold">{x('title')}</h2>
  <p class="text-sm text-fg-muted">
    {x('intro')}
    <a href="/admin/fields/import-history" class="text-accent hover:underline">{x('history_link')}</a>
  </p>
</header>

{#if !auth.ready}
  <p class="text-fg-muted" role="status" data-testid="schema-exchange-loading">{x('loading')}</p>
{:else if auth.capsUnavailable}
  <div class="rounded border border-warning/40 bg-warning/10 p-3 text-sm" role="alert" data-testid="schema-exchange-caps-unavailable">
    {x('caps_unavailable')}
    <button type="button" onclick={retryAuth} disabled={retryingAuth} class="ml-2 underline disabled:opacity-50" data-testid="schema-exchange-caps-retry">{x('retry')}</button>
  </div>
{:else if !allowed}
  <div class="rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger" role="alert" data-testid="schema-exchange-denied">
    {x('denied')}
  </div>
{:else}
  {#if lostUnknown}
    <div class="mb-4 rounded border border-warning/40 bg-warning/10 p-3 text-sm" role="alert" data-testid="schema-lost-unknown">
      {x('lost_unknown', { operator: lostUnknown.operator })}
      <a href="/admin/fields/import-history" class="text-accent underline">{x('history_link')}</a>
      <button type="button" onclick={() => (lostUnknown = null)} class="ml-2 underline">{x('dismiss')}</button>
    </div>
  {/if}

  <section class="mb-8 rounded border border-border bg-bg-soft p-4" data-testid="schema-export">
    <h3 class="mb-1 text-lg font-semibold">{x('export_heading')}</h3>
    <p class="mb-3 text-sm text-fg-muted">{x('export_help', { max: EXPORT_MAX_CODES })}</p>
    {#if fieldsError}
      <div class="mb-3 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger" role="alert" data-testid="schema-export-fields-error">
        {fieldsError}
        <button type="button" onclick={loadFields} disabled={fieldsLoading} class="ml-2 underline" data-testid="schema-export-fields-retry">{x('retry')}</button>
      </div>
    {/if}
    <div class="mb-2 flex flex-wrap items-center gap-3 text-sm">
      <input
        type="search"
        bind:value={fieldFilter}
        placeholder={x('filter_placeholder')}
        aria-label={x('filter_label')}
        class="rounded border border-border-strong bg-bg p-1.5 text-fg"
      />
      <span data-testid="schema-export-count" aria-live="polite" class:text-danger={selected.length > EXPORT_MAX_CODES}>
        {x('selected_count', { n: selected.length })}{selected.length > EXPORT_MAX_CODES ? ' ' + x('selected_too_many', { max: EXPORT_MAX_CODES }) : ''}
      </span>
      {#if selected.length}
        <button type="button" onclick={() => (selected = [])} class="text-fg-muted underline">{x('clear_selection')}</button>
      {/if}
    </div>
    <ul class="mb-3 max-h-72 overflow-y-auto rounded border border-border bg-bg p-2 text-sm" data-testid="schema-export-fields">
      {#each shown as f (f.code)}
        <li>
          <label class="flex items-center gap-2 py-0.5">
            <input type="checkbox" checked={selected.includes(f.code)} onchange={() => toggle(f.code)} data-testid="schema-export-pick" value={f.code} />
            <code>{f.code}</code> <span class="text-fg-muted">{f.label} ({f.type})</span>
          </label>
        </li>
      {:else}
        <li class="text-fg-muted" role="status">{fieldsLoaded ? x('no_exportable') : fieldsError ? '' : x('loading_fields')}</li>
      {/each}
    </ul>
    <button
      type="button"
      onclick={exportSelected}
      disabled={!canExport}
      data-testid="schema-export-button"
      class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
    >{exporting ? x('exporting') : x('export_button')}</button>
    {#if exportError}
      <p class="mt-2 text-sm text-danger" role="alert" data-testid="schema-export-error">{exportError}</p>
    {/if}
    {#if exportedName}
      <p class="mt-2 text-sm text-success" role="status" data-testid="schema-export-done">{x('export_saved', { name: exportedName })}</p>
    {/if}
  </section>

  <section class="mb-8 rounded border border-border bg-bg-soft p-4" data-testid="schema-import">
    <h3 class="mb-1 text-lg font-semibold">{x('import_heading')}</h3>
    <p class="mb-3 text-sm text-fg-muted">{x('import_help')}</p>
    {#if locked && unknown}
      <p class="mb-3 text-sm text-warning" role="status" data-testid="schema-import-locked">{x('locked_note')}</p>
    {/if}
    <div class="grid gap-3">
      <label class="flex flex-col gap-1 text-sm">
        <span class="text-fg-muted">{x('file_label')}</span>
        <input type="file" accept="application/json,.json" onchange={chooseFile} disabled={locked || fileLoading} data-testid="schema-import-file" />
        {#if fileLoading}<span class="text-xs text-fg-muted" role="status" data-testid="schema-import-file-loading">{x('file_loading')}</span>{/if}
        {#if fileName}<span class="text-xs text-fg-muted">{x('file_read', { name: fileName })}</span>{/if}
        {#if fileError}<span class="text-xs text-danger" role="alert" data-testid="schema-import-file-error">{fileError}</span>{/if}
      </label>
      <label class="flex flex-col gap-1 text-sm">
        <span class="text-fg-muted">{x('text_label')}</span>
        <textarea
          value={rawText}
          oninput={onText}
          disabled={locked || fileLoading}
          rows="8"
          spellcheck="false"
          class="rounded border border-border-strong bg-bg p-2 font-mono text-xs text-fg"
          data-testid="schema-import-text"
        ></textarea>
      </label>
      <div class="grid gap-3 sm:grid-cols-2">
        <label class="flex flex-col gap-1 text-sm">
          <span class="text-fg-muted">{x('source_label_label', { max: SOURCE_LABEL_MAX_CHARS })}</span>
          <input value={sourceLabel} oninput={(e) => onSource('label', e)} disabled={locked} class="rounded border border-border-strong bg-bg p-2 text-fg" data-testid="schema-import-source-label" />
          <span class="text-xs text-fg-muted" class:text-danger={labelChars > SOURCE_LABEL_MAX_CHARS}>{x('source_label_count', { n: labelChars, max: SOURCE_LABEL_MAX_CHARS })}</span>
        </label>
        <label class="flex flex-col gap-1 text-sm">
          <span class="text-fg-muted">{x('source_uri_label')}</span>
          <input value={sourceUri} oninput={(e) => onSource('uri', e)} disabled={locked} class="rounded border border-border-strong bg-bg p-2 text-fg" data-testid="schema-import-source-uri" />
        </label>
      </div>
    </div>
    <button
      type="button"
      onclick={runPreview}
      disabled={previewing || locked || fileLoading || rawText === ''}
      data-testid="schema-import-preview-button"
      class="mt-3 rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
    >{previewing ? x('previewing') : x('preview_button')}</button>
    {#if previewError}
      <div class="mt-3 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger" role="alert" data-testid="schema-import-preview-error">
        {previewError}
        {#if previewProblems.length}
          <ul class="mt-1 list-disc pl-5">
            {#each previewProblems as p, i (i)}<li><code>{p.path}</code>: {p.problem}</li>{/each}
          </ul>
        {/if}
      </div>
    {/if}
  </section>

  {#if preview}
    <section class="mb-8 rounded border border-border bg-bg-soft p-4" data-testid="schema-import-preview">
      <h3 class="mb-2 text-lg font-semibold">{x('preview_heading')}</h3>
      <dl class="mb-3 grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[max-content_1fr]">
        <dt class="text-fg-muted">{x('source_label_short')}</dt>
        <dd data-testid="schema-preview-source-label">{preview.source?.label ?? x('none')}</dd>
        <dt class="text-fg-muted">{x('source_uri_short')}</dt>
        <dd class="break-all" data-testid="schema-preview-source-uri">{preview.source?.uri ?? x('none')}</dd>
        <dt class="text-fg-muted">{x('document_digest')}</dt>
        <dd><code class="break-all text-xs" data-testid="schema-preview-digest">{preview.document_digest}</code></dd>
        <dt class="text-fg-muted">{x('expires')}</dt>
        <dd data-testid="schema-preview-expiry">
          {new Date(preview.expires_at).toLocaleString()}
          {#if expired}<span class="ml-1 text-warning" data-testid="schema-preview-expired">{x('expired_clock')}</span>{/if}
        </dd>
      </dl>
      <p class="mb-3 rounded border border-border bg-bg p-2 text-sm" data-testid="schema-preview-advisory">{preview.advisory}</p>

      {#if preview.unresolved_terms.length}
        <div class="mb-3 text-sm" data-testid="schema-preview-unresolved">
          <h4 class="font-medium">{x('unresolved_heading')}</h4>
          <ul class="list-disc pl-5">
            {#each preview.unresolved_terms as u, i (i)}<li><code>{u.field}</code>: <code>{u.term}</code></li>{/each}
          </ul>
        </div>
      {/if}

      <div class="grid gap-3">
        {#each preview.fields as f (f.code)}
          <article class="rounded border border-border bg-bg p-3 text-sm" data-testid="schema-preview-field" data-code={f.code} data-local={f.local}>
            <header class="mb-2 flex flex-wrap items-baseline gap-2">
              <code class="font-semibold">{f.code}</code>
              <span class="rounded border border-border px-1.5 text-xs">{f.local}</span>
              <span class="text-xs text-fg-muted">{x('local.' + f.local)}</span>
              {#if f.local_status}<span class="text-xs text-fg-muted">{x('local_status', { status: f.local_status })}</span>{/if}
              <span class="text-xs text-fg-muted">{x('incoming_summary', { label: f.incoming.label, type: f.incoming.type, subject: f.incoming.subject_kind })}</span>
            </header>
            {#if f.problem}<p class="mb-2 text-danger">{f.problem}</p>{/if}
            {#if f.differences.length}
              <table class="mb-2 w-full text-xs" data-testid="schema-preview-differences">
                <thead class="text-fg-muted"><tr><th class="text-left">{x('property')}</th><th class="text-left">{x('local_side')}</th><th class="text-left">{x('incoming_side')}</th></tr></thead>
                <tbody>
                  {#each f.differences as d (d.property)}
                    <tr class="align-top">
                      <td class="pr-2"><code>{d.property}</code></td>
                      <td class="pr-2"><code class="break-all">{valueText(d.local)}</code></td>
                      <td>
                        <code class="break-all">{valueText(d.incoming)}</code>
                        {#if d.terms_only_incoming?.length}<div>{x('terms_only_incoming', { terms: d.terms_only_incoming.join(', ') })}</div>{/if}
                        {#if d.terms_only_local?.length}<div>{x('terms_only_local', { terms: d.terms_only_local.join(', ') })}</div>{/if}
                        {#if d.terms_changed?.length}<div>{x('terms_changed', { terms: d.terms_changed.join(', ') })}</div>{/if}
                      </td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            {/if}
            <details class="mb-2" open={f.local === 'absent'}>
              <summary class="cursor-pointer text-fg-muted">{x('incoming_heading')}</summary>
              <dl class="mt-1 grid gap-x-3 gap-y-0.5 text-xs sm:grid-cols-[max-content_1fr]" data-testid="schema-preview-incoming">
                {#each PORTABLE_KEYS as k (k)}
                  <dt><code>{k}</code></dt>
                  <dd class="break-all">
                    {#if k === 'options' && Array.isArray(f.incoming.options)}
                      {#if f.incoming.options.length}{@render optionTree(f.incoming.options)}{:else}<code>[]</code>{/if}
                    {:else}
                      <code>{valueText(f.incoming[k])}</code>
                    {/if}
                  </dd>
                {/each}
              </dl>
            </details>
            <fieldset class="flex flex-col gap-1" disabled={frozen !== null || locked || previewDead}>
              <legend class="text-fg-muted">{x('decision_legend', { code: f.code })}</legend>
              {#each f.decisions as choice (choice)}
                <label class="flex items-start gap-2">
                  <input
                    type="radio"
                    name={'decision-' + f.code}
                    value={choice}
                    checked={decisions[f.code] === choice}
                    onchange={() => decide(f.code, choice)}
                    data-testid="schema-decision"
                    data-choice={choice}
                  />
                  <span><span data-testid={'schema-decision-label-' + choice}>{x('decision.' + choice)}</span> <code class="text-xs text-fg-muted">{choice}</code></span>
                </label>
              {/each}
            </fieldset>
          </article>
        {/each}
      </div>

      {#if !previewDead}
        <div class="mt-3 rounded border border-border bg-bg p-3 text-sm" data-testid="schema-apply-summary">
          <p>
            {#each Object.entries(decisionCounts) as [d, n] (d)}<span class="mr-3"><code>{d}</code>: {n}</span>{/each}
            <span class="text-fg-muted">{x('decided_of', { n: Object.keys(decisions).length, total: preview.fields.length })}</span>
          </p>
          <p class="text-fg-muted">{x('local_stays_local')}</p>
        </div>
        <div class="mt-3 flex flex-wrap items-center gap-3">
          {#if ready || frozen}
            <button
              type="button"
              onclick={runApply}
              disabled={applying}
              data-testid="schema-apply-button"
              class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
            >{applying ? x('applying') : frozen && applyError ? x('retry_same') : x('apply_button')}</button>
          {:else}
            <span class="text-sm text-fg-muted" data-testid="schema-apply-pending">{x('choose_all')}</span>
          {/if}
        </div>
      {/if}
      {#if applyError}
        <div
          class="mt-3 rounded border p-3 text-sm {unknown ? 'border-warning/40 bg-warning/10' : 'border-danger/40 bg-danger/10 text-danger'}"
          role="alert"
          data-testid="schema-apply-error"
          data-ambiguous={unknown}
        >
          {applyError}
          {#if applyProblems.length}
            <ul class="mt-1 list-disc pl-5">
              {#each applyProblems as p, i (i)}<li><code>{p.path}</code>: {p.problem}</li>{/each}
            </ul>
          {/if}
          {#if unknown}
            <p class="mt-2">
              <a href="/admin/fields/import-history" class="text-accent underline" data-testid="schema-apply-history-link">{x('check_history')}</a>
            </p>
            <div class="mt-2 flex flex-wrap items-center gap-2">
              <label class="flex items-center gap-1">
                <input type="checkbox" bind:checked={ackAbandon} disabled={applying} data-testid="schema-abandon-ack" />
                {x('abandon_ack')}
              </label>
              <button type="button" onclick={abandon} disabled={!ackAbandon || applying} class="underline disabled:opacity-50" data-testid="schema-abandon">{x('abandon')}</button>
            </div>
          {/if}
        </div>
      {/if}
    </section>
  {/if}

  {#if receipt}
    <section class="rounded border border-border bg-bg-soft p-4" data-testid="schema-apply-receipt" data-replayed={receipt.replayed}>
      <h3 class="mb-2 text-lg font-semibold">{x('result_heading')}</h3>
      {#if receipt.replayed}
        <p class="mb-2 text-sm" data-testid="schema-receipt-replayed">{x('result_replayed')}</p>
      {/if}
      <dl class="mb-2 grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[max-content_1fr]">
        <dt class="text-fg-muted">{x('receipt')}</dt><dd><code class="text-xs" data-testid="schema-receipt-id">{receipt.receipt_id}</code></dd>
        <dt class="text-fg-muted">{x('applied_at')}</dt><dd>{new Date(receipt.applied_at).toLocaleString()}</dd>
        <dt class="text-fg-muted">{x('document_digest')}</dt><dd><code class="break-all text-xs">{receipt.document_digest}</code></dd>
        <dt class="text-fg-muted">{x('decisions_digest')}</dt><dd><code class="break-all text-xs">{receipt.decisions_digest}</code></dd>
        <dt class="text-fg-muted">{x('created_count')}</dt><dd data-testid="schema-receipt-changed">{receipt.changed}</dd>
      </dl>
      <p class="mb-2 text-sm text-fg-muted">
        {x('result_meaning')}
        <a href={'/admin/fields/import-history?receipt=' + receipt.receipt_id} class="text-accent hover:underline">{x('view_in_history')}</a>
      </p>
      <table class="w-full text-sm">
        <thead class="text-fg-muted"><tr><th class="text-left">{x('code')}</th><th class="text-left">{x('decision_col')}</th><th class="text-left">{x('outcome_col')}</th><th class="text-left">{x('field_id_col')}</th></tr></thead>
        <tbody>
          {#each receipt.fields as it (it.code)}
            <tr data-testid="schema-receipt-item" data-outcome={it.outcome}>
              <td><code>{it.code}</code></td>
              <td><code>{it.decision}</code></td>
              <td><code>{it.outcome}</code> <span class="text-xs text-fg-muted">{x('outcome.' + it.outcome)}</span></td>
              <td><code class="text-xs">{it.field_id ?? x('none')}</code></td>
            </tr>
          {/each}
        </tbody>
      </table>
    </section>
  {/if}
{/if}
