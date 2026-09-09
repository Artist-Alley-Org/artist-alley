// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Same-batch companion reconciliation (#1408).
//
// ## The bug this removes
//
// Dropping a model and its textures together produced N unrelated
// assets. Every `File` in the drop became its own upload row, nothing
// looked at the batch, and the model's `companions` stayed empty — so
// the upload succeeded, the render job succeeded, and the model came
// out grey. #754 made the model SAY what it was missing; this makes the
// same drop SATISFY it.
//
// ## The rule: the model's DECLARED references are authoritative
//
// This module never asks "does this look like a texture?". Classifying
// by type would attach an unrelated illustration to a model that never
// named it, and would still miss a `.bin` buffer. The only thing that
// decides is the list the model itself declares, which the SERVER
// parses (`GET /assets/{id}/companion-requirements`) — the browser does
// not re-implement format3d, for the same reason the create page asks
// the server for the asset type instead of computing it: a second
// expression of a rule is free to disagree with the first.
//
// The server satisfies a requirement by EXACT string match of a stored
// companion path against a declared path (assets/companions.go). So
// every path this module produces is a declared path copied verbatim,
// never a path it invented.
//
// ## Two passes, and a refusal
//
//  1. PATH-EXACT. A declared reference is relative to the model file's
//     directory, so `wood/model.gltf` declaring `textures/diffuse.png`
//     expects a batch member at `wood/textures/diffuse.png`. When the
//     browser gave us real directory information (a directory drop read
//     through `webkitGetAsEntry`, or a `webkitdirectory` input), that
//     comparison is exact and `wood/diffuse.png` vs `metal/diffuse.png`
//     resolve to different files with no ambiguity at all.
//
//  2. BASENAME, ONLY WHEN 1:1. A flat multi-file drop carries no
//     directory information — `DataTransfer.files` is a flat FileList
//     and `webkitRelativePath` is empty. Collapsing to basename
//     matching unconditionally is exactly how `img.jpg` gets attached
//     to the model that wanted `metal/img.jpg` when it belonged to the
//     one that wanted `wood/img.jpg`. So a basename match is accepted
//     only when it is unambiguous in BOTH directions: one candidate
//     file for that basename, and one distinct declared path for it
//     across every model in the batch.
//
//  3. Anything left is REFUSED, not guessed. A colliding basename is
//     `undecided/ambiguous` and goes to the artist. A leftover file in
//     a batch that contains a model whose declaration we could only
//     read PARTIALLY (an .obj names .mtl libraries; each .mtl names its
//     own textures, one level further down than the uploaded bytes can
//     see) is `undecided/incomplete` — because in that batch "this file
//     is unrelated" is a claim we have no basis for. Only when every
//     model's declaration is COMPLETE is a non-matching file called
//     unrelated and sent on to become its own asset, which is what the
//     upload flow does with an ordinary file and must keep doing.
//
// A candidate matching a path that TWO models both declare is not
// ambiguous: two models can legitimately share a texture, and each gets
// its own companion row. Ambiguity is about not knowing WHICH file the
// artist meant, not about a file being wanted twice.

/** One file offered to the batch, with whatever path the browser gave. */
export interface CandidateFile {
  id: string;
  /** Batch-relative path, normalised. Falls back to the bare filename. */
  path: string;
  /** True only when the browser actually supplied directory information. */
  hasPath: boolean;
}

/** One model row in the batch and what the server says it declares. */
export interface ModelTarget {
  rowId: string;
  /** Batch-relative path of the model file itself. */
  modelPath: string;
  /** Declared companion paths, verbatim from the server. */
  declared: string[];
  /**
   * True when the declaration is the WHOLE list. False for an .obj
   * (`partial`), for an unreadable parse, and for a format we have no
   * reader for — in all three the batch cannot conclude that a leftover
   * file is unrelated.
   */
  complete: boolean;
}

export interface CompanionAssignment {
  candidateId: string;
  rowId: string;
  /** The declared path, verbatim — this is what the server matches on. */
  path: string;
  via: 'path' | 'basename';
}

export interface UndecidedCandidate {
  candidateId: string;
  reason: 'ambiguous' | 'incomplete';
  /** Every model this file could plausibly belong to, with a suggested path. */
  options: { rowId: string; path: string }[];
}

export interface ReconcileResult {
  assignments: CompanionAssignment[];
  undecided: UndecidedCandidate[];
  /** Candidate ids that belong to no model and become their own assets. */
  unrelated: string[];
}

