<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // ArchiveView — browse a ZIP / TAR / TAR.GZ asset without
  // extracting the whole thing. The manifest is already cached on
  // asset.metadata.archive (populated by the preview.archive job);
  // we render it as a file tree on the left and let the user click
  // an entry to preview it inline (text/code) or download it.
  //
  // Entry bytes stream from /assets/{id}/archive/entry?path=... —
  // the backend opens the archive on demand, seeks to the entry,
  // and pipes the decompressed bytes back. Never extracts the
  // whole archive.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';
  import type { ArchiveSessionInstance, ArchiveManifest, TreeNode } from '$lib/archive/session.svelte';
  import { buildTree, fmtBytes } from '$lib/archive/session.svelte';
  import { languageIdForExt, loadLanguage } from '$lib/codemirror/lang';
  import { t } from '$stores/lang.svelte';

  type Asset = import('./controller').ViewAsset;

  interface Props {
    asset: Asset;
    controller: ViewController;
    session: ArchiveSessionInstance;
  }
  let { asset, controller = $bindable(), session = $bindable<ArchiveSessionInstance>() }: Props = $props();

  const ext = $derived((asset.file_extension || '').toLowerCase().replace(/^\./, ''));

  // ── Bootstrap — read manifest off the asset's metadata. The
  // preview.archive job stamps it; on first visit it may not be
  // populated yet, so we also fetch the asset to retry.
  onMount(() => {
    controller.kind = 'archive';
    controller.hasTimeline = false;
    controller.totalFrames = 0;
    controller.fps = 0;
    controller.duration = 0;
    controller.playing = false;
    controller.spritesUrl = null;
    controller.spritesVttUrl = null;
    controller.formatAnchor = () => '';
    controller.hudExtra = ext.toUpperCase();
    controller.play = () => {};
    controller.pause = () => {};
    controller.togglePlay = () => {};
    controller.seekToFrame = () => {};
    controller.stepFrames = () => {};
    controller.setRate = () => {};
    controller.tools = null;

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const fromProp = (asset.metadata as any)?.archive as Wire | undefined;
    if (fromProp) applyManifest(fromProp);
    else void fetchManifest();
  });

  // ── Manifest plumbing ──────────────────────────────────────
  interface Wire {
    format?: string;
    entries?: Array<{
      path: string;
      size?: number;
      compressed_size?: number;
      modified?: string;
      is_dir?: boolean;
      comment?: string;
    }>;
    entry_count?: number;
    truncated?: boolean;
    total_size?: number;
  }
  function applyManifest(w: Wire) {
    const m: ArchiveManifest = {
      format: w.format ?? '',
      entries: (w.entries ?? []).map((e) => ({
        path: e.path,
        size: e.size ?? 0,
        compressedSize: e.compressed_size ?? 0,
        modified: e.modified ?? '',
        isDir: !!e.is_dir,
        comment: e.comment ?? '',
      })),
      entryCount: w.entry_count ?? (w.entries ?? []).length,
      truncated: !!w.truncated,
      totalSize: w.total_size ?? 0,
    };
    session.manifest = m;
    session.loading = false;
  }
  async function fetchManifest() {
    try {
      const r = await fetch(`/api/v1/assets/${asset.id}`, { credentials: 'include' });
      if (!r.ok) throw new Error(`asset GET ${r.status}`);
      const full = await r.json();
      const ar = full?.metadata?.archive;
      if (ar) applyManifest(ar);
      else {
        // Preview job hasn't populated metadata yet. Leave loading
        // true; the user can click "Refresh" in the panel (future)
        // or wait and reload.
        session.loadError = 'Archive manifest is still being extracted — try again in a few seconds.';
        session.loading = false;
      }
    } catch (e) {
      session.loadError = e instanceof Error ? e.message : String(e);
      session.loading = false;
    }
  }

  // ── Tree + selection ──────────────────────────────────────
  const tree = $derived.by<TreeNode[]>(() =>
    session.manifest ? buildTree(session.manifest.entries, session.filter, session.hideDotfiles) : [],
  );

  // ── Entry preview ─────────────────────────────────────────
  // Extensions we'll fetch + render inline as text. Anything else
  // shows a "preview not available" + Download.
  const TEXT_EXTS = new Set([
    'txt', 'log', 'md', 'markdown', 'rst', 'csv', 'tsv',
    'json', 'jsonc', 'yaml', 'yml', 'toml', 'ini', 'cfg', 'conf', 'env', 'properties',
    'sh', 'bash', 'zsh', 'fish', 'ps1',
    'py', 'pyi', 'rb', 'lua', 'pl', 'pm',
    'js', 'mjs', 'cjs', 'jsx', 'ts', 'tsx',
    'go', 'rs', 'java', 'kt', 'c', 'h', 'cpp', 'cc', 'hpp', 'cs',
    'php', 'sql', 'graphql', 'xml', 'html', 'htm', 'css', 'scss',
    'svg', 'gitignore', 'gitattributes', 'dockerfile', 'makefile',
    'diff', 'patch',
  ]);
  const IMAGE_EXTS = new Set(['jpg', 'jpeg', 'png', 'gif', 'webp', 'avif', 'svg']);

  /** Pick the right preview mode for the selected entry. */
  function previewKind(path: string): 'text' | 'image' | 'binary' {
    const dot = path.lastIndexOf('.');
    const e = dot >= 0 ? path.slice(dot + 1).toLowerCase() : '';
    if (IMAGE_EXTS.has(e)) return 'image';
    if (TEXT_EXTS.has(e) || e === '' /* README without extension */) return 'text';
    return 'binary';
  }
  function entryUrl(path: string): string {
    return `/api/v1/assets/${asset.id}/archive/entry?path=${encodeURIComponent(path)}`;
  }

  // Fetch + render text preview when selectedPath flips. 1 MB cap
  // so a giant JSON file doesn't lock the panel; binary kinds skip
  // the fetch entirely.
  const PREVIEW_CAP = 1 * 1024 * 1024;
  $effect(() => {
    const path = session.selectedPath;
    if (!path) return;
    const kind = previewKind(path);
    if (kind !== 'text') return;
    if (session.previewSize > PREVIEW_CAP) {
      session.previewError = `Entry is ${fmtBytes(session.previewSize)} — too large to preview inline. Download to view.`;
      return;
    }
    session.previewLoading = true;
    session.previewError = null;
    session.previewText = '';
    void (async () => {
      try {
        const r = await fetch(entryUrl(path), { credentials: 'include' });
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        session.previewMime = r.headers.get('content-type') ?? '';
        session.previewText = await r.text();
      } catch (e) {
        session.previewError = e instanceof Error ? e.message : String(e);
      } finally {
        session.previewLoading = false;
      }
    })();
  });

  // ── CodeMirror mount ──────────────────────────────────────
  // The text preview pane lazy-mounts a read-only CodeMirror 6
  // editor when previewText lands so we get syntax highlighting,
  // line numbers, and search without dragging in the full DocView
  // chrome. Each time the selected entry changes we rebuild the
  // editor (cheap — most entries are < 1 MB and the grammar pack
  // is already cached).
  let codeContainer: HTMLDivElement | undefined = $state();
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let cmView: any = null;
  $effect(() => {
    const text = session.previewText;
    const path = session.selectedPath;
    const el = codeContainer;
    if (!el || !path || previewKind(path) !== 'text' || session.previewLoading || session.previewError) {
      if (cmView) { cmView.destroy(); cmView = null; }
      return;
    }
    const ext = (() => {
      const dot = path.lastIndexOf('.');
      return dot >= 0 ? path.slice(dot + 1) : '';
    })();
    let cancelled = false;
    void (async () => {
      const [
        { EditorState },
        { EditorView, lineNumbers, highlightActiveLine, highlightActiveLineGutter, drawSelection, keymap },
        { defaultKeymap, history, historyKeymap },
        { searchKeymap, search },
        { syntaxHighlighting, defaultHighlightStyle, foldGutter, foldKeymap, bracketMatching },
        { oneDark },
      ] = await Promise.all([
        import('@codemirror/state'),
        import('@codemirror/view'),
        import('@codemirror/commands'),
        import('@codemirror/search'),
        import('@codemirror/language'),
        import('@codemirror/theme-one-dark'),
      ]);
      const lang = await loadLanguage(languageIdForExt(ext));
      if (cancelled || !codeContainer) return;
      if (cmView) cmView.destroy();
      const state = EditorState.create({
        doc: text,
        extensions: [
          history(),
          drawSelection(),
          lineNumbers(),
          highlightActiveLine(),
          highlightActiveLineGutter(),
          bracketMatching(),
          foldGutter(),
          search({ top: true }),
          syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
          keymap.of([...defaultKeymap, ...historyKeymap, ...searchKeymap, ...foldKeymap]),
          oneDark,
          EditorView.lineWrapping,
          EditorState.readOnly.of(true),
          EditorView.theme({
            '&': { height: '100%', fontSize: '12px' },
            '.cm-scroller': {
              fontFamily: '"JetBrains Mono", "Fira Code", "SF Mono", Menlo, Consolas, monospace',
              lineHeight: '1.45',
            },
          }),
          lang ? [lang] : [],
        ],
      });
      cmView = new EditorView({ state, parent: codeContainer });
    })();
    return () => { cancelled = true; };
  });
  onDestroy(() => { if (cmView) { cmView.destroy(); cmView = null; } });
