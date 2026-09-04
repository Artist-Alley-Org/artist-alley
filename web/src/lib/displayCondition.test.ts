// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * The CLIENT half of the display-condition parity proof (#1173, #1119,
 * ADR 0099 §2).
 *
 * The authority is Go: `facet.SplitFieldTerm`, the same function the
 * search `filter=field:...` predicate uses. THE BROWSER CANNOT CALL IT,
 * so `displayCondition.ts` reimplements the grammar, and two
 * implementations of one rule drift.
 *
 * ⛔ THE CASES ARE NOT WRITTEN HERE. They are in
 * `displayCondition.cases.json`, which
 * `app/internal/metadata/display_condition_parity_test.go` reads as well.
 * Adding a case there adds it to both planes at once; adding one here
 * would create exactly the second copy the shared file exists to prevent.
 * That is 20a's `fieldEmptiness.cases.json` precedent applied to a second
 * rule.
 */

import { describe, it, expect } from 'vitest';
import cases from './displayCondition.cases.json';
import {
  parseConditionTerm,
  conditionOpAllowed,
  conditionTermMatches,
  conditionShows,
  conditionControllerCodes,
  type ConditionController,
  type ConditionOp,
} from './displayCondition';

type ParseCase = {
  why: string;
  input: string;
  ok: boolean;
  code?: string;
  op?: string;
  value?: string;
};
type MatrixCase = { type: string; op: string; allowed: boolean };
type RawController = { type: string; readable: boolean; text?: string; options?: string[] };
type EvaluateCase = { why: string; term: string; controller: RawController; match: boolean };
type ConditionCase = {
  why: string;
  condition: string[];
  controllers: Record<string, RawController>;
  shown: boolean;
};

// `as unknown as` rather than a direct assertion, and only here.
// TypeScript infers a UNION of per-entry literal shapes from an imported
// JSON file, so `controllers` widens to an object whose optional keys are
// `undefined` and no longer overlaps `Record<string, RawController>`.
// That is a property of how the corpus is written, not a signal about the
// runtime shape, which the anti-vacuity floors below and the Go suite
// reading the same file both check for real.
const parseCases = cases.parse as unknown as ParseCase[];
const matrixCases = cases.matrix as unknown as MatrixCase[];
const evaluateCases = cases.evaluate as unknown as EvaluateCase[];
const conditionCases = cases.condition as unknown as ConditionCase[];

function toController(c: RawController): ConditionController {
  return { type: c.type, readable: c.readable, text: c.text, options: c.options };
}

describe('displayCondition.cases.json is actually loaded', () => {
  // ANTI-VACUITY. A corpus that silently became empty — a rename, a bad
  // merge, a JSON key typo — would make every `it.each` below register
  // zero cases and report green while proving nothing. These floors are
  // the denominators the sprint reports, and they match the ones the Go
  // suite asserts on the same file.
  it('carries every section at the agreed floor', () => {
    expect(parseCases.length).toBeGreaterThanOrEqual(12);
    expect(matrixCases.length).toBeGreaterThanOrEqual(20);
    expect(evaluateCases.length).toBeGreaterThanOrEqual(10);
    expect(conditionCases.length).toBeGreaterThanOrEqual(12);
  });
});

describe('parseConditionTerm — the five grammar properties', () => {
  it.each(parseCases)('$input — $why', (c) => {
    const got = parseConditionTerm(c.input);
    if (!c.ok) {
      expect(got).toBeNull();
      return;
    }
    expect(got).not.toBeNull();
    expect(got!.code).toBe(c.code);
    expect(got!.op).toBe(c.op);
    expect(got!.value).toBe(c.value);
  });
});

describe('conditionOpAllowed — the closed operator/type table', () => {
  it.each(matrixCases)('$type $op -> $allowed', (c) => {
    expect(conditionOpAllowed(c.type, c.op as ConditionOp)).toBe(c.allowed);
  });
});

