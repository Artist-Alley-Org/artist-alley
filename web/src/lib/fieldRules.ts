// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * The client-side reading of the field-configuration WRITE rules
 * (#1173, #1389).
 *
 * These decide what a control looks like and which request an editor
 * sends. They do not decide whether a write is allowed: `read_only`,
 * `regexp_filter` and `required` are all enforced by the four
 * field-value handlers, which answer 422 with a machine reason, and
 * that refusal has to stay reachable and visible. A flag a client can
 * evaluate is a flag a client can skip.
 *
 * One module rather than a copy per component, for the reason
 * `fieldOptions.ts` exists: a second implementation of a rule is a
 * second rule.
 */

/** The subset of a field definition these rules read. */
export interface FieldRuleDef {
  type: string;
  read_only?: boolean;
  regexp_filter?: string | null;
}

/**
 * Does this field carry a pattern that applies to its type?
 *
 * The server's `regexpFilterApplies` narrows it to exactly these two,
 * because both store the operator's own words verbatim in `value_text`.
 * `rich_text` shares the column and is NOT one of them: what is stored
 * is sanitised markup, so a pattern would be matched against tags.
 */
export function fieldPatternApplies(def: FieldRuleDef): boolean {
  return !!def.regexp_filter && (def.type === 'text' || def.type === 'longtext');
}

/**
 * Compile a configured pattern the way the server matches it, or null
 * when it cannot be compiled here.
 *
 * WHOLE-VALUE semantics, non-capturing group and all: the server
 * assembles `\A(?:pattern)\z` so that `a|b` means "the whole value is a
 * or the whole value is b" rather than "starts with a, or ends with b".
 * `^…$` is the closest JavaScript equivalent; it is not identical
 * (JavaScript's `$` follows a trailing newline, `\z` does not), which is
 * one more reason this is advisory and the server is authoritative.
 *
 * Returning null on a compile failure is deliberate. Go's RE2 accepts
 * syntax JavaScript rejects and vice versa, and showing a person a
 * regexp error for a pattern their operator wrote and the server
 * accepts would be reporting our own incompatibility as their mistake.
 * The check simply does not run, and the server's 422 is what they see.
 */
export function compileFieldPattern(def: FieldRuleDef): RegExp | null {
  const p = def.regexp_filter;
  if (!p || !fieldPatternApplies(def)) return null;
  try {
    return new RegExp('^(?:' + p + ')$');
  } catch {
    return null;
  }
}

/**
 * Does this text break the field's configured pattern, as far as the
 * browser can tell?
 *
 * An EMPTY value is never reported here. Removing a value is a Clear,
 * which carries nothing to match, and `required` — not the pattern — is
 * what decides whether emptying is allowed.
 */
export function fieldPatternViolated(def: FieldRuleDef, text: string): boolean {
  const re = compileFieldPattern(def);
  if (re === null || text === '') return false;
  return !re.test(text);
}