// ---- Path helpers ---------------------------------------------------------
//
// Deliberately string-only. These normalise what a BROWSER handed us
// (backslashes from a Windows-authored tree, a stray `./`, a doubled
// separator) into the same shape the server's cleaned declared paths
// are in, so the two can be compared as strings. They do NOT
// percent-decode: a declared URI arrives already decoded from
// format3d's `cleanCompanionURI`, while a local filename is literal, so
// decoding here would corrupt a file genuinely named `a%20b.png`.

/** Forward slashes, no leading `/` or `./`, `.`/`..` segments resolved. */
export function normalizeRelPath(p: string): string {
  const raw = (p ?? '').replace(/\\/g, '/');
  const out: string[] = [];
  for (const seg of raw.split('/')) {
    if (seg === '' || seg === '.') continue;
    if (seg === '..') {
      // A `..` that would escape the root is dropped rather than kept:
      // the server refuses such a reference outright (cleanCompanionURI),
      // so a path that keeps one can never match a declared path anyway.
      out.pop();
      continue;
    }
    out.push(seg);
  }
  return out.join('/');
}

export function baseName(p: string): string {
  const n = normalizeRelPath(p);
  const i = n.lastIndexOf('/');
  return i < 0 ? n : n.slice(i + 1);
}

/** Directory part of a relative path; '' when there is none. */
export function dirName(p: string): string {
  const n = normalizeRelPath(p);
  const i = n.lastIndexOf('/');
  return i < 0 ? '' : n.slice(0, i);
}

/** `dir` + `rel`, normalised. `rel` is relative to `dir`. */
export function joinRel(dir: string, rel: string): string {
  const d = normalizeRelPath(dir);
  const r = normalizeRelPath(rel);
  if (!d) return r;
  if (!r) return d;
  return normalizeRelPath(`${d}/${r}`);
}

/** `full` expressed relative to `dir`, or `full` when it is not under `dir`. */
export function relativeTo(dir: string, full: string): string {
  const d = normalizeRelPath(dir);
  const f = normalizeRelPath(full);
  if (!d) return f;
  if (f === d) return '';
  if (f.startsWith(`${d}/`)) return f.slice(d.length + 1);
  return f;
}

// ---- The reconciliation ---------------------------------------------------

interface DeclaredSlot {
  rowId: string;
  /** The declared path verbatim — what gets sent to the server. */
  declared: string;
  /** Where that reference points, as a batch-relative path. */
  expected: string;
  base: string;
}

