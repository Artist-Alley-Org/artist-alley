# i18n coverage audit for v0.1.0

**Phase 1.55.V-1 — report-only.**
Audit date: 2026-07-09.
Repo tip: `feat/1.55.V-1-i18n-audit` (based on `origin/dev`).

## §0 Audit scope + methodology

### Scope

- Every `.svelte` file under `web/src/routes/**` and `web/src/lib/**`
  (169 files total in the current tree).
- Every `.svelte.ts` state file whose `error`/`loadError` strings reach
  the DOM.
- The i18n runtime (`web/src/lib/stores/lang.svelte.ts` +
  `web/src/lib/i18n/*.json`), the locale registry
  (`web/src/lib/i18n/locales.ts`), and the coverage guard
  (`web/src/lib/i18n/i18n-coverage.test.ts`).
- All 25 Playwright specs under `scripts/dogfood/ui/tests/` +
  14 vitest unit tests under `web/src/**`.
- Backend user-facing strings in `app/internal/http/**` and the
  cross-package `openapi.*JSONResponse{Error: "..."}` pattern.

### Methodology

**Q1–Q3** (architecture, guard, catalogue matrix): read the core
modules, ran leaf-key counts against every JSON dictionary, extracted
the tracked-file list from the coverage guard.

**Q4** (routes) + **Q5** (components): multi-pass grep for
five hardcoded-string patterns (`>Capitalised text<`,
`placeholder="Capital…"`, `aria-label="Capital…"`, `title="Capital…"`,
`alt="Capital…"`) followed by a manual read of the largest files in
each surface. State-file errors reaching the DOM traced from
`.svelte.ts` origin to the render site.

**Q6** (Playwright): grep every spec for locale-switching primitives
(`aa_lang`, `setLocale`, `switchLanguage`, `changeLanguage`, `'es'`,
`'fr'`, `Español`, `Français`, `data-locale=`, `lang=`) + read every
hit's assertion path.

**Q7** (backend): grep every `.go` under `app/internal/` for
`http.Error(w, ...)` and `Error:\s*"[A-Za-z]` inside error-response
structs, dedup by unique string, cross-check the frontend rendering
sites for `apiErr.error ?? t(...)` patterns.

**Q8** (fallback): read `lang.svelte.ts:t()` line-by-line, verified
the contract with the existing `lang.test.ts` unit tests.

### Finding structure

Every finding in §4-§7 follows the fixed shape:

```
- <Finding name>
  - Location: <file:line>
  - String: "<hardcoded text>"
  - Context: button label | placeholder | error message | ...
  - Recommendation: <suggested key + fix approach>
  - Priority: MUST | SHOULD | NICE
```

Priority tiers:

- **MUST**: user-visible on primary flows (login, upload, home,
  search, viewer, post detail, settings). Locale switch fails visibly.
- **SHOULD**: user-visible on secondary flows (admin, account
  sub-pages, viewer tool panels). Locale switch fails less visibly.
- **NICE**: technical/rare (dev tools, error debug modals, edge-case
  unreached-in-normal-use text). Cost > benefit.

## §1 i18n system architecture

### Core modules

- **Locale registry** — `web/src/lib/i18n/locales.ts:28-51`.
  Hand-maintained `SUPPORTED_LOCALES` array `[en, es, fr]` with
  static `completionPct` (`100 / 5 / 5`) + CLDR-style
  `resolveLocale(pref)` (exact match → language stem → default).
  `DEFAULT_LOCALE = 'en'`.
- **Runtime store** — `web/src/lib/stores/lang.svelte.ts:71-134`.
  Class `LangState` exported as singleton `lang` (line 153) with bound
  `t` (line 160).
- **Catalogues** — three JSON files under `web/src/lib/i18n/`:
  `en.json` (91 685 bytes, 1 733 lines, **1 608 leaf keys**),
  `es.json` (2 396 bytes, 72 lines, **52 leaf keys**),
  `fr.json` (1 971 bytes, 53 lines, **37 leaf keys**).
- **Coverage guard** — `web/src/lib/i18n/i18n-coverage.test.ts`
  (vitest, 164 lines). Detail in §2.
- **Server-side locale endpoint** — `app/internal/i18n/handler.go:57-81`
  serves `GET /i18n/locales` with a hard-coded registry that must be
  kept in sync by hand with `locales.ts:28-32` (per
  `handler.go:24-27`). **Currently unused by the frontend** — the
  registry ships bundled; no `.svelte` or `.ts` file references the
  endpoint. Registry `completionPct` (5%) is stale vs. real coverage
  (3.2%/2.3%).

### Dictionary structure

Catalogues are nested JSON objects, up to 6 levels deep at
authoring time (`admin.sections.identity.tiles.users.title`). The
store flattens once at module load via `flatten()`
(`lang.svelte.ts:45-55`) into `Record<string,string>` with dotted
keys — numbers/booleans stringify, arrays get `String()`d.

### Translation function

- Signature: `t(key: string, vars?: Record<string, string | number>): string`
  (`lang.svelte.ts:123-133`).
- Interpolation: `{var}` syntax, regex `/\{(\w+)\}/g`
  (`interpolate()`, `lang.svelte.ts:63-69`).
- Import forms:
  - `import { t } from '$stores/lang.svelte'` — bound convenience
    (83 `.svelte` templates use this).
  - `import { lang } from '$stores/lang.svelte'` then `lang.t(...)` —
    used where callers also need `lang.pref` / `lang.resolved`
    (e.g. `UserMenu.svelte:31,141`,
    `account/preferences/+page.svelte:58,176-186`).
- **`.svelte.ts` state files never import `t`.** Grep across
  `**/*.svelte.ts` returns nothing. Every translation happens in the
  `.svelte` template layer.
- Reactivity: reading `this.resolved` inside `t()` subscribes the
  caller's `$derived` / render-effect to locale changes
  (`lang.svelte.ts:124-127`).

### Runtime vs. compile-time

**Compile-time bundle, runtime lookup.** Catalogues are ES-module JSON
imports, so Vite bundles all three at build time. Lookups happen at
runtime against the pre-flattened `FLAT` table. Adding a locale
requires a rebuild.

### Locale switching

Three-tier persistence with priority (per
`LangState.init()`, `lang.svelte.ts:86-96`):

1. **User profile** `user.language` — canonical once signed in.
2. **Cookie** `aa_lang` (name at `lang.svelte.ts:33`;
   `Max-Age = 1 year`, `SameSite=Lax`, `Path=/`, written client-side
   at `lang.svelte.ts:147-151`).
3. **`navigator.language`** via `systemPref()`.
4. Otherwise `DEFAULT_LOCALE`.

`lang.init()` invoked once in the root layout
(`web/src/routes/+layout.svelte:35`, inside `onMount`).

`lang.set(pref)` writes the cookie and (if signed in) PATCHes
`/users/{ref}` with `{ language: pref }`. PATCH failure is swallowed
silently at `lang.svelte.ts:113-115` — cookie + in-memory state remain
authoritative for the UI.

**No HTTP `Accept-Language` header path.** The server never inspects
locale for HTML rendering (SPA — SvelteKit runs client-side).

### Fallback behaviour on missing keys

See §8 for the full analysis. Summary: silent English fallback for
keys present in `en.json`, raw key echo when even `en.json` is
missing.

## §2 Coverage guard analysis

### Location

`web/src/lib/i18n/i18n-coverage.test.ts` (164 lines). Only occurrence
in the repo — no companion script, no CI-specific runner.

### What it checks

Three regex heuristics against `.svelte` source (after stripping
`<script>` blocks at `i18n-coverage.test.ts:101`):

- `TEXT_BETWEEN_TAGS` (line 71) — `/>([^<>{}\n]+?)</g`. Text nodes
  between adjacent tags.
- `QUOTED_ATTR` (line 72) — matches `placeholder=`, `title=`,
  `aria-label=`, `alt=` with any of five quoting styles (double,
  single, `{\`…\`}`, `{'…'}`, `{"…"}`).
- `PURE_T_CALL_ATTR` (line 73) — whitelists `placeholder={t(...)}`
  (etc.) so already-migrated attrs don't false-positive.

The `isLikelyEnglish()` filter (`i18n-coverage.test.ts:75-90`)
rejects: strings under 2 chars, entries in the `ALLOW` set (`['—']`
at line 67-69), non-alpha strings, mustache-only fragments, all-caps
strings ≤3 chars (accepted false-negative for "OK", per comment
line 84-86). Accepts if the string contains a lowercase letter or a
space.

A synthetic positive-control test
(`i18n-coverage.test.ts:139-151`) pins the heuristic — if a future
refactor breaks the regex, that test fails first.

### Files it scans — explicit allow-list of 24 files

`TRACKED_FILES` at `i18n-coverage.test.ts:24-49`:

```
src/lib/components/viewers/ArchiveView.svelte
src/lib/components/viewers/tools/ArchiveTool/Body.svelte
src/lib/components/CollectionFieldsSection.svelte
src/routes/admin/users/+page.svelte
src/routes/admin/users/[ref]/+page.svelte
src/routes/admin/teams/+page.svelte
src/routes/admin/teams/[id]/+page.svelte
src/routes/admin/asset-types/+page.svelte
src/routes/admin/asset-types/[ref]/+page.svelte
src/routes/admin/system/log/+page.svelte
src/routes/admin/system/license/+page.svelte
src/routes/admin/system/activities/+page.svelte
src/routes/admin/system/users/+page.svelte
src/routes/admin/fields/+page.svelte
src/routes/admin/federation/peers/+page.svelte
src/routes/admin/federation/directories/+page.svelte
src/routes/account/sessions/+page.svelte
src/routes/account/password/+page.svelte
src/routes/account/preferences/+page.svelte
src/routes/account/blocked/+page.svelte
src/routes/account/notifications/+page.svelte
src/routes/account/messages/+page.svelte
src/routes/account/messages/[peer]/+page.svelte
src/routes/account/profile/+page.svelte
```

