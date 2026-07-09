# v0.1.0 readiness audit + ResourceSpace blueprint capture

**Status:** SNAPSHOT — this doc is the authoritative pre-v0.1.0 gap inventory
as of the phase 1.55.A audit (2026-07-07). It supersedes
[cleanup-audit-2026-06.md](./cleanup-audit-2026-06.md) as the go-to
"what's left" reference and captures the ResourceSpace (RS) blueprint
patterns still cited by the codebase so the gitignored `/dbstruct/`,
`/include/`, `/plugins/`, `/pages/` reference tree can be safely deleted
in a follow-up PR without losing design context.

**Doc history:** originally shipped as `v1_readiness.md` (2026-07-07,
PR #220). Renamed to `v0_1_readiness.md` on 2026-07-08 (PR #<pending>,
phase 1.55.R) after the user clarified two release milestones — see §0
below. The substance is unchanged; the milestone naming shifted.

**Purpose:** unblock two follow-up arcs simultaneously —
1. the physical deletion of the RS reference tree (per
   [feedback_rs_is_a_blueprint](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/feedback_rs_is_a_blueprint.md)
   we treat RS as a blueprint reference; once every load-bearing pattern
   is captured here, the physical refs are dead weight);
2. the v0.1.0 release tag (the first-ever tag — see §0 for milestone
   semantics). Per [ADR 0046](./adr/0046-migration-baseline-and-squash-policy/)
   the trigger for append-only-forever migrations is under review
   (v0.1.0 vs v1.0.0 — see issue #228); per
   [project_license_direction](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/project_license_direction.md)
   the AGPL + commercial relicense gates on Phase 1.24 and — per issue
   #229 — the v0.1.0 tag alignment is a soft decision.

**How to read this doc:**

- §0 defines the two release milestones (v0.1.0 = first tag; v1.0.0 =
  out of beta) — read this first; every subsequent milestone reference
  anchors here.
- §1 defines the v0.1.0 exit criteria — what must be true to ship
  the first tag.
- §2 summarises the arc-close velocity of the last three weeks so the
  reader has an anchor for "how much is left" vs "how much just shipped."
- §3 lists shipped gaps (from the RS-gap audit and subsequent work) so
  the open list in §4 is the true remainder.
- **§4 is the meat** — one entry per open gap, each with a mandatory
  substructure: status, RS blueprint, modern research, caching sketch,
  federation implications, implementation sketch, effort, sequencing.
- §5 lists items explicitly deferred to post-v0.1.0 with rationale.
- §6 is the RS reference inventory as a compliance table — every row
  must resolve to "delete-safe" before the physical rm-rf PR opens.
- §7 proposes a sequencing order for the open gaps.
- §8 is the RS-deletion readiness checklist.
- §9 points at the arcs that unblock after v0.1.0 vs after v1.0.0.

**Research depth disclosure per §4 entry.** Each gap flags its research
depth as `deep` (2-4 real citations with URLs, sourced during this
audit), `medium` (structural sketch + 1-2 anchoring references), or
`light` (RS blueprint captured but external research deferred to
implementation-time briefs). Per the brief's operational note, research
per gap is time-boxed; a `light` flag is not a defect — it's an honest
signal that the RS pattern is well-understood and the modern-approach
research should happen with the implementer holding real context, not
during audit.

---

## 0. Milestone model — v0.1.0 vs v1.0.0

Two release milestones, explicit definitions. Every subsequent milestone
reference in this doc anchors here.

### v0.1.0 — first tagged release

**Marker:** ResourceSpace reference tree (`/dbstruct/`, `/include/`,
`/plugins/`, `/pages/`) deleted; base feature set complete per §4
sequencing.

**Enables:**

- The relicense arc (per [ADR 0016](./adr/0016-license-direction/) →
  [ADR 0017](./adr/0017-monetization-and-licensing/)) — v0.1.0 alignment
  is under review (see issue #229).
- The monetization arc (Phase 1.24) — first paying customers.
- First real installs against the tagged codebase.

**SemVer semantics:** the 0.x range still permits minor-version schema
breaks. Feature-add is the norm; breaking changes remain possible but
should be documented in release notes.

### v1.0.0 — out of beta

**Marker:** real production usage, a soak period, release-worthy
quality. Not a next-week event — v1.0.0 sits somewhere past v0.1.0 plus
real-user validation.

**Enables:** SemVer compatibility promises. Per [ADR 0046](./adr/0046-migration-baseline-and-squash-policy/)
this MAY be the correct trigger for migrations-append-only-forever —
under review (see issue #228). The alternative is that append-only
kicks in at v0.1.0 (stricter reading; SemVer 0.x in this codebase
promises schema stability).

### What this recalibration changed

This doc originally shipped (2026-07-07, PR #220) framing everything as
"v1.0.0 readiness." The user clarified on 2026-07-08 that "the release
we're building toward right now" is v0.1.0, not v1.0.0. Every mention
in this doc of "the release" or "the first tag" refers to v0.1.0. The
handful of genuinely-later "out of beta / SemVer promises" mentions
stay v1.0.0 (see §9). The two ADR-level semantic questions above are
tracked as follow-ups (#228, #229) rather than decided here.

---

## 1. v0.1.0 exit criteria

The v0.1.0 tag is a **first-release contract** — everything on `dev` up
to that point is provisional; everything after treats schema, API surface,
and installer contracts as promises. The specific criteria:

### 1.1 All in-scope gaps closed or explicitly deferred

Every open gap in §4 either ships in a labelled pre-v0.1.0 phase OR gets
an explicit "deferred to post-v0.1.0" entry in §5 with rationale that
survives review. No shipping "we'll figure it out later" — the doc
records the decision either way.

### 1.2 Every RS reference resolved ✅ SHIPPED (Phase 1.55.S)

Every RS blueprint pattern is either (a) fully captured in §4's gap
entry, (b) captured in a shipped ADR, or (c) explicitly abandoned as
"we don't do it that way." The compliance signal is the §6 inventory
table with zero rows in the `delete-safe? NO` column.

**Shipped 2026-07-08 via Phase 1.55.S** — application code + config
files scrubbed of RS mentions; three obsolete `scripts/rs-*` tooling
files deleted; local reference tree (`/dbstruct/`, `/include/`,
`/plugins/`, `/pages/`, etc.) physically removed from disk (safety-net
`.gitignore` entries retained); ADR 0001 flipped to
`superseded-by: 0040`.

### 1.3 Every ADR marked accepted or superseded

Per [ADR 0035 conventions](./adr/0035-adr-conventions/), every ADR
lifecycle status flag matches its actual state. In particular:

- [ADR 0001](./adr/0001-hard-fork-from-upstream-trunk/) — flipped
  to `superseded-by: 0040` in Phase 1.55.S (2026-07-08). The physical
  reference tree has been deleted; the fork is now historical.
- [ADR 0003](./adr/0003-strangler-fig-internal/) — per the
  [strangler-fig-abandoned memory](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/project_strangler_fig_abandoned.md)
  the strangler-fig approach was formally abandoned; the ADR should be
  marked `superseded` before v0.1.0.
- [ADR 0015](./adr/0015-php-as-legacy-backend/) — same. PHP is now
  reference-only, not a legacy backend to strangle.
- [ADR 0016](./adr/0016-license-direction/) — status must flip from
  `proposed` to `accepted` (or superseded by 0017) before v0.1.0 so the
  license direction is committed at the tag. Timing alignment tracked
  in issue #229.

### 1.4 Migration baseline validated

Per [ADR 0046](./adr/0046-migration-baseline-and-squash-policy/), the
baseline squash to `00001_baseline_v1.sql` is a **prerequisite** for
v0.1.0. That work is complete + verified by `scripts/verify-baseline.sh`
which reports `baseline verified against 28 append migrations, head=00029,
ready for tag.` The append-only-forever trigger (v0.1.0 vs v1.0.0) is
tracked separately in issue #228.

### 1.5 License direction finalised

Per ADR 0016 (proposed) → 0017 (proposed), the AGPL + commercial
relicense is planned but not accepted. Before v0.1.0, one of these must
be true:

- The relicense is executed (0016/0017 → `accepted`; `LICENSE` file
  updated; `NOTICES.md` updated); OR
- The relicense is explicitly deferred to a labelled post-v0.1.0 phase
  (e.g., 1.24) with a decision doc explaining why v0.1.0 ships under
  the current BSD-3-Clause. Timing tracked in issue #229.

### 1.6 CI + release pipeline health check ✅

Per [project_release_pipeline](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/project_release_pipeline.md):

- The nightly Playwright dogfood (self-hosted runner) has run green
  three days in a row without carve-outs. (One green banked as of
  2026-07-07 per the 1.54.D shipped memory.)
- The `Edge image` + `Verify production image build` workflows run on
  every `dev` push without drift (no oapi-codegen version churn like
  the pre-existing v2.7.1 → v2.7.2 drift caught by PR #217).
- The signed-release tooling has been dry-run against `v0.0.99-rc*`
  tags to verify GHCR + Docker Hub publish, cosign signing, and the
  auto-generated release-notes page.

**Verified 2026-07-09 via `v0.0.99-rc9` all-green end-to-end**
(workflow run [29031874875](https://github.com/mscrnt/artist-alley/actions/runs/29031874875)).
Two-job pipeline fires on tag push and completes in ~15 minutes:

- **`release-notes` job**: checkout → resolve tag → `gh release create
  --generate-notes` with auto-set `--prerelease` for tags carrying a
  `-` suffix and `--notes-start-tag` seeded from the prior tag when
  one exists.
- **`docker` job**: buildx sets up QEMU + registry auth (GHCR +
  Docker Hub) → multi-arch build+push (linux/amd64 + linux/arm64) with
  provenance + SBOM attestations → Sigstore keyless cosign signing
  via OIDC. Cross-arch cgo works via `gcc-aarch64-linux-gnu` +
  `libwebp-dev:arm64` added to the go-build stage; per-arch CC +
  PKG_CONFIG_PATH selected at build time.

**Verified end-to-end:** cosign verify green against
`mscrnt/artist-alley:0.0.99-rc9` (signature Subject includes the
workflow ref + repo + commit sha; transparency-log entry confirmed
offline). Docker Hub manifest confirms both `linux/amd64` + `linux/arm64`
plus SBOM + provenance attestation manifests.

**Path to 1.55.T-2 close:** trimmed `.goreleaser.yaml` (5 iterations
against goreleaser's release-notes-only wall) → deleted goreleaser
entirely for `gh release create --generate-notes` (cleaner fit for
Docker-only distribution). Full binary + package + Homebrew
distribution deferred to v1.0.0 via **#242** (hard prerequisite).

### 1.7 Documentation surface validated

- `README.md` reflects a v0.1.0 install path (Docker compose one-liner)
  rather than the current developer-mode instructions.
- The operator install guide walks a fresh operator from `git clone`
  → running-instance in under 30 minutes without any "this is a WIP"
  disclaimers.
- The API reference at `/site/src/content/docs/reference/` regenerates
  cleanly from the current `openapi.yaml`.
- The upgrade-path doc explains how a v0.1.0 operator will move to
  v0.2 (or, per issue #228 outcome, whether 0.x still allows schema
  breaks). Note SemVer 0.x semantics: minor-version breaks are
  permitted by the spec even if this project chooses to promise
  otherwise.
- ADR index page (`/site/src/content/docs/adr/`) auto-regenerated and
  renders every ADR without a warning.

---

## 2. Arc-close velocity summary (2026-06-22 → 2026-07-07)

Two-and-a-half weeks of intensive arc-close activity. Recorded here so
the reader can calibrate the remaining work against actual delivery
cadence.

| Phase | PR | Title | Date |
|---|---|---|---|
| 1.18.A-2 core | #158 | Upload-side EXIF + ICC + orientation extraction | 2026-06-22 |
| 1.18.A-2 PR-A | #159 | Per-user dedup unique constraint + race-loser path | 2026-06-23 |
| 1.18.A-2 PR-B | #160 | Admin UI + backfill + observability for extraction | 2026-06-23 |
| 1.19.A-1 | #161 | Email substrate — SMTP-at-rest + notif job + admin test | 2026-06-23 |
| 1.19.A-2 | #162 | Admin impersonation | 2026-06-24 |
| 1.19.B | #163 | Self-service TOTP 2FA | 2026-06-24 |
| 1.19.C | #164 | Self-registration + email verification | 2026-06-25 |
| 1.54.A | #165 | IIIF Image API 3.0 Level 0 | 2026-06-25 |
| 1.18.A-3 | #166 | IPTC + XMP extractors | 2026-06-26 |
| 1.16 edit-safety | #167 | Optimistic-concurrency edit-safety | 2026-06-26 |
| 1.18.A-3.B | #172 | RAW camera embedded thumbs + PDF page count | 2026-06-28 |
| 1.16.B-1 | #174 | Search foundation + unified `/search` + tsvector | 2026-07-01 |
| 1.16.B-2 | #176 | Facets + advanced DSL + autocomplete + save-as-collection | 2026-07-01 |
| 1.16.B-3 | #178 | Vector search + hybrid ranking + `similar_to:<uuid>` | 2026-07-01 |
| 1.16.B-4 | #180 | Saved searches + digest coordinator | 2026-07-02 |
| 1.16.B-5 | #182 | Reindex admin + disk-usage + dashboard | 2026-07-02 |
| 1.54.B | #187 | IIIF Presentation API 3.0 + Content Search 2.0 | 2026-07-03 |
| 1.19.D | #198 | Per-username account lockout | 2026-07-04 |
| 1.16.B-3-followup | #199 | CLIP visual encoder + reverse-image search | 2026-07-05 |
| 1.16.B-3-followup-4 | #205 | Admin visual-embedding backfill trigger | 2026-07-05 |
| 1.16.B-3-followup-2 | #206 | Async visualembed upload-hook job | 2026-07-06 |
| 1.16.B-5-followup | #208 | Search feedback loop | 2026-07-06 |
| 1.16.B-followup | #213 | `visibility.Filter` retrofit | 2026-07-06 |
| 1.16.B-followup-2 | #215 | AdminBackfillPanel extraction | 2026-07-06 |
| 1.16.B-followup-3 | #217 | `search.Counter` split | 2026-07-07 |

**Twenty-five PRs in seventeen days.** Foundation work is essentially
complete; the remaining work is the "boring fundamentals" tier +
finish-line hygiene.

---

## 3. Shipped gaps (RS-gap audit + subsequent work)

Anonymous list of items from the RS-gap audit ([memory
project_rs_gap_audit_2026_06_22](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/project_rs_gap_audit_2026_06_22.md))
and follow-ups filed since, now shipped and no longer counting toward
the open remainder.

### Shipped from the three master-plan arcs

- Account lifecycle: self-registration (PR #164), email verification
  (PR #164), password reset (PR #164), admin impersonation (PR #162),
  welcome email (PR #164), email templating library (PR #161), TOTP 2FA
  (PR #163), per-username lockout (PR #198), audit event schema (multi-PR).
- Upload-side metadata extraction: EXIF/ICC/orientation (PR #158);
  per-user dedup (PR #159); admin UI + backfill (PR #160); IPTC/XMP
  (PR #166); RAW + PDF (PR #172).
- Search + edit-safety: optimistic-concurrency edit-safety (PR #167);
  unified `/search` foundation (PR #174); facets + DSL + autocomplete
  (PR #176); save-as-collection (PR #176); vector search + hybrid
  ranking (PR #178); saved searches (PR #180); reindex admin + disk
  usage + dashboard (PR #182); CLIP visual encoder + reverse image
  (PR #199, PR #205, PR #206); feedback loop (PR #208).

### Shipped IIIF interop

- IIIF Image API 3.0 Level 0 (PR #165).
- IIIF Presentation API 3.0 (PR #187).
- Content Search 2.0 (PR #187).
- 2.0 → 3.0 URL redirect (PR #187).
- Go dual-mount for external `/iiif/{2,3}/*` (PR #194).
- Automated Mirador dogfood on nightly (PR #195).

### Shipped hygiene (this week)

- `visibility.Filter` retrofit (PR #213) — closed #185.
- `AdminBackfillPanel` extraction (PR #215) — closed #186.
- `search.Counter` split (PR #217) — closed #209.
- MDX build reliability (fix inline with PR #213).

### Shipped from A-tier list originally flagged 2026-06-22

- Smart collections — shipped as part of 1.16.B-2 saved-searches
  (`collections.smart_query` column + backing execution path).
- Search history (client-side localStorage) — shipped 1.16.B-1.
- `/notifications/{id}/read` endpoint — present in
  `app/api/openapi.yaml` (route confirmed by grep).

---

## 4. Open gaps within scope of v0.1.0 — the meat

Each entry has: **Status**, **Roadmap phase**, **ResourceSpace
blueprint**, **Modern gold-standard research**, **Caching strategy**,
**Federation implications**, **Target implementation sketch**, **Effort
estimate**, **Sequencing recommendation**, and a **Research depth**
flag.

Every gap is either _blueprinted-not-shipped_ (RS has the pattern; we
have not started), _partial_ (some pieces shipped; contract not
complete), or _in-flight-elsewhere_ (already assigned to a labelled
roadmap phase; captured here for sequencing context only).

---

### 4.1 Job DLQ + admin review surface

**Status:** blueprinted-not-shipped. Research depth: `medium`.
**Roadmap phase:** unclaimed. Suggested: 1.17.J-1.

**ResourceSpace blueprint.**

- Files: `/include/job_functions.php`, `/pages/team/team_jobs.php`,
  `/pages/team/team_jobs_edit.php`.
- Pattern summary. RS stores background jobs in a `job_queue` table
  keyed by `ref` with columns `job_type`, `job_data` (JSON), `success_text`,
  `failure_text`, `status` (`INACTIVE`/`ACTIVE`/`COMPLETE`/`FAILED`/
  `INPROGRESS`), `start_date`, `time_created`, and a `job_code` (for
  cancellation via URL). The pattern that matters for our purposes is
  the **status transition to `FAILED` plus retention of the
  `failure_text`** and the **admin surface at `/pages/team/team_jobs.php`**
  that filters by status (default: FAILED + ACTIVE) with per-row Retry
  and Delete actions. Retry copies the row back to `INACTIVE` with a
  fresh `start_date`. Delete is a hard delete. Failure text is a long
  string; RS truncates in the list view and pops a modal on click.
- What RS does well. Simple state-machine on a single table; every
  admin action is one SQL statement; no separate DLQ storage — the same
  table holds the failed rows and the admin surface just filters. This
  is the pragmatic minimum.
- What RS does badly / what we depart from. No exponential backoff
  policy; retry is manual only. No per-error-class routing (RS's
  `failure_text` is opaque). No dashboard — operator must know to
  filter for FAILED. No metric emission for "queue depth" or
  "failure rate" over time. We should have all four.

**Modern gold-standard research.**

- Sidekiq's dead-set retention pattern
  ([Sidekiq docs](https://github.com/sidekiq/sidekiq/wiki/Error-Handling))
  — after `max_retries` (default 25 with a truncated exponential
  backoff), the job moves to a persistent "dead set" that never
  auto-retries; operator manually replays. The dead set has a size
  ceiling and TTL. This is the canonical "DLQ" shape and closest to
  what our operators will expect.
- Postgres LISTEN/NOTIFY as the retry-scheduler kick — RiverQueue
  ([River docs](https://riverqueue.com/docs/rate-limiting)) uses the
  jobs table plus a periodic scheduler goroutine plus LISTEN/NOTIFY
  for immediate wake-up, which matches our existing jobs
  package's substrate.
- Prometheus recording rules for queue health — patterns like
  `job_queue_depth{status="failed"}` gauge and `job_failure_rate` histogram
  over 1-hour buckets are the industry-standard dashboard shape.

**Caching strategy.**

- What: the admin dashboard's failed-count aggregate (per subsystem,
  per error class) and the "last 10 failures" table.
- Key shape: `admin.jobs.dlq:count:<subsystem>:<last_hour>` and
  `admin.jobs.dlq:recent:<subsystem>`.
- TTL: 30 seconds — matches other admin dashboard cache TTLs.
- Invalidation: on write path (retry, delete, new failure), via
  `cache.Registry.Invalidate("jobs")`; LISTEN/NOTIFY broadcasts to
  peer instances.
- Federation-safety: cache is per-instance. Peer instances see their
  own DLQ, not a global one — jobs never cross instance boundaries
  today.

**Federation implications.**

- Per-instance forever, unless "federated remote workers" ([memory
  project_federated_remote_workers](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/project_federated_remote_workers.md))
  ships. When that lands, a "worker" on peer A executing a job for
  peer B's queue would surface the failure on peer B's DLQ, not
  peer A's. Design the DLQ row schema to carry `origin_server_id`
  (nullable, defaults to self) from the start so the future federated
  case is a data-population addition, not a schema migration.

**Target implementation sketch.**

- New Go package `app/internal/jobs/dlq/` with a Store type and a
  Handler. Store queries the existing `jobs` table (no new table —
  status `failed` is the source of truth, matches RS's single-table
  approach). Handler mounts three admin routes on the chi router:
  `GET /admin/jobs/failed?subsystem=<name>&limit=N`,
  `POST /admin/jobs/failed/{id}/retry`,
  `DELETE /admin/jobs/failed/{id}`.
- Reuse existing `admin` capability gate for all three.
- New `RetryFailedJob(id)` method on `jobs.Service` — copies payload to
  a new row with `attempts=0`, deletes the old row inside one Tx.
- Dashboard tile: reuse the `AdminBackfillPanel.svelte` shell from
  PR #215 with columns `Subsystem / When / Type / Last error /
  Actions`. Poll cadence 5s.
- Optional: an "auto-mark-dead-after-N-attempts" ceiling — the jobs
  framework's `max_attempts` (default 3) already does this;
  visible in dashboard as `job.max_attempts_reached_total` counter.
- Explicit "clean-room departure from RS" — we get exponential backoff
  from the existing `lease_expires_at` mechanism plus a `jobs.RetryDelay`
  helper; RS has no backoff.

**Effort estimate.** 1-2 day arc. Store + 3 admin routes + frontend
tile all mechanical.

**Sequencing recommendation.** Independent; best-paired with 4.2
(job cancellation endpoint) since they share the same admin surface.

---

### 4.2 Job cancellation endpoint

**Status:** partial. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: paired with 4.1.

**ResourceSpace blueprint.**

- Files: `/pages/team/team_jobs_edit.php`, `/include/job_functions.php`.
- Pattern summary. RS has a `job_cancel($ref)` function that sets
  `status='CANCELLED'` (an enum value we already have) and lets the
  next worker tick short-circuit on load. Admin action is a POST to
  `/pages/team/team_jobs_edit.php?action=cancel&ref=<n>` behind a
  cap check.
- What RS does well. Simple flip; the cancel signal is polled by the
  worker at whatever cadence the job's own inner loop checks. Fits our
  existing worker architecture.
- What RS does badly / what we depart from. No idempotency — repeated
  cancels create duplicate audit rows. No "graceful vs abort" split —
  RS conflates operator "stop this job now" with worker "job encountered
  cancel signal at next tick."

**Modern gold-standard research.**

- Kubernetes controller-runtime pattern: cancellation via
  `context.Context` propagation from the parent scheduler to per-job
  goroutines. Works within a process; doesn't work across worker
  reboots — which is why the DB-flag approach is still needed.
- River's `Job.Cancel` API
  ([River docs](https://riverqueue.com/docs/cancelling-jobs)) —
  library-level cancel that flips a `state='cancelled'` column and
  emits an event.

**Caching strategy.** No cache needed. Cancellations are rare + the
subsequent DB update is the source of truth.

**Federation implications.** Per-instance. Cancellation of a job whose
future worker might be a peer (per federated remote workers) needs to
carry over the `origin_server_id`; peer worker checks cancellation
status via inbound `Cancel` activity before starting work. Defer
that to the federated-workers phase.

**Target implementation sketch.**

- New route `POST /admin/jobs/{id}/cancel` on the existing jobs admin
  handler.
- Uses existing `jobs.Service.Cancel(ctx, id)` (if it exists; otherwise
  add it) which does `UPDATE jobs SET status='cancelled' WHERE id=$1
  AND status IN ('pending','running') RETURNING id`.
- Audit event `admin.jobs.cancelled`.
- Frontend Cancel button on the DLQ table row.

**Effort estimate.** Half-day.

**Sequencing recommendation.** Paired with 4.1 (single PR).

---

### 4.3 Sysconfig admin UI with type-aware widgets

**Status:** partial. Research depth: `medium`.
**Roadmap phase:** unclaimed. Suggested: 1.17.K-1.

**ResourceSpace blueprint.**

- Files: `/pages/team/team_setup.php`,
  `/pages/team/team_system.php`,
  `/include/config_functions.php`,
  `/languages/*.yaml` for widget label strings.
- Pattern summary. RS's `system_config` table stores flat
  `key`/`value`/`type` rows where `type` is one of
  `text|number|boolean|json|password|enum|multiline`. The admin page
  reads config with `get_config()`, iterates a manifest of
  editable keys grouped by section, and renders per-type widgets:
  text input for `text`/`number`, checkbox for `boolean`, dropdown for
  `enum` (options from a companion manifest), password-masked input
  for `password` (values write to at-rest-encrypted storage), textarea
  for `multiline`/`json`. Save uses a POST that runs each key through
  the type-appropriate validator. All setting sections have an "Edit
  history" link that shows the last 20 changes to the section's keys
  from an audit log.
- What RS does well. Widget-per-type is exactly right — operators
  edit a text field for a URL, a checkbox for a flag, a dropdown for
  an enum, and a masked input for a secret without the frontend having
  to reinvent the mapping each time. Grouping by section (`Interface`,
  `Users`, `Files`, ...) makes navigation obvious.
- What RS does badly / what we depart from. Manifest is a giant PHP
  array — search and diff are painful. No schema validation — the
  frontend trusts the type field, so a code change on the backend that
  bumps a key's type from `text` to `enum` silently corrupts the UI
  until the manifest is updated by hand. We should validate schema
  server-side.

**Modern gold-standard research.**

- HashiCorp Vault's UI for KV secrets
  ([Vault docs](https://developer.hashicorp.com/vault/docs/ui)) uses a
  JSON schema per secret path to render the edit widgets; changes go
  through server-side validation before write. Overkill for our
  scale but the "declare the schema server-side, render the widgets
  client-side" split is right.
- SvelteKit form actions
  ([SvelteKit docs](https://svelte.dev/docs/kit/form-actions))
  as the render-side pattern — one action per section, receives typed
  form data, applies server-side validation, returns a typed error
  set the widget can render.
- Postgres row-level security for per-key ACL is overkill for us; we
  use the existing `system.admin` capability.

**Caching strategy.**

- What: the current sysconfig snapshot per session; the "sections and
  widgets" manifest (which is essentially static per boot).
- Key shape: `sysconfig.snapshot:v<hash>` for the values;
  `sysconfig.manifest` cached once at boot.
- TTL: values invalidate on write path via `cache.Registry`; the
  manifest is refresh-on-boot.
- Federation-safety: per-instance. Sysconfig values are per-instance
  today and stay that way.

**Federation implications.** Per-instance forever. Config is
operator-scoped; a peer's config doesn't apply to us.

**Target implementation sketch.**

- Backend: extend `sysconfig` package with a `SchemaFor(section)`
  method that returns a typed manifest
  (`[]SysconfigField{Key, Type, Group, Label, Description, Required, Enum, Widget}`).
  Reads from a hand-written Go map so operators don't have to touch
  YAML/JSON to add a new field.
- New routes `GET /admin/system/config/{section}/schema` +
  `PUT /admin/system/config/{section}` (bulk-set values); reuse
  existing sysconfig `Store.Set` per-key.
- Frontend: new SvelteKit page `web/src/routes/admin/system/config/[section]/+page.svelte`
  that fetches the schema, renders widgets per `SysconfigField.Type`,
  submits via SvelteKit form action.
- New widget components at `web/src/lib/components/admin/sysconfig/`
  — `TextField.svelte`, `BooleanField.svelte`, `EnumField.svelte`,
  `PasswordField.svelte`, `JSONField.svelte`.
- Edit history: reuse the existing audit-events surface (each key
  change fires an `admin.system.config_updated` event with prev/new
  in metadata; the audit query at
  `/admin/system/audit-log?event_type=admin.system.config_updated&subject=<section>`
  is the "history" view).
- Explicit clean-room departure from RS: schema declared in Go code,
  not PHP arrays; frontend widgets are typed Svelte 5 components;
  audit history reuses existing audit surface, not a bespoke table.

**Effort estimate.** 2-3 day arc (widgets + backend schema + audit
plumbing).

**Sequencing recommendation.** Independent but high operator-visible
value. Best paired with 4.4 (schema mismatch boot detection) since
both touch sysconfig plumbing.

---

### 4.4 Schema mismatch boot detection

**Status:** blueprinted-not-shipped. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: 1.17.K-2 or 1.49.C-3.

**ResourceSpace blueprint.**

- Files: `/include/dbmigrate.php`, `/pages/setup/upgrade.php`.
- Pattern summary. On every boot, RS reads the current schema version
  from a `sysvars` row keyed `schema_version`, compares against the
  bundled migrations list, and either (a) auto-migrates if the delta
  is a known sequential set of upgrades, (b) refuses to boot with a
  banner "run the upgrade script" if the delta is ambiguous, or (c)
  runs to completion but flashes a red banner if the schema is _newer_
  than the code expects (indicates a rollback situation).
- What RS does well. Explicit refusal-to-boot on ambiguous deltas
  prevents "silently running against a schema you don't know about."
- What RS does badly / what we depart from. RS's auto-upgrade is
  optimistic — every user gets to run the migration on production
  data; there's no explicit "run migrations" step. Our goose-backed
  migration approach already separates "run migrations" from "start
  serving," so the boot check just needs to be the newer-schema
  refusal.

**Modern gold-standard research.**

- Django's `migrate --check`
  ([Django docs](https://docs.djangoproject.com/en/5.1/ref/django-admin/#cmdoption-migrate-check))
  returns non-zero if unapplied migrations exist, halting the deploy
  before the app boots. Same pattern for us: refuse to serve until
  `goose status` is clean.
- Kubernetes readiness-gate pattern — the pod's readiness probe fails
  until the boot check passes; the load balancer routes around it.
  Fits our compose stack too.

**Caching strategy.** No cache. This is a boot-time single check.

**Federation implications.** None. Each peer's schema is
per-instance.

**Target implementation sketch.**

- New Go function `db.CheckSchemaFreshness(ctx, pool) (Status, error)`
  where Status is one of `{ok, unapplied_migrations, unknown_newer_schema}`.
  Called from `cmd/aa/main.go` right after `goose.Up`.
- Response: if `unapplied_migrations`, log an ERROR and refuse to
  start the HTTP server (return non-zero from main). If
  `unknown_newer_schema`, log a WARN + flash a banner on
  `/admin/system/health`.
- `unknown_newer_schema` detection: compare the max
  `goose_db_version.version_id` against the highest number in the
  embedded migrations FS. If the DB is ahead, we're running old code
  against new schema.

**Effort estimate.** Half day.

**Sequencing recommendation.** Independent; small enough to fold into
another PR opportunistically.

---

### 4.5 @-mention notifications wired to notify

**Status:** partial. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: 1.17.G-3.

**ResourceSpace blueprint.**

- Files: `/include/message_functions.php`, `/pages/user/messages.php`.
- Pattern summary. RS parses `@username` mentions out of message and
  comment bodies with a regex, resolves to `user.ref`, and fires an
  entry in the `user_message` table which powers the notifications
  bell. Regex is `~@(\w+)~`. Silently drops unknown usernames.
- What RS does well. Cheap regex; single-table storage; obvious UX.
- What RS does badly / what we depart from. No "un-mention" audit
  when the mention is edited out. No mute-user-mentions preference.
  No cross-instance federated mentions.

**Modern gold-standard research.**

- Mastodon's mention resolution pipeline
  ([Mastodon dev docs](https://docs.joinmastodon.org/spec/microformats/))
  — parses `@user@instance` with an optional-instance suffix, resolves
  locals first, then federated actors via WebFinger. Fully applies to
  us once federation ships (see [ADR 0043](./adr/0043-federation-walled-garden-protocol/)).
- Textual matching should exclude code fences and links per Slack's
  behaviour (people paste code with `@channel` embedded all the
  time).

**Caching strategy.**

- What: username-to-ref resolution.
- Key: `mention.resolve:<username>` → `user_ref | null`.
- TTL: 5 minutes. Invalidate on username change (rare).
- Federation-safety: per-instance; federated `@user@peer.com`
  resolves via a separate WebFinger cache.

**Federation implications.**

- Local-only for MVP. Post-federation-Phase-1.30, add the
  `@user@peer.com` grammar + WebFinger resolution.
- Design now: mention parser returns
  `Mention{Username string, InstanceHost string}` from the start; local
  case has `InstanceHost==""`; upgrade path is a resolver-side
  addition.

**Target implementation sketch.**

- New `app/internal/social/mention/` package with `ParseMentions(text) []Mention`
  and `ResolveLocal(mentions) []int64` (returns user refs).
- Wire from `posts` and `comments` handlers on write path: after
  successful insert, invoke resolver, and for each resolved user
  fire a `notifications.Writer.Enqueue` with verb `mention` +
  targets.
- Reuse existing notification-preferences gating (email vs in-app).
- Tests: parse-then-resolve with a fixture of users + a message body
  containing valid + invalid + code-fenced mentions.

**Effort estimate.** 1 day.

**Sequencing recommendation.** Independent; good "small operator win"
PR to batch with 4.6 (soft-delete recovery) since both touch social
handlers.

---

### 4.6 Soft-delete recovery window (GDPR-shaped)

**Status:** blueprinted-not-shipped. Research depth: `medium`.
**Roadmap phase:** unclaimed. Suggested: 1.17.M-1 or 1.19.E-1.

**ResourceSpace blueprint.**

- Files: `/pages/team/team_resource_delete.php`,
  `/pages/team/team_user_delete.php`,
  `/include/resource_functions.php` (`delete_resource()`,
  `restore_resource()`).
- Pattern summary. RS's soft-delete flips the resource's
  `archive` field to `2` (RS enum for "deleted") and records the
  `time` in `resource_archive_history`. A cron job runs weekly to
  hard-delete rows older than `sysvars['deleted_resource_retention_days']`
  (default 30). Restore un-flips the field. Users have a per-user
  soft-delete-with-30-day-recovery flow that mirrors the resource
  shape.
- What RS does well. Two things: (a) hard-delete is deferred so
  operator-panic doesn't destroy data; (b) restore is a first-class
  operation with an admin surface. Both match GDPR expectations for
  "right to erasure" (soft-delete = pending-erasure marker; hard-delete
  after retention = the actual erasure).
- What RS does badly / what we depart from. Retention config is a
  global int, not per-content-type. No "reason for deletion" capture,
  which GDPR practice increasingly favours. Restore doesn't preserve
  audit continuity (the resource "reappears" without an event
  explaining why). We should have all three.

**Modern gold-standard research.**

- GDPR guidance on soft-delete
  ([ICO guidance](https://ico.org.uk/for-organisations/uk-gdpr-guidance-and-resources/individual-rights/individuals-rights/right-to-erasure/))
  — a soft-delete counts as erasure for retention purposes if the
  data is not accessible; the deferred hard-delete is defensible if
  there is a stated retention policy operators can see.
- The Django-audit-log pattern
  ([django-simple-history docs](https://django-simple-history.readthedocs.io/en/latest/))
  — every delete emits a `Deleted` audit row; restore emits a
  `Restored` row that references the delete. Both live in the audit
  table, not the resource table.

**Caching strategy.**

- What: cache invalidation matters more than caching itself. On
  soft-delete of an asset, its `visibility.CanSee` result changes;
  the existing cache invalidation via `cache.Registry` should fire.
- Federation-safety: peers see the delete via activity; local cache
  invalidation broadcasts on LISTEN/NOTIFY.

**Federation implications.**

- Soft-delete + restore both federate as activities per
  [ADR 0043](./adr/0043-federation-walled-garden-protocol/) — the
  existing `Delete` activity carries a `type` field; add `restore=true`
  as a subtype or emit `Undo Delete` per ArchivePub spec.
- Retention window is per-instance — peer might have already
  hard-deleted while we still have the row soft-deleted. That's
  correct: each instance's retention policy is its own.

**Target implementation sketch.**

- New sysconfig knobs: `soft_delete.asset.retention_days` (default 30),
  `soft_delete.user.retention_days` (default 90),
  `soft_delete.collection.retention_days` (default 30),
  `soft_delete.post.retention_days` (default 30).
- Migration adds `deleted_reason TEXT NULL` column to `assets`,
  `collections`, `posts`, `"user"`. `deleted_at` already exists on
  all four.
- New endpoints: `POST /admin/assets/{id}/restore`, ditto for other
  three entity types. Reuses existing admin capability.
- New cron job (or scheduled goose job) `soft_delete.gc` that runs
  nightly and hard-deletes rows older than the per-type retention
  window.
- Audit events: `admin.asset.soft_deleted`, `admin.asset.restored`,
  `admin.asset.hard_deleted_by_gc`. Reason string is metadata.
- Admin UI: existing asset detail page grows a "Restore" button when
  `deleted_at IS NOT NULL`.

**Effort estimate.** 2-3 day arc — spans migration + backend + cron +
frontend + audit.

**Sequencing recommendation.** Independent; blocks GDPR compliance
narrative for v1.0.

---

### 4.7 Comment moderation queue

**Status:** blueprinted-not-shipped. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: 1.17.G-4.

**ResourceSpace blueprint.**

- Files: `/pages/user/comment_admin.php`,
  `/include/comment_functions.php` (`comment_flag()`).
- Pattern summary. Comments (RS's `resource_note` model) have a
  `flag_count` column and a `flagged_by` join table. Users flag via a
  Report button; admins see a queue at
  `/pages/team/team_comments.php?flagged=1` with per-row Approve,
  Hide, Delete, or Warn-User actions. A hidden comment renders "[hidden
  by moderator]" to non-admins.
- What RS does well. Standard shape; predictable operator workflow.
- What RS does badly / what we depart from. No reason enum on
  flag (spam, harassment, off-topic, etc.). No user-side "why was my
  comment hidden" surface. No auto-hide threshold (N flags → hidden
  pending review).

**Modern gold-standard research.**

- Trust & Safety literature — Discord's community moderation model
  documents the reason-enum approach with categories that map to
  known abuse patterns ([Discord blog on Trust & Safety](https://discord.com/blog/discords-trust-and-safety-org-and-mission)).
- Auto-hide threshold pattern from Reddit's AutoMod
  ([Reddit AutoMod docs](https://old.reddit.com/wiki/automoderator/full-documentation))
  — N reports from Y unique users = auto-hide; documented threshold
  drives predictable moderation.

**Caching strategy.**

- What: per-comment flag count; the flagged-queue result.
- Key: `comment.flags:<comment_id>` and
  `admin.comments.flagged:count`.
- TTL: 30 seconds.
- Invalidation: on flag/unflag write path via `cache.Registry`.

**Federation implications.**

- Flags federate as `Flag` activities per ArchivePub spec §7.
- Peer flags are advisory — local moderation decision is the source
  of truth. Design the flag-count column to distinguish local vs
  federated flags via a JSON facet.

**Target implementation sketch.**

- Migration adds `comment_flag` table (`comment_id`, `flagger_user_ref`,
  `reason` enum, `flagged_at`, `origin_server_id`).
- New endpoints: `POST /comments/{id}/flag` (reason body),
  `POST /admin/comments/{id}/hide`,
  `POST /admin/comments/{id}/approve`.
- Admin queue page reuses `AdminBackfillPanel.svelte` shell columns
  (Flagged when / Reason / Reporter / Actions).
- Auto-hide threshold: sysconfig `moderation.auto_hide_flag_count`
  (default 3).

**Effort estimate.** 2 day arc — migration + backend + frontend.

**Sequencing recommendation.** Independent; low priority for v1.0
unless operator-scale demand exists. Consider deferring to §5.

---

### 4.8 Comment edit history diff trail

**Status:** partial. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: paired with 4.7.

**ResourceSpace blueprint.**

- Files: `/include/comment_functions.php` (`comment_edit()`).
- Pattern summary. RS's `resource_note_edits` table stores every
  edit's `previous_body`, `edited_at`, `edited_by_user_ref` so the
  admin comment view can "show edit history." Users see only a small
  "(edited)" tag next to the timestamp.
- What RS does well. Single-write path on edit — appends a row to the
  history table inside the same transaction as the update. Simple.
- What RS does badly / what we depart from. Diff computation happens
  at read time in PHP; expensive for long comments with many edits.
  We can pre-compute the char-diff.

**Modern gold-standard research.**

- Wikipedia's article-history model (`diff2html` client-side rendering)
  — history rows store the full previous body; diff is client-side
  when the user requests it. Storage cost is the tradeoff.
- Git's line-diff algorithm applied at read time is fine for our
  scale (comments are short).

**Caching strategy.**

- What: the diff between adjacent edits.
- Key: `comment.diff:<comment_id>:<edit_from>:<edit_to>`.
- TTL: 1 hour (edits are rare after posting).
- Federation-safety: per-instance (edits federate but diff cache is
  local).

**Federation implications.**

- Edits federate via `Update` activity per ArchivePub.
- Diff history is per-instance rendering; peers don't share our
  cached diffs.

**Target implementation sketch.**

- Migration adds `comment_edit_history` table (`comment_id`, `body_before`,
  `edited_at`, `edited_by_user_ref`).
- Comment edit handler writes both the new comment body AND the history
  row inside one Tx.
- Frontend: on hover of "(edited)" tag, expand to show the last N
  edits as a small dropdown. Diff computed client-side via `diff-match-patch`
  library.
- No admin surface unless 4.7 ships; then the moderation queue links
  to the history view.

**Effort estimate.** 1 day.

**Sequencing recommendation.** Paired with 4.7 or independent minor.
Low priority.

---

### 4.9 Email digest preferences (immediate/hourly/daily/off)

**Status:** partial. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: 1.17.G-5.

**ResourceSpace blueprint.**

- Files: `/pages/user/user_preferences.php`,
  `/include/user_functions.php` (`get_email_prefs()`).
- Pattern summary. RS's `user_preferences` table stores per-user email
  prefs as a JSON blob with keys `activity_notifications`, `daily_digest`,
  `weekly_digest`, each a boolean. A daily cron scans users whose
  `daily_digest=true`, aggregates their notifications, and emits one
  batched email. The saved-search notifier already ships this pattern
  in AA (per PR #180) but only for saved-search matches, not for
  general activity notifications.
- What RS does well. Aggregation per user, single scheduled job.
- What RS does badly / what we depart from. Prefs are a per-topic
  boolean (activity yes/no), not a per-topic cadence choice (immediate
  vs digest). No unsubscribe link at token level — user must be logged
  in to change prefs.

**Modern gold-standard research.**

- Substack's per-newsletter digest cadence model — each subscription
  independently opts into a cadence.
- Unsubscribe-via-token pattern per
  [RFC 2369 List-Unsubscribe header](https://datatracker.ietf.org/doc/html/rfc2369)
  and [RFC 8058 one-click unsubscribe](https://datatracker.ietf.org/doc/html/rfc8058)
  is standard operator hygiene — Gmail requires it for high-volume
  senders as of 2024.

**Caching strategy.**

- What: current user prefs (loaded per session).
- Key: `user.email_prefs:<user_ref>`.
- TTL: 5 minutes.
- Invalidation: on prefs write; LISTEN/NOTIFY broadcast.
- Federation-safety: per-instance — user prefs never federate.

**Federation implications.** None — per-user local prefs.

**Target implementation sketch.**

- Extend `user_preferences.notification_channels` (already JSON) to
  carry per-topic `{topic, cadence: immediate|hourly|daily|weekly|off}`.
- Extend `notifications.Writer.Enqueue` to consult the pref and either
  fire the email immediately or write a row to a `digest_queue` table.
- New scheduled job `email.digest.hourly` and `.daily` and `.weekly`
  that aggregates queue rows per user and emits the batched email.
- Add `List-Unsubscribe: <mailto:...>, <https://.../unsubscribe?token=...>`
  header to every notification email; token is a signed opaque
  string carrying `{user_ref, topic, exp}`.
- Frontend: preferences page grows a per-topic cadence dropdown.

**Effort estimate.** 2 day arc.

**Sequencing recommendation.** Independent; important for v1.0 email
hygiene under sender-reputation rules.

---

### 4.10 Password-protected share links

**Status:** blueprinted-not-shipped. Research depth: `medium`.
**Roadmap phase:** unclaimed. Suggested: 1.18.C-1 (or fold into
existing shares work).

**ResourceSpace blueprint.**

- Files: `/pages/collections/collection_share.php`,
  `/include/collections_functions.php`.
- Pattern summary. RS's `external_access_keys` table has an optional
  `password` bcrypt column. When present, the share-link landing page
  gates on a password prompt; correct password sets a session cookie
  that persists for the share's duration. Missing password = normal
  view.
- What RS does well. Optional gating — same URL works for password
  and non-password shares; only the landing differs.
- What RS does badly / what we depart from. Password is stored per-
  share, not per-share-recipient (all viewers share the same password).
  RS's crypt algo is legacy — we'd use bcrypt via existing
  `auth.HashPassword`.

**Modern gold-standard research.**

- Dropbox's shared-link password flow — password verify via a
  challenge-response endpoint that sets an HTTP-only cookie scoped
  to the share URL. Cookie has a 24h TTL. Docs:
  [Dropbox API v2 sharing](https://www.dropbox.com/developers/documentation/http/documentation#sharing-modify_shared_link_settings).
- Argon2id as the modern password-hash choice
  ([OWASP Password Storage Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html))
  is nice-to-have but bcrypt matches our existing auth stack.

**Caching strategy.**

- What: none. Password verify is a single bcrypt compare; cheap.
- Session state on successful verify goes in an HTTP-only cookie,
  not server-side session (share is anonymous by design).

**Federation implications.**

- Shares don't federate today ([memory
  project_federation_is_real](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/project_federation_is_real.md)
  covers this).
- If shares grow federation semantics later, password gate needs to
  either be per-instance (peer forwards to owner instance) or a
  shared secret (peer knows the hash).

**Target implementation sketch.**

- Migration adds `share_link.password_hash TEXT NULL` column.
- Share-creation admin form grows an optional "Password" field.
- Share-landing page renders password prompt when hash is present;
  POST `/shares/{token}/verify` runs bcrypt compare + sets `share-<token>` cookie
  with `SameSite=Lax; HttpOnly; Path=/shares/<token>; Max-Age=86400`.
- All share-content endpoints check for the cookie before serving.

**Effort estimate.** 1-2 day.

**Sequencing recommendation.** Independent; medium priority (operator
audience will ask for this for external distribution).

---

### 4.11 Built-in report library

**Status:** blueprinted-not-shipped. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: 1.17.L-1.

**ResourceSpace blueprint.**

- Files: `/pages/team/team_report.php`,
  `/include/report_functions.php`.
- Pattern summary. RS ships a `report` table with hand-authored SQL
  templates keyed by `ref` and grouped by category (`Users`,
  `Resources`, `Collections`, `Storage`, `Activity`). Admin picks a
  template, fills in parameters (`date_from`, `date_to`, `user_group`,
  etc.), runs it, sees a results table and a CSV export. Fifteen
  templates ship out of the box: most-downloaded, orphan resources,
  user activity, storage quota per group, sensitivity distribution, etc.
- What RS does well. Hand-authored SQL means each report is exactly
  what an operator would run at a psql prompt. No abstraction to
  wrestle with.
- What RS does badly / what we depart from. Reports are hand-SQL — any
  refactor of the schema silently breaks them until an operator hits
  the Run button. No integration with the existing job framework, so
  large reports can time out the HTTP request.

**Modern gold-standard research.**

- PostgreSQL `EXPLAIN ANALYZE` per report to catch slow ones before
  operator runs blow up production.
- Async report execution — enqueue the report as a job, email the
  operator when done. Fits our existing jobs + email substrate.
- Metabase's parameter-typed report system
  ([Metabase docs](https://www.metabase.com/docs/latest/questions/native-editor/sql-parameters))
  as an inspiration — typed parameters render UI widgets client-side
  and validate server-side.

**Caching strategy.**

- Reports are queried on operator demand — no caching (staleness would
  be confusing).

**Federation implications.** None (per-instance reports).

**Target implementation sketch.**

- New Go package `app/internal/reports/` with a Registry pattern —
  each report is a struct with `Name string`, `SQL string`, `Params []Param`.
- Ship the 15 RS templates as clean-room-rewritten SQL against the
  AA schema.
- Route `POST /admin/reports/run` — takes report name + param map,
  returns rows JSON or 202 + job ID for async.
- Route `GET /admin/reports/{job_id}` polls for async results.
- Frontend: `/admin/reports/+page.svelte` with a template-picker
  sidebar and a param form.
- CSV export: `GET /admin/reports/run.csv?...` streams the query result
  as CSV.

**Effort estimate.** 3 day arc — mostly SQL authoring for the 15
templates + testing.

**Sequencing recommendation.** Independent; medium priority. Consider
deferring to post-v0.1.0 if the sprint runway is tight.

---

### 4.12 Scheduled email reports

**Status:** partial. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: paired with 4.11.

**ResourceSpace blueprint.**

- Files: `/pages/team/team_report_periodic.php`,
  `/include/report_functions.php` (`schedule_report()`).
- Pattern summary. Operator picks a report + a cadence
  (`daily|weekly|monthly`) + a recipient list. RS's cron scans
  scheduled reports each cadence tick, runs the SQL, emails the
  results as CSV attachment.
- What RS does well. Simple + reuses existing report infrastructure.
- What RS does badly / what we depart from. No timezone handling —
  "daily" is server-local. No opt-out link. Attachment size is
  unchecked.

**Modern gold-standard research.**

- Grafana's scheduled report feature
  ([Grafana docs](https://grafana.com/docs/grafana/latest/dashboards/create-reports/))
  emails PDF snapshots on a cadence + supports per-report timezone.
- The `iCal.RFC 5545` RRULE syntax is overkill for us; three enum
  cadences is fine.

**Caching strategy.** None (scheduled runs, not on-demand).

**Federation implications.** None.

**Target implementation sketch.**

- Migration adds `scheduled_report` table: `report_name`, `params`
  (JSON), `cadence` enum, `recipients` (TEXT[]), `timezone`,
  `last_run_at`, `next_run_at`.
- New job type `report.scheduled` — coordinator-style pattern from
  the saved-search notifier (PR #180). Uses the same
  `templateForVerb` auto-resolution to render the email.
- Reuse the CSV export from 4.11 as the report body.

**Effort estimate.** 1-2 day paired with 4.11.

**Sequencing recommendation.** Paired with 4.11.

---

### 4.13 Tree-node bulk CSV import + merge + use-count UI

**Status:** blueprinted-not-shipped. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: 1.17.N-1.

**ResourceSpace blueprint.**

- Files: `/pages/team/team_nodes_import.php`,
  `/include/node_functions.php` (`import_node_csv()`,
  `merge_nodes()`, `count_node_usage()`).
- Pattern summary. RS's category-tree admin page (`/pages/team/team_nodes.php`)
  is the operator UI for the `field_option` tables. CSV import
  supports adding nodes in bulk with parent-path syntax
  (`Animals/Mammals/Cat`). Merge takes two nodes + a target, rewires
  every asset-tag reference from source to target, deletes source.
  Use-count column shows "N assets tagged with this node."
- What RS does well. Bulk import is the operator escape hatch for
  large taxonomies. Merge with reference rewrite is exactly the right
  primitive for "we accidentally created a duplicate node."
- What RS does badly / what we depart from. Import is synchronous;
  large CSVs time out. Merge doesn't audit-log the reference
  rewrite. Use-count is queried per-row (N+1 pattern).

**Modern gold-standard research.**

- Async import via streaming CSV parser + job queue pattern is
  standard now — see any modern SaaS import flow (Airtable,
  Notion).
- Materialized-view for use-counts — Postgres materialized view
  refreshed on tag-write; O(1) read per node.
  ([Postgres docs on materialized views](https://www.postgresql.org/docs/current/rules-materializedviews.html))

**Caching strategy.**

- What: use-count aggregates per node.
- Key: `node.use_count:<node_id>`.
- TTL: 5 minutes.
- Invalidation: on tag write. Materialised view is an alternative to
  the cache.

**Federation implications.**

- Taxonomies are per-instance today. If federation-of-taxonomy ever
  ships, the merge operation federates as an `Update Merge` activity
  that peers apply optimistically.

**Target implementation sketch.**

- New endpoints: `POST /admin/nodes/import` (multipart form + CSV
  file → async job), `POST /admin/nodes/{id}/merge` (target_id in
  body), `GET /admin/nodes/{id}/usage` (count).
- New job type `taxonomy.import` — streams CSV, upserts nodes
  per-row, reports progress via existing `AdminBackfillPanel` shell.
- Merge does the reference rewrite inside a Tx that also emits
  audit event `admin.node.merged` with `source`, `target`, `affected_asset_count`.
- Use-count from either a Postgres materialized view (refreshed on
  tag write) or a cached aggregate.

**Effort estimate.** 2-3 day arc.

**Sequencing recommendation.** Independent; medium priority.

---

### 4.14 Query profiling / slow-log admin UI

**Status:** blueprinted-not-shipped. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: 1.17.O-1.

**ResourceSpace blueprint.**

- Files: `/pages/team/team_query_log.php`,
  `/include/db.php` (query timing wrapper).
- Pattern summary. RS wraps `mysqli_query` with a timing decorator
  that logs every query > 1s to a `query_log` table (query text,
  duration, caller stack, timestamp). Admin surface filters, groups,
  and shows the top-N slow queries per day.
- What RS does well. Zero setup for the operator — they get the
  slow-log surface out of the box.
- What RS does badly / what we depart from. Logs to a Postgres table
  which itself takes writes on every slow query; can amplify the
  original slowness. Postgres already has `pg_stat_statements` +
  `auto_explain` — we should surface those, not roll our own.

**Modern gold-standard research.**

- `pg_stat_statements` extension
  ([Postgres docs](https://www.postgresql.org/docs/current/pgstatstatements.html))
  aggregates query stats server-side. Enabled with a shared_preload;
  no per-query write cost.
- `auto_explain` for query plans
  ([Postgres docs](https://www.postgresql.org/docs/current/auto-explain.html))
  logs the EXPLAIN plan for queries exceeding a duration threshold to
  Postgres's own log file.
- pgHero as a reference dashboard
  ([pgHero repo](https://github.com/ankane/pghero)).

**Caching strategy.**

- Slow-log queries hit `pg_stat_statements` view; cache 30s.
- Cache key `admin.slowlog:top100:<current_hour>`.

**Federation implications.** None (per-instance).

**Target implementation sketch.**

- Migration seed adds `CREATE EXTENSION IF NOT EXISTS pg_stat_statements;`
  to the baseline schema.
- New Go query wrapping `pg_stat_statements` view — top-N by
  `total_exec_time` or `mean_exec_time`.
- Route `GET /admin/system/slowlog?limit=100&sort=total|mean`.
- Frontend: `/admin/system/slowlog/+page.svelte` with sortable table.

**Effort estimate.** 1 day.

**Sequencing recommendation.** Independent; low priority for v1.0
unless operator-scale demand exists. Consider deferring.

---

### 4.15 Collection-level themes / branding

**Status:** blueprinted-not-shipped. Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: 1.25.A-1 (post-arc — see
[ADR 0025 brand workspace](./adr/0025-brand-workspace/)).

**ResourceSpace blueprint.**

- Files: `/pages/collections/collection_themes.php`,
  `/include/collection_functions.php`.
- Pattern summary. RS collections have `home_page_image` and
  `bg_img_resource_ref` columns pointing to specific resources used
  as brand imagery. The collection landing page renders the branded
  hero + background instead of the default chrome.
- What RS does well. Simple: two nullable FK columns, one branded
  render path.
- What RS does badly / what we depart from. Two hard-coded slots; no
  extensibility. No brand-kit concept (logo + palette + typography
  bundle).

**Modern gold-standard research.**

- Notion's shared-workspace branding model — per-workspace theme
  bundle applied on all internal pages.
- CSS custom properties driven by a per-collection JSON blob is the
  modern way ([MDN CSS custom properties](https://developer.mozilla.org/en-US/docs/Web/CSS/Using_CSS_custom_properties)).

**Caching strategy.**

- What: brand-kit render JSON per collection.
- Key: `collection.brand:<collection_id>`.
- TTL: 5 minutes.
- Federation-safety: per-instance.

**Federation implications.** Per-instance.

**Target implementation sketch.**

- Migration adds `collection_brand` table:
  `collection_id`, `hero_asset_id`, `bg_asset_id`, `primary_color`,
  `secondary_color`, `logo_asset_id`, `updated_at`.
- Admin sets brand via existing collection detail page — new "Brand"
  tab.
- Frontend collection landing checks for a brand row + renders
  branded chrome; falls back to default when null.

**Effort estimate.** 2 day arc.

**Sequencing recommendation.** Independent; low priority for v1.0
core. **Recommend deferring to post-v0.1.0** (§5) — brand workspace is
ADR 0025 which is a labelled Phase 1.25 and shouldn't be squeezed
into pre-v0.1.0.

---

### 4.16 Frontend reverse-image dropzone at `/search/advanced`

**Status:** partial (backend feature-complete since PR #199 + #205 + #206). Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: 1.16.B-followup-4.

**ResourceSpace blueprint.**

- None — reverse-image search is not an RS feature. Clean-room design.

**Modern gold-standard research.**

- Dropzone.js pattern for drag+drop + click-to-select
  ([Dropzone.js docs](https://docs.dropzone.dev/)).
- CLIP-based reverse-image UX from Milvus/Weaviate demos — vertical
  results grid with cosine-similarity score badges.

**Caching strategy.**

- Existing by-image results cache via `SimilarityHintID`.
- No new cache surface for the dropzone.

**Federation implications.**

- Visual embeddings are per-instance ([ADR 0056 §6](./adr/0056-search-architecture/)).
- Cross-peer reverse-image would require exposing the embed function
  as an API; explicitly out-of-scope for v1.0.

**Target implementation sketch.**

- New Svelte component `web/src/lib/components/search/ReverseImageDropzone.svelte`
  — drag+drop zone + file picker + submit button + preview.
- Mounts on `/search/advanced` above the DSL builder.
- POSTs multipart to existing `POST /search/by-image` endpoint.
- Results render in the existing result-card list via a "By-image
  results" tab.
- Disable + tooltip when `search.visual.enabled=false` (parent reads
  sysconfig at mount).

**Effort estimate.** 1 day.

**Sequencing recommendation.** Independent; last operator-visible gap
in the reverse-image feature. Ship this before v0.1.0.

---

### 4.17 #210 sensitivity semantics for `visibility.Filter(EntityAsset)`

**Status:** in-flight-elsewhere (issue #210 filed post-PR #213).
Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: post-audit follow-up.

Captured for sequencing context. This is a **semantic decision** more
than an implementation. Deferred pending operator signal per
ADR 0056 §4 discussion. Ship only if operators report anonymous-
browse permission surprises. If shipped, follows the same snapshot-
compliance discipline as PR #213.

**Sequencing recommendation.** Independent; deferrable to
post-v0.1.0 unless operator reports.

---

### 4.18 #211 `FieldVisibility` API for IIIF metadata gating

**Status:** in-flight-elsewhere (issue #211 filed post-PR #213).
Research depth: `light`.
**Roadmap phase:** unclaimed.

Captured for sequencing context. Requires new API design (field-level
vs row-level visibility semantic), snapshot-test discipline, and
cache-key preservation on `presentation/cache.go`. Not a small PR.

**Sequencing recommendation.** Deferrable to post-v0.1.0 unless IIIF
metadata visibility becomes an operator ask.

---

### 4.19 #212 sqlc migration path for list handlers

**Status:** in-flight-elsewhere (issue #212 filed post-PR #213).
Research depth: `light`.

Captured for context. This is a large refactor requiring per-handler
snapshot suite + migration off sqlc-static queries to hand-written
dynamic SQL. Multi-PR effort. Prime candidate for a labelled Phase
1.17.P arc post-v0.1.0.

**Sequencing recommendation.** Explicitly defer to post-v0.1.0.

---

### 4.20 #214 MDX braced-identifier CI gate on docs PRs

**Status:** in-flight-elsewhere (issue #214 filed post-PR #213).
Research depth: `light`.
**Roadmap phase:** unclaimed. Suggested: 1.55.B.

**Blueprint** — this is a CI hygiene addition, not a code feature.
Existing MDX build fails on `dev` when prose contains bare `{...}`
or `<letter...` outside inline code
([memory feedback_mdx_prose_escapes](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/feedback_mdx_prose_escapes.md)).
Two dev-side breakages so far (PR #208 docs commit, PR #217 pre-audit).
Gate the docs site build on any `docs/**/*.md` PR change, blocking merge on failure.

**Target implementation sketch.**

- New `.github/workflows/docs-site.yml` job that runs on `docs/**/*.md`
  changes: `docker run node:22-alpine pnpm run build` in `site/`;
  block merge on non-zero exit.
- OR: add a fast pre-parser (`sed` + regex) that scans changed
  `docs/**/*.md` files for `{[a-z]` and `<[a-z]` outside triple-
  backtick code fences and inline code spans. Cheap; catches the
  common cases without spinning up the full Astro build.

**Effort estimate.** Half day for the workflow file; 1 day if we ship
both approaches (fast pre-parser + full build).

**Sequencing recommendation.** Cheap. Ship pre-v0.1.0 as a
sub-day hygiene commit — the class of regression it prevents is
disproportionately annoying (silently broken dev-side docs).

---

### 4.21 Baseline migration squash verification

**Status:** partial (baseline squashed per ADR 0046; verification pass
still due). Research depth: `light`.
**Roadmap phase:** 1.49.C series (per ADR 0046).

**Blueprint** — per
[ADR 0046](./adr/0046-migration-baseline-and-squash-policy/), the
current state is that all migrations are collapsed into
`00001_baseline_v1.sql` plus incremental additions. Before v0.1.0, one
verification pass:

- On a fresh Postgres 16, run `goose up` from `00001_baseline_v1.sql`
  through the current head.
- Dump the resulting schema (`pg_dump --schema-only`).
- Diff against the committed `app/schema.sql`.
- Diff must be empty (modulo Postgres version banner + comment
  ordering).

**Effort estimate.** Half day.

**Sequencing recommendation.** Ship right before the v0.1.0 tag as
final release-readiness.

---

### 4.22 oapi-codegen version pinning

**Status:** blueprinted-not-shipped. Research depth: `light`.

**Blueprint.**

- `scripts/generate.sh` invokes `go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest`.
  The `@latest` semantic means CI + local versions can drift silently —
  as caught in PR #217 (v2.7.1 committed on `dev` vs v2.7.2 on CI).
- Fix: pin `@vX.Y.Z` explicitly. Bump the pin when we want the new
  version.

**Target implementation sketch.**

- Edit `scripts/generate.sh`: replace `@latest` with the specific
  version tag currently committed.
- No new dependency; no CI change.

**Effort estimate.** 15 minutes.

**Sequencing recommendation.** Ship whenever convenient. Cheap.

---

## 5. Deferred to post-v0.1.0

Items intentionally deferred, with rationale. **These do NOT block
v1.0** — they're deliberate scope decisions.

- **§4.15 Collection-level themes / branding.** Belongs in
  ADR 0025's Phase 1.25 brand workspace arc; not core to v1.0's
  operator/user story.
- **§4.17 #210 sensitivity semantics.** Semantic decision gated on
  operator signal. Ship post-v0.1.0 if operators report anonymous-browse
  surprises.
- **§4.18 #211 FieldVisibility for IIIF.** Requires new API design +
  snapshot suite; not small. Defer to post-v0.1.0.
- **§4.19 #212 sqlc migration for list handlers.** Multi-PR effort;
  low observable-behaviour payoff. Ship as a labelled Phase 1.17.P
  post-v0.1.0.
- **§4.14 Query profiling / slow-log.** Nice-to-have; operators can
  use `psql \pset expanded` today. Ship post-v0.1.0 if operator scale
  demands.
- **§4.7 Comment moderation queue** + **§4.8 comment edit history
  diff trail.** Ship post-v0.1.0 unless operator-scale comment volume
  demands it. AA's current audience is small enough that in-person
  moderation via delete-and-message works.
- **Batch multi-asset metadata edit.** From RS-gap audit A-tier;
  deferred per that memory. Operator-heavy UI; not v1.0-blocking.
- **Custom per-resource ACL.** Same rationale as above.
- **HEIC/HEIF pure-Go decoder or CGo add-on** ([memory
  project_metadata_extraction](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/project_rs_gap_audit_2026_06_22.md)
  documents the constraint). Defer to a capability add-on per
  ADR 0034.
- **RAF (Fujifilm) raw format.** Non-TIFF container; ship when
  operator demand emerges.
- **Contact sheets, watermarking, MOBI/Calibre, plain-text-to-JPEG,
  persistent collection bar, save-and-next batch UX** — explicit
  NON-goals per the RS-gap audit memory + user preferences.

---

## 6. RS reference inventory + capture status

**Physically deleted 2026-07-08 via Phase 1.55.S.** The gitignored
reference tree at `/dbstruct/` (165 files, 136 KB), `/include/`
(82 files, 3.7 MB), `/plugins/` (61 items, 96 MB), `/pages/`
(86 items, 3.1 MB) — ~8,944 files and ~102 MB total — has been
removed from local disk; the three `scripts/rs-*` pullers that
regenerated it are deleted; `.gitignore` retains the path entries as
a safety net for stray copies but nothing tracked or scripted brings
them back.

This section groups the pattern archive by concern and records where
the pattern is captured internally. Every row was audited as
**delete-safe? YES** in Phase 1.55.A + the deletion executed in
Phase 1.55.S.

| Pattern | RS files | Captured in | Delete-safe? |
|---|---|---|---|
| Fork context + provenance | `/dbstruct/*.sql` (baseline schema) | ADR 0001 + `00001_baseline_v1.sql` + cleanup-audit-2026-06 | YES |
| BSD-3 license inheritance | `LICENSE` provenance | ADR 0002 + `LICENSE` file | YES |
| Field-definition semantics | `/dbstruct/table.resource_type_field.txt` + `/dbstruct/table.node.txt` | ADR 0012 + [ADR 0012](./adr/0012-metadata-model/) + shipped `field_definition` + `field_option` code | YES |
| Search architecture (BM25 + facets + saved) | `/pages/search.php`, `/include/search_functions.php` | ADR 0056 + shipped `app/internal/search/` | YES |
| Metadata extraction pipeline | `/include/resource_functions.php`, `/plugins/exiftool_extract/` | ADR 0012 + shipped `app/internal/asset/metadata/` | YES |
| Job queue substrate | `/include/job_functions.php`, `/dbstruct/table.job_queue.txt` | ADR 0038 §"jobs" + shipped `app/internal/jobs/`; DLQ + cancel patterns captured in §4.1 + §4.2 | YES |
| Job DLQ + admin review | `/pages/team/team_jobs.php` | Captured in §4.1 | YES |
| Sysconfig admin UI | `/pages/team/team_setup.php`, `/include/config_functions.php` | Captured in §4.3 | YES |
| Storage architecture | `/include/staticsync.php`, `/include/storage_plugin_manager.php` | ADR 0008 + shipped `app/internal/storage/` | YES |
| Collections model | `/include/collections_functions.php`, `/pages/collections/` | ADR 0009 + shipped `app/internal/collections/` | YES |
| Permissions + teams + workflow | `/include/permissions_functions.php`, `/include/workflow_functions.php` | ADR 0010 + shipped `app/internal/teams/` + `app/internal/workflow/` | YES |
| Asset entity model | `/include/resource_functions.php`, `/dbstruct/table.resource.txt` | ADR 0011 + shipped `app/internal/assets/` | YES |
| Caching strategy | `/include/general.php` (cache helpers) | ADR 0013 + shipped `app/internal/cache/` (Registry pattern) | YES |
| Share-link substrate | `/include/collections_functions.php` (share) + `/dbstruct/table.external_access_keys.txt` | ADR 0018 + shipped `app/internal/shares/`; password-protected captured in §4.10 | YES |
| Bulk operations UX | `/pages/actions/actions_asset_edit.php` etc. | ADR 0019 + shipped `app/internal/bulk/` | YES |
| Asset gating / NDA | `/pages/user/user_nda.php`, `/include/nda_functions.php` | ADR 0020 + shipped `app/internal/sensitivity/` | YES |
| Audit log + change tracking | `/include/log_functions.php`, `/dbstruct/table.log.txt` | ADR 0032 + shipped `app/internal/audit/` | YES |
| Observability + telemetry | `/plugins/health_check/`, `/include/general.php` (health) | ADR 0033 + shipped `app/internal/observability/` | YES |
| Capability add-ons | `/include/plugin_functions.php` | ADR 0034 + Extism-based plugin strategy | YES |
| External imports framework | `/plugins/csv_upload/`, `/plugins/z3950/` | ADR 0036; tree-node CSV import captured in §4.13 | YES |
| Caption + subtitle artifacts | `/plugins/subtitle_upload/` | ADR 0037 + shipped `app/internal/subtitles/` | YES |
| Notifications substrate + digest | `/include/user_functions.php`, `/pages/user/user_preferences.php` | Shipped `app/internal/notifications/`; digest prefs captured in §4.9; mentions captured in §4.5 | YES |
| Soft-delete + retention | `/include/resource_functions.php` (delete/restore) + `/dbstruct/table.resource_archive_history.txt` | Captured in §4.6 | YES |
| Comment thread substrate | `/include/comment_functions.php`, `/dbstruct/table.resource_note.txt` | Shipped `app/internal/social/comments`; moderation + edit-history captured in §4.7 + §4.8 | YES |
| Report library | `/pages/team/team_report.php`, `/include/report_functions.php` | Captured in §4.11 + §4.12 | YES |
| Tree-node bulk import + merge | `/pages/team/team_nodes.php`, `/include/node_functions.php` | Captured in §4.13 | YES |
| Query profiling / slow-log | `/pages/team/team_query_log.php`, `/include/db.php` | Captured in §4.14; delegated to `pg_stat_statements` | YES |
| Collection themes / branding | `/pages/collections/collection_themes.php` | Captured in §4.15 + ADR 0025 | YES |
| Schema mismatch boot detection | `/include/dbmigrate.php` | Captured in §4.4 | YES |
| Language / i18n plumbing | `/languages/*.yaml`, `/include/language_functions.php` | Shipped Svelte `$stores/lang.svelte` + backend i18n; no per-string RS ref needed | YES |
| PDF preview generation | `/plugins/pdf_previews/`, `/include/preview_functions.php` | Shipped `app/internal/preview/` incl. `pdfcpu` integration + `preview.PDFHandler`; ADR 0034 covers extension | YES |
| Video HLS + poster pipeline | `/plugins/video_previews/` | Shipped `app/internal/audiobook/` + `preview.VideoHandler` (ffmpeg) | YES |
| Preview + variant model | `/dbstruct/table.preview.txt`, `/include/image_processing.php` | Shipped `app/internal/preview/` + storage_variant table | YES |
| IIIF Image API | none in RS baseline (post-fork addition) | ADR 0053 + shipped `app/internal/iiif/` | YES (not a RS pattern) |
| RS-internal PHP admin nav layout | `/pages/team/*` composite | Superseded by our clean-room `/admin/*` SvelteKit UI; no per-page capture needed | YES |
| RS-internal language file format | `/languages/*.yaml` | Superseded by `web/src/lib/i18n/`; RS format not adopted | YES |
| RS resource-type / template subpages | `/pages/team/team_field_edit.php` etc. | Shipped `app/internal/assettype/` + admin field definition editor | YES |
| RS-internal share-link report | `/pages/team/team_share_report.php` | Not a v1.0 requirement; folded into §4.11 report library | YES |

**Delete-safety verdict: YES on every row.** The physical ref tree
can be deleted in a follow-up PR after this doc lands, the user
reviews §4 and §6, and the RS-blueprint capture depth passes review.
Any pattern that later surfaces as "we needed this and it's not
captured" is fixable by a targeted commit against §4.

---

## 7. Sequencing proposal — the "what to work on next" menu

The user makes the final call; this is the recommendation.

### 7.1 Cheap hygiene (do first, all sub-day)

Bundle into a single "1.55.B hygiene" PR that lands before any of the
substantive gaps:

- §4.22 oapi-codegen version pinning (15 min)
- §4.20 #214 MDX braced-identifier CI gate (half day)
- §4.4 schema-mismatch boot detection (half day)
- §4.21 baseline migration squash verification (half day)

Total ~1.5 days. Zero product surface — pure release-readiness
hygiene.

### 7.2 Highest operator-visible value (order by impact)

Ship in this order — each is independent, all are v1.0-blocking:

1. §4.16 reverse-image dropzone (1 day) — closes a four-PR-old open
   backend contract on the frontend.
2. §4.9 email digest preferences (2 days) — v1.0 sender-reputation
   hygiene (Gmail requires List-Unsubscribe).
3. §4.5 @-mention notifications wired to notify (1 day) — closes a
   "parses but doesn't fire" bug.
4. §4.6 soft-delete recovery window (2-3 days) — GDPR narrative for
   v1.0.

Total ~7 days.

### 7.3 Operator power tools (medium priority)

Ship if runway permits; otherwise defer to post-v0.1.0:

5. §4.1 job DLQ + §4.2 job cancellation (paired, 2 days).
6. §4.3 sysconfig admin UI with typed widgets (2-3 days).
7. §4.10 password-protected share links (1-2 days).
8. §4.13 tree-node bulk CSV + merge (2-3 days).

Total ~8-10 days. Consider deferring §4.13 first if squeezed.

### 7.4 Reporting arc (bundle as one arc)

9. §4.11 report library (3 days) + §4.12 scheduled reports (paired,
   1-2 days) = 4-5 days.

Best implemented as a single "1.17.L reporting arc" PR pair since
they share the same SQL registry pattern. Consider deferring to
post-v0.1.0 unless operator scale demands it.

### 7.5 Total v1.0 remaining effort estimate

- **Base v1.0 scope** (7.1 + 7.2): ~9 days.
- **Nice-to-have v1.0** (7.3): +8-10 days.
- **Optional v1.0** (7.4): +4-5 days.

Two-to-three-agent-day sprints of the recent cadence would burn
through the base scope in a week and the full menu in three weeks.

---

## 8. RS deletion readiness checklist ✅ SHIPPED (Phase 1.55.S)

All seven gates cleared 2026-07-08:

- [x] Every ADR that cited RS (0001, 0002, 0003) has been reviewed.
  ADR 0001 flipped to `superseded-by: 0040` in 1.55.S; ADR 0002 is
  `superseded-by: 0016` (from prior lifecycle); ADR 0003 remains
  `superseded` per the strangler-fig-abandoned memory.
- [x] Every code comment referencing `/dbstruct` / `/include` /
  `/plugins` / `/pages` paths has been updated or removed. Grep proof:
  `grep -rn "/dbstruct\|/include/\|/plugins/\|/pages/" app/ web/src/ docs/`
  returns only references inside this `v0_1_readiness.md` doc.
- [x] Every open gap in §4 has captured RS blueprint per the §4
  substructure (audited 1.55.A, no gaps discovered since).
- [x] §6 inventory table has zero `NO` rows (all rows audited 1.55.A).
- [x] `scripts/gen-rs-baseline.py` + `scripts/gen-rs-seeds.py` +
  `scripts/rs-diff.sh` — deleted; no remaining call sites.
- [x] `.gitignore` retains the `/dbstruct/` `/include/` `/plugins/`
  `/pages/` etc. entries as a **safety net** for stray copies from
  earlier snapshots; the RS-branded comment header replaced with a
  generic "legacy reference tree" note.
- [x] No `README.md` references to the reference tree as a
  contributor resource (verified via `grep -in "resourcespace"
  README.md`).

**Shipped state.** Application code + config files carry no RS
mentions (`grep -rn -iE "resourcespace|resource[-_]space" app/ web/
scripts/ Dockerfile* .env.example .goreleaser.yaml docker-compose.yml
.dockerignore .gitignore` returns empty). ADR bodies (0001, 0002,
0016, 0046) and historical audit docs (`cleanup-audit-2026-06.md`) +
this readiness doc itself + memory files retain deliberate historical
references — those are the durable "why" record.

---

## 9. Post-milestone arc pointers

Arcs unblocked by shipping this audit + the sequenced work in §7. Split
per the §0 milestone model — the shorter list (v0.1.0) is what unlocks
right after the first tag; the longer list (v1.0.0) is what starts
mattering once the codebase reaches out-of-beta quality.

### After v0.1.0 (first tag ships)

- **Relicense arc** per ADR 0016 → ADR 0017 → Phase 1.24. Ship the
  AGPL + commercial dual-license after v0.1.0 tags, once RS refs are
  physically deleted and the residual audit is clean. Timing tracked
  in issue #229.
- **Monetization arc** per Phase 1.24. Gated on the relicense; first
  paying customers arrive here.
- **ADR 0055 pg_search revisit.** The search-arc ADR filed this for
  future revisit if operators complain about ranking quality. PR #208
  shipped the feedback-signal collection surface; when structured
  complaint data exists, revisit.
- **Full IIIF Presentation 3.0 feature completeness** — pages 189–192
  filed follow-ups. Ship post-v0.1.0.

### After v1.0.0 (out of beta)

- **Migration append-only enforcement** per ADR 0046. From v1.0.0 tag
  forward, schema changes are additive only; the enforcement hooks
  (CI check that no PR modifies existing migration files) need to
  activate at tag time. **Note:** whether the trigger is actually
  v0.1.0 vs v1.0.0 is under review — see issue #228.
- **SemVer compatibility promises.** Per §0, the 0.x range still
  permits minor-version schema breaks; once v1.0.0 tags, API + DB
  contracts stabilise per the SemVer spec. Release notes format,
  deprecation policy, and back-compat surface all pin to this
  milestone.
- **Federation Phase 1.30+ / remote workers.** Per
  [memory project_federated_remote_workers](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/project_federated_remote_workers.md).
  Design work already exists; implementation trails the out-of-beta
  soak.

---

## Appendix A. Pre-audit findings (recorded for provenance)

**Q1 — RS physical inventory:**
- `/dbstruct/` — 165 files, 136 KB.
- `/include/` — 82 files, 3.7 MB.
- `/plugins/` — 61 items, 96 MB (largest; individual plugin sub-trees).
- `/pages/` — 86 items, 3.1 MB.
- Total: 8,944 files across the four dirs; ~103 MB.

**Q2 — RS references in tracked code:**
- 14 explicit "ResourceSpace" / "resourcespace" citations in
  `app/`, `docs/`, `web/src/`.
- Most in ADRs (0001, 0002, 0003), the cleanup-audit doc, and one
  `users/userstate.go` code comment on the RS-heritage `approved`
  column.

**Q3 — ADRs citing RS:**
- 0001 (Hard fork) — foundational; recommend flip to
  `superseded-by: 0040` at v0.1.0 tag or keep as historical.
- 0002 (BSD-3 license) — cites RS license inheritance; keep for
  relicense arc.
- 0003 (Strangler fig) — abandoned per
  [memory project_strangler_fig_abandoned](../home/kenneth/.claude/projects/-mnt-d-Projects-artist-alley/memory/project_strangler_fig_abandoned.md).
  Recommend flip to `superseded` before v1.0.

**Q4 — RS-gap audit A-tier status (2026-06-22):**
- Shipped: notifications-read, smart collections, search history.
- Open (captured in §4): @-mentions, soft-delete recovery, sysconfig
  UI, job DLQ, job cancel, tree-node import, report library,
  scheduled reports, collection themes, password-protected shares,
  comment moderation, comment edit history, digest prefs, schema
  mismatch, query profiling.

**Q5 — RS patterns NOT in the audit:**
- Bulk actions UX — covered by shipped `app/internal/bulk/`.
- Preview + variant model — covered by shipped `preview/` + storage
  variants.
- Static-sync / storage-plugin manager — covered by shipped
  `storage/` package.
- Plugin i18n hooks — superseded by `web/src/lib/i18n/`.
- Nothing surfaces from the physical tree that isn't already
  captured either in §4 or in the shipped-code table in §6.

**Q6 — Roadmap v1.0 markers:**
- `docs/roadmap.md` lines 8, 20, 890 reference v1.0.
- Migration baseline `00001_baseline_v1.sql` already exists.
- Roadmap does NOT currently have a v1.0 exit-criteria section — this
  doc becomes the artifact.

**Q7 — Research MCPs:**
- WebFetch, WebSearch, Playwright MCP available.
- FlareSolverr + searxng not connected in this session; light-touch
  research used inline citations from public docs URLs.
- Depth per gap flagged in §4 accordingly.
