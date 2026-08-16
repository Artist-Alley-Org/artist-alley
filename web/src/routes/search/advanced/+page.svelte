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

  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { t } from '$stores/lang.svelte';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import AdvancedQueryBuilder from '$components/search/AdvancedQueryBuilder.svelte';
  import ReverseImageDropzone from '$components/search/ReverseImageDropzone.svelte';
  import { selectableOptions, normalizeOptions } from '$lib/fieldOptions';

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

  /** Fields this caller may both read and filter on. */
  const filterable = $derived(
    fields
      .filter((f) => f.searchable !== false && f.status !== 'archived')
      .filter((f) => VOCAB_TYPES.includes(f.type))
      // The display half of the read gate. A field whose read_capability
      // this caller lacks is not offered; the server refuses it anyway.
      .filter((f) => !f.read_capability || auth.can(f.read_capability))
      .sort((a, b) => (a.display_order ?? 0) - (b.display_order ?? 0) || a.label.localeCompare(b.label)),
  );

  /** code -> chosen values. Multiple values in one field OR together,
   *  which is the facet grammar's own rule for non-tag dimensions. */
  let chosen = $state<Record<string, string[]>>({});

  function toggleValue(code: string, value: string) {
    const cur = chosen[code] ?? [];
    chosen = {
      ...chosen,
      [code]: cur.includes(value) ? cur.filter((v) => v !== value) : [...cur, value],
    };
  }

  function optionsFor(f: FieldDef): { value: string; label: string }[] {
    try {
      return selectableOptions(normalizeOptions(f.options)).map((o) => ({
        value: o.value,
        label: o.label ?? o.value,
      }));
    } catch {
      return [];
    }
  }

  const activeCount = $derived(
    Object.values(chosen).reduce((n, vs) => n + vs.length, 0),
  );

  // Whether the instance has the CLIP/similarity channel.
  //
  // `undefined` = not yet known, and the arm RENDERS while unknown:
  // showing it and collapsing on the first 501 is the behaviour the
  // existing dropzone already has, and the alternative (hide until
  // proven) would hide the feature on every install that has it.
  //
  // ⚠️ RESIDUAL, and it is an acceptance gap worth naming rather than
  // burying: this is attempt-based, so on a disabled instance the arm is
  // visible until someone drops an image. A DECLARATIVE flag is the
  // right shape — `search.visual.enabled` surfaced on /appearance, the
  // public boot payload appearance.svelte.ts already fetches — and it
  // is not here because it needs an openapi.yaml schema change plus a
  // Go+TS regeneration, which is a change to a shared generated surface
  // rather than a line in this file.
  let clipEnabled = $state<boolean | undefined>(undefined);

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
            {@const opts = optionsFor(f)}
            {#if opts.length > 0}
              <div data-testid="field-filter-{f.code}">
                <div class="mb-1.5 text-sm font-medium text-fg">{f.label}</div>
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
  {#if clipEnabled !== false}
    <section
      class="mt-6 rounded-lg border border-border bg-surface p-4"
      data-testid="advanced-by-image"
    >
      <h2 class="mb-3 text-sm font-semibold uppercase tracking-wide text-fg-muted">
        {t('search.advanced_page.by_image_heading')}
      </h2>
      <ReverseImageDropzone oncapability={(ok) => (clipEnabled = ok)} />
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
