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
    // The link to the search SURFACE. Renamed from `advancedSearch` /
    // `nav-advanced-search` in #850: the label named `/search/advanced`,
    // a page that no longer exists — the builder is a panel inside
    // /search now. It was also registered here and never applied to the
    // element, which is why the two specs that use it located it by
    // accessible name and both broke the moment the name changed.
    searchPage:       'nav-search-page',
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

  // Account → tiles that stopped being placeholders (#600) -----
  account: {
    // The overview grid. Scope tile locators to it — the /account
    // sidebar renders the same hrefs, so an unscoped
    // a[href="/account/x"] is a strict-mode violation.
    tiles:              'account-tiles',
    followingTabPrefix: 'following-tab-',  // suffix: following | followers
    followingTable:     'following-table',
    followingEmpty:     'following-empty',
    requestsList:       'requests-list',
    requestsEmpty:      'requests-empty',
    helpLinks:          'help-links',
    shortcutsGroups:    'shortcuts-groups',
    shortcutGroupPrefix: 'shortcut-group-', // suffix with the group id
  },

  // Browse surfaces ------------------------------------------
  //
  // The three `nativeDrag` consumers (#1138). They are grouped rather
  // than scattered because the drag guard drives all three in one
  // sweep — a consumer that loses its id here is a consumer that
  // silently stops being covered.
  browse: {
    wall:                'browse-wall',
    marqueeBand:         'marquee-band',
    featuredRailScroller:'featured-rail-scroller',
    featuredRailItem:    'featured-rail-item',
    teamsRailScroller:   'teams-rail-scroller',
    teamsRailChip:       'teams-rail-chip',
    // The footer control bar, and the asset-type filter that joined the
    // sort toggle in its right cluster (#1166). The bar is shared by
    // browse, the profile pages and post-by-asset; the FILTER is
    // browse-only, which is why both ids are registered — the specs pin
    // its presence on browse and its absence everywhere else.
    viewControls:        'view-controls',
    kindFilterToggle:    'kind-filter-toggle',
    kindFilterPanel:     'kind-filter-panel',
    kindFilterAll:       'kind-filter-all',
    kindFilterOption:    'kind-filter-option',
    kindFilterApply:     'kind-filter-apply',
    // The card's kind badge, which is what a type-filtered wall has to
    // agree with. `cardKind` is the single-asset glyph; `cardKindMulti`
    // is the count-plus-Shapes badge a multi-asset post draws instead.
    cardKind:            'card-kind',
    cardKindMulti:       'card-kind-multi',
    // The thumbnail card's top chrome band and the FORMAT it states —
    // one file's extension, the extension every readable member shares,
    // or the word "mixed" (#1190).
    thumbBandTop:        'thumb-band-top',
    thumbBandExtension:  'thumb-band-extension',
    // The empty wall, and the two lines that have to name what emptied
    // it: the type filter and the Following scope leave no other trace
    // on the page, and a reader who cannot see the narrowing reads an
    // honestly empty result as a broken one (#1190).
    empty:               'browse-empty',
    emptyTitle:          'browse-empty-title',
    emptyHint:           'browse-empty-hint',
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
