<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/site-text — operator overrides of shipped UI strings
  // (#794, ADR 0081 §1).
  //
  // Every string the app renders comes from a dotted i18n key. This
  // page lists all of them beside what they ship as, lets an operator
  // type a replacement for one language, and lets them put it back.
  //
  // The shipped column comes from the SAME bundled catalogue `t()`
  // resolves against (shippedStrings()), not from a second copy the
  // server sends — so "ships as" cannot drift from what actually
  // renders when an override is removed.
  //
  // Values are PLAIN TEXT. Nothing here is ever rendered as HTML;
  // ADR 0085 keeps rich text the application's only HTML surface.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { lang, t, shippedStrings } from '$stores/lang.svelte';
  import { SUPPORTED_LOCALES } from '$lib/i18n/locales';

  /** How many rows render before "Show more". The catalogue is ~2,150
   *  keys; painting them all costs a visible freeze on a phone, and an
   *  operator looking for one string searches rather than scrolls. */
  const PAGE_SIZE = 40;

  let language = $state('en');
  let query = $state('');
  let overriddenOnly = $state(false);
  let shown = $state(PAGE_SIZE);

  /** key → operator value, for the selected language. Server truth. */
  let overrides = $state<Record<string, string>>({});
  /** key → what's currently in the row's input. */
  let drafts = $state<Record<string, string>>({});
  /** key currently being written, so only that row's button spins. */
  let busyKey = $state<string | null>(null);
  let loading = $state(true);
  let toast = $state<{ kind: 'ok' | 'err'; text: string } | null>(null);

  const shipped = $derived(shippedStrings(language));
  const shippedEn = shippedStrings('en');

  /** Every overridable key, sorted. Derived from the en catalogue
   *  because that is the complete one — a locale with 57 translated
   *  keys must still be able to override all 2,150. */
  const allKeys = Object.keys(shippedEn).sort();

  const matches = $derived.by(() => {
    const q = query.trim().toLowerCase();
    return allKeys.filter((k) => {
      if (overriddenOnly && overrides[k] === undefined) return false;
      if (!q) return true;
      if (k.toLowerCase().includes(q)) return true;
      const base = shipped[k] ?? shippedEn[k] ?? '';
      return base.toLowerCase().includes(q);
    });
  });

  const visible = $derived(matches.slice(0, shown));

  onMount(() => { void load(); });

  // Reset paging whenever the filter changes, so "Show more" never
  // carries a previous search's depth into a new one.
  $effect(() => {
    void query;
    void overriddenOnly;
    void language;
    shown = PAGE_SIZE;
  });

  function flash(kind: 'ok' | 'err', text: string) {
    toast = { kind, text };
    setTimeout(() => { toast = null; }, 4000);
  }

  async function load() {
    loading = true;
    try {
      const r = await api.GET('/site-text');
      const map = (r.data?.overrides ?? {}) as Record<string, Record<string, string>>;
      applyMap(map);
    } finally {
      loading = false;
    }
  }

  function applyMap(map: Record<string, Record<string, string>>) {
    overrides = { ...(map[language] ?? {}) };
    drafts = { ...overrides };
  }

  // Re-slice the server map when the operator switches language.
  // Reloading is cheap and keeps one code path; the map is small.
  async function switchLanguage(next: string) {
    language = next;
    await load();
  }

  function draftFor(key: string): string {
    return drafts[key] ?? '';
  }

  async function save(key: string) {
    if (busyKey) return;
    busyKey = key;
    try {
      const r = await api.PUT('/site-text/{key}', {
        params: { path: { key } },
        body: { language, value: draftFor(key) } as never,
      });
      if (r.error) {
        const status = (r.response as Response | undefined)?.status;
        const msg = (r.error as { error?: string }).error;
        // A 422 names the key server-side; show that message verbatim
        // rather than a generic one — knowing WHICH key was refused is
        // the whole point of the fail-loud rule (ADR 0081 §1).
        if (status === 422) flash('err', msg || t('admin.site_text.err_unknown_key'));
        else if (status === 403) flash('err', t('admin.site_text.err_forbidden'));
        else flash('err', msg || t('admin.site_text.err_generic'));
        return;
      }
      overrides = { ...overrides, [key]: draftFor(key) };
      lang.applyOverride(language, key, draftFor(key));
      flash('ok', t('admin.site_text.saved'));
    } finally {
      busyKey = null;
    }
  }

  async function revert(key: string) {
    if (busyKey) return;
    busyKey = key;
    try {
      const r = await api.DELETE('/site-text/{key}', {
        params: { path: { key }, query: { language } },
      });
      if (r.error) {
        const status = (r.response as Response | undefined)?.status;
        if (status === 403) flash('err', t('admin.site_text.err_forbidden'));
        else flash('err', t('admin.site_text.err_generic'));
        return;
      }
      const nextOv = { ...overrides };
      delete nextOv[key];
      overrides = nextOv;
      const nextDrafts = { ...drafts };
      delete nextDrafts[key];
      drafts = nextDrafts;
      lang.clearOverride(language, key);
      flash('ok', t('admin.site_text.reverted'));
    } finally {
      busyKey = null;
    }
  }
</script>

<svelte:head><title>{t('admin.site_text.title')}</title></svelte:head>

