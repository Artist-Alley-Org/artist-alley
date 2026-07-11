<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // EbookTool body — TOC / search / bookmarks / reading settings.
  // Binds the shared EbookSession that EpubView also binds; both
  // ends mutate the same $state object so a chapter pick here
  // flips the reader's currentIdx without an event bus.
  //
  // Sections (top to bottom):
  //   1. Reading — font size + theme. Drives the iframe's CSS.
  //   2. Search — query box → hit list. Click a hit to jump.
  //   3. Bookmarks — add-current + list with note + jump / delete.
  //   4. Contents — full chapter list, active highlighted.
  //
  // Text-comment annotations are a future phase (need a DB
  // migration + text-anchor model). The bookmark surface here is
  // the entry point — once annotations land, "Annotate" becomes
  // a third action in the selection menu inside the iframe.

  import type { ToolContext } from '../contract';
  import type { EbookTheme, EbookFontFamily } from '$lib/ebook/session.svelte';

  let { ctx }: { ctx: ToolContext } = $props();
  const session = $derived(ctx.ebookSession);

  // Debounce-light: re-runs search on every keystroke once query
  // is >= 2 chars; the backend search is fast (cached chapter
  // text) so 100ms feels instant without flooding.
  let searchInput = $state('');
  let searchTimer: ReturnType<typeof setTimeout> | undefined;
  $effect(() => {
    if (!session) return;
    const q = searchInput;
    if (searchTimer) clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      void session.runSearch(q);
    }, 120);
  });

  let bookmarkNote = $state('');
  function addBookmark() {
    if (!session) return;
    session.addBookmark(bookmarkNote);
    bookmarkNote = '';
  }

  const THEMES: { id: EbookTheme; label: string; swatch: string }[] = [
    { id: 'light', label: 'Light', swatch: '#ffffff' },
    { id: 'sepia', label: 'Sepia', swatch: '#f4ecd8' },
    { id: 'dark',  label: 'Dark',  swatch: '#1a1a1a' },
  ];

  // Three font choices — system (UI default sans), serif (the
  // literary default that most readers reach for), and mono (rare
  // but useful for code-heavy technical books). Anything more
  // granular would mean shipping web-fonts, which we're not doing.
  const FONTS: { id: EbookFontFamily; label: string; sample: string }[] = [
    { id: 'system', label: 'Sans',  sample: 'Aa' },
    { id: 'serif',  label: 'Serif', sample: 'Aa' },
    { id: 'mono',   label: 'Mono',  sample: 'Aa' },
  ];

  function formatDate(iso: string): string {
    try { return new Date(iso).toLocaleDateString(); } catch { return iso; }
  }
</script>

