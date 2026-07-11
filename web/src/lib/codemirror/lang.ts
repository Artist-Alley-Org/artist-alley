// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Shared CodeMirror grammar loader. Both DocView (text-asset reader)
// and ArchiveView (in-archive entry preview) need the same extension
// → language mapping + dynamic-imported grammar pack — extracted so
// adding a language family touches one place.
//
// Lazy imports keep each grammar out of the main bundle. CodeMirror
// 6 ships first-party packs for the popular set and a "legacy-modes"
// stream-language wrapper for the long tail (lua / shell / toml…).

import type { Extension } from '@codemirror/state';

// File extension → language id used by loadLanguage. Add new ext
// mappings here; loadLanguage handles the grammar dispatch.
export const EXT_LANG: Record<string, string> = {
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

export function languageIdForExt(ext: string | null | undefined): string {
  if (!ext) return 'plain';
  const e = ext.toLowerCase().replace(/^\./, '');
  return EXT_LANG[e] ?? 'plain';
}

/** Dynamically import the CodeMirror grammar for `id`. Returns null
 *  when the id has no grammar (plain text falls back to no highlight). */
export async function loadLanguage(id: string): Promise<Extension | null> {
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
    const stream = await import('@codemirror/language');
    switch (id) {
      case 'lua':        { const m = await import('@codemirror/legacy-modes/mode/lua');        return stream.StreamLanguage.define(m.lua); }
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
    // eslint-disable-next-line no-console
    console.warn('codemirror: language pack load failed', id, e);
  }
  return null;
}
