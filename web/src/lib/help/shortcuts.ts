// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The keyboard-shortcut cheatsheet, as data.
//
// One catalogue, two surfaces: /account/shortcuts (any signed-in user)
// and /admin/help/shortcuts (operators, who cannot be assumed to also
// browse /account). Both render it through
// $components/ShortcutsCheatsheet.svelte, so the list is written once.
//
// EVERY ROW BELOW CORRESPONDS TO A HANDLER THAT EXISTS. The bar for
// adding a row is "point at the file:line that binds it" — a cheatsheet
// that documents a key we do not handle is worse than no cheatsheet,
// because the user blames themselves. Source handlers:
//
//   AssetPlaylist.svelte              handleKeydown()   viewer navigation
//   viewers/AssetViewer.svelte        handleKey()       playback / loop / view
//   viewers/EpubView.svelte           onKey()           ebook chapters
//   viewers/SpriteCanvas.svelte       onSpriteKey()     sprite animation
//   whiteboard/WhiteboardCanvas.svelte handleKey()      whiteboard tools + zoom
//   whiteboard/BrushCanvas.svelte     onDocKey()        whiteboard clipboard
//   SearchBar.svelte                  onKey()           search suggestions
//
// There is no app-global keymap module and no command palette — every
// binding is scoped to a surface, which is why the groups below are
// named after surfaces rather than after actions.

export interface ShortcutRow {
  /** Rendered as <kbd> chips, in order. */
  keys: string[];
  /** i18n key for the action label. */
  descKey: string;
}

export interface ShortcutGroup {
  id: string;
  titleKey: string;
  /** Optional caveat rendered under the group title (scope, conflicts). */
  noteKey?: string;
  rows: ShortcutRow[];
}

/** Modifier chip. The handlers all test `ctrlKey || metaKey`, so the
 *  two are genuinely interchangeable and the chip says so. */
const MOD = 'Ctrl/⌘';

