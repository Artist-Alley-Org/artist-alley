// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Top-level admin section structure.
//
// This is the canonical menu shape — every admin page belongs to one
// of the sections below. The AdminMenu dropdown, the /admin home
// tile grid, the sidebar in /admin/+layout.svelte, and the dynamic
// /admin/[section]/+page.svelte landing all read from here.
//
// Each tile is one of:
//   status: 'live'   → fully implemented; href is set
//   status: 'stub'   → route exists, shows a placeholder
//   status: 'future' → not built yet; href omitted, shown dimmed
//
// When a future tile graduates to live, flip status + add href. The
// roadmap memo (memory: project_admin_roadmap) records which release
// each future tile is scheduled into — that scheduling is a dev-side
// concern and is deliberately NOT surfaced to operators (#801).

export type TileStatus = 'live' | 'stub' | 'future';

export interface AdminTile {
  key: string;        // unique within section, used for i18n lookup
  href?: string;      // omit for future tiles
  status: TileStatus;
  // Capability the tile's backend GET/list handler enforces (#385),
  // derived from the handler source, not memory. Present on live tiles
  // that a read cap can open; ABSENT means the page requires
  // `system.admin` (no read alternative) — a superuser-only tile.
  // A read-cap holder sees exactly the tiles whose `cap` they hold, so
  // no tile 403s on click.
  cap?: string;
  // Universally visible — the tile's page guards nothing sensitive
  // (help, docs, about, release notes, support) and enforces no
  // server-side cap. Distinct from an absent `cap`, which means
  // superuser-only (#399): most cap-less tiles are unmigrated admin
  // surfaces, NOT public. Only set `public` where the page is safe for
  // any operator who can reach the admin shell.
  public?: boolean;
}

export interface AdminSection {
  slug: string;       // URL segment under /admin/
  iconKey: string;    // names a known icon in AdminIcon.svelte
  tiles: AdminTile[];
}

