// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * `display_condition` in the browser: when a field should be OFFERED at
 * all (#1173, #1119, ADR 0099).
 *
 * # ⛔ A CONDITION IS A FORM HINT AND NEVER AUTHORIZATION
 *
 * Everything in this module decides whether a CONTROL is drawn. Nothing
 * here decides whether a value may be read or written. A hidden field
 * keeps its stored value, keeps its capabilities, and is still writable
 * through the field-value endpoints. If a rule here ever looks like it is
 * protecting something, that is the bug.
 *
 * # Why this file exists at all, and why it is not a shortcut
 *
 * The server's authority is `facet.SplitFieldTerm` (Go), the SAME
 * function the search `filter=field:...` predicate uses, so that a
 * condition and a filter cannot disagree about what a term means. THE
 * BROWSER CANNOT CALL IT. So this is a second implementation of one
 * grammar, which is the shape that drifts.
 *
 * The mitigation is the one `fieldEmptiness.cases.json` established:
 * `displayCondition.cases.json` is read by BOTH suites, so a change that
 * moves one plane fails on the other plane's test. Do not add a case to
 * this file's tests without adding it there.
 *
 * # The five properties of the grammar, none of them guessable
 *
 *  1. Split on the FIRST character from `=~<>`.
 *  2. Match operators LONGEST FIRST, so `>=` is never `>` followed by a
 *     value beginning `=`.
 *  3. Later operator characters stay IN the value: `expr=a=b` has the
 *     value `a=b`.
 *  4. The CODE is lowercased and trimmed.
 *  5. The parsed VALUE is trimmed.
 *
 * And the sixth, which lives in the comparison rather than the parse:
 * NOTHING EVER TRIMS OR CASE-FOLDS THE STORED VALUE. The asymmetry with
 * (5) is deliberate and is what an operator will actually hit: a stored
 * `" Commission "` does NOT match `work_type= Commission `, because the
 * literal parses to `Commission`. Trimming the stored value here would
 * make the form disagree with the server and with search.
 */

/** The operators the shared grammar knows. */
export type ConditionOp = '=' | '~' | '>=' | '<=';

/** One parsed `<code><op><value>` term. */
export interface ConditionTerm {
  /** The entry exactly as stored, so a message can quote what was written. */
  raw: string;
  /** Lowercased and trimmed. */
  code: string;
  op: ConditionOp;
  /** Trimmed. The STORED value it is compared against is not. */
  value: string;
}

/**
 * Operator start characters, and the match order.
 *
 * LONGEST FIRST is load-bearing (property 2). Reordering this array so
 * `=` comes before `>=` makes `year>=1920` parse as an unknown `>`
 * operator, which fails, which fails the condition open, which silently
 * shows a field that should have been hidden.
 */
const OP_CHARS = '=~<>';
const OPS: ConditionOp[] = ['>=', '<=', '~', '='];

/**
 * The character class a field code may use, mirroring the server's
 * `validFieldCode`. Lowercase only, because the parser folds the code
 * before this runs, so an uppercase code is normalised rather than
 * refused; a space or a colon is what fails.
 */
const CODE_RE = /^[a-z0-9_-]+$/;

/**
 * Split one stored entry into its code, operator and value.
 *
 * Returns null for anything malformed rather than guessing: a bare code
 * with no operator, an empty parsed value, and a code carrying an illegal
 * character all fail here. A caller that gets null has an UNEVALUABLE
 * term, which fails the whole condition OPEN.
 */
export function parseConditionTerm(raw: string): ConditionTerm | null {
  let i = -1;
  for (let n = 0; n < raw.length; n++) {
    if (OP_CHARS.includes(raw[n])) {
      i = n;
      break;
    }
  }
  if (i < 0) return null;
  const code = raw.slice(0, i).trim().toLowerCase();
  const rest = raw.slice(i);
  let op: ConditionOp | null = null;
  let value = '';
  for (const candidate of OPS) {
    if (rest.startsWith(candidate)) {
      op = candidate;
      value = rest.slice(candidate.length).trim();
      break;
    }
  }
  if (op === null || value === '' || !CODE_RE.test(code)) return null;
  return { raw, code, op, value };
}

/**
 * Which operators a controller of each type accepts (ADR 0099 §3).
 *
 * A CLOSED table, and the client's copy of the server's
 * `displayConditionOps`. It is here rather than derived from the API
 * because it decides UNEVALUABILITY, and a form that had to ask the
 * server which pairings are legal would need a round trip to draw itself.
 *
 * ⛔ `boolean` IS ABSENT ON PURPOSE. Sprint 20a gave it a three-state
 * CONTROL; that changed the control and not the REPRESENTATION (still
 * 1/0 in `value_num`), and admitting it would require a search-engine
 * change. `rich_text` is absent because what is stored is sanitised HTML,
 * so a condition would match markup. `>=` and `<=` appear nowhere: they
 * are range bounds, which is a filtering question.
 */
const OPS_BY_TYPE: Record<string, ConditionOp[]> = {
  text: ['=', '~'],
  longtext: ['=', '~'],
  select: ['='],
  tree: ['='],
  multi_select: ['='],
};

export function conditionOpAllowed(fieldType: string, op: ConditionOp): boolean {
  return (OPS_BY_TYPE[fieldType] ?? []).includes(op);
}

/**
 * One controller field as the evaluator sees it.
 *
 * ⚠️ THE THREE STATES ARE DISTINCT AND CONFLATING ANY TWO IS A BUG:
 *
 *   absent from the resolver     -> UNEVALUABLE, fails open (SHOWN)
 *   present with readable false  -> UNEVALUABLE, fails open (SHOWN)
 *   present, readable, no value  -> REAL FALSE (HIDDEN)
 *
 * `readable` is a SERVER-DERIVED fact for this caller and this subject,
 * read from `GET /{subject}/{id}/field-composition`. It is never inferred
 * from whether a value arrived, and never from the caller's capability
 * list: that list carries GLOBALLY held codes only, so a grant scoped to
 * the team owning the asset is invisible in the browser and the inference
 * would answer "no" for exactly the operator the field was configured
 * for.
 */
export interface ConditionController {
  type: string;
  readable: boolean;
  /** The stored `value_text`, verbatim and untrimmed. */
  text?: string | null;
  /** The stored `value_options`, for a `multi_select`. */
  options?: string[] | null;
}

/** Answers "what is the state of the controller with this code". */
export type ConditionResolver = (code: string) => ConditionController | undefined;

/**
 * Does ONE term match a readable controller?
 *
 * Reached only for a controller that resolved, is readable, and whose
 * type accepts the operator. The unevaluable cases are decided by
 * `conditionShows` before any of this runs.
 *
 * ⛔ NOTHING HERE TRIMS OR CASE-FOLDS THE STORED VALUE. `=` is exact and
 * case-sensitive. `~` lowercases both sides for the comparison only.
 */
export function conditionTermMatches(t: ConditionTerm, c: ConditionController): boolean {
  if (t.op === '=') {
    if (c.type === 'multi_select') {
      // MEMBERSHIP. Equality against the whole set would make a
      // multi-valued field usable as a controller only while it held
      // exactly one value.
      return (c.options ?? []).some((o) => o === t.value);
    }
    return (c.text ?? '') === t.value;
  }
  if (t.op === '~') {
    return (c.text ?? '').toLowerCase().includes(t.value.toLowerCase());
  }
  // An operator outside the matrix is unreachable through configuration.
  // Returning false here would HIDE a field because of a rule nobody can
  // see, so the caller treats this pairing as unevaluable instead and
  // never gets here.
  return false;
}

/**
 * Should the dependent field be SHOWN?
 *
 * `true` is SHOWN, which is the direction that makes the fail-open
 * default readable: every early return below returns `true`.
 *
 *   N === 0                    -> shown, no term examined
 *   any term unparseable       -> shown  (unevaluable)
 *   any controller missing     -> shown  (unevaluable)
 *   any controller unreadable  -> shown  (unevaluable)
 *   any operator/type mismatch -> shown  (unevaluable)
 *   otherwise                  -> AND of the terms
 *
 * ⛔ THE TWO PASSES ARE SEPARATE AND MUST STAY SEPARATE. The first asks
 * only "can every term be evaluated"; the second computes the
 * conjunction. Folding them into one loop that ANDs as it goes is the
 * "unknown counts as true" bug: with one term FALSE and another unknown,
 * `false && true` still HIDES the field, and cardinality cases 4 and 5
 * are exactly that shape.
 *
 * The mirror trap is the other half: a READABLE controller holding
 * nothing is a REAL FALSE and still hides. An evaluator that treats every
 * absent value as unknown never hides anything and passes every test that
 * only checks the true arm.
 */
export function conditionShows(
  condition: string[] | null | undefined,
  resolve: ConditionResolver,
): boolean {
  if (!condition || condition.length === 0) return true;

  const terms: ConditionTerm[] = [];
  for (const raw of condition) {
    const t = parseConditionTerm(raw);
    if (t === null) return true;
    terms.push(t);
  }

  // PASS 1 — evaluability. Nothing is decided here.
  const states: ConditionController[] = [];
  for (const t of terms) {
    const c = resolve(t.code);
    if (!c || !c.readable) return true;
    if (!conditionOpAllowed(c.type, t.op)) return true;
    states.push(c);
  }

  // PASS 2 — the conjunction. Every term is known evaluable, so a false
  // here is a REAL false, including the one a readable-but-empty
  // controller produces.
  for (let i = 0; i < terms.length; i++) {
    if (!conditionTermMatches(terms[i], states[i])) return false;
  }
  return true;
}

/**
 * The codes a condition names, for a surface that wants to know which
 * controllers it depends on.
 *
 * Unparseable entries contribute nothing: they name no field. Deduped and
 * order-preserving.
 */
export function conditionControllerCodes(condition: string[] | null | undefined): string[] {
  const out: string[] = [];
  for (const raw of condition ?? []) {
    const t = parseConditionTerm(raw);
    if (t && !out.includes(t.code)) out.push(t.code);
  }
  return out;
}