### Files it skips

Everything else. Repo has **169 `.svelte` files** — guard covers
**24 (~14%)**. Explicit non-goal per the header comment
(`i18n-coverage.test.ts:11-14`): _"we don't lint the entire repo
because most of it predates the i18n plumbing and the diff would be
too loud to be actionable."_

### CI wiring

- Wired implicitly via `npm test` in `.github/workflows/ci.yml:134-140`
  (job `vitest (frontend unit tests)`). Vitest picks it up
  automatically via the `**/*.test.ts` glob.
- `web/package.json` script: `"test": "vitest run"` — no separate
  i18n job.

### Block or warn

**Hard block.** Each tracked file becomes an `it(...)` with
`expect(offenders, ...).toEqual([])` at `i18n-coverage.test.ts:132`.
Any offender fails the assertion → vitest fails → CI fails.

### Last fire on record

- File created 2026-06-03 in commit `21441c8d`.
- Last fire: **PR #198** (`eef04486`, merged 2026-07-04, "feat(1.19.D):
  per-username account lockout"). Fired 11 hits under
  `admin.user_detail.lockout_*`; the fix routed those strings through
  `t()` against 11 new `en.json` keys.

### What it MISSES

Six classes of raw English can slip past:

1. **Any file not in `TRACKED_FILES`.** 145 of 169 `.svelte` files
   (~86%) are unguarded — including the entire
   `/routes/collections/*`, `/routes/search/*`, `/routes/setup/`,
   `/routes/+page.svelte` (home), viewer components (`PostHost`,
   `AssetPlaylist`, `CommentsThread`), upload modals, admin
   operational surfaces (`admin/federation/{inbox,outbox,shares}`,
   `admin/search/*`), `AdminSectionLanding`, `AdminMenu`,
   `BrowseFooter`, and every `.svelte.ts` session file.
2. **`<script>` blocks are stripped.** Toast messages, error strings,
   `throw new Error('...')`, and dropdown option labels built in TS
   all pass unchecked (accepted false-negative per header comment
   lines 62-66).
3. **Only 4 attributes.** `placeholder|title|aria-label|alt`. Missed:
   `label="..."` on `<option>` / custom form components,
   `aria-labelledby` targets, `aria-description`, `aria-details`,
   `value="..."` when it's the display text, `data-tooltip="..."`.
4. **Template-literal / concat titles.** `title={t(...) + " literal"}`
   or `title={\`hello ${x}\`}` — the `QUOTED_ATTR` regex has five
   alternates but none cover backtick templates with `${}` or
   string concat.
5. **Multi-line text nodes.** `TEXT_BETWEEN_TAGS` uses `[^<>{}\n]+?`;
   anything with `\n` inside splits into half-matches, both usually
   fail `isLikelyEnglish` on length.
6. **HTML entities.** `&mdash;` and similar treated as English text;
   `ALLOW` only lists the literal `—` character.

## §3 Dictionary coverage matrix

### Locale files (paths + sizes + leaf-key counts)

| Locale | Path | Bytes | Lines | Leaf keys |
|---|---|---:|---:|---:|
| en | `web/src/lib/i18n/en.json` | 91 685 | 1 733 | **1 608** |
| es | `web/src/lib/i18n/es.json` | 2 396 | 72 | **52** |
| fr | `web/src/lib/i18n/fr.json` | 1 971 | 53 | **37** |

Overall: es = **3.2%** of en, fr = **2.3%** of en. Both a hair below
the `completionPct: 5` claim in `locales.ts:30-31` +
`handler.go:40-41`.

### Per-top-level-section coverage

| Section | EN | ES | ES % | FR | FR % |
|---|---:|---:|---:|---:|---:|
| **account** | 208 | 7 | 3.4 % | 0 | 0 % |
| **admin** | 1019 | 23 | 2.3 % | 22 | 2.2 % |
| admin_menu | 11 | 8 | 72.7 % | 1 | 9.1 % |
| aiedit | 19 | 0 | 0 % | 0 | 0 % |
| archive | 31 | 0 | 0 % | 0 | 0 % |
| blogs | 2 | 0 | 0 % | 0 | 0 % |
| browse | 34 | 0 | 0 % | 0 | 0 % |
| collection_picker | 11 | 0 | 0 % | 0 | 0 % |
| collections | 67 | 0 | 0 % | 0 | 0 % |
| common | 16 | 2 | 12.5 % | 2 | 12.5 % |
| errors | 1 | 0 | 0 % | 0 | 0 % |
| impersonation | 4 | 0 | 0 % | 0 | 0 % |
| login | 16 | 0 | 0 % | 0 | 0 % |
| messages | 14 | 0 | 0 % | 0 | 0 % |
| nav | 13 | 2 | 15.4 % | 2 | 15.4 % |
| notifications | 24 | 0 | 0 % | 0 | 0 % |
| playlist_actions | 4 | 0 | 0 % | 0 | 0 % |
| post_menu | 2 | 0 | 0 % | 0 | 0 % |
| register | 17 | 0 | 0 % | 0 | 0 % |
| review | 2 | 0 | 0 % | 0 | 0 % |
| search | 2 | 0 | 0 % | 0 | 0 % |
| social | 7 | 0 | 0 % | 0 | 0 % |
| **user_menu** | 10 | 10 | **100 %** | 10 | **100 %** |
| verify | 12 | 0 | 0 % | 0 | 0 % |
| viewer_hotkeys | 25 | 0 | 0 % | 0 | 0 % |
| viewer_menu | 37 | 0 | 0 % | 0 | 0 % |

### `admin.*` sub-section breakdown

| Sub-section | EN | ES | FR |
|---|---:|---:|---:|
| admin.system | 297 | 0 | 0 |
| admin.sections | 222 | 0 | 0 |
| admin.federation | 189 | 0 | 0 |
| admin.user_detail | 65 | 0 | 0 |
| admin.audit | 37 | 0 | 0 |
| admin.activities | 28 | 0 | 0 |
| admin.users | 28 | 0 | 0 |
| admin.team_detail | 26 | 0 | 0 |
| admin.asset_type_detail | 23 | 0 | 0 |
| **admin.fields** | 22 | **15 (68 %)** | **15 (68 %)** |
| admin.teams | 19 | 0 | 0 |
| admin.asset_types | 10 | 0 | 0 |
| admin.requests | 9 | 0 | 0 |
| admin.asset | 9 | 0 | 0 |
| admin.about | 7 | 0 | 0 |
| **admin.collection_fields** | 7 | **7 (100 %)** | **7 (100 %)** |
| admin.tile | 6 | 0 | 0 |
| admin.api_explorer | 4 | 0 | 0 |
| admin.roles | 3 | 0 | 0 |
| admin.status | 3 | 0 | 0 |
| admin.workflow | 3 | 0 | 0 |
| admin.title | 1 | 1 | 0 |
| admin.intro | 1 | 0 | 0 |

### Fully-translated sections in both es + fr

- `user_menu` (10/10, 100 %)
- `admin.collection_fields` (7/7, 100 %)

Also fully covered in es only: `admin.title` (1/1). Near-full:
`admin_menu` in es is 8/11 (72.7 %). `admin.fields` (both es + fr)
is 15/22 (68 %); the 7 missing en keys are filter labels added after
the original translation snapshot.

### Stub sections (zero keys)

- **In es**: 20 sections — `aiedit, archive, blogs, browse,
  collection_picker, collections, errors, impersonation, login,
  messages, notifications, playlist_actions, post_menu, register,
  review, search, social, verify, viewer_hotkeys, viewer_menu`.
- **In fr**: 21 sections — same as es **plus `account`** (fr never
  translated any of the 208 account keys; es managed 7).

### Divergence (keys in es or fr but not en)

**None.** Programmatic check
`set(flatten(es)) − set(flatten(en))` and same for fr both return the
empty set. No orphan translations, no drift from the en schema.

### The 1.19.D lockout keys never propagated

`admin.user_detail.lockout_*` (11 new keys added in PR #198,
2026-07-04) has **zero es/fr coverage**. The 1.19.D fire proved the
guard works on the source side, but the newly-added keys never
propagated to `es.json`/`fr.json`. This is the mechanism that leaves
`es.json` and `fr.json` frozen forever — the guard has no notion of
"key exists in every locale."

## §4 Hardcoded string findings — routes

**Total findings: ~275 — ~87 MUST / ~37 SHOULD / ~150+ NICE.**

Scope: `web/src/routes/**/*.svelte`. Files not listed below have been
read or grepped and are effectively fully translated (login,
register, auth/verify, home layout, layout children, account
overview/layout/profile/preferences/2fa/messages inbox, blogs,
review, admin layout + landing + section landings, and ~20 admin
pages that already route text through `t()`).

### §4.1 MUST — primary-flow findings

#### `web/src/routes/+page.svelte` (browse feed home)