export const ADMIN_SECTIONS: AdminSection[] = [
  {
    slug: 'identity',
    iconKey: 'identity',
    tiles: [
      { key: 'users',      status: 'live',   href: '/admin/users', cap: 'users.read' },
      { key: 'roles',      status: 'live',   href: '/admin/roles', cap: 'roles.read' },
      { key: 'groups',     status: 'live',   href: '/admin/teams', cap: 'teams.read' },
      // Per-user active sessions are managed on the user-detail page
      // (admin/users/[ref], Phase 1.17.C). The tile lands on the user
      // list; drill into a user to view + revoke their sessions.
      { key: 'sessions',   status: 'live',   href: '/admin/users', cap: 'users.read' },
      // `audit` moved to the automation section (its natural home alongside workflow + triggers).
      // `workflow` moved to the automation section.
      // Per-user grants/revokes are managed on the user-detail page
      // (admin/users/[ref], Phase 1.17.F). The tile lands on the user
      // list, from which an operator drills into a user to edit them.
      { key: 'capability_grants', status: 'live',   href: '/admin/users', cap: 'users.read' },
      { key: 'requests',   status: 'live',   href: '/admin/requests', cap: 'requests.read' },
    ],
  },
  {
    slug: 'content',
    iconKey: 'content',
    tiles: [
      // Gated on the ADMIN cap, not a read cap (#961), for the same
      // reason site_text is gated on its write cap (#794): the list GET
      // (/asset_types) is anonymous — it is the upload type picker — so
      // the page never 403s on load. What it 403s on is the only thing
      // the page is FOR. Every route past the index reads or writes the
      // per-type ACLs, and all three of those endpoints gate on
      // `system.asset_types.admin` (assettype/acls_handler.go).
      //
      // Before this the tile carried no cap, which means superuser-only:
      // `system.admin` holders saw it and nobody else did, so an operator
      // who delegated `system.asset_types.admin` to a named account left
      // them with a capability and no way to find the page. Naming the
      // cap makes the delegation discoverable and keeps the invariant —
      // a visible tile never 403s on click — true in both directions.
      { key: 'asset_types', status: 'live',   href: '/admin/asset-types', cap: 'system.asset_types.admin' },
      { key: 'fields',         status: 'live',   href: '/admin/fields' },
      { key: 'metadata_extraction', status: 'live', href: '/admin/metadata-extraction/failures', cap: 'system.metadata_extraction.read' },
      { key: 'field_options',  status: 'future' },
      // `field_sets` removed 2026-07-31 (#738). The tile promised
      // "bundle related fields for reuse across types", which
      // `display_group` + `applies_to` already do; the underlying
      // field_definition.field_set_id column is dropped by migration
      // 00022 and ADR 0012 is amended with the reasoning.
      { key: 'taxonomy',       status: 'future' },
      // Gated on the WRITE cap, not a read cap (#794). The GET is
      // anonymous — the strings are the UI itself — so the page never
      // 403s on load; what it 403s on is every action. A tile that
      // opens to a list of buttons the caller cannot press is worse
      // than a tile they do not see.
      { key: 'site_text',      status: 'live',   href: '/admin/site-text', cap: 'system.config.write' },
      // Gated on the WRITE cap, not a read cap (#795). The catalogue
      // GET needs system.config.read; a config-read holder sees the
      // tile and the shipped bodies, and the write cap gates every edit.
      { key: 'email_templates', status: 'live', href: '/admin/email-templates', cap: 'system.config.read' },
      { key: 'featured',       status: 'live',   href: '/admin/content/featured', cap: 'featured.read' },
      { key: 'defaults',       status: 'future' },
    ],
  },
  {
    slug: 'storage',
    iconKey: 'storage',
    tiles: [
      { key: 'backends',    status: 'future' },
      { key: 'usage',       status: 'live',   href: '/admin/storage/usage',    cap: 'system.storage.read' },
      { key: 'variants',    status: 'live',   href: '/admin/storage/variants', cap: 'system.storage.read' },
      { key: 'orphans',     status: 'live',   href: '/admin/storage/orphans',   cap: 'system.storage.read' },
      { key: 'duplicates',  status: 'future' },
      { key: 'checksums',   status: 'live',   href: '/admin/storage/checksums', cap: 'system.storage.read' },
      { key: 'reimport',    status: 'future' },
      { key: 'trash',       status: 'live',   href: '/admin/storage/trash' },
    ],
  },
  {
    slug: 'jobs',
    iconKey: 'jobs',
    tiles: [
      { key: 'queue',        status: 'live',   href: '/admin/jobs/queue',   cap: 'system.jobs.read' },
      { key: 'workers',      status: 'live',   href: '/admin/jobs/workers', cap: 'system.jobs.read' },
      { key: 'kinds',        status: 'live',   href: '/admin/jobs/kinds',     cap: 'system.jobs.read' },
      { key: 'failed',       status: 'live',   href: '/admin/jobs/failed',    cap: 'system.jobs.read' },
      { key: 'schedules',    status: 'live',   href: '/admin/jobs/schedules', cap: 'system.jobs.read' },
      { key: 'live',         status: 'live',   href: '/admin/jobs/live',    cap: 'system.jobs.read' },
      { key: 'render_farm',  status: 'future' },
    ],
  },
  {
    slug: 'search',
    iconKey: 'search',
    tiles: [
      { key: 'index',        status: 'future' },
      { key: 'reindex',      status: 'live',   href: '/admin/search/reindex' },
      { key: 'analytics',    status: 'live',   href: '/admin/search/dashboard' },
      { key: 'synonyms',     status: 'future' },
      { key: 'saved',        status: 'live',   href: '/admin/saved-searches' },
      { key: 'smart',        status: 'future' },
    ],
  },
  {
    slug: 'automation',
    iconKey: 'automation',
    tiles: [
      { key: 'workflow',          status: 'live',   href: '/admin/workflow' },
      { key: 'audit',             status: 'live',   href: '/admin/audit', cap: 'system.audit.read' },
      { key: 'triggers',          status: 'future' },
      { key: 'webhooks',          status: 'future' },
      { key: 'notifications',     status: 'future' },
      { key: 'scheduled_exports', status: 'future' },
    ],
  },
  {
    slug: 'moderation',
    iconKey: 'moderation',
    tiles: [
      { key: 'reports',         status: 'future' },
      { key: 'queue',           status: 'future' },
      { key: 'comments',        status: 'future' },
      { key: 'banned',          status: 'future' },
      { key: 'flagging_rules',  status: 'future' },
      // Anonymous/public browsing shipped in v0.5.0 as the `public_mode`
      // operator toggle, which lives on the site-settings page. The tile
      // is a front door to that existing switch (no dedicated page of its
      // own), so it points there and carries the same read cap the site
      // page enforces.
      { key: 'anonymous',       status: 'live',   href: '/admin/system/site', cap: 'system.config.read' },
      { key: 'rate_limits',     status: 'future' },
    ],
  },
  {
    slug: 'federation',
    iconKey: 'federation',
    tiles: [
      { key: 'peers',         status: 'live',   href: '/admin/federation/peers', cap: 'federation.read' },
      { key: 'directories',   status: 'live',   href: '/admin/federation/directories', cap: 'federation.read' },
      { key: 'shares',        status: 'live',   href: '/admin/federation/shares', cap: 'federation.read' },
      { key: 'outbox',        status: 'live',   href: '/admin/federation/outbox', cap: 'federation.read' },
      { key: 'inbox',         status: 'live',   href: '/admin/federation/inbox', cap: 'federation.read' },
      { key: 'blocklist',     status: 'future' },
      { key: 'activitypub',   status: 'future' },
    ],
  },
  {
    slug: 'integrations',
    iconKey: 'integrations',
    tiles: [
      { key: 'api_explorer', status: 'live',   href: '/admin/integrations/api' },
      { key: 'api_tokens',   status: 'future' },
      { key: 'oauth_apps',   status: 'future' },
      { key: 'webhooks',     status: 'future' },
      { key: 'plugins',      status: 'future' },
      { key: 'outbound',     status: 'future' },
    ],
  },
  {
    slug: 'system',
    iconKey: 'system',
    tiles: [
      { key: 'site',         status: 'live',   href: '/admin/system/site', cap: 'system.config.read' },
      { key: 'smtp',         status: 'live',   href: '/admin/system/smtp', cap: 'system.config.read' },
      { key: 'auth',         status: 'live',   href: '/admin/system/auth', cap: 'system.config.read' },
      { key: 'self_edit_gates', status: 'live', href: '/admin/system/users' },
      // The license page (/admin/system/license) shipped with the
      // licensing work but never got a tile — this is its front door.
      { key: 'license',      status: 'live',   href: '/admin/system/license', cap: 'system.license.read' },
      // Phase 1.14.A-bridge — collapsed three AI tiles into one
      // landing tile. /admin/system/ai is now a hub pointing at the
      // provider list, the inference config, the usage dashboard,
      // and the tag-provenance operator doc. Each surface is still
      // directly reachable; the tile grid is the discoverable
      // entry point.
      { key: 'ai',           status: 'live',   href: '/admin/system/ai', cap: 'system.config.read' },
      { key: 'log',          status: 'live',   href: '/admin/system/log', cap: 'system.config.read' },
      { key: 'activities',   status: 'live',   href: '/admin/system/activities', cap: 'system.activities.read' },
      // Federation tiles moved to the dedicated `federation` section
      // — duplicating them under system was confusing UX (the
      // section landing pages would each show partial overlapping
      // sets).
      { key: 'themes',       status: 'live',   href: '/admin/system/themes', cap: 'system.config.read' },
      // #709 — which browse layouts the install offers. Gated on the
      // READ cap like its neighbours in this section, not on the write
      // cap the way site_text is: this page loads through the ADMIN
      // GET, so `system.config.read` is what it actually needs to
      // render. The save button is disabled without the write cap.
      { key: 'browse_views', status: 'live',   href: '/admin/system/browse-views', cap: 'system.config.read' },
      { key: 'maintenance',  status: 'future' },
      { key: 'feature_flags', status: 'future' },
      { key: 'backup',       status: 'future' },
      { key: 'dbtools',      status: 'future' },
      { key: 'performance',  status: 'future' },
      { key: 'health',       status: 'future' },
    ],
  },
  {
    slug: 'reports',
    iconKey: 'reports',
    tiles: [
      { key: 'asset_usage',    status: 'future' },
      { key: 'user_activity',  status: 'future' },
      { key: 'storage_trends', status: 'future' },
      { key: 'job_perf',       status: 'future' },
      { key: 'dashboards',     status: 'future' },
      { key: 'scheduled',      status: 'future' },
      { key: 'export',         status: 'future' },
    ],
  },
  {
    slug: 'tools',
    iconKey: 'tools',
    tiles: [
      // reindex / checksum_verify / find_orphans were placeholder
      // duplicates: each shipped as a real page elsewhere — search
      // reindex at /admin/search/reindex, and the storage integrity
      // sweeps at /admin/storage/checksums + /admin/storage/orphans
      // (v0.4.0, #421). Removed here so there's one canonical home per
      // operation rather than a live page plus a dead "future" tile.
      { key: 'regen_previews',  status: 'future' },
      { key: 're_exif',         status: 'future' },
      { key: 'find_missing',    status: 'future' },
      { key: 'dummy_data',      status: 'future' },
      { key: 'bulk_import',     status: 'future' },
      { key: 'bulk_export',     status: 'future' },
      { key: 'migrate',         status: 'future' },
    ],
  },
  {
    slug: 'help',
    iconKey: 'help',
    tiles: [
      { key: 'docs',         status: 'live',   href: '/admin/help/docs',          public: true },
      { key: 'shortcuts',    status: 'live',   href: '/admin/help/shortcuts',     public: true },
      { key: 'about',        status: 'live',   href: '/admin/about',              public: true },
      { key: 'release_notes', status: 'live',  href: '/admin/help/release-notes', public: true },
      { key: 'support',      status: 'live',   href: '/admin/help/support',       public: true },
    ],
  },
];

export function sectionBySlug(slug: string): AdminSection | undefined {
  return ADMIN_SECTIONS.find((s) => s.slug === slug);
}

// Every capability referenced by a live tile. `canSeeAdmin` uses this to
// decide whether to show the admin entry point at all: a user who holds
// any one of these can open at least one admin surface. Superuser-only
// tiles (no `cap`) aren't here — they don't grant a read-only user the
// menu, `system.admin` does.
export const ADMIN_TILE_CAPS: readonly string[] = [
  ...new Set(
    ADMIN_SECTIONS.flatMap((s) => s.tiles)
      .filter((t) => t.status === 'live' && t.cap)
      .map((t) => t.cap as string),
  ),
];
