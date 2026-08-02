// helpers/testids.ts
//
// Single source of truth for `data-testid` values. Pin a stable
// id here, reference it from both the Svelte component AND the
// Playwright test, and the test stops breaking when copy / i18n /
// role-name / class drift changes the markup.
//
// Add a new testid here BEFORE wiring it in a component or test
// so the catalogue stays the canonical reference. Group by
// surface (nav / form / page) so adjacent ids cluster.
//
// Naming convention:
//   `<surface>-<element>` for atomic controls
//   `<surface>-<element>-<modifier>` for variants
// Use kebab-case. Don't include the verb (we don't say
// `click-upload-button` — `nav-upload-button` is enough; the
// test names the action).

export const testIds = {
  // Top navbar -----------------------------------------------
  nav: {
    brand:            'nav-brand',
    search:           'nav-search',
    advancedSearch:   'nav-advanced-search',
    upload:           'nav-upload-button',
    notifications:    'nav-notifications-button',
    messages:         'nav-messages-button',
    userMenuTrigger:  'nav-user-menu-trigger',
    adminMenuTrigger: 'nav-admin-menu-trigger',
  },

  // User menu dropdown ---------------------------------------
  userMenu: {
    panel:            'user-menu-panel',
    signOut:          'user-menu-sign-out',
    profile:          'user-menu-profile',
    preferences:      'user-menu-preferences',
    tokens:           'user-menu-tokens',
    themeSubmenu:     'user-menu-theme',
    languageSubmenu:  'user-menu-language',
  },

  // Admin menu dropdown --------------------------------------
  adminMenu: {
    panel:            'admin-menu-panel',
    sectionPrefix:    'admin-menu-section-', // suffix with section slug
  },

  // Login form -----------------------------------------------
  login: {
    username:         'login-username',
    password:         'login-password',
    submit:           'login-submit',
    errorBanner:      'login-error-banner',
  },

  // Admin → Content → Site text (#794) ------------------------
  siteText: {
    page:             'site-text-page',
    language:         'site-text-language',
    search:           'site-text-search',
    changedOnly:      'site-text-changed-only',
    list:             'site-text-list',
    row:              'site-text-row',
    count:            'site-text-count',
    toast:            'site-text-toast',
    loadMore:         'site-text-load-more',
    inputPrefix:      'site-text-input-',   // suffix with the i18n key
    savePrefix:       'site-text-save-',    // suffix with the i18n key
    revertPrefix:     'site-text-revert-',  // suffix with the i18n key
  },

  // Generic page surfaces ------------------------------------
  page: {
    errorBoundary:    'page-error-boundary',
    notFound:         'page-not-found',
  },
} as const;

/**
 * Build a Playwright selector that targets a `data-testid` value.
 * Wrap with `page.locator(tid(...))`.
 *
 * Use this instead of inline `[data-testid="..."]` strings so the
 * convention is enforced + a typo throws at the call site.
 */
export function tid(id: string): string {
  return `[data-testid="${id}"]`;
}
