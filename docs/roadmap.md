# Roadmap

A snapshot of what's shipped, what's in flight, and what's on the
map. Subject to change as we learn what teams actually need.

## Shipped

The current release stream covers the foundations:

- **Single binary deploy.** Go server with the SvelteKit SPA
  embedded via `go:embed`. Multi-arch Docker images (amd64 + arm64),
  `.deb` + `.rpm` packages with systemd unit, static binaries for
  linux / macOS / Windows, Homebrew formula. Every image
  Sigstore-signed.
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

## In flight

These have foundations in place; the rest of the surface area is the
current focus:

- **First tagged release** — `v0.1.0` against the channels above.
  Pre-1.0 means schemas can still break across minors.
- **Image processing pipeline** (Phase 1.18.A — shipped). Variant
  generation (col / preview / screen / hires), thumbhash placeholders,
  content-addressed cache headers, generic background-job queue with
  in-process workers + HTTP claim API for external farms /
  federated peers.
- **Video pipeline + animator review player** (Phase 1.18.B). The
  load-bearing UX: a SyncSketch / Keyframe-Pro-2 replacement built
  into the post modal. See "Review tool" arc below for the full
  feature plan.
- **AI auto-tagging** (Phase 1.14). Use the configured AI providers
  for tag inference on upload + reverse-image search via embeddings.

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

### 1.18.B-3 — Subtitles + multi-format captions
- Native WebVTT track support (`<track>` elements).
- Worker-side conversion of SRT / SSA / ASS / SUB / IDX → WebVTT at
  preview time; tracks stored as `subs/{lang}.vtt` variants.
- Burned-subtitle option for export.
- Per-user subtitle preferences (default language, font size, position).

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
Extends to **PDF page annotations** (the `annotate_pdf` RS pattern):
anchored to `(asset_id, page, x, y, w, h)`; rendered against the PDF
raster preview; exported in the PDF summary with comment thread +
screenshots per anchor. Drawing tool UX patterns borrowed from
`barts_annotations` (pen / highlighter / arrow / text / eraser)
but built clean-room in Svelte.

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

### 1.18.B-15 — Sprite-sheet viewer
- Auto-slice by uniform grid (rows × cols, configurable cell size)
  and by edge-detection (find tightest bounding box per visible
  region) — operator picks the strategy per asset.
- Frame index panel with thumbnails; reorder, name, and group
  frames into named action sequences (idle / walk / attack).
- Animation timeline + scrubber that plays the slices in order with
  configurable FPS, ping-pong, hold-frame, and onion-skin between
  adjacent frames. Reuses the existing video HUD primitives.
- Per-frame metadata stored as anchor-tagged annotations: action
  name, duration, hitbox region, anchor point. Same anchor model
  as 1.18.B-6 so the annotation overlay works on sprite slices for
  free.
- Export individual cels as PNG / WebP, or export the action set
  as a packed sprite sheet for engine import.

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

The phases queued behind the current focus:

- **Search 2.0** (Phase 1.12). Advanced search builder with
  field-level filters, saved searches, smart collections, synonyms +
  boosts, search analytics. Extended for RS feature parity:
  **saved-search alerts** (notify when results change), **endless
  scrolling** in result views as an alternative to pagination,
  **categorical / faceted refinement** with suggested-keyword
  refinements derived from the result set, **keyboard shortcuts**
  for power-user tagging / review (customizable in account
  settings).
- **Identity & teams** (Phase 1.17). Groups + team hierarchy,
  active session management, capability grants. Extended for RS
  feature parity: **table-level change tracking** with before / after
  diffs in the audit log (not just event-level), **user approval
  states** (pending / approved / disabled) with admin approval flow,
  **resource request workflow** (user asks for an asset →
  capability-holder approves or denies → notification fires →
  request lifecycle tracked, including auto-expiry).
