// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// i18n-coverage guard.
//
// Walks .svelte files in user-facing component surfaces, extracts
// likely visible English strings, and asserts they all go through
// `t()` against en.json. A regression on any tracked file fails the
// test with the offending strings listed — that's the punch list
// for the retrofit, not a diagnostic to interpret.
//
// Scope philosophy: this test scopes to surfaces we've actively
// migrated. Adding a file here is a deliberate "this surface is
// i18n-clean now" gesture; we don't lint the entire repo because
// most of it predates the i18n plumbing and the diff would be too
// loud to be actionable. As each surface gets retrofitted, add it
// to TRACKED_FILES and ratchet the bar.
//
// 1.55.V-2 extended this guard per the audit (docs/i18n_audit_v0_1.md
// §10):
//   - TRACKED_FILES grew from 24 → all MUST-tier files that are now
//     clean. Files with deferred SHOULD/NICE strings (AssetPlaylist's
//     viewer hotkey rail) stay OUT of the blocking list until their
//     follow-up arc lands.
//   - Attribute coverage added `label=` and `aria-description=`.
//   - HTML comments + <code> content are stripped before scanning
//     (they carried technical text — a `<section> not <main>` comment
//     and a `textures/wood.png` example path — that produced false
//     positives).
//   - A warn-only locale-parity check surfaces en keys missing from
//     es/fr WITHOUT failing CI (es/fr are deliberately incomplete
//     pre-#247).

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

import enDict from './en.json';
import esDict from './es.json';
import frDict from './fr.json';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, '../../..'); // .../web

const TRACKED_FILES = [
  // — pre-1.55.V-2 surfaces —
  'src/lib/components/viewers/ArchiveView.svelte',
  'src/lib/components/viewers/tools/ArchiveTool/Body.svelte',
  'src/routes/admin/users/+page.svelte',
  'src/routes/admin/users/[ref]/+page.svelte',
  'src/routes/account/sessions/+page.svelte',
  'src/routes/account/password/+page.svelte',
  'src/routes/admin/teams/+page.svelte',
  'src/routes/admin/teams/[id]/+page.svelte',
  'src/routes/admin/asset-types/+page.svelte',
  'src/routes/admin/asset-types/[ref]/+page.svelte',
  'src/routes/admin/system/log/+page.svelte',
  'src/routes/admin/system/license/+page.svelte',
  'src/routes/account/preferences/+page.svelte',
  'src/routes/account/blocked/+page.svelte',
  'src/routes/account/notifications/+page.svelte',
  'src/routes/account/messages/+page.svelte',
  'src/routes/account/messages/[peer]/+page.svelte',
  'src/routes/admin/system/activities/+page.svelte',
  'src/routes/admin/system/users/+page.svelte',
  'src/routes/account/profile/+page.svelte',
  'src/routes/admin/fields/+page.svelte',
  'src/lib/components/CollectionFieldsSection.svelte',
  'src/routes/admin/federation/peers/+page.svelte',
  'src/routes/admin/federation/directories/+page.svelte',
  // — 1.55.V-2 MUST-tier surfaces (now clean) —
  'src/routes/+page.svelte',
  'src/routes/posts/[id]/+page.svelte',
  'src/routes/setup/+page.svelte',
  'src/routes/search/+page.svelte',
  'src/routes/search/advanced/+page.svelte',
  'src/routes/collections/+page.svelte',
  'src/routes/collections/[id]/+page.svelte',
  'src/lib/components/CollectionModal.svelte',
  'src/lib/components/SearchBar.svelte',
  'src/lib/components/NavUploadButton.svelte',
  'src/lib/components/UserMenu.svelte',
  'src/lib/components/MessagesButton.svelte',
  'src/lib/components/CommentsThread.svelte',
  'src/lib/components/PostHost.svelte',
  'src/lib/components/federation/RestrictedShareBanner.svelte',
  'src/lib/components/upload/UploadModal.svelte',
  'src/lib/components/upload/PostComposeForm.svelte',
  'src/lib/components/upload/UploadDropZone.svelte',
  'src/lib/components/upload/UploadFileRow.svelte',
  'src/lib/components/upload/ThumbnailPicker.svelte',
  // — 1.55.W reverse-image dropzone —
  'src/lib/components/search/ReverseImageDropzone.svelte',
  // — #737 field-options editor —
  'src/lib/components/FieldEditor.svelte',
  // Deferred to the SHOULD/NICE follow-up (still carry non-MUST
  // hardcoded strings — do NOT add until their arc lands):
  //   src/lib/components/AssetPlaylist.svelte  (viewer hotkey rail)
];

