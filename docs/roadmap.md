# Roadmap

A snapshot of what's shipped, what's in flight, and what's on the
map. Subject to change as we learn what teams actually need.

## Foundation status

**Pre-v1.0 foundation is essentially complete.** Fifty-eight accepted
ADRs cover the load-bearing concerns: storage, caching, frontend
stack, federation protocol (walled-garden + encrypted, ArchivePub
v1.0-rc1), capability add-ons, audit log, observability, packaging,
migration baseline policy. The encryption arc (Phase 1.22.I) closed
the federation foundation. The pre-MVP cleanup (Phase 1.49) closed
the technical debt. The Phase 1.17 identity arc is the last
foundational arc in flight; the remaining sub-phases (1.17.B
through 1.17.F) are mechanical execution of decisions captured in
[ADR 0010](/adr/0010-permissions-teams-workflow/) (now `accepted`).

The transition marker — the **v0.1.0 release tag**, the first-ever
tag — shipped 2026-07-11, followed by the v0.1.1 patch on 2026-07-13;
the second milestone (v1.0.0 = out of beta) sits further out.
Per [ADR 0046](/adr/0046-migration-baseline-and-squash-policy/)
squash point #1 executed at the tag (single baseline migration, ADR
0057); the append-only-forever trigger (v0.1.0 vs v1.0.0) remains
under review — see issue #228; details in
[docs/v0_1_readiness.md §0](./v0_1_readiness.md).

## Shipped

The current release stream covers the foundations:

- **v0.1.0 — first public tag (2026-07-11).** The first-ever tagged
  release: AGPL-3.0-only + dual commercial licensing executed
  (1.55.AA), org move to Artist-Alley-Org, site split (docs site
  moved to the private artist-alley-site repo), migration fold to a
  single baseline migration (ADR 0057), Docker-only distribution
  (GHCR + Docker Hub, Sigstore-signed). Pre-1.0 still means schemas
  can break across minors.
