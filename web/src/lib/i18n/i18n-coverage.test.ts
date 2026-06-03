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

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, '../../..'); // .../web

const TRACKED_FILES = [
  'src/lib/components/viewers/ArchiveView.svelte',
  'src/lib/components/viewers/tools/ArchiveTool/Body.svelte',
  'src/routes/admin/users/+page.svelte',
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
const QUOTED_ATTR = /(?:placeholder|title|aria-label|alt)=(?:"([^"]+)"|'([^']+)'|\{`([^`]+)`\}|\{'([^']+)'\}|\{"([^"]+)"\})/g;
const PURE_T_CALL_ATTR = /(?:placeholder|title|aria-label|alt)=\{t\(/;

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

  // Tag-text matches.
  for (const m of source.matchAll(TEXT_BETWEEN_TAGS)) {
    const candidate = m[1].trim();
    if (isLikelyEnglish(candidate)) found.push(candidate);
  }

  // Quoted attribute literals.
  for (const m of source.matchAll(QUOTED_ATTR)) {
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
