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

  // ── Language detection ────────────────────────────────────────
  // Mapping from file extension to CodeMirror language id. Used for
  // grammar loading + the Stats badge.
  const EXT_LANG: Record<string, string> = {
    md: 'markdown', markdown: 'markdown', mdx: 'markdown',
    py: 'python', pyi: 'python',
    js: 'javascript', mjs: 'javascript', cjs: 'javascript',
    jsx: 'javascript-jsx',
    ts: 'typescript', tsx: 'typescript-jsx',
    json: 'json', jsonc: 'json',
    yaml: 'yaml', yml: 'yaml',
    html: 'html', htm: 'html', vue: 'html', svelte: 'html',
    css: 'css', scss: 'css', sass: 'css', less: 'css',
    sql: 'sql',
    go: 'go',
    rs: 'rust',
    c: 'cpp', h: 'cpp', cpp: 'cpp', cc: 'cpp', cxx: 'cpp', hpp: 'cpp', hh: 'cpp',
    xml: 'xml', plist: 'xml', svg: 'xml',
    // legacy-modes set (loaded via @codemirror/legacy-modes when picked)
    lua: 'lua',
    sh: 'shell', bash: 'shell', zsh: 'shell',
    rb: 'ruby',
    pl: 'perl', pm: 'perl',
    toml: 'toml',
    ini: 'properties', cfg: 'properties', conf: 'properties', env: 'properties', properties: 'properties',
    diff: 'diff', patch: 'diff',
    dockerfile: 'dockerfile',
    makefile: 'cmake',
    mk: 'cmake',
  };
  const languageId = $derived(EXT_LANG[ext] ?? 'plain');

  async function loadLanguage(id: string) {
    // First-party lang packs.
    try {
      switch (id) {
        case 'markdown': { const m = await import('@codemirror/lang-markdown'); return m.markdown(); }
        case 'python':   { const m = await import('@codemirror/lang-python');   return m.python(); }
        case 'javascript': { const m = await import('@codemirror/lang-javascript'); return m.javascript(); }
        case 'javascript-jsx': { const m = await import('@codemirror/lang-javascript'); return m.javascript({ jsx: true }); }
        case 'typescript': { const m = await import('@codemirror/lang-javascript'); return m.javascript({ typescript: true }); }
        case 'typescript-jsx': { const m = await import('@codemirror/lang-javascript'); return m.javascript({ typescript: true, jsx: true }); }
        case 'json': { const m = await import('@codemirror/lang-json'); return m.json(); }
        case 'yaml': { const m = await import('@codemirror/lang-yaml'); return m.yaml(); }
        case 'html': { const m = await import('@codemirror/lang-html'); return m.html(); }
        case 'css':  { const m = await import('@codemirror/lang-css');  return m.css(); }
        case 'sql':  { const m = await import('@codemirror/lang-sql');  return m.sql(); }
        case 'go':   { const m = await import('@codemirror/lang-go');   return m.go(); }
        case 'rust': { const m = await import('@codemirror/lang-rust'); return m.rust(); }
        case 'cpp':  { const m = await import('@codemirror/lang-cpp');  return m.cpp(); }
        case 'xml':  { const m = await import('@codemirror/lang-xml');  return m.xml(); }
      }
      // Legacy-modes — stream-language wrapped grammars for the
      // dialects without a first-party pack. Imports are cheap; one
      // module per language family.
      const legacy = await import('@codemirror/legacy-modes/mode/lua');
      const stream = await import('@codemirror/language');
      switch (id) {
        case 'lua':        return stream.StreamLanguage.define(legacy.lua);
        case 'shell':      { const m = await import('@codemirror/legacy-modes/mode/shell');      return stream.StreamLanguage.define(m.shell); }
        case 'ruby':       { const m = await import('@codemirror/legacy-modes/mode/ruby');       return stream.StreamLanguage.define(m.ruby); }
        case 'perl':       { const m = await import('@codemirror/legacy-modes/mode/perl');       return stream.StreamLanguage.define(m.perl); }
        case 'toml':       { const m = await import('@codemirror/legacy-modes/mode/toml');       return stream.StreamLanguage.define(m.toml); }
        case 'properties': { const m = await import('@codemirror/legacy-modes/mode/properties'); return stream.StreamLanguage.define(m.properties); }
        case 'diff':       { const m = await import('@codemirror/legacy-modes/mode/diff');       return stream.StreamLanguage.define(m.diff); }
        case 'dockerfile': { const m = await import('@codemirror/legacy-modes/mode/dockerfile'); return stream.StreamLanguage.define(m.dockerFile); }
        case 'cmake':      { const m = await import('@codemirror/legacy-modes/mode/cmake');      return stream.StreamLanguage.define(m.cmake); }
      }
    } catch (e) {
      // Grammar import failure isn't fatal — fall through to plain.
      // eslint-disable-next-line no-console
      console.warn('doc: language pack load failed', id, e);
    }
    return null;
  }

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
      { lintKeymap },
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
    };

    cleanupFn = () => { view.destroy(); host = null; };
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

  // HUD shows the language tag + line count.
  $effect(() => {
    const lng = session.languageId !== 'plain' ? session.languageId : ext;
    controller.hudExtra = lng.toUpperCase();
  });

  const showPreview = $derived(
    session.renderMarkdown && languageId === 'markdown' && renderedHTML.length > 0,
  );
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
