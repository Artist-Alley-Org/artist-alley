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

## In flight

These have foundations in place; the rest of the surface area is the
current focus:

- **First tagged release** — `v0.1.0` against the channels above.
  Pre-1.0 means schemas can still break across minors.
- **Image processing pipeline** (Phase 1.15). Variant generation
  (col / thumb / med / big), EXIF/IPTC/XMP parsing, video thumbs,
  job queue dashboard.
- **AI auto-tagging** (Phase 1.14). Use the configured AI providers
  for tag inference on upload + reverse-image search via embeddings.

## Up next

The phases queued behind the current focus:

- **Search 2.0** (Phase 1.12). Advanced search builder with
  field-level filters, saved searches, smart collections,
  synonyms + boosts, search analytics.
- **Identity & teams** (Phase 1.17). Groups + team hierarchy,
  active session management, audit log, capability grants.
- **Integrations** (Phase 1.18). Real LDAP / SAML / OAuth login
  flows, OAuth applications surface, outbound webhooks, notification
  rules + delivery channels (in-app + email).

## On the map

Larger arcs that will land but aren't the current focus:

- **Storage tooling** (Phase 1.19). Storage usage dashboard,
  orphan cleanup, checksum verification, dedupe UI, bulk re-import,
  backup + restore, database tools.
- **Reports & analytics** (Phase 1.20). Asset usage, user activity,
  storage trends, job performance, custom dashboards, scheduled
  reports, drafts, trash, activity log surfaces.
- **Community & moderation** (Phase 1.21). Reports queue, comment
  moderation, banned users / IPs, anonymous browse policy, rate
  limits, bookmarks.
- **Federation** (Phase 1.22). Peer servers, inbound + outbound
  feeds, sync status, conflict resolution. The data model already
  carries `origin_server_id` so today's single-instance code is
  forward-compatible.
- **Plugin ecosystem** (Phase 1.23). WASM extension model via
  Extism. In-tree Go packages until external authors arrive.

## Things we deliberately aren't building

- A general-purpose DAM. We're shaped specifically for studio
  art-review workflows.
- A pipeline integration platform. We're a destination for finished
  and in-progress art, not a Perforce / Shotgrid replacement.
- A SaaS product. Self-hosted by design.

---

This roadmap is the **canonical** order — the labelled phase tags
inside the admin menu (the muted "Phase 1.X" pills on future tiles)
reference these same numbers. When the order changes, this file moves
first.
