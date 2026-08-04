// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Per-user account section structure.
//
// Mirrors web/src/lib/admin/sections.ts but for /account/*. The
// sidebar in /account/+layout.svelte, the overview tile grid at
// /account, and the dynamic /account/[stub] catch-all all read
// from here.
//
// Existing live routes (/account/profile, /account/preferences,
// /account/tokens) keep their flat paths — the [stub] route only
// catches slugs that aren't already a static page. As phases land,
// tiles flip from future → stub → live.
//
// What `status` actually does (#600 — it is easy to over-read):
// the tile grid and the sidebar render EVERY item regardless of
// status, with no badge and no filter. The only consumer is
// /account/[stub]/+page.ts. So `status` is a claim about which page
// answers the href, not a visibility switch:
//
//   'live'   → a static +page.svelte owns this href. Asserted by
//              sections.test.ts, so a wrong label fails CI instead of
//              waiting for a user to click through to a 404.
//   'stub'   → served by /account/[stub] as a "coming soon" panel,
//              OR by a static placeholder page of its own (the `ai`
//              tile is the latter).
//   'future' → currently unused here; kept for parity with
//              lib/admin/sections.ts.

export type TileStatus = 'live' | 'stub' | 'future';

export interface AccountItem {
  slug: string;       // last segment under /account/
  group: string;      // matches an AccountGroup.id
  status: TileStatus;
  href: string;       // resolved route (most are /account/{slug})
}

export interface AccountGroup {
  id: string;
  iconKey: string;
}

export const ACCOUNT_GROUPS: AccountGroup[] = [
  { id: 'identity',      iconKey: 'identity' },
  { id: 'communication', iconKey: 'communication' },
  { id: 'workspace',     iconKey: 'workspace' },
  { id: 'activity',      iconKey: 'activity' },
  { id: 'help',          iconKey: 'help' },
];

export const ACCOUNT_ITEMS: AccountItem[] = [
  // Identity
  { slug: 'profile',     group: 'identity', status: 'live', href: '/account/profile' },
  { slug: 'preferences', group: 'identity', status: 'live', href: '/account/preferences' },
  { slug: 'ai',          group: 'identity', status: 'stub', href: '/account/preferences/ai' },
  { slug: 'password',    group: 'identity', status: 'live', href: '/account/password' },
  { slug: 'twofa',       group: 'identity', status: 'live', href: '/account/security/2fa' },
  { slug: 'sessions',    group: 'identity', status: 'live', href: '/account/sessions' },
  { slug: 'blocked',     group: 'identity', status: 'live', href: '/account/blocked' },
  { slug: 'connected',   group: 'identity', status: 'stub', href: '/account/connected' },

  // Communication
  { slug: 'messages',      group: 'communication', status: 'live', href: '/account/messages' },
  { slug: 'notifications', group: 'communication', status: 'live', href: '/account/notifications' },
  { slug: 'subscriptions', group: 'communication', status: 'stub', href: '/account/subscriptions' },

  // Workspace
  { slug: 'shared',        group: 'workspace', status: 'live', href: '/account/shared' },
  // Sits next to `shared` on purpose: one tile is access someone gave
  // you, the other is access you asked for. The page (Phase 1.17.E)
  // shipped without a nav entry, so nothing linked to it until #600.
  { slug: 'requests',      group: 'workspace', status: 'live', href: '/account/requests' },
  { slug: 'tokens',        group: 'workspace', status: 'live', href: '/account/tokens' },
  { slug: 'saved-searches', group: 'workspace', status: 'live', href: '/account/saved-searches' },
  { slug: 'bookmarks',     group: 'workspace', status: 'stub', href: '/account/bookmarks' },
  { slug: 'drafts',        group: 'workspace', status: 'stub', href: '/account/drafts' },
  { slug: 'trash',         group: 'workspace', status: 'stub', href: '/account/trash' },

  // Activity & insights
  { slug: 'activity',  group: 'activity', status: 'stub', href: '/account/activity' },
  { slug: 'stats',     group: 'activity', status: 'stub', href: '/account/stats' },
  { slug: 'following', group: 'activity', status: 'live', href: '/account/following' },

  // Help
  { slug: 'help',      group: 'help', status: 'live', href: '/account/help' },
  { slug: 'shortcuts', group: 'help', status: 'live', href: '/account/shortcuts' },
];

export function itemBySlug(slug: string): AccountItem | undefined {
  return ACCOUNT_ITEMS.find((i) => i.slug === slug);
}

export function itemsByGroup(group: string): AccountItem[] {
  return ACCOUNT_ITEMS.filter((i) => i.group === group);
}
