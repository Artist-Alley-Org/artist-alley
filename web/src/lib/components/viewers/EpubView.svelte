<script lang="ts">
  // EpubView — page-by-page EPUB reader.
  //
  // Backend serves the spine list, chapter HTML (with relative
  // resource refs rewritten through us), and resource bytes.
  // The reader fetches one chapter at a time and renders it
  // inside a sandboxed iframe srcdoc — same-origin so cookies
  // travel and the rewritten /resources URLs load through us,
  // sandbox locked-down (no scripts, no top-nav) so a hostile
  // EPUB can't run JS in our origin.
  //
  // Reading position persists in localStorage per asset
  // (`aa.epub.{id}.chapter` + `.scroll`) so reopening the
  // viewer drops the user back where they left off — the
  // standard ebook-reader UX.
  //
  // Controls live in the canvas overlay top bar: chapter prev /
  // next + TOC picker. Page-down / ← / → / PgUp + PgDn are
  // handled inside the iframe by scroll; chapter boundaries
  // flip on ← / → at chapter ends.
  //
  // Per the four core principles:
  //   ABC — Spine + chapter HTML cached at the backend (the
  //     `cache.Registry` domains `asset.epub.spine` /
  //     `asset.epub.chapter`); the client also fetches once per
  //     chapter and lets the browser cache the iframe srcdoc.
  //   UX first — keyboard nav, persisted position, dark-mode
  //     aware iframe styling.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';

  type Asset = import('./controller').ViewAsset;

  interface Props {
    asset: Asset;
    controller: ViewController;
  }
  let { asset, controller = $bindable() }: Props = $props();

  interface SpineEntry {
    idx: number;
    label: string;
    href: string;
    media_type: string;
  }

  let spine = $state<SpineEntry[]>([]);
  let loadingSpine = $state(true);
  let spineError = $state<string | null>(null);
  let currentIdx = $state(0);
  let chapterHTML = $state<string | null>(null);
  let loadingChapter = $state(false);
  let chapterError = $state<string | null>(null);
  let tocOpen = $state(false);

  const positionKey = $derived(`aa.epub.${asset.id}.chapter`);

  async function loadSpine() {
    loadingSpine = true;
    spineError = null;
    try {
      const r = await fetch(`/api/v1/assets/${asset.id}/epub/spine`, {
        credentials: 'include',
      });
      if (!r.ok) throw new Error(`spine HTTP ${r.status}`);
      spine = (await r.json()) as SpineEntry[];
      // Restore reading position. Falls back to 0 on a fresh open
      // or when the stored idx is out of range (e.g. the asset was
      // re-uploaded and now has a different chapter count).
      try {
        const stored = parseInt(localStorage.getItem(positionKey) ?? '', 10);
        if (Number.isFinite(stored) && stored >= 0 && stored < spine.length) {
          currentIdx = stored;
        }
      } catch {
        /* private browsing — ignore */
      }
    } catch (e) {
      spineError = e instanceof Error ? e.message : String(e);
    } finally {
      loadingSpine = false;
    }
  }

  async function loadChapter(idx: number) {
    if (idx < 0 || idx >= spine.length) return;
    loadingChapter = true;
    chapterError = null;
    try {
      const r = await fetch(
        `/api/v1/assets/${asset.id}/epub/chapters/${idx}`,
        { credentials: 'include' },
      );
      if (!r.ok) throw new Error(`chapter HTTP ${r.status}`);
      const html = await r.text();
      chapterHTML = wrapForIframe(html);
      // Persist position only when loading succeeded.
      try {
        localStorage.setItem(positionKey, String(idx));
      } catch {
        /* ignore */
      }
    } catch (e) {
      chapterError = e instanceof Error ? e.message : String(e);
    } finally {
      loadingChapter = false;
    }
  }

  // Wrap the chapter body in a minimal HTML envelope that gives
  // it our preferred typography + dark-mode aware colours +
  // generous reading max-width. The original <html>/<head> stays
  // intact — we splice our overrides into <head>.
  function wrapForIframe(raw: string): string {
    const override = `
<style>
  :root { color-scheme: light dark; }
  html, body { background: transparent; color: inherit; }
  body {
    font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    font-size: 18px;
    line-height: 1.7;
    max-width: 38rem;
    margin: 0 auto;
    padding: 2rem 1.25rem 4rem;
    color: var(--aa-ebook-fg, #1a1a1a);
  }
  @media (prefers-color-scheme: dark) {
    body { color: var(--aa-ebook-fg, #d8d8d8); }
    a { color: #8ab4f8; }
  }
  img { max-width: 100%; height: auto; }
  p { margin: 0 0 1em; }
  h1, h2, h3 { line-height: 1.25; }
  a { color: #1a73e8; }
  ::selection { background: #ffeb3b66; }
</style>`;
    // Splice the override into <head>; if no <head>, prepend it.
    const headRe = /<head[^>]*>/i;
    if (headRe.test(raw)) return raw.replace(headRe, (m) => m + override);
    return override + raw;
  }

  function goTo(idx: number) {
    if (idx < 0 || idx >= spine.length) return;
    currentIdx = idx;
    void loadChapter(idx);
    tocOpen = false;
  }
  function prev() { goTo(currentIdx - 1); }
  function next() { goTo(currentIdx + 1); }

  function onKey(e: KeyboardEvent) {
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.key === 'ArrowLeft') { e.preventDefault(); prev(); }
    else if (e.key === 'ArrowRight') { e.preventDefault(); next(); }
  }

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
    void loadSpine();
  });
  onDestroy(() => { document.removeEventListener('keydown', onKey); });

  // Reactively load the active chapter once the spine + currentIdx
  // are resolved. Re-fires when the user picks from the TOC or
  // hits ← / →.
  $effect(() => {
    if (!loadingSpine && !spineError && spine.length > 0) {
      void loadChapter(currentIdx);
    }
  });

  $effect(() => {
    controller.hudExtra = spine.length > 0
      ? `${currentIdx + 1} / ${spine.length}`
      : '';
  });

  const activeLabel = $derived(spine[currentIdx]?.label ?? '');