- **Browser tab title (search + default)**
  - Location: `+page.svelte:204`
  - String: `` `${query} — artist-alley` `` and `'Browse — artist-alley'`
  - Context: `<svelte:head>` title
  - Recommendation: `browse.title`, `browse.title_search`
  - Priority: MUST
- **Results-for heading**
  - Location: `+page.svelte:210`
  - String: `"Results for"`
  - Context: heading under active search
  - Recommendation: `browse.results_for` with `{query}` var
  - Priority: MUST
- **Empty-state title (no matches / no posts)**
  - Location: `+page.svelte:222`
  - String: `"No matches"` / `"No posts yet"`
  - Context: empty-state h2
  - Recommendation: `browse.empty.no_matches`, `browse.empty.no_posts_yet`
  - Priority: MUST
- **Empty-state body**
  - Location: `+page.svelte:225-226`
  - String: `"Try a different search term."` /
    `"Once posts are uploaded they'll appear here, newest first."`
  - Context: empty-state paragraph
  - Recommendation: `browse.empty.try_different`, `browse.empty.uploaded_appear_here`
  - Priority: MUST
- **Footer sentinel**
  - Location: `+page.svelte:278`
  - String: `"— end of feed —"`
  - Context: infinite-scroll footer
  - Recommendation: `browse.end_of_feed`
  - Priority: MUST
- **Error banner fallback**
  - Location: `+page.svelte:96,104`
  - String: `'Failed to load'`
  - Context: error state, rendered at line 217
  - Recommendation: `errors.failed_to_load`
  - Priority: MUST

#### `web/src/routes/posts/[id]/+page.svelte`

- **Browser tab title**
  - Location: `posts/[id]/+page.svelte:36`
  - String: `"Post — artist-alley"`
  - Recommendation: `post.detail.title`
  - Priority: MUST

#### `web/src/routes/setup/+page.svelte` (first-run install wizard)

Entire page bypasses `t()`. Setup is the very first surface a fresh
install renders, cannot be skipped, and every visible string is a
MUST hit. **~30 strings** — enumerated inline rather than grouped
since every one is MUST-tier:

- Errors: `setup/+page.svelte:75,79,114,123`
  - `'Passwords do not match.'`, `'Password must be at least 8 characters.'`,
    `'Setup failed'` — Recommendation: `setup.err.*`
- Titles + headings + body copy:
  - `:131` `"First-run setup — artist-alley"` — `setup.title`
  - `:138` `"First-run setup"` — `setup.heading`
  - `:140-141` `"Create the first administrator and configure your site. You can change any of this later."` — `setup.body`
  - `:153` `"Administrator"`, `:209` `"Site"`, `:234` `"SMTP"` — section headings, `setup.section.*`
- Field labels + placeholder + helper:
  - `:158,166,175,183,193` `"Username"`, `"Email"`, `"Full name"`, `"Password"`, `"Confirm password"` — `setup.admin.*`
  - `:189` `"At least 8 characters."` — `setup.admin.password_hint`
  - `:213-227` `"Site name"`, `"Base URL"`, placeholder `"https://art.example.com"`, helper `"Used in outgoing links (e.g. password reset). May be left blank now."` — `setup.site.*`
  - `:242` checkbox `"Configure now"` — `setup.smtp.configure_now`
  - `:248-293` SMTP fields (`"Host"`, `"Port"`, `"Encryption"`, option `"None"` — STARTTLS/TLS proper nouns can stay, `"From address"`, placeholder `"Site <noreply@example.com>"`, `"Username"`, `"Password"`) — `setup.smtp.*`
  - `:297` body copy `"Email features (password reset, notifications) stay disabled until SMTP is configured. You can fill this in later from the admin settings."` — `setup.smtp.body`
- Submit label:
  - `:304` `"Create admin & finish setup"` — `setup.submit`
- Priority (all above): **MUST**

#### `web/src/routes/search/+page.svelte` (main unified search)

`<svelte:head>` uses `t('nav.advanced_search')` but the body is
largely raw text — ~30 strings on the primary search flow:

- `:248` `"Facets"` (sidebar heading) — `search.facets_heading` — MUST
- `:254` `"Clear all"` (button) — `search.clear_all` — MUST
- `:293` `"Advanced builder"` (link) — `search.advanced_builder` — MUST
- `:300` `"Save search"` (button) — `search.save_search_button` — MUST
- `:306` `"Save as collection"` (button) — `search.save_as_collection` — MUST
- `:315` placeholder `"Type a query…"` — `search.query_placeholder` — MUST
- `:323` submit `"Search"` — `common.search` — MUST
- `:334` counter `"Showing {n} of {total} results"` (English wraps around interpolations) — `search.counter` — MUST
- `:339` `"Searching…"` (status) — `search.searching` — MUST
- `:341` empty state `"No matches. Try a different query."` — `search.no_matches` — MUST
- `:351` badge `"Federated"` — `search.federated_badge` — MUST
- `:355` fallback `"Untitled"` — `common.untitled` — MUST
- `:376` pagination `"Loading…"` / `"Load more"` — `search.load_more_*` — MUST
- Error state `:102` `` `search: ${searchResp.status}` `` (rendered 328) — `search.err_generic` — MUST
- Save-as-collection modal (`:387-415`): `"Save search as collection"`, `"Save these results as a collection"`, `"Collection name"`, `"Cancel"`, `"Saving…"` / `"Save collection"` — `search.save_collection.*` — MUST
- Save-search modal (`:428-479`): `"Save search"`, `"Save this search for updates"`, `"We'll re-run this query on your interval and email you when new matches appear."`, `"Name"`, `"Notify every (minutes)"`, `"Notification channel"`, options `"Email digest"` / `"Track only (no email)"`, `"Cancel"`, `"Saving…"` / `"Save search"` — `search.save_search.*` — MUST
- Save status (`:187,191,225,229`): `` `Save failed: ${status} ${err}` ``, `` `Saved ${count} results${' (truncated to first 100)'}.` ``, `` `Saved. Runs every ${minutes} minutes.` `` — `search.save_result_*` — MUST

#### `web/src/routes/search/advanced/+page.svelte` (DSL builder)

Whole file has zero `t()` imports. ~15 strings:

- `:14-24` FIELD options (rendered as `<option>` text at `:99`):
  `"Title"`, `"Description / body"`, `"Tag"`,
  `"Owner (username or ref)"`, `"Asset type"`, `"Sensitivity"`,
  `"File extension"`, `"Similar to asset (UUID)"` — `search.advanced.field.*` — MUST
- `:66` title `"Advanced search — artist-alley"` — `search.advanced.title` — MUST
- `:69` heading `"Advanced search"` — `search.advanced.heading` — MUST
- `:71-75` body copy — `search.advanced.body` — MUST
- `:79` `"Free-text query (optional)"` — `search.advanced.freetext_label` — MUST
- `:83` placeholder `"cat OR dog"` — `search.advanced.freetext_placeholder` — MUST
- `:92` checkbox `"NOT"` — `search.advanced.not` — MUST
- `:105` placeholder `"value"` — `search.advanced.value_placeholder` — MUST
- `:113` `aria-label="Remove row"` — `search.advanced.remove_row` — MUST
- `:123` `"+ Add row"` — `search.advanced.add_row` — MUST
- `:126,127` `"Compiled DSL"`, fallback `"(empty)"` — `search.advanced.compiled_*` — MUST
- `:135` submit `"Search"` — `common.search` — MUST

#### `web/src/routes/collections/[id]/+page.svelte`

Body is well translated. Untranslated remnants:

- `:123` state `'restore failed'` (rendered `:210`) — `collections.restore_failed` — MUST
- `:204-207` admin banner `"Deleted {date}"`, `"— {reason}"` — `collections.deleted_at_banner` — MUST
- `:220` `"Restoring…"` / `"Restore"` — `collections.restoring`, `collections.restore` — MUST

#### `web/src/routes/collections/+page.svelte`

- `:176` admin-only checkbox `"Include deleted"` — `collections.include_deleted` — MUST (surfaced when caller has `system.admin`)

### §4.2 SHOULD — secondary-flow findings

#### `web/src/routes/account/security/+page.svelte` (federation key rotation)

Zero `t()` imports; whole page untranslated. Account-settings sub-page
(the primary security surface — `/account/security/2fa` — IS
translated), so this is SHOULD. ~15 strings including:

- `:36` error `'Rotation failed.'`
- `:47` title `"Security — artist-alley"`
- `:49` heading `"Federation encryption keys"`
- `:50-56` intro paragraph (X25519, grace window)
- `:60` status `"Rotation complete."`
- `:62-68` dt/dd labels (`"New version"`, `"Previous version"`,
  `"(retained N days)"`, `"Algorithm"`, `"New public key"`)
- `:80` CTA `"Rotate federation keys"`
- `:83-87` confirm paragraph
- `:95,103` confirm buttons (`"Rotating…"` / `"Yes, rotate now"`, `"Cancel"`)
- Recommendation: `account.federation_keys.*` namespace
- Priority: **SHOULD**

#### `web/src/routes/account/saved-searches/+page.svelte`

Zero `t()` imports; whole page untranslated. Reachable from the
digest email + from `/search` "Save search". ~20 strings including:

