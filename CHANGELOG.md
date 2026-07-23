# Changelog

All notable user-facing + wire-format changes to artist-alley.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions track the ArchivePub federation spec ([docs/protocol/archivepub.md](docs/protocol/archivepub.md))
where applicable, otherwise note "no-spec-impact."

## [Unreleased]

_Nothing yet._

## [v0.6.0] — 2026-07-23 — Public read surface + demo hardening

### User-facing changes

- **Public user-profile pages.** Every user now has a profile page, reachable by
  username (`/users/by-username/{name}`) or stable ref, showing their display
  name, avatar, and the assets/posts/collections a viewer is allowed to see. It
  reuses the existing visibility rules — anonymous visitors see only public work
  (and only when public mode is on), and an owner can opt out of anonymous
  exposure. This also cleared the last of the dead author/similar-asset links
  (#478).

- **Shared view controls across every asset surface.** The browse view switcher
  (grid / masonry / thumbnail / list) and sort direction now appear on the
  profile and post-by-asset pages too, not just the main browse — one consistent
  control bar everywhere assets are shown (#511).

- **Faster 3D previews, and multi-file models fixed.** Open-format 3D previews
  (glTF/GLB, FBX, OBJ) now render through a headless three.js worker instead of
  Blender — much faster, and **arm64 deployments get 3D previews for the first
  time** (the Blender path was amd64-only). Multi-file glTF (a `.gltf` plus its
  external `.bin`/textures) now renders correctly, where before it failed
  silently (#497/#498/#507/#508, #486). Blender stays as an automatic fallback.

### Fixes

- **Federation-path query bug.** A metadata-adapter query referenced a
  nonexistent column (`owning_team_id` instead of the real `team_id`), so that
  path errored on every call. Pre-existing since ≤v0.5.2 and invisible to
  standard CI (which doesn't run federation); caught by the federation nightly
  and fixed before this release (#538).

- **CI reliability.** A large hardening pass on the test suite — shared-auth
  setup resilience, worker-isolation races, and timeout tuning — so a green run
  genuinely means green, not retry-masked (#485, #481, #505, #527, #535).

## [v0.5.2] — 2026-07-21

A content-visibility capability so read-only viewers (the public demo) can see
their whole catalogue.

- **`content.read.all` capability (#474).** A content-plane-only read cap,
  honored solely in `visibility.CanReadContent` alongside the `system.admin`
  wildcard — it grants asset-byte reads at every sensitivity tier and nothing
  else (no admin surfaces, no writes; it is not a wildcard). This lets a
  read-only role (e.g. the demo viewer) see `team`/`restricted` content that
  would otherwise return blank "Preview unavailable" tiles, without exposing
  any administrative surface. Migration `00014` defines the cap; granting it to
  a role is a deploy-side provisioning step (ADR 0060).

## [v0.5.1] — 2026-07-21

Promoted all of `dev` since v0.5.0 — the foundation work below (audit
retention/export, scheduled actions) plus two demo-surfaced fixes and a
visibility-consolidation batch. A patch version number, a substantial release.

### Operator-facing changes

- **Audit-log retention and export.** The audit log now has a retention
  policy — configurable per event category (a default of 7 years, with
  shorter or longer holds per category), a legal-hold flag that exempts
  individual events from purge, and a nightly enforcement pass. A GDPR
  erasure request anonymises a user across the log — the events are
  kept, the person is replaced by a `deleted-user` placeholder — so the
  trail survives without the personal data. And the whole log can be
  exported as CSV or NDJSON over a date range, streamed so exports of
  millions of rows don't exhaust memory; IP addresses are withheld from
  the export for callers who can't see them in the live view.

- **Scheduled actions.** Operators (and, later, the privacy, commerce and
  audit-retention features) can now schedule a change to run at a future
  time — change an asset's sensitivity, soft-delete, change state, or
  notify — and cancel it before it fires. Each action executes atomically
  with its audit entry, so it either fully happens and is logged or fully
  does not; a failure is recorded rather than half-applied. This is the
  generic engine (ADR 0020); the asset-gating features that use it —
  blur, reveal, timed embargo lift — land in later sprints.

### User-facing changes

- **Shareable, reloadable asset pages.** Assets now have a real
  `/assets/[id]` page, so a link to an asset opens, reloads, and shares
  correctly. Before this, clicking an asset inside a collection
  dead-ended on a "Not found" page — the tile linked to a route that
  never existed (#475). A build-time link-integrity check now guards
  against dead internal links (ADR 0068).

- **3D previews work on published builds again.** Turntable thumbnails
  for 3D models (glTF / OBJ / FBX and more) had silently stopped
  generating on released images — the published image shipped without
  the renderer — so every 3D asset showed no preview (#470). Fixed for
  amd64, with a build-and-render smoke so it can't regress unnoticed.

### Fixes

- Content-visibility hardening: soft-deleted collections no longer
  appear to signed-in non-owners, and the IIIF image path enforces the
  same visibility rule as the browse grid (#451, #460), plus audit and
  admin-gating cleanups (#458, #431).

## [v0.5.0] — 2026-07-20 — Public mode: anonymous browsing

Content is now reachable without an account, on an operator's terms. The
visibility model got a single enforcement point, sensitivity moved to the
content plane, and opening the surface surfaced (and closed) three
pre-existing access holes in the foundation it was built on.

### Operator-facing changes

- **A `public` visibility tier now exists** for collections and posts, and
  anonymous callers have a defined, enforced view of content: published,
  public, ready assets and public collections/posts only. Content
  visibility is decided in exactly one place — the visibility predicate —
  which every read path splices in (ADR 0063).
  Authenticated behaviour is deliberately unchanged. An authenticated
  caller still *sees* assets of every sensitivity in listings — that is
  intended, not a gap: sensitivity gates the bytes, never the rows, so
  restricted material stays listed as a locked item rather than
  vanishing (ADR 0020 via ADR 0064).
- **Asset browse now goes through that same predicate.** The browse query was
  sqlc-generated static SQL, which cannot accept a runtime fragment — it was
  the one read path visibility could not reach. Converted to hand-built SQL and
  gated. The superadmin-only `include_deleted` flag waives the soft-delete
  check **and only that** — publication status, sensitivity and processing
  state still apply, so the flag cannot drift into meaning "skip authorization".

- **Asset sensitivity is now enforced when serving files.** Previously any
  authenticated caller could download any asset's bytes — including `draft`
  and `restricted` material — because the byte-streaming endpoints checked
  only that a caller was signed in. Sensitivity now gates **content**: `team`
  assets require team membership, and `restricted`/`embargo` are limited to
  the owner and system administrators. Listing is deliberately unchanged —
  restricted assets remain visible as locked items rather than vanishing
  (ADR 0064, following ADR 0020). Denials return 404 rather than 403 so a
  response cannot be used to confirm that a restricted asset exists.

- **Two remaining copies of the visibility rule were removed, and a
  latent IIIF gap was found in the process.** Reverse-image search
  carried its own hand-written "anonymous sees public only" filter; it
  now uses the same visibility predicate as every other read path,
  which also correctly hides draft and still-processing assets that the
  old copy let through. The IIIF manifest layer keeps its own
  sensitivity gate — investigation confirmed it is not a duplicate but
  the *only* thing refusing a restricted asset's manifest to an
  anonymous caller, and a misleading code comment that invited its
  removal was corrected.

- **Audit-log IP addresses are now gated behind their own capability.**
  A read-only auditor could previously see the IP of every actor in the
  log — personal data that identifies people and approximates their
  location — because it rode along with the ordinary
  `system.audit.read` view. Seeing *what happened* and seeing *from
  where* are now separate grants: `system.audit.read` returns the log
  without IPs, and a dedicated `system.audit.pii.read` is required to
  see them. The address is withheld at the API, not merely hidden in
  the UI.

- **Access requests can no longer name a capability that doesn't exist.**
  `requested_capability` on an asset-access request was free text stored
  verbatim, in a field that feeds an authorisation decision — so a
  requester could put anything at all in it. It is now constrained to
  the real capability registry by a foreign key, and a request naming an
  unknown capability is rejected with a clear 400 instead of failing
  deeper in. Deleting a capability that still has outstanding requests
  now fails loudly rather than silently discarding the record of who
  asked for what.
  This narrows the field rather than fully securing it: a request can
  still name a *real* capability the requester shouldn't be able to ask
  for. Which capabilities are legitimately requestable is decided with
  the access-grant flow, which remains deliberately unbuilt.

- **A logged-out visitor now has something to look at.** Curated
  content can be featured for a public audience, and the front page
  renders it. Featuring is now a placement rather than a flag on the
  thing featured — the same collection can be featured publicly and
  internally at once, with its own ordering in each, and an individual
  asset can be featured without wrapping it in a collection.
  Two separate featured mechanisms had grown up side by side; there is
  now one. Featuring never widens access: a featured item renders only
  if the viewer could already see it, so publishing the rail does not
  publish the library.

- **Public browsing is now an operator choice, and it is off by
  default.** Anonymous access had no switch: any instance running this
  code served its public content to the internet whether the operator
  wanted that or not, and an existing install would have had it turned
  on by an upgrade. There is now a setting for it, enforced at the API
  rather than by hiding pages — turning it off means anonymous requests
  are refused, not merely unlinked. A fresh install starts private, and
  first-boot, login and SSO keep working with it off.

- **Signing in no longer hid public collections, and logged-out
  visitors could no longer see private ones.** Two visibility defects
  surfaced while opening anonymous access, both now fixed. An
  authenticated user got "not found" on a public collection they did
  not own — signing in *removed* access, and an administrator saw less
  than a logged-out stranger. Separately, the collection **list**
  endpoint applied no visibility rule at all, so an anonymous request
  returned every collection in the system, private ones included, with
  their names. Listing now goes through the same single visibility
  decision as every other read path.

- **A collection's contents are now visible to logged-out visitors —
  and were previously readable by any signed-in account.** Listing what
  is inside a collection applied no visibility check at all: any
  authenticated caller could enumerate the full contents of any
  collection by id, including collections they had no access to, and the
  response carried titles, types and publication status for draft
  material. The endpoint now checks the caller may see the collection,
  and filters the contents themselves — so a public collection shows
  only its public items to an anonymous visitor, while its owner still
  sees everything. Public collection pages render their contents rather
  than appearing empty.

- **Browsing without an account now works.** Listing assets and
  collections, and opening a single asset or collection, no longer
  require a signed-in caller: `GET /assets`, `GET /assets/{id}`,
  `GET /collections` and `GET /collections/{id}` serve anonymous
  requests, with the visibility predicate deciding what comes back —
  published, public, ready content only. Every write path still
  requires authentication.
  **This also closed a pre-existing hole**, which is the more important
  half: the two detail endpoints previously checked only that *some*
  caller was signed in and then fetched by id, so any authenticated
  account could read any asset or collection — including another
  user's private collection — simply by knowing its id. Both now run a
  real visibility check, and a denial returns 404 rather than 403 so a
  response cannot confirm that a hidden item exists.
  One consequence to expect: a public collection's *contents* are not
  yet anonymous, so a logged-out collection page shows its title and an
  empty body until that lands separately.

- **Anonymous visitors can now load public images.** The byte-streaming
  endpoints previously required a signed-in caller before anything else
  ran; they now defer to the same content check, which admits anonymous
  callers to `public`-tier assets and nothing else. `team`, `restricted`
  and `embargo` bytes remain unreachable without an account, across
  every byte-serving path (originals, derivatives, HLS segments and
  archive entries). This is the first surface where an anonymous request
  receives real content rather than metadata — the metadata endpoints
  are still authenticated and land separately.

### Infrastructure / housekeeping

- **The site now rebuilds from a signal that can fail.** When docs
  this repo owns change, the marketing site was rebuilt by firing a
  Cloudflare deploy hook — a bare POST that reports success for having
  been sent, not for a build that worked. Nineteen production deploys
  failed over twenty-four hours behind that signal with nothing to show
  it. The trigger now dispatches to the site repository instead,
  carrying the exact commit that changed so a rapid second push cannot
  cause the wrong content to be built, and a rejected credential fails
  loudly rather than skipping silently.


- `app/schema.sql` refreshed from a cleanly migrated database. The
  committed copy had drifted in **column order** — Postgres physical
  order is creation order, so columns added by later migrations land at
  the tail, and the stale file described an order the migrations never
  produce. That silently changed which Go types sqlc generated. Query
  column lists were realigned with the real schema; pg_dump's
  `\restrict`/`\unrestrict` markers are stripped so the file is
  byte-reproducible.
- Version files corrected to 0.4.0 (they had been left at 0.3.1).

## [v0.4.0] — 2026-07-18

Operator visibility: the async pipeline and the storage layer are now
observable and manageable from the admin surface. No-spec-impact.

### Operator-facing changes

- **Jobs admin.** The whole async pipeline (derivatives, previews, AI
  tagging, federation outbox) runs on the job queue, and until now it could
  only be inspected with `psql`. New surfaces, read-gated on
  `system.jobs.read` so a read-only operator can watch without holding
  `system.admin`: **queue** (jobs by status/type with age and priority),
  **workers** (active workers, lease state, stale-lease flag), **live**
  (status counts), **failed** (with `last_error`), **kinds** (per-type
  concurrency), **schedules** (future-dated work). Requeue, cancel, and
  concurrency edits require `system.admin`; a job that is currently running
  is never touched by either action.
- **Storage admin.** **Usage** (deduplicated bytes on disk, originals vs
  derivatives, breakdowns by content type and backend) and **variants**
  (per-family inventory), read-gated on the new `system.storage.read`.
- **Storage integrity sweeps.** `orphan_scan` reconciles the object store
  against the database in both directions; `checksum_verify` re-hashes
  stored bytes against the content-addressed key. Both run as batched,
  resumable job kinds, so they appear in the jobs queue like any other work,
  and both report into an admin surface. Findings are **advisory** and
  record scan time; no destructive cleanup ships in this release.
- **About reports the real version.** The page previously showed a
  hard-coded placeholder. It now reads a new anonymous `GET /build-info`
  endpoint serving the version baked in at build time. The displayed licence
  was also corrected to AGPL-3.0-only, matching the repository.
- **Help is visible to read-only operators.** Documentation, shortcuts,
  about, release notes, and support are now explicitly public admin tiles
  rather than implicitly superuser-only, and appear identically on desktop
  and mobile (ADR 0061).

### Infrastructure / housekeeping

- Storage backends gained an ordered, cursor-resumable `List` (ADR 0062).
  Filesystem walk order is not lexicographic over the key space, so the fs
  backend prunes and sorts to honour the contract; a shared contract test
  enforces it for every backend.
- Dependabot grouped per ecosystem into minor-and-patch versus majors with a
  lower open-PR limit, so routine bumps stay auto-mergeable and a batch no
  longer starves the self-hosted runners ahead of a release.
- Pre-checkout stale-`.git`-lock sweep on every self-hosted job, fixing
  intermittent checkout failures caused by cancelled mid-fetch runs.

## [v0.3.1] — 2026-07-17

Admin read-cap UI + foundation cleanup. No-spec-impact.

### Operator-facing changes

- **Admin UI for read-cap holders.** The frontend half of v0.3.0's read
  capabilities: the admin menu + route guard now gate **per-tile on the
  capability each surface enforces**, so a read-only role (without
  `system.admin`) sees and can browse the admin sections its caps permit —
  the admin menu lights up on the public demo. Backend still enforces every
  write.

### Infrastructure / housekeeping

- Repo-wide `gofmt` normalization + a `gofmt -l` CI gate.
- `make release` target codifying the release prep (version bump, openapi
  regen, drift check, open the promotion PR) — does not tag or toggle
  protection.
- Dependabot `github-actions` group split (routine bumps auto-merge; majors
  gated); steel secondary token wired into the Alert info tone.
- CHANGELOG + roadmap reconciled to current (they had drifted two releases
  behind).

## [v0.3.0] — 2026-07-17

Derivatives, read-only admin, responsive UI. No-spec-impact.

### Operator-facing changes

- **Media derivatives generated on seed/upload.** `aa seed` (and the
  upload path) now produce `col`/`hires`/`screen` thumbnails plus
  `sprites.jpg` video hover-scrub sheets — the browse grid renders real
  thumbnails instead of 404ing, and videos get a slideshow preview.
- **Read-only admin access.** A role can hold `*.read` admin
  capabilities and browse the admin surface **without** the
  `system.admin` superuser cap — six previously superuser-only surfaces
  (activities, featured, license, metadata-extraction, federation,
  requests) now render read-only, and the admin menu + route guard show
  each section per the capability its handler enforces. Backend enforces
  every write regardless.
- **Responsive + accessible UI.** Browse + navbar are fluid from a 390px
  phone to a 3840px / 32:9 ultrawide — an `auto-fill` grid where size is
  the lever and column count is the outcome (no breakpoint cliffs), an
  Instagram-style single-column `feed` view, hide-on-scroll chrome, and
  WCAG 2.2 AA target sizing on coarse pointers. Desktop layout unchanged.
- **Featured content curation** is seeded, so the admin Featured rail and
  the public collections featured tab both show content on a fresh seed.
- **Operator-bug fixes.** `PATCH /admin/system/site` now merges instead
  of blanking omitted fields (was: updating base_url wiped the site
  name); unroutable file extensions no longer mint guaranteed-terminal
  preview jobs; the nightly `ref` dispatch footgun is closed.

### Infrastructure

- CI/nightly stability arc — per-run compose isolation + resource caps,
  and five stacked shared-daemon/host causes fixed; the federation
  nightly is green for the first time since 2026-06-21. Repo-wide `gofmt`
  normalization + a `gofmt -l` CI gate.

## [v0.2.0] — 2026-07-16 — Admin surface unlock + public demo

Post-v0.1.2 incremental work. No-spec-impact.

### Operator-facing changes

- **Admin tiles unlocked (Tier 1–2).** The admin surface is now fully
  navigable: audit-log viewer (`/admin/audit`), per-user active
  sessions + capability grants/revokes, resource requests, **trash**
  with soft-delete restore across assets/posts/collections, system
  log, and an **API explorer served from the Go binary**
  (`/api/v1/openapi.json`, replacing the external-spec fetch).
- **`AA_DEMO_MODE`.** Env-gated demo mode — a `demo`/`demo` credential
  hint + fill button on the sign-in screen and a read-only banner
  when signed in as the demo user. Off by default; zero footprint on
  real installs.
- **Public read-only demo** at `demo.artist-alley.org` — runs the
  release image behind a write-blocking nginx edge, seeded from the
  Layer-A dataset, and auto-redeploys on each release.

## [v0.1.2] — 2026-07-15

> Reconstructed from the `v0.1.1..v0.1.2` commit range — this release was
> tagged without CHANGELOG or GitHub release notes at the time.

Brand, polish, and dependency hygiene; no wire-format changes.

### User-facing changes

- **Burnt/Steel brand.** Repaletted to the burnt accent + steel secondary,
  wired through components; finalized the chevron mark and the configured
  site-name handling; enlarged the sign-in brand mark; added a `viewBox`
  to the favicon/logo SVGs so the browser-tab favicon renders.
- **API docs are cleaner.** A usable getting-started, clearer error
  documentation, and internal phase codes dropped from the published spec
  (the first pass of the ongoing scrub).
- **Install quickstart fixed** — corrected `AA_MASTER_KEY`, the image path,
  the cosign identity, and pgvector setup.

### Fixes

- **Per-type job concurrency caps** are now applied in the single-process
  worker pool.
- **Saved-search notifications** no longer hot-loop — reschedules are
  grid-aligned.

### Infrastructure / housekeeping

- Supply-chain forks retargeted from `mscrnt/*` to `Artist-Alley-Org`.
- `pdfjs-dist` upgraded to v6; dependency sweep clearing Dependabot alerts.
- Test suite isolated from the shared dev database (#291); CI prunes
  dangling images to stop a runner disk leak.
- Real-world IP scrubbed from published surfaces; ArchivePub stamped
  v1.0-final (spec-only).

## [v0.1.1] — 2026-07-13

> Reconstructed from the `v0.1.0..v0.1.1` commit range — tagged without
> notes at the time.

A patch release restoring media processing and clearing shipped-artifact
vulnerabilities.

### Fixes

- **In-process worker pool never claimed jobs** (nil `Types` + a gate
  guard), so media processing silently stalled after v0.1.0. Fixed (#279)
  — this is the reason v0.1.1 exists.
- **GHCR image owner casing** — the org rename broke edge + release image
  pushes; the owner is now lowercased (#280).

### Infrastructure / housekeeping

- Shipped-artifact vulnerabilities cleared (torch floor raised, `aa-clip`
  bumped, npm sweep) — all open Dependabot alerts closed.

## [v0.1.0] — 2026-07-11 — Encryption arc (Phase 1.22.I)

The full encrypted-federation arc (1.22.I-a through 1.22.I-i) is
shipped + dogfood-validated end-to-end. ArchivePub spec at
**v1.0-rc1** with Appendix A conformance test vectors locked.
Seven-day soak window through **2026-06-22**; v1.0 final ships
as a no-code spec-only commit if soak is clean (otherwise
v1.0-rc2 first).

### Operator-facing changes

- **New** `POST /account/security/rotate-federation-keys` —
  user self-rotation of the X25519 federation keypair. Previous
  key is retained for the configured grace window (default 30
  days) so in-flight envelopes still decrypt.
- **New** `POST /admin/federation/users/{ref}/rotate-keys` —
  operator-initiated rotation for compromised-key recovery.
  `rotated_by_user_ref` records the admin's `user.ref` so the
  audit feed distinguishes recovery from self-rotation.
- **New** `GET /admin/federation/key-health` — aggregate
  observability dashboard data: users without a keypair, remote
  actors missing encryption keys, peers without negotiated
  capabilities, retained keys near expiry. Drill-down rows for
  the first + last categories ride along.
- **Behavior** Federation activities for `restricted`-tier
  assets are now encrypted end-to-end via NaCl-box. Senders
  refuse to dispatch when the recipient peer hasn't negotiated
  the `nacl-box` capability OR the recipient's pubkey isn't
  cached locally.
- **Behavior** Receivers reject plaintext envelopes targeting
  `restricted`-tier assets with `reject_reason=encryption_required`
  + audit `federation.inbox.encryption_required_rejected`.
- **Behavior** Asset sensitivity is set at create time (default
  `public`) and consulted by both sender + receiver gates.
  Changing the tier post-create propagates to in-flight
  emissions automatically (intentional: simpler than copy-at-
  grant semantics; a follow-up phase can layer the alternate
  behavior on top if operator feedback demands).

### Wire-format additions

- `aa:encryptionPublicKey` block in actor profile JSON (v0.3).
- `supported_capabilities` field in peer handshake offer /
  confirm envelopes (v0.4).
- `encryption` block in envelope JSON — per-recipient NaCl-box
  ciphertext + sender/recipient key id+version + nonce (v0.5).
- New reject reasons: `decrypt_failed` (v0.6),
  `encryption_required` (active at v1.0-rc1).

### New conformance test vectors

Appendix A of the spec now lists the 8 active scenarios under
`scripts/dogfood/scenarios/` that any conformant ArchivePub
implementation MUST pass against a peer running the reference:

- `01-like-cross-instance` — wire signature + dispatch
- `05-restricted-asset-roundtrip` — receiver-side defense gate
- `06-wire-dispatch` — outbox dispatcher + sub-1s p99
- `07-encryption-key-distribution` — actor profile + remote-actor cache
- `08-capability-negotiation` — handshake intersection
- `09-outbox-encryption-sender-side` — NaCl-box envelope shape
- `11-refusal-flip` — sensitivity-driven sender refusal
- `12-rotation-lifecycle` — rotation + sweeper + admin observability

Scenarios 02, 03, 04 remain outline scripts pending product
wiring (collection share UI, cascade observability).

### Migrations

| # | Schema change | Phase |
|---|---|---|
| 00007 | `federation_user_keys` table — X25519 keypair storage with `is_current` partial unique + multi-version retention | 1.22.I-b |
| 00008 | `federation_remote_actors.encryption_public_key` columns | 1.22.I-c |
| 00009 | `federation_peers.capabilities` + `capabilities_negotiated_at` | 1.22.I-d |
| 00010 | `federation_outbox.was_encrypted` + sender/recipient key version observability | 1.22.I-e |
| 00011 | `federation_inbox.was_encrypted` + `decrypted_with_key_version` | 1.22.I-f |
| 00012 | `federation_outbox.refused_reason` + `status='refused'` admission | 1.22.I-g |
| 00013 | `federation_user_keys.rotated_at` + `rotated_by_user_ref` + `system_config.federation.user_keys.retained_until_days` | 1.22.I-h |
| 00014 | `assets.sensitivity` (tier vocabulary + partial index on restricted/embargo) | 1.22.I-i |

### Backend admin observability

- 3 new audit events: `federation.user.key_rotated`,
  `federation.user.key_retained_expired`,
  `federation.inbox.encryption_required_rejected`.
- Background `userkeys.Sweeper` goroutine — ticks every hour
  with a boot-time first sweep covering downtime expirations;
  emits one audit per non-zero reap (quiet steady state).
- Receiver-side dispatcher stage-3.5 — gates plaintext envelopes
  against the target object's sensitivity tier via the
  `SensitivityLookup` callback (currently resolves `asset`-kind
  objects; other kinds pass through pending their own
  sensitivity columns).

### Out of scope / deferred

- Per-peer policy overrides ("always encrypt to peer X")
- Cross-instance key revocation broadcasts
- Hardware-token / HSM integration
- Algorithm migration mechanics (X25519 → P256 / PQ)
- `federation_shares.sensitivity` copy-at-grant semantics
  (asset-axis sensitivity is the single source of truth at v1.0-rc1)
