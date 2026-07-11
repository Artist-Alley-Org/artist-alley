<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // DocTool body — the document viewer's side-panel surface.
  // Binds the shared DocSession that DocView also binds; both ends
  // mutate the same $state object so flipping wrap / theme / font
  // / search in the panel updates the editor without an event bus.
  //
  // Sections (top → bottom):
  //   1. Reading    — font family, size, line-height, theme, wrap,
  //                   line numbers, tab width, whitespace markers,
  //                   render-as-markdown toggle (for .md only)
  //   2. Outline    — markdown headings or code symbols; click
  //                   jumps the editor to that line
  //   3. Find       — query + flags (case / regex / word), prev /
  //                   next; Replace pane (UI scaffold for Phase B)
  //   4. Bookmarks  — line + optional note (localStorage per-asset,
  //                   user-private; same shape as EbookTool)
  //   5. Stats      — language, lines, words, chars, file size
  //   6. Annotations · coming soon — Phase B teaser

  import type { ToolContext } from '../contract';
  import type { DocTheme, DocFontFamily, DocAnnotation } from '$lib/doc/session.svelte';

  let { ctx }: { ctx: ToolContext } = $props();
  const session = $derived(ctx.docSession);

  const THEMES: { id: DocTheme; label: string; swatch: string }[] = [
    { id: 'light', label: 'Light', swatch: '#ffffff' },
    { id: 'sepia', label: 'Sepia', swatch: '#f4ecd8' },
    { id: 'dark',  label: 'Dark',  swatch: '#1a1a1a' },
  ];
  const FONTS: { id: DocFontFamily; label: string }[] = [
    { id: 'sans',  label: 'Sans' },
    { id: 'serif', label: 'Serif' },
    { id: 'mono',  label: 'Mono' },
  ];

  let bookmarkNote = $state('');
  function addBookmark() {
    if (!session) return;
    session.addBookmark(bookmarkNote);
    bookmarkNote = '';
  }

  let replaceOpen = $state(false);

  // ── Annotations panel state ───────────────────────────────────
  const FILTERS: { id: DocAnnotation['style']; label: string }[] = [
    { id: 'highlight',     label: 'Highlight' },
    { id: 'strikethrough', label: 'Strike' },
    { id: 'underline',     label: 'Under' },
    { id: 'comment',       label: 'Comment' },
    { id: 'note',          label: 'Note' },
  ];
  const HIGHLIGHT_SWATCHES = ['#fef08a', '#bef264', '#7dd3fc', '#f9a8d4', '#fca5a5'];

  const visibleAnnotations = $derived(
    (session?.annotations ?? []).filter((a) =>
      (session?.annotationsFilter === null || a.style === session?.annotationsFilter)
      && (session?.annotationsShowResolved || !a.resolved),
    ),
  );

  let editingId = $state<string | null>(null);
  let editDraft = $state('');
  let editColor = $state(HIGHLIGHT_SWATCHES[0]);
  function beginEdit(a: DocAnnotation) {
    editingId = a.id;
    editDraft = a.body;
    editColor = a.color;
  }
  function cancelEdit() {
    editingId = null;
    editDraft = '';
  }
  async function saveEdit(a: DocAnnotation) {
    if (!session) return;
    await session.updateAnnotation(a.id, {
      body: editDraft,
      color: editColor,
    });
    cancelEdit();
  }
  function jumpTo(id: string) {
    window.dispatchEvent(new CustomEvent('aa-doc-anno-jump', { detail: { id } }));
  }

  function formatDate(iso: string): string {
    try { return new Date(iso).toLocaleDateString(); } catch { return iso; }
  }
  function fmtBytes(n: number | null): string {
    if (n == null) return '—';
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
    return `${(n / 1024 / 1024).toFixed(1)} MB`;
  }
  function fmtCount(n: number): string {
    if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
    if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
    return String(n);
  }
</script>

