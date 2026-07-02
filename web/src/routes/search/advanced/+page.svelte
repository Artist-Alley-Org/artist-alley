<script lang="ts">
  // /search/advanced — visual DSL builder (Phase 1.16.B-2).
  //
  // Row-based UX: each row is (field, value); rows AND together.
  // Live preview of the compiled DSL string. Submit → /search?q=<DSL>.
  //
  // Field whitelist mirrors app/internal/search/dsl/parser.go
  // Field enum; unknown fields on submit are rejected by the
  // backend parser with a 400 + valid-field list.

  import { goto } from '$app/navigation';

  const FIELDS = [
    { value: 'title',       label: 'Title' },
    { value: 'description', label: 'Description / body' },
    { value: 'tag',         label: 'Tag' },
    { value: 'owner',       label: 'Owner (username or ref)' },
    { value: 'type',        label: 'Asset type' },
    { value: 'sensitivity', label: 'Sensitivity' },
    { value: 'extension',   label: 'File extension' },
    // Phase 1.16.B-3 — vector similarity anchor. Value is a
    // UUID; the backend resolves it to the anchor's stored
    // embedding.
    { value: 'similar_to',  label: 'Similar to asset (UUID)' },
  ];

  type Row = { field: string; value: string; not: boolean };
  let rows = $state<Row[]>([{ field: 'title', value: '', not: false }]);
  let freeText = $state('');

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
    const q = compiled;
    if (!q) return;
    // Route through the ?dsl= query param so the backend runs
    // the DSL parser + resolves any similar_to:<uuid> anchors.
    void goto(`/search?dsl=${encodeURIComponent(q)}`);
  }
</script>

<svelte:head><title>Advanced search — artist-alley</title></svelte:head>

<div class="mx-auto w-full max-w-3xl px-6 py-8">
  <h1 class="font-display mb-4 text-3xl font-semibold">Advanced search</h1>

  <p class="mb-4 text-sm text-fg-muted">
    Build a query with typed fields. Each row is combined with AND;
    the preview below shows exactly what will be submitted to the
    server-side DSL parser.
  </p>

  <form onsubmit={submit} class="space-y-3">
    <label class="block">
      <span class="mb-1 block text-xs font-medium text-fg-muted">Free-text query (optional)</span>
      <input
        bind:value={freeText}
        type="text"
        placeholder="cat OR dog"
        class="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
      />
    </label>

    <div class="space-y-2" data-testid="advanced-rows">
      {#each rows as row, i (i)}
        <div class="flex items-center gap-2">
          <label class="flex items-center gap-1 text-xs text-fg-muted">
            <input type="checkbox" bind:checked={row.not} class="h-3.5 w-3.5" /> NOT
          </label>
          <select
            bind:value={row.field}
            class="rounded-md border border-border bg-surface px-2 py-1.5 text-sm"
          >
            {#each FIELDS as f (f.value)}
              <option value={f.value}>{f.label}</option>
            {/each}
          </select>
          <input
            bind:value={row.value}
            type="text"
            placeholder="value"
            class="flex-1 rounded-md border border-border bg-surface px-3 py-1.5 text-sm"
            data-testid="advanced-row-value"
          />
          <button
            type="button"
            onclick={() => removeRow(i)}
            class="rounded-md border border-border bg-surface px-2 py-1.5 text-sm text-fg-muted hover:bg-surface-elevated"
            aria-label="Remove row"
          >×</button>
        </div>
      {/each}
    </div>

    <button
      type="button"
      onclick={addRow}
      class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:border-border-strong"
    >+ Add row</button>

    <div class="mt-6 rounded-md border border-border bg-surface p-3">
      <div class="mb-1 text-xs font-medium text-fg-muted">Compiled DSL</div>
      <code class="block font-mono text-sm text-fg" data-testid="advanced-compiled">{compiled || '(empty)'}</code>
    </div>

    <div class="flex justify-end">
      <button
        type="submit"
        disabled={!compiled}
        class="rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent disabled:opacity-50"
      >Search</button>
    </div>
  </form>
</div>
