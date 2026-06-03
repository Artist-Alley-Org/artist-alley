<script lang="ts">
  // EpubView — page-by-page EPUB reader.
  //
  // Reading state (current chapter, font size, theme, bookmarks,
  // search) lives in a shared EbookSession that AssetViewer
  // builds per asset; the EbookTool's side-panel body binds the
  // same instance, so picking a TOC entry in the panel flips
  // currentIdx here without an event bus.
  //
  // The body fetches one chapter at a time and renders it
  // inside a sandboxed iframe srcdoc — same-origin so cookies
  // travel and the rewritten /resources URLs load through us,
  // sandbox locked-down (no scripts, no top-nav) so a hostile
  // EPUB can't run JS in our origin.
  //
  // Controls in the top bar: chapter prev / next + TOC picker.
  // Page-down / ← / → / PgUp + PgDn handled inside the iframe
  // by scroll; chapter boundaries flip on ← / → at chapter ends.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';
  import type { EbookSessionInstance } from '$lib/ebook/session.svelte';

  type Asset = import('./controller').ViewAsset;

  interface Props {
    asset: Asset;
    controller: ViewController;
    /** Shared reactive state with the EbookTool — AssetViewer
     *  builds one per asset and binds both sides. */
    session: EbookSessionInstance;
  }
  let { asset, controller = $bindable(), session = $bindable<EbookSessionInstance>() }: Props = $props();

  let chapterHTML = $state<string | null>(null);
  let tocOpen = $state(false);
  let iframeEl: HTMLIFrameElement | undefined = $state();
  /** Throttle handle for scroll persistence — fires at most once
   *  per 250 ms while the user is scrolling, plus one final write
   *  on chapter change so the last position isn't lost. */
  let scrollTimer: ReturnType<typeof setTimeout> | undefined;

  async function loadChapter(idx: number) {
    if (idx < 0 || idx >= session.spine.length) return;
    session.chapterLoading = true;
    session.chapterError = null;
    try {
      const r = await fetch(
        `/api/v1/assets/${asset.id}/epub/chapters/${idx}`,
        { credentials: 'include' },
      );
      if (!r.ok) throw new Error(`chapter HTTP ${r.status}`);
      const html = await r.text();
      chapterHTML = wrapForIframe(html);
    } catch (e) {
      session.chapterError = e instanceof Error ? e.message : String(e);
    } finally {
      session.chapterLoading = false;
    }
  }

  // Splice our typography + theme overrides into <head>. Tokens
  // come from the session so the EbookTool's pickers flip the
  // iframe styling live. We use !important sparingly — the
  // EPUB's own stylesheet often hardcodes font-family on body
  // (Calibre exports do this) so our overrides need to win
  // without nuking the EPUB's structural CSS.
  function wrapForIframe(raw: string): string {
    const t = themeTokens(session.theme);
    const ff = fontFamilyStack(session.fontFamily);
    const override = `
<style>
  :root { color-scheme: ${session.theme === 'dark' ? 'dark' : 'light'}; }
  html, body { background: ${t.bg} !important; color: ${t.fg} !important; }
  body {
    font-family: ${ff} !important;
    font-size: ${session.fontSize}px !important;
    line-height: 1.7;
    max-width: ${session.maxWidth}rem;
    margin: 0 auto;
    padding: 2rem 1.5rem 4rem;
  }
  /* Inherit the body font down into headings + lists + tables
     so a Calibre export's "font-family: serif" on <h1> doesn't
     fight the user's choice. */
  h1, h2, h3, h4, h5, h6, p, li, blockquote, span, div, em, strong,
  td, th, dt, dd { font-family: inherit !important; }
  img { max-width: 100%; height: auto; }
  p { margin: 0 0 1em; }
  h1, h2, h3 { line-height: 1.25; }
  a { color: ${t.link}; }
  ::selection { background: ${t.selection}; }
</style>`;
    const headRe = /<head[^>]*>/i;
    if (headRe.test(raw)) return raw.replace(headRe, (m) => m + override);
    return override + raw;
  }

  function fontFamilyStack(f: string): string {
    switch (f) {
      case 'serif':
        return '"Iowan Old Style", "Palatino Linotype", "Book Antiqua", Palatino, Georgia, "Times New Roman", serif';
      case 'mono':
        return '"JetBrains Mono", "Fira Code", "SF Mono", Menlo, Consolas, monospace';
      case 'system':
      default:
        return 'system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif';
    }
  }

  function themeTokens(theme: string) {
    switch (theme) {
      case 'light':
        return { bg: '#ffffff', fg: '#1a1a1a', link: '#1a73e8', selection: '#ffeb3b66' };
      case 'sepia':
        return { bg: '#f4ecd8', fg: '#3b2f1d', link: '#8b5e34', selection: '#d4af3766' };
      case 'dark':
      default:
        return { bg: '#1a1a1a', fg: '#d8d8d8', link: '#8ab4f8', selection: '#5a4a1066' };
    }
  }

  function onKey(e: KeyboardEvent) {
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === 'ArrowLeft') { e.preventDefault(); flushScroll(); session.goPrev(); }
    else if (e.key === 'ArrowRight') { e.preventDefault(); flushScroll(); session.goNext(); }
  }

  // Read the iframe's current scroll position. Returns 0 when the
  // iframe isn't mounted / contentWindow isn't accessible.
  function currentScroll(): number {
    try {
      const w = iframeEl?.contentWindow;
      if (!w) return 0;
      // scrollY for windows that scroll naturally; the EPUB body
      // sets max-width via our wrapper so the document scrolls.
      return Math.round(w.scrollY ?? w.document.documentElement.scrollTop ?? 0);
    } catch {
      return 0;
    }
  }

  // Throttled persist — fires at most every 250 ms while the
  // user drags the scrollbar / hits PgDn.
  function schedulePersistScroll() {
    if (scrollTimer) return;
    scrollTimer = setTimeout(() => {
      scrollTimer = undefined;
      session.setScroll(session.currentIdx, currentScroll());
    }, 250);
  }

  // Synchronous persist — called on chapter change / unmount so
  // the last position isn't lost to a pending throttle window.
  function flushScroll() {
    if (scrollTimer) { clearTimeout(scrollTimer); scrollTimer = undefined; }
    session.setScroll(session.currentIdx, currentScroll());
  }

  // Wire up scroll listening + restore-on-load after the iframe
  // mounts a new chapter. Re-runs whenever chapterHTML changes;
  // cleanup pulls the listener so the previous chapter doesn't
  // double-fire into the new one's session entry.
  $effect(() => {
    void chapterHTML;
    if (!iframeEl) return;
    const fr = iframeEl;
    function handle() {
      try {
        const w = fr.contentWindow;
        if (!w) return;
        // Restore the saved scroll for THIS chapter (if any).
        const restore = session.scrollByChapter[session.currentIdx] ?? 0;
        if (restore > 0) {
          // Defer one frame so the iframe's layout is settled —
          // setting scrollY before paint scrolls a zero-height
          // doc and the value gets lost.
          requestAnimationFrame(() => {
            try { w.scrollTo(0, restore); } catch { /* ignore */ }
          });
        }
        // Listen for scroll inside the iframe.
        w.addEventListener('scroll', schedulePersistScroll, { passive: true });
      } catch { /* cross-origin shouldn't happen on srcdoc — ignore */ }
    }
    fr.addEventListener('load', handle);
    return () => {
      fr.removeEventListener('load', handle);
      try { fr.contentWindow?.removeEventListener('scroll', schedulePersistScroll); } catch { /* ignore */ }
    };
  });

  onMount(() => {
    controller.kind = 'ebook';
    controller.hasTimeline = false;
    controller.totalFrames = 0;
    controller.fps = 0;
    controller.duration = 0;
    controller.playing = false;
    controller.spritesUrl = null;
    controller.spritesVttUrl = null;
    controller.formatAnchor = () => '';
    controller.play = () => {};
    controller.pause = () => {};
    controller.togglePlay = () => {};
    controller.seekToFrame = () => {};
    controller.stepFrames = () => {};
    controller.setRate = () => {};
    document.addEventListener('keydown', onKey);
    void session.loadSpine();
  });
  onDestroy(() => { document.removeEventListener('keydown', onKey); });

  // Re-fetch chapter on session.currentIdx change (driven by ← /
  // → here OR by goTo() from the EbookTool side panel).
  $effect(() => {
    if (!session.spineLoading && !session.spineError && session.spine.length > 0) {
      void loadChapter(session.currentIdx);
    }
  });

  // Re-render chapter HTML (re-wrap) when font/theme change so
  // the iframe picks up the new styling without a chapter refetch.
  $effect(() => {
    void session.fontSize;
    void session.theme;
    void session.fontFamily;
    void session.maxWidth;
    if (chapterHTML) {
      // Re-wrap the LAST raw chapter — but we only kept the
      // wrapped version. Easiest: re-fetch (cached on the server,
      // so it's a one-roundtrip cheap hit).
      void loadChapter(session.currentIdx);
    }
  });

  $effect(() => {
    controller.hudExtra = session.spine.length > 0
      ? `${session.currentIdx + 1} / ${session.spine.length}`
      : '';
  });

  const activeLabel = $derived(session.spine[session.currentIdx]?.label ?? '');