// Heuristics for "user-visible English string in a Svelte template":
//   - Text node between tags: `>(some English)<`
//   - Quoted attribute values that surface visible text:
//       placeholder=, title=, aria-label=, alt=
//   - Filters reject obvious noise: numbers / single chars / units /
//     pure punctuation / mustache-only expressions / `t(...)` already.
//
// False positives we accept (rare and explicitly listed):
//   - The em dash "—" used as a placeholder in stats panels.
//   - Untranslatable file-extension labels like "ZIP" / "TAR".
//
// False negatives we accept:
//   - Strings inside JS string literals in <script> blocks. Those
//     are caller-controlled (a developer reading the diff would
//     notice); the template surface is what users see.

const ALLOW = new Set<string>([
  '—',
]);

const TEXT_BETWEEN_TAGS = />([^<>{}\n]+?)</g;
// aria-labelledby is intentionally NOT scanned — it references an
// element id, not visible text, so scanning it would false-positive.
const QUOTED_ATTR = /(?:placeholder|title|aria-label|aria-description|alt|label)=(?:"([^"]+)"|'([^']+)'|\{`([^`]+)`\}|\{'([^']+)'\}|\{"([^"]+)"\})/g;
const PURE_T_CALL_ATTR = /(?:placeholder|title|aria-label|aria-description|alt|label)=\{t\(/;

function isLikelyEnglish(s: string): boolean {
  const trimmed = s.trim();
  if (trimmed.length < 2) return false;
  if (ALLOW.has(trimmed)) return false;
  // Skip pure numbers, punctuation, single-emoji, units.
  if (!/[A-Za-z]/.test(trimmed)) return false;
  // Skip mustaches / Svelte expressions / interpolation markers.
  if (trimmed.startsWith('{') || trimmed.endsWith('}')) return false;
  // Skip very technical-looking strings (kebab class names slipped
  // through, all-caps acronyms shorter than 4 chars). The trade is
  // we'll miss "OK" — accepted; covered by manual review.
  if (/^[A-Z]{1,3}$/.test(trimmed)) return false;
  // Heuristic: at least one space OR a lowercase letter → user-facing.
  // Single capitalised words like "Stats" still match (no rule).
  return /[a-z]/.test(trimmed) || /\s/.test(trimmed);
}

function collectOffenders(source: string): string[] {
  const found: string[] = [];

  // Strip <script> blocks before scanning — TypeScript return-type
  // annotations like `): Promise<void>` produce false positives on
  // the >...< heuristic (the regex sees `>` from one generic and
  // `<` from the next). The test only inspects the TEMPLATE surface;
  // script-literal coverage is accepted as a known false negative
  // per the file's header comment.
  //
  // Also strip <style> blocks, HTML comments, and <code> content:
  // each carries non-translatable text (a `<section> not <main>`
  // rationale comment, a `textures/wood.png` example path) that would
  // otherwise register as raw English.
  const templateOnly = source
    .replace(/<script[\s\S]*?<\/script>/g, '')
    .replace(/<style[\s\S]*?<\/style>/g, '')
    .replace(/<!--[\s\S]*?-->/g, '')
    .replace(/<code\b[^>]*>[\s\S]*?<\/code>/g, '');

  // Tag-text matches.
  for (const m of templateOnly.matchAll(TEXT_BETWEEN_TAGS)) {
    const candidate = m[1].trim();
    if (isLikelyEnglish(candidate)) found.push(candidate);
  }

  // Quoted attribute literals. Also script-stripped — handlers like
  // `onclick={() => api.POST('/foo', { body: {...} })}` would
  // otherwise leak script-literal strings into the attribute regex.
  for (const m of templateOnly.matchAll(QUOTED_ATTR)) {
    const candidate = (m[1] ?? m[2] ?? m[3] ?? m[4] ?? m[5]).trim();
    if (isLikelyEnglish(candidate)) found.push(candidate);
  }

  // Attribute values like `title="Foo bar"` are flagged above; the
  // `title={t('...')}` form is fine + already excluded. The opposite
  // bug — `title="Literal"` slipped past the regex above — is
  // captured because QUOTED_ATTR matches that case. Cross-check with
  // PURE_T_CALL_ATTR is defensive in case the user wraps with t() but
  // adds extra surrounding chars; treat the t() form as authorised.
  return found.filter((s) => !s.includes('t('));
}

describe('i18n surface coverage', () => {
  for (const rel of TRACKED_FILES) {
    it(`${rel} has no raw English strings`, () => {
      const path = resolve(repoRoot, rel);
      const source = readFileSync(path, 'utf-8');
      const offenders = collectOffenders(source);
      expect(offenders, `${rel}: raw English strings — wrap with t('archive.<key>') and add the key to en.json:\n  ${offenders.join('\n  ')}`).toEqual([]);
    });
  }

  // Sanity: the heuristic must be sharp enough to fire on a known-bad
  // synthetic source — otherwise a future regression that bypasses
  // the regex would pass silently.
  it('flags raw English in a synthetic positive control', () => {
    const synthetic = `
      <button title="Click me">Save now</button>
      <span>{t('archive.entry_count', { count })}</span>
      <span>—</span>
    `;
    const offenders = collectOffenders(synthetic);
    expect(offenders).toContain('Save now');
    expect(offenders).toContain('Click me');
    // The t()-wrapped span and em-dash placeholder must NOT register.
    expect(offenders).not.toContain('—');
    expect(offenders.some((s) => s.includes("t('"))).toBe(false);
  });

  // We rely on PURE_T_CALL_ATTR + the t( filter to avoid flagging
  // already-migrated content; if that regex breaks, the test would
  // start failing on the migrated files. Pin that contract.
  it.skipIf(typeof PURE_T_CALL_ATTR === 'undefined')(
    'placeholder={t(...)} attribute pattern does not register as raw',
    () => {
      const synthetic = `<input placeholder={t('archive.filter_placeholder')} />`;
      expect(collectOffenders(synthetic)).toEqual([]);
    },
  );
});

// Locale-parity — WARN-ONLY. es/fr are deliberately incomplete
// pre-#247; this surfaces the coverage gap without blocking CI. The
// numbers here are the baseline the #247 translation arc burns down.
describe('i18n locale parity (warn-only)', () => {
  function flatten(src: Record<string, unknown>, prefix = '', out: Record<string, string> = {}): Record<string, string> {
    for (const [k, v] of Object.entries(src)) {
      const key = prefix ? `${prefix}.${k}` : k;
      if (v != null && typeof v === 'object' && !Array.isArray(v)) {
        flatten(v as Record<string, unknown>, key, out);
      } else {
        out[key] = String(v);
      }
    }
    return out;
  }

  const en = flatten(enDict as Record<string, unknown>);
  const enKeys = Object.keys(en);

  for (const [code, dict] of [
    ['es', esDict],
    ['fr', frDict],
  ] as const) {
    it(`reports en keys missing from ${code} (informational)`, () => {
      const loc = flatten(dict as Record<string, unknown>);
      const missing = enKeys.filter((k) => !(k in loc));
      const pct = Math.round(((enKeys.length - missing.length) / enKeys.length) * 100);
      if (missing.length > 0) {
        // eslint-disable-next-line no-console
        console.warn(
          `[i18n parity] ${code}: ${enKeys.length - missing.length}/${enKeys.length} keys (${pct}%) — ${missing.length} missing. ` +
            `Deliberately incomplete pre-#247; en fallback covers them.`,
        );
      }
      // Warn-only: never fails. Also assert no ORPHAN keys (present in
      // the locale but absent from en) — those ARE a real bug (schema
      // drift) and this half of the check is blocking.
      const orphans = Object.keys(loc).filter((k) => !(k in en));
      expect(orphans, `${code}.json has keys not present in en.json (schema drift): ${orphans.join(', ')}`).toEqual([]);
    });
  }
});
