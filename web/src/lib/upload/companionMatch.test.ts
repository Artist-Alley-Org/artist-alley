// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Unit cover for the #1408 matcher.
//
// These are the CARDINALITY cases. The interactive regression
// (scripts/dogfood/ui/tests/standalone/companion-reconciliation-1408.spec.ts)
// proves the real drop → persisted companion path end to end; this
// proves the decisions that end-to-end run can only exercise one at a
// time, and it proves them where a wrong answer is visible as a value
// rather than as a grey model.
//
// Every case here has N >= 2 models wherever the answer depends on
// there being more than one, because a one-model batch cannot express
// the failure this feature exists to prevent: attaching `diffuse.png`
// to the model that wanted `metal/diffuse.png` when it belonged to the
// one that wanted `wood/diffuse.png`.

import { describe, it, expect } from 'vitest';
import {
  reconcileCompanions,
  suggestCompanionPath,
  normalizeRelPath,
  relativeTo,
  joinRel,
  baseName,
  dirName,
} from './companionMatch';

const complete = (rowId: string, modelPath: string, declared: string[]) => ({
  rowId,
  modelPath,
  declared,
  complete: true,
});

describe('path helpers', () => {
  it('normalises what a browser hands over', () => {
    expect(normalizeRelPath('Textures\\wood.png')).toBe('Textures/wood.png');
    expect(normalizeRelPath('./a//b/../c.png')).toBe('a/c.png');
    expect(normalizeRelPath('/leading/x.png')).toBe('leading/x.png');
  });

  it('does NOT percent-decode', () => {
    // A declared URI arrives already decoded from the server's
    // cleanCompanionURI; a local filename is literal. Decoding here
    // would corrupt a file genuinely named with a percent sign.
    expect(normalizeRelPath('a%20b.png')).toBe('a%20b.png');
  });

  it('splits and rejoins around the model directory', () => {
    expect(dirName('wood/model.gltf')).toBe('wood');
    expect(dirName('model.gltf')).toBe('');
    expect(baseName('wood/textures/diffuse.png')).toBe('diffuse.png');
    expect(joinRel('wood', 'textures/diffuse.png')).toBe('wood/textures/diffuse.png');
    expect(relativeTo('wood', 'wood/textures/diffuse.png')).toBe('textures/diffuse.png');
  });
});

describe('reconcileCompanions — with real relative paths', () => {
  // ⭐ The structural claim this file's headline case rests on: TWO
  // models, and a basename that collides across them. Asserted, not
  // assumed — a fixture that quietly lost one model would make every
  // "no cross-wiring" assertion below vacuously true.
  const models = [
    complete('row-wood', 'wood/model.gltf', ['textures/diffuse.png']),
    complete('row-metal', 'metal/model.gltf', ['textures/diffuse.png']),
  ];

  it('the fixture really is N>=2 with a colliding basename', () => {
    expect(models.length).toBeGreaterThanOrEqual(2);
    const bases = models.flatMap((m) => m.declared.map(baseName));
    expect(new Set(bases).size).toBe(1);
    expect(bases.length).toBe(2);
    const expected = new Set(models.map((m) => joinRel(dirName(m.modelPath), m.declared[0])));
    expect(expected.size).toBe(2);
  });

  it('places each texture on its own model and never cross-wires', () => {
    const res = reconcileCompanions(models, [
      { id: 'c-wood', path: 'wood/textures/diffuse.png', hasPath: true },
      { id: 'c-metal', path: 'metal/textures/diffuse.png', hasPath: true },
    ]);

    expect(res.undecided).toEqual([]);
    expect(res.unrelated).toEqual([]);
    expect(res.assignments).toHaveLength(2);
    // Identity, both directions — a swap would still be "2 assignments".
    const byCand = new Map(res.assignments.map((a) => [a.candidateId, a]));
    expect(byCand.get('c-wood')?.rowId).toBe('row-wood');
    expect(byCand.get('c-metal')?.rowId).toBe('row-metal');
    // And the path sent is the DECLARED one, verbatim — the server
    // matches on exact string, so a reconstructed path could only miss.
    expect(byCand.get('c-wood')?.path).toBe('textures/diffuse.png');
    expect(byCand.get('c-wood')?.via).toBe('path');
  });

  it('a wrong-name file is unrelated and satisfies nothing', () => {
    const res = reconcileCompanions(models, [
      { id: 'c-wood', path: 'wood/textures/diffuse.png', hasPath: true },
      { id: 'c-stray', path: 'wood/textures/not-declared.png', hasPath: true },
    ]);
    expect(res.assignments.map((a) => a.candidateId)).toEqual(['c-wood']);
    expect(res.unrelated).toEqual(['c-stray']);
    // The metal model got nothing, so its requirement stays missing.
    expect(res.assignments.some((a) => a.rowId === 'row-metal')).toBe(false);
  });

  it('one file two models both declare is attached to BOTH', () => {
    // Not ambiguity. Ambiguity is not knowing which file was meant;
    // this is one file legitimately wanted twice, and each asset needs
    // its own companion row.
    const shared = [
      complete('row-a', 'shared/a.gltf', ['wood.png']),
      complete('row-b', 'shared/b.gltf', ['wood.png']),
    ];
    const res = reconcileCompanions(shared, [
      { id: 'c', path: 'shared/wood.png', hasPath: true },
    ]);
    expect(res.undecided).toEqual([]);
    expect(res.assignments.map((a) => a.rowId).sort()).toEqual(['row-a', 'row-b']);
  });
});