</script>

<div class="relative flex h-full w-full flex-col bg-surface text-fg">
  <!-- Top bar: prev / TOC / next. Always visible so the user has
       deterministic navigation; iframe scroll happens beneath. -->
  <div class="flex shrink-0 items-center gap-2 border-b border-border bg-surface-elevated px-3 py-1.5 text-sm">
    <button
      type="button"
      onclick={() => { flushScroll(); session.goPrev(); }}
      disabled={session.currentIdx <= 0}
      class="rounded p-1.5 text-fg-muted hover:bg-white/10 hover:text-fg disabled:opacity-30 disabled:hover:bg-transparent"
      title="Previous chapter (←)"
      aria-label="Previous chapter"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m15 18-6-6 6-6"/></svg>
    </button>
    <div class="relative flex-1">
      <button
        type="button"
        onclick={() => (tocOpen = !tocOpen)}
        disabled={session.spine.length === 0}
        class="flex w-full items-center justify-between rounded px-2 py-1 text-left hover:bg-white/5 disabled:opacity-50"
        title="Chapter picker"
      >
        <span class="truncate">{activeLabel || (session.spineLoading ? 'Loading…' : 'No chapters')}</span>
        <span class="ml-2 shrink-0 font-mono text-xs text-fg-muted">{session.spine.length > 0 ? `${session.currentIdx + 1} / ${session.spine.length}` : ''}</span>
      </button>
      {#if tocOpen && session.spine.length > 0}
        <button
          type="button"
          class="fixed inset-0 z-10 cursor-default bg-transparent"
          onclick={() => (tocOpen = false)}
          aria-label="Close TOC"
        ></button>
        <div class="absolute left-0 right-0 top-full z-20 mt-1 max-h-80 overflow-y-auto rounded border border-border bg-surface-elevated shadow-2xl">
          {#each session.spine as e (e.idx)}
            <button
              type="button"
              onclick={() => { flushScroll(); session.goTo(e.idx); tocOpen = false; }}
              class={`flex w-full items-center justify-between px-3 py-1.5 text-left text-sm hover:bg-surface ${e.idx === session.currentIdx ? 'text-accent' : 'text-fg'}`}
            >
              <span class="truncate">{e.label}</span>
              <span class="ml-2 shrink-0 font-mono text-xs text-fg-muted">{e.idx + 1}</span>
            </button>
          {/each}
        </div>
      {/if}
    </div>
    <button
      type="button"
      onclick={() => { flushScroll(); session.goNext(); }}
      disabled={session.currentIdx >= session.spine.length - 1}
      class="rounded p-1.5 text-fg-muted hover:bg-white/10 hover:text-fg disabled:opacity-30 disabled:hover:bg-transparent"
      title="Next chapter (→)"
      aria-label="Next chapter"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>
    </button>
  </div>

  <div class="relative flex-1 overflow-hidden bg-surface">
    {#if session.spineLoading}
      <div class="flex h-full w-full items-center justify-center text-sm text-fg-muted">
        <p>Loading EPUB…</p>
      </div>
    {:else if session.spineError}
      <div class="flex h-full w-full items-center justify-center p-8 text-center text-sm text-danger">
        <p>Couldn't open EPUB: {session.spineError}</p>
      </div>
    {:else if session.chapterError}
      <div class="flex h-full w-full items-center justify-center p-8 text-center text-sm text-danger">
        <p>Couldn't load chapter: {session.chapterError}</p>
      </div>
    {:else if chapterHTML}
      <iframe
        bind:this={iframeEl}
        title={asset.title || 'EPUB chapter'}
        srcdoc={chapterHTML}
        sandbox="allow-same-origin"
        class="h-full w-full border-0 bg-transparent"
      ></iframe>
      {#if session.chapterLoading}
        <div class="pointer-events-none absolute right-3 top-3 rounded bg-black/60 px-2 py-1 text-xs text-white">Loading…</div>
      {/if}
    {/if}
  </div>
</div>
