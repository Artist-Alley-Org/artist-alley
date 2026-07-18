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
//   status: 'future' → not built yet; href omitted, phase badge shown
//
// When a future tile graduates to live, flip status + add href. The
// roadmap memo (memory: project_admin_roadmap) records which phase
// each future tile is scheduled into.

export type TileStatus = 'live' | 'stub' | 'future';

export interface AdminTile {
  key: string;        // unique within section, used for i18n lookup
  href?: string;      // omit for future tiles
  status: TileStatus;
  phase?: string;     // e.g. "1.17" — only meaningful for status='future'
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
      { key: 'asset_types', status: 'live',   href: '/admin/asset-types' },
      { key: 'fields',         status: 'live',   href: '/admin/fields' },
      { key: 'metadata_extraction', status: 'live', href: '/admin/metadata-extraction/failures', cap: 'system.metadata_extraction.read' },
      { key: 'field_options',  status: 'future', phase: '1.17' },
      { key: 'field_sets',     status: 'future', phase: '1.17' },
      { key: 'taxonomy',       status: 'future', phase: '1.18' },
      { key: 'site_text',      status: 'future', phase: '1.18' },
      { key: 'email_templates', status: 'future', phase: '1.18' },
      { key: 'featured',       status: 'live',   href: '/admin/content/featured', cap: 'featured.read' },
      { key: 'defaults',       status: 'future', phase: '1.18' },
    ],
  },
  {
    slug: 'storage',
    iconKey: 'storage',
    tiles: [
      { key: 'backends',    status: 'future', phase: '1.19' },
      { key: 'usage',       status: 'live',   href: '/admin/storage/usage',    cap: 'system.storage.read' },
      { key: 'variants',    status: 'live',   href: '/admin/storage/variants', cap: 'system.storage.read' },
      { key: 'orphans',     status: 'live',   href: '/admin/storage/orphans',   cap: 'system.storage.read' },
      { key: 'duplicates',  status: 'future', phase: '1.15' },
      { key: 'checksums',   status: 'live',   href: '/admin/storage/checksums', cap: 'system.storage.read' },
      { key: 'reimport',    status: 'future', phase: '1.15' },
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
      { key: 'render_farm',  status: 'future', phase: '1.16' },
    ],
  },
  {
    slug: 'search',
    iconKey: 'search',
    tiles: [
      { key: 'index',        status: 'future', phase: '1.12' },
      { key: 'reindex',      status: 'live',   href: '/admin/search/reindex' },
      { key: 'analytics',    status: 'live',   href: '/admin/search/dashboard' },
      { key: 'synonyms',     status: 'future', phase: '1.12' },
      { key: 'saved',        status: 'live',   href: '/admin/saved-searches' },
      { key: 'smart',        status: 'future', phase: '1.11' },
    ],
  },
  {
    slug: 'automation',
    iconKey: 'automation',
    tiles: [
      { key: 'workflow',          status: 'live',   href: '/admin/workflow' },
      { key: 'audit',             status: 'live',   href: '/admin/audit', cap: 'system.audit.read' },
      { key: 'triggers',          status: 'future', phase: '1.18' },
      { key: 'webhooks',          status: 'future', phase: '1.18' },
      { key: 'notifications',     status: 'future', phase: '1.18' },
      { key: 'scheduled_exports', status: 'future', phase: '1.19' },
    ],
  },
  {
    slug: 'moderation',
    iconKey: 'moderation',
    tiles: [
      { key: 'reports',         status: 'future', phase: '1.21' },
      { key: 'queue',           status: 'future', phase: '1.21' },
      { key: 'comments',        status: 'future', phase: '1.21' },
      { key: 'banned',          status: 'future', phase: '1.21' },
      { key: 'flagging_rules',  status: 'future', phase: '1.21' },
      { key: 'anonymous',       status: 'future', phase: '1.13' },
      { key: 'rate_limits',     status: 'future', phase: '1.21' },
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
      { key: 'blocklist',     status: 'future', phase: '1.22.G' },
      { key: 'activitypub',   status: 'future', phase: '1.22.K' },
    ],
  },
  {
    slug: 'integrations',
    iconKey: 'integrations',
    tiles: [
      { key: 'api_explorer', status: 'live',   href: '/admin/integrations/api' },
      { key: 'api_tokens',   status: 'future', phase: '1.18' },
      { key: 'oauth_apps',   status: 'future', phase: '1.18' },
      { key: 'webhooks',     status: 'future', phase: '1.18' },
      { key: 'plugins',      status: 'future', phase: '1.23' },
      { key: 'outbound',     status: 'future', phase: '1.18' },
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
      { key: 'maintenance',  status: 'future', phase: '1.19' },
      { key: 'feature_flags', status: 'future', phase: '1.20' },
      { key: 'backup',       status: 'future', phase: '1.19' },
      { key: 'dbtools',      status: 'future', phase: '1.19' },
      { key: 'performance',  status: 'future', phase: '1.20' },
      { key: 'health',       status: 'future', phase: '1.16' },
    ],
  },
  {
    slug: 'reports',
    iconKey: 'reports',
    tiles: [
      { key: 'asset_usage',    status: 'future', phase: '1.20' },
      { key: 'user_activity',  status: 'future', phase: '1.20' },
      { key: 'storage_trends', status: 'future', phase: '1.20' },
      { key: 'job_perf',       status: 'future', phase: '1.20' },
      { key: 'dashboards',     status: 'future', phase: '1.20' },
      { key: 'scheduled',      status: 'future', phase: '1.20' },
      { key: 'export',         status: 'future', phase: '1.20' },
    ],
  },
  {
    slug: 'tools',
    iconKey: 'tools',
    tiles: [
      { key: 'reindex',         status: 'future', phase: '1.12' },
      { key: 'regen_previews',  status: 'future', phase: '1.15' },
      { key: 're_exif',         status: 'future', phase: '1.15' },
      { key: 'checksum_verify', status: 'future', phase: '1.19' },
      { key: 'find_orphans',    status: 'future', phase: '1.19' },
      { key: 'find_missing',    status: 'future', phase: '1.19' },
      { key: 'dummy_data',      status: 'future', phase: '1.16' },
      { key: 'bulk_import',     status: 'future', phase: '1.15' },
      { key: 'bulk_export',     status: 'future', phase: '1.19' },
      { key: 'migrate',         status: 'future', phase: '1.19' },
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