- `:37,56` error interpolation (` `load failed: ${status}` `, ` `run failed: ${status}` `) — rendered 113/142
- `:60` status ` `${hits} hits · ${added} new · ${notified ? 'emailed' : 'no email'}` `
- `:81` confirm dialog ` confirm(`Delete "${name}"?`) `
- `:90-97` relTime returns (`"never"`, `"just now"`, ` `${m}m ago` `, ` `${h}h ago` `, ` `${d}d ago` `)
- `:103,106` title + heading (`"Saved searches — artist-alley"`, `"Saved searches"`)
- `:107-110` intro paragraph
- `:117` `"Loading…"`
- `:119-122` empty state
- `:132` badge `"Paused"`
- `:137-139` row description (` `Every ${n} min · ${...}` `)
- `:152,157,162` buttons (`"Running…"` / `"Run now"`, `"Pause"` / `"Resume"`, `"Delete"`)
- Recommendation: `account.saved_searches.*`
- Priority: **SHOULD**

#### `web/src/routes/account/tokens/+page.svelte`

- `:123` warning `"Save this token — it is never shown again:"` — `account.tokens.save_token_warning` — SHOULD

#### `web/src/routes/account/messages/[peer]/+page.svelte`

- `:152` fallback header `` `Peer #${peerRef}` `` (visible while `peerName` resolution races or fails) — `messages.peer_fallback` — SHOULD

### §4.3 NICE — technical / admin surfaces (grouped)

15+ admin operational pages have 0-3 `t()` calls each. Grouped rather
than enumerated per line because the pattern is uniform: intro text,
filter labels, table column headers, empty-state text, button labels,
page titles all bypass `t()`.

**Fully-untranslated (each is one file, one recommendation: fully
port under an `admin.<page>.*` namespace):**

- `admin/search/dashboard/+page.svelte` (~29 hits)
- `admin/federation/outbox/+page.svelte`
- `admin/federation/inbox/+page.svelte`
- `admin/federation/key-health/+page.svelte`
- `admin/search/feedback/+page.svelte`
- `admin/search/feedback/audit/[user_ref]/+page.svelte`
- `admin/search/reindex/+page.svelte`
- `admin/search/disk-usage/+page.svelte`
- `admin/search/visual-backfill/+page.svelte`
- `admin/saved-searches/+page.svelte`
- `admin/saved-searches/failures/+page.svelte`
- `admin/metadata-extraction/failures/+page.svelte`
- `admin/metadata-extraction/backfills/+page.svelte`

All priority: **NICE** (operator surface; deferrable to a batch sweep
in a future ops-translation phase).

**Effectively fully translated (grep passed):**
`admin/federation/{peers,directories,shares}`, `admin/system/log`,
`admin/system/license`, `admin/system/activities`,
`admin/system/site`, most `admin/asset-types`, `admin/fields`,
`admin/teams`, `admin/roles`, `admin/requests`, `admin/workflow`,
`admin/about`, `admin/integrations/api`, `admin/users`,
`admin/ai/*`. Residual literals in these are proper-noun `<option>`
values (e.g. `"OpenAI"`, `"GitHub"`) — brand names, safe to skip.

### §4.4 Cross-cutting route notes

- `<svelte:head>` `— artist-alley` suffix is a brand string; team
  convention is to leave it literal.
- Data-driven enum text (e.g. `{r.state}` state labels `"pending"` /
  `"granted"` / `"denied"` / `"expired"`) is rendered verbatim from
  server data. NICE / potentially a Q7-style backend-code
  translation gap depending on stack; not enumerated here.
- Decorative logo alt attrs (`login/+page.svelte:128`,
  `register/+page.svelte:116`, `auth/verify/+page.svelte:84`,
  `setup/+page.svelte:137`, `+layout.svelte:113`) all correctly use
  `alt=""` with `aria-hidden`. Pass.

## §5 Hardcoded string findings — shared components

**Total findings: ~340 — 79 MUST / 141 SHOULD / 120 NICE.**

Scope: 93 `.svelte` files under `web/src/lib/**` +
`.svelte.ts` state files whose error strings reach the DOM.

Roughly 27 of 93 components import `t()` today. This section groups
by directory; every MUST hit is individually enumerated, SHOULD /
NICE hits grouped when the pattern is uniform.

### §5.1 MUST — foundational chrome + primary components

#### `web/src/lib/components/CollectionModal.svelte`

- `:75` `aria-label="Close"` — `common.close` — MUST

#### `web/src/lib/components/SearchBar.svelte` (nav-cluster primary)

- `:26` default prop `placeholder = 'Search'` — `nav.search_placeholder` (dict already has this) — MUST
- `:223` `aria-label="Search"` — `nav.search_label` — MUST
- `:236` `aria-label="Clear search"` — `search.clear` — MUST
- `:265` heading `>Suggestions<` — `search.suggestions_heading` — MUST
- `:283` heading `>Recent searches<` — `search.recent_heading` — MUST
- `:288` button `>Clear<` — `search.clear_history` — MUST

#### `web/src/lib/components/NavUploadButton.svelte`

- `:26` `title="Upload (or drop files anywhere)"` — `nav.upload_button_title` — MUST
- `:27` `aria-label="Upload"` — `nav.upload` — MUST

#### `web/src/lib/components/UserMenu.svelte`

- `:43` `title="User menu"` — `nav.open_user_menu` — MUST

#### `web/src/lib/components/MessagesButton.svelte`

- `:148` `>{fromMe ? 'You: ' : ''}<...` — hardcoded English "You: " prefix rendered in thread body preview — `messages.you_prefix` — MUST

#### `web/src/lib/components/CommentsThread.svelte` (per-post)

~15 strings; see full inventory in the raw scan. Highlights:

- Relative-time suffixes `:225-232` (`'just now'`, `` `${m}m` ``, etc.) — `common.time.*` — MUST
- Fallback display name `:243` `` `user ${ref}` `` — `common.user_ref_fallback` — MUST
- Error fallbacks `:79,85,155,165,210,215` (`'Failed to load comments'`, `'Failed to post'`, `'Failed to delete'`) — `comments.err_*` — MUST
- Compose placeholder `:254` `placeholder="Add a comment…"` — `comments.compose_placeholder` — MUST
- Submit label `:266` `'Posting…'` / `'Comment'` — `comments.posting_label` / `.submit_label` — MUST
- Empty state `:291` `>No comments yet — be the first.<` — `comments.empty` — MUST
- Edit chrome `:315` `title="Edited …"`, `:316,360` `>(edited)<`, `:325,370` `>Reply<`, `:333,378` `>Delete<` — `comments.*` — MUST
- Reply modal `:397` placeholder, `:410,417` `>Cancel<` / posting label — `comments.reply_*` — MUST

#### `web/src/lib/components/PostHost.svelte`

- `:606` `aria-label="Whiteboard preview"` — `whiteboard.preview_dialog_label` — MUST
- `:610,838` fallback `'Untitled sketch'` — `whiteboard.untitled_sketch` — MUST
- `:615,616` `title="Close (Esc)"` + `aria-label="Close preview"` — `common.close_esc` / `.close_preview` — MUST
- `:656` `>Whiteboard<` — `whiteboard.title_slot_label` — MUST
- `:713` English pluralization `` `${author.post_count} post${author.post_count === 1 ? '' : 's'}` `` — `user_meta.post_count` (plural forms) — MUST
- `:729-730` `aria-label="Post actions"` + `title="Post actions"` — `post_menu.actions_button` — MUST
- `:777` badge `>team<` — `post_badges.team` — MUST
- `:793` `>Asset Details<` — `post_host.asset_details_heading` — MUST
- `:816` `Whiteboards` — `post_host.whiteboards_heading` — MUST
- `:827` `title="Preview whiteboard"` — `whiteboard.preview_button_title` — MUST
- `:850-851` `title="Delete whiteboard"` + aria — `whiteboard.delete_button` — MUST
- `:863` empty state — `post_host.whiteboards_empty` — MUST
- `:883` `Metadata` — `post_host.metadata_heading` — MUST
- `:928` `title={liked ? 'Unlike' : 'Like'}` — `post_host.like_button` / `.unlike_button` — MUST

#### `web/src/lib/components/AssetPlaylist.svelte`

- `:537` `aria-label="Previous asset"` — `viewer_playlist.prev_asset` — MUST
- `:550` `aria-label="Next asset"` — `viewer_playlist.next_asset` — MUST
- `:585` `aria-label="Resize playlist strip"` — `viewer_playlist.resize_strip` — MUST
- `:599` `aria-label={stripCollapsed ? 'Show asset strip' : 'Hide asset strip'}` — `viewer_playlist.show_strip` / `.hide_strip` — MUST
- `:624` `aria-label="Show asset {i + 1}"` — `viewer_playlist.show_asset_n` — MUST
- `:657` empty state `>No assets in this playlist.<` — `viewer_playlist.empty` — MUST

#### `web/src/lib/components/federation/RestrictedShareBanner.svelte`

7 strings, all MUST — `federation.*` namespace.

- `:46` aria-label, `:52` headline, `:53-60` full body para, `:62-65` grant headline, `:66-69` grant body, `:71-72` two `<li>` options.

#### `web/src/lib/components/upload/*.svelte` (5 files)

Primary flow. ~70 MUST hits total; grouped by file, key inventory in
the raw scan. Highlights:

- `UploadModal.svelte:59-61` derived submit labels (with plural + `{n}` var); `:84` heading `>Upload<`; `:89` close aria; `:115` dropzone prompt; `:133` empty state; `:159,161` status labels; `:170,178` buttons — `upload.*`
- `PostComposeForm.svelte:90` toggle label; `:99,102,106,109` title + description placeholders + aria; `:116-186` all field labels (`Visibility`, `Post mode`, `Workflow state`, `Tags`), all `<option>` labels (`Public`, `Followers`, `Private`, `One post with all files`, `One post per file`, `Default`), tag chip aria-labels, collection prefill note — `upload.compose.*`
- `UploadDropZone.svelte:28-29` overlay title + subtitle — `upload.dropzone.*`
- `UploadFileRow.svelte:221` title aria; `:230-231` remove aria + title; `:224` state label (English `Pending`/`Uploading`/`Ready`/`Errored`); `:252` `Dedup'd from existing bytes`; `:262` retry; `:276,289` tag placeholders; `:304-355` metadata summary + loading + no-fields + `Yes`/`No`; `:480` remove-companion aria — `upload.file_row.*`
- `ThumbnailPicker.svelte:108` heading; `:122,127,130,141` mode labels + `Coming soon` title; `:155,176,185,187,193,196` state text (waiting / hint / uploading / ready / error) — `upload.thumbnail.*`

#### `web/src/lib/**/*.svelte.ts` state files (error strings reaching DOM)

**All MUST** — surface as visible error banners:

- `playlist/postSource.svelte.ts:115` `'Failed to load post'` → `playlist.err_load_post`
- `playlist/postSource.svelte.ts:123` `'Untitled'` → `common.untitled`
- `playlist/postSource.svelte.ts:146` `'Failed to load'` → `common.err_load`
- `stores/upload.svelte.ts:350,355,379,481,499,567,568,607,670,743` — 10 English error strings rendered into `composeError` / `row.error` — `upload.err_*` / `common.err_network`
- `stores/auth.svelte.ts:105` `'Invalid credentials'` fallback — `auth.err_invalid_credentials`

### §5.2 SHOULD — viewer + tool panels (grouped)

- **`viewers/AssetViewer.svelte`** (~25 hits) — transport rail (`title="Jump to frame (G)"`, jump form (`>Go to<`, placeholder `"frame, mm:ss, or 5.2s"`, `>Go<`), `aria-label="Scrubber"`, bookmark titles, `title="−10 (Shift+←)"` / `"Step back (,)"` / `"Play/Pause (K)"` / `"Step fwd (.)"` / `"+10 (Shift+→)"` / `"Speed {r}×"`, loop in/out titles + labels, `>clear<`, hotkey hint line `>JKL · ⇧← → · I/O loop · 1-5 speed · G goto · F fullscreen · ⌘wheel zoom<`) — `viewer.*` — SHOULD
- **`viewers/ToolPanelShell.svelte`** (~7 hits) — `title="Expand panel"` + aria, `aria-label="Asset tools"`, `>No tool<`, expand/shrink/collapse titles — `viewer.*` — SHOULD
- **`viewers/ViewerMenuBar.svelte`** (1 hit) — `:475` `aria-label="Active"` — `viewer_menu.active` — SHOULD. `:461` fallback `?? 'Side panel'` after `t('viewer_menu.side_panel')` — remove fallback — NICE.
- **`viewers/AudiobookView.svelte`** (5 hits) — `>end of chapter<`, cancel-sleep aria, remaining ETA, `>.aax can't play in the browser<` + explanation — `audiobook.*` — SHOULD
- **`viewers/EpubView.svelte`** (8 hits) — prev/next/close-TOC aria + titles, `>Loading EPUB…<`, error strings, chapter picker — `epub.*` — SHOULD
- **`viewers/PDFView.svelte`** (4 hits) — loading + error + download-original + rendered-pages — `pdf.*` + `common.download_original` — SHOULD
- **`viewers/FontView.svelte`** (~14 hits) — error + loading + `>Try it<` + `>Size<`, dark/light bg toggles, glyph coverage heading, full metadata dt list — `font.*` — SHOULD
- **`viewers/ImageView.svelte`** (~3 hits) — download-original + preview-unavailable — `common.*` / `image.*` — SHOULD
- **`viewers/DocView.svelte`** (~11 hits) — loading, download-original, annotation toolbar aria, colour swatches, highlighter buttons (`Highlight`, `Strikethrough`, `Underline`, `Comment`, `Sticky note`), draft placeholder, `>Cancel<` / `>Save<` — `doc.*` + `common.*` — SHOULD
- **`viewers/PlaceholderView.svelte`** (~2 hits) — phase note + download-original — `placeholder.*` — SHOULD
- **`viewers/ModelView.svelte`** (1 hit) — download-original — `common.*` — SHOULD
- **`viewers/MediaView.svelte`** (~3 hits) — waveform reset title + zoom pill + scrubber aria — `media.*` — SHOULD
- **`viewers/SpriteCanvas.svelte`** (~8 hits) — download-original, `>Loading sprite…<`, sprite HUD line, frame-tile titles (5 variants), `aria-label="Has a note"` — `sprite.*` — SHOULD
- **`viewers/SpriteToolPanel.svelte`** (**~70 hits**, grouped): dozens of section headings (`Display`, `Image info`, `Palette swap`, `Alternates`, `Auto detect`, `Metadata`, `Animations`, `Frame ops`, `Lightbox`, `Slice grid`, `Playback`, `Slices`, `Onion skin`, `Export`), field labels (`Zoom`, `Smoothing`, `Background`, `Dimensions`, `Cell W/H`, `Origin X/Y`, `Pad X/Y`, `Prev/Next frames`), tooltip titles, placeholders (`Label (defaults to a timestamp)…`, `Tag name…`, `Brainstorm note for this frame…`, `New slice name…`), aria-labels — `sprite_tool.*` — SHOULD (single fix pattern)

### §5.3 SHOULD — viewer tool bodies

10 `Body.svelte` files under `viewers/tools/*/Body.svelte` — each
`~15-40` hits. Grouped fix: namespace section labels under
`<tool>_tool.*`:

- `AudiobookTool/Body.svelte` (~21 hits) — dt/dd labels + placeholder + save/edit — `audiobook_tool.*`
- `EbookTool/Body.svelte` (~10 hits) — section headings + `placeholder="Find in book…"` + `placeholder="Optional note…"` + `title="Bookmark current chapter"` + remove-bookmark title/aria — `ebook_tool.*`
- `DocTool/Body.svelte` (~30 hits) — reading / font / find / bookmarks / stats + all case-sensitive/whole-word/regex titles + `>Cancel<` / `>Save<` — `doc_tool.*`
- `ModelTool/Body.svelte` (~40 hits) — FOV/frame/reset/spin/env/exposure/color/shadows/ground plane/contact shadow/grid/axes/bounding box/loop/rewind + long tooltip strings — `model_tool.*`
- `DetailsTool/Body.svelte` (~5 hits) — dt labels + `>Download original<` — `details_tool.*` + `common.download_original`
- `ArchiveTool/Body.svelte` — passed
- All 10 `Tips.svelte` — NICE tier (accordion-collapsed by default; hotkey rows)

### §5.4 SHOULD — whiteboard

- **`whiteboard/WhiteboardCanvas.svelte`** (~9 hits) — exit whiteboard title + aria + button text, zoom out/in/reset titles + aria + text, fit-to-content title + aria — `whiteboard.*` — SHOULD
- **`whiteboard/ColorPicker.svelte`** (~6 hits) — eyedropper title + text + unsupported note, pin title + text, `>Done<`, `>Recent<` — `whiteboard.color.*` — SHOULD
- **`whiteboard/WhiteboardToolPanel.svelte`** (**~100 hits**, grouped) — aria-labels (`Save whiteboard`, `Pick canvas background color`, `Select primary color slot`, `Swap primary and secondary colors`, `Open custom color picker`, `Brush size`, `Opacity`, `Add child node`, `Delete node`, `Add layer`, `Move layer up/down`, `Layer opacity`, `Undo`), titles (`Import a Photoshop .abr brush pack`, `Swap colors (X)`, `Bold`, `Italic`, `Delete (Del / Backspace)`, `Move selected item to another layer`, `Undo (Ctrl/⌘+Z)`, `Redo (Ctrl/⌘+Shift+Z)`, `Clear all strokes`), text (`Filled`, `Brush size`, `Opacity`, `Size`, `Delete`, `Post`), `:1513` placeholder `"Add a comment…"` — `whiteboard_tool.*` — SHOULD (single fix pattern)
- **`whiteboard/BrushCanvas.svelte`** (~2 hits) — rotate title + aria — SHOULD

### §5.5 SHOULD — session state file error strings

Each viewer session (`audiobook`, `doc`, `ebook`, `archive`, `3d`,
`whiteboard`, `sprite`) has a `loadError` string set from thrown
Errors or fetch failures. The render sites (viewer views) already
have their own English wrappers (`"Couldn't load"`, `"Couldn't
render"`). Recommendation: a shared `viewer.err_load` key +
interpolation of the low-level detail.

Explicit hits:

- `sprite/session.svelte.ts:360` `'Companion JSON is not an object.'` — `sprite.err_companion_not_object` — SHOULD
- `sprite/session.svelte.ts:421` `'Companion JSON had no valid frame rects.'` — `sprite.err_companion_no_frames` — SHOULD

### §5.6 NICE — grouped