export function reconcileCompanions(
  models: ModelTarget[],
  candidates: CandidateFile[],
): ReconcileResult {
  const assignments: CompanionAssignment[] = [];
  const undecided: UndecidedCandidate[] = [];
  const unrelated: string[] = [];

  if (models.length === 0 || candidates.length === 0) {
    return { assignments, undecided, unrelated: candidates.map((c) => c.id) };
  }

  const slots: DeclaredSlot[] = [];
  for (const m of models) {
    const dir = dirName(m.modelPath);
    for (const d of m.declared) {
      const declared = normalizeRelPath(d);
      if (!declared) continue;
      slots.push({
        rowId: m.rowId,
        declared,
        expected: joinRel(dir, declared),
        base: baseName(declared),
      });
    }
  }

  const assigned = new Set<string>();
  const satisfied = new Set<DeclaredSlot>();

  // ---- Pass 1: path-exact -------------------------------------------------
  //
  // Grouped by path on both sides first, because the interesting failure
  // is a COLLISION and a collision is only visible as a cardinality.
  const candsByPath = new Map<string, CandidateFile[]>();
  for (const c of candidates) {
    const key = normalizeRelPath(c.path);
    const list = candsByPath.get(key);
    if (list) list.push(c);
    else candsByPath.set(key, [c]);
  }

  const ambiguousBy = new Map<string, { rowId: string; path: string }[]>();
  const markAmbiguous = (candidateId: string, options: { rowId: string; path: string }[]) => {
    const prev = ambiguousBy.get(candidateId) ?? [];
    for (const o of options) {
      if (!prev.some((p) => p.rowId === o.rowId && p.path === o.path)) prev.push(o);
    }
    ambiguousBy.set(candidateId, prev);
  };

  for (const slot of slots) {
    const hits = candsByPath.get(slot.expected);
    if (!hits || hits.length === 0) continue;
    if (hits.length > 1) {
      // Two files in the same batch at the same relative path. Nothing
      // distinguishes them, so nothing here may pick one.
      for (const h of hits) markAmbiguous(h.id, [{ rowId: slot.rowId, path: slot.declared }]);
      continue;
    }
    const c = hits[0];
    assignments.push({ candidateId: c.id, rowId: slot.rowId, path: slot.declared, via: 'path' });
    assigned.add(c.id);
    satisfied.add(slot);
  }

  // ---- Pass 2: basename, only when 1:1 ------------------------------------
  //
  // Runs over what pass 1 could not place. This is the flat-drop path,
  // where the browser gave nothing but names.
  interface BaseGroup {
    slots: DeclaredSlot[];
    cands: CandidateFile[];
  }
  const byBase = new Map<string, BaseGroup>();
  const group = (b: string): BaseGroup => {
    let g = byBase.get(b);
    if (!g) {
      g = { slots: [], cands: [] };
      byBase.set(b, g);
    }
    return g;
  };
  for (const slot of slots) {
    if (satisfied.has(slot)) continue;
    group(slot.base).slots.push(slot);
  }
  for (const c of candidates) {
    if (assigned.has(c.id)) continue;
    if (ambiguousBy.has(c.id)) continue;
    group(baseName(c.path)).cands.push(c);
  }

  for (const [, g] of byBase) {
    if (g.slots.length === 0 || g.cands.length === 0) continue;
    // Distinct DECLARED targets, not distinct slots: two models naming
    // the same relative path want the same file, and both should get it.
    const distinct = new Set(g.slots.map((s) => s.expected));
    if (g.cands.length === 1 && distinct.size === 1) {
      const c = g.cands[0];
      for (const slot of g.slots) {
        assignments.push({
          candidateId: c.id,
          rowId: slot.rowId,
          path: slot.declared,
          via: 'basename',
        });
        satisfied.add(slot);
      }
      assigned.add(c.id);
      continue;
    }
    // Anything else is a genuine collision: same name, different
    // declared locations, or more than one file offering that name.
    // The artist is asked; nothing is guessed.
    const options = g.slots.map((s) => ({ rowId: s.rowId, path: s.declared }));
    for (const c of g.cands) markAmbiguous(c.id, options);
  }

  for (const [candidateId, options] of ambiguousBy) {
    if (assigned.has(candidateId)) continue;
    undecided.push({ candidateId, reason: 'ambiguous', options });
    assigned.add(candidateId);
  }

  // ---- Pass 3: the leftovers ----------------------------------------------
  //
  // "Unrelated" is a CLAIM. It is only true when every model in the
  // batch told us its whole list; an .obj did not.
  const incomplete = models.filter((m) => !m.complete);
  for (const c of candidates) {
    if (assigned.has(c.id)) continue;
    if (incomplete.length > 0) {
      undecided.push({
        candidateId: c.id,
        reason: 'incomplete',
        options: incomplete.map((m) => ({
          rowId: m.rowId,
          // Suggest where the file SITS relative to the model, which is
          // the reference an .mtl one level down would use. Suggested,
          // editable, and never applied without the artist saying so.
          path: c.hasPath ? relativeTo(dirName(m.modelPath), c.path) : baseName(c.path),
        })),
      });
      continue;
    }
    unrelated.push(c.id);
  }

  return { assignments, undecided, unrelated };
}

/**
 * The path to pre-fill when the artist attaches a companion BY HAND.
 *
 * `path: file.name` was the old default and it is wrong whenever the
 * model declares a subdirectory: the server matches the stored path
 * against the declared path by exact string, so a file attached as
 * `img.jpg` never satisfies a declared `textures/img.jpg`, and the
 * artist was left to notice that and retype it. Preferring a declared
 * path the file can only be — by its position in a picked directory, or
 * by being the one file with that name — removes the retyping without
 * ever inventing a path the model did not name.
 */
export function suggestCompanionPath(
  declared: string[],
  modelPath: string,
  entry: { path: string; hasPath: boolean },
): string {
  const decl = declared.map(normalizeRelPath).filter(Boolean);
  const rel = entry.hasPath ? relativeTo(dirName(modelPath), entry.path) : '';
  if (rel && decl.includes(rel)) return rel;
  const base = baseName(entry.path);
  const byBase = decl.filter((d) => baseName(d) === base);
  if (byBase.length === 1) return byBase[0];
  if (rel) return rel;
  return base;
}
