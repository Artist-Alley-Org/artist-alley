// CodeMirror 6 decoration extension that paints the doc-viewer's
// text-range annotations. Single source of truth: the StateField
// reads the externally-provided annotation list (via the
// setDocAnnotations StateEffect) and turns each entry into either a
// MARK decoration (highlight / strikethrough / underline / comment)
// or a WIDGET decoration (note — a tiny sticky icon at the start
// position when the range is zero-width).
//
// The mapping from annotation style → CSS class lives here so the
// editor's theme + the panel agree without a lookup table elsewhere.
//
// Decorations re-evaluate when the document changes (the StateField
// rebuilds in update()), so the underlying char offsets stay aligned
// even after a future edit-mode edit. Out-of-bounds anchors clamp
// to the document's last position rather than throwing.

import { StateField, StateEffect, RangeSetBuilder } from '@codemirror/state';
import { Decoration, EditorView, WidgetType } from '@codemirror/view';
import type { DecorationSet } from '@codemirror/view';

export interface DocAnnotationView {
  id: string;
  style: 'highlight' | 'strikethrough' | 'underline' | 'comment' | 'note';
  color: string;
  startLine: number;
  startCol: number;
  endLine: number;
  endCol: number;
  resolved: boolean;
}

export const setDocAnnotations = StateEffect.define<DocAnnotationView[]>();

class NoteWidget extends WidgetType {
  constructor(
    readonly id: string,
    readonly color: string,
    readonly resolved: boolean,
  ) { super(); }
  eq(other: NoteWidget): boolean {
    return other.id === this.id && other.color === this.color && other.resolved === this.resolved;
  }
  toDOM(): HTMLElement {
    const el = document.createElement('span');
    el.className = 'aa-doc-anno-note';
    el.setAttribute('data-annotation-id', this.id);
    el.style.setProperty('--anno-color', this.color);
    el.title = this.resolved ? 'Resolved note' : 'Note';
    el.textContent = '✎';
    if (this.resolved) el.classList.add('aa-doc-anno-resolved');
    return el;
  }
}

function lineColToPos(doc: import('@codemirror/state').Text, line: number, col: number): number {
  const totalLines = doc.lines;
  const ln = Math.max(1, Math.min(line, totalLines));
  const lineObj = doc.line(ln);
  const c = Math.max(0, Math.min(col, lineObj.length));
  return lineObj.from + c;
}

function buildDecorations(
  doc: import('@codemirror/state').Text,
  annotations: DocAnnotationView[],
): DecorationSet {
  // Range builder requires ranges added in `from` ascending order.
  const sorted = [...annotations].sort((a, b) => {
    if (a.startLine !== b.startLine) return a.startLine - b.startLine;
    return a.startCol - b.startCol;
  });
  const b = new RangeSetBuilder<Decoration>();
  for (const a of sorted) {
    try {
      const from = lineColToPos(doc, a.startLine, a.startCol);
      const to = lineColToPos(doc, a.endLine, a.endCol);
      if (from === to) {
        // Zero-width annotation → widget (note glyph).
        b.add(from, from, Decoration.widget({
          widget: new NoteWidget(a.id, a.color, a.resolved),
          side: 1,
        }));
        continue;
      }
      const klass = `aa-doc-anno aa-doc-anno-${a.style}` + (a.resolved ? ' aa-doc-anno-resolved' : '');
      b.add(Math.min(from, to), Math.max(from, to), Decoration.mark({
        class: klass,
        attributes: {
          'data-annotation-id': a.id,
          'style': `--anno-color:${a.color}`,
        },
      }));
    } catch {
      // Out-of-bounds anchor — skip silently. Future edit-mode
      // will adjust anchors on document changes; for now we just
      // drop entries that don't fit anymore.
    }
  }
  return b.finish();
}

export const annotationField = StateField.define<DecorationSet>({
  create: () => Decoration.none,
  update(value, tr) {
    let next = value.map(tr.changes);
    for (const ef of tr.effects) {
      if (ef.is(setDocAnnotations)) {
        next = buildDecorations(tr.state.doc, ef.value);
      }
    }
    return next;
  },
  provide: (f) => EditorView.decorations.from(f),
});

/** Stylesheet for the decoration classes. Loaded as a theme so the
 *  editor picks it up automatically. Color comes from the per-
 *  annotation `--anno-color` custom property the spec stamps onto
 *  the span; this lets one CSS rule cover every swatch the user
 *  picks. */
export const annotationTheme = EditorView.baseTheme({
  '.aa-doc-anno': {
    borderRadius: '2px',
    paddingTop: '0.5px',
    paddingBottom: '0.5px',
    transition: 'background 120ms ease',
  },
  '.aa-doc-anno-highlight': {
    background: 'color-mix(in srgb, var(--anno-color), transparent 40%)',
  },
  '.aa-doc-anno-comment': {
    background: 'color-mix(in srgb, var(--anno-color), transparent 65%)',
    boxShadow: 'inset 0 -2px 0 var(--anno-color)',
    cursor: 'pointer',
  },
  '.aa-doc-anno-strikethrough': {
    textDecoration: 'line-through',
    textDecorationColor: 'var(--anno-color)',
    textDecorationThickness: '2px',
  },
  '.aa-doc-anno-underline': {
    textDecoration: 'underline',
    textDecorationColor: 'var(--anno-color)',
    textDecorationThickness: '2px',
    textUnderlineOffset: '3px',
  },
  '.aa-doc-anno-resolved': {
    opacity: '0.45',
  },
  '.aa-doc-anno-note': {
    display: 'inline-block',
    background: 'var(--anno-color)',
    color: '#000',
    padding: '0 4px',
    marginLeft: '2px',
    marginRight: '2px',
    borderRadius: '3px',
    fontSize: '0.85em',
    lineHeight: '1.1',
    fontWeight: 'bold',
    cursor: 'pointer',
    verticalAlign: 'baseline',
  },
});