{#if !session}
  <div class="p-4 text-sm text-fg-muted"><p>Document viewer is loading…</p></div>
{:else}
  <div class="flex flex-col">

    <!-- ── 1. Reading ────────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Reading</h3>

      <div class="mb-2">
        <span class="mb-1 block text-fg-muted">Font</span>
        <div class="grid grid-cols-3 gap-1">
          {#each FONTS as f (f.id)}
            <button
              type="button"
              onclick={() => session.setFontFamily(f.id)}
              class={`rounded border px-1.5 py-1 text-[10px] ${session.fontFamily === f.id ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
              title={f.label}
            >{f.label}</button>
          {/each}
        </div>
      </div>

      <label class="mb-2 block">
        <span class="mb-1 flex items-center justify-between text-fg-muted">
          <span>Size</span>
          <span class="font-mono text-fg">{session.fontSize}px</span>
        </span>
        <input
          type="range" min="10" max="24" step="1"
          value={session.fontSize}
          oninput={(e) => session.setFontSize(+(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
        />
      </label>

      <label class="mb-2 block">
        <span class="mb-1 flex items-center justify-between text-fg-muted">
          <span>Line height</span>
          <span class="font-mono text-fg">{session.lineHeight.toFixed(1)}</span>
        </span>
        <input
          type="range" min="1.0" max="2.2" step="0.1"
          value={session.lineHeight}
          oninput={(e) => session.setLineHeight(+(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
        />
      </label>

      <div class="mb-2">
        <span class="mb-1 block text-fg-muted">Theme</span>
        <div class="grid grid-cols-3 gap-1">
          {#each THEMES as t (t.id)}
            <button
              type="button"
              onclick={() => session.setTheme(t.id)}
              class={`flex items-center justify-center gap-1.5 rounded border px-2 py-1.5 ${session.theme === t.id ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
              title={t.label}
            >
              <span class="h-3 w-3 rounded border border-black/30" style:background-color={t.swatch}></span>
              <span class="text-[10px]">{t.label}</span>
            </button>
          {/each}
        </div>
      </div>

      <div class="mb-2 flex items-center justify-between text-fg-muted">
        <span>Tab width</span>
        <div class="flex overflow-hidden rounded border border-border">
          {#each [2, 4, 8] as n (n)}
            <button
              type="button"
              onclick={() => session.setTabSize(n)}
              class={`px-2 py-1 text-[10px] ${session.tabSize === n ? 'bg-accent/20 text-fg' : 'bg-surface text-fg hover:bg-surface-elevated'}`}
            >{n}</button>
          {/each}
        </div>
      </div>

      <label class="mb-1 flex items-center justify-between text-fg-muted">
        <span>Wrap lines</span>
        <button type="button" onclick={() => session.toggleWrap()} class="inline-flex h-5 w-9 items-center rounded-full transition-colors" class:bg-accent={session.wrap} class:bg-border={!session.wrap} role="switch" aria-checked={session.wrap} aria-label="Wrap long lines">
          <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.wrap} class:translate-x-0.5={!session.wrap}></span>
        </button>
      </label>
      <label class="mb-1 flex items-center justify-between text-fg-muted">
        <span>Line numbers</span>
        <button type="button" onclick={() => session.toggleLineNumbers()} class="inline-flex h-5 w-9 items-center rounded-full transition-colors" class:bg-accent={session.lineNumbers} class:bg-border={!session.lineNumbers} role="switch" aria-checked={session.lineNumbers} aria-label="Show line numbers">
          <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.lineNumbers} class:translate-x-0.5={!session.lineNumbers}></span>
        </button>
      </label>
      {#if session.languageId === 'markdown'}
        <label class="flex items-center justify-between text-fg-muted">
          <span>Render markdown</span>
          <button type="button" onclick={() => session.toggleRenderMarkdown()} class="inline-flex h-5 w-9 items-center rounded-full transition-colors" class:bg-accent={session.renderMarkdown} class:bg-border={!session.renderMarkdown} role="switch" aria-checked={session.renderMarkdown} aria-label="Render markdown preview">
            <span class="block h-4 w-4 transform rounded-full bg-white shadow transition-transform" class:translate-x-4={session.renderMarkdown} class:translate-x-0.5={!session.renderMarkdown}></span>
          </button>
        </label>
      {/if}
    </section>

    <!-- ── 2. Outline ────────────────────────────────────────── -->
    {#if session.outline.length > 0}
      <section class="border-b border-border p-3 text-xs">
        <div class="mb-2 flex items-center justify-between">
          <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">Outline</h3>
          <span class="font-mono text-[10px] text-fg-muted">{session.outline.length}</span>
        </div>
        <div class="max-h-72 space-y-0.5 overflow-y-auto">
          {#each session.outline as o, i (i)}
            <button
              type="button"
              onclick={() => session.goToLine(o.line)}
              class="flex w-full items-center justify-between gap-2 rounded px-2 py-1 text-left text-[10px] hover:bg-surface-elevated"
              style:padding-left={`${0.5 + o.depth * 0.75}rem`}
              title={`Line ${o.line}`}
            >
              <span class="truncate text-fg">{o.label}</span>
              <span class="shrink-0 font-mono text-[10px] text-fg-muted/70">{o.line}</span>
            </button>
          {/each}
        </div>
      </section>
    {/if}

    <!-- ── 3. Find / Replace ─────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Find</h3>
      <input
        type="search"
        value={session.searchQuery}
        oninput={(e) => session.setSearchQuery((e.currentTarget as HTMLInputElement).value)}
        placeholder="Find in document…"
        class="w-full rounded border border-border bg-surface px-2 py-1 text-[11px] text-fg focus:border-accent focus:outline-none"
      />
      <div class="mt-1 flex items-center gap-1">
        <button
          type="button"
          onclick={() => (session.setSearchCaseSensitive(!session.searchCaseSensitive))}
          class={`rounded border px-1.5 py-0.5 text-[10px] ${session.searchCaseSensitive ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
          title="Case sensitive"
        >Aa</button>
        <button
          type="button"
          onclick={() => session.setSearchWholeWord(!session.searchWholeWord)}
          class={`rounded border px-1.5 py-0.5 text-[10px] ${session.searchWholeWord ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
          title="Whole word"
        >W</button>
        <button
          type="button"
          onclick={() => session.setSearchRegex(!session.searchRegex)}
          class={`rounded border px-1.5 py-0.5 text-[10px] ${session.searchRegex ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
          title="Regex"
        >.*</button>
        <span class="ml-auto flex items-center gap-1">
          <button
            type="button"
            onclick={() => session.findPrev()}
            disabled={!session.searchQuery}
            class="rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg hover:border-accent disabled:opacity-40"
            title="Previous match (⇧ Ctrl/⌘ G)"
            aria-label="Previous match"
          >‹</button>
          <button
            type="button"
            onclick={() => session.findNext()}
            disabled={!session.searchQuery}
            class="rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg hover:border-accent disabled:opacity-40"
            title="Next match (Ctrl/⌘ G)"
            aria-label="Next match"
          >›</button>
        </span>
      </div>
      <button
        type="button"
        onclick={() => (replaceOpen = !replaceOpen)}
        class="mt-1 w-full rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg-muted hover:border-accent hover:text-fg"
      >{replaceOpen ? 'Hide replace' : 'Replace…'}</button>
      {#if replaceOpen}
        <div class="mt-1 rounded border border-border bg-surface/60 p-2">
          <input
            type="text"
            value={session.replaceWith}
            oninput={(e) => session.setReplaceWith((e.currentTarget as HTMLInputElement).value)}
            placeholder="Replace with…"
            class="w-full rounded border border-border bg-surface px-2 py-1 text-[11px] text-fg focus:border-accent focus:outline-none"
          />
          <p class="mt-1 text-[10px] leading-snug text-fg-muted">
            Replace lands when the editor flips to edit mode (Phase&nbsp;D). The
            input persists in your session so the workflow is ready when it
            does.
          </p>
        </div>
      {/if}
    </section>

    <!-- ── 4. Bookmarks ──────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Bookmarks</h3>
      <div class="mb-2 flex gap-1">
        <input
          type="text"
          bind:value={bookmarkNote}
          placeholder="Optional note…"
          onkeydown={(e) => { if (e.key === 'Enter') addBookmark(); }}
          class="flex-1 rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg focus:border-accent focus:outline-none"
        />
        <button
          type="button"
          onclick={addBookmark}
          class="rounded border border-accent bg-accent/15 px-2 py-1 text-[10px] font-medium text-fg hover:bg-accent/25"
          title={`Bookmark line ${session.currentLine}`}
        >+</button>
      </div>
      {#if session.bookmarks.length === 0}
        <p class="rounded border border-border bg-surface/60 px-2 py-1 text-[10px] leading-snug text-fg-muted">
          No bookmarks yet. Press + to save the current line.
        </p>
      {:else}
        <div class="space-y-0.5">
          {#each session.bookmarks as bm (bm.createdAt + ':' + bm.line)}
            <div class="group flex items-start gap-1 rounded border border-border bg-surface px-2 py-1">
              <button
                type="button"
                onclick={() => session.goToLine(bm.line)}
                class="flex-1 text-left text-[10px]"
              >
                <div class="flex items-center justify-between">
                  <span class="truncate font-mono text-fg">Line {bm.line}</span>
                  <span class="ml-2 shrink-0 text-fg-muted/70">{formatDate(bm.createdAt)}</span>
                </div>
                {#if bm.note}
                  <div class="mt-0.5 text-fg-muted">{bm.note}</div>
                {/if}
              </button>
              <button
                type="button"
                onclick={() => session.removeBookmark(bm.line, bm.createdAt)}
                class="text-fg-muted opacity-0 hover:text-danger group-hover:opacity-100"
                aria-label="Remove bookmark"
                title="Remove bookmark"
              >×</button>
            </div>
          {/each}
        </div>
      {/if}
    </section>

    <!-- ── 5. Stats ──────────────────────────────────────────── -->
    {#if session.stats}
      <section class="border-b border-border p-3 text-xs">
        <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Stats</h3>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1 text-[10px]">
          <dt class="text-fg-muted">Language</dt>
          <dd class="font-mono text-fg">{session.languageId}</dd>
          <dt class="text-fg-muted">Lines</dt>
          <dd class="font-mono text-fg">{fmtCount(session.stats.lines)}</dd>
          <dt class="text-fg-muted">Words</dt>
          <dd class="font-mono text-fg">{fmtCount(session.stats.words)}</dd>
          <dt class="text-fg-muted">Characters</dt>
          <dd class="font-mono text-fg">{fmtCount(session.stats.characters)}</dd>
          <dt class="text-fg-muted">File size</dt>
          <dd class="font-mono text-fg">{fmtBytes(session.stats.fileSize)}</dd>
          <dt class="text-fg-muted">Cursor</dt>
          <dd class="font-mono text-fg">Line {session.currentLine}</dd>
        </dl>
      </section>
    {/if}

    <!-- ── 6. Lint ───────────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <div class="mb-2 flex items-center justify-between">
        <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">
          Lint{session.lintLinter && !session.lintSkipped ? ` · ${session.lintLinter}` : ''}
        </h3>
        {#if session.lintDiagnostics.length > 0}
          <span class="font-mono text-[10px] text-fg-muted">{session.lintDiagnostics.length}</span>
        {/if}
      </div>
      <div class="mb-2 flex items-center gap-1">
        <button
          type="button"
          onclick={() => session.runLint()}
          disabled={session.lintRunning}
          class="rounded border border-accent bg-accent/15 px-2 py-1 text-[10px] font-medium text-fg hover:bg-accent/25 disabled:opacity-50"
          title="Run the linter for this file"
        >{session.lintRunning ? 'Running…' : 'Run lint'}</button>
        {#if session.lintLinter !== null}
          <button
            type="button"
            onclick={() => session.clearLint()}
            class="rounded border border-border bg-surface px-2 py-1 text-[10px] text-fg hover:border-accent"
          >Clear</button>
        {/if}
      </div>
      {#if session.lintError}
        <p class="rounded border border-danger/40 bg-danger/10 px-2 py-1 text-[10px] text-danger">
          {session.lintError}
        </p>
      {:else if session.lintSkipped}
        <p class="rounded border border-border bg-surface/60 px-2 py-1 text-[10px] leading-snug text-fg-muted">
          No linter wired for <span class="font-mono text-fg">.{session.languageId}</span> yet —
          JSON / YAML / Markdown are covered today; py / js / ts / lua / sh follow.
        </p>
      {:else if session.lintLinter && session.lintDiagnostics.length === 0}
        <p class="rounded border border-success/40 bg-success/10 px-2 py-1 text-[10px] text-fg">
          No issues found.
        </p>
      {:else if session.lintDiagnostics.length > 0}
        <div class="max-h-72 space-y-0.5 overflow-y-auto pr-1">
          {#each session.lintDiagnostics as d, i (i)}
            <button
              type="button"
              onclick={() => session.goToLine(d.line)}
              class={`block w-full rounded border px-2 py-1 text-left text-[10px] hover:border-accent ${d.severity === 'error' ? 'border-danger/50 bg-danger/5' : d.severity === 'warning' ? 'border-yellow-500/50 bg-yellow-500/5' : 'border-border bg-surface/60'}`}
            >
              <div class="flex items-center gap-1">
                <span class={`shrink-0 rounded px-1 py-px font-mono text-[9px] uppercase ${d.severity === 'error' ? 'bg-danger/30 text-fg' : d.severity === 'warning' ? 'bg-yellow-500/30 text-fg' : 'bg-fg-muted/20 text-fg-muted'}`}>
                  {d.severity}
                </span>
                <span class="font-mono text-fg-muted">L{d.line}:{d.col}</span>
                <span class="ml-auto text-fg-muted/70">{d.source}</span>
              </div>
              <div class="mt-0.5 text-fg">{d.message}</div>
            </button>
          {/each}
        </div>
      {/if}
    </section>

    <!-- ── 7. Annotations ────────────────────────────────────── -->
    <section class="p-3 text-xs">
      <div class="mb-2 flex items-center justify-between">
        <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">Annotations</h3>
        {#if session.annotations.length > 0}
          <span class="font-mono text-[10px] text-fg-muted">{visibleAnnotations.length} / {session.annotations.length}</span>
        {/if}
      </div>

      {#if session.annotationsLoading}
        <p class="rounded border border-border bg-surface/60 px-2 py-1 text-[10px] text-fg-muted">Loading…</p>
      {:else if session.annotationsError}
        <p class="rounded border border-danger/40 bg-danger/10 px-2 py-1 text-[10px] text-danger">{session.annotationsError}</p>
      {:else}
        <!-- Style filter chips. Click an active chip to clear. -->
        <div class="mb-2 flex flex-wrap items-center gap-1">
          {#each FILTERS as f (f.id)}
            <button
              type="button"
              onclick={() => session.setAnnotationsFilter(session.annotationsFilter === f.id ? null : f.id)}
              class={`rounded border px-1.5 py-0.5 text-[10px] capitalize ${session.annotationsFilter === f.id ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg-muted hover:border-accent hover:text-fg'}`}
              title={`Filter to ${f.label}`}
            >{f.label}</button>
          {/each}
          <label class="ml-auto flex items-center gap-1 text-[10px] text-fg-muted">
            <input
              type="checkbox"
              checked={session.annotationsShowResolved}
              onchange={() => session.toggleAnnotationsShowResolved()}
              class="h-3 w-3 accent-accent"
            />
            <span>Show resolved</span>
          </label>
        </div>

        {#if visibleAnnotations.length === 0}
          <p class="rounded border border-border bg-surface/60 px-2 py-1 text-[10px] leading-snug text-fg-muted">
            {#if session.annotations.length === 0}
              No annotations yet. Select text in the document and pick a tool from the floating toolbar.
            {:else}
              No annotations match the current filter.
            {/if}
          </p>
        {:else}
          <div class="space-y-1">
            {#each visibleAnnotations as a (a.id)}
              <div class={`group rounded border bg-surface px-2 py-1 ${a.resolved ? 'opacity-60' : ''}`} style:border-color={a.color}>
                <div class="mb-0.5 flex items-center gap-1 text-[10px]">
                  <span class="rounded px-1 py-px font-mono text-[9px] uppercase tracking-wide text-fg" style:background-color={a.color}>
                    {a.style}
                  </span>
                  <span class="font-mono text-fg-muted/70">L{a.startLine}{a.startLine !== a.endLine ? `–${a.endLine}` : ''}</span>
                  <span class="ml-auto text-fg-muted/60">{formatDate(a.createdAt)}</span>
                </div>
                <button
                  type="button"
                  onclick={() => jumpTo(a.id)}
                  class="block w-full text-left text-[10px]"
                >
                  {#if a.body}
                    <div class="text-fg">{a.body}</div>
                  {:else}
                    <div class="text-fg-muted italic">(no commentary)</div>
                  {/if}
                </button>
                <div class="mt-1 flex items-center justify-end gap-1 opacity-0 transition-opacity group-hover:opacity-100">
                  <button
                    type="button"
                    onclick={() => session.updateAnnotation(a.id, { resolved: !a.resolved })}
                    class="rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg hover:border-accent"
                    title={a.resolved ? 'Mark unresolved' : 'Mark resolved'}
                  >{a.resolved ? '↶' : '✓'}</button>
                  <button
                    type="button"
                    onclick={() => beginEdit(a)}
                    class="rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg hover:border-accent"
                    title="Edit body"
                  >✎</button>
                  <button
                    type="button"
                    onclick={() => session.deleteAnnotation(a.id)}
                    class="rounded border border-border bg-surface px-1.5 py-0.5 text-[10px] text-fg hover:border-danger hover:text-danger"
                    title="Delete"
                  >×</button>
                </div>
                {#if editingId === a.id}
                  <div class="mt-1 rounded border border-accent bg-surface/80 p-1">
                    <textarea
                      bind:value={editDraft}
                      rows="2"
                      class="w-full resize-none rounded border border-border bg-surface px-1.5 py-1 text-[10px] text-fg focus:border-accent focus:outline-none"
                    ></textarea>
                    <div class="mt-1 flex items-center gap-1">
                      {#each HIGHLIGHT_SWATCHES as c (c)}
                        <button
                          type="button"
                          onclick={() => (editColor = c)}
                          class="h-4 w-4 rounded-full border-2"
                          class:border-fg={editColor === c}
                          class:border-transparent={editColor !== c}
                          style:background-color={c}
                          aria-label="Color {c}"
                        ></button>
                      {/each}
                      <button
                        type="button"
                        onclick={cancelEdit}
                        class="ml-auto rounded border border-border bg-surface px-2 py-0.5 text-[10px] text-fg hover:border-accent"
                      >Cancel</button>
                      <button
                        type="button"
                        onclick={() => void saveEdit(a)}
                        class="rounded border border-accent bg-accent/15 px-2 py-0.5 text-[10px] font-medium text-fg hover:bg-accent/25"
                      >Save</button>
                    </div>
                  </div>
                {/if}
              </div>
            {/each}
          </div>
        {/if}
        {#if session.annotationsWriteError}
          <p class="mt-1 rounded border border-danger/40 bg-danger/10 px-2 py-1 text-[10px] text-danger">
            {session.annotationsWriteError}
          </p>
        {/if}
      {/if}
    </section>
  </div>
{/if}
