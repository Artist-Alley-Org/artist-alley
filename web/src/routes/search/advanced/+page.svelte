<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The Advanced Search PAGE (#1157).
  //
  // The control beside the nav box used to read "Search" and open the
  // result surface, which said nothing about what it was for. It is now
  // "Advanced search" and opens this page: the conditional search the
  // owner already likes, PLUS a filter row per searchable metadata field,
  // PLUS the search-by-image arm when the instance has that channel.
  //
  // # ONE QUERY REPRESENTATION (#1067), and what that rules out
  //
  // This page composes nothing of its own. It produces exactly the two
  // things /search already executes:
  //
  //   - `dsl=` — the compiled conditional query, from the SAME
  //     AdvancedQueryBuilder component the slide-over hosts. Not a copy
  //     of it: the component is imported, so its field whitelist cannot
  //     drift from the one the panel uses.
  //   - repeated `filter=<dimension>:<value>` — the facet selection.
  //     Field filters are `filter=field:<code>=<value>`, which is one
  //     more DIMENSION on the existing grammar and not a second query
  //     language. #907's doc makes that the explicit extension point,
  //     and #910's `collection:` is the precedent.
  //
  // So submitting navigates to `/search?…` and the results land on the
  // ordinary browse-shaped surface with the query in the URL — the same
  // address anyone could have typed, shared or bookmarked.
  //
  // # Why the field rows render from `field_definition` and not facets
  //
  // Because the vocabulary is the FIELD's, not the result set's. A
  // facet bucket lists values that occur in the current results; a field
  // definition lists the values an operator DEFINED. An advanced form is
  // asked before there is a result set, so it has to read the
  // definitions — `GET /fields`, whose `options` carry the vocabulary.
  //
  // # read_capability gating
  //
  // `GET /fields` returns `read_capability` as DATA and does not filter
  // (verified: metadata/handler.go ListFields maps every row through
  // with no capability check). So a field the caller cannot read must
  // not be OFFERED here, and this page drops those rows. That is the
  // display half only — the load-bearing half is server-side, in
  // facet.Selection.Authorize, which refuses a `field:` term naming a
  // field this caller may not read regardless of what any client sends.
  //
  // # Two things #1191 changed
  //
  // 1. The words. This page shipped reading "Compiled DSL" and "the
  //    server-side DSL parser" — vocabulary from the implementation,
  //    printed at the person using it. Every string it renders is plain
  //    now. The preview of what will run STAYS, because seeing the
  //    search you built is how you learn to build one; it just has a
  //    name a person can act on.
  // 2. The scale. Every field rendered its ENTIRE vocabulary as chips,
  //    which is right at a dozen values and unusable at a thousand.
  //    Past CHIP_LIMIT a field's row becomes a typeahead instead — see
  //    the constant, which also records where this stops and #1173
  //    begins.

  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { t } from '$stores/lang.svelte';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { site } from '$stores/site.svelte';
  import AdvancedQueryBuilder from '$components/search/AdvancedQueryBuilder.svelte';
  import ReverseImageDropzone from '$components/search/ReverseImageDropzone.svelte';
  import VocabularyCombobox from '$components/VocabularyCombobox.svelte';
  import { selectableOptions, normalizeOptions, type FieldOption } from '$lib/fieldOptions';

  // The nav control carries whatever was in the box, so a caller who
  // typed something and then reached for "Advanced" does not lose it.
  // It seeds the BUILDER's free-text row rather than a second input of
  // this page's own: two free-text boxes on one form is two answers to
  // one question, and only one of them can reach the server.
  const initialQuery = page.url.searchParams.get('q') ?? '';

  type FieldDef = {
    id: string;
    code: string;
    label: string;
    type: string;
    options?: unknown;
    searchable?: boolean;
    show_in_advanced_search?: boolean;
    status?: string;
    subject_kind?: string;
    read_capability?: string | null;
    display_order?: number;
    display_group?: string;
  };

  let fields = $state<FieldDef[]>([]);
  let fieldsLoading = $state(true);
  let fieldsError = $state('');

  // THE FIRST TRANCHE (#1157): the controlled vocabularies. `select`
  // and `tree` store one slug in `value_text`; `multi_select` stores an
  // array in `value_options`. The backend dimension matches BOTH
  // columns in one expression, so all three render the same picker here
  // and the client never has to know which column a field uses.
  //
  // `text`/`date` are deliberately NOT rendered — see the note at the
  // bottom of this file.
  const VOCAB_TYPES = ['select', 'multi_select', 'tree'];

  /**
   * Fields this caller may both read and filter on.
   *
   * # THE OPERATOR'S ANSWER, NOT THIS PAGE'S GUESS (#1173, ADR 0092 §3)
   *
   * This list used to be inferred, and `searchable !== false` was the
   * inference that mattered: `searchable` has meant "this field's text
   * feeds the search index" since the 00001 baseline —
   * `rebuild_asset_search_text()` reads it — and answering "should
   * there be a control for it here" with it conflated two independent
   * settings. An operator who wanted `production_notes` off this form
   * had to make it unfindable to get it, and an operator who unticked
   * `searchable` for indexing reasons silently lost the filter.
   *
   * `show_in_advanced_search` is the field's own declaration and is
   * what this page reads now. ADR 0092 §3: "surfaces read the flags;
   * they do not infer participation from a field's type or from
   * whether it happens to have values."
   *
   * `!== false` and not `=== true`, which is the whole safety property
   * of the change: the column defaults TRUE, so every field that
   * appeared here before the flag existed still appears, and an install
   * that never opens the admin page renders identically.
   *
   * # What is NOT participation, and stays
   *
   * `VOCAB_TYPES` is still here and is not the inference that was
   * removed. It says what this page can DRAW — a picker over a
   * vocabulary — and a `number` or `date` field has no control on this
   * form to render into yet. That is this page's own limit, not a
   * statement about the field, and it lifts when the operator grammar
   * (#1165, folded into #1173) gives ranges and text operators their
   * controls. Until then, ticking the flag on a `text` field stores the
   * operator's intent and this page renders nothing for it — which is
   * why the admin form says which surfaces read the flag so far rather
   * than hiding the toggle.
   *
   * `status !== 'archived'` also stays, and it is not an inference
   * either: `status` IS the retire-without-delete flag ADR 0092 §3
   * asks for, already built (00001 + ArchiveFieldDefinition). It is
   * belt-and-braces here because the fetch below already asks for
   * `status: active`.
   *
   * The read gate is unchanged and composes ON TOP: a field the caller
   * may not read is dropped whatever its participation flag says. The
   * load-bearing half of that is server-side in
   * `facet.Selection.Authorize`; this is the display half.
   */
  const filterable = $derived(
    fields
      .filter((f) => f.show_in_advanced_search !== false && f.status !== 'archived')
      .filter((f) => VOCAB_TYPES.includes(f.type))
      .filter((f) => !f.read_capability || auth.can(f.read_capability))
      .sort((a, b) => (a.display_order ?? 0) - (b.display_order ?? 0) || a.label.localeCompare(b.label)),
  );

  /** code -> chosen values. Multiple values in one field OR together,
   *  which is the facet grammar's own rule for non-tag dimensions. */
  let chosen = $state<Record<string, string[]>>({});

  /**
   * How many values a field may have before its row stops being a wall
   * of chips and becomes a type-to-filter box (#1191).
   *
   * 20 rather than a rounder-feeling 12 or 16, and the reason is
   * measured rather than aesthetic: the largest vocabulary the product
   * SHIPS is `keywords` at 17 terms (migration 00024). A threshold
   * under that would change how a brand-new install's own field looks
   * on the day it is installed, which is a redesign of the default
   * rather than a concession to scale. 20 clears the shipped list with
   * headroom and is still a chip row you take in at a glance — at this
   * page's widths that is two or three wrapped lines.
   *
   * A vocabulary that has GROWN past 20 does get the typeahead, and
   * that is the point: `keywords` is an open vocabulary, so a working
   * instance's copy of it is exactly the field that stops fitting.
   *
   * # The boundary this constant does NOT cross (#1173)
   *
   * This is a DISPLAY threshold, not a transport one. `GET /fields`
   * ships every field's whole vocabulary to the browser either way, so
   * the typeahead below filters an array that is already in memory and
   * needs no endpoint. A vocabulary too large to SHIP is a different
   * problem with a different fix — server-side value search, which is
   * #1173's dynamic-keyword work. Raising this number is free; the
   * payload is what eventually is not.
   */
  const CHIP_LIMIT = 20;

  function setValues(code: string, values: string[]) {
    chosen = { ...chosen, [code]: values };
  }

  function toggleValue(code: string, value: string) {
    const cur = chosen[code] ?? [];
    setValues(code, cur.includes(value) ? cur.filter((v) => v !== value) : [...cur, value]);
  }

  /** The field's whole vocabulary, normalised. */
  function vocabFor(f: FieldDef): FieldOption[] {
    try {
      return normalizeOptions(f.options);
    } catch {
      return [];
    }
  }

  /**
   * Whether this field's row is a typeahead rather than chips.
   *
   * `tree` is excluded on purpose and not by oversight: a tree's
   * vocabulary is a shape, not a list, and flattening it into a
   * typeahead would lose the only thing that makes it a tree. Its own
   * picker is a separate piece of work; until then it keeps exactly the
   * rendering it has.
   */
  function usesTypeahead(f: FieldDef, offered: FieldOption[]): boolean {
    return f.type !== 'tree' && offered.length > CHIP_LIMIT;
  }

  const activeCount = $derived(
    Object.values(chosen).reduce((n, vs) => n + vs.length, 0),
  );

  // Whether the instance has the CLIP/similarity channel.
  //
  // #1163 closed the residual this comment used to describe: the flag is
  // now DECLARED on the /appearance boot payload the app already fetches
  // (`visual_search_enabled`), so the arm is absent on an install without
  // the channel rather than visible until someone drops an image and
  // gets a 501 back.
  //
  // The `oncapability` signal is kept as the second channel, not as the
  // first: the boot flag is resolved once at process start, so an
  // operator who turns the sidecar off mid-session is still caught by
  // the response the component reads. `false` from either source hides
  // the section.
  let clipDenied = $state(false);
  const clipEnabled = $derived(site.visualSearchEnabled && !clipDenied);

  // The compiled DSL the builder last produced. The builder's own
  // submit and this page's submit do the SAME thing — run the search —
  // so the builder's button is not a second, weaker action sitting
  // beside the real one. It passes its DSL through explicitly rather
  // than via this state, because state written in the same tick is not
  // readable by the navigation that follows it.
  let builderDsl = $state('');

  /** Build the /search address. This is the whole "one representation"
   *  contract in one function: a `dsl=` (or `q=`) plus repeated
   *  `filter=` tokens, which is exactly what /search already parses. */
  function targetURL(dsl: string): string {
    const params = new URLSearchParams();
    const d = dsl.trim();
    // `dsl` and `q` are mutually exclusive on /search — the DSL carries
    // its own free-text term, so when the builder produced something the
    // typed text is already inside it.
    if (d) params.set('dsl', d);
    else if (initialQuery.trim()) params.set('q', initialQuery.trim());
    for (const [code, values] of Object.entries(chosen)) {
      for (const v of values) params.append('filter', `field:${code}=${v}`);
    }
    return `/search?${params.toString()}`;
  }

  const canSubmit = $derived(
    builderDsl.trim() !== '' || initialQuery.trim() !== '' || activeCount > 0,
  );

  async function run(dsl: string) {
    if (!dsl.trim() && !initialQuery.trim() && activeCount === 0) return;
    await goto(targetURL(dsl));
  }

  function reset() {
    chosen = {};
    builderDsl = '';
  }

  $effect(() => {
    let cancelled = false;
    (async () => {
      fieldsLoading = true;
      try {
        const { data, error } = await api.GET('/fields', {
          params: { query: { status: 'active', subject_kind: 'asset' } as never },
        });
        if (cancelled) return;
        if (error || !data) {
          fieldsError = t('search.advanced_page.fields_error');
          fields = [];
          return;
        }
        fields = (Array.isArray(data) ? data : []) as FieldDef[];
      } catch {
        if (!cancelled) fieldsError = t('search.advanced_page.fields_error');
      } finally {
        if (!cancelled) fieldsLoading = false;
      }
    })();
    return () => {
      cancelled = true;
    };
  });
</script>

<svelte:head>
  <title>{t('search.advanced_page.title')}</title>
</svelte:head>

<!-- Full viewport width, no max-w: this is a working surface, not an
     article. It collapses to one column below md. -->
<div class="w-full px-4 py-6 sm:px-6" data-testid="advanced-search-page">
  <header class="mb-6">
    <h1 class="text-2xl font-semibold text-fg">{t('search.advanced_page.title')}</h1>
    <p class="mt-1 max-w-3xl text-sm text-fg-muted">{t('search.advanced_page.intro')}</p>
  </header>

  <div class="grid gap-6 lg:grid-cols-2">
    <!-- The conditional search. Unchanged component, unchanged rules. -->
    <section
      class="rounded-lg border border-border bg-surface p-4"
      data-testid="advanced-conditions"
    >
      <h2 class="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-muted">
        {t('search.advanced_page.conditions_heading')}
      </h2>
      <AdvancedQueryBuilder
        initialFreeText={initialQuery}
        showImageSearch={false}
        onsubmit={(dsl) => {
          builderDsl = dsl;
          void run(dsl);
        }}
      />
      {#if builderDsl}
        <p class="mt-3 rounded bg-surface-elevated px-3 py-2 font-mono text-xs text-fg break-all">
          {builderDsl}
        </p>
      {/if}
    </section>

    <!-- The metadata field filters. -->
    <section
      class="rounded-lg border border-border bg-surface p-4"
      data-testid="advanced-field-filters"
    >
      <h2 class="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-muted">
        {t('search.advanced_page.fields_heading')}
      </h2>

      {#if fieldsLoading}
        <p class="text-sm text-fg-muted">{t('search.advanced_page.fields_loading')}</p>
      {:else if fieldsError}
        <p class="text-sm text-danger">{fieldsError}</p>
      {:else if filterable.length === 0}
        <p class="text-sm text-fg-muted">{t('search.advanced_page.fields_empty')}</p>
      {:else}
        <div class="flex flex-col gap-4">
          {#each filterable as f (f.id)}
            {@const vocab = vocabFor(f)}
            {@const opts = selectableOptions(vocab, chosen[f.code] ?? [])}
            {#if opts.length > 0}
              <div data-testid="field-filter-{f.code}">
                <div id="field-filter-label-{f.code}" class="mb-1.5 text-sm font-medium text-fg">
                  {f.label}
                </div>
                {#if usesTypeahead(f, opts)}
                  <!-- Same picker the upload row and the collection field
                       editor use, with its create arm off: this field's
                       vocabulary is fixed here, and search must never be
                       able to mint a term. What it contributes is what a
                       long chip wall cannot — filter as you type, arrow
                       keys, and tokens that come back off. -->
                  <VocabularyCombobox
                    options={vocab}
                    value={chosen[f.code] ?? []}
                    open={false}
                    labelledBy="field-filter-label-{f.code}"
                    placeholder={t('search.advanced_page.field_filter_placeholder')}
                    testid={f.code}
                    onchange={(values) => setValues(f.code, values)}
                  />
                {:else}
                  <div class="flex flex-wrap gap-1.5">
                    {#each opts as o (o.value)}
                      {@const on = (chosen[f.code] ?? []).includes(o.value)}
                      <button
                        type="button"
                        onclick={() => toggleValue(f.code, o.value)}
                        aria-pressed={on}
                        data-testid="field-option-{f.code}-{o.value}"
                        class="rounded-full border px-2.5 py-1 text-xs transition-colors
                               {on
                          ? 'border-accent bg-accent text-on-accent'
                          : 'border-border bg-surface text-fg-muted hover:bg-state-hover hover:text-fg'}"
                      >
                        {o.label}
                      </button>
                    {/each}
                  </div>
                {/if}
              </div>
            {/if}
          {/each}
        </div>
      {/if}
    </section>
  </div>

  <!-- Search by image. #1157 — this arm renders ONLY when the instance
       has the CLIP/similarity channel.

       ReverseImageDropzone is the component that already knows: it
       posts to /search/by-image and a 501 means `search.visual.enabled`
       is off, which it surfaces as `notConfigured`. That is where the
       frontend learns the flag today — there is no declarative field on
       any bootstrap payload — so this section is bound to the same
       signal rather than to a second, invented one.

       `oncapability` lets the dropzone tell its host what it learned, so
       the SECTION disappears instead of rendering a heading above a
       "not configured" message. -->
  {#if clipEnabled}
    <section
      class="mt-6 rounded-lg border border-border bg-surface p-4"
      data-testid="advanced-by-image"
    >
      <h2 class="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-muted">
        {t('search.advanced_page.by_image_heading')}
      </h2>
      <ReverseImageDropzone oncapability={(ok) => (clipDenied = !ok)} />
    </section>
  {/if}

  <!-- The submit bar. Sticky at the bottom so a long field list never
       pushes the action off screen — the form is the page, so its
       primary action stays reachable at 390px. -->
  <div
    class="sticky bottom-0 mt-6 flex flex-wrap items-center gap-3 border-t border-border
           bg-surface/95 py-3 backdrop-blur"
  >
    <button
      type="button"
      onclick={() => run(builderDsl)}
      disabled={!canSubmit}
      data-testid="advanced-submit"
      class="rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent
             hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-50"
    >
      {t('search.advanced_page.submit')}
    </button>
    <button
      type="button"
      onclick={reset}
      data-testid="advanced-reset"
      class="rounded-md border border-border px-3 py-2 text-sm text-fg-muted hover:bg-state-hover hover:text-fg"
    >
      {t('search.advanced_page.reset')}
    </button>
    {#if activeCount > 0}
      <span class="text-xs text-fg-muted" data-testid="advanced-active-count">
        {t('search.advanced_page.active_filters', { count: String(activeCount) })}
      </span>
    {/if}
  </div>
</div>

<!-- RESIDUAL, recorded where the next author will see it (#1157):
     `text`/`longtext` fields want a contains box and `date`/`datetime`
     a range, and neither ships here. Both need MORE than a UI row — the
     `field:` dimension's value grammar is an EQUALITY match against
     value_text / value_options, so a contains needs an operator in the
     grammar (`field:code~substring`) and a range needs two bounds
     against `value_date`. That is a change to the shared wire grammar
     and to dimensionSQL, not a widget, and doing it badly is exactly
     the second query language #1067 forbids. -->
