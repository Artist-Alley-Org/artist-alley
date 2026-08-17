<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The visual DSL builder (Phase 1.16.B-2), now a PANEL rather than a
  // page (#850).
  //
  // It used to be /search/advanced — its own route, its own destination,
  // its own empty state. That made "advanced" a separate MODE of
  // searching: you left the results you were looking at, built a query
  // somewhere else, and came back to a different page. The builder is
  // not a different kind of search; it is a different way of composing
  // the same query, so it lives inside /search and is hidden until
  // asked for.
  //
  // Row-based UX unchanged: each row is (field, value); rows AND
  // together; live preview of the compiled DSL. The field whitelist
  // mirrors app/internal/search/dsl/parser.go's Field enum — unknown
  // fields are rejected by the backend parser with a 400 + valid-field
  // list, so this list going stale is loud rather than silent.

  import { t } from '$stores/lang.svelte';
  import ReverseImageDropzone from '$components/search/ReverseImageDropzone.svelte';

  interface Props {
    /** Called with the compiled DSL string when the builder is
     *  submitted. The host owns what happens next — on /search that is
     *  re-running the query in place rather than navigating away. */
    onsubmit: (dsl: string) => void;
    /** Seeds the free-text row (#1157). The advanced PAGE is reached
     *  from the nav box carrying `?q=`, and dropping what the caller had
     *  already typed is the same trap the nav control's own doc warns
     *  about. Default empty keeps the slide-over host unchanged. */
    initialFreeText?: string;
    /** Whether to render the reverse-image dropzone inside this panel
     *  (#1157). True for the slide-over, where this component IS the
     *  whole advanced surface. False for the advanced PAGE, which gives
     *  search-by-image a section of its own so it can be hidden when the
     *  instance has no CLIP channel — rendering it in both places put
     *  two dropzones on that page. */
    showImageSearch?: boolean;
  }
  let { onsubmit, initialFreeText = '', showImageSearch = true }: Props = $props();

  const FIELDS = [
    { value: 'title',       labelKey: 'search.advanced.field.title' },
    { value: 'description', labelKey: 'search.advanced.field.description' },
    { value: 'tag',         labelKey: 'search.advanced.field.tag' },
    { value: 'owner',       labelKey: 'search.advanced.field.owner' },
    { value: 'type',        labelKey: 'search.advanced.field.type' },
    { value: 'sensitivity', labelKey: 'search.advanced.field.sensitivity' },
    { value: 'extension',   labelKey: 'search.advanced.field.extension' },
    // Phase 1.16.B-3 — vector similarity anchor. Value is a UUID; the
    // backend resolves it to the anchor's stored embedding.
    { value: 'similar_to',  labelKey: 'search.advanced.field.similar_to' },
  ];

  type Row = { field: string; value: string; not: boolean };
  let rows = $state<Row[]>([{ field: 'title', value: '', not: false }]);
  let freeText = $state(initialFreeText);

  const compiled = $derived.by(() => {
    const parts: string[] = [];
    const ft = freeText.trim();
    if (ft) {
      parts.push(ft);
    }
    for (const r of rows) {
      const v = r.value.trim();
      if (!v) continue;
      const needsQuotes = /\s/.test(v);
      const qv = needsQuotes ? `"${v.replace(/"/g, '\\"')}"` : v;
      parts.push(`${r.not ? 'NOT ' : ''}${r.field}:${qv}`);
    }
    return parts.join(' AND ');
  });

  function addRow() {
    rows = [...rows, { field: 'title', value: '', not: false }];
  }

  function removeRow(i: number) {
    rows = rows.filter((_, idx) => idx !== i);
    if (rows.length === 0) rows = [{ field: 'title', value: '', not: false }];
  }

  function submit(e: Event) {
    e.preventDefault();
    if (!compiled) return;
    onsubmit(compiled);
  }
</script>

<p class="mb-4 text-sm text-fg-muted">{t('search.advanced.body')}</p>

<!-- Reverse-image search sits above the DSL builder — a parallel search
     mode in the same panel (Phase 1.55.W). Suppressed when the host
     renders its own arm (#1157). -->
{#if showImageSearch}
  <ReverseImageDropzone />
{/if}

<form onsubmit={submit} class="space-y-3">
  <label class="block">
    <span class="mb-1 block text-xs font-medium text-fg-muted">{t('search.advanced.freetext_label')}</span>
    <input
      bind:value={freeText}
      type="text"
      placeholder={t('search.advanced.freetext_placeholder')}
      class="w-full rounded-md border border-border-strong bg-surface px-3 py-2 text-sm"
    />
  </label>

  <div class="space-y-2" data-testid="advanced-rows">
    {#each rows as row, i (i)}
      <!-- Wraps at narrow widths (#901/#903). The row used to be one
           unwrapping flex line whose select + input + button could not
           fit inside 390px, which is one of the two things that made the
           page scroll sideways on a phone. -->
      <div class="flex flex-wrap items-center gap-2">
        <label class="flex min-h-11 items-center gap-1 text-xs text-fg-muted">
          <input type="checkbox" bind:checked={row.not} class="h-4 w-4" /> {t('search.advanced.not')}
        </label>
        <select
          bind:value={row.field}
          class="min-h-11 rounded-md border border-border-strong bg-surface px-2 py-1.5 text-sm"
        >
          {#each FIELDS as f (f.value)}
            <option value={f.value}>{t(f.labelKey)}</option>
          {/each}
        </select>
        <!-- `basis-full` under sm: at 390px the NOT checkbox, the field
             select and the value input cannot share a line without the
             value shrinking to about three characters (measured). It
             drops to its own row instead. -->
        <input
          bind:value={row.value}
          type="text"
          placeholder={t('search.advanced.value_placeholder')}
          class="order-last min-h-11 w-full min-w-0 basis-full rounded-md border border-border-strong bg-surface
                 px-3 py-1.5 text-sm sm:order-none sm:w-auto sm:flex-1 sm:basis-auto"
          data-testid="advanced-row-value"
        />
        <button
          type="button"
          onclick={() => removeRow(i)}
          class="min-h-11 min-w-11 rounded-md border border-border bg-surface px-2 py-1.5 text-sm text-fg-muted hover:bg-surface-elevated"
          aria-label={t('search.advanced.remove_row')}
        >×</button>
      </div>
    {/each}
  </div>

  <button
    type="button"
    onclick={addRow}
    class="min-h-11 rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong"
  >{t('search.advanced.add_row')}</button>

  <!-- The preview of what will run. It stays (it is how someone learns
       what the rows above add up to, and it is copy-pasteable), but
       #1191 took the developer words off its label: an artist reading
       "Compiled DSL" learns nothing they can act on. -->
  <div class="mt-6 overflow-x-auto rounded-md border border-border bg-surface p-3">
    <div class="mb-1 text-xs font-medium text-fg-muted">{t('search.advanced.preview_label')}</div>
    <code class="block font-mono text-sm text-fg" data-testid="advanced-compiled">{compiled || t('search.advanced.preview_empty')}</code>
  </div>

  <div class="flex justify-end">
    <button
      type="submit"
      disabled={!compiled}
      class="min-h-11 rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent disabled:opacity-50"
    >{t('common.search')}</button>
  </div>
</form>
