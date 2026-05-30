<script lang="ts">
  // PDFView — inline PDF rendering via pdfjs-dist. Each page renders
  // to its own <canvas>; the parent <div> scrolls vertically so the
  // user reads a multi-page document the way they'd read it in any
  // other PDF viewer (no per-page navigation needed for the MVP).
  //
  // Future polish: zoom controls (controller exposes the slider
  // already), text-layer overlay for selection, link annotations,
  // page-jump in the HUD.
  //
  // PDF.js loads its worker as a separate file. Vite resolves the
  // ?url import to a static asset path so the browser can fetch
  // it cross-origin-safe.

  import { onMount, onDestroy } from 'svelte';
  import * as pdfjs from 'pdfjs-dist';
  // The bundled worker is shipped with pdfjs-dist. Vite's ?url
  // suffix gives us a stable URL the worker script can be loaded
  // from instead of bundling the worker into the main chunk
  // (which inflates the page load and isn't necessary).
  import workerSrc from 'pdfjs-dist/build/pdf.worker.min.mjs?url';
  import type { ViewController } from './controller';
  import { defaultController } from './controller';

  // One-time worker registration — PDF.js stashes this globally,
  // so re-mounting PDFView for a different asset doesn't repeat
  // the assignment.
  if (!pdfjs.GlobalWorkerOptions.workerSrc) {
    pdfjs.GlobalWorkerOptions.workerSrc = workerSrc;
  }

  type Asset = import('./controller').ViewAsset;

  let { asset, controller = $bindable<ViewController>() }: {
    asset: Asset;
    controller: ViewController;
  } = $props();

  // Read the asset's PDF metadata stamped by preview.pdf (pdfinfo
  // output). Optional throughout — the viewer works fine on any
  // PDF, with or without metadata; this just feeds the HUD.
  interface PDFMeta {
    num_pages?: number;
    title?: string;
    author?: string;
    encrypted?: boolean;
  }
  const meta = $derived<PDFMeta>(
    ((asset.metadata as Record<string, unknown> | null | undefined)?.pdf as PDFMeta | undefined) ?? {},
  );

  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);

  let container = $state<HTMLDivElement | null>(null);
  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let renderedPages = $state(0);
  let totalPages = $state(0);

  // PDF.js render passes return a Promise-shaped task; keep refs
  // so we can cancel mid-flight when the user navigates away.
  let activeDoc: { destroy: () => void } | null = null;
  let cancelled = false;

  onMount(() => {
    void renderDocument();
  });
  onDestroy(() => {
    cancelled = true;
    if (activeDoc) {
      try { activeDoc.destroy(); } catch { /* ignore */ }
      activeDoc = null;
    }
  });

  async function renderDocument() {
    if (!container) return;
    loading = true;
    loadError = null;
    renderedPages = 0;

    try {
      const task = pdfjs.getDocument({ url: fileUrl, withCredentials: true });
      const doc = await task.promise;
      if (cancelled) {
        doc.destroy();
        return;
      }
      activeDoc = doc;
      totalPages = doc.numPages;

      controller = {
        ...defaultController(),
        hudExtra: [
          meta.title || asset.title || '',
          totalPages > 0 ? `${totalPages} pages` : '',
        ].filter(Boolean).join(' · '),
      };

      // Render every page sequentially. For very long PDFs we'd
      // want virtual scrolling (mount/unmount canvases as they
      // enter the viewport) — that's a polish step, not MVP.
      for (let pageNum = 1; pageNum <= doc.numPages; pageNum++) {
        if (cancelled) return;
        const page = await doc.getPage(pageNum);
        const viewport = page.getViewport({ scale: 1.5 });
        const canvas = document.createElement('canvas');
        canvas.width = viewport.width;
        canvas.height = viewport.height;
        canvas.className = 'mb-3 max-w-full bg-white shadow-md';
        const ctx = canvas.getContext('2d');
        if (!ctx) continue;
        await page.render({ canvas, canvasContext: ctx, viewport }).promise;
        if (cancelled) return;
        container.appendChild(canvas);
        renderedPages = pageNum;
        if (pageNum === 1) loading = false; // first page lights up the viewer
      }
      loading = false;
    } catch (e) {
      loading = false;
      loadError = e instanceof Error ? e.message : 'Failed to load PDF.';
    }
  }
</script>

<div class="flex h-full w-full flex-col overflow-hidden bg-[#1a1c22]">
  {#if loading}
    <div class="flex flex-1 items-center justify-center text-sm text-white/60">
      Loading PDF…
    </div>
  {/if}
  {#if loadError}
    <div class="flex flex-1 items-center justify-center px-6 text-center">
      <div>
        <p class="text-sm text-danger">Couldn't render PDF</p>
        <p class="mt-1 font-mono text-xs text-white/40">{loadError}</p>
        <a
          href={fileUrl}
          download
          class="mt-3 inline-block text-xs text-accent hover:underline"
        >Download original</a>
      </div>
    </div>
  {/if}

  <div
    bind:this={container}
    class="flex-1 overflow-y-auto px-4 py-4"
    class:hidden={!!loadError && renderedPages === 0}
  ></div>

  {#if renderedPages > 0 && totalPages > renderedPages}
    <div class="border-t border-white/10 px-4 py-1 text-center text-xs text-white/40">
      Rendered {renderedPages} / {totalPages} pages…
    </div>
  {/if}
</div>
