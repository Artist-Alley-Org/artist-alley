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

export type TileStatus = 'live' | 'stub' | 'future';

export interface AccountItem {
  slug: string;       // last segment under /account/
  group: string;      // matches an AccountGroup.id
  status: TileStatus;
  phase?: string;     // for status='future' or 'stub' with a target phase
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
  { slug: 'ai',          group: 'identity', status: 'stub', phase: '1.14', href: '/account/preferences/ai' },
  { slug: 'password',    group: 'identity', status: 'live', phase: '1.17', href: '/account/password' },
  { slug: 'security',    group: 'identity', status: 'stub', phase: '1.17', href: '/account/security' },
  { slug: 'sessions',    group: 'identity', status: 'live', phase: '1.17', href: '/account/sessions' },
  { slug: 'connected',   group: 'identity', status: 'stub', phase: '1.18', href: '/account/connected' },

  // Communication
  { slug: 'messages',      group: 'communication', status: 'stub', phase: '1.21', href: '/account/messages' },
  { slug: 'notifications', group: 'communication', status: 'stub', phase: '1.18', href: '/account/notifications' },
  { slug: 'subscriptions', group: 'communication', status: 'stub', phase: '1.13', href: '/account/subscriptions' },

  // Workspace
  { slug: 'tokens',        group: 'workspace', status: 'live', href: '/account/tokens' },
  { slug: 'saved-searches', group: 'workspace', status: 'stub', phase: '1.12', href: '/account/saved-searches' },
  { slug: 'bookmarks',     group: 'workspace', status: 'stub', phase: '1.21', href: '/account/bookmarks' },
  { slug: 'drafts',        group: 'workspace', status: 'stub', phase: '1.20', href: '/account/drafts' },
  { slug: 'trash',         group: 'workspace', status: 'stub', phase: '1.19', href: '/account/trash' },

  // Activity & insights
  { slug: 'activity',  group: 'activity', status: 'stub', phase: '1.20', href: '/account/activity' },
  { slug: 'stats',     group: 'activity', status: 'stub', phase: '1.20', href: '/account/stats' },
  { slug: 'following', group: 'activity', status: 'stub', phase: '1.13', href: '/account/following' },

  // Help
  { slug: 'help',      group: 'help', status: 'stub', phase: '1.16', href: '/account/help' },
  { slug: 'shortcuts', group: 'help', status: 'stub', phase: '1.16', href: '/account/shortcuts' },
];

export function itemBySlug(slug: string): AccountItem | undefined {
  return ACCOUNT_ITEMS.find((i) => i.slug === slug);
}

export function itemsByGroup(group: string): AccountItem[] {
  return ACCOUNT_ITEMS.filter((i) => i.group === group);
}