- **Integrations** (Phase 1.18). Real LDAP / SAML / OAuth login
  flows, OAuth applications surface, outbound webhooks, notification
  rules + delivery channels. Extended for RS feature parity:
  **email template engine + send queue** with operator-editable
  templates, **timezone-aware delivery** (digest at 8 AM in the
  recipient's TZ), **event-scoped notifications** that link to a
  triggering record and auto-resolve when it resolves (e.g., a
  pending-request notification disappears when the request is
  approved), **conditional download terms** that show a per-resource
  terms page based on metadata (NDA acknowledgement, watermark
  warning, license summary). Chat platforms (Phase 1.30) plug in as
  one of the delivery channels alongside email / in-app / webhook.

## On the map

Larger arcs that will land but aren't the current focus:

- **Storage tooling** (Phase 1.19). Storage usage dashboard, orphan
  cleanup, checksum verification, dedupe UI, bulk re-import,
  backup + restore, database tools. Extended for RS feature parity:
  **scheduled integrity checks** with off-peak windows (verify the
  bytes on disk match the recorded hash, batched against a
  configurable nightly window), **tiered storage / offline archive**
  (move cold assets to S3 Glacier / tape via a per-bucket policy,
  fetch-on-demand surfaces), **per-download bandwidth tracking** so
  storage cost can be attributed to teams / users.
- **Reports & analytics** (Phase 1.20). Asset usage, user activity,
  storage trends, job performance, custom dashboards, scheduled
  reports, drafts, trash, activity log surfaces. Extended for RS
  feature parity: **custom SQL report builder** with placeholder
  variables (date range, team, user), **CSV / Excel / PDF export**
  per report, **report thumbnails** rendered server-side for the
  reports list, **per-collection download analytics**, **scheduled
  email delivery** of any report on a cron expression.
- **Community & moderation** (Phase 1.21). Reports queue, comment
  moderation, banned users / IPs, anonymous browse policy, rate
  limits, bookmarks. Extended for RS feature parity: **comment
  flagging with reason tracking** (spam, abuse, sensitive content,
  off-topic — configurable list), **email escalation to admins** on
  flagged content, **per-comment hide / restore** with audit trail,
  **activity stream surfaced as a structured comments thread** on
  the post-detail modal (system events become quoted-style entries
  alongside human comments — the `barts_log_to_comments` pattern).
- **Federation** (Phase 1.22). Peer servers, inbound + outbound
  feeds, sync status, conflict resolution. The data model already
  carries `origin_server_id` so today's single-instance code is
  forward-compatible.
- **Plugin ecosystem** (Phase 1.23). WASM extension model via
  Extism. In-tree Go packages until external authors arrive.
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
- **RS migration tool** (Phase 1.25). Turnkey path from an existing
  ResourceSpace install to Artist Alley — the largest natural
  conversion audience, especially with Pro / Enterprise scale tiers
  on the table. Two halves: a BSD-3-licensed companion PHP plugin
  installed on the source RS (`mscrnt/aa-rs-migrator`, separate repo)
  that exposes a read-only HTTP API, and an admin-side wizard in
  artist-alley that connects to it, analyses the source server, and
  drives the transfer. The plugin surfaces RS's schema (resource
  types, metadata fields, field definitions, workflow states), the
  resource catalog with metadata + relationships + collections, the
  resource_log audit history, the user / group / permission graph,
  and streams original binaries + preview variants on demand. The AA
  wizard offers field-level mapping with auto-suggestion (RS field →
  AA field, with name + type heuristics), resource-type and
  user-group mapping, and a reviewable plan before execution.
  Sub-phases:
    - **1.25.A — RS companion plugin.** Single-package PHP plugin
      distributed via the standard RS plugin manager. Read-only
      endpoints. Plugin-issued bearer tokens scoped to the
      migrator's read-paths only. Resumable / incremental — the
      plugin exposes a per-resource fingerprint (mtime + size +
      partial hash) so the AA side can skip already-transferred
      content and resume on failure.
    - **1.25.B — Migration wizard.** Admin UI in AA: connect-to-RS
      form, schema analysis, field-mapping table, type / user / group
      mapping, plan preview with expected counts and total transfer
      size. Plan is saved as JSON so it can be reviewed, edited,
      versioned, and replayed.
    - **1.25.C — Migration engine.** Job queue jobs (reusing 1.18.A
      infrastructure) for resource transfer, with worker concurrency
      bounded by license-derived value (consistent with 1.24's
      tangled enforcement). Resumable per-resource. Progress
      surfaced live in the wizard. Dedup against AA's
      content-addressed storage so re-running over the same source
      is cheap.
    - **1.25.D — Validation + cutover.** Post-migration verification
      (counts match, sample hash checks, missing-asset report). Plugin
      can optionally lock the source RS into read-only mode during
      the final cutover window. Generated mapping doc for the
      operator so the URL-rewrite step on their reverse proxy is
      paint-by-number.
  Federation (Phase 1.22) and this share most of the resource-pull
  plumbing — `origin_server_id` on the resource table, the HTTP
  claim API on the job queue, etc. — so 1.25's engine is partly a
  warm-up for federation. Gated on Phase 1.16 (Resource types) and
  Phase 1.17 (Identity & teams) for clean mapping targets.
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
  ADR 0026.
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