- `AssetCard.svelte:207,212` badge labels `>video<` / `>3D<` — `asset_type.*` — SHOULD
- `ExtractionConfigPicker.svelte:115,127` `>Source<` / `>Mode<` — `extraction.*_label` — SHOULD
- `FieldValueInput.svelte:208` placeholder `"UUID"` — NICE
- All viewer `Tips.svelte` (10 files) — NICE (accordion-collapsed by default)
- `AdminBackfillPanel.svelte` (~14 hits) — defaults (`startLabel = 'Start'`, `emptyText = 'No runs yet.'`), status labels (`'cancelled'`, `'failed'`, `'done'`, `'running'`), relTime (`'just now'`, `` `${m}m ago` ``), section heading, refresh button, table column labels, `>Cancel<` — SHOULD

### §5.7 Cross-file grouped repetitive findings

Extract shared keys to drop many DOM leaks at once:

- **`Download original`** (7 hits) — FontView, ImageView, PDFView, DocView, PlaceholderView, ModelView, SpriteCanvas, DetailsTool/Body — single `common.download_original`
- **`Close`** aria/title — CollectionModal, UploadModal, PostHost — `common.close`
- **`Cancel` / `Save`** — DocView, tool bodies, upload modal — `common.cancel` / `common.save`
- **Relative-time suffixes** — CommentsThread + AdminBackfillPanel both hand-roll English relative time; extract to a shared helper backed by `common.time.*`
- **`Failed to …` fallbacks** — CommentsThread (3 sites), `upload.svelte.ts` (many), `postSource.svelte.ts` — central `common.err_*` keys with a shared "server message wins, key is fallback" pattern (this pattern is the same one that Q7 identifies as the backend-error blocker)

## §6 Test coverage findings — Playwright locale switching

### Enumeration

- 39 frontend test files total (14 vitest unit + 25 Playwright specs).
- Playwright config: `scripts/dogfood/ui/playwright.config.ts` — two
  projects (`standalone` + `federation`), both drive Studio A at
  `http://localhost:5173`. No `web/tests/`, `web/e2e/`, or
  `web/playwright/` directory.
- Backend Go i18n tests: `app/internal/i18n/handler.go` has no
  companion test file. Grep hits in `handler_test.go` files are
  incidental.

### Tests that reference any locale/language string

- **`scripts/dogfood/ui/tests/standalone/ui-09-account-pages.spec.ts:28-32`**
  — test "preferences page renders theme + language pickers" asserts
  `main` contains `/Language/i` — i.e. verifies the English heading is
  present on `/account/preferences`. Does not open the picker, does
  not click a language button, does not set a cookie, does not read
  back a translated string.
- **`ui-03`, `ui-05`, `ui-12`, `ui-14`, `ui-16`, `ui-18`** — mention
  `i18n` in **comments only** (e.g., `// Heading from the new page
  (i18n: account.requests.title)`) with no runtime coverage.

### Tests that switch locale AND assert on translated text

**Zero.** Not one Playwright spec:

- Sets the `aa_lang` cookie
- Calls `lang.set('es')` (or any locale mutator) via `page.evaluate`
- Selects `'es'` or `'fr'` from the picker
- Sends an `Accept-Language` header
- Asserts on any Spanish endonym (`Español`), French endonym
  (`Français`), Spanish word (`Idioma`, `Buscar`, `Cuenta`), or French
  word (`Langue`)

### Vitest partial credit (not a substitute)

- `web/src/lib/stores/lang.test.ts:23-28` — asserts unknown-key
  returns the key (tier 3 fallback).
- `web/src/lib/stores/lang.test.ts:30-42` — with
  `lang.resolved = 'es'`, asserts English fallback via
  `t('user_menu.signed_in_as', { username: 'alice' })`. Proves
  tier 2 (silent English fallback), does not prove Spanish renders.
- `web/src/lib/i18n/i18n-coverage.test.ts` — static-scan test (see §2).
  Does not exercise locale switch.

### Assessment

**Playwright locale coverage is a MUST-tier gap for 1.55.V-2.** No
existing spec proves:

- The picker actually flips the visible catalogue.
- Any Spanish or French translation renders anywhere.
- The `aa_lang` cookie persists across a reload.
- An i18n regression is caught at the visible-DOM level.

Minimum for 1.55.V-2 to close the gap: one spec that
(a) navigates to `/account/preferences`,
(b) clicks the `es` button,
(c) reloads or navigates,
(d) asserts `UserMenu` shows `Español` (line 132 of `UserMenu.svelte`)
    or the navbar shows `Subir` (from `es.json:3`).
A parallel `fr` case is trivial once the fixture exists.

## §7 Backend user-facing string findings

### Category 1 — `http.Error(w, <raw JSON English>, status)`

**35 hits**, all in low-level file / archive handlers:

- `app/internal/http/handlers/archive_bundle.go:61,66,72,81,94,99`
- `app/internal/http/handlers/hls.go:56,62,76,82,101`
- `app/internal/http/handlers/asset_file.go:53,58,63,82,131,144`
- `app/internal/http/handlers/archive_entry.go:76,81,86,92,98,107,120,125,136,140,146,150,158,162,173,185,191,197`

Each writes hand-built JSON like:

```
http.Error(w, `{"error":"archive too large to bundle — download original"}`, http.StatusRequestEntityTooLarge)
```

Recommendation: **move to error codes**; the frontend maps codes to
translated strings.
Priority: MUST for user-facing (archive UI, HLS video playback, asset
file download all render these). SHOULD for the two dev-facing hits
(`static_spa.go:56` `"frontend bundle missing"` — dev/misconfig;
`middleware/recover.go:25` `"internal server error"` — panic path).

### Category 2 — `openapi.*JSONResponse{Error: "..."}`

**699 total hits across the codebase; 214 unique English strings.**
Sample of the first ~30 unique:

```
"a peer with this instance_url already exists"
"account has expired"
"account has no native password to change"
"account is not approved"
"acl entry not found"
"admin capability required"
"alternate not found"
"an asset with this file already exists in your library"
"annotation not found"
"asset has no source file"
"asset not found"
"asset was edited by someone else after your last load; reload and try again"
"authentication required"
"cannot DM yourself"
"cannot block yourself"
"cannot delete this comment"
"cannot edit another user's profile"
"cannot follow yourself"
"cannot impersonate while already impersonating; end the current impersonation first"
"code did not verify; check your authenticator and try again"
"collection was edited by someone else after your last load; reload and try again"
"current password is incorrect"
"instance base URL not configured (set System → Site → Base URL)"
"invalid credentials"
"license validation failed"
"missing capability for this field: <name>"
"new password cannot match the current password"
```

**All user-facing.** Two properties confirm:

1. OpenAPI Error schema is `{ error: string, description: "Human-readable error summary" }` — no `code` field. See
   `app/api/openapi.yaml:8360-8367`.
2. Frontend renders `apiErr.error` directly. Confirmed sites:
   - `CollectionPicker.svelte:92,168`
   - `CommentsThread.svelte:79,155,210`
   - `ShareCollectionModal.svelte:60,83,105`
   - `NewCollectionModal.svelte:55`, `EditCollectionModal.svelte:75`
   - `ImpersonationBanner.svelte:30`
   - `SimilarAssetsPanel.svelte:58`
   - `upload/UploadFileRow.svelte:254-255` — `{#if row.error}<span class="text-danger">· {row.error}</span>`
   - `upload/ThumbnailPicker.svelte:92`, `PostHost.svelte:229,489`

The idiom `apiErr.error ?? t('…')` means **any backend English `error`
field short-circuits the client's `t()` fallback**. Every one of the
214 unique strings above renders verbatim to a Spanish or French
user, even after §4+§5 fixes.

Recommendation: **structural change**. Move backend errors to a
code + optional args shape:
`{ code: "asset.not_found", args: {...}, error: "asset not found" }`
(keep `error` for developer/log tooling but stop rendering it on the
client). Add an `errors.<code>` namespace to `en.json` +
`es.json` + `fr.json`. Frontend renders `t('errors.' + apiErr.code,
apiErr.args)` and falls through to `apiErr.error` only if the code
is missing (compatibility net during rollout).

