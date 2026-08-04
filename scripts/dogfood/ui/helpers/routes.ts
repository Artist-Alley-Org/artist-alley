// Route manifest — the single source of truth for the smoke
// suite. Every route gets exactly one entry; tests iterate over
// the manifest so adding a new page automatically gains smoke
// coverage. Param routes (e.g. /admin/users/[ref]) carry a
// `param` field with a known-good sentinel value used in tests.

export interface RouteSpec {
  /** Display name for test titles. */
  label: string;
  /** URL path, with concrete params substituted in. */
  path: string;
  /**
   * How a non-admin authenticated user should see this route.
   *   ok       — page renders normally
   *   admin    — requires admin; non-admin should hit a guard
   *   anon-ok  — also serves unauthed (login, setup)
   *   skip     — too dynamic to assert generically (e.g.
   *              messages/[peer] requires a real peer)
   */
  scope: 'ok' | 'admin' | 'anon-ok' | 'skip';
  /** Optional sentinel for `[param]` routes — fixed up at test time. */
  paramTemplate?: string;
}

export const ANONYMOUS_ROUTES: RouteSpec[] = [
  { label: 'Login',     path: '/login', scope: 'anon-ok' },
  { label: 'Setup',     path: '/setup', scope: 'anon-ok' },
  { label: 'Blogs',     path: '/blogs', scope: 'anon-ok' },
];

export const AUTHENTICATED_USER_ROUTES: RouteSpec[] = [
  { label: 'Browse (home)',       path: '/',            scope: 'ok' },
  { label: 'Search',              path: '/search',      scope: 'ok' },
  { label: 'Collections',         path: '/collections', scope: 'ok' },
  { label: 'Review',              path: '/review',      scope: 'ok' },

  // Account section
  { label: 'Account overview',         path: '/account',                    scope: 'ok' },
  { label: 'Account profile',          path: '/account/profile',            scope: 'ok' },
  { label: 'Account preferences',      path: '/account/preferences',        scope: 'ok' },
  { label: 'Account AI preferences',   path: '/account/preferences/ai',     scope: 'ok' },
  { label: 'Account password',         path: '/account/password',           scope: 'ok' },
  { label: 'Account tokens',           path: '/account/tokens',             scope: 'ok' },
  { label: 'Account sessions',         path: '/account/sessions',           scope: 'ok' },
  { label: 'Account notifications',    path: '/account/notifications',      scope: 'ok' },
  { label: 'Account messages',         path: '/account/messages',           scope: 'ok' },
  { label: 'Account blocked users',    path: '/account/blocked',            scope: 'ok' },
  { label: 'Account shared with me',   path: '/account/shared',             scope: 'ok' },
  { label: 'Account saved searches',   path: '/account/saved-searches',     scope: 'ok' },
  { label: 'Account access requests',  path: '/account/requests',           scope: 'ok' },
  { label: 'Account following',        path: '/account/following',          scope: 'ok' },
  { label: 'Account help',             path: '/account/help',               scope: 'ok' },
  { label: 'Account shortcuts',        path: '/account/shortcuts',          scope: 'ok' },

  // Param routes: messages/[peer] requires a known peer ref;
  // skipped in smoke to avoid brittle dependency on seed data.
];

export const ADMIN_ROUTES: RouteSpec[] = [
  // Top-level admin landing pages (catch-all + per-section)
  { label: 'Admin home',                 path: '/admin',                      scope: 'admin' },
  { label: 'Admin about',                path: '/admin/about',                scope: 'admin' },

  // Identity / access (catch-all section landings via /admin/[section])
  { label: 'Admin users list',           path: '/admin/users',                scope: 'admin' },
  { label: 'Admin roles',                path: '/admin/roles',                scope: 'admin' },
  { label: 'Admin teams',                path: '/admin/teams',                scope: 'admin' },

  // Content / metadata
  { label: 'Admin asset types',          path: '/admin/asset-types',          scope: 'admin' },
  { label: 'Admin fields',               path: '/admin/fields',               scope: 'admin' },
  { label: 'Admin workflow',             path: '/admin/workflow',             scope: 'admin' },

  // Federation (5 surfaces shipped in 1.22.D)
  { label: 'Admin federation peers',         path: '/admin/federation/peers',         scope: 'admin' },
  { label: 'Admin federation directories',   path: '/admin/federation/directories',   scope: 'admin' },
  { label: 'Admin federation shares',        path: '/admin/federation/shares',        scope: 'admin' },
  { label: 'Admin federation outbox',        path: '/admin/federation/outbox',        scope: 'admin' },
  { label: 'Admin federation inbox',         path: '/admin/federation/inbox',         scope: 'admin' },

  // Integrations
  { label: 'Admin API explorer',         path: '/admin/integrations/api',     scope: 'admin' },

  // System
  { label: 'Admin system landing',       path: '/admin/system',               scope: 'admin' },
  { label: 'Admin system site',          path: '/admin/system/site',          scope: 'admin' },
  { label: 'Admin system smtp',          path: '/admin/system/smtp',          scope: 'admin' },
  { label: 'Admin system auth',          path: '/admin/system/auth',          scope: 'admin' },
  { label: 'Admin system AI',            path: '/admin/system/ai',            scope: 'admin' },
  { label: 'Admin system activities',    path: '/admin/system/activities',    scope: 'admin' },
  { label: 'Admin system themes',        path: '/admin/system/themes',        scope: 'admin' },
  { label: 'Admin system log',           path: '/admin/system/log',           scope: 'admin' },
  { label: 'Admin system license',       path: '/admin/system/license',       scope: 'admin' },
];

/** Section landings reached via the catch-all /admin/[section]. */
export const ADMIN_CATCHALL_SECTIONS: RouteSpec[] = [
  { label: 'Admin federation landing',   path: '/admin/federation',           scope: 'admin' },
  { label: 'Admin identity landing',     path: '/admin/identity',             scope: 'admin' },
  { label: 'Admin content landing',      path: '/admin/content',              scope: 'admin' },
  { label: 'Admin storage landing',      path: '/admin/storage',              scope: 'admin' },
  { label: 'Admin pipeline landing',     path: '/admin/processing-pipeline',  scope: 'admin' },
  { label: 'Admin search landing',       path: '/admin/search',               scope: 'admin' },
  { label: 'Admin automation landing',   path: '/admin/automation',           scope: 'admin' },
  { label: 'Admin community landing',    path: '/admin/community',            scope: 'admin' },
  { label: 'Admin integrations landing', path: '/admin/integrations',         scope: 'admin' },
  { label: 'Admin reports landing',      path: '/admin/reports',              scope: 'admin' },
  { label: 'Admin maintenance landing',  path: '/admin/maintenance',          scope: 'admin' },
  { label: 'Admin help landing',         path: '/admin/help',                 scope: 'admin' },
];

export const ALL_ROUTES: RouteSpec[] = [
  ...ANONYMOUS_ROUTES,
  ...AUTHENTICATED_USER_ROUTES,
  ...ADMIN_ROUTES,
  ...ADMIN_CATCHALL_SECTIONS,
];
