<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // DocView — read-only document body for txt / md / code files.
  //
  // Renders via CodeMirror 6 (lazy-imported so the editor + grammar
  // bundles stay out of the main chunk for sessions that never open
  // a doc asset). All user-visible state — font / theme / wrap / line
  // numbers / search query — lives on the shared DocSession; both
  // ends of the chrome bind the same $state object.
  //
  // For markdown the panel can flip session.renderMarkdown to swap
  // the source view for a sanitized HTML preview (marked + DOMPurify).
  //
  // Read-only for Phase A. Edit-and-save lands in Phase D once we
  // wire a versioned asset-text endpoint.

  import { onMount, onDestroy } from 'svelte';
  import type { ViewController } from './controller';
  import type { DocSessionInstance, DocOutlineEntry } from '$lib/doc/session.svelte';
  import { persistDocScroll } from '$lib/doc/session.svelte';
  import { annotationField, annotationTheme, setDocAnnotations } from '$lib/doc/annotation-deco';
  import { EXT_LANG, loadLanguage } from '$lib/codemirror/lang';

  type Asset = import('./controller').ViewAsset;

  interface Props {
    asset: Asset;
    controller: ViewController;
    session: DocSessionInstance;
  }
  let { asset, controller = $bindable(), session = $bindable<DocSessionInstance>() }: Props = $props();

  const fileUrl = $derived(`/api/v1/assets/${asset.id}/file`);
  const ext = $derived((asset.file_extension || '').toLowerCase().replace(/^\./, ''));

  let container: HTMLDivElement | undefined = $state();
  let previewEl: HTMLDivElement | undefined = $state();

  // CodeMirror 6 holds its own EditorView; we stash imports + the
  // view on `host` (non-reactive) and react via $effects on session
  // fields to push prefs into the editor.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let host = $state<any>(null);
  /** Raw source text — kept so markdown render-toggle doesn't refetch. */
  let sourceText = $state<string>('');
  /** Markdown HTML cache — computed on demand inside the effect. */
  let renderedHTML = $state<string>('');
  /** Annotation the panel asked the editor to scroll into view. */
  let focusedAnnotationId = $state<string | null>(null);

  // Extension → CodeMirror language id + dynamic grammar loader live
  // in $lib/codemirror/lang so ArchiveView's in-archive preview shares
  // the same pack inventory.
  const languageId = $derived(EXT_LANG[ext] ?? 'plain');

  // ── Outline builders ──────────────────────────────────────────
  function buildMarkdownOutline(text: string): DocOutlineEntry[] {
    const out: DocOutlineEntry[] = [];
    const lines = text.split(/\r?\n/);
    let inFence = false;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      // Skip headings inside fenced code blocks.
      if (/^```/.test(line)) { inFence = !inFence; continue; }
      if (inFence) continue;
      const m = line.match(/^(#{1,6})\s+(.+?)\s*#*\s*$/);
      if (m) {
        out.push({ depth: m[1].length - 1, label: m[2].trim(), line: i + 1 });
      }
    }
    return out;
  }
  // Cheap code symbol extractor — catches the common "definition"
  // patterns per language without standing up a tree-sitter pipeline.
  // Misses overloads / arrow-function consts / lambdas; that's fine
  // for a side-panel nav aid, not a refactoring tool.
  function buildCodeOutline(id: string, text: string): DocOutlineEntry[] {
    const out: DocOutlineEntry[] = [];
    const lines = text.split(/\r?\n/);
    const patterns: RegExp[] = [];
    switch (id) {
      case 'python': patterns.push(/^\s*(?:async\s+)?def\s+([A-Za-z_][\w]*)\s*\(/, /^\s*class\s+([A-Za-z_][\w]*)/); break;
      case 'javascript': case 'javascript-jsx': case 'typescript': case 'typescript-jsx':
        patterns.push(/^\s*(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)\s*\(/, /^\s*(?:export\s+)?class\s+([A-Za-z_$][\w$]*)/, /^\s*(?:export\s+)?(?:const|let)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s+)?\(/); break;
      case 'go': patterns.push(/^func\s+(?:\([^)]+\)\s+)?([A-Za-z_][\w]*)\s*\(/, /^type\s+([A-Za-z_][\w]*)\s/); break;
      case 'rust': patterns.push(/^\s*(?:pub\s+)?fn\s+([A-Za-z_][\w]*)/, /^\s*(?:pub\s+)?struct\s+([A-Za-z_][\w]*)/, /^\s*(?:pub\s+)?enum\s+([A-Za-z_][\w]*)/, /^\s*(?:pub\s+)?trait\s+([A-Za-z_][\w]*)/); break;
      case 'cpp': patterns.push(/^\s*(?:[\w*<>:&\s]+?)\s+([A-Za-z_][\w]*)\s*\([^;]*\)\s*(?:const\s*)?\{/, /^\s*class\s+([A-Za-z_][\w]*)/, /^\s*struct\s+([A-Za-z_][\w]*)/); break;
      case 'ruby': patterns.push(/^\s*def\s+([A-Za-z_][\w!?=]*)/, /^\s*class\s+([A-Za-z_][\w]*)/, /^\s*module\s+([A-Za-z_][\w]*)/); break;
      case 'lua': patterns.push(/^\s*function\s+([A-Za-z_][\w.:]*)/, /^\s*local\s+function\s+([A-Za-z_][\w]*)/); break;
      case 'shell': patterns.push(/^\s*(?:function\s+)?([A-Za-z_][\w-]*)\s*\(\s*\)\s*\{/); break;
      default: return out;
    }
    for (let i = 0; i < lines.length; i++) {
      for (const re of patterns) {
        const m = lines[i].match(re);
        if (m) { out.push({ depth: 0, label: m[1], line: i + 1 }); break; }
      }
    }
    return out;
  }

  function rebuildOutline(text: string) {
    if (session.languageId === 'markdown') {
      session.outline = buildMarkdownOutline(text);
    } else {
      session.outline = buildCodeOutline(session.languageId, text);
    }
  }

  function computeStats(text: string, fileSize: number | null) {
    const lines = text.split(/\r?\n/).length;
    // Cheap word counter — splits on whitespace runs, skips empties.
    const words = (text.match(/\S+/g) || []).length;
    session.stats = {
      lines, words, characters: text.length, fileSize,
    };
  }

  // ── Markdown render ───────────────────────────────────────────
  async function renderMarkdownNow(text: string): Promise<string> {
    const { marked } = await import('marked');
    const DOMPurify = (await import('dompurify')).default;
    const html = await marked.parse(text, { async: true });
    // Sanitize through DOMPurify — Marked emits safe HTML but a
    // hostile uploaded doc could still pack `<script>` etc. Belt +
    // suspenders since users upload arbitrary content.
    return DOMPurify.sanitize(html, { USE_PROFILES: { html: true } });
  }

  // ── Mount ─────────────────────────────────────────────────────
  onMount(() => {
    controller.kind = 'doc';
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

    session.languageId = languageId;
    session.outline = [];
    session.stats = null;
    session.loading = true;
    session.loadError = null;

    void boot();
  });

  let cleanupFn: (() => void) | null = null;
  onDestroy(() => {
    if (cleanupFn) cleanupFn();
  });

  async function boot() {
    try {
      // Fetch source — cap at 8 MB so a `.log` upload doesn't lock
      // the tab. Bigger files surface a "download to view full"
      // notice; the user keeps the canvas chrome.
      const MAX = 8 * 1024 * 1024;
      const r = await fetch(fileUrl, { credentials: 'include' });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const len = parseInt(r.headers.get('content-length') || '0', 10);
      if (len > MAX) {
        session.loadError = `File too large (${(len / 1024 / 1024).toFixed(1)} MB). Open via Download for the full source.`;
        session.loading = false;
        return;
      }
      const text = await r.text();
      if (text.length > MAX) {
        session.loadError = `File too large (${(text.length / 1024 / 1024).toFixed(1)} MB).`;
        session.loading = false;
        return;
      }
      sourceText = text;
      computeStats(text, len || text.length);
      rebuildOutline(text);
      await mountEditor(text);
    } catch (e) {
      session.loadError = e instanceof Error ? e.message : String(e);
    } finally {
      session.loading = false;
    }
  }

  async function mountEditor(text: string) {
    if (!container) return;
    // Dynamic-import the editor surface so the bundle splits per kind.
    const [
      { EditorState, Compartment, EditorSelection },
      { EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, drawSelection, highlightSpecialChars },
      { defaultKeymap, history, historyKeymap, indentWithTab },
      { search, openSearchPanel, closeSearchPanel, findNext, findPrevious, replaceNext, replaceAll, setSearchQuery, SearchQuery, searchKeymap },
      { syntaxHighlighting, defaultHighlightStyle, foldGutter, foldKeymap, indentUnit, bracketMatching, indentOnInput },
      { oneDark },
      { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap },
      { lintKeymap, linter, lintGutter, setDiagnostics, openLintPanel },
    ] = await Promise.all([
      import('@codemirror/state'),
      import('@codemirror/view'),
      import('@codemirror/commands'),
      import('@codemirror/search'),
      import('@codemirror/language'),
      import('@codemirror/theme-one-dark'),
      import('@codemirror/autocomplete'),
      import('@codemirror/lint'),
    ]);

    const lang = await loadLanguage(languageId);

    // Compartments let us reconfigure each axis independently when
    // a session field changes — without rebuilding the whole state.
    const themeCompartment = new Compartment();
    const wrapCompartment = new Compartment();
    const lineNumCompartment = new Compartment();
    const tabCompartment = new Compartment();
    const fontCompartment = new Compartment();
    const langCompartment = new Compartment();

    function themeExt() {
      const t = session.theme;
      const baseTheme = EditorView.theme({
        '&': {
          backgroundColor: t === 'light' ? '#ffffff' : t === 'sepia' ? '#f4ecd8' : '#1a1a1a',
          color: t === 'light' ? '#1a1a1a' : t === 'sepia' ? '#3b2f1d' : '#d8d8d8',
          height: '100%',
        },
        '.cm-scroller': { fontFamily: 'inherit', lineHeight: String(session.lineHeight) },
        '.cm-gutters': {
          backgroundColor: t === 'light' ? '#f5f5f5' : t === 'sepia' ? '#ece1c4' : '#202024',
          color: t === 'light' ? '#999' : t === 'sepia' ? '#7e6a4a' : '#777',
          border: 'none',
        },
        '.cm-activeLine': { backgroundColor: t === 'light' ? '#f5f5f580' : t === 'sepia' ? '#ece1c480' : '#26262a80' },
        '.cm-activeLineGutter': { backgroundColor: 'transparent' },
        '.cm-cursor': { borderLeftColor: t === 'light' ? '#1a1a1a' : t === 'sepia' ? '#3b2f1d' : '#d8d8d8' },
      }, { dark: t === 'dark' });
      return t === 'dark' ? [oneDark, baseTheme] : [baseTheme, syntaxHighlighting(defaultHighlightStyle, { fallback: true })];
    }

    function fontExt() {
      const ff =
        session.fontFamily === 'sans'  ? 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif' :
        session.fontFamily === 'serif' ? 'Georgia, "Times New Roman", serif' :
                                          '"JetBrains Mono", "Fira Code", "SF Mono", Menlo, Consolas, monospace';
      return EditorView.theme({
        '.cm-content, .cm-gutters': {
          fontFamily: ff,
          fontSize: session.fontSize + 'px',
          lineHeight: String(session.lineHeight),
        },
      });
    }

    const state = EditorState.create({
      doc: text,
      extensions: [
        history(),
        drawSelection(),
        highlightSpecialChars(),
        highlightActiveLine(),
        highlightActiveLineGutter(),
        bracketMatching(),
        closeBrackets(),
        autocompletion(),
        indentOnInput(),
        foldGutter(),
        search({ top: true }),
        keymap.of([
          ...defaultKeymap, ...historyKeymap, ...searchKeymap,
          ...foldKeymap, ...closeBracketsKeymap, ...completionKeymap,
          ...lintKeymap, indentWithTab,
        ]),
        themeCompartment.of(themeExt()),
        fontCompartment.of(fontExt()),
        wrapCompartment.of(session.wrap ? EditorView.lineWrapping : []),
        lineNumCompartment.of(session.lineNumbers ? lineNumbers() : []),
        tabCompartment.of(indentUnit.of(' '.repeat(session.tabSize))),
        langCompartment.of(lang ? [lang] : []),
        annotationField,
        annotationTheme,
        // Lint extension — diagnostics are pushed via setDiagnostics
        // from the $effect on session.lintDiagnostics below. The
        // gutter shows severity dots; hovering the editor surfaces
        // the message tooltips. linter(() => []) wires up the panel
        // without providing its own diagnostic source.
        lintGutter(),
        linter(() => []),
        EditorState.readOnly.of(true),
        EditorView.updateListener.of((upd) => {
          if (upd.selectionSet || upd.docChanged || upd.viewportChanged) {
            const head = upd.state.selection.main.head;
            const ln = upd.state.doc.lineAt(head).number;
            if (session.currentLine !== ln) {
              session.currentLine = ln;
              persistDocScroll(asset.id, ln);
            }
          }
          if (upd.selectionSet || upd.docChanged) {
            reportSelection(upd.view);
          }
        }),
        EditorView.domEventHandlers({
          mouseup(_, view) {
            // Re-anchor on mouseup so the floating toolbar sits
            // where the cursor landed, not where the drag started.
            queueMicrotask(() => reportSelection(view));
            return false;
          },
          click(ev, view) {
            // Click an existing annotation → open it in the panel.
            const t = ev.target as HTMLElement | null;
            const annoEl = t?.closest('[data-annotation-id]') as HTMLElement | null;
            if (!annoEl) return false;
            const id = annoEl.getAttribute('data-annotation-id');
            if (id) {
              focusedAnnotationId = id;
              // Surface in the panel (scroll the entry into view)
              session.setSelection(null);
              const ev2 = new CustomEvent('aa-doc-anno-focus', {
                detail: { id },
                bubbles: true,
              });
              view.dom.dispatchEvent(ev2);
            }
            return false;
          },
        }),
      ],
    });

    if (!container) return;
    const view = new EditorView({ state, parent: container });

    // Restore last scroll position (best effort — file may have
    // shrunk so we clamp to last line).
    const startLine = Math.min(session.currentLine || 1, state.doc.lines);
    if (startLine > 1) {
      const pos = state.doc.line(startLine).from;
      view.dispatch({
        selection: EditorSelection.cursor(pos),
        effects: EditorView.scrollIntoView(pos, { y: 'center' }),
      });
    }

    host = {
      view, EditorView, EditorSelection, Compartment,
      themeCompartment, wrapCompartment, lineNumCompartment,
      tabCompartment, fontCompartment, langCompartment,
      themeExt, fontExt, lineNumbers, indentUnit,
      search: { setSearchQuery, openSearchPanel, closeSearchPanel, findNext, findPrevious, replaceNext, replaceAll, SearchQuery },
      lint: { setDiagnostics, openLintPanel },
    };

    // Initial paint of any annotations already loaded into the
    // session, then fetch fresh ones from the server.
    pushAnnotations(view);
    void session.loadAnnotations();

    cleanupFn = () => { view.destroy(); host = null; };
  }

  // ── Selection + annotation bridges ────────────────────────────
  function reportSelection(view: import('@codemirror/view').EditorView) {
    const sel = view.state.selection.main;
    const doc = view.state.doc;
    const startLine = doc.lineAt(sel.from);
    const endLine = doc.lineAt(sel.to);
    const desc = {
      startLine: startLine.number,
      startCol: sel.from - startLine.from,
      endLine: endLine.number,
      endCol: sel.to - endLine.from,
      empty: sel.empty,
    };
    if (sel.empty) {
      session.setSelection(desc, null);
      return;
    }
    // Anchor the toolbar above the end of the selection. Coords
    // are viewport-relative because the toolbar lives in fixed
    // positioning at the top of AssetViewer's overlay.
    const coords = view.coordsAtPos(sel.to);
    if (coords) {
      session.setSelection(desc, { x: (coords.left + coords.right) / 2, y: coords.top });
    } else {
      session.setSelection(desc, null);
    }
  }

  function pushAnnotations(view: import('@codemirror/view').EditorView) {
    view.dispatch({ effects: setDocAnnotations.of(
      session.annotations.map((a) => ({
        id: a.id,
        style: a.style,
        color: a.color,
        startLine: a.startLine,
        startCol: a.startCol,
        endLine: a.endLine,
        endCol: a.endCol,
        resolved: a.resolved,
      })),
    ) });
  }

  // ── Reactive bridges (session → CodeMirror) ──────────────────
  $effect(() => {
    if (!host) return;
    void session.theme; void session.lineHeight;
    host.view.dispatch({ effects: host.themeCompartment.reconfigure(host.themeExt()) });
  });
  $effect(() => {
    if (!host) return;
    void session.fontFamily; void session.fontSize; void session.lineHeight;
    host.view.dispatch({ effects: host.fontCompartment.reconfigure(host.fontExt()) });
  });
  $effect(() => {
    if (!host) return;
    host.view.dispatch({
      effects: host.wrapCompartment.reconfigure(
        session.wrap ? host.EditorView.lineWrapping : [],
      ),
    });
  });
  $effect(() => {
    if (!host) return;
    host.view.dispatch({
      effects: host.lineNumCompartment.reconfigure(
        session.lineNumbers ? host.lineNumbers() : [],
      ),
    });
  });
  $effect(() => {
    if (!host) return;
    host.view.dispatch({
      effects: host.tabCompartment.reconfigure(host.indentUnit.of(' '.repeat(session.tabSize))),
    });
  });
  // Jump-to-line trigger.
  $effect(() => {
    if (!host) return;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    void (session as any)._jumpLineTrigger;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const ln = Math.min((session as any)._pendingJumpLine, host.view.state.doc.lines);
    if (ln < 1) return;
    const pos = host.view.state.doc.line(ln).from;
    host.view.dispatch({
      selection: host.EditorSelection.cursor(pos),
      effects: host.EditorView.scrollIntoView(pos, { y: 'center' }),
    });
    host.view.focus();
  });
  // Search query changes — feed CM's setSearchQuery so the next-
  // match counter inside the panel mirrors what's in the editor.
  $effect(() => {
    if (!host) return;
    const q = session.searchQuery;
    const sq = new host.search.SearchQuery({
      search: q,
      caseSensitive: session.searchCaseSensitive,
      regexp: session.searchRegex,
      wholeWord: session.searchWholeWord,
    });
    host.view.dispatch({ effects: host.search.setSearchQuery.of(sq) });
  });
  $effect(() => {
    if (!host) return;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    void (session as any)._findNextTrigger;
    if (!session.searchQuery) return;
    host.search.findNext(host.view);
  });
  $effect(() => {
    if (!host) return;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    void (session as any)._findPrevTrigger;
    if (!session.searchQuery) return;
    host.search.findPrevious(host.view);
  });

  // Markdown render mode toggle — re-renders source via marked.
  $effect(() => {
    if (!session.renderMarkdown || languageId !== 'markdown' || !sourceText) {
      renderedHTML = '';
      return;
    }
    void renderMarkdownNow(sourceText).then((html) => { renderedHTML = html; });
  });

  // Push lint diagnostics into CodeMirror whenever the session list
  // changes. The setDiagnostics helper converts each entry to a
  // CM6 Diagnostic by translating (line, col) → char offset.
  $effect(() => {
    if (!host) return;
    const diags = session.lintDiagnostics;
    const doc = host.view.state.doc;
    function lineColToPos(line: number, col: number): number {
      const ln = Math.max(1, Math.min(line, doc.lines));
      const lineObj = doc.line(ln);
      const c = Math.max(0, Math.min(col - 1, lineObj.length));
      return lineObj.from + c;
    }
    const mapped = diags.map((d) => {
      const from = lineColToPos(d.line, d.col);
      const to = d.endLine
        ? lineColToPos(d.endLine, d.endCol ?? d.col + 1)
        : Math.min(from + 1, doc.length);
      return {
        from,
        to: Math.max(to, from + 1),
        severity: d.severity,
        message: d.message,
        source: d.source,
      };
    });
    host.view.dispatch(host.lint.setDiagnostics(host.view.state, mapped));
  });

  // Re-paint editor decorations when the annotation list mutates.
  // The viewer-driven `setDocAnnotations` effect inside the editor
  // rebuilds the decoration set from scratch each call — cheap for
  // the dozen-or-so annotations a single document carries.
  $effect(() => {
    if (!host) return;
    void session.annotations;
    pushAnnotations(host.view);
  });

  // The panel emits a custom 'aa-doc-anno-jump' event with an
  // annotation id when the user clicks an entry — scroll the editor
  // to that range. Listening at the window level since the panel
  // lives in a sibling DOM subtree.
  function onJump(e: Event) {
    if (!host) return;
    const detail = (e as CustomEvent<{ id: string }>).detail;
    const ann = session.annotations.find((a) => a.id === detail.id);
    if (!ann) return;
    const ln = Math.min(ann.startLine, host.view.state.doc.lines);
    const lineFrom = host.view.state.doc.line(ln).from;
    const col = Math.min(ann.startCol, host.view.state.doc.line(ln).length);
    const pos = lineFrom + col;
    host.view.dispatch({
      selection: host.EditorSelection.cursor(pos),
      effects: host.EditorView.scrollIntoView(pos, { y: 'center' }),
    });
    focusedAnnotationId = detail.id;
  }
  onMount(() => {
    window.addEventListener('aa-doc-anno-jump', onJump as EventListener);
  });
  onDestroy(() => {
    window.removeEventListener('aa-doc-anno-jump', onJump as EventListener);
  });

  // HUD shows the language tag + line count.
  $effect(() => {
    const lng = session.languageId !== 'plain' ? session.languageId : ext;
    controller.hudExtra = lng.toUpperCase();
  });

  const showPreview = $derived(
    session.renderMarkdown && languageId === 'markdown' && renderedHTML.length > 0,
  );

  // ── Selection toolbar ─────────────────────────────────────────
  // Anchored above the user's selection. Five swatches + four style
  // buttons; pressing one fires session.createAnnotation and clears
  // the selection so the toolbar dismisses. Comments + notes open a
  // local body-text prompt inline before persisting.
  const HIGHLIGHT_SWATCHES = ['#fef08a', '#bef264', '#7dd3fc', '#f9a8d4', '#fca5a5'];
  let pickedColor = $state(HIGHLIGHT_SWATCHES[0]);
  let commentDraft = $state<{ style: 'comment' | 'note'; body: string } | null>(null);

  function selectionExists(): boolean {
    return !!session.selection && !session.selection.empty;
  }

  async function applyStyle(style: 'highlight' | 'strikethrough' | 'underline') {
    if (!session.selection) return;
    const sel = session.selection;
    await session.createAnnotation({
      style, color: pickedColor, body: '',
      anchor: { startLine: sel.startLine, startCol: sel.startCol, endLine: sel.endLine, endCol: sel.endCol },
    });
    session.setSelection(null);
  }
  function openCommentDraft(style: 'comment' | 'note') {
    if (!session.selection) return;
    commentDraft = { style, body: '' };
  }
  async function submitDraft() {
    if (!session.selection || !commentDraft) return;
    const sel = session.selection;
    await session.createAnnotation({
      style: commentDraft.style,
      color: pickedColor,
      body: commentDraft.body.trim(),
      // Notes anchor at the cursor (zero-width) when selection is
      // empty, but if there's a range selected we keep it so a "note
      // about this whole paragraph" lands as a paragraph-scoped pin.
      anchor: {
        startLine: sel.startLine, startCol: sel.startCol,
        endLine: commentDraft.style === 'note' ? sel.startLine : sel.endLine,
        endCol:  commentDraft.style === 'note' ? sel.startCol  : sel.endCol,
      },
    });
    commentDraft = null;
    session.setSelection(null);
  }
  function cancelDraft() { commentDraft = null; }
</script>

<div class="relative flex h-full w-full flex-col bg-surface text-fg">
  <!-- Container is always rendered (even during load/error) so the
       bind:this is wired before boot() calls mountEditor. The
       loading / error overlays float above it. -->
  <div class="relative flex-1 overflow-hidden">
    <div
      bind:this={container}
      class="absolute inset-0 overflow-hidden"
      class:hidden={showPreview || session.loading || !!session.loadError}
    ></div>
    {#if showPreview}
      <!-- Markdown preview pane (sanitized HTML). -->
      <div
        bind:this={previewEl}
        class="absolute inset-0 overflow-y-auto px-8 py-6 prose-doc"
        style:font-family={session.fontFamily === 'sans' ? 'system-ui, sans-serif' : session.fontFamily === 'serif' ? 'Georgia, serif' : 'ui-monospace, monospace'}
        style:font-size={session.fontSize + 'px'}
        style:line-height={String(session.lineHeight)}
        style:color={session.theme === 'light' ? '#1a1a1a' : session.theme === 'sepia' ? '#3b2f1d' : '#d8d8d8'}
        style:background={session.theme === 'light' ? '#ffffff' : session.theme === 'sepia' ? '#f4ecd8' : '#1a1a1a'}
      >
        <!-- eslint-disable-next-line svelte/no-at-html-tags -->
        {@html renderedHTML}
      </div>
    {/if}
    {#if session.loading}
      <div class="absolute inset-0 flex items-center justify-center bg-surface text-sm text-fg-muted">
        <p>Loading {ext.toUpperCase()}…</p>
      </div>
    {:else if session.loadError}
      <div class="absolute inset-0 flex flex-col items-center justify-center gap-2 bg-surface p-8 text-center text-sm text-danger">
        <p>{session.loadError}</p>
        <a href={fileUrl} class="text-accent underline" target="_blank">Download original</a>
      </div>
    {/if}

    <!-- Floating selection toolbar. Appears when text is selected
         and we have a viewport anchor for it. Positioned above the
         selection's top-right; the parent flex column is the
         offset reference. -->
    {#if selectionExists() && session.selectionAnchor && !commentDraft}
      <div
        class="pointer-events-auto fixed z-50 flex items-center gap-1 rounded-md border border-border bg-surface-elevated px-1.5 py-1 text-xs shadow-2xl"
        style:left={`${Math.max(8, session.selectionAnchor.x - 110)}px`}
        style:top={`${Math.max(8, session.selectionAnchor.y - 44)}px`}
        role="toolbar"
        aria-label="Annotation tools"
      >
        <div class="flex items-center gap-0.5">
          {#each HIGHLIGHT_SWATCHES as c (c)}
            <button
              type="button"
              onclick={() => (pickedColor = c)}
              class="h-5 w-5 rounded-full border-2 transition-transform hover:scale-110"
              class:border-fg={pickedColor === c}
              class:border-transparent={pickedColor !== c}
              style:background-color={c}
              title="Color"
              aria-label="Color {c}"
            ></button>
          {/each}
        </div>
        <span class="mx-1 h-4 w-px bg-border"></span>
        <button type="button" onclick={() => applyStyle('highlight')} title="Highlight" aria-label="Highlight" class="rounded p-1 hover:bg-surface">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m9 11-6 6v3h3l6-6"/><path d="m22 12-4.6 4.6a2 2 0 0 1-2.8 0l-5.2-5.2a2 2 0 0 1 0-2.8L14 4"/></svg>
        </button>
        <button type="button" onclick={() => applyStyle('strikethrough')} title="Strikethrough" aria-label="Strikethrough" class="rounded p-1 hover:bg-surface">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M16 4H9a3 3 0 0 0-2.83 4"/><path d="M14 12a4 4 0 0 1 0 8H6"/><line x1="4" y1="12" x2="20" y2="12"/></svg>
        </button>
        <button type="button" onclick={() => applyStyle('underline')} title="Underline" aria-label="Underline" class="rounded p-1 hover:bg-surface">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 4v6a6 6 0 0 0 12 0V4"/><line x1="4" y1="20" x2="20" y2="20"/></svg>
        </button>
        <button type="button" onclick={() => openCommentDraft('comment')} title="Comment" aria-label="Comment" class="rounded p-1 hover:bg-surface">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>
        </button>
        <button type="button" onclick={() => openCommentDraft('note')} title="Sticky note" aria-label="Sticky note" class="rounded p-1 hover:bg-surface">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"/><polyline points="17 21 17 13 7 13 7 21"/></svg>
        </button>
      </div>
    {/if}

    <!-- Comment / note draft prompt — replaces the toolbar while the
         user types the body. ESC cancels, Enter (without shift) saves. -->
    {#if commentDraft && session.selectionAnchor}
      <div
        class="pointer-events-auto fixed z-50 w-72 rounded-md border border-accent bg-surface-elevated p-2 shadow-2xl"
        style:left={`${Math.max(8, session.selectionAnchor.x - 140)}px`}
        style:top={`${Math.max(8, session.selectionAnchor.y - 110)}px`}
      >
        <div class="mb-1 flex items-center justify-between text-[10px] uppercase tracking-wider text-fg-muted">
          <span>{commentDraft.style === 'note' ? 'Sticky note' : 'Comment'}</span>
          <span class="font-mono">
            {session.selection?.startLine}:{session.selection?.startCol}
          </span>
        </div>
        <textarea
          bind:value={commentDraft.body}
          placeholder="Type your {commentDraft.style}…"
          rows="3"
          autofocus
          onkeydown={(e) => {
            if (e.key === 'Escape') cancelDraft();
            else if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); void submitDraft(); }
          }}
          class="w-full resize-none rounded border border-border bg-surface px-2 py-1 text-xs text-fg focus:border-accent focus:outline-none"
        ></textarea>
        <div class="mt-1 flex items-center justify-between">
          <div class="flex items-center gap-0.5">
            {#each HIGHLIGHT_SWATCHES as c (c)}
              <button
                type="button"
                onclick={() => (pickedColor = c)}
                class="h-4 w-4 rounded-full border-2 transition-transform hover:scale-110"
                class:border-fg={pickedColor === c}
                class:border-transparent={pickedColor !== c}
                style:background-color={c}
                aria-label="Color {c}"
              ></button>
            {/each}
          </div>
          <div class="flex items-center gap-1">
            <button type="button" onclick={cancelDraft} class="rounded border border-border bg-surface px-2 py-0.5 text-[10px] text-fg hover:border-accent">Cancel</button>
            <button type="button" onclick={() => void submitDraft()} class="rounded border border-accent bg-accent/15 px-2 py-0.5 text-[10px] font-medium text-fg hover:bg-accent/25">Save</button>
          </div>
        </div>
      </div>
    {/if}
  </div>
</div>

<style>
  /* Lightweight prose tuning for the markdown preview. CodeMirror
     doesn't render HTML so we own this stylesheet locally. */
  :global(.prose-doc) { max-width: 70rem; margin: 0 auto; }
  :global(.prose-doc h1) { font-size: 1.75em; margin: 1em 0 0.5em; font-weight: 700; }
  :global(.prose-doc h2) { font-size: 1.4em; margin: 1em 0 0.5em; font-weight: 700; }
  :global(.prose-doc h3) { font-size: 1.2em; margin: 0.8em 0 0.4em; font-weight: 600; }
  :global(.prose-doc p)  { margin: 0.5em 0; line-height: inherit; }
  :global(.prose-doc ul), :global(.prose-doc ol) { padding-left: 1.5em; margin: 0.5em 0; }
  :global(.prose-doc li) { margin: 0.15em 0; }
  :global(.prose-doc blockquote) { border-left: 3px solid currentColor; opacity: 0.7; padding-left: 0.8em; margin: 0.6em 0; }
  :global(.prose-doc code) { padding: 0.1em 0.35em; background: rgba(127,127,127,0.18); border-radius: 0.2em; font-family: ui-monospace, monospace; font-size: 0.95em; }
  :global(.prose-doc pre) { padding: 0.8em 1em; background: rgba(127,127,127,0.14); border-radius: 0.4em; overflow-x: auto; margin: 0.6em 0; }
  :global(.prose-doc pre code) { padding: 0; background: transparent; }
  :global(.prose-doc a) { color: var(--color-accent, #8ab4f8); text-decoration: underline; }
  :global(.prose-doc table) { border-collapse: collapse; margin: 0.6em 0; }
  :global(.prose-doc th), :global(.prose-doc td) { border: 1px solid rgba(127,127,127,0.3); padding: 0.3em 0.6em; }
  :global(.prose-doc img) { max-width: 100%; height: auto; }
  :global(.prose-doc hr) { border: 0; border-top: 1px solid rgba(127,127,127,0.3); margin: 1em 0; }
</style>
