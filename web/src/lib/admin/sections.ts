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
      { key: 'users',      status: 'live',   href: '/admin/users' },
      { key: 'roles',      status: 'live',   href: '/admin/roles' },
      { key: 'groups',     status: 'live',   href: '/admin/teams' },
      // Per-user active sessions are managed on the user-detail page
      // (admin/users/[ref], Phase 1.17.C). The tile lands on the user
      // list; drill into a user to view + revoke their sessions.
      { key: 'sessions',   status: 'live',   href: '/admin/users' },
      // `audit` moved to the automation section (its natural home alongside workflow + triggers).
      // `workflow` moved to the automation section.
      // Per-user grants/revokes are managed on the user-detail page
      // (admin/users/[ref], Phase 1.17.F). The tile lands on the user
      // list, from which an operator drills into a user to edit them.
      { key: 'capability_grants', status: 'live',   href: '/admin/users' },
      { key: 'requests',   status: 'live',   href: '/admin/requests' },
    ],
  },
  {
    slug: 'content',
    iconKey: 'content',
    tiles: [
      { key: 'asset_types', status: 'live',   href: '/admin/asset-types' },
      { key: 'fields',         status: 'live',   href: '/admin/fields' },
      { key: 'metadata_extraction', status: 'live', href: '/admin/metadata-extraction/failures' },
      { key: 'field_options',  status: 'future', phase: '1.17' },
      { key: 'field_sets',     status: 'future', phase: '1.17' },
      { key: 'taxonomy',       status: 'future', phase: '1.18' },
      { key: 'site_text',      status: 'future', phase: '1.18' },
      { key: 'email_templates', status: 'future', phase: '1.18' },
      { key: 'featured',       status: 'future', phase: '1.14' },
      { key: 'defaults',       status: 'future', phase: '1.18' },
    ],
  },
  {
    slug: 'storage',
    iconKey: 'storage',
    tiles: [
      { key: 'backends',    status: 'future', phase: '1.19' },
      { key: 'usage',       status: 'future', phase: '1.19' },
      { key: 'variants',    status: 'future', phase: '1.15' },
      { key: 'orphans',     status: 'future', phase: '1.19' },
      { key: 'duplicates',  status: 'future', phase: '1.15' },
      { key: 'checksums',   status: 'future', phase: '1.19' },
      { key: 'reimport',    status: 'future', phase: '1.15' },
      { key: 'trash',       status: 'future', phase: '1.19' },
    ],
  },
  {
    slug: 'jobs',
    iconKey: 'jobs',
    tiles: [
      { key: 'queue',        status: 'future', phase: '1.15' },
      { key: 'workers',      status: 'future', phase: '1.15' },
      { key: 'kinds',        status: 'future', phase: '1.15' },
      { key: 'failed',       status: 'future', phase: '1.15' },
      { key: 'schedules',    status: 'future', phase: '1.15' },
      { key: 'live',         status: 'future', phase: '1.15' },
      { key: 'render_farm',  status: 'future', phase: '1.16' },
    ],
  },
  {
    slug: 'search',
    iconKey: 'search',
    tiles: [
      { key: 'index',        status: 'future', phase: '1.12' },
      { key: 'reindex',      status: 'future', phase: '1.12' },
      { key: 'analytics',    status: 'future', phase: '1.12' },
      { key: 'synonyms',     status: 'future', phase: '1.12' },
      { key: 'saved',        status: 'future', phase: '1.12' },
      { key: 'smart',        status: 'future', phase: '1.11' },
    ],
  },
  {
    slug: 'automation',
    iconKey: 'automation',
    tiles: [
      { key: 'workflow',          status: 'live',   href: '/admin/workflow' },
      { key: 'audit',             status: 'live',   href: '/admin/audit' },
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
      { key: 'peers',         status: 'live',   href: '/admin/federation/peers' },
      { key: 'directories',   status: 'live',   href: '/admin/federation/directories' },
      { key: 'shares',        status: 'live',   href: '/admin/federation/shares' },
      { key: 'outbox',        status: 'live',   href: '/admin/federation/outbox' },
      { key: 'inbox',         status: 'live',   href: '/admin/federation/inbox' },
      { key: 'blocklist',     status: 'future', phase: '1.22.G' },
      { key: 'activitypub',   status: 'future', phase: '1.22.K' },
    ],
  },
  {
    slug: 'integrations',
    iconKey: 'integrations',
    tiles: [
      { key: 'api_explorer', status: 'stub',   href: '/admin/integrations/api' },
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
      { key: 'site',         status: 'live',   href: '/admin/system/site' },
      { key: 'smtp',         status: 'live',   href: '/admin/system/smtp' },
      { key: 'auth',         status: 'live',   href: '/admin/system/auth' },
      { key: 'self_edit_gates', status: 'live', href: '/admin/system/users' },
      // Phase 1.14.A-bridge — collapsed three AI tiles into one
      // landing tile. /admin/system/ai is now a hub pointing at the
      // provider list, the inference config, the usage dashboard,
      // and the tag-provenance operator doc. Each surface is still
      // directly reachable; the tile grid is the discoverable
      // entry point.
      { key: 'ai',           status: 'live',   href: '/admin/system/ai' },
      { key: 'log',          status: 'live',   href: '/admin/system/log' },
      { key: 'activities',   status: 'live',   href: '/admin/system/activities' },
      // Federation tiles moved to the dedicated `federation` section
      // — duplicating them under system was confusing UX (the
      // section landing pages would each show partial overlapping
      // sets).
      { key: 'themes',       status: 'live',   href: '/admin/system/themes' },
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
      { key: 'docs',         status: 'future', phase: '1.16' },
      { key: 'shortcuts',    status: 'future', phase: '1.16' },
      { key: 'about',        status: 'live',   href: '/admin/about' },
      { key: 'release_notes', status: 'future', phase: '1.16' },
      { key: 'support',      status: 'future', phase: '1.16' },
    ],
  },
];

export function sectionBySlug(slug: string): AdminSection | undefined {
  return ADMIN_SECTIONS.find((s) => s.slug === slug);
}