export const SHORTCUT_GROUPS: ShortcutGroup[] = [
  {
    id: 'viewer_nav',
    titleKey: 'shortcuts.group.viewer_nav',
    noteKey: 'shortcuts.note.viewer_nav',
    rows: [
      { keys: ['←', '→'], descKey: 'shortcuts.viewer.sibling' },
      { keys: ['↑', '↓'], descKey: 'shortcuts.viewer.within' },
      { keys: ['I'], descKey: 'shortcuts.viewer.toggle_pane' },
      { keys: ['Esc'], descKey: 'shortcuts.viewer.close' },
    ],
  },
  {
    id: 'playback',
    titleKey: 'shortcuts.group.playback',
    noteKey: 'shortcuts.note.playback',
    rows: [
      { keys: ['Space'], descKey: 'shortcuts.playback.play_pause' },
      { keys: ['L'], descKey: 'shortcuts.playback.play' },
      { keys: ['K'], descKey: 'shortcuts.playback.pause' },
      { keys: ['J'], descKey: 'shortcuts.playback.step_back' },
      { keys: [','], descKey: 'shortcuts.playback.frame_back' },
      { keys: ['.'], descKey: 'shortcuts.playback.frame_fwd' },
      { keys: ['1', '2', '3', '4', '5'], descKey: 'shortcuts.playback.rate' },
      { keys: ['G'], descKey: 'shortcuts.playback.goto_frame' },
    ],
  },
  {
    id: 'loop',
    titleKey: 'shortcuts.group.loop',
    rows: [
      { keys: ['I'], descKey: 'shortcuts.loop.in' },
      { keys: ['O'], descKey: 'shortcuts.loop.out' },
      { keys: ['Backspace'], descKey: 'shortcuts.loop.clear' },
    ],
  },
  {
    id: 'view',
    titleKey: 'shortcuts.group.view',
    rows: [
      { keys: ['F'], descKey: 'shortcuts.view.fullscreen' },
      { keys: ['R'], descKey: 'shortcuts.view.reset' },
      { keys: ['T'], descKey: 'shortcuts.view.tile' },
      { keys: [MOD, '+ wheel'], descKey: 'shortcuts.view.zoom' },
    ],
  },
  {
    id: 'ebook',
    titleKey: 'shortcuts.group.ebook',
    rows: [{ keys: ['←', '→'], descKey: 'shortcuts.ebook.chapter' }],
  },
  {
    id: 'sprite',
    titleKey: 'shortcuts.group.sprite',
    rows: [
      { keys: ['Space'], descKey: 'shortcuts.sprite.play_pause' },
      { keys: [','], descKey: 'shortcuts.sprite.prev_frame' },
      { keys: ['.'], descKey: 'shortcuts.sprite.next_frame' },
      { keys: ['F3'], descKey: 'shortcuts.sprite.onion' },
    ],
  },
  {
    id: 'whiteboard_tools',
    titleKey: 'shortcuts.group.whiteboard_tools',
    rows: [
      { keys: ['V'], descKey: 'shortcuts.wb.select' },
      { keys: ['P'], descKey: 'shortcuts.wb.pen' },
      { keys: ['M'], descKey: 'shortcuts.wb.marker' },
      { keys: ['H'], descKey: 'shortcuts.wb.highlighter' },
      { keys: ['E'], descKey: 'shortcuts.wb.eraser' },
      { keys: ['L'], descKey: 'shortcuts.wb.line' },
      { keys: ['A'], descKey: 'shortcuts.wb.arrow' },
      { keys: ['R'], descKey: 'shortcuts.wb.rect' },
      { keys: ['O'], descKey: 'shortcuts.wb.ellipse' },
      { keys: ['G'], descKey: 'shortcuts.wb.triangle' },
      { keys: ['B'], descKey: 'shortcuts.wb.bucket' },
      { keys: ['I'], descKey: 'shortcuts.wb.eyedropper' },
      { keys: ['T'], descKey: 'shortcuts.wb.text' },
      { keys: ['Q'], descKey: 'shortcuts.wb.lasso' },
      { keys: ['C'], descKey: 'shortcuts.wb.crop' },
      { keys: ['X'], descKey: 'shortcuts.wb.swap_colors' },
    ],
  },
  {
    id: 'whiteboard_edit',
    titleKey: 'shortcuts.group.whiteboard_edit',
    noteKey: 'shortcuts.note.whiteboard_edit',
    rows: [
      { keys: [`${MOD}+Z`], descKey: 'shortcuts.wb.undo' },
      { keys: [`${MOD}+Shift+Z`, `${MOD}+Y`], descKey: 'shortcuts.wb.redo' },
      { keys: [`${MOD}+A`], descKey: 'shortcuts.wb.select_all' },
      { keys: [`${MOD}+C`, `${MOD}+X`, `${MOD}+V`], descKey: 'shortcuts.wb.clipboard' },
      { keys: [`${MOD}+D`], descKey: 'shortcuts.wb.duplicate' },
      { keys: ['Delete', 'Backspace'], descKey: 'shortcuts.wb.delete' },
      { keys: ['Space', '+ drag'], descKey: 'shortcuts.wb.pan' },
      { keys: ['F'], descKey: 'shortcuts.wb.fit' },
      { keys: ['0'], descKey: 'shortcuts.wb.reset_zoom' },
      { keys: ['+', '−'], descKey: 'shortcuts.wb.zoom' },
      { keys: ['Esc'], descKey: 'shortcuts.wb.exit' },
    ],
  },
  {
    id: 'search',
    titleKey: 'shortcuts.group.search',
    rows: [
      { keys: ['↑', '↓'], descKey: 'shortcuts.search.nav' },
      { keys: ['Enter'], descKey: 'shortcuts.search.go' },
      { keys: ['Esc'], descKey: 'shortcuts.search.close' },
    ],
  },
  {
    id: 'text',
    titleKey: 'shortcuts.group.text',
    rows: [
      { keys: ['Enter', ','], descKey: 'shortcuts.text.add_tag' },
      { keys: ['Backspace'], descKey: 'shortcuts.text.remove_tag' },
      { keys: ['Enter'], descKey: 'shortcuts.text.send' },
      { keys: ['Shift+Enter'], descKey: 'shortcuts.text.newline' },
    ],
  },
];