Priority: **MUST-scale** by count, but this is a v1.0.0 arc — no way
to remediate 214 unique strings + backend refactor + frontend mapper
inside 1.55.V-2's scope. Recommendation for 1.55.V-2: **audit-only**
in the fix arc; file a separate issue for the backend-error-code
refactor as a v1.0.0 prerequisite (parallel to #242).

### Category 3 — `w.Write([]byte(...))` / `writeJSON` / `render.JSON`

**Zero** user-facing English hits outside the 35 `http.Error` calls.
All JSON writing in modern handlers goes through the
`openapi.*JSONResponse` types.

### Category 4 — `openapi.yaml` descriptions

`description:` blocks are for docs/tooling (Swagger UI,
`oapi-codegen` godoc), not runtime rendering. Developer-facing;
English is fine. **Keep as-is.**

### Tally

| Category | Count | Audience | Verdict |
|---|---|---|---|
| `http.Error` raw JSON English | 35 | user-facing | should be codes |
| `openapi.*JSONResponse{Error: "..."}` | 699 (214 unique) | user-facing | should be codes |
| `w.Write` / `writeJSON` English | 0 | — | — |
| `http.Error` dev/log adjacent | 2 | dev-facing | keep as-is |
| OpenAPI `description:` blocks | many | dev-facing | keep as-is |

### Overall pattern

**The backend returns raw English exclusively.** Zero error `code`
fields anywhere in `app/internal/` — grep for `code:\s*"[a-z_]+"`
returned 0. Even a fully-translated `es.json`/`fr.json` on the
frontend surfaces English on every 4xx/5xx path until the backend
switches to codes and the frontend gets a code-to-key mapper.

## §8 Fallback behavior notes

### Implementation

`web/src/lib/stores/lang.svelte.ts:123-133`:

```ts
t = (key, vars?) => {
  const code = this.resolved;
  const hit = FLAT[code]?.[key];
  if (hit !== undefined) return interpolate(hit, vars);
  const en = FLAT[DEFAULT_LOCALE]?.[key];
  if (en !== undefined) return interpolate(en, vars);
  return key;
};
```

### Behavior

Three-tier resolution:

1. Active locale (`FLAT[code]`) — if key exists, interpolate + return.
2. English (`FLAT['en']`) — silent fallback. Interpolate + return.
3. Raw key string (e.g. `nav.upload`).

Contract pinned by `lang.test.ts:23-28` (tier 3) and
`lang.test.ts:30-42` (tier 2).

### Assessment

The behavior is a **hybrid**: silent English fallback for keys
present in `en.json` (best UX in prod), loud key-echo only when keys
are missing in both the active locale AND English. Sounds ideal but
has a critical failure mode for this milestone.

**It masks the ~3% coverage reality.** Every unresolved
Spanish/French key resolves to English via tier 2 — silently. To a
Spanish user, the UI is ~97% English with no visible signal that
anything is missing. The `(5%)` completion badge in
`UserMenu.svelte:153` and `preferences/+page.svelte:188` is the only
tell (and is itself stale — actual coverage is 3.2% / 2.3%).

The loud tier fires only when a component calls `t('some.new.key')`
that a dev forgot to add to `en.json` too — surfaces during dev.
Hardcoded English strings in components (the bug the coverage guard
is trying to catch) don't go through `t()` at all, so the fallback
chain never sees them — they render as literal English regardless
of locale.

**Verdict**: fallback is the right choice for a release with
incomplete catalogues — users get *something readable* not
`[missing.key]` in prod. But there's **no runtime observability of
untranslated coverage**. Any 1.55.V-2 remediation should pair the
Playwright locale-switch coverage (Q6) with either:

- A dev-mode flag that makes tier 2 loud (prefix hits with a marker
  like `⟨en⟩ Upload`) so QA sees coverage holes in-browser, or
- Instrumentation that logs missed-locale-hit keys to the dev console
  when `import.meta.env.DEV` — turns the silent fallback into an
  audit trail.

Neither exists today; the fallback path in `lang.svelte.ts:130` is
completely opaque.

## §9 Fix recommendations (grouped by priority)

### MUST — before v0.1.0 tag (target: 1.55.V-2)

1. **Setup wizard** (`routes/setup/+page.svelte`) — port all ~30
   strings under `setup.*` namespace. First surface a fresh install
   renders.
2. **Home + post detail + collection detail titles/empty states** —
   `routes/+page.svelte`, `routes/posts/[id]/+page.svelte`,
   `routes/collections/[id]/+page.svelte`.
3. **Search + advanced search** (`routes/search/+page.svelte`,
   `routes/search/advanced/+page.svelte`) — ~45 hits across both;
   primary discovery surface.
4. **Foundational chrome** —
   `SearchBar.svelte` (nav-cluster, present on every page),
   `NavUploadButton.svelte`, `UserMenu.svelte`,
   `MessagesButton.svelte`, `CollectionModal.svelte`. ~15 hits.
5. **Upload flow** — `UploadModal.svelte`, `PostComposeForm.svelte`,
   `UploadDropZone.svelte`, `UploadFileRow.svelte`,
   `ThumbnailPicker.svelte`, plus `stores/upload.svelte.ts` state
   error strings. ~70 hits + 10 state-file errors.
6. **Comments + post viewer** — `CommentsThread.svelte`,
   `PostHost.svelte`, `AssetPlaylist.svelte`. ~40 hits.
7. **Federation share warning** — `RestrictedShareBanner.svelte`.
   7 hits.
8. **State-file error strings** — `postSource.svelte.ts`,
   `auth.svelte.ts`. 4 hits.
9. **Playwright locale-switch spec** — one new spec per §6.

**Estimated total MUST hits: ~230 strings + 1 new test spec.**
Feasible for 1.55.V-2 as a 2-3 day arc if extract-shared-keys is done
first (§5.7 grouped fixes drop ~50 hits at once via `common.*`).

### SHOULD — batch pass, ideally in 1.55.V-2 or a follow-up sprint

1. **Viewer views** — AssetViewer transport rail, viewer views
   (Doc/Audiobook/Epub/PDF/Font/Image/Media/Model/Placeholder +
   Sprite). ~80 hits under `viewer.*` / `<kind>.*`.
2. **Viewer tool bodies** — 5 `Body.svelte` files. ~110 hits under
   `<tool>_tool.*`.
3. **Whiteboard tool panel + canvas + color picker** —
   `WhiteboardToolPanel.svelte` (~100 hits), `WhiteboardCanvas.svelte`,
   `ColorPicker.svelte`. Single fix pattern.
4. **Account sub-pages** — `account/security/+page.svelte`,
   `account/saved-searches/+page.svelte`,
   `account/tokens/+page.svelte`, `account/messages/[peer]/+page.svelte`.
   ~40 hits.
5. **AdminBackfillPanel** — defaults, status labels, relative time,
   headings. ~14 hits.

**Estimated total SHOULD hits: ~350 strings.** Batch into 1.55.V-3
or fold into 1.55.V-2 if the arc grows.

### NICE — deferrable to a v1.0.0 ops-translation phase

1. **Admin operational pages** — 13 admin surfaces
   (`admin/search/*`, `admin/federation/{inbox,outbox,key-health}`,
   `admin/saved-searches/*`, `admin/metadata-extraction/*`). ~150 hits
   under `admin.<page>.*` namespaces.
2. **Viewer Tips accordions** — 10 files, hotkey rows only visible
   when accordion open. ~50 hits.
3. **AssetCard badges** (`video`, `3D`).
4. **`ExtractionConfigPicker` labels**.

### Backend user-facing error strings — v1.0.0 prerequisite (separate arc)

**File a v1.0.0 prerequisite issue** parallel to #242. Scope:

- Extend the OpenAPI Error schema with an optional `code` field.
- Add an `errors.<code>` namespace to `en.json` + `es.json` + `fr.json`.
- Refactor 214 unique error strings in `app/internal/**` to emit
  codes.
- Update every `apiErr.error ?? t('…')` frontend site to prefer
  `t('errors.' + apiErr.code, apiErr.args)` with `apiErr.error` as
  compatibility fallback during rollout.
- Priority: hard v1.0.0 blocker per §7 — no path to a truly-localized
  UI without this.

## §10 Coverage-guard extension plan

The current guard (§2) covers ~14% of `.svelte` files with a
3-regex heuristic and 3-tier attribute allow-list. 1.55.V-2 executes
this plan; the audit only sketches.

### Immediate extensions (1.55.V-2)

1. **Expand `TRACKED_FILES` to all files touched in §4+§5 MUST +
   SHOULD tiers.** Explicit allow-list; drop the "14% by design"
   posture now that we have a fix-arc backlog to burn down.
2. **Add attribute coverage**:
   - `label="..."` on `<option>` and custom form components
   - `aria-labelledby="..."` targets (partial — requires cross-file
     awareness; can start with same-file only)
   - `aria-description="..."`, `aria-details="..."`
   - `value="..."` on `<option>` where value is the display text
     (heuristic: value is CamelCase or spaces)
   - `data-tooltip="..."` if the codebase ever adopts a tooltip lib
     with data-attr convention (defer if unused today)
3. **Add template-literal / concat attr coverage**:
   - `title={\`… ${…}` and `title={t(…) + " …"}` patterns
   - Backtick templates with `${}` interpolation
   - String concat with `+`
4. **Extend the ALLOW set** — add HTML entity escapes (`&mdash;`,
   `&nbsp;`, `&amp;` variants) so they don't trip
   `isLikelyEnglish`.
5. **Add multi-line text-node coverage** — swap the regex negation
   `[^<>{}\n]+?` for a multi-line-aware form; test with the current
   fixture.
6. **Add a `.svelte.ts` state-file scanner** — grep for `error =
   $state('English text')` and `throw new Error('English text')`
   where the string reaches the DOM. Track separately from the
   `.svelte` scanner (different assertion target).

### Structural extensions (1.55.V-2 or follow-up)

7. **Add a locale-parity check** — assert every key in `en.json`
   exists in `es.json` and `fr.json`. Would fail today loudly (only
   ~3% coverage). Two options:
   - Fail the CI on parity gaps (accurate but blocks all PRs until
     catalogues fill).
   - Warn only (dev sees the gap, PR ships anyway).
   - **Recommended**: warn-only with a numeric budget — parity gap
     baseline captured at 1.55.V-2 merge; every PR must not grow it.
     Same pattern as size-regression budgets.
8. **Add a "keys never used" check** — assert every key in `en.json`
   is actually referenced by at least one `t(...)` call in `.svelte`
   or `.svelte.ts`. Catches drift when a component is deleted but its
   dict keys linger. Complements guard #7.
9. **Wire into `.github/workflows/ci.yml` as a distinct job** — today
   the guard rides on the general `vitest (frontend unit tests)`
   step. A distinct `i18n-coverage` job makes the failure line-item
   obvious in the PR checklist + can carry a separate warn-only
   posture for the parity check.

### Dev-mode observability (from §8 verdict)

10. **Instrument `lang.svelte.ts:t()` with a dev-only console log** for
    tier-2 hits (silent English fallback). Guarded by
    `import.meta.env.DEV` so prod stays quiet. Optional loud-mode
    flag (`localStorage.setItem('aa.i18n.loud', '1')`) that prefixes
    tier-2 hits with `⟨en⟩` in the DOM — QA sees coverage holes
    at a glance.

### Frontend-facing followups (defer to 1.55.V-2 or later)

11. **Backend `/i18n/locales` endpoint** — either wire the frontend to
    consume it (and drop `SUPPORTED_LOCALES`) or delete the endpoint
    + `handler.go`. Current state (endpoint exists, unused, stale
    completionPct) is dead weight.
12. **`completionPct` accuracy** — recompute at build time from real
    key counts (`en.json` denominator, `<locale>.json` numerator)
    rather than hard-coded `5`. Ship as part of guard #7's numeric
    budget mechanism.

---

## Cross-cutting observations

1. **The guard is a punch-list, not a coverage check.** It asserts
   zero raw English on 24 chosen surfaces; it says nothing about
   es/fr completeness or the 145 unguarded `.svelte` files.
2. **The server endpoint `/i18n/locales` is unwired on the frontend.**
   Registry ships bundled; endpoint's `completionPct` is a stale
   hint (5%) that neither matches the true numbers (3.2% / 2.3%) nor
   is consumed anywhere. Either drop it or make the frontend consult
   it and drop `SUPPORTED_LOCALES`.
3. **All en catalogue mutations are silent to es/fr.** No script
   diffs the three files or writes a "missing keys" report; the guard
   doesn't check that a key exists in every locale. A key added to en
   on Monday appears in Spanish UIs as the English fallback string
   forever without warning.
4. **`admin.user_detail.lockout_*` (added in #198) has zero es/fr
   coverage** — the 1.19.D fire proved the guard works on the source
   side, but the newly-added keys never propagated. Same mechanism
   that leaves `es.json` and `fr.json` frozen at their initial stub
   size.
5. **`.svelte.ts` state files never import `t`.** Safe today because
   no state file constructs user-visible strings via `t()` — all
   raw-English error strings go through the render layer's `t()` call.
   But the pattern needs to be worked out for any future headless
   state that needs to render (e.g., toast copy from a background
   task).
6. **Cookie set is not surfaced on server responses.** `writeCookie`
   runs entirely client-side. An SSR pass (should SvelteKit adopt
   one) would ignore the cookie because nothing reads it server-side.

## §11 MUST-tier disposition (Phase 1.55.V-2)

Phase 1.55.V-2 (PR against #248) executed the MUST tier from §9. This
section tracks every MUST finding to disposition. es/fr were NOT
translated — per owner decision (#247), new keys are en-only and es/fr
fall through to English via the existing fallback (§8). That is the
deliberate pre-#247 state.

### Summary

- **`common.*` extraction**: expanded from 16 → 40 keys (added close,
  search, submit, next, previous, yes, no, clear, clear_all, go,
  load_more, download_original, untitled, saving, posting, confirm,
  pause, resume, optional_note_placeholder, failed_to_load, and the
  relative-time `dur_minutes/hours/days/months`). Shared labels across
  the MUST findings now point at one key each.
- **Scoped keys added**: 211 new leaf keys across `browse.*`,
  `post.detail.*`, `setup.*`, `search.*` (incl `search.advanced.*`,
  `search.save_collection.*`, `search.save_search.*`), `upload.*`
  (modal/compose/dropzone/file_row/thumbnail + err_*), `comments.*`,
  `post_host.*`, `whiteboard.*`, `post_menu.*`, `post_badges.*`,
  `user_meta.*`, `viewer_playlist.*`, `federation.*`, `collections.*`,
  `messages.*`, `nav.*`, `playlist.*`, `auth.*`. en.json grew from
  1 608 → 1 843 leaf keys (+235 = 24 common + 211 scoped).
- **Collapse ratio**: the ~166 raw MUST strings the audit enumerated
  (plus extras the fix caught in the same files) resolved to 235 total
  new keys of which 24 are shared `common.*` reused across many sites
  — so the effective unique-label surface is well below the raw hit
  count wherever a label repeats (Save/Cancel/Close/Loading/etc.).
- **Files touched**: 23 source files (20 `.svelte` routes+components +
  3 `.svelte.ts` state files). es.json / fr.json untouched.

### Route findings (§4) — disposition

| Surface | MUST findings | Disposition |
|---|---|---|
| `routes/+page.svelte` (home) | 7 | FIXED — `browse.*` + `common.failed_to_load` |
| `routes/posts/[id]/+page.svelte` | 1 | FIXED — `post.detail.title` |
| `routes/setup/+page.svelte` | ~30 | FIXED — full `setup.*` namespace (27 strings) |
| `routes/search/+page.svelte` | ~30 | FIXED — `search.*` + modals + `common.*` |
| `routes/search/advanced/+page.svelte` | ~15 | FIXED — `search.advanced.*` |
| `routes/collections/+page.svelte` | 1 | FIXED — `collections.include_deleted` |
| `routes/collections/[id]/+page.svelte` | 3 | FIXED — `collections.*` |

### Component findings (§5) — disposition

| Surface | MUST findings | Disposition |
|---|---|---|
| `CollectionModal.svelte` | 1 | FIXED — `common.close` |
| `SearchBar.svelte` | 6 | FIXED — `search.*` + `common.search` + `nav.search_placeholder` |
| `NavUploadButton.svelte` | 2 | FIXED — `nav.upload` + `nav.upload_button_title` |
| `UserMenu.svelte` | 1 | FIXED — `nav.open_user_menu` |
| `MessagesButton.svelte` | 1 | FIXED — `messages.you_prefix` |
| `CommentsThread.svelte` | ~15 | FIXED — `comments.*` + `common.dur_*` |
| `PostHost.svelte` | ~15 | FIXED (MUST lines) — `post_host.*` / `whiteboard.*` / `post_menu.*` / `post_badges.*` / `user_meta.*`. SHOULD/NICE strings in this file remain (deferred). |
| `AssetPlaylist.svelte` | 6 | FIXED (MUST lines) — `viewer_playlist.*`. File still carries SHOULD viewer-hotkey strings → NOT added to the blocking guard list. |
| `federation/RestrictedShareBanner.svelte` | 7 | FIXED — `federation.*` |
| `upload/UploadModal.svelte` | ~10 | FIXED — `upload.modal.*` + `common.*` |
| `upload/PostComposeForm.svelte` | ~18 | FIXED — `upload.compose.*` |
| `upload/UploadDropZone.svelte` | 2 | FIXED — `upload.dropzone.*` |
| `upload/UploadFileRow.svelte` | ~20 | FIXED — `upload.file_row.*` + `common.*` |
| `upload/ThumbnailPicker.svelte` | ~13 | FIXED — `upload.thumbnail.*` |
| state: `playlist/postSource.svelte.ts` | 3 | FIXED — `playlist.err_load_post` + `common.*` |
| state: `stores/upload.svelte.ts` | 10 | FIXED — `upload.err_*` (10 sites → 9 keys) |
| state: `stores/auth.svelte.ts` | 1 | FIXED — `auth.err_invalid_credentials` |

### Test coverage (§6) — disposition

FIXED. New spec `scripts/dogfood/ui/tests/standalone/ui-30-i18n-locale-switch.spec.ts`
proves the locale-switch mechanism end-to-end: navbar search placeholder
flips en ("Search assets…") → es ("Buscar recursos…") on switching to
Español via `/account/preferences`, and persists across reload via the
`aa_lang` cookie. Asserts on an existing-Spanish key
(`nav.search_placeholder`) because most new V-2 keys are en-only — the
spec proves the switch, not es/fr coverage.

### Guard extension (§10) — disposition

FIXED (subset executed this arc):

- `TRACKED_FILES` 24 → 44 (all now-clean MUST files; `AssetPlaylist`
  held back for its deferred viewer-hotkey SHOULD strings).
- Attribute coverage: added `label=` + `aria-description=`.
  (`aria-labelledby` intentionally excluded — it's an id ref.)
- Strip `<!-- comments -->` + `<code>…</code>` + `<style>` before
  scanning (removes two false positives: a `<section> not <main>`
  rationale comment, a `textures/wood.png` example path).
- Warn-only locale-parity check reporting es 3% / fr 2% coverage
  without failing CI; the orphan-key half (locale keys absent from en)
  stays blocking as a schema-drift guard.
- `locales.ts` `completionPct` now computed from the bundled
  catalogues (was a stale hardcoded `5`).

Deferred guard items (to a follow-up): template-literal-with-`${}`
attribute coverage, `.svelte.ts` state-file scanning, dev-mode
missing-key observability instrumentation, wiring the guard into a
distinct CI job.

### Backend strings (§7) — NOT this arc

The 214 unique English error strings returned raw as
`openapi.*JSONResponse{Error: "..."}` are a backend refactor + frontend
mapper — out of scope. Tracked as #246 (v1.0.0 prerequisite).

### Deferred to follow-up

SHOULD (~178) + NICE (~270) tier strings — viewer views + tool bodies +
whiteboard tool panel + account sub-pages + admin operational pages.
Filed as a follow-up issue at handoff. The extended guard is scoped so
these do NOT fail CI (the deferred files are not on the blocking
`TRACKED_FILES` list).
