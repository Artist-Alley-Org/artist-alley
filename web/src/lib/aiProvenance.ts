// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE AI DECLARATION, AS THE CLIENT READS IT (#1243, ADR 0094).
//
// #1167 built the WRITE half — a three-position control on upload and on
// the asset edit form — and the type lived beside it in the upload
// store. Nothing ever read the value back: every consumer in the tree
// was on the upload path. This module is the read half, and it holds the
// type as well, because a display rule that lives in a different file
// from the values it discriminates is a rule two surfaces can disagree
// about.
//
// ⛔ THE ONE PROHIBITION, AND IT IS THE WHOLE REASON THIS IS A FUNCTION
// AND NOT A LOOKUP TABLE:
//
//   `null` and `'none'` are DIFFERENT FACTS and NEITHER IS EVER SHOWN.
//
//   - `null` means UNDECLARED — nobody was asked. Every asset that
//     predates #1167 is null and was deliberately not backfilled.
//   - `'none'` means the maker declared no generative AI.
//
// Both render nothing, which makes the distinction look academic from a
// pixel's point of view — and it is not, because the tempting shortcut
// ("show a NoAI badge when the value is `none`") drags the other one
// with it the moment somebody writes `!isAi(v)`. The schema is explicit:
// a client that renders a missing value as "no AI" is LYING FOR THE
// ARTIST. ADR 0094's 2026-08-25 amendment extends that prohibition to
// the marker's accessible name and tooltip, so there is no string
// anywhere in this module for either state.
//
// ⭐ `assisted` and `generated` are marked DISTINGUISHABLY. They are the
// distinction the enum exists to hold ("I upscaled a texture" vs "a
// model made this"), and one shared marker undoes it.

/**
 * The maker's AI declaration (#1167, ADR 0094). `null` is the fourth
 * state and the default: UNDECLARED. It is a different statement from
 * `'none'` and the difference is load-bearing.
 */
export type AiProvenance = 'none' | 'assisted' | 'generated' | null;

/** The two states that are ever shown. Narrower than `AiProvenance` on
 *  purpose — anything holding one of these has already passed the gate,
 *  so a surface cannot reach a label for `none` or `null` by accident. */
export type MarkedAi = 'assisted' | 'generated';

/**
 * Is this declaration one the UI marks?
 *
 * The parameter is deliberately `unknown`-ish (`string | null |
 * undefined`) rather than `AiProvenance`: the value arrives from
 * hand-written API mirrors all over the client, and a narrow parameter
 * type would push a cast to every call site — which is where a `'none'`
 * would eventually slip through as `MarkedAi`.
 */
export function isMarkedAi(v: string | null | undefined): v is MarkedAi {
  return v === 'assisted' || v === 'generated';
}

/**
 * The i18n key naming this declaration, or null when nothing is shown.
 *
 * Reuses the SAME keys the upload control writes with
 * (`ai_provenance.assisted` / `.generated`), so the word an artist chose
 * when declaring is the word every reader sees. Two catalogues of
 * synonyms for one enum is how a label and a form drift apart.
 */
export function aiLabelKey(v: string | null | undefined): string | null {
  return isMarkedAi(v) ? `ai_provenance.${v}` : null;
}