</script>

<div class="relative flex h-full w-full flex-col bg-surface text-fg">
  <!-- Top bar: prev / TOC / next. Always visible so the user has
       deterministic navigation; iframe scroll happens beneath. -->
  <div class="flex shrink-0 items-center gap-2 border-b border-border bg-surface-elevated px-3 py-1.5 text-sm">
    <button
      type="button"
      onclick={prev}
      disabled={currentIdx <= 0}
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
        disabled={spine.length === 0}
        class="flex w-full items-center justify-between rounded px-2 py-1 text-left hover:bg-white/5 disabled:opacity-50"
        title="Chapter picker"
      >
        <span class="truncate">{activeLabel || (loadingSpine ? 'Loading…' : 'No chapters')}</span>
        <span class="ml-2 shrink-0 font-mono text-xs text-fg-muted">{spine.length > 0 ? `${currentIdx + 1} / ${spine.length}` : ''}</span>
      </button>
      {#if tocOpen && spine.length > 0}
        <!-- TOC popdown. Click outside (or pick a chapter) closes. -->
        <button
          type="button"
          class="fixed inset-0 z-10 cursor-default bg-transparent"
          onclick={() => (tocOpen = false)}
          aria-label="Close TOC"
        ></button>
        <div class="absolute left-0 right-0 top-full z-20 mt-1 max-h-80 overflow-y-auto rounded border border-border bg-surface-elevated shadow-2xl">
          {#each spine as e (e.idx)}
            <button
              type="button"
              onclick={() => goTo(e.idx)}
              class={`flex w-full items-center justify-between px-3 py-1.5 text-left text-sm hover:bg-surface ${e.idx === currentIdx ? 'text-accent' : 'text-fg'}`}
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
      onclick={next}
      disabled={currentIdx >= spine.length - 1}
      class="rounded p-1.5 text-fg-muted hover:bg-white/10 hover:text-fg disabled:opacity-30 disabled:hover:bg-transparent"
      title="Next chapter (→)"
      aria-label="Next chapter"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m9 18 6-6-6-6"/></svg>
    </button>
  </div>

  <!-- Body: iframe sandbox. Loading + error states render in-line
       so the reader chrome stays visible. -->
  <div class="relative flex-1 overflow-hidden bg-surface">
    {#if loadingSpine}
      <div class="flex h-full w-full items-center justify-center text-sm text-fg-muted">
        <p>Loading EPUB…</p>
      </div>
    {:else if spineError}
      <div class="flex h-full w-full items-center justify-center p-8 text-center text-sm text-danger">
        <p>Couldn't open EPUB: {spineError}</p>
      </div>
    {:else if chapterError}
      <div class="flex h-full w-full items-center justify-center p-8 text-center text-sm text-danger">
        <p>Couldn't load chapter: {chapterError}</p>
      </div>
    {:else if chapterHTML}
      <iframe
        title={asset.title || 'EPUB chapter'}
        srcdoc={chapterHTML}
        sandbox="allow-same-origin"
        class="h-full w-full border-0 bg-transparent"
      ></iframe>
      {#if loadingChapter}
        <div class="pointer-events-none absolute right-3 top-3 rounded bg-black/60 px-2 py-1 text-xs text-white">Loading…</div>
      {/if}
    {/if}
  </div>
</div>
