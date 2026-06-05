<script lang="ts">
  import { onMount } from 'svelte';
  import { theme } from '$stores/theme.svelte';
  import { lang, t } from '$stores/lang.svelte';
  import { api } from '$api/client';

  // Local types mirror the openapi UserPreferencesResponse shape.
  // We don't pull from schema.d.ts directly because the openapi-fetch
  // client returns loosely-typed Records on the maps, and the UI is
  // easier to write against narrow shapes.
  interface ViewSelections {
    home_tab?: string;
    browse_layout?: string;
    browse_sort?: string;
  }
  interface PrefsResponse {
    notification_channels: Record<string, string[]>;
    default_views: ViewSelections;
    known_event_types: string[];
    known_channels: string[];
    default_channels_by_event: Record<string, string[]>;
  }

  // The select-options for the three view knobs. The valid value sets
  // are pinned here client-side; the server also accepts arbitrary
  // strings (it doesn't enforce the enum) so adding a new layout in
  // a later phase is a one-line change here without a migration.
  const HOME_TAB_OPTIONS = ['', 'latest', 'following', 'trending', 'for_you'];
  const BROWSE_LAYOUT_OPTIONS = ['', 'grid', 'masonry', 'thumbnail', 'list'];
  const BROWSE_SORT_OPTIONS = ['', 'newest', 'oldest', 'popular', 'trending'];

  let saved = $state(false);
  let savingPrefs = $state(false);
  let loadError = $state<string | null>(null);
  let prefs = $state<PrefsResponse | null>(null);

  onMount(() => {
    void load();
  });

  async function load(): Promise<void> {
    try {
      const r = await api.GET('/account/preferences');
      if (r.data) {
        prefs = r.data as unknown as PrefsResponse;
      }
    } catch (err) {
      loadError = err instanceof Error ? err.message : t('account.preferences.notif_load_error');
    }
  }

  function pickTheme(p: 'light' | 'dark' | 'system') {
    theme.set(p);
    flashSaved();
  }

  async function pickLocale(code: string) {
    await lang.set(code);
    flashSaved();
  }

  function flashSaved() {
    saved = true;
    setTimeout(() => (saved = false), 2000);
  }

  // Channel pref toggles call this directly — flips the channel on/off
  // for the event in local state, then PATCHes. Server is authoritative
  // (returns the resolved response) and we replace local state from
  // its response so we never drift.
  async function toggleChannel(event: string, channel: string): Promise<void> {
    if (!prefs) return;
    const existing = prefs.notification_channels[event] ?? resolvedChannelsFor(event);
    const next = existing.includes(channel)
      ? existing.filter((c) => c !== channel)
      : [...existing, channel];
    const patch = {
      ...prefs.notification_channels,
      [event]: next,
    };
    await savePrefs({ notification_channels: patch });
  }

  // View-knob change handler — sends just the views section. Empty
  // string means "fall back to default" — we send it explicitly so
  // the server can clear any previously-set value.
  async function setView(key: keyof ViewSelections, value: string): Promise<void> {
    if (!prefs) return;
    await savePrefs({
      default_views: { ...prefs.default_views, [key]: value },
    });
  }

  async function savePrefs(patch: Partial<PrefsResponse>): Promise<void> {
    if (!prefs || savingPrefs) return;
    savingPrefs = true;
    try {
      const body = {
        notification_channels: patch.notification_channels ?? prefs.notification_channels,
        default_views: patch.default_views ?? prefs.default_views,
      };
      const r = await api.PATCH('/account/preferences', { body });
      if (r.data) {
        prefs = r.data as unknown as PrefsResponse;
        flashSaved();
      } else if (r.error) {
        loadError = (r.error as { error?: string } | undefined)?.error ?? t('account.preferences.notif_save_error');
      }
    } finally {
      savingPrefs = false;
    }
  }

  // The effective channel list for an event — explicit user override
  // wins, otherwise falls back to system defaults. Used to render the
  // toggle state in the UI.
  function resolvedChannelsFor(event: string): string[] {
    if (!prefs) return [];
    const override = prefs.notification_channels[event];
    if (override !== undefined) return override;
    return prefs.default_channels_by_event[event] ?? [];
  }

  // True when the user has an explicit override for this event (vs
  // falling back to defaults). Used to render the "follows system
  // default" hint next to each row.
  function hasOverride(event: string): boolean {
    return prefs?.notification_channels[event] !== undefined;
  }
