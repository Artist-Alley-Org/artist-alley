<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/content/featured — admin-curated featured list (GitHub
  // #341). Lists current featured assets/collections/teams with remove +
  // reorder, and an add control (kind + subject UUID). Cap-gating is
  // server-side (system.admin); a 403 surfaces as a friendly error.
  //
  // TEAM subjects arrived with #1084, and this page had to move with the
  // migration rather than after it. Every kind-dependent line here was
  // written as a BINARY — `=== 'asset' ? A : B` — so a third kind did
  // not fail loudly, it silently rendered a team as a Collection with a
  // link to /collections/<team-id>, which 404s. That is worse than not
  // shipping: this page is where an operator goes to REMOVE a placement,
  // and it was describing the row wrongly. The ternaries below are now
  // exhaustive lookups, so the next kind breaks the type check instead
  // of the UI.

  import { onMount } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';

  type SubjectKind = 'asset' | 'collection' | 'team';

  /** The route each kind's title links to, and the letter its
   *  no-thumbnail placeholder shows. A record rather than a chain of
   *  ternaries: adding a kind to `SubjectKind` without adding it here is
   *  a type error, which is the only reason this survives the next
   *  subject kind. */
  const SUBJECT_ROUTE: Record<SubjectKind, string> = {
    asset: '/assets',
    collection: '/collections',
    team: '/teams',
  };
  const SUBJECT_LETTER: Record<SubjectKind, string> = {
    asset: 'A',
    collection: 'C',
    team: 'T',
  };
  const SUBJECT_LABEL: Record<SubjectKind, string> = {
    asset: 'admin.featured.kind_asset',
    collection: 'admin.featured.kind_collection',
    team: 'admin.featured.kind_team',
  };

  interface FeaturedItem {
    id: string;
    subject_kind: SubjectKind;
    subject_id: string;
    position: number;
    title: string;
    /** The asset whose col variant renders the tile (#625): the subject
     *  itself for an asset, the hero-card fallback for a collection.
     *  Null when nothing is servable. This — not subject_kind — is what
     *  thumbUrl keys on, because for a collection subject_id is the
     *  COLLECTION and the variant endpoint would 404 on it. */
    cover_asset_id?: string | null;
    asset_file_hash?: string | null;
    /** A servable `col` variant exists — for the asset itself, or for
     *  the collection's resolved cover (#625). On THIS endpoint that is
     *  the whole meaning: the admin curation query deliberately applies
     *  no sensitivity gate ("served to operators who read every tier,
     *  so variant existence alone decides it"), so unlike the public
     *  rail there is no per-caller readability folded in. */
    preview_available?: boolean;
    created_at: string;
  }

  let items = $state<FeaturedItem[]>([]);
  let loading = $state(true);
  let error = $state<string | null>(null);

  // Add form.
  let addKind = $state<SubjectKind>('asset');
  let addId = $state('');
  let adding = $state(false);

  // Transient toast.
  let toast = $state<{ kind: 'ok' | 'err'; text: string } | null>(null);

  onMount(() => { void refresh(); void refreshBand(); });

  function flash(kind: 'ok' | 'err', text: string) {
    toast = { kind, text };
    setTimeout(() => { toast = null; }, 3000);
  }

  async function refresh() {
    loading = true;
    error = null;
    try {
      const r = await api.GET('/admin/featured');
      if (r.error) {
        error = (r.error as { error?: string }).error ?? t('admin.featured.err_generic');
        return;
      }
      const data = r.data as { items?: FeaturedItem[] };
      items = (data.items ?? []) as FeaturedItem[];
    } finally {
      loading = false;
    }
  }

  const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

  async function add() {
    if (adding) return;
    const id = addId.trim();
    if (!UUID_RE.test(id)) {
      flash('err', t('admin.featured.err_bad_id'));
      return;
    }
    adding = true;
    try {
      const r = await api.POST('/admin/featured', {
        body: { subject_kind: addKind, subject_id: id } as never,
      });
      if (r.error || !r.data) {
        const status = (r.response as Response | undefined)?.status;
        flash('err', status === 409 ? t('admin.featured.err_duplicate') : t('admin.featured.err_generic'));
        return;
      }
      addId = '';
      flash('ok', t('admin.featured.added'));
      await refresh();
    } finally {
      adding = false;
    }
  }

  async function remove(id: string) {
    const r = await api.DELETE('/admin/featured/{id}', { params: { path: { id } } });
    if (r.error) {
      flash('err', t('admin.featured.err_generic'));
      return;
    }
    flash('ok', t('admin.featured.removed'));
    await refresh();
  }

  async function move(index: number, delta: number) {
    const target = index + delta;
    if (target < 0 || target >= items.length) return;
    // Optimistic local swap, then persist the whole order.
    const next = items.slice();
    [next[index], next[target]] = [next[target], next[index]];
    items = next;
    const r = await api.PUT('/admin/featured/order', {
      body: { ids: next.map((i) => i.id) } as never,
    });
    if (r.error) {
      flash('err', t('admin.featured.err_generic'));
      await refresh();
      return;
    }
    flash('ok', t('admin.featured.reordered'));
  }

  // #619 keyed this on preview_available instead of the dead has_image;
  // #625 drops the subject-kind gate on top of that. For a collection,
  // subject_id is the COLLECTION id — the variant endpoint would 404 on
  // it — so the gate is cover_asset_id, which the server resolves to the
  // subject itself for an asset and to the hero-card fallback for a
  // collection (same contract as FeaturedRail.svelte's thumbUrl). Both
  // fields present ⇒ a servable col exists, so this never builds a URL
  // that 404s; absent ⇒ null, placeholder, zero requests.
  function thumbUrl(it: FeaturedItem): string | null {
    if (!it.cover_asset_id || !it.asset_file_hash) return null;
    return `/api/v1/assets/${it.cover_asset_id}/variants/col`;
  }

  function subjectHref(it: FeaturedItem): string {
    return `${SUBJECT_ROUTE[it.subject_kind]}/${it.subject_id}`;
  }

  // ── The operator promo band (#1118) ───────────────────────────────
  //
  // The band lives on this page rather than getting one of its own
  // because it is the SAME MECHANISM: its cards are `featured_items`
  // rows (ADR 0065), curated with the same add/remove/reorder verbs, and
  // an operator who wants to know "what is being pushed at readers"
  // should find both answers in one place.
  //
  // What the two lists do NOT share is the surface. The rail list above
  // reads `band_id IS NULL` and this one reads the band's id, so a card
  // never appears in both — an operator pressing "remove" always removes
  // it from the list they are looking at.

  type BandScope = 'org' | 'public';
  interface Band {
    id: string;
    title: string;
    blurb: string;
    cta_label: string;
    cta_url: string;
    enabled: boolean;
    after_page: number;
    scope: BandScope;
    items: FeaturedItem[];
  }

  /** The stored band, or null when the install has none. Distinguished
   *  from "a band with no cards", which is a band and offers a different
   *  next action. */
  let band = $state<Band | null>(null);
  /** The form's own state, kept separate from `band` so an unsaved edit
   *  is never mistaken for what is live. */
  let form = $state({
    title: '',
    blurb: '',
    cta_label: '',
    cta_url: '',
    enabled: false,
    after_page: 1,
    scope: 'org' as BandScope,
  });
  let savingBand = $state(false);
  let bandCardKind = $state<'asset' | 'collection'>('asset');
  let bandCardId = $state('');
  let addingCard = $state(false);

  function loadForm(b: Band | null) {
    band = b;
    form = {
      title: b?.title ?? '',
      blurb: b?.blurb ?? '',
      cta_label: b?.cta_label ?? '',
      cta_url: b?.cta_url ?? '',
      enabled: b?.enabled ?? false,
      after_page: b?.after_page ?? 1,
      scope: b?.scope ?? 'org',
    };
  }

  async function refreshBand() {
    const r = await api.GET('/admin/featured/promo');
    // 404 is "no band yet", an ordinary state and not an error — it is
    // the answer that offers "create" rather than "edit".
    loadForm(r.error ? null : ((r.data as Band | undefined) ?? null));
  }

  async function saveBand() {
    if (savingBand) return;
    savingBand = true;
    try {
      const r = await api.PUT('/admin/featured/promo', { body: { ...form } as never });
      if (r.error || !r.data) {
        const status = (r.response as Response | undefined)?.status;
        flash('err', status === 400 ? t('admin.featured.band_err_cta') : t('admin.featured.err_generic'));
        return;
      }
      loadForm(r.data as Band);
      flash('ok', t('admin.featured.band_saved'));
    } finally {
      savingBand = false;
    }
  }

  async function deleteBand() {
    if (!band || !confirm(t('admin.featured.band_delete_confirm'))) return;
    const r = await api.DELETE('/admin/featured/promo');
    if (r.error) {
      flash('err', t('admin.featured.err_generic'));
      return;
    }
    loadForm(null);
    flash('ok', t('admin.featured.band_deleted'));
  }

  async function addBandCard() {
    if (addingCard) return;
    const id = bandCardId.trim();
    if (!UUID_RE.test(id)) {
      flash('err', t('admin.featured.err_bad_id'));
      return;
    }
    addingCard = true;
    try {
      const r = await api.POST('/admin/featured/promo/items', {
        body: { subject_kind: bandCardKind, subject_id: id } as never,
      });
      if (r.error || !r.data) {
        const status = (r.response as Response | undefined)?.status;
        flash(
          'err',
          status === 409 ? t('admin.featured.band_err_duplicate') : t('admin.featured.err_generic'),
        );
        return;
      }
      bandCardId = '';
      flash('ok', t('admin.featured.added'));
      await refreshBand();
    } finally {
      addingCard = false;
    }
  }

  // Removal and reordering go through the SAME endpoints the rail list
  // uses — a placement id is a placement id — so there is no band-only
  // twin of either to keep in step.
  async function removeBandCard(id: string) {
    const r = await api.DELETE('/admin/featured/{id}', { params: { path: { id } } });
    if (r.error) {
      flash('err', t('admin.featured.err_generic'));
      return;
    }
    flash('ok', t('admin.featured.removed'));
    await refreshBand();
  }

  async function moveBandCard(index: number, delta: number) {
    if (!band) return;
    const target = index + delta;
    if (target < 0 || target >= band.items.length) return;
    const next = band.items.slice();
    [next[index], next[target]] = [next[target], next[index]];
    band.items = next;
    const r = await api.PUT('/admin/featured/order', {
      body: { ids: next.map((i) => i.id) } as never,
    });
    if (r.error) {
      flash('err', t('admin.featured.err_generic'));
      await refreshBand();
      return;
    }
    flash('ok', t('admin.featured.reordered'));
  }
</script>

<svelte:head><title>{t('admin.featured.title')} — {site.name}</title></svelte:head>

<section class="space-y-4">
  <header>
    <h1 class="text-2xl font-semibold text-fg">{t('admin.featured.title')}</h1>
    <p class="mt-1 text-sm text-fg-muted">{t('admin.featured.intro')}</p>
  </header>

  {#if toast}
    <p role="status" class={toast.kind === 'ok'
      ? 'rounded border border-success/40 bg-success/10 px-3 py-2 text-sm text-success'
      : 'rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger'}>{toast.text}</p>
  {/if}

  <!-- Add control -->
  <form class="flex flex-wrap items-end gap-2 rounded-lg border border-border bg-surface-elevated p-3"
        onsubmit={(e) => { e.preventDefault(); void add(); }}>
    <label class="block text-xs">
      <span class="mb-1 block text-fg-muted">{t('admin.featured.kind')}</span>
      <select bind:value={addKind} class="rounded border border-border-strong bg-surface px-2 py-1 text-sm" data-testid="featured-add-kind">
        <option value="asset">{t('admin.featured.kind_asset')}</option>
        <option value="collection">{t('admin.featured.kind_collection')}</option>
        <!-- #1084. The endpoint accepts a team, so the only surface that
             can curate one has to offer it; without this the feature
             would be reachable by curl alone. -->
        <option value="team">{t('admin.featured.kind_team')}</option>
      </select>
    </label>
    <label class="block flex-1 text-xs">
      <span class="mb-1 block text-fg-muted">{t('admin.featured.subject_id')}</span>
      <input type="text" bind:value={addId} placeholder={t('admin.featured.subject_id_placeholder')}
             class="w-full rounded border border-border-strong bg-surface px-2 py-1 font-mono text-sm" data-testid="featured-add-id" />
    </label>
    <button type="submit" disabled={adding}
            class="rounded border border-accent bg-accent/10 px-3 py-1 text-sm font-medium text-accent hover:bg-accent/20 disabled:opacity-50"
            data-testid="featured-add-submit">
      {adding ? t('admin.featured.adding') : t('admin.featured.add')}
    </button>
  </form>

  {#if loading}
    <p class="text-fg-muted">{t('common.loading')}</p>
  {:else if error}
    <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {:else if items.length === 0}
    <p class="text-fg-muted" data-testid="featured-empty">{t('admin.featured.empty')}</p>
  {:else}
    <ul class="space-y-2" data-testid="featured-list">
      {#each items as it, i (it.id)}
        <li class="flex items-center gap-3 rounded-lg border border-border bg-surface-elevated p-3" data-testid="featured-row-{it.id}">
          <div class="flex flex-col gap-1">
            <button type="button" onclick={() => move(i, -1)} disabled={i === 0}
                    class="rounded border border-border px-1 text-xs text-fg-muted hover:border-accent disabled:opacity-30"
                    aria-label={t('admin.featured.move_up')} title={t('admin.featured.move_up')}>▲</button>
            <button type="button" onclick={() => move(i, 1)} disabled={i === items.length - 1}
                    class="rounded border border-border px-1 text-xs text-fg-muted hover:border-accent disabled:opacity-30"
                    aria-label={t('admin.featured.move_down')} title={t('admin.featured.move_down')}>▼</button>
          </div>

          {#if thumbUrl(it)}
            <img src={thumbUrl(it)} alt="" class="h-12 w-12 shrink-0 rounded object-cover" loading="lazy" />
          {:else}
            <div class="flex h-12 w-12 shrink-0 items-center justify-center rounded bg-surface text-xs text-fg-muted">
              {SUBJECT_LETTER[it.subject_kind]}
            </div>
          {/if}

          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2">
              <span class="rounded bg-surface px-1.5 py-0.5 text-xs uppercase text-fg-muted">
                {t(SUBJECT_LABEL[it.subject_kind])}
              </span>
              <a href={subjectHref(it)} class="truncate text-sm font-medium text-accent hover:underline">
                {it.title || t('admin.featured.untitled')}
              </a>
            </div>
            <p class="mt-0.5 font-mono text-xs text-fg-muted">{it.subject_id}</p>
          </div>

          <button type="button" onclick={() => remove(it.id)}
                  class="rounded border border-danger/50 px-2 py-1 text-xs text-danger hover:bg-danger/10"
                  data-testid="featured-remove-{it.id}">
            {t('admin.featured.remove')}
          </button>
        </li>
      {/each}
    </ul>
  {/if}

  <!-- ── The promo band (#1118) ──────────────────────────────────
       A separate surface on the same page: the same curation verbs
       over the same table, pointed at the band instead of the rail. -->
  <section class="mt-8 space-y-3 border-t border-border pt-6" data-testid="promo-band-admin">
    <header>
      <h2 class="text-xl font-semibold text-fg">{t('admin.featured.band_heading')}</h2>
      <p class="mt-1 text-sm text-fg-muted">{t('admin.featured.band_intro')}</p>
      {#if !band}
        <p class="mt-2 text-sm text-fg-muted" data-testid="promo-band-none">
          {t('admin.featured.band_none')}
        </p>
      {/if}
    </header>

    <form
      class="grid gap-3 rounded-lg border border-border bg-surface-elevated p-3 sm:grid-cols-2"
      onsubmit={(e) => { e.preventDefault(); void saveBand(); }}
    >
      <label class="block text-xs sm:col-span-2">
        <span class="mb-1 block text-fg-muted">{t('admin.featured.band_title')}</span>
        <input type="text" bind:value={form.title} required
               placeholder={t('admin.featured.band_title_placeholder')}
               class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm"
               data-testid="promo-band-title" />
      </label>
      <label class="block text-xs sm:col-span-2">
        <span class="mb-1 block text-fg-muted">{t('admin.featured.band_blurb')}</span>
        <input type="text" bind:value={form.blurb}
               placeholder={t('admin.featured.band_blurb_placeholder')}
               class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm"
               data-testid="promo-band-blurb" />
      </label>
      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">{t('admin.featured.band_cta_label')}</span>
        <input type="text" bind:value={form.cta_label}
               placeholder={t('admin.featured.band_cta_label_placeholder')}
               class="w-full rounded border border-border-strong bg-surface px-2 py-1 text-sm"
               data-testid="promo-band-cta-label" />
      </label>
      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">{t('admin.featured.band_cta_url')}</span>
        <input type="text" bind:value={form.cta_url}
               placeholder={t('admin.featured.band_cta_url_placeholder')}
               class="w-full rounded border border-border-strong bg-surface px-2 py-1 font-mono text-sm"
               data-testid="promo-band-cta-url" />
      </label>
      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">{t('admin.featured.band_after_page')}</span>
        <input type="number" min="1" bind:value={form.after_page}
               class="w-24 rounded border border-border-strong bg-surface px-2 py-1 text-sm"
               data-testid="promo-band-after-page" />
        <span class="mt-1 block text-fg-muted">{t('admin.featured.band_after_page_hint')}</span>
      </label>
      <label class="block text-xs">
        <span class="mb-1 block text-fg-muted">{t('admin.featured.band_scope')}</span>
        <select bind:value={form.scope}
                class="rounded border border-border-strong bg-surface px-2 py-1 text-sm"
                data-testid="promo-band-scope">
          <option value="org">{t('admin.featured.band_scope_org')}</option>
          <option value="public">{t('admin.featured.band_scope_public')}</option>
        </select>
      </label>
      <label class="flex items-center gap-2 text-sm sm:col-span-2">
        <input type="checkbox" bind:checked={form.enabled} data-testid="promo-band-enabled" />
        <span class="text-fg">{t('admin.featured.band_enabled')}</span>
      </label>
      <div class="flex items-center gap-2 sm:col-span-2">
        <button type="submit" disabled={savingBand}
                class="rounded border border-accent bg-accent/10 px-3 py-1 text-sm font-medium text-accent hover:bg-accent/20 disabled:opacity-50"
                data-testid="promo-band-save">
          {savingBand ? t('admin.featured.band_saving') : t('admin.featured.band_save')}
        </button>
        {#if band}
          <button type="button" onclick={() => void deleteBand()}
                  class="rounded border border-danger/50 px-3 py-1 text-sm text-danger hover:bg-danger/10"
                  data-testid="promo-band-delete">
            {t('admin.featured.band_delete')}
          </button>
        {/if}
      </div>
    </form>

    {#if band}
      <h3 class="text-sm font-semibold text-fg">{t('admin.featured.band_cards')}</h3>
      <form class="flex flex-wrap items-end gap-2 rounded-lg border border-border bg-surface-elevated p-3"
            onsubmit={(e) => { e.preventDefault(); void addBandCard(); }}>
        <label class="block text-xs">
          <span class="mb-1 block text-fg-muted">{t('admin.featured.kind')}</span>
          <!-- Two kinds, not three: a band renders a COVER, and a team's
               picture is admissible only through the teams rail's
               render-time hero re-check (#982). -->
          <select bind:value={bandCardKind}
                  class="rounded border border-border-strong bg-surface px-2 py-1 text-sm"
                  data-testid="promo-band-card-kind">
            <option value="asset">{t('admin.featured.kind_asset')}</option>
            <option value="collection">{t('admin.featured.kind_collection')}</option>
          </select>
        </label>
        <label class="block flex-1 text-xs">
          <span class="mb-1 block text-fg-muted">{t('admin.featured.subject_id')}</span>
          <input type="text" bind:value={bandCardId}
                 placeholder={t('admin.featured.subject_id_placeholder')}
                 class="w-full rounded border border-border-strong bg-surface px-2 py-1 font-mono text-sm"
                 data-testid="promo-band-card-id" />
        </label>
        <button type="submit" disabled={addingCard}
                class="rounded border border-accent bg-accent/10 px-3 py-1 text-sm font-medium text-accent hover:bg-accent/20 disabled:opacity-50"
                data-testid="promo-band-card-add">
          {addingCard ? t('admin.featured.adding') : t('admin.featured.band_add_card')}
        </button>
      </form>

      {#if band.items.length === 0}
        <p class="text-sm text-fg-muted" data-testid="promo-band-cards-empty">
          {t('admin.featured.band_cards_empty')}
        </p>
      {:else}
        <ul class="space-y-2" data-testid="promo-band-card-list">
          {#each band.items as it, i (it.id)}
            <li class="flex items-center gap-3 rounded-lg border border-border bg-surface-elevated p-3"
                data-testid="promo-band-card-{it.id}">
              <div class="flex flex-col gap-1">
                <button type="button" onclick={() => moveBandCard(i, -1)} disabled={i === 0}
                        class="rounded border border-border px-1 text-xs text-fg-muted hover:border-accent disabled:opacity-30"
                        aria-label={t('admin.featured.move_up')} title={t('admin.featured.move_up')}>▲</button>
                <button type="button" onclick={() => moveBandCard(i, 1)} disabled={i === band.items.length - 1}
                        class="rounded border border-border px-1 text-xs text-fg-muted hover:border-accent disabled:opacity-30"
                        aria-label={t('admin.featured.move_down')} title={t('admin.featured.move_down')}>▼</button>
              </div>
              {#if thumbUrl(it)}
                <img src={thumbUrl(it)} alt="" class="h-12 w-12 shrink-0 rounded object-cover" loading="lazy" />
              {:else}
                <div class="flex h-12 w-12 shrink-0 items-center justify-center rounded bg-surface text-xs text-fg-muted">
                  {SUBJECT_LETTER[it.subject_kind]}
                </div>
              {/if}
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2">
                  <span class="rounded bg-surface px-1.5 py-0.5 text-xs uppercase text-fg-muted">
                    {t(SUBJECT_LABEL[it.subject_kind])}
                  </span>
                  <a href={subjectHref(it)} class="truncate text-sm font-medium text-accent hover:underline">
                    {it.title || t('admin.featured.untitled')}
                  </a>
                </div>
                <p class="mt-0.5 font-mono text-xs text-fg-muted">{it.subject_id}</p>
              </div>
              <button type="button" onclick={() => removeBandCard(it.id)}
                      class="rounded border border-danger/50 px-2 py-1 text-xs text-danger hover:bg-danger/10"
                      data-testid="promo-band-card-remove-{it.id}">
                {t('admin.featured.remove')}
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    {/if}
  </section>
</section>