<section class="flex flex-col gap-4 p-4 sm:p-6" data-testid="site-text-page">
  <header>
    <h1 class="text-2xl font-semibold text-fg">{t('admin.site_text.title')}</h1>
    <p class="mt-1 max-w-4xl text-sm text-fg-muted">{t('admin.site_text.intro')}</p>
    <p class="mt-1 text-xs text-fg-muted">{t('admin.site_text.plain_text_note')}</p>
  </header>

  {#if toast}
    <p role="status" data-testid="site-text-toast"
       class={toast.kind === 'ok'
         ? 'rounded border border-success/40 bg-success/10 px-3 py-2 text-sm text-success'
         : 'rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger'}>{toast.text}</p>
  {/if}

  <!-- Controls. Wrap + full-width inputs so the row stacks cleanly at
       390px instead of overflowing horizontally. -->
  <div class="flex flex-wrap items-end gap-3 rounded-lg border border-border bg-surface-elevated p-3">
    <label class="block text-xs">
      <span class="mb-1 block text-fg-muted">{t('admin.site_text.language')}</span>
      <select value={language} onchange={(e) => void switchLanguage((e.currentTarget as HTMLSelectElement).value)}
              class="min-h-11 rounded border border-border-strong bg-surface px-2 py-1 text-sm"
              data-testid="site-text-language">
        {#each SUPPORTED_LOCALES as l (l.code)}
          <option value={l.code}>{l.nativeName}</option>
        {/each}
      </select>
    </label>

    <label class="block min-w-[12rem] flex-1 text-xs">
      <span class="mb-1 block text-fg-muted">{t('admin.site_text.search')}</span>
      <input type="search" bind:value={query} placeholder={t('admin.site_text.search_placeholder')}
             class="min-h-11 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm"
             data-testid="site-text-search" />
    </label>

    <label class="flex min-h-11 items-center gap-2 text-sm text-fg-muted">
      <input type="checkbox" bind:checked={overriddenOnly} class="h-5 w-5" data-testid="site-text-changed-only" />
      {t('admin.site_text.filter_overridden')}
    </label>
  </div>

  {#if loading}
    <p class="text-fg-muted">{t('common.loading')}</p>
  {:else}
    <p class="text-xs text-fg-muted" data-testid="site-text-count">
      {t('admin.site_text.count', { shown: visible.length, total: matches.length })}
    </p>

    {#if matches.length === 0}
      <p class="text-fg-muted" data-testid="site-text-empty">
        {overriddenOnly ? t('admin.site_text.empty_overridden') : t('admin.site_text.empty')}
      </p>
    {:else}
      <ul class="flex flex-col gap-2" data-testid="site-text-list">
        {#each visible as key (key)}
          {@const isOverridden = overrides[key] !== undefined}
          {@const dirty = draftFor(key) !== (overrides[key] ?? '')}
          <li class="rounded-lg border border-border bg-surface-elevated p-3" data-testid="site-text-row">
            <div class="flex flex-wrap items-center gap-2">
              <code class="break-all font-mono text-xs text-fg-muted" data-testid="site-text-key">{key}</code>
              {#if isOverridden}
                <span class="rounded bg-accent/15 px-1.5 py-0.5 text-[0.65rem] uppercase tracking-wide text-accent"
                      data-testid="site-text-changed">{t('admin.site_text.changed_badge')}</span>
              {/if}
            </div>

            <p class="mt-1 text-sm text-fg-muted">
              <span class="text-xs uppercase tracking-wide">{t('admin.site_text.shipped')}</span>
              <span class="ml-1 text-fg">{shipped[key] ?? shippedEn[key] ?? ''}</span>
            </p>

            <div class="mt-2 flex flex-wrap items-end gap-2">
              <label class="block min-w-[12rem] flex-1 text-xs">
                <span class="mb-1 block text-fg-muted">{t('admin.site_text.your_text')}</span>
                <input type="text"
                       value={draftFor(key)}
                       oninput={(e) => { drafts = { ...drafts, [key]: (e.currentTarget as HTMLInputElement).value }; }}
                       class="min-h-11 w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm"
                       data-testid="site-text-input-{key}" />
              </label>
              <button type="button" onclick={() => void save(key)}
                      disabled={busyKey === key || !dirty}
                      class="min-h-11 rounded border border-accent bg-accent/10 px-3 py-1 text-sm font-medium text-accent hover:bg-accent/20 disabled:opacity-40"
                      data-testid="site-text-save-{key}">
                {busyKey === key ? t('admin.site_text.saving') : t('admin.site_text.save')}
              </button>
              {#if isOverridden}
                <button type="button" onclick={() => void revert(key)}
                        disabled={busyKey === key}
                        class="min-h-11 rounded border border-border-strong px-3 py-1 text-sm text-fg-muted hover:border-danger hover:text-danger disabled:opacity-40"
                        data-testid="site-text-revert-{key}">
                  {busyKey === key ? t('admin.site_text.reverting') : t('admin.site_text.revert')}
                </button>
              {/if}
            </div>
          </li>
        {/each}
      </ul>

      {#if matches.length > visible.length}
        <button type="button" onclick={() => { shown += PAGE_SIZE; }}
                class="min-h-11 self-start rounded border border-border-strong px-3 py-1 text-sm text-fg-muted hover:border-accent hover:text-accent"
                data-testid="site-text-load-more">
          {t('admin.site_text.load_more')}
        </button>
      {/if}
    {/if}
  {/if}
</section>