describe('conditionTermMatches — the comparison rules', () => {
  it.each(evaluateCases)('$term — $why', (c) => {
    const term = parseConditionTerm(c.term);
    expect(term, `corpus term ${c.term} must parse`).not.toBeNull();
    expect(conditionTermMatches(term!, toController(c.controller))).toBe(c.match);
  });
});

describe('conditionShows — conjunction with whole-condition fail-open', () => {
  it.each(conditionCases)('$why', (c) => {
    const resolve = (code: string) => {
      const ctrl = c.controllers[code];
      return ctrl ? toController(ctrl) : undefined;
    };
    expect(conditionShows(c.condition, resolve)).toBe(c.shown);
  });
});

describe('the fail-open rule is not "unknown counts as true"', () => {
  // Stated directly, without the corpus, so deleting a case from the JSON
  // cannot quietly remove the rule. The two implementations differ on
  // exactly one input shape and this is it: one term FALSE, one term
  // unevaluable. An AND that substituted `true` for the unknown would
  // answer HIDDEN.
  const resolveKnownOnly = (code: string): ConditionController | undefined =>
    code === 'known' ? { type: 'text', readable: true, text: 'no' } : undefined;

  it('a false term beside an unevaluable one SHOWS the field', () => {
    expect(conditionShows(['known=yes', 'missing=x'], resolveKnownOnly)).toBe(true);
  });

  it('and the evaluator really does hide when every term is evaluable', () => {
    // Without this the assertion above would pass against an evaluator
    // that shows everything.
    const all = (): ConditionController => ({ type: 'text', readable: true, text: 'no' });
    expect(conditionShows(['known=yes', 'other=x'], all)).toBe(false);
  });

  it('an UNREADABLE controller beside a false term SHOWS the field', () => {
    const resolve = (code: string): ConditionController | undefined =>
      code === 'known'
        ? { type: 'text', readable: true, text: 'no' }
        : { type: 'text', readable: false };
    expect(conditionShows(['known=yes', 'secret=x'], resolve)).toBe(true);
  });

  it('a READABLE controller holding nothing is a REAL false and HIDES', () => {
    // The mirror trap. An evaluator that treated every absent value as
    // unknown would never hide anything and would pass every true-arm
    // test in the suite.
    const resolve = (): ConditionController => ({ type: 'text', readable: true, text: '' });
    expect(conditionShows(['ctrl=yes'], resolve)).toBe(false);
  });
});

describe('the whitespace discriminator', () => {
  // The condition literal is TRIMMED by the parser and the STORED value
  // is not. This is the asymmetry an operator will actually hit.
  it('a stored " Commission " does not match a condition literal of Commission', () => {
    const resolve = (): ConditionController => ({
      type: 'text',
      readable: true,
      text: ' Commission ',
    });
    expect(conditionShows(['work_type= Commission '], resolve)).toBe(false);
  });

  it('and the same literal matches the untrimmed value it really equals', () => {
    const resolve = (): ConditionController => ({
      type: 'text',
      readable: true,
      text: 'Commission',
    });
    expect(conditionShows(['work_type= Commission '], resolve)).toBe(true);
  });
});

describe('N = 0', () => {
  const boom = () => {
    throw new Error('the resolver must not be called when there is no condition');
  };
  it.each([
    ['null', null],
    ['undefined', undefined],
    ['an empty array', [] as string[]],
  ])('%s means always shown, with no evaluator side effect', (_name, cond) => {
    expect(conditionShows(cond as string[] | null | undefined, boom)).toBe(true);
  });
});

describe('conditionControllerCodes', () => {
  it('lists the codes a condition depends on, deduped and in order', () => {
    expect(conditionControllerCodes(['b=1', 'a~x', 'b=2'])).toEqual(['b', 'a']);
  });
  it('ignores entries that name no field', () => {
    expect(conditionControllerCodes(['not a term', 'a=1'])).toEqual(['a']);
  });
  it('is empty for no condition', () => {
    expect(conditionControllerCodes(null)).toEqual([]);
  });
});