{#if !session}
  <div class="p-4 text-sm text-fg-muted">
    <p>Ebook reader is loading…</p>
  </div>
{:else}
  <div class="flex flex-col">

    <!-- Reading settings ─────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Reading</h3>
      <label class="mb-2 block">
        <span class="mb-1 flex items-center justify-between text-fg-muted">
          <span>Font size</span>
          <span class="font-mono text-fg">{session.fontSize}px</span>
        </span>
        <input
          type="range"
          min="12"
          max="28"
          step="1"
          value={session.fontSize}
          oninput={(e) => session.setFontSize(+(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
        />
      </label>
      <label class="mb-2 block">
        <span class="mb-1 flex items-center justify-between text-fg-muted">
          <span>Column width</span>
          <span class="font-mono text-fg">{session.maxWidth}rem</span>
        </span>
        <input
          type="range"
          min="32"
          max="90"
          step="1"
          value={session.maxWidth}
          oninput={(e) => session.setMaxWidth(+(e.currentTarget as HTMLInputElement).value)}
          class="w-full accent-accent"
        />
      </label>
      <div class="mb-2">
        <span class="mb-1 block text-fg-muted">Font</span>
        <div class="grid grid-cols-3 gap-1">
          {#each FONTS as f (f.id)}
            <button
              type="button"
              onclick={() => session.setFontFamily(f.id)}
              class={`flex items-center justify-center gap-1.5 rounded border px-2 py-1.5 ${session.fontFamily === f.id ? 'border-accent bg-accent/20 text-fg' : 'border-border bg-surface text-fg hover:border-accent'}`}
              title={f.label}
            >
              <span
                class="font-mono text-[11px] leading-none"
                style:font-family={f.id === 'serif' ? 'Georgia, "Times New Roman", serif' : f.id === 'mono' ? 'ui-monospace, Menlo, Consolas, monospace' : 'system-ui, sans-serif'}
              >{f.sample}</span>
              <span class="text-[10px]">{f.label}</span>
            </button>
          {/each}
        </div>
      </div>
      <div>
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
    </section>

    <!-- Search ───────────────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Search</h3>
      <input
        type="search"
        bind:value={searchInput}
        placeholder="Find in book…"
        class="w-full rounded border border-border bg-surface px-2 py-1 text-[11px] text-fg focus:border-accent focus:outline-none"
      />
      {#if session.searchBusy}
        <p class="mt-2 text-[10px] text-fg-muted">Searching…</p>
      {:else if session.searchError}
        <p class="mt-2 text-[10px] text-danger">{session.searchError}</p>
      {:else if session.searchQuery && session.searchResults.length === 0}
        <p class="mt-2 text-[10px] text-fg-muted">No matches.</p>
      {:else if session.searchResults.length > 0}
        <p class="mt-2 text-[10px] text-fg-muted">{session.searchResults.length} hit{session.searchResults.length === 1 ? '' : 's'}</p>
        <div class="mt-1 max-h-72 space-y-0.5 overflow-y-auto">
          {#each session.searchResults as hit, i (i)}
            <button
              type="button"
              onclick={() => session.goTo(hit.chapterIdx)}
              class="block w-full rounded border border-border bg-surface px-2 py-1 text-left text-[10px] hover:border-accent"
            >
              <div class="mb-0.5 flex items-center justify-between">
                <span class="truncate text-fg-muted">{hit.chapterLabel}</span>
                <span class="font-mono text-fg-muted/70">#{hit.chapterIdx + 1}</span>
              </div>
              <div class="text-fg">{hit.snippet}</div>
            </button>
          {/each}
        </div>
      {/if}
    </section>

    <!-- Bookmarks ────────────────────────────────────────────── -->
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
          title="Bookmark current chapter"
        >+</button>
      </div>
      {#if session.bookmarks.length === 0}
        <p class="rounded border border-border bg-surface/60 px-2 py-1 text-[10px] leading-snug text-fg-muted">
          No bookmarks yet. Press + to save the current chapter (and optionally add a note).
        </p>
      {:else}
        <div class="space-y-0.5">
          {#each session.bookmarks as bm (bm.createdAt + ':' + bm.idx)}
            {@const label = session.spine[bm.idx]?.label ?? `Chapter ${bm.idx + 1}`}
            <div class="group flex items-start gap-1 rounded border border-border bg-surface px-2 py-1">
              <button
                type="button"
                onclick={() => session.goTo(bm.idx)}
                class="flex-1 text-left text-[10px]"
              >
                <div class="flex items-center justify-between">
                  <span class="truncate font-medium text-fg">{label}</span>
                  <span class="ml-2 shrink-0 text-fg-muted/70">{formatDate(bm.createdAt)}</span>
                </div>
                {#if bm.note}
                  <div class="mt-0.5 text-fg-muted">{bm.note}</div>
                {/if}
              </button>
              <button
                type="button"
                onclick={() => session.removeBookmark(bm.idx, bm.createdAt)}
                class="text-fg-muted opacity-0 hover:text-danger group-hover:opacity-100"
                aria-label="Remove bookmark"
                title="Remove bookmark"
              >×</button>
            </div>
          {/each}
        </div>
      {/if}
    </section>

    <!-- Table of contents ────────────────────────────────────── -->
    <section class="border-b border-border p-3 text-xs">
      <div class="mb-2 flex items-center justify-between">
        <h3 class="text-[10px] font-medium uppercase tracking-wider text-fg-muted">Contents</h3>
        {#if session.spine.length > 0}
          <span class="font-mono text-[10px] text-fg-muted">{session.currentIdx + 1} / {session.spine.length}</span>
        {/if}
      </div>
      {#if session.spineLoading}
        <p class="text-[10px] text-fg-muted">Loading chapters…</p>
      {:else if session.spineError}
        <p class="text-[10px] text-danger">{session.spineError}</p>
      {:else}
        <div class="max-h-96 space-y-0.5 overflow-y-auto">
          {#each session.spine as e (e.idx)}
            <button
              type="button"
              onclick={() => session.goTo(e.idx)}
              class={`flex w-full items-center justify-between gap-2 rounded px-2 py-1 text-left text-[10px] ${e.idx === session.currentIdx ? 'bg-accent/20 text-accent' : 'text-fg hover:bg-surface-elevated'}`}
            >
              <span class="truncate">{e.label}</span>
              <span class="shrink-0 font-mono text-fg-muted/70">{e.idx + 1}</span>
            </button>
          {/each}
        </div>
      {/if}
    </section>

    <!-- Future annotations note. Kept as a stub section so users
         see the roadmap; the real surface lands when the DB
         migration + text-anchor model are in. -->
    <section class="p-3 text-xs">
      <h3 class="mb-2 text-[10px] font-medium uppercase tracking-wider text-fg-muted">Annotations · coming soon</h3>
      <p class="text-[10px] leading-snug text-fg-muted">
        Highlight a passage in the reader to attach a comment to it. Annotations will sync across sessions like bookmarks, but anchored to the exact text rather than a chapter.
      </p>
    </section>
  </div>
{/if}
