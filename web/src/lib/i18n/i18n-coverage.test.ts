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

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, relative, resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

import enDict from './en.json';
import esDict from './es.json';
import frDict from './fr.json';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, '../../..'); // .../web

/** Nested catalogue → flat dotted-key map. Mirrors lang.svelte.ts. */
function flatten(
  src: Record<string, unknown>,
  prefix = '',
  out: Record<string, string> = {},
): Record<string, string> {
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
  // The site-text override page (#794). Tracked from the day it lands:
  // a page whose whole subject is operator-editable wording has no
  // business shipping hardcoded English of its own.
  'src/routes/admin/site-text/+page.svelte',
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
  // — #854 the per-field page. Tracked from the day it lands: it is
  //   where the app explains what a mirrored field is and why its
  //   column cannot be retargeted, and an operator running in another
  //   language has to be able to read that. —
  'src/routes/admin/fields/[code]/+page.svelte',
  'src/lib/components/CollectionFieldsSection.svelte',
  'src/routes/admin/federation/peers/+page.svelte',
  'src/routes/admin/federation/directories/+page.svelte',
  // — 1.55.V-2 MUST-tier surfaces (now clean) —
  'src/routes/+page.svelte',
  'src/routes/posts/[id]/+page.svelte',
  'src/routes/setup/+page.svelte',
  'src/routes/search/+page.svelte',
  'src/routes/collections/+page.svelte',
  'src/routes/collections/[id]/+page.svelte',
  'src/lib/components/Modal.svelte',
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
  // — #850 one result surface: the advanced builder became a panel and
  //   the facet rail became a slide-over —
  'src/lib/components/search/AdvancedQueryBuilder.svelte',
  'src/lib/components/search/SearchSlideOver.svelte',
  // — #737 field-options editor —
  'src/lib/components/FieldEditor.svelte',
  // — #774 surfaces whose keys were dead until the resolution guard
  //   below caught them. Tracked so both halves stay clean. —
  'src/routes/admin/ai/config/+page.svelte',
  'src/routes/admin/ai/usage/+page.svelte',
  'src/lib/components/SimilarAssetsPanel.svelte',
  'src/lib/components/AssetTagBadge.svelte',
  'src/lib/components/FieldValueInput.svelte',
  // — #881 the request-access loop. Tracked from the start: the
  //   dialog's copy is the only place the app tells a user what a
  //   granted request does and does not do, and an operator override
  //   has to be able to reach it. —
  // — #880 the share loop. The dialog is the only place the app states
  //   what a grant does, how long it lasts, and which principal kinds
  //   are inert, so its copy is exactly the kind an operator override
  //   has to be able to reach. —
  'src/lib/components/ShareEntityModal.svelte',
  'src/lib/components/CardRestricted.svelte',
  'src/lib/components/RequestAccessDialog.svelte',
  'src/lib/components/RequestQueue.svelte',
  'src/routes/account/requests/+page.svelte',
  'src/routes/admin/requests/+page.svelte',
  'src/routes/account/trash/+page.svelte',
  // — #981 the delete affordances. Tracked from the start for the same
  //   reason as the request-access dialog: the confirm dialog's copy is
  //   the only place the app states what a delete DOES (it is
  //   recoverable, it goes to your trash, the owner reads your reason),
  //   and an operator override has to be able to reach it. The toast is
  //   the acknowledgement of the same act. —
  'src/lib/components/ConfirmDeleteDialog.svelte',
  'src/lib/components/ToastHost.svelte',
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

// ---------------------------------------------------------------
// Key-resolution guard (#774).
//
// The surface-coverage block above asserts strings are *wrapped* in
// t(). It never asked whether the key t() is handed actually exists.
// That gap shipped 46 dead keys across four surfaces — /admin/ai/config
// and /admin/ai/usage rendered `admin.ai_inference.budget_hard_label`
// as literal text to operators, because t() falls back to returning
// the key itself when nothing resolves.
//
// This block closes it: every t() key in src/ must resolve against
// en.json. Unlike the block above it is repo-wide, not opt-in —
// there is no judgement call in "does this key exist".
//
// How `t` is identified — this is the part that decides whether the
// guard survives. A naive /t\(/ matcher hits the tail of `import(`,
// `await(`, `format(`, and every other identifier ending in `t`; the
// first draft of this sweep reported `hls.js` as a missing key,
// harvested from `await import('hls.js')` in MediaView.svelte. A
// guard that cries wolf gets deleted, and then it protects nothing.
// So we resolve `t` to its import binding: a file is only scanned if
// it imports `t` from $stores/lang.svelte, and only that binding's
// name (honouring `as` aliases) is matched, with a preceding
// non-identifier character so `import(` / `obj.t(` never qualify.
//
// Known, deliberate false negatives:
//   - t(someVariable) — the key isn't a literal, so it can't be
//     checked statically (AccountStubPage, BrowseFooter, …). Those
//     keys come from typed tables the surface-coverage block sees.
//   - Interpolated keys are checked structurally, not exactly: the
//     literal segments must match at least one real en key. That
//     catches `asset.tag_source.${s}` when the block actually lives
//     at `admin.asset.tag_source` — which is exactly how the fourth
//     defect in #774 was found — without needing to enumerate every
//     runtime value.

describe('i18n key resolution', () => {
  const EN_FLAT = flatten(enDict as Record<string, unknown>);
  const EN_KEYS = Object.keys(EN_FLAT);

  const SRC_ROOT = resolve(repoRoot, 'src');
  const SCANNED_EXT = /\.(svelte|svelte\.ts|ts)$/;
  // Test files are excluded: they talk *about* keys (deliberately bad
  // ones, in synthetic fixtures) rather than rendering them, and this
  // file in particular would harvest its own controls. The store that
  // defines t() is excluded for the same reason.
  const SKIP = /(\.test\.ts|\.spec\.ts|lang\.svelte\.ts)$/;

  function walk(dir: string, out: string[] = []): string[] {
    for (const entry of readdirSync(dir)) {
      const p = join(dir, entry);
      if (statSync(p).isDirectory()) walk(p, out);
      else if (SCANNED_EXT.test(entry)) out.push(p);
    }
    return out;
  }

  /**
   * The local name `t` is bound to in this file, or null if the file
   * doesn't import it. Handles `import { t }`, `import { lang, t }`
   * and `import { t as translate }`.
   */
  function localTName(source: string): string | null {
    const imp = source.match(/import\s*\{([^}]*)\}\s*from\s*['"]\$stores\/lang\.svelte['"]/);
    if (!imp) return null;
    for (const spec of imp[1].split(',')) {
      const parts = spec.trim().split(/\s+as\s+/);
      if (parts[0] === 't') return parts[1] ?? 't';
    }
    return null;
  }

  /** Literal first arguments passed to the resolved `t` binding. */
  function extractKeys(source: string, name: string): string[] {
    // Leading (^|[^\w$.]) is what keeps `import(`, `await(` and
    // `obj.t(` out. Captures '…', "…" and `…` first arguments.
    const re = new RegExp(`(?:^|[^\\w$.])${name}\\(\\s*(['"\`])((?:[^'"\`\\\\]|\\\\.)*?)\\1`, 'g');
    return [...source.matchAll(re)].map((m) => m[2]);
  }

  /**
   * `${…}` segments make a key a family, not a key. Match the literal
   * parts against real keys: interpolated values never contain a dot
   * (they're slugs / enum members), so `[^.]+` is the right wildcard.
   */
  function familyMatches(key: string): boolean {
    const pattern = key
      .split(/\$\{[^}]*\}/)
      .map((lit) => lit.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
      .join('[^.]+');
    const re = new RegExp(`^${pattern}$`);
    return EN_KEYS.some((k) => re.test(k));
  }

  const files = walk(SRC_ROOT).filter((f) => !SKIP.test(f));

  it('scans a meaningful number of files (the sweep is wired up)', () => {
    // Guards against a refactor that silently empties the walk — a
    // zero-file sweep would report zero missing keys and look green.
    const scanned = files.filter((f) => localTName(readFileSync(f, 'utf-8')) !== null);
    expect(scanned.length).toBeGreaterThan(100);
  });

  it('every t() key resolves against en.json', () => {
    const missing = new Map<string, Set<string>>();
    for (const file of files) {
      const source = readFileSync(file, 'utf-8');
      const name = localTName(source);
      if (!name) continue;
      for (const key of extractKeys(source, name)) {
        const ok = key.includes('${') ? familyMatches(key) : key in EN_FLAT;
        if (ok) continue;
        if (!missing.has(key)) missing.set(key, new Set());
        missing.get(key)!.add(relative(repoRoot, file));
      }
    }
    const report = [...missing]
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([k, files_]) => `  ${k}  <- ${[...files_].join(', ')}`)
      .join('\n');
    expect(
      [...missing.keys()],
      `t() keys with no entry in en.json — add the key, or fix the ` +
        `prefix if the string already lives elsewhere:\n${report}`,
    ).toEqual([]);
  });

  // Negative control. `await import('hls.js')` is the exact string
  // that made the first draft of this sweep report a phantom failure.
  // If someone loosens the matcher, this fails before it reaches a
  // reviewer.
  it('does not harvest keys from identifiers ending in t', () => {
    const synthetic = `
      import { t } from '$stores/lang.svelte';
      const mod = await import('hls.js');
      const fmt = await someImport('not-a-key');
      const x = obj.t('also.not.a.key');
      const real = t('common.loading');
    `;
    const name = localTName(synthetic);
    expect(name).toBe('t');
    expect(extractKeys(synthetic, name!)).toEqual(['common.loading']);
  });

  // Positive control: the guard must actually fire on a bad key.
  it('flags a key that is absent from en.json', () => {
    const synthetic = `
      import { t } from '$stores/lang.svelte';
      const a = t('common.loading');
      const b = t('admin.ai_inference.budget_hard_label');
    `;
    const keys = extractKeys(synthetic, localTName(synthetic)!);
    expect(keys.filter((k) => !(k in EN_FLAT))).toEqual([
      // The pre-#774 prefix. Lives at admin.system.ai_inference.*.
      'admin.ai_inference.budget_hard_label',
    ]);
  });

  // Interpolated keys resolve structurally, and a wrong prefix on an
  // interpolated key is still caught.
  it('checks interpolated keys against the key family', () => {
    expect(familyMatches('asset.tag_source.${source}')).toBe(true);
    expect(familyMatches('admin.asset.tag_source.${source}')).toBe(false);
    expect(familyMatches('nope.${x}.title')).toBe(false);
  });

  // Files that never import t() are skipped entirely, so a stray
  // `t(` in an unrelated module can't produce a phantom key.
  it('ignores files that do not import t', () => {
    expect(localTName(`const t = (s) => s; t('fake.key');`)).toBeNull();
  });
});

// Locale-parity — WARN-ONLY. es/fr are deliberately incomplete
// pre-#247; this surfaces the coverage gap without blocking CI. The
// numbers here are the baseline the #247 translation arc burns down.
describe('i18n locale parity (warn-only)', () => {
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