describe('reconcileCompanions — flat drop, no path information', () => {
  it('matches on basename when it is unambiguous in BOTH directions', () => {
    const models = [complete('row', 'model.gltf', ['textures/diffuse.png'])];
    const res = reconcileCompanions(models, [{ id: 'c', path: 'diffuse.png', hasPath: false }]);
    expect(res.assignments).toEqual([
      { candidateId: 'c', rowId: 'row', path: 'textures/diffuse.png', via: 'basename' },
    ]);
  });

  it('REFUSES a colliding basename instead of guessing', () => {
    // ⛔ The headline negative. Two models, two different declared
    // locations, one file called diffuse.png and nothing on it that
    // says which. A 50/50 guess renders one model with the other's
    // texture, which reads as an art mistake, not an upload one.
    const models = [
      complete('row-wood', 'model-wood.gltf', ['wood/diffuse.png']),
      complete('row-metal', 'model-metal.gltf', ['metal/diffuse.png']),
    ];
    const res = reconcileCompanions(models, [{ id: 'c', path: 'diffuse.png', hasPath: false }]);
    expect(res.assignments).toEqual([]);
    expect(res.unrelated).toEqual([]);
    expect(res.undecided).toHaveLength(1);
    expect(res.undecided[0].reason).toBe('ambiguous');
    expect(res.undecided[0].options.map((o) => o.rowId).sort()).toEqual(['row-metal', 'row-wood']);
    expect(res.undecided[0].options.map((o) => o.path).sort()).toEqual([
      'metal/diffuse.png',
      'wood/diffuse.png',
    ]);
  });

  it('REFUSES when two files offer the same name', () => {
    const models = [complete('row', 'model.gltf', ['textures/diffuse.png'])];
    const res = reconcileCompanions(models, [
      { id: 'c1', path: 'a/diffuse.png', hasPath: false },
      { id: 'c2', path: 'b/diffuse.png', hasPath: false },
    ]);
    // hasPath false means those directory names are not trustworthy;
    // both are just `diffuse.png` to the matcher.
    expect(res.assignments).toEqual([]);
    expect(res.undecided.map((u) => u.candidateId).sort()).toEqual(['c1', 'c2']);
  });
});

describe('reconcileCompanions — incomplete declarations', () => {
  it('will not call a leftover unrelated when an .obj is in the batch', () => {
    // An .obj declares .mtl libraries; each .mtl declares its own
    // textures one level below what the uploaded bytes can see. In that
    // batch "this file is unrelated" is a claim with no basis.
    const models = [
      { rowId: 'row-obj', modelPath: 'chair/chair.obj', declared: ['chair.mtl'], complete: false },
    ];
    const res = reconcileCompanions(models, [
      { id: 'c-mtl', path: 'chair/chair.mtl', hasPath: true },
      { id: 'c-tex', path: 'chair/maps/wood.png', hasPath: true },
    ]);
    expect(res.assignments).toEqual([
      { candidateId: 'c-mtl', rowId: 'row-obj', path: 'chair.mtl', via: 'path' },
    ]);
    expect(res.unrelated).toEqual([]);
    expect(res.undecided).toHaveLength(1);
    expect(res.undecided[0].candidateId).toBe('c-tex');
    expect(res.undecided[0].reason).toBe('incomplete');
    // The suggestion is where the file SITS relative to the model,
    // which is the reference an .mtl beside it would use.
    expect(res.undecided[0].options[0].path).toBe('maps/wood.png');
  });

  it('a batch of ordinary files with no model is entirely unrelated', () => {
    const res = reconcileCompanions([], [{ id: 'c', path: 'photo.png', hasPath: false }]);
    expect(res.unrelated).toEqual(['c']);
    expect(res.assignments).toEqual([]);
    expect(res.undecided).toEqual([]);
  });
});

describe('suggestCompanionPath', () => {
  const declared = ['textures/diffuse.png', 'textures/normal.png'];

  it('prefers the declared path over the bare filename', () => {
    // `path: file.name` was the old default, and a companion attached
    // as `diffuse.png` never satisfied a declared `textures/diffuse.png`
    // because the server matches by exact string.
    expect(suggestCompanionPath(declared, 'model.gltf', { path: 'diffuse.png', hasPath: false }))
      .toBe('textures/diffuse.png');
  });

  it('uses a real relative path when the browser gave one', () => {
    expect(
      suggestCompanionPath(declared, 'wood/model.gltf', {
        path: 'wood/textures/normal.png',
        hasPath: true,
      }),
    ).toBe('textures/normal.png');
  });

  it('falls back to the filename rather than inventing a path', () => {
    expect(suggestCompanionPath(declared, 'model.gltf', { path: 'other.png', hasPath: false }))
      .toBe('other.png');
  });

  it('does not pick between two declared paths sharing a basename', () => {
    const ambiguous = ['wood/diffuse.png', 'metal/diffuse.png'];
    expect(suggestCompanionPath(ambiguous, 'model.gltf', { path: 'diffuse.png', hasPath: false }))
      .toBe('diffuse.png');
  });
});