</script>

<div class="flex h-full w-full overflow-hidden bg-surface text-fg">
  <!-- File tree (left) -->
  <aside class="flex h-full w-72 shrink-0 flex-col border-r border-border bg-surface-elevated">
    <header class="border-b border-border p-2">
      <input
        type="search"
        value={session.filter}
        oninput={(e) => session.setFilter((e.currentTarget as HTMLInputElement).value)}
        placeholder={t('archive.filter_placeholder')}
        class="w-full rounded border border-border bg-surface px-2 py-1 text-[11px] text-fg focus:border-accent focus:outline-none"
      />
      <label class="mt-1 flex items-center gap-1 text-[10px] text-fg-muted">
        <input
          type="checkbox"
          checked={session.hideDotfiles}
          onchange={() => session.toggleHideDotfiles()}
          class="h-3 w-3 accent-accent"
        />
        <span>{t('archive.hide_dotfiles')}</span>
        <span class="ml-auto font-mono">
          {t('archive.entry_count', { count: session.manifest ? session.manifest.entryCount : 0 })}
        </span>
      </label>
    </header>
    <div class="flex-1 overflow-y-auto">
      {#if session.loading}
        <p class="p-3 text-xs text-fg-muted">{t('archive.loading_manifest')}</p>
      {:else if session.loadError}
        <p class="m-3 rounded border border-danger/40 bg-danger/10 px-2 py-1 text-[10px] text-danger">{session.loadError}</p>
      {:else if tree.length === 0}
        <p class="p-3 text-xs text-fg-muted">{t('archive.no_entries_match')}</p>
      {:else}
        {#each tree as node (node.path)}
          {@render treeNode(node, 0)}
        {/each}
      {/if}
    </div>
    {#if session.manifest?.truncated}
      <p class="border-t border-yellow-500/30 bg-yellow-500/10 px-2 py-1 text-[10px] text-yellow-300">
        {t('archive.manifest_truncated', { count: session.manifest.entryCount })}
      </p>
    {/if}
  </aside>

  <!-- Entry preview (right) -->
  <section class="relative flex flex-1 flex-col overflow-hidden">
    {#if !session.selectedPath}
      <div class="flex h-full w-full items-center justify-center text-sm text-fg-muted">
        <p>{t('archive.select_entry')}</p>
      </div>
    {:else}
      {@const kind = previewKind(session.selectedPath)}
      <header class="flex items-center justify-between gap-2 border-b border-border bg-surface-elevated px-3 py-2 text-xs">
        <div class="min-w-0 flex-1">
          <div class="truncate font-mono text-fg">{session.selectedPath}</div>
          <div class="mt-0.5 flex items-center gap-2 text-[10px] text-fg-muted">
            <span>{fmtBytes(session.previewSize)}</span>
            {#if session.previewMime}
              <span class="rounded bg-surface px-1 py-px font-mono">{session.previewMime}</span>
            {/if}
          </div>
        </div>
        <button
          type="button"
          onclick={() => session.downloadEntry(session.selectedPath!)}
          class="shrink-0 rounded border border-accent bg-accent/15 px-2 py-1 text-[10px] font-medium text-fg hover:bg-accent/25"
          title={t('archive.download_entry_title')}
        >{t('archive.download')}</button>
      </header>
      <div class="flex-1 overflow-auto">
        {#if kind === 'image'}
          <div class="flex h-full w-full items-center justify-center bg-black/30 p-4">
            <img
              src={entryUrl(session.selectedPath)}
              alt={session.selectedPath}
              class="max-h-full max-w-full object-contain"
            />
          </div>
        {:else if kind === 'text'}
          {#if session.previewLoading}
            <p class="p-3 text-xs text-fg-muted">{t('archive.loading_entry')}</p>
          {:else if session.previewError}
            <p class="m-3 rounded border border-danger/40 bg-danger/10 px-2 py-1 text-[10px] text-danger">{session.previewError}</p>
          {:else}
            <div bind:this={codeContainer} class="h-full w-full"></div>
          {/if}
        {:else}
          <div class="flex h-full w-full flex-col items-center justify-center gap-3 p-8 text-center text-sm text-fg-muted">
            <svg xmlns="http://www.w3.org/2000/svg" width="48" height="48" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round">
              <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/>
              <polyline points="14 2 14 8 20 8"/>
            </svg>
            <p>{t('archive.no_inline_preview')}</p>
          </div>
        {/if}
      </div>
    {/if}
  </section>
</div>

{#snippet treeNode(node: TreeNode, depth: number)}
  <button
    type="button"
    onclick={() => (node.isDir ? session.toggleFolder(node.path) : session.selectEntry(node.path))}
    class={`group flex w-full items-center gap-1 px-2 py-0.5 text-left text-[10px] ${session.selectedPath === node.path ? 'bg-accent/20 text-accent' : 'hover:bg-surface'}`}
    style:padding-left={`${0.5 + depth * 0.85}rem`}
    title={node.path}
  >
    {#if node.isDir}
      <span class="inline-block w-3 text-fg-muted">{session.expanded[node.path] ? '▾' : '▸'}</span>
      <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-fg-muted/80"><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>
      <span class="truncate">{node.name}</span>
      <span class="ml-auto shrink-0 font-mono text-fg-muted/60">{node.childCount}</span>
    {:else}
      <span class="inline-block w-3"></span>
      <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="text-fg-muted/70"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>
      <span class="truncate">{node.name}</span>
      <span class="ml-auto shrink-0 font-mono text-fg-muted/60">{fmtBytes(node.size)}</span>
    {/if}
  </button>
  {#if node.isDir && session.expanded[node.path]}
    {#each node.children as child (child.path)}
      {@render treeNode(child, depth + 1)}
    {/each}
  {/if}
{/snippet}