- **v0.1.1 — first patch release (2026-07-13).** Worker-pool claim
  fix (#279) restoring media processing, GHCR owner-casing fix
  (#280), dependency cleanup (#281 / #283 / #284) clearing all open
  Dependabot alerts.
- **Single binary deploy.** Go server with the SvelteKit SPA
  embedded via `go:embed`. Multi-arch Docker images (amd64 + arm64),
  every image Sigstore-signed. Docker is the sole distribution
  channel until v1.0.0 — native packages (`.deb` / `.rpm`, static
  binaries, Homebrew) are a v1.0.0 target.
- **Postgres + storage.** Goose migrations run on boot. Storage
  backend is filesystem by default, or any S3-compatible bucket
  (AWS, R2, B2, MinIO).
- **Identity & auth.** Sessions, API tokens with scopes, roles &
  capabilities, password policy + SSO provider config UI (real
  enforcement of LDAP / SAML / OAuth lands with phase 1.18).
- **Upload pipeline.** Drag-anywhere → modal queue → background
  uploads with progress, per-row tags + per-asset metadata, three
  post-composition modes (one post / one-per-file / no-post),
  separate-cover-thumbnail support.
- **Posts + assets + collections.** Posts wrap one or many assets;
  collections wrap posts; tags + full-text search across all of it.
- **Admin-extensible metadata** (Phase 1.9). Operators define their
  own custom fields per asset_type (Phase 1.9.A — shipped with the
  baseline) and per collection (Phase 1.9.B — shipped 2026-06-20 via
  PR #144). Typed field vocabulary (text / longtext / rich_text /
  number / boolean / date / datetime / select / multi_select / tree /
  reference), per-field read/write capability gates, audit history,
  federation-ready provenance. ADR 0012. The subject-kind
  discriminator means future "things with metadata" (posts, users)
  reuse the same field_definition pipeline.
- **Browse feed.** Grid / masonry / thumbnail / list views with
  the sortable spreadsheet for list mode (toggleable columns,
  per-row thumbnails). Floating footer with view + feed filter
  (team / trending / latest / following) + sort direction.
- **Post detail modal.** Two-pane shell, multi-asset vertical
  scroller, sidebar with per-asset metadata, comments thread,
  likes, dedicated review canvas with zoom / pan / tile.
- **Admin shell.** 13-section admin menu with dynamic landing
  pages, capability-gated; Scalar-embedded API explorer; real
  config surfaces for site, SMTP, auth providers, AI providers,
  appearance (font slot picker — 14 fonts across 4 slots).
- **Account shell.** Profile, theme + language preferences, API
  tokens management. Other surfaces (security, sessions, drafts,
  trash, activity, stats) stubbed with phase tags.
- **Theme system.** M3-shaped color tokens (3-tier surface ladder,
  semantic success/warning/danger, on-* pairs, focus ring), light
  + dark palettes with intentional cross-mode hue shift, admin-
  configurable font slots (brand / display / sans / mono).
- **i18n.** Per-user language preference, locale catalogue endpoint,
  hand-rolled flat-key dictionary store; English shipped, Spanish +
  French stubs for the picker.
- **Universal asset viewer** (Phase 1.18.B-2 + 1.18.E-6). A single
  Svelte shell hosts per-kind view bodies (image, video, audio, PDF,
  font, 3D) with a shared anchor model — a frame number for video,
  a page for PDF, a camera + time-on-track for 3D — so future
  annotation overlays and presentation rooms wire to the shell once
  and every kind inherits them. Universal pan + zoom (⌘-wheel zoom-
  toward-cursor), click-to-pause, fullscreen + `F`, jump-to-frame
  (`G`), and per-kind transport bars all sit on the shell.
- **Format coverage** (Phase 1.18.A / 1.18.B-1 / 1.18.B-10–12 /
  1.18.C / 1.18.D / 1.18.E). Images: PNG, JPEG, WebP, plus HDR /
  EXR / Radiance `.pic` via ffmpeg tonemap (1.18.E-4) and a pure-Go
  RGBE decoder (1.18.E-5). Video: HLS adaptive ladder with frame-
  accurate scrubbing. Audio: waveform PNG + click-to-seek scrub.
  PDF: multi-page navigator + raster. Fonts: specimen render. 3D:
  native viewers for glTF / GLB / OBJ / FBX / Marmoset `.mview`,
  Blender-rendered turntable thumbnails for heavy formats (`.blend`,
  others coming under 1.18.B-11), pure-Go importers for legacy game
  formats (MD2 / MD3 / MDL / MS3D — 1.18.C-2 / C-3). Asset companion
  files (textures, `.mtl`, `.bin`) resolved per-asset so 3D loaders
  can pull their sidecars.
- **Federation v1 — walled-garden protocol** (Phase 1.22.D). The
  reference implementation of [**ArchivePub**](/protocol/archivepub/),
  the open federation protocol for DAM-shaped content built on the
  ActivityPub data model. Per-actor HTTP inbox + outbox with HTTP-Sig
  + Ed25519 envelope verify; share-list access control; outbox
  dispatcher with LISTEN/NOTIFY; HTTP/2 delivery worker with batched
  per-peer POST + signing + exponential backoff; recipient resolver
  against `federation_shares`; admin queue UI with re-queue +
  cascade-cancel + audit. Sub-1s p99 end-to-end against production
  defaults. First federated DAM, open-source or commercial. See
  ADR 0043. **Encrypted federation arc 1.22.I-a through 1.22.I-i
  COMPLETE + dogfood-validated end-to-end 2026-06-15** via
  ui-nightly 27558910639: all 8 conformance vectors (scenarios
  01, 05, 06, 07, 08, 09, 11, 12) PASS in 34.8s wall.
  Shipment trail: I-a dogfood infra (#109 + #110), I-b keypair
  (#111), I-c key distribution (#112), I-d capability
  negotiation (#113), I-e outbox encryption (#114), I-f inbox
  decryption (#115), I-g sender refusal flip (#116), I-h
  rotation lifecycle + admin UI (#126 + #127 + #129), I-i
  receiver-gate activation + scenario 05 + spec v1.0-rc1 (#128 +
  #130). Plus eight follow-up PRs (#117–#125) closing real
  production-class bugs surfaced by the dogfood loop — every
  gap caught by the loop, none by unit tests alone.
  **ArchivePub spec at v1.0-rc1** with Appendix A conformance
  test vectors locked; the 7-day soak window closed 2026-06-22
  clean; the spec remains v1.0-rc1 with the v1.0-final stamp
  pending.
- **v0.1.0 release readiness** (Phase 1.55). The meta-arc getting
  from current-dev to the v0.1.0 tag — the first-ever tagged release
  (see [docs/v0_1_readiness.md §0](./v0_1_readiness.md) for the
  milestone model that separates v0.1.0 = first tag from v1.0.0 =
  out of beta). 1.55.A shipped 2026-07-07 via PR #220 (squash
  `9d14fc30`, closes #219): `docs/v0_1_readiness.md` is the master
  audit — 9,907 words / 1,692 lines / 9 sections covering v0.1.0
  exit criteria (7 ADR-anchored), arc-close velocity (25 PRs
  2026-06-22 → 2026-07-07), 22 open gaps with mandatory substructure
  (Status / Roadmap phase / RS blueprint capture / 2024-2026
  gold-standard research citations / Caching strategy / Federation
  implications / Target sketch / Effort / Sequencing), post-v0.1.0
  deferrals, RS reference inventory with delete-safety verdict YES
  on every row, sequencing proposal (base v0.1.0 scope ~9 days;
  full menu ~17-22 days), 7-gate RS deletion readiness checklist,
  post-milestone arc pointers (split v0.1.0 vs v1.0.0). **Unblocks two follow-up arcs:**
  (a) physical deletion of the ~102 MB gitignored `/dbstruct/` +
  `/include/` + `/plugins/` + `/pages/` ResourceSpace reference tree
  (§6 confirms every pattern is captured internally); (b) the
  AGPL + commercial relicense arc per ADR 0016 → 0017 direction,
  gated on this audit + Phase 1.24. **Recommended next sub-phase**
  per §7.1: **1.55.B hygiene bundle** (~1.5 days) — bundle #218
  oapi-codegen version pin + #214 MDX braced-identifier CI gate +
  §4.4 schema-mismatch boot detection + §4.21 baseline migration
  squash verification into one PR. All sub-day, all pure release-
  readiness hygiene.
  **1.55.B shipped 2026-07-08** — release-readiness hygiene bundle
  (§7.1 complete). Four sub-day items in one PR: oapi-codegen pinned
  to v2.7.2 in scripts/generate.sh (no more silent @latest drift
  breaking Codegen check); MDX braced-identifier hazard gate at
  scripts/check-mdx-hazards.sh + a workflow that scans changed
  synced-to-Astro docs on every PR (0 hazards on current dev tip;
  no grandfather list needed); db.CheckSchemaFreshness wired
  post-Migrate in app/cmd/aa/main.go — refuses to start on
  SchemaUnappliedMigrations, warns on SchemaUnknownNewerSchema,
  proceeds silently on SchemaOK; scripts/verify-baseline.sh runs
  three checks (baseline present + ADR 0046 referenced; baseline
  applies clean on a fresh pgvector/pgvector:pg16 scratch DB via
  the real db.Migrate + goose.UpContext path; migration filename
  sequence contiguous) and the current run reports "baseline
  verified against 28 append migrations, head=00029, ready for
  tag." Deferred: unified /admin/system/health surface for
  the schema-freshness warning — no such endpoint exists today
  (only per-subsystem shims); boot WARN log is enough pre-v0.1.0
  per pre-release-practices.
  **1.55.R shipped 2026-07-08** — v0.1.0 milestone rename pass. Pure
  docs recalibration after the user clarified two milestones:
  v0.1.0 = first tagged release (RS deleted + base feature set);
  v1.0.0 = out of beta (real usage + soak + stable production).
  `docs/v1_readiness.md` renamed to `docs/v0_1_readiness.md` via
  `git mv`; new §0 Milestone model section added at top of doc;
  every "v1.0.0" that meant "the release we're building toward"
  retagged to "v0.1.0"; §9 split into post-v0.1.0 + post-v1.0.0
  subsections; ADR 0046 grows a pending-review note pointing at
  issue #228 (whether append-only kicks in at v0.1.0 vs v1.0.0);
  issues #228 + #229 filed to track the ADR 0046 + 0016/0017
  semantic decisions separately. Zero substance change; naming
  recalibration only. Closes #227.
  **1.55.S shipped 2026-07-08** — RS sanitization + physical
  reference-tree deletion (§1.2 exit criterion). Three obsolete
  `scripts/rs-*` pullers deleted; RS mentions scrubbed from 9
  code/config files (`userstate.go`, admin About page, Dockerfile,
  `.goreleaser.yaml`, `docker-compose.yml`, `.env.example`,
  `.dockerignore`, `.gitignore` header, `README.md` untouched — zero
  refs there); ADR 0001 flipped to `superseded-by: 0040`; the
  `docs/v0_1_readiness.md` §1.2 exit criterion flipped to ✅ shipped
  + §6 preamble annotated "physically deleted 2026-07-08" + §8
  checklist all seven gates cleared. ADR 0002 skipped for a
  pending-review pointer (already `superseded_by: 0016` from prior
  lifecycle; redundant). Local `/dbstruct/` `/include/` `/plugins/`
  `/pages/` physically removed from disk; `.gitignore` entries
  retained as safety net. `grep -rn -iE "resourcespace|resource[-_]space"`
  in application code + config now returns empty; deliberate
  historical mentions retained in ADR bodies + `cleanup-audit-2026-06.md`
  + this readiness doc's §6 inventory + memory files. Closes #231.
  **1.55.U-1 shipped 2026-07-08** — schema + cache audit report for
  the v0.1.0 baseline re-squash. Report-only; zero code changes.
  Ships `docs/schema_audit_v0_1.md` — a 10-section inventory of the
  29-file migration chain, 78 tables, 30 named cache domains + 4
  dedicated Cache types, FK cascade behaviour, and CHECK-constraint
  density. Findings: **0 MUST / 23 SHOULD / 11 NICE** (deduplicated).
  The SHOULD tier is dominated by 15 unindexed FK columns (partial
  indexes with `WHERE <col> IS NOT NULL` predicates) plus 3 explicit
  `NO ACTION` → `SET NULL` cascade promotions plus 4 cache-invalidator
  verification gaps (asset PATCH → IIIF, collection POST/PATCH →
  owner profile, per-user cache-key exhaustive sweep, ActorOutbox
  federation-key shape). §10 sketches the 1.55.U-2 re-squash plan:
  collapse 29 files into `00001_baseline_v0_1.sql` with SHOULD fixes
  inlined; zero append migrations at v0.1.0 tag time. Q6 nullability
  + defaults sweep clean; Q7 EXPLAIN spot-checks healthy on the two
  representative queries. `pg_stat` verified index churn zero across
  the chain (no index name recreated more than once in any Up
  block). Closes #233.
  **1.55.U-2 shipped 2026-07-09** — gold-standard baseline
  re-squash. Ships `app/internal/db/migrations/00001_baseline_v0_1.sql`
  (5,631 lines): the collapse of the 29-file chain into a single
  file per §10 of the audit doc. **All 23 SHOULD + 11 NICE findings
  dispositioned per docs/schema_audit_v0_1.md §11.** Applied inline:
  15 partial FK-covering indexes + 3 explicit `NO ACTION` → `SET NULL`
  cascade promotions. Cache-invalidator fixes (§7.1–§7.4) shipped
  as distinct commits: targeted IIIF invalidation on asset + collection
  writes (replaces bulk `InvalidateAll` with `InvalidateAsset(id)`/
  `InvalidateCollection(id)`); owner-profile invalidation on collection
  Create/Delete/Restore (mirrors the posts-side pattern); per-user
  cache-key exhaustive audit + ActorOutbox federation-key shape
  audit-verified. `apiServer` grew `pool` + `cacheReg` fields for the
  cross-domain invalidators. ADR 0057 (v0.1.0 baseline schema shape)
  shipped + synced to Astro — documents ownership model, federation
  posture, soft-delete pattern, indexing strategy, cache invalidation
  model, and data seeds. Old 29 migration files deleted;
  `scripts/verify-baseline.sh` updated for the new filename and green
  against a scratch `pgvector/pgvector:pg16` container. Fresh boot on
  the compose stack applies cleanly; goose records `version_id=1`;
  seed data populates correctly (57 capabilities, 13 asset_types,
  10 roles, 43 role_capabilities, 42 system_config, 7 workflow_states,
  11 workflow_transitions, 13 field_definition, 1
  `federation_dispatch_state` singleton — the last-mile fix in
  commit `7a44d86c` that unblocked CI Integration tests). Full
  `./scripts/test.sh` green. `app/schema.sql` unchanged; sqlc
  regen produces byte-identical Go output. pg_dump `--schema-only`
  diff pre-vs-post shows only the 18 intended changes (15 indexes +
  3 cascade promotions) — zero unintended drift. Closes #235.
  **1.55.W shipped 2026-07-09** — reverse-image dropzone at
  `/search/advanced` (closes §4.16). Frontend for the visual-search
  backend that's been feature-complete since #199 + #205 + #206 with no
  UI to drive it. New `ReverseImageDropzone.svelte` mounts above the DSL
  builder: drag+drop / file-picker + preview + submit + states. POSTs
  multipart (`file` field) to the existing `POST /search/by-image` via
  raw `fetch` (the endpoint isn't in openapi). The thin
  `{asset_id, similarity}` response is hydrated top-30 via
  `GET /assets/{id}` and rendered as an `AssetCard` thumbnail grid with
  cosine-similarity badges (Milvus/Weaviate pattern). Results render
  **inline** — the advanced page previously only redirected to `/search`,
  so this is its first inline results surface. Disabled/not-configured
  is attempt-and-handle (no client-readable `search.visual.enabled`
  flag): a 501 `sidecar_not_installed` flips to an explanatory state;
  413 / 429 / 503 / generic each surface a user-facing message. All
  strings i18n-keyed under `search.by_image.*` (en-only per #247) +
  added to the coverage-guard tracked list. **Zero backend / openapi /
  migration changes.** Browser-verified end-to-end (render → select →
  preview → submit → handled response); Playwright spec `ui-31` covers
  render + interaction + handled-state (the standalone CI stack lacks
  the CLIP sidecar, so by-image returns 501/404 → the spec asserts the
  handled path per the audit's Q10). svelte-check 0 errors; i18n guard
  green (49 tests). The reverse-image feature is now end-to-end complete
  (backend 4 PRs + this frontend). Closes #251.
  **1.55.X shipped 2026-07-09** — @-mention notifications wired to
  notify (closes §4.5). New `app/internal/social/mention/` package:
  `ParseMentions` extracts `@username` from post title+description +
  comment bodies (charset `[a-zA-Z0-9_-]{1,32}` matching the
  registration pattern — hyphens are valid usernames) excluding code
  fences / inline code / links (standard chat-app behaviour); `Resolver.ResolveLocal`
  maps usernames → local refs via a 5-min cache (null-cached to shrug
  off `@here`/`@channel`; usernames are API-immutable so no invalidator).
  Both `posts.CreatePost` + `social.CreatePostComment` fire the hook
  **after** the insert commits (best-effort — a notify failure never
  rolls back the content), targeting the containing post so the bell
  deep-links to `/posts/{id}`. Reuses the **pre-existing** `mention_of_me`
  verb (already in `KnownEventTypes`) + `notifications.Writer`, which
  already gates self-mentions (actor==recipient), blocks, and per-verb
  prefs — **so self-mentions are de-duped by the substrate, no resolver
  filter**. Parser returns `Mention{Username, InstanceHost}` — the
  federation seam (`InstanceHost` always empty in v0.1.0; post-Phase-1.30
  `@user@peer.com` is a resolver-side addition via WebFinger, not a
  rewrite). Unknown usernames drop silently; no un-mention audit
  (deliberate). **Frontend needed zero changes** — the bell already
  renders `notifications.verb_mention_of_me` + deep-links post targets,
  so this arc was backend-only. **Zero migrations** (verb + prefs
  pre-existed). 18 tests: parse (code-fence/link/email exclusion,
  hyphenated + case-insensitive usernames, length cap, federation-seam),
  DB-backed resolve (known/unknown/federated/cache-hit), service fire,
  and handler-level end-to-end (comment mention lands a notification row
  with target=post + actor threaded; plain comment fires nothing;
  self-mention doesn't notify self). Closes #253.
  **1.55.AA shipped 2026-07-11** — execute the AGPL + commercial
  relicense (closes #229). Flips the OSS repo from BSD-3 to
  **AGPL-3.0-only** (dual-licensed with a separate commercial license)
  per ADR 0016/0017 — before the v0.1.0 first-public-release, so the
  first public commit ships under the intended long-term license. Root
  `LICENSE` replaced with the verbatim canonical AGPL-3.0 text (diff vs
  gnu.org = only the two "How to Apply" template lines: program name +
  `Copyright (C) 2026 Kenneth Blossom`). New `LICENSING.md` documents the
  dual model — AGPL-3.0-only for open-source use (§13 network copyleft),
  or a paid commercial license for embedding/SaaS without the copyleft,
  contact `licensing@artist-alley.org` (placeholder until the Cloudflare
  portal lands). **SPDX one-liner headers** (`SPDX-License-Identifier:
  AGPL-3.0-only` + `Copyright (C) 2026 Kenneth Blossom`) scripted onto
  **876 non-generated source files** (638 `.go` + 170 `.svelte` + 67
  `.ts` + the goose migration), correct comment syntax per language
  (HTML comment for svelte). **Generated files deliberately NOT stamped**
  — sqlc `*.gen.go`/`models.go`/`db.go`/`queries.sql`, `openapi.gen.go`,
  `panicshim_gen.go`, `schema.d.ts`, and pg_dump `schema.sql` are
  excluded so regen doesn't strip headers → zero codegen drift (verified;
  a first pass wrongly stamped the sqlc-input `queries.sql` and leaked
  the header into generated `.go` — caught + reverted). `web/package.json`
  `license` → `AGPL-3.0-only`; README badge + license section updated to
  the dual model; ADR 0016 gets an "executed" note. Attribution audit
  was already clean (1.55.S: no RS-derived shipped code, single
  copyright holder). `go build` + `svelte-check` + i18n guard green;
  full Go suite green except the pre-existing env freshness test; zero
  behavior change (headers are comments). SPDX identifier + copyright
  line + commercial contact were user-confirmed before commit.
  Closes #261.
  **1.55.Z shipped 2026-07-11** — remove `site/` from the OSS repo +
  rewire OSS CI (site-split OSS-side cleanup). The public docs +
  marketing site now lives + serves from the private repo
  `mscrnt/artist-alley-site` (www.artist-alley.org), pulling this repo's
  docs from `dev` at build — so the OSS `site/` tree (48 files) + its
  build/deploy workflows were dead weight. `git rm -r site/`; deleted
  `docs.yml` (Astro build verification) + `site-github-snapshot.yml`
  (GitHub-stats refresher that wrote into `site/`). **Kept
  `mdx-hazard-check.yml`** — it guards the OSS doc SOURCE (braced-
  identifier scan over `docs/adr/**` / `docs/install/**` /
  `docs/roadmap.md`) that the private site still renders, so an OSS ADR
  can't break the site build. Stripped the now-dead `site/**`
  `paths-ignore` entries from `ci.yml` + `ui-pr.yml` (app CI untouched).
  Added `site-rebuild-trigger.yml`: on push to `dev` touching
  `docs/adr/**` / `docs/roadmap.md` / `docs/install/**` /
  `app/api/openapi.yaml` / `app/schema.sql`, POSTs the Cloudflare Pages
  deploy hook (`SITE_DEPLOY_HOOK` secret) so the private site rebuilds +
  pulls the latest docs — secret-gated (no-op + logged if unset, never
  fails CI), self-hosted runner (zero hosted minutes). Updated README's
  project-structure tree + roadmap link (no more `site/` paths). `docs/`
  source untouched — this removes the renderer, not the source. Clean
  break, no shims. Closes #259.
  **1.55.U-3 shipped 2026-07-10** — fold `00002_digest_queue` into the
  v0.1.0 baseline (squash point #1 per ADR 0046). Inlined the single
  post-baseline append (1.55.Y's `digest_queue` table + partial index +
  FKs + `user_preferences.email_cadence` column) into
  `00001_baseline_v0_1.sql` and deleted `00002` — the migrations dir is
  single-file again, so **v0.1.0 ships with exactly one migration**.
  DDL copied verbatim from `app/schema.sql` (the pg_dump parity target,
  untouched). **pg_dump `--schema-only` parity: folded baseline ≡ (old
  baseline + 00002) — schema-identical, zero drift.** `verify-baseline.sh`
  green (`head=00001`, contiguous, 0 append migrations); sqlc regen
  byte-identical (schema.sql unchanged); digest tests green against the
  folded schema. After this the dir stays single-file until the next
  feature appends `00002`, `00003`… through beta; the v1.0.0 fold is
  squash point #2 (future repeat). Closes #257.
  **1.55.Y shipped 2026-07-10** — email digest preferences + one-click
  unsubscribe (closes §4.9). Per-topic email cadence (immediate /
  hourly / daily / weekly / off) in a new
  `user_preferences.email_cadence` JSONB column (additive; unset =
  immediate so no existing user is surprised). The `notifications.Writer`
  forks at its email-enqueue point: immediate → enqueue now; digest
  cadences → insert a `digest_queue` row; off = "email" absent from
  channels. **In-app notifications always fire independently** of email
  cadence. One hour-ticking `digest.Coordinator` (cadence-gated by the
  clock, mirroring the softdelete gc — cleaner than three job types)
  batches each user's due rows into one `notification_digest` email,
  marks them sent, self-re-enqueues. **RFC 8058 one-click unsubscribe**:
  every notification email (immediate + digest) carries
  `List-Unsubscribe` + `List-Unsubscribe-Post: One-Click` with a
  stateless HMAC-signed token (scramble key); `GET/POST
  /api/v1/unsubscribe` verifies + drops the email channel (per-topic, or
  `__all__` for the digest token) — no login, the token is the auth.
  Three range-clamped sysconfig timing knobs (`digest` section). Prefs
  (incl. cadence) ride the existing 5-min `userprefs.by_user` cache.
  Frontend: per-topic cadence `<select>` on the preferences page +
  i18n keys. **First post-baseline migration** (`00002_digest_queue.sql`)
  — folds back into `00001_baseline_v0_1.sql` at the pre-tag re-squash
  so **v0.1.0 ships zero append migrations** (per the owner directive +
  ADR 0046). ~30 tests (signer round-trip/expiry/tamper, DueCadences,
  coordinator batch/empty/idempotent, Writer fork immediate/digest/off,
  UnsubscribeEmail per-topic + all, cadence validation) all green. Zero
  changes to in-app delivery or the bell. Closes #255.
  **1.55.T shipped 2026-07-09** — v0.1.0 release-pipeline dry-run.
  Fired three `v0.0.99-rc*` tags against `.github/workflows/release.yml`
  end-to-end to verify the signed-release matrix (goreleaser → binaries
  / .deb / .rpm / Homebrew; docker buildx → GHCR + Docker Hub multi-arch;
  Sigstore keyless cosign signing) BEFORE cutting v0.1.0. Three real
  defects caught:
  (1) **rc1**: `npm ci` on the runner tripped npm/cli#4828 — rolldown
  native binding missing → **fixed** in workflow with `npm ci` → clean
  `npm install` fallback;
  (2) **rc2**: goreleaser dirty-check tripped because `Stage bundle into
  Go embed dir` step `rm -rf`'d the tracked nested `.gitignore` +
  `npm ci` sporadically rewrites `package-lock.json` → **fixed** by
  preserving tracked embed-dir files (`find … ! -name .gitignore
  ! -name .embed-placeholder -exec rm -rf`) + resetting lockfile churn
  via `git checkout` post-build + printing `git status --short` as a
  diagnostic;
  (3) **rc3**: `.goreleaser.yaml` sets `CGO_ENABLED=0` but the `aa`
  binary now depends on `mscrnt/webp` (cgo binding around `libwebp`) —
  arm64 tarball cross-compile impossible without cgo cross-toolchain →
  **NOT fixed**, escalated as **#238** (release-blocker) with 4 candidate
  resolutions (Docker-only distribution / docker-in-goreleaser /
  cgo cross-toolchain on runner / pure-Go webp fallback). Runner
  parallel-fork-exec limits also surfaced as followup **#239**.
  All three `v0.0.99-rc*` tags deleted; zero GitHub Releases/GHCR/
  Docker Hub artifacts published (goreleaser failed before push on
  all three runs). §1.6 of `docs/v0_1_readiness.md` flipped to 🟡 —
  pipeline audit complete + fixes 1+2 landed, but #238 gates a green
  rc4 dry-run. Closes #237.
  **1.55.T-2 shipped 2026-07-09** — Docker-only distribution + all-green
  rc9 dry-run + §1.6 close. Resolves #238 via **Option A** (planner
  decision: drop binary/deb/rpm/brew from the v0.1.0 release matrix
  since Docker is the documented channel and the Dockerfile already
  handles cgo correctly per-arch). Six more `v0.0.99-rc*` tags fired
  (rc4→rc9) — every RC surfaced a real defect the pipeline would have
  hit at v0.1.0:
  (4) **rc4**: goreleaser's metadata pipe unconditionally invokes `go`
  (not skippable) → added `Set up Go` + `workdir=app` back to the
  release-notes job;
  (5) **rc5**: goreleaser synthesizes a default `builds:` when config
  omits one → added `--skip=build,archive` — which then exposed that…
  (6) **rc6**: `--skip=build` isn't a valid goreleaser v2 pipe (only
  22 specific pipes are skippable; `build` is not among them) →
  deleted `.goreleaser.yaml` entirely, replaced with `gh release
  create --generate-notes` (fully automated on tag push, one fewer
  dep, cleaner fit for Docker-only). Filed **#242** to track full
  binary + package + Homebrew restoration as a **hard v1.0.0
  prerequisite** so the debt is visible;
  (7) **rc7**: docker job broke at Go build inside the Dockerfile —
  `--platform=$BUILDPLATFORM` pins the go-build stage to amd64 for
  speed, but only host `gcc` was installed; when TARGETARCH=arm64,
  cgo shelled to host gcc which couldn't assemble arm64 mnemonics
  from `mscrnt/webp`'s .S files → added `dpkg --add-architecture
  arm64` + `gcc-aarch64-linux-gnu` + `libwebp-dev:arm64` +
  per-TARGETARCH CC/PKG_CONFIG_PATH selection to the Dockerfile;
  (8) **rc8**: build + push + Sigstore cosign sign all green
  end-to-end; only the cosmetic Summary step failed on bash
  interpolation of multi-line `${{ steps.meta.outputs.tags }}` into
  a for-loop → switched to env-var passthrough + `while IFS= read -r`
  line iteration; **cosign verify green** against
  `mscrnt/artist-alley:0.0.99-rc8` (Sigstore transparency-log entry
  confirmed offline);
  (9) **rc9**: all 15 workflow steps green — release-notes ✓, docker
  buildx multi-arch (amd64 + arm64) ✓, cosign sign ✓, Summary ✓.
  Duration ~15 min. Verified cosign green against
  `mscrnt/artist-alley:0.0.99-rc9`; Docker Hub manifest confirms
  both arches + SBOM + provenance attestations.
  All 6 rc tags + GH Releases deleted; GHCR + Docker Hub images from
  rc8/rc9 stay published (cleanup requires `delete:packages` scope
  not on the coding-agent's local token; noted as user-cleanup task).
  §1.6 of `docs/v0_1_readiness.md` flipped ✅. Closes #238 + #241.
  **1.55.V-1 shipped 2026-07-09** — i18n coverage audit for v0.1.0.
  Report-only arc. Produces `docs/i18n_audit_v0_1.md` (10 sections):
  system architecture + coverage-guard analysis + dictionary coverage
  matrix (en 1608 keys / es 52 keys / fr 37 keys — **3.2% and 2.3%
  coverage** vs the `completionPct: 5` claim in `locales.ts`) +
  ~275 route findings (~87 MUST / ~37 SHOULD / ~150 NICE) +
  ~340 shared-component findings (79 MUST / 141 SHOULD / 120 NICE) +
  Playwright locale-switch coverage assessment (**zero specs switch
  locale + assert translations**; MUST-tier gap for 1.55.V-2) +
  backend user-facing-string audit (**214 unique English error
  strings** across 699 hits in `app/internal/**` returned raw as
  `openapi.*JSONResponse{Error: "..."}`; frontend `apiErr.error ??
  t('…')` idiom short-circuits translation — flagged as **v1.0.0
  prerequisite** parallel to #242 since the shape is a backend refactor
  + frontend mapper, not a straight-line V-2 fix) + fallback-behavior
  documentation (3-tier: active locale → en → raw key; silent English
  fallback masks the ~3% catalogue coverage with no runtime
  observability) + coverage-guard extension plan (~10 actionable
  items for 1.55.V-2 to execute against, including expanding
  `TRACKED_FILES` from 24 → all MUST+SHOULD files, adding
  attribute coverage for `label=` / `aria-labelledby` / template
  literals, a locale-parity check with warn-only budget, dev-mode
  observability instrumentation). Zero code changes; audit doc is the
  durable artifact + 1.55.V-2 executes against §9 fix recommendations
  and §10 guard extensions. Closes #244.
  **1.55.V-2 shipped 2026-07-09** — i18n MUST-tier fix + locale-switch
  test + guard extension. Executes the MUST scope from
  `docs/i18n_audit_v0_1.md` §9. Extends `common.*` from 16 → 40 shared
  keys (close/search/clear/load_more/download_original/untitled/saving/
  relative-time dur_*/etc.) and adds **211 scoped keys** across
  browse/setup/search/upload/comments/post_host/whiteboard/collections/
  federation/viewer_playlist/auth/playlist — en.json 1608 → 1843 leaf
  keys, **zero existing values changed**. 23 source files wrapped (20
  `.svelte` + 3 `.svelte.ts` state files); **es.json/fr.json untouched**
  — new keys are en-only and es/fr fall through to English per the owner
  decision (#247 tracks actual translation). New Playwright spec
  `ui-30-i18n-locale-switch` closes the audit's zero-test MUST gap:
  asserts the navbar search placeholder flips en→es on switching to
  Español + persists across reload via the `aa_lang` cookie (asserts on
  the existing-Spanish `nav.search_placeholder` — proves the mechanism,
  not es/fr coverage). Coverage guard extended per §10: `TRACKED_FILES`
  24 → 44 (all now-clean MUST files; `AssetPlaylist` held back for its
  deferred viewer-hotkey SHOULD strings), added `label=`/
  `aria-description=` attribute coverage, strips HTML comments + `<code>`
  + `<style>` before scanning (kills 2 false positives), and a
  **warn-only locale-parity check** reporting es 3% / fr 2% without
  failing CI (orphan-key half stays blocking as a schema-drift guard).
  `locales.ts` `completionPct` now **computed** from the bundled
  catalogues (was a stale hardcoded 5). svelte-check 0 errors; 283
  vitest tests green. SHOULD (~178) + NICE (~270) tiers deferred to
  **#249**; backend error strings remain **#246** (v1.0.0 prereq); es/fr
  translation remains **#247**. Audit §11 tracks every MUST finding to
  disposition. Closes #248.
  **1.55.C-1a shipped 2026-07-07** — soft-delete recovery foundation
  (§4.6 partial). Migration 00029 adds `deleted_reason` to assets +
  posts + collections and adds `deleted_at` to collections;
  `app/internal/softdelete/` package ships the Restore + HardDeletePast
  primitives per entity plus the nightly gc CoordinatorJob wired at
  boot; `sysconfig.SoftDeleteConfig` exposes 4 retention knobs +
  gc-hour-utc (range-validated); 10 new audit event constants +
  Recorder methods; user hard-delete-by-gc anchors off the existing
  `admin.users.archived` audit event rather than adding a competing
  `deleted_at` column (hybrid scope per pre-audit). GC coordinator
  reads sysconfig every tick so operator retention changes take effect
  on the next nightly pass.
  **1.55.C-1b shipped 2026-07-08** — soft-delete surface layer
  (§4.6 complete). DELETE handlers on assets + posts + collections
  accept an optional `SoftDeleteRequest` body carrying an operator
  reason string (max 500 chars); collections DELETE flips from HARD
  to SOFT delete on the same code path (clean break per pre-release
  practices); 3 new admin restore endpoints at
  `POST /admin/{entity}/{id}/restore` delegating to
  `softdelete.Service` from the foundation with a CTE-based snapshot
  of the pre-restore state so the audit event carries `prior_reason`
  + age-at-restore; `?include_deleted=true` admin-only query param
  on the 3 list handlers (non-admin toggles ignored); `GetCollection`
  grows a fallback branch that surfaces soft-deleted rows to admins
  so the Restore button on `/collections/[id]` has something to
  render; admin UI ships the include-deleted toggle on `/collections`
  list + Restore action on collection detail; posts/assets admin
  detail-page Restore UI deferred (no admin detail page exists;
  assets viewer-based). Live smoke green end-to-end.

## In flight

These have foundations in place; the rest of the surface area is the
current focus:

- **Image processing pipeline** (Phase 1.18.A — shipped). Variant
  generation (col / preview / screen / hires), thumbhash placeholders,
  content-addressed cache headers, generic background-job queue with
  in-process workers + HTTP claim API for external farms /
  federated peers.
- **Video pipeline + animator review player** (Phase 1.18.B). The
  load-bearing UX: a SyncSketch / Keyframe-Pro-2 replacement built
  into the post modal. See "Review tool" arc below for the full
  feature plan.
- **AI arc** (Phase 1.14). The AI inference subsystem is shipped:
  multi-provider abstraction + router + jobs + admin surface
  (1.14.A, PR #149), asset/AI bridge layer + tag provenance
  (1.14.A-bridge, PR #150), CLIP embeddings + pgvector similarity
  (1.14.B, PR #151), Whisper transcription + subtitle integration
  (1.14.C, PR #152), and the first internal MCP caller — img2img
  via the ComfyUI MCP bridge (1.14.E-1, PR #156). **AI auto-tagging
  itself remains in-flight** (issue #18): the inference + provenance
  scaffolding is there, but the upload-time tag inference call is
  not yet wired. Reverse-image search runs against the 1.14.B CLIP
  embeddings today. Next AI sub-phases: 1.14.D (bridge consumption
  cleanup), 1.14.E-2 (full Creative tools panel + mask UI + four
  remaining ops), 1.14.F (caption persistence).

- **Upload-side metadata extraction** (Phase 1.18.A-2 / 1.18.A-3 —
  shipped 2026-06-22 → 2026-06-26). Every uploaded image now extracts
  EXIF (1.18.A-2, PR #158), preserves the ICC chunk through the
  variant pipeline, applies EXIF orientation at variant-render time
  (source bytes pristine), and per-user dedups via a partial unique
  index (PR-A, PR #159). Operators wire extraction config per
  field-definition via an admin picker, review failures in a paginated
  queue, and trigger backfill against existing photos through the
  admin UI (1.18.A-2 PR-B, PR #160). 1.18.A-3 (PR #166) added IPTC
  + XMP extractors against the existing extractor interface. **HEIC
  remains deliberately unsupported** (only pure-Go HEIF reader pulls
  libde265 via CGo; vetoed by the no-CGo guardrail; tracked as
  capability add-on per ADR 0034). Remaining: 1.18.A-3.B (raw camera
  embedded thumbs CR2/NEF/ARW/DNG + PDF page count + title/author),
  1.18.A-4 (video thumbnail at configurable timecode).

- **Account lifecycle** (Phase 1.19 — arc COMPLETE 2026-06-23 →
  2026-07-04). AA can now accept a public user without admin
  handholding, with auth hardening against username enumeration.
  Five PRs landed:
  - **1.19.A-1** (PR #161): email substrate with SMTP-at-rest +
    template library + test-mode capture
  - **1.19.A-2** (PR #162): admin impersonation with
    capability-intersected effective caps + always-visible banner
    + audit recording both actor IDs
  - **1.19.B** (PR #163): self-service TOTP 2FA with enrollment +
    login gate + recovery codes
  - **1.19.C** (PR #164): self-registration + email verification +
    admin approval queue with email-enumeration-safe responses
  - **1.19.D** (PR #198): per-username account lockout — closes
    the original 1.19.B deferral. Migration 00025 adds
    `user.failed_login_count` + `user.lockout_until` + `auth.unlock`
    capability. Race-safe atomic UPDATE-with-CASE (N=10-goroutine
    test verifies exactly-threshold-many increments before
    lockout). Anti-enumeration invariant proven: locked path runs
    bcrypt against dummy password so response timing matches
    wrong-password; 401 shape identical. Composes with existing
    per-IP + per-username rate limits (429 for rate, 401 for
    lockout — distinct failure modes protect against different
    threats). Admin unlock via `POST /admin/users/{ref}/unlock-account`
    with `auth.unlock` capability. IP-subnet-hash audit
    (HMAC-SHA256 salted with ScrambleKey; /24 IPv4, /56 IPv6) —
    threat class without per-request IP log. Federation-safe:
    per-instance state, never federates. Closes issue #171.
  See ADR 0054. No SMS / no email-OTP fallback in v1 — TOTP only.
  Follow-ups explicitly declined per 1.19.D handoff: per-team
  lockout policies (global sysconfig sufficient), password-change
  auto-clear (threat-model gap), email notification on lockout
  (anti-enumeration), bulk unlock UI (rare action), CAPTCHA
  integration (separate arc), full per-request IP audit log
  (subnet hash is intentional).

- **IIIF interoperability** (Phase 1.54 — arc shipped 2026-06-25 →
  2026-07-03). See ADR 0053.
  - **1.54.A** (PR #165): IIIF Image API 3.0 Level 0 over the
    existing variant pipeline. Manifest endpoints (`info.json`),
    region / size / rotation / format / quality parameters,
    content-hash-keyed tile cache, anonymous `iiif.read` capability
    gated on existing visibility, `/admin/iiif/health` per the
    generic subsystem-health pattern. Tile cache is content-
    addressable forever; no persisted derivatives.
  - **1.54.B** (PR #187): Presentation API 3.0 collection + asset
    manifests at `/iiif/3/{kind}/{id}/manifest.json`; navPlace
    geo-tag extension for GPS-tagged assets; embargo-stub manifests
    per ADR 0020; Content Search 2.0 at `/iiif/3/{kind}/{id}/search`
    (asset-scope substring-scans metadata pairs; collection-scope
    dispatches through the 1.16.B `search.Engine` filtered to pinned
    members); 2.0→3.0 URL redirect at `/iiif/2/...` (301, `full`→
    `max` size grammar); federated canvas resolver (5-min in-process
    cache, outside `app/internal/federation/` per soak rule); IIIF
    subsystem card on `/admin/search/dashboard`; Playwright
    structural smoke. Closes issue #170.
  - **1.54.C** (PR #194): External URL alias — Go dual-mount at
    root (in addition to existing `/api/v1/iiif/*` mount). Fixes a
    pre-existing 1.54.A URL emit/mount mismatch where handlers
    emitted `/iiif/3/...` but were only reachable at
    `/api/v1/iiif/3/...`. Nginx-rewrite path was tried first but
    failed CI because `ui-pr.yml` runs embed_web app image
    standalone with no nginx (belt-and-braces: works for both
    standalone and nginx-fronted deployments). Closes issue #188.
  - **1.54.D** (PR #195): Automated Mirador dogfood via Playwright
    on the self-hosted nightly runner —
    `scripts/dogfood/ui/tests/standalone/ui-13-iiif-mirador-dogfood.spec.ts`
    with 3 structural-DOM assertions (Mirador manifest render,
    metadata sidebar populated, Content Search 2.0 endpoint returns
    hits) against a seeded mixed-format fixture collection (2 JPEGs
    + 1 PNG + 1 PDF, one JPEG carrying GPS EXIF for navPlace).
    Mirador via unpkg.com CDN at nightly-only cadence. Screenshot
    artifacts retained 30 days for operator post-hoc review (~2 min
    per merge, down from ~30 min manual dogfood). **Two real
    1.54.B / 1.54.C bugs surfaced and fixed as unblocking
    carve-outs**: Canvas missing required `width`/`height` per
    Presentation 3.0 §5.7 (Mirador crashed on `null.getValue`;
    default 1200×900 landscape placeholder), and Vite dev proxy
    didn't cover `/iiif` alongside `/api` (with `changeOrigin: false`
    so `publicBaseURL(r)` sees original Host). **IIIF arc
    code-complete.** Issue #193 closes on three consecutive green
    nightly runs after merge (first green banked from
    workflow_dispatch on the feature branch; scheduled nightly at
    07:00 UTC provides the remaining two). Follow-ups filed:
    **1.54.E** per-page PDF tile routing, **1.54.F** Content Search
    per-line text extraction (asset_text FTS table), **1.54.G**
    Custom Provider block sysconfig UI, **1.54.H** Content Search
    AnnotationCollection pagination, plus two 1.54.D carve-out
    follow-ups: real image dimensions into `EntityRef.Width/Height`,
    Vite `changeOrigin: false` regression guard.

- **Edit-safety** (Phase 1.16 — partial; shipped 2026-06-26).
  Optimistic-concurrency edit-safety on `PATCH /assets/{id}` +
  `PATCH /collections/{id}` + `PATCH /posts/{id}` (PR #167) using
  `If-Unmodified-Since` headers against the entity's `updated_at`
  timestamp; 409 Conflict with full current-state body on stale
  writes; frontend modals capture baseline + surface 409 with
  explicit "overwrite" action. Lock-free, federation-safe. See ADR
  0052. Resource locking, batch multi-asset metadata edit, and
  custom per-resource ACL remain for follow-ups within the 1.16
  bracket.

- **Search arc** (Phase 1.16.B — shipped 2026-07-01 → 2026-07-02
  across five sub-phases). Unified `/search` endpoint over Postgres
  tsvector with field weighting, DSL parser with strict whitelist,
  cross-package `visibility.Filter` package, pgvector hybrid
  ranking, LISTEN/NOTIFY cache invalidation, saved-searches with
  delta detection + digest emails, admin reindex + observability
  surface. See ADR 0056. Sub-phase ship list:
  - **1.16.B-1** (PR #174): foundation — unified endpoint + BM25-
    shaped ranking + tsvector expansion + cursor pagination +
    LISTEN/NOTIFY-broadcast `QueryResultCache` + `/admin/search/health`
  - **1.16.B-2** (PR #176): facets + advanced DSL + autocomplete
    (`pg_trgm`) + weighted-tsvector retrofit + `visibility` package
  - **1.16.B-3** (PR #178): vector search — `similar_to:<uuid>`
    compile + Engine hybrid ranking + `POST /search/by-image`
    reserved 501 (unblocked by 1.16.B-3-followup below)
  - **1.16.B-3-followup** (PR #199, 2026-07-05): CLIP visual
    encoder sidecar + reverse image search activation.
    `tools/aa-clip-visual-local/` — Python 3.12 + FastAPI +
    OpenCLIP ViT-L/14 OpenAI checkpoint (768-dim, ~2 GB fat image
    with baked checkpoint, Docker Compose profile `visual-search`).
    Migration 00026 adds `asset_visual_embedding` table with
    `vector(768)` + HNSW cosine index (m=16, ef_construction=64),
    **deliberately separate from `asset_embedding_d768`** — two
    embedding spaces (text via Ollama nomic-embed-text, visual via
    CLIP ViT-L/14) with zero cross-comparison. `POST /search/by-image`
    handler flips from 501 stub to 200 when the visual provider
    is registered; nil-provider preserves 501 byte-for-byte. Sysconfig
    `search.visual.{enabled,sidecar_url,timeout_ms,max_upload_bytes,rate_limit_per_user_per_minute,auto_embed_on_upload}`.
    Provider bootstrap probes `/health` at boot; refuses registration
    on dim mismatch (guards against wrong-model swaps corrupting
    the vector column). Coarse MVP visibility floor (anon = public
    only; auth = row-level downstream; consolidation with
    `visibility.Filter` tracked at #185). Closes issue #183.
    Follow-ups filed as separate issues (#200–#204 range).
  - **1.16.B-3-followup-4** (PR #205, 2026-07-05): Admin visual-
    embedding backfill trigger. Migration 00027 adds
    `search_visual_backfill_run` table with partial UNIQUE INDEX
    enforcing single-active-run at DB layer (23505 → HTTP 409).
    New `app/internal/search/vector/visualbackfill/` package
    (Store + Job + Handler) mirrors 1.16.B-5 reindex shape
    verbatim minus the `target` column. Coordinator loop with
    per-batch cancel probe, rate-limited via `golang.org/x/time/rate`,
    typed transient-vs-permanent error classification (only
    `ErrSidecarUnreachable` retries; decode/dim-mismatch/missing-
    hash → failed without abort). Fail-fast 503 when provider
    unregistered (prevents polluting history table). 3 new sysconfig
    knobs (`BackfillBatchSize=100`, `BackfillRateLimitPerSecond=5.0`,
    `BackfillTransientRetryCount=1`); 3 new health gauges
    (`visual_backfill_active`, `visual_embedding_backlog`,
    `visual_embedding_total`). Frontend
    `/admin/search/visual-backfill/+page.svelte` (backlog/coverage/
    total tiles + Start-503-aware + progress bar + Cancel + recent-
    runs table) + `/admin/search/dashboard` live Visual search tile
    replaces pre-followup "By-image (reserved)" placeholder.
    Closes issue #200; partially subsumes #203.
  - **1.16.B-3-followup-2** (PR #206, 2026-07-06): Async
    visualembed upload-hook job. New
    `app/internal/search/vector/visualembed/` package with
    atomic-based Counter (6 keys: success / transient_failed /
    permanent_failed / rate_limited_wait / skipped / pending),
    `JobTypeVisualEmbed` handler that delegates retry entirely to
    the jobs framework (`*jobs.TerminalError` for permanent, plain
    error for transient — mirrors 1.14.B `ai.embed` shape verbatim
    rather than in-handler retry loop, per pre-audit Q3), and
    `Dispatcher` with guard chain (provider registered → sysconfig
    auto_embed enabled → asset extension is image → Jobs) called
    immediately after the `ai.embed` enqueue in the CreateAsset
    fanout. 2 new sysconfig knobs (`AutoEmbedRateLimitPerSecond=5.0`,
    `AutoEmbedRetryCount=2`). Process-shared rate limiter with 5s
    wait timeout — bulk uploads queue-and-smooth rather than
    fail-then-retry; rate-limit-wait time metered as its own
    counter key. Dedicated counter avoids ballooning search-query
    p95/p99 (embed durations 100–500ms would swamp shared
    percentile window). Deterministic idempotency key
    (`search.visual_embed|<asset_id>`) prevents federation-replay
    or upload-retry double-enqueue. Silent skip on guard failure
    (non-image uploads are common in mixed corpora — skip counter
    surfaces volume without log flood). Runtime-toggleable via
    sysconfig (no restart). Deferred: dedicated duration histogram
    (#207). **Reverse-image search coverage now complete —
    existing corpus via backfill, new uploads via auto-embed.**
    Closes issue #201; #183 arc fully wrapped.
  - **1.16.B-4** (PR #180): saved searches + delta detection +
    email-on-match via the 1.19.A-1 substrate + digest coordinator
  - **1.16.B-5** (PR #182): admin reindex tooling + disk-usage view
    + saved-search admin + full `/admin/search/dashboard` +
    federation-inbox embed hook (out-of-tree). Issue #168 closed;
    ADR 0056 accepted.
  - **1.16.B-5-followup** (PR #208, 2026-07-06): Search feedback
    loop — thumbs up/down on results + admin aggregation.
    Migration 00028 adds `search_feedback` table with
    `UNIQUE (user_ref, hit_asset_id, query_hash)` enforcing
    vote-flipping via `ON CONFLICT DO UPDATE` + CHECK constraints
    (direction ∈ 'up' or 'down', hit_position ≥ 1) + 4 supporting
    indexes. New `app/internal/search/feedback/` package: SHA-256
    query hash over trim + collapse-whitespace + lowercase
    canonical form (documented as NOT full AST canonicalization —
    `cat AND dog` still distinct from `dog AND cat`); hand-written
    SQL store matching reindex/visualbackfill/visualembed pattern;
    disabled → visibility → rate-limit → upsert gate chain;
    `PoolVisibility` (exists + non-deleted; enumeration-safe
    conflation with `hit_not_visible` — attacker can't probe UUIDs
    via feedback). `POST /search/feedback` + `DELETE /search/feedback/{id}`
    require authenticated user (401 anonymous); rate-limited via
    `SELECT COUNT(*) WHERE user_ref = $1 AND feedback_at > NOW() -
    INTERVAL '24 hours'` — undo (DELETE) refunds the token
    naturally by lowering the count; no separate refund
    bookkeeping; survives restarts. `GET /admin/search/feedback`
    anonymized aggregation (top down-voted queries + under-ranked
    hits; both use `latest_dsl` CTE for display-form DSL per
    `query_hash`). `GET /admin/search/feedback/audit/{user_ref}`
    per-user abuse-review log; access fires
    `admin.search.feedback.audit_viewed` audit event recording
    both actor + subject. Anonymized-by-default aggregation
    (per-user log is behind a distinct URL that requires typing
    ref explicitly AND fires audit). Frontend
    `ThumbButtons.svelte` with optimistic UI (updates locally,
    POSTs in background, reverts on error) + 300ms debounce +
    5s undo-toast firing DELETE + a11y (`aria-pressed`, labels) +
    hides entirely when unauthenticated. 3 sysconfig knobs
    (`Enabled *bool` pointer semantic so fresh installs default
    true, `MaxPerUserPerDay=60`, `AggregationWindowDays=7`),
    range-validated. `auth.IPSubnetHash` exported with a `domain`
    argument (HMAC-SHA256 salted with ScrambleKey; /24 IPv4,
    /56 IPv6) — 1.19.D lockout path delegates to the shared
    implementation; domain prefix prevents cross-subsystem hash
    collision on rotated salts. 5 new `search.Counter` Result
    classes (`search_feedback_up`, `_down`, `_undo`, `_rate_limit`, `_disabled`)
    + `AsFeedbackCounter` adapter mirroring the saved-search
    pattern. New `search_feedback_active_voters` gauge (DISTINCT
    user count in aggregation window) on `/admin/search/health`.
    New Feedback tile on `/admin/search/dashboard` groups the 5
    Result classes + active_voters gauge; nav link. Query cache
    NOT invalidated on feedback events (load-bearing decision —
    feedback is out-of-band signal, results stay stable for the
    60s cache TTL). Per-instance state — never federates. Runtime-
    toggleable via sysconfig (no restart). Signal for future ADR
    0055 pg_search revisit — structured data on 'which queries
    surface bad results' instead of vibes. Deferred: dedicated
    duration histogram (would pollute the shared search.Counter
    latency window — same reasoning as PR #206); split
    `search.Counter.RecordEvent` vs `RecordLatency` (#209).
    **Closes issue #184.**
  - **1.16.B-followup** (PR #213, 2026-07-06): `visibility.Filter`
    retrofit. Scope-trimmed after pre-audit. The follow-up brief
    described four surfaces duplicating the shared check (list
    handlers, IIIF gate, by-image floor, feedback PoolVisibility).
    Pre-audit found ONE genuine duplicate (feedback) plus three
    surfaces with distinct semantic shapes that would require
    speculative API additions to unify: IIIF is field-level
    metadata gating not row-level; by-image uses a `sensitivity`
    column that `visibility.Filter(EntityAsset)` does not touch;
    list handlers are sqlc-static queries with no dynamic
    visibility fragments to consolidate. Shipped the honest scope:
    new `visibility.CanSee` helper that composes the shared
    Predicate into an EXISTS-shaped query byte-for-byte matching
    the pre-retrofit inline SQL feedback used. `feedback.PoolVisibility`
    swapped to call the helper. Enumeration-safe collapse of
    `not-visible` and `not-exists` into 403 `hit_not_visible`
    survives verbatim (all 11 snapshot-test error-body assertions
    preserved). ADR 0056 §4 updated. Follow-ups filed for the three
    deferred sub-scopes: sensitivity semantics for by-image (#210);
    a FieldVisibility API for IIIF (#211); sqlc migration for list
    handlers (#212). **Closes issue #185.**
  - **1.16.B-followup-2** (PR #215, 2026-07-06): shared
    `AdminBackfillPanel.svelte` extraction — closes #186. Pure
    frontend refactor consolidating three admin backfill surfaces
    that shipped their UX shell independently across PRs #160
    (metadata extraction), #182 (search reindex), and #205
    (visual-embedding backfill). Component lives at
    `web/src/lib/components/admin/AdminBackfillPanel.svelte`,
    props-driven and API-agnostic — parents own fetch, component
    consumes `onStart` / `onRefresh` / `onCancel` callbacks. Every
    subsystem divergence expressed via Svelte 5 snippets or optional
    props (no `if page === X` branches inside): `controls` snippet
    for the per-subsystem form (metadata: asset-type + file-ext +
    include-non-image; reindex: scope + target picker; visual: none);
    `gauges` snippet for the backlog/embedded/coverage tiles
    (visual only today); `extraColumnHeaders` + `extraRowCells`
    snippets for extra columns before Processed (metadata: Scope;
    reindex: Scope + Target); `extraColumnHeadersAfterFailed` +
    `extraRowCellsAfterFailed` for extras after Failed (visual:
    Progress); `startDisabledReason` prop generalises visual's
    503-when-sidecar-not-registered gate; `disableStartWhenActive`
    prop lets metadata queue runs while another is active (per its
    existing behaviour). Vitest snapshot suite committed FIRST as
    16-cell compliance baseline (subsystem × state matrix + 503
    gate), running green on every subsequent commit. Vitest
    semantic unit tests cover Start-disabled tooltip, Cancel
    callback + confirm gate, empty-state colspan math across snippet
    counts, status-badge classification for running/done/failed/
    cancelled. Live Playwright smoke on all three admin pages
    confirmed identical column headers + button labels + gauge
    values pre/post-refactor. Vitest config extended with
    `resolve.conditions: ['browser']` so Svelte 5 + Testing Library
    components can render (previously only pure `.svelte.ts` helper
    tests existed; this is the first component-render test path).
    Zero backend / API / sysconfig / migration changes.
    **Closes issue #186.**
  - **1.16.B-followup-3** (PR #217, 2026-07-07): `search.Counter`
    split — closes #209. Cheap 30-min hygiene refactor identified
    during PR #208. Split the shared `search.Counter.Record(class,
    duration)` into `RecordLatency(class, duration)` (semantics
    unchanged; real request timings) and `RecordEvent(class)`
    (new; increments `requests[class]` only — never touches the
    latency window). Twelve event-only callers migrated to
    `RecordEvent` (five in `AsFeedbackCounter` adapter + seven in
    `AsSavedSearchCounter` adapter); seven latency callers moved
    to `RecordLatency` (main `/search` handler + six sites in
    `by_image.go`). Prior conflation caused zero-value observations
    from event-only paths to dilute p50/p95/p99 percentile
    reporting. Clean break per no-backcompat-shims memory — no
    `Record` shim; grep proof `.Record(Result` empty across
    `app/internal/search/`. Zero observable change from
    `/admin/search/health` (response shape unchanged; percentiles
    now reflect real query timings). Three new unit tests protect
    the invariant. Also fixed pre-existing v2.7.1 → v2.7.2
    oapi-codegen version-comment drift on `dev` that had been
    silently blocking the Codegen drift check on every PR.
    **Closes issue #209.**
  Follow-ups filed: #210 sensitivity semantics for
  `visibility.Filter(EntityAsset)`; #211 `FieldVisibility` API for
  IIIF metadata gating; #212 sqlc migration path for list handlers;
  #214 MDX braced-identifier CI gate on docs PRs; #216
  `.pnpm-store` gitignore + docs-build hygiene; #218 pin
  oapi-codegen version explicitly in `scripts/generate.sh`.
  **1.16.B search arc fully closed** — 5 sub-phases plus 6
  followups shipped end-to-end.

## Review tool — the load-bearing UX arc

The post modal is becoming the animator's review tool. Phase 1.18.B
ships in sub-phases so each piece can be tested against real review
sessions before the next lands.

### 1.18.B-1 — Video pipeline (shipped)
- ffmpeg-based preview.video handler: probe → poster → HLS adaptive
  ladder (480p / 720p / 1080p) → 100-cell sprite sheet + WebVTT.
- Encoder detection at boot: probes ffmpeg `-encoders` AND verifies
  each candidate with a 4-frame synthetic encode. Picks NVENC > QSV
  > VideoToolbox > libx264. Falls back gracefully on any failure.
- Wildcard chi route for multi-segment HLS variant keys.
- `<video>` element with HLS.js (native HLS on Safari). HUD with
  HH:MM:SS:FF timecode + frame counter via
  `requestVideoFrameCallback`. JKL transport, ±1 / ±10 frame step
  (arrows + comma/period + Shift), I/O loop region, 1-5 speed
  presets, sticky muted-autoplay + persistent volume via
  localStorage, sprite hover preview on the scrubber.
- Mounted in the post carousel directly (no extra Review-mode click).

### 1.18.B-2 — Player polish + quality-of-life (in flight)
- Click-to-pause on the video canvas; click-and-drag scrub on the
  HUD timecode.
- Fullscreen button + `F` shortcut; always-on-top (PiP) when the
  browser allows.
- Jump-to-frame input + jump-to-timecode parser.
- Pan + zoom on the video frame (mouse wheel + drag) — pixel-perfect
  inspection without leaving playback.
- Flip + rotate (H / V / 90°).
- Ping-pong loop, hold-frame at IN/OUT, range bookmarks.
- Frame bookmarks (single + range) with cycle hotkeys.
- Audio scrub (variable-speed) while dragging the playhead.
- Screen capture to clipboard / file.

### 1.18.B-3 — Subtitles + multi-format captions (shipped)
**Shipped via PR #137 (`5d16dcb`).** ui-nightly green at 10m21s on 2026-06-16.
- Native WebVTT track support (`<track>` elements) wired into the existing VideoPlayer.
- Pure-Go worker-side conversion of SRT / SSA / ASS / SUB → WebVTT; IDX deferred to a capability add-on per ADR 0034.
- Sidecar auto-detection on multi-file asset ingest (`clip.mp4` + `clip.en.srt` pattern).
- Burned-subtitle export via the existing worker queue.
- Per-user subtitle preferences (enabled / preferredLang / fontSize / position).
- Dedicated `asset_subtitle_tracks` table — tracks bound to assets via FK + CASCADE; NOT counted in asset-count queries; only applicable to audio/video assets (422 guard).
- First post-baseline migration under ADR 0046's append-only convention; sets the `0000N_description.sql` convention going forward.

### 1.18.B-4 — Image sequences + RAM cache + always-on-top
- Treat an image-sequence asset (numbered PNG/EXR/JPEG run) as a
  first-class playable timeline.
- Client-side RAM cache: pre-decoded frame buffer for instant scrub
  on short clips.
- "Always on top" window mode (browser PiP / SPA detached window).

### 1.18.B-5 — Presentation rooms (global, real-time)
Presentation is a global capability — image, video, 3D, PDF, any
asset that can be shown. The player / viewer is a host; the
"presentation room" is a separate concern that wraps any host.

- WebSocket presence room keyed on `(post_id or asset_id)`: live
  cursor, live scrub position, who's watching.
- Synchronous mode: a presenter drives the playhead / camera /
  page. Observers follow. Offline = solo (default).
- Shared chat + reactions alongside whatever's being shown.
- Mobile + tablet observers via the same UI.
- The protocol is asset-type-agnostic: a frame number, a 3D camera
  matrix, a PDF page index — all "anchor" objects the host sends
  on each tick. Observers' viewers replay them.

### 1.18.B-6 — Annotation system (global, frame/anchor-aware)
Annotations are a global capability too — they live in their own
package and any asset host (image, video, 3D, PDF) surfaces them.
Extends to **PDF page annotations**: anchored to `(asset_id, page,
x, y, w, h)`; rendered against the PDF raster preview; exported in
the PDF summary with comment thread + screenshots per anchor. Clean-room
Svelte implementation of pen / highlighter / arrow / text / eraser
tools.

- Anchor model: an annotation binds to an asset_id PLUS an
  asset-type-specific anchor — a frame number for video, a
  camera+target for 3D, a page+x/y for PDF, a coord+zoom for
  images. The same `annotations` table stores them all; the host
  package owns rendering.
- Brush engine: pen, highlighter, eraser, shapes, text, arrows.
- Foreground + background layers; background transparency.
- Laser pointer for live review (broadcasts via the presentation
  room when one is active).
- Ghosting / onion-skin between adjacent anchors (video frames,
  3D camera takes, PDF pages).
- Annotation bookmarks; cycle hotkeys.
- PDF summary export with comment thread + screenshots per anchor.

### Architecture note — how the global systems connect
The three layers stay separate so each evolves independently:

1. **Host viewers** — `ImageReview`, `VideoPlayer`, `ModelViewer`,
   `PDFViewer`, etc. Each owns its surface (pan/zoom, scrub, camera,
   page). All emit + accept a typed "anchor" describing where we
   are.
2. **Annotation overlay** — a single Svelte component layered on
   top of any host. Reads + writes anchor-tagged annotations via
   the shared `/assets/{id}/annotations` API. Doesn't know about
   the host beyond the anchor schema.
3. **Presentation room** — a separate WebSocket-backed store that
   broadcasts the current anchor + cursor + chat. Any host can
   listen to it (slave its anchor to the presenter) and emit to it
   (when this user is the presenter).

Concretely: VideoPlayer emits `{type:"video", frame:N}` anchors;
ModelViewer emits `{type:"model", camera:[...], target:[...]}`;
PDFViewer emits `{type:"pdf", page:N, x, y, zoom}`. Annotations are
indexed by `(asset_id, anchor_hash)`. Presentation rooms forward
whatever anchor the presenter emits to everyone in the room.

### 1.18.B-7 — Timeline assembly
- Build a review timeline from multiple source files (different
  takes, different shots).
- Per-source in / out points, ordering, audio override.
- Seamless playback between sources.
- Export the assembled timeline as a standalone media file (ffmpeg).

### 1.18.B-8 — A/B comparison
- Split viewer (horizontal / vertical / grid).
- A/B wipe with draggable seam.
- A/B transparency / overlay blend.
- Pair any two assets — different versions, reference footage.

### 1.18.B-9 — DCC integrations
- Python client API for external tools to drive the player
  (open, seek, annotate, snapshot).
- Maya → review send-back script.
- Blender / Houdini integrations (community-driven via the same API).
- Webhook + presence broadcasts for studio production trackers.

### 1.18.B-10 — 3D viewer (native)
- `<model-viewer>` for glTF / GLB native — instant load, AR,
  animations.
- three.js loader fallbacks for OBJ (+ .mtl + texture set) and FBX.
- Marmoset Toolbag `.mview` native player (self-contained JS).
- USDZ via Quick Look on Apple devices.
- Camera presets, lighting picker, turntable poster generation,
  wireframe / UV inspect modes.

### 1.18.B-11 — 3D heavy converters (worker side)
- Blender headless → glTF for `.blend`.
- FBX2glTF for cached glTF when the source format is heavy.
- Maya `.mb` / `.ma` and 3ds Max `.max` — extract embedded preview
  where possible; full conversion gated on a licensed converter
  side-car. Native-viewer-first; convert only on miss.
- Per-format converter run as a separate job type so a render farm
  can pick it up.

### 1.18.B-12 — Audio / SVG / PDF / fonts
- Audio waveform PNG + scrub.
- SVG → rasterized variant set.
- PDF first-page raster + multi-page navigator.
- Font specimen render.

### 1.18.B-13 — Project / workspace
- "Review project" entity: a collection of assets curated for a
  review session, with timelines, annotations, chat history.
- Save / share / fork projects.
- Snapshot a project state for "this is what we showed at the
  Tuesday review".

### 1.18.B-14 — Federation + share-with-anyone
- Public share link with permission scope (view / comment /
  annotate) and optional expiry.
- Federated peer instances surface annotations + chat back to the
  origin instance. The job queue's existing `origin_server_id` +
  the HTTP claim API are the carrier.
- Quick-generate PDF summary for offline clients.

### 1.18.B-15 — Sprite-sheet viewer (shipped)
- Auto-slice by uniform grid (rows × cols, configurable cell size)
  and by edge-detection (find tightest bounding box per visible
  region) — operator picks the strategy per asset.
- Frame index panel with thumbnails; reorder, name, and group
  frames into named action sequences (idle / walk / attack).
- Animation timeline + scrubber that plays the slices in order with
  configurable FPS, ping-pong, hold-frame, and onion-skin between
  adjacent frames. Reuses the existing video HUD primitives.
- Per-frame metadata persisted as a sidecar companion JSON
  (action ranges, per-frame notes, slices, alt-files /
  palette-swap previews). The anchor-tagged annotation integration
  with 1.18.B-6 is deferred — the companion format is forward-
  compatible so a later commit can rewire it into the shared
  annotation surface without re-tagging existing data.
- Export individual cels as PNG / WebP, or export the action set
  as a packed sprite sheet (PNG + JSON) or a per-frame zip; a
  client-side GIF encoder lets the browser produce share-ready
  animations without a server round-trip.

### 1.18.B-16 — Texture inspector
- Channel splitter: view R / G / B / A as separate greyscale views,
  or as a packed-channel overlay (e.g., R = roughness, G = metallic,
  B = ambient occlusion — the convention surfaces in the inspector
  UI so reviewers don't have to guess).
- Linear ↔ sRGB toggle for accurate color-managed review of
  source textures vs runtime appearance.
- Normal map preview with light-source rotation; tangent-space
  validation (is this normal map decoded the way you think it is?).
- Mip-chain preview: scroll through pre-computed mip levels at the
  size they'll appear in-engine. Catch mip-popping problems before
  ship.
- Alpha channel mode: solid / checkerboard / outline overlay so
  reviewers can see transparency cleanly.
- File-format awareness: BC1 / BC3 / BC5 / BC7 / ASTC / ETC2
  decoders so the inspector shows what the engine actually
  sees, not just what Photoshop saved.

### 1.18.B-17 — Color & palette inspector
- Eyedropper with hex / RGB / HSL / OKLCH readout; copy to
  clipboard in any format.
- Dominant-color extraction: top-N palette of any image, with
  share-of-pixels percentages. Useful for "does this character
  match our style palette?" reviews.
- Palette comparison: drop two assets side by side, see their
  extracted palettes overlaid, with delta-E coverage of every
  swatch. Catches style drift between artists.
- Save and name reference palettes per workflow / collection;
  validate any new upload against a reference palette and warn on
  drift beyond a configurable delta-E threshold.

### 1.18.B-18 — Bitmap / format inspector
- Size diff between source and encoded variants: source PNG vs
  BC3 vs BC7 vs ASTC, byte-for-byte and as a percentage. Helps
  optimization reviewers spot regressions.
- Side-by-side encoded preview against the source, with the same
  A/B wipe primitives from 1.18.B-8 so format compression artifacts
  surface visually.
- Alpha-on-color delta view: highlight pixels whose alpha or color
  changed between source and encoded variant, in any user-selectable
  color. Surfaces premultiply / unpremultiply mistakes immediately.
- Per-format metadata badges: compression type, block size, encoder
  used, expected runtime cost. Operators know exactly what they're
  looking at without reading file headers manually.

### 1.18.B-19 — Review queue (auto-rotated)
The "review mode" navbar button (already shipped) opens a system-curated
queue of assets that need review. Reuses the existing AssetViewer +
AssetPlaylist surfaces; no new viewer shell or "review session" toggle —
the navbar button is dedicated enough.

- **Auto-rotated queue.** A scheduled job rolls over to a fresh queue
  on a configurable cadence. Default policy is **opt-in** — operator
  enables auto-rotation in admin settings; absence keeps the queue
  manual / operator-curated. Once enabled, the queue auto-populates
  with assets matching a curation rule (initial impl: "assets created
  since last queue rollover that haven't been reviewed").
- **Last-viewed-item resume.** Per-(user, queue) bookmark of "where
  did I stop reviewing?" Opening the queue resumes at that position;
  no scroll-hunt to find the last spot. Cached per user; invalidated
  on queue rollover.
- **Override viewed status.** Reviewers can manually mark an asset
  as reviewed (advance the queue) OR mark previously-reviewed as
  unreviewed (re-queue it). Per-(user, queue, asset) state.
- **Count badge** on the navbar review button — count of
  not-yet-reviewed items in the current queue for this user.
- **Operator-tunable cadence** via system_config (default daily at
  the operator's site timezone if auto-rotation is enabled).
- Borrowed conceptually from an internal studio pipeline reference
  reviewed under the clean-room rules (ADR 0040); no code lift.
  Departs from that reference on policy: the reference rotates on cron
  unconditionally; AA defaults to opt-in to avoid surprising
  operators who didn't ask for it.
- Per ABC: queue contents cached; invalidated on rollover + on
  review completion + on viewed-status override.
- Per the cadence: soak-safe; no federation runtime involvement.

### Future ideas — from the internal review-mode reference (deferred, not scheduled)

These were surfaced from the internal reference review on 2026-06-18 and
are worth pulling forward when their parent surface lands. Not
scheduled as standalone sub-phases — they fold into adjacent work.

- **Hover info panel + right-click-to-pin** — when hovering a thumbnail
  anywhere (browse feed, playlist, collection view), show a large
  preview + metadata + annotation count. Right-click pins so it stays
  while you keep scrolling — explicit compare affordance. Fold into a
  future viewer/browse polish phase; not review-mode-specific.
- **Annotation count badges on thumbnails** — at-a-glance signal of
  which assets have feedback. Falls out of the annotations work in
  1.18.B-6 once annotations exist; small UI addition then.
- **Fullscreen "Artist View" with deep-link state persistence** — modal
  state survives refresh + can be deep-linked. Fold into AssetViewer
  polish later (extends 1.18.B-2 territory).

Explicitly rejected from the internal reference:
- **Save-and-next batch workflow** — friction point for artists;
  contradicts the minimize-artist-input principle. Artists want one
  page with little input.
- **Strip view of a playlist** — not on the roadmap.
- **Smart metadata field reordering on edit** — fragile UX that
  breaks operator muscle memory.

## Admin settings — fleshing out every placeholder

The admin shell currently has 13 sections; most have a real surface
in place, the rest are intentional stubs so the menu shape is stable.
This is the list of "make every placeholder a real surface" work,
roughly in order of practical value to operators.

### System — extant
- **Site**, **SMTP**, **Auth providers**, **AI providers** — real
  forms, persisted to `system_config`.
- **Appearance / Themes** — palette tokens + font slot picker.

### System — to flesh out
- **Previews** (Phase 1.18.C). Real admin UI for
  `sysconfig.PreviewConfig`: variant sizes, qualities, fit mode,
  default encoder preference (`auto` / `prefer-gpu` / `cpu-only`),
  storage limits, retention.
- **Jobs queue dashboard** (Phase 1.18.D). Live queue depth by
  type + status, recent failures with stack-trace expand, retry /
  cancel / requeue, manual enqueue, worker presence (incl. external
  farms + federated peers), backpressure controls.
- **System log** (Phase 1.20). Real audit-log surface (currently a
  stub). Filterable by actor, action, target, time. Export to CSV.
- **Storage backends** (Phase 1.19). Active backend, switch fs ↔ s3
  without downtime, content-addressed dedupe report, orphan
  cleanup, presigned-URL preview, capacity + cost dashboards.
- **Federation** (Phase 1.22). Peer instance registration, trust
  scopes, outbox / inbox status, conflict resolution.
- **Webhooks** (Phase 1.20). Outbound webhook endpoints, signing
  keys, retry policy, delivery log.
- **Notifications** (Phase 1.20). Notification rules + delivery
  channels (in-app, email, webhook). Rate limits.
- **Backups** (Phase 1.19). Backup destination config, schedule,
  retention, manual snapshot, restore flow.
- **Health probes** (Phase 1.20). Readiness checks per subsystem
  (DB, storage, encoder, federation), self-test buttons.

### Catalog / metadata — extant
- **Fields & metadata** — real list of `field_definitions`.
- **Workflow states** — real list of `workflow_states`.

### Catalog / metadata — to flesh out
- **Fields editor** (Phase 1.16). Create / update / archive fields
  from the UI; reorder; default values per resource type;
  required-on-upload toggle; resource-type scoping; field sets.
- **Workflow editor** (Phase 1.16). Build state machines: define
  states, transitions, required capabilities per transition,
  initial / terminal states, icon + color, requires-note flag.
- **Resource types** (Phase 1.16). Currently stub. Create resource
  types, attach fields, per-type variant set overrides, per-type
  workflow domain wiring.

### Identity / access — extant
- **Users** — list with role assignment.
- **Roles & capabilities** — read-only list.

### Identity / access — to flesh out
- **Users 2.0** (Phase 1.17). Bulk role assignment, deactivate /
  reactivate, force-password-reset, invite flow, last-active /
  session list, capability override.
- **Roles editor** (Phase 1.17). Create / update / delete roles,
  set capability sets, team-scoped roles, role hierarchy.
- **Teams** (Phase 1.17). Team tree, membership management, team
  hierarchy, team-scoped capabilities.
- **API token admin** (Phase 1.17). All tokens across the install,
  revoke any, token-kind = `worker` for external farm + federated
  workers (scoped to specific job types).
- **Audit-log filters** (Phase 1.20). Per-actor, per-action,
  per-target views; export.

### Operations — to add
- **Background jobs** dedicated section (Phase 1.18.D — see above).
- **Migration runner status** (Phase 1.20). Show every applied
  migration + checksum; run pending in maintenance mode.
- **Maintenance mode** (Phase 1.20). Banner + read-only flip,
  scoped exemption list.
- **Feature flags** (Phase 1.21). Sysconfig-backed toggles for
  in-flight features; per-user / per-team overrides.

### Integrations — to flesh out
- **Plugin manager** (Phase 1.23). Install / enable / disable
  plugins; permission grants per plugin; WASM sandbox config.
- **DCC client APIs** (Phase 1.18.B-9). Token + scope management
  for Maya / Blender / etc. clients hitting the review API.

### About / help — to flesh out
- **About**. Build info (commit + tag), license + attributions,
  release notes link, capability matrix.
- **Help & shortcuts**. Searchable doc embedded; full hotkey map
  per area (browse, review, admin).

## Up next

The phases queued behind the current focus, in build order:

- **Pre-MVP cleanup** (Phase 1.49). Foundational debt-clearing for
  the v0.1.0 release. Per [ADR 0046](/adr/0046-migration-baseline-and-squash-policy/),
  pre-MVP migrations MAY be squashed once into a single baseline;
  the append-only-forever trigger (v0.1.0 vs v1.0.0) is under
  review — see issue #228.
  Sub-phases:
  - ✅ 1.49.A — PanicShim consolidation (shipped)
  - ✅ 1.49.B — Legacy fallback drop (shipped)
  - ✅ 1.49.C-1 — DB schema audit report (shipped via PR #132;
    [`docs/cleanup-audit-2026-06.md`](https://github.com/Artist-Alley-Org/artist-alley/blob/dev/docs/cleanup-audit-2026-06.md);
    26 findings)
  - ✅ 1.49.C-2 — Migration baseline squash (shipped via PR #135
    `8c4922f`; 14 migrations → 1 baseline at
    [`00001_baseline_v0_1.sql`](https://github.com/Artist-Alley-Org/artist-alley/blob/dev/app/internal/db/migrations/00001_baseline_v0_1.sql);
    all 24 audit-derived edits applied; CI 7/7 green)
  - ✅ 1.49.D — Scrub product references from source (shipped; sole
    residual is one comparison in `app/internal/social/mention/doc.go`,
    tracked in issue #83)

  Gated the v0.1.0 release tag. Soak-compatible — the audit + the
  squash + the scrub all touch DB schema / docs / source-text only;
  the federation runtime is not affected.

- **Identity & teams** (Phase 1.17). Groups + team hierarchy,
  active session management, capability grants. Extended with: **table-level change tracking** with before / after
  diffs in the audit log (not just event-level), **user approval
  states** (pending / approved / disabled) with admin approval flow,
  **resource request workflow** (user asks for an asset →
  capability-holder approves or denies → notification fires →
  request lifecycle tracked, including auto-expiry).

- **Audit log & change tracking** (Phase 1.40). The central event log
  consolidating the audit-trail story currently scattered across
  Phases 1.17 / 1.20 / 1.21 / 1.24 / 1.26–1.28 / 1.32. Owns the event
  schema (actor + action + target + outcome + changeset before / after
  + metadata + correlation_id + license_kid), the dotted event
  taxonomy (`auth.*` / `user.*` / `asset.*` / `share.*` / `commerce.*`
  / `platform.*` / `system.*` / etc.), the capture API
  (`audit.Record(ctx, ...)` with buffered async writes — failure to
  record never fails the originating operation), the admin filter +
  search + detail + export surface at `/admin/audit`, retention
  policy (default 7 years, per-category overrides at Enterprise,
  enforced via the Phase 1.28 scheduled-action engine), and signed
  Enterprise export (Ed25519 over the JSONL payload, public key at
  `/.well-known/audit-signing-key` per ADR 0017). The
  `correlation_id` field links every event from a single request /
  job so cascade effects are reconstructable. Same features at
  Community + Pro; Enterprise adds the signed export, per-category
  retention overrides, and multi-instance audit-log federation. See
  ADR 0032.

- **Observability & operator telemetry** (Phase 1.41). Production-
  readiness layer: `/metrics` Prometheus endpoint with HTTP / DB /
  job-queue / storage / cache / session / license / federation /
  audit families; OpenTelemetry tracing via OTLP with auto-instrumented
  HTTP / DB / job / external-API spans (10 % sampling default,
  errors 100 %, W3C Trace Context propagation); structured-log
  shipping via opt-in forwarder providers (Loki / Datadog /
  Cloudwatch / Vector / Promtail / syslog); per-subsystem log
  levels hot-reloaded from admin + a `?log_level=debug` per-request
  escape hatch; admin live-tail log viewer at `/admin/logs/tail`
  via WebSocket; pre-baked Grafana dashboards in
  `infra/grafana/dashboards/` for HTTP / job queue / storage /
  federation / license / audit. Health endpoints stay at
  `/healthz` + `/readyz` with detailed per-subsystem status at
  `/admin/system/health`. Same features at Community + Pro; Enterprise
  adds priority ops support, vendor-specific OTLP exporter configs
  (New Relic / Honeycomb / etc.), and an SLA on production incident
  resolution. See ADR 0033.

- **Capability add-ons — registry + installer** (Phase 1.42).
  Out-of-band heavy components (CLIP / Whisper / Tesseract / Stable
  Diffusion / ComfyUI / Flux / future ML runtimes) ship as
  **first-party add-ons** pulled from a curated registry at
  `add-ons.artist-alley.org` — NOT baked into the binary. Sits
  between integrations (in-process Go) and future plugins (WASM):
  add-ons are heavy artefacts (Docker containers + model weights),
  optional, lifecycle-managed from a `/admin/capabilities` surface.
  Capability-slot model formalises the existing provider
  abstractions (image-embedding, transcription, OCR,
  image-classification, image-generation, image-inpaint, NSFW
  classification); multiple add-ons can satisfy a slot; operator
  picks the active provider. **No GPU gating** — operator hardware
  is operator hardware regardless of license tier; cloud-bridged
  add-ons (we host the inference) are the only tier-gated mode.
  Plugin-style YAML manifest format, plus
  artifact / resource / hosting / config fields. Digest-verified
  pulls; air-gapped operators mirror the registry. Resource
  requirements are advisory, not enforcing. Audit hooks fire
  `addon.*` into Phase 1.40; metrics expose under `addon_` prefix
  in Phase 1.41. Phase 1.42 ships the registry + installer + first
  three add-ons (CLIP for image embeddings, Whisper for
  transcription, Tesseract for OCR) so the surface is meaningful at
  v1. See ADR 0034.

- **Integrations** (Phase 1.18). Real LDAP / SAML / OAuth login
  flows, OAuth applications surface, outbound webhooks, notification
  rules + delivery channels. Extended with:
  **email template engine + send queue** with operator-editable
  templates, **timezone-aware delivery** (digest at 8 AM in the
  recipient's TZ), **event-scoped notifications** that link to a
  triggering record and auto-resolve when it resolves (e.g., a
  pending-request notification disappears when the request is
  approved), **conditional download terms** that show a per-resource
  terms page based on metadata (NDA acknowledgement, watermark
  warning, license summary), and a **dedicated webhook delivery
  worker** — outbound webhooks are queued through the existing job
  system with exponential-backoff retries, per-endpoint signing
  secrets, delivery-log admin surface for inspection + manual
  re-send, dead-letter handling after N failures, and a `webhook.*`
  audit category (Phase 1.40) for every fire / retry / drop. Chat
  platforms (Phase 1.30) plug in as one of the delivery channels
  alongside email / in-app / webhook.

- **Search 2.0** (Phase 1.12). Advanced search builder with
  field-level filters, saved searches, smart collections, synonyms +
  boosts, search analytics. Extended with:
  **saved-search alerts** (notify when results change), **endless
  scrolling** in result views as an alternative to pagination,
  **categorical / faceted refinement** with suggested-keyword
  refinements derived from the result set, **keyboard shortcuts**
  for power-user tagging / review (customizable in account
  settings).

- **Licensing & monetization** (Phase 1.24). Ed25519-signed `.lic`
  files, three tiers (Community 15 active seats / 50k assets, Pro 50
  / 500k, Enterprise unlimited + SSO + audit + multi-tenant + HA +
  priority support). Active-seats defined as `last_active_at` in
  trailing 30 days. Enforcement via tangled value derivation across
  consumers (search quota, upload concurrency, cache sizing, plugin
  gating, federation gating) rather than a single `if valid` gate —
  resilient to casual stripping without obfuscation or binary blobs.
  Cloudflare Workers for signing (private key in Worker Secrets);
  customer portal on Cloudflare Pages. Gated on Phase 1.17 (Identity
  & teams) because seat counting needs `last_active_at`. See ADR 0016
  + ADR 0017.


## On the map

Larger arcs sequenced by dependency + audience — the order here reflects when each becomes valuable, not strict gating:


- **Privacy & consent management** (Phase 1.32). Cookie / tracker
  banner that renders ONLY when a non-essential category is in use
  on the current instance (a bare studio install with no analytics
  shows no banner at all). Categories: essential, functional,
  analytics, third-party embeds. Data Subject Access Request (DSAR)
  tooling: machine-readable JSON export of all personal data,
  soft-then-hard deletion via the scheduled-action engine,
  reassignment of authored content to a `deleted-user` tombstone so
  collaborative content survives. Per-resource-type retention
  policies executed via the same scheduled-action engine. Editable
  `/legal/privacy` and `/legal/terms` markdown surfaces with a
  usable starting template (not legal advice). Privacy is not a
  paid differentiator — Community and Pro both get every feature;
  Enterprise adds non-repudiation signing on the audit-log export.
  See ADR 0024.

- **Share links** (Phase 1.26). Signed, expiring resource + collection
  + post share URLs for external collaborators (contractors,
  publishers, agencies, festival juries) who do not have accounts.
  URL is `GET /share/{token}` where the token is a 32-byte random
  identifier and the share-link row is the source of truth — no
  HMAC signing, so revocation is single-click. Each link binds a
  target (asset / post / collection), a scope (`view` /
  `comment` / `annotate` / `download`), optional `nbf` / `exp`,
  optional Argon2id password, optional max-use ceiling. Per-fetch
  audit trail records IP + user-agent + password result + scope
  satisfied. Downloads stream via presigned URLs on S3-style
  backends; the binary doesn't proxy gigabytes. See ADR 0018.

- **Bulk operations** (Phase 1.27). Multi-select edit / tag / delete /
  move-to-collection / state-transition across the browse feed and
  search results. Floating action bar with persistent selection
  across pagination + a "select all in filter" expansion mode.
  Destructive actions show preview counts and require a typed-count
  confirmation. CSV export of search results with operator-picked
  columns. Contact-sheet PDF generation with configurable grid +
  per-thumb metadata footer. All bulk actions submit as a single
  job-queue job (cancellable, resumable, progress streamed) with
  audit-log entries that preserve the selection IDs for batch undo
  where reversible. See ADR 0019.

- **Asset gating & NDA workflow** (Phase 1.28). Per-asset sensitivity
  tier (`public` / `team` / `restricted` / `embargo`); restricted
  and embargo assets are **server-baked blurred** in browse views —
  the blur is a real preview variant so even a network-tap leak is
  blurred. A "Reveal" button on the asset lifts the blur for the
  session and logs the reveal. Pairs with a generic scheduled-
  action engine (`change_sensitivity`, `restrict`, `delete`,
  `change_state`, `notify` actions on assets / posts / collections /
  users at a future timestamp), executed via the existing job queue.
  Common recipes: NDA expiry on a contractor auto-restricts every
  asset they uploaded; embargo → public flips at a marketing-
  supplied timestamp; trash auto-purges at 30 days. See ADR 0020.

- **Storage tooling** (Phase 1.19). Storage usage dashboard, orphan
  cleanup, checksum verification, dedupe UI, bulk re-import,
  backup + restore, database tools. Extended with:
  **scheduled integrity checks** with off-peak windows (verify the
  bytes on disk match the recorded hash, batched against a
  configurable nightly window), **tiered storage / offline archive**
  (move cold assets to S3 Glacier / tape via a per-bucket policy,
  fetch-on-demand surfaces), **per-download bandwidth tracking** so
  storage cost can be attributed to teams / users.

- **Reports & analytics** (Phase 1.20). Asset usage, user activity,
  storage trends, job performance, custom dashboards, scheduled
  reports, drafts, trash, activity log surfaces. Extended with: **custom SQL report builder** with placeholder
  variables (date range, team, user), **CSV / Excel / PDF export**
  per report, **report thumbnails** rendered server-side for the
  reports list, **per-collection download analytics**, **scheduled
  email delivery** of any report on a cron expression.

- **Community & moderation** (Phase 1.21). Reports queue, comment
  moderation, banned users / IPs, anonymous browse policy, rate
  limits, bookmarks. Extended with: **comment
  flagging with reason tracking** (spam, abuse, sensitive content,
  off-topic — configurable list), **email escalation to admins** on
  flagged content, **per-comment hide / restore** with audit trail,
  **activity stream surfaced as a structured comments thread** on
  the post-detail modal (system events become quoted-style entries
  alongside human comments — an activity-log-to-comments pattern),
  **structured support intake** — `/contact` form with
  categorization (bug / feature / abuse / account help / other) feeds
  into the same moderation queue with assignment, status tracking,
  and SLA timers; replaces the loose "email the admin" pattern with
  a queryable surface, and a **personal activity feed** on the user
  dashboard surfacing followed posts, recently viewed assets,
  bookmarks, pending notifications, and the user's own recent
  uploads + comments — pulled from the audit log (Phase 1.40)
  filtered to the user's visibility scope.

- **Announcements home widget** (Phase 1.37). Operator-authored
  announcements with audience scoping (`org` / `team:{name}` /
  `role:{name}`), severity tiers (info / notice / warning /
  critical), optional schedule (`nbf` / `exp`), pinning, and per-user
  read state. Homepage hero strip shows the top three active for the
  user; account dashboard tile shows the full list with read state.
  Auto-fed from system events — planned maintenance, license upgrade,
  failed scheduled jobs, federation peer offline — become
  announcements automatically. Chat platforms (Phase 1.30) mirror
  new announcements to the configured Slack channel. See ADR 0029.

- **Chat platform integrations** (Phase 1.30). Slack first, then
  Teams + Discord via the same provider abstraction. Outbound: rich
  Block-Kit-style messages for every notifiable event with
  deep-link buttons back into the post-detail modal. Inbound:
  `/aa search <query>`, `/aa post <id>`, `/aa upload` slash
  commands. Unfurl: Artist Alley URLs in chat get rich previews with
  blur respect for sensitive content. DM notifications opt-in via
  per-user Slack account connect. Admin surface defines per-event
  channel routing rules (`event:upload.completed in team:Concept Art
  → #concept-art-feed`). Chat is a delivery channel within the
  existing notification-rule subsystem from Phase 1.18 — not a
  separate engine. See ADR 0022.

- **External platform integrations** (Phase 1.29). Provider-
  abstraction layer for outbound publishing and inbound asset
  ingestion against Vimeo, YouTube, Adobe Creative Cloud Libraries,
  and Falcon.io. Each provider is a Go package in
  `app/internal/platforms/<name>/` implementing a common interface
  (publish / pull / sync / embed / webhook). Linked-asset model
  stores `platform_links[]` on the resource row so the post-detail
  modal shows "Published to Vimeo (in sync)" and flags drift /
  deletion. Pull-as-asset uses the same `origin_*` row flag that
  federation does — the storage layer doesn't care whether content
  came from a peer AA or an external platform. OAuth client secrets
  in Cloudflare Worker Secrets; per-studio refresh tokens encrypted
  at rest. License tier caps concurrent platform connections via the
  tangled-derivation model. See ADR 0021.

- **RSS / Atom feeds** (Phase 1.31). Standard syndication for
  collections, saved searches, user uploads, and tags so studios can
  wire Artist Alley into feed readers, Slack / Teams / Discord
  RSS connectors, and Zapier / n8n / Make / IFTTT triggers with no
  custom integration. URLs: `/feed/collection/{slug}.atom`,
  `/feed/search/{id}.atom`, `/feed/user/{handle}/uploads.atom`,
  `/feed/tag/{slug}.atom`. Atom primary, RSS 2.0 for compatibility.
  Per-user feed token in the URL for non-public content; revocable
  in account settings. Embargo + restricted items filter out of
  feeds entirely. Server-side cache 5 min with ETag + If-Modified-
  Since. See ADR 0023.

- **AI creative editing** (Phase 1.34). Provider-abstracted in-paint,
  out-paint, variations, and remove-background tools in the asset
  viewer's Creative tools panel. Providers: OpenAI (DALL-E +
  GPT-Image), Stability AI, ComfyUI local (for studios who keep
  pixels on-network). Generated images are **new assets** that share
  a `creative_lineage` relationship with the source — the source is
  never overwritten. The new asset's metadata records provider,
  prompt, seed, and parameters for both licensing and audit. License
  tier governs which providers are available + monthly token budgets.
  Community: ComfyUI local only. Pro: + OpenAI + Stability with
  caps. Enterprise: unlimited + bring-your-own API keys. See
  ADR 0026. **Status:** first realization shipped 2026-06-22 as
  Phase 1.14.E-1 (PR #156) — `img2img` via the ComfyUI MCP bridge
  using the ADR 0051 MCP-mediated path, `creative_lineage` table
  live (migration 00014), viewer trigger via `Img2ImgPopover`,
  bridge at `tools/comfyui-mcp-bridge/` with Flux Kontext example
  validated end-to-end. Remaining ops + mask UI ship in 1.14.E-2;
  OpenAI / Stability providers + tier gating in 1.14.E-3 (gated on
  licensing arc).

- **Artist Alley as an MCP server** (Phase 1.52). Expose the
  instance's asset catalogue via the Model Context Protocol so AI
  coding agents (Claude Code, Cursor, Codex, etc.) and creative
  agents can query and reason over a studio's archive the same way
  they query a codebase. Surface includes typed MCP tools for
  asset search by tag / sensitivity / collection, similarity search
  via the Phase 1.14.B CLIP embeddings, collection-context retrieval
  (asset list + metadata + relationships in one call), and
  multi-asset summarisation through the Phase 1.14.A provider
  abstraction. Authenticated via the existing API-token surface
  with a new `mcp.use` capability; per-tool capability gates so
  operators can expose only the surface they're comfortable with.
  Federation-aware: the MCP server can resolve cross-instance
  references via the existing federation actor URIs. Sibling
  concept to the [codebase-memory-mcp](https://github.com/DeusData/codebase-memory-mcp)
  pattern but for asset catalogues rather than source code. See
  ADR 0050. Depends on Phase 1.14.A (provider abstraction) +
  Phase 1.14.B (CLIP embeddings) shipping first.

- **Artist Alley as an MCP client** (Phase 1.53). The inverse of
  Phase 1.52 — instead of exposing AA's catalogue to external
  agents, AA's own AI orchestrator consumes external Model Context
  Protocol servers as tool sources. Adds `mcp_server` as a new
  provider kind to the Phase 1.14.A AI provider abstraction:
  operator registers an MCP server (URL + auth + capability
  scoping); its tools become callable from job handlers + workflow
  rules + admin actions. Validation references:
  [SceneWeaver's tessa-mcp / comfyui-mcp pattern](https://github.com/Artist-Alley-Org/artist-alley/blob/dev/docs/adr/0051-artist-alley-as-mcp-client.md)
  (each external service wrapped as a dedicated MCP server; AA
  orchestrates). Initial integration target: **ComfyUI MCP** for
  studios who run local image-generation infrastructure (clean
  complement to Phase 1.34's hosted image-edit providers). Generic
  MCP-server registration means any operator-built bridge plugs
  in without per-tool code in AA. Per-server + per-tool capability
  gates so operators control exposure precisely. Inherits the
  Phase 1.14.A audit + cost-tracking + privacy-routing
  infrastructure — every MCP tool call records to `ai_provider_call`
  with the MCP-server name as the "provider" + the tool name as
  the "model." Local-only by default; no MCP traffic federates.
  See ADR 0051. **Status:** foundation shipped 2026-06-21 as Phase
  1.53.A (PR #154) — registration tables + dispatcher 6-step guard
  chain + `mcp.client.{use,admin,images.read,images.write}` capabilities
  + health checker + JSON-RPC over HTTP provider + admin UI at
  `/admin/ai/mcp-clients`. First internal caller shipped 2026-06-22
  as Phase 1.14.E-1 (PR #156) — `aiedit` subsystem dispatching
  `img2img` through the bridge. ADR 0051 status flipped to `accepted`.

- **Featured collections & homepage curation** (Phase 1.35). Tree
  edges on the collection model (`collection_parent_id`, capped at
  depth 5) + a `featured` boolean with `team` / `org` / `public`
  scope. The same tree powers the team dashboard, the global signed-
  in homepage, and the anonymous public landing — one model, three
  audiences. Each featured node has an optional `hero_asset_id` (the
  card cover) and a `featured_order` integer for sibling sort.
  Cascade-publish flips a parent + children at once. Public featured
  collections respect per-asset sensitivity from ADR 0020 — an
  `embargo` asset shows only its title until embargo lifts. See
  ADR 0027.

- **Brand workspace** (Phase 1.33). Curated overlay on the
  collection + theme model: a `brand_kit` binds a set of canonical
  collections, an editable design-token export (CSS / Tailwind /
  JSON / Figma tokens v3), a markdown guidelines doc with inline
  asset blocks, per-asset usage rules (`internal-only` / `partner`
  / `press` / `public`), and a publish state. Public portal at
  `/brand/{slug}`. Token export at `/brand/{slug}/tokens.json` for
  automated pipelines. Brand-steward role is Pro+; everyone can
  read. Usage rules enforced through the Phase 1.18 conditional
  download terms. See ADR 0025.

- **PBR 3D viewer polish** (Phase 1.36). Right-side inspector in
  `ModelView.svelte` review mode: per-material list with hide / solo
  toggles, editable live PBR params (base color, metallic, roughness,
  normal strength, emissive) with the source asset never written,
  per-map texture preview that hand-offs to the texture inspector
  (Phase 1.18.B-16), curated 8-pack of IBL HDR environments + a
  project-HDR picker, A / B wipe between two materials on the same
  mesh (reusing the 1.18.B-8 A/B compare primitives). Override sets
  export as a structured `material_review.json` attached to the post
  — engineers ingesting reviews don't have to parse prose. See ADR
  0028.

- **Federation** (Phase 1.22). Federation v1 shipped (1.22.A-D wire
  protocol + inbox/outbox, encryption arc 1.22.I); phase-2 items are
  tracked in epic #287.

- **Plugin ecosystem** (Phase 1.23). WASM extension model via
  Extism. In-tree Go packages until external authors arrive.

- **Operator-configurable ad slots** (Phase 1.38). Opt-in surface for
  operators running public-facing community instances (fan sites,
  festival hubs, art-school portfolios) to monetize hosting via ads.
  Defined zones across feed top / between-every-Nth feed item /
  sidebar top + bottom / post-modal sidebar / footer; each zone is
  toggled and provider-bound per instance. Providers ship for Google
  AdSense, Meta Audience Network, Carbon Ads, EthicalAds, and a
  custom-HTML option. Default off everywhere; AAA-internal studios
  see zero ad markup. Operator allow-lists categories (block
  gambling / adult / political / whatever) + sets frequency rules
  (min N feed items between inline ads, max ads per page,
  time-of-day windows). Privacy compliance: ad slots auto-enable the
  third-party-embeds category in the cookie banner (ADR 0024) and
  do NOT load until consent. Per-user opt-out in account settings;
  Pro / Enterprise tiers may auto-suppress ads for paid users at the
  operator's option. **No revenue share** — operator ad income is
  theirs. **No anti-AdBlock measures** — blocked users get a clean
  view, no fight. Ad slots are sandboxed iframes with reserved
  dimensions to avoid CLS. All tiers — Community can monetize a
  public instance the same as Pro / Enterprise. See ADR 0030.

- **Commerce — Stripe + Shopify** (Phase 1.39). Operator-side commerce
  for selling content directly: digital downloads (concept-art packs,
  asset bundles, soundtracks), print-on-demand merchandise, physical
  originals, subscription access to WIP / patron tiers. Provider-
  abstracted with Stripe (direct, digital + physical + subscription),
  Shopify (full storefront for physical + tax + shipping), and
  Gumroad (lightweight digital + subscription) at launch. Listings
  attach to assets, posts, collections, or brand-kit items with
  pricing, delivery type, and a license-grant clause. Digital
  fulfillment reuses Phase 1.26 share links — buyer gets a single-
  use signed link with a 90-day expiry after payment. Subscription
  access expires via Phase 1.28's scheduled-action engine. Tax,
  refunds, disputes all defer to the payment processor. License-
  tier caps active-listing count (Community 5, Pro 500, Enterprise
  unlimited + organization-level Stripe / Shopify accounts).
  Per-instance public storefront at `/shop/{slug}` aggregates all
  active listings. **No revenue share** — operator's sales go to
  operator's bank; AA's monetization is per-tier license fees, the
  same stance as Phase 1.38 ads. Federated peers can surface remote
  listings read-only with a "buy on origin" CTA; federated checkout
  is out of scope. See ADR 0031.
## Things we deliberately aren't building

- A general-purpose DAM. We're shaped specifically for studio
  art-review workflows.
- A pipeline integration platform. We're a destination for finished
  and in-progress art, not a Perforce / Shotgrid replacement.
- A hosted-only SaaS product. We're self-hosted by design — a
  managed hosted Pro offering is on the table for indie studios who
  don't want to run their own infra, but the product remains
  fully self-hostable, and Enterprise customers self-host
  exclusively.

---

This roadmap is the **canonical** order — the labelled phase tags
inside the admin menu (the muted "Phase 1.X" pills on future tiles)
reference these same numbers. When the order changes, this file moves
first.