</script>

<svelte:head><title>{t('account.preferences.title')} — artist-alley</title></svelte:head>

<div class="mb-6 flex flex-wrap items-baseline justify-between gap-3">
  <div>
    <h2 class="text-2xl font-semibold">{t('account.preferences.title')}</h2>
    <p class="text-sm text-fg-muted">{t('account.preferences.intro')}</p>
  </div>
  {#if saved}
    <p role="status" class="rounded border border-success/40 bg-success-container px-3 py-1.5 text-sm text-success">
      {t('account.preferences.saved')}
    </p>
  {/if}
</div>

<!-- Top row: theme + language sit side-by-side on wider screens since
     each is a single button row that fits comfortably in its half. -->
<div class="mb-6 grid grid-cols-1 gap-6 lg:grid-cols-2">
  <section class="rounded-lg border border-border bg-surface p-4">
    <h3 class="mb-2 text-sm font-medium text-fg">{t('account.preferences.theme_label')}</h3>
    <div class="flex flex-wrap gap-2">
      {#each [
        { code: 'system' as const, label: t('user_menu.theme_system') },
        { code: 'light' as const,  label: t('user_menu.theme_light') },
        { code: 'dark' as const,   label: t('user_menu.theme_dark') },
      ] as opt (opt.code)}
        <button
          type="button"
          onclick={() => pickTheme(opt.code)}
          class={`rounded-md border px-3 py-1.5 text-sm ${theme.pref === opt.code ? 'border-accent bg-accent/10 text-accent' : 'border-border bg-surface text-fg-muted hover:text-fg'}`}
          aria-pressed={theme.pref === opt.code}
        >
          {opt.label}
        </button>
      {/each}
    </div>
    <p class="mt-2 text-xs text-fg-muted">{t('account.preferences.theme_system_help')}</p>
  </section>

  <section class="rounded-lg border border-border bg-surface p-4">
    <h3 class="mb-2 text-sm font-medium text-fg">{t('account.preferences.language_label')}</h3>
    <div class="flex flex-wrap gap-2">
      <button
        type="button"
        onclick={() => pickLocale('')}
        class={`rounded-md border px-3 py-1.5 text-sm ${lang.pref === '' ? 'border-accent bg-accent/10 text-accent' : 'border-border bg-surface text-fg-muted hover:text-fg'}`}
        aria-pressed={lang.pref === ''}
      >
        {t('user_menu.theme_system')}
      </button>
      {#each lang.locales as l (l.code)}
        <button
          type="button"
          onclick={() => pickLocale(l.code)}
          class={`rounded-md border px-3 py-1.5 text-sm ${lang.pref === l.code ? 'border-accent bg-accent/10 text-accent' : 'border-border bg-surface text-fg-muted hover:text-fg'}`}
          aria-pressed={lang.pref === l.code}
        >
          {l.nativeName}
          {#if l.completionPct < 100}
            <span class="text-xs text-fg-muted">({l.completionPct}%)</span>
          {/if}
        </button>
      {/each}
    </div>
    <p class="mt-2 text-xs text-fg-muted">{t('account.preferences.language_system_help')}</p>
  </section>
</div>

<!-- Bottom row: notifications grid takes the wide column (3 of 4 cols
     on xl); default views sit beside it on a single column. On
     smaller screens they stack. -->
<div class="mb-6 grid grid-cols-1 gap-6 xl:grid-cols-4">
  <section class="rounded-lg border border-border bg-surface p-4 xl:col-span-3">
    <header class="mb-3">
      <h3 class="text-sm font-medium text-fg">{t('account.preferences.notif_title')}</h3>
      <p class="text-xs text-fg-muted">{t('account.preferences.notif_intro')}</p>
    </header>

    {#if loadError}
      <p role="alert" class="rounded border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{loadError}</p>
    {:else if !prefs}
      <p class="text-xs text-fg-muted">{t('common.loading')}</p>
    {:else}
      <div class="overflow-hidden rounded-lg border border-border">
        <table class="w-full text-sm">
          <thead class="bg-surface text-fg-muted">
            <tr>
              <th class="w-full px-4 py-2 text-left font-medium">{t('account.preferences.notif_event_col')}</th>
              {#each prefs.known_channels as ch (ch)}
                <th class="whitespace-nowrap px-6 py-2 text-center font-medium">{t(`account.preferences.notif_channel_${ch}`)}</th>
              {/each}
            </tr>
          </thead>
          <tbody>
            {#each prefs.known_event_types as event (event)}
              {@const channels = resolvedChannelsFor(event)}
              {@const overridden = hasOverride(event)}
              <tr class="border-t border-border hover:bg-surface/50">
                <td class="px-4 py-2.5">
                  <div class="font-medium">{t(`account.preferences.notif_event_${event}`)}</div>
                  {#if !overridden}
                    <div class="text-xs text-fg-muted">{t('account.preferences.notif_default_hint')}</div>
                  {/if}
                </td>
                {#each prefs.known_channels as ch (ch)}
                  <td class="whitespace-nowrap px-6 py-2.5 text-center">
                    <input
                      type="checkbox"
                      class="h-4 w-4"
                      checked={channels.includes(ch)}
                      onchange={() => toggleChannel(event, ch)}
                      disabled={savingPrefs}
                      aria-label={t(`account.preferences.notif_aria_${ch}`, { event: t(`account.preferences.notif_event_${event}`) })}
                    />
                  </td>
                {/each}
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>

  <section class="rounded-lg border border-border bg-surface p-4 xl:col-span-1">
    <header class="mb-3">
      <h3 class="text-sm font-medium text-fg">{t('account.preferences.views_title')}</h3>
      <p class="text-xs text-fg-muted">{t('account.preferences.views_intro')}</p>
    </header>

    {#if prefs}
      <div class="space-y-3">
        <label class="block text-sm">
          <span class="mb-1 block font-medium text-fg">{t('account.preferences.views_home_tab')}</span>
          <select
            class="w-full rounded border border-border bg-bg px-2 py-1.5"
            value={prefs.default_views.home_tab ?? ''}
            onchange={(e) => setView('home_tab', (e.target as HTMLSelectElement).value)}
            disabled={savingPrefs}
          >
            {#each HOME_TAB_OPTIONS as opt (opt)}
              <option value={opt}>{opt === '' ? t('account.preferences.views_use_default') : t(`account.preferences.views_home_tab_${opt}`)}</option>
            {/each}
          </select>
        </label>

        <label class="block text-sm">
          <span class="mb-1 block font-medium text-fg">{t('account.preferences.views_browse_layout')}</span>
          <select
            class="w-full rounded border border-border bg-bg px-2 py-1.5"
            value={prefs.default_views.browse_layout ?? ''}
            onchange={(e) => setView('browse_layout', (e.target as HTMLSelectElement).value)}
            disabled={savingPrefs}
          >
            {#each BROWSE_LAYOUT_OPTIONS as opt (opt)}
              <option value={opt}>{opt === '' ? t('account.preferences.views_use_default') : t(`account.preferences.views_browse_layout_${opt}`)}</option>
            {/each}
          </select>
        </label>

        <label class="block text-sm">
          <span class="mb-1 block font-medium text-fg">{t('account.preferences.views_browse_sort')}</span>
          <select
            class="w-full rounded border border-border bg-bg px-2 py-1.5"
            value={prefs.default_views.browse_sort ?? ''}
            onchange={(e) => setView('browse_sort', (e.target as HTMLSelectElement).value)}
            disabled={savingPrefs}
          >
            {#each BROWSE_SORT_OPTIONS as opt (opt)}
              <option value={opt}>{opt === '' ? t('account.preferences.views_use_default') : t(`account.preferences.views_browse_sort_${opt}`)}</option>
            {/each}
          </select>
        </label>
      </div>
    {/if}
  </section>
</div>
