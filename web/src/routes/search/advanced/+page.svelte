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
    /**
     * Which resource types this field applies to; EMPTY means all
     * (#1173).
     *
     * The API has always sent this — it is `required` on the
     * FieldDefinition schema — and this type used to omit it, so the
     * page received the scoping and threw it away, then rendered every
     * field in one flat list because it had nothing left to section by.
     * The data was never the missing piece.
     */
    applies_to?: number[];
  };

  type AssetTypeDef = { ref: number; name?: string | null };

  let fields = $state<FieldDef[]>([]);
  let fieldsLoading = $state(true);
  let fieldsError = $state('');
  let assetTypes = $state<AssetTypeDef[]>([]);

  // THE FIRST TRANCHE (#1157): the controlled vocabularies. `select`
  // and `tree` store one slug in `value_text`; `multi_select` stores an
  // array in `value_options`. The backend dimension matches BOTH
  // columns in one expression, so all three render the same picker here
  // and the client never has to know which column a field uses.
  const VOCAB_TYPES = ['select', 'multi_select', 'tree'];

  // THE SECOND TRANCHE (#1165). The note that used to sit at the bottom
  // of this file said `text` and `date` could not be rendered because
  // the `field:` value grammar was equality-only, and that a contains or
  // a range "is a change to the shared wire grammar and to dimensionSQL,
  // not a widget". That change has landed: the grammar carries four
  // operators now (facet.FieldOp), so these two families have something
  // truthful to compile to.
  //
  // Each family maps to ONE operator, and the mapping is the whole
  // reason the widgets differ:
  //
  //   - text  → `~`, one box, case-insensitive substring.
  //   - date  → `>=` and `<=`, two boxes, either or both. They AND
  //     together server-side, which is what makes two inputs a RANGE
  //     rather than two alternatives.
  //
  // `rich_text` is included with text because it stores into value_text
  // like the others. `number` is deliberately absent: the grammar's
  // bounds read `value_date`, so a numeric range needs its own operator
  // pair and its own column, and shipping a box that silently matched
  // nothing would be worse than shipping no box.
  const TEXT_TYPES = ['text', 'longtext', 'rich_text'];
  const DATE_TYPES = ['date', 'datetime'];

  /** Which operator family a field's row renders. */
  function familyOf(f: FieldDef): 'vocab' | 'text' | 'date' | 'none' {
    if (VOCAB_TYPES.includes(f.type)) return 'vocab';
    if (TEXT_TYPES.includes(f.type)) return 'text';
    if (DATE_TYPES.includes(f.type)) return 'date';
    return 'none';
  }

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
   * The TYPE check is still here and is not the inference that was
   * removed. It says what this page can DRAW, and #1165 has just widened
   * it: vocabulary, text and date fields all have a control now, and
   * `number` still does not. That remains this page's own limit rather
   * than a statement about the field.
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
      .filter((f) => familyOf(f) !== 'none')
      .filter((f) => !f.read_capability || auth.can(f.read_capability))
      .sort((a, b) => (a.display_order ?? 0) - (b.display_order ?? 0) || a.label.localeCompare(b.label)),
  );

  /**
   * The resource types the caller has scoped the search to (#1173).
   *
   * This is NOT a display-only control. Selecting a type is itself a
   * filter — `filter=type:<ref>`, the `asset_type` dimension the facet
   * rail has used since #907 — so the sections and the query are two
   * readings of one piece of state rather than two pieces that have to
   * be kept in step. That is the same argument #1157 makes for building
   * field rows out of `filter=field:…`: the page composes nothing of its
   * own.
   */
  let selectedTypes = $state<number[]>([]);

  function toggleType(ref: number) {
    selectedTypes = selectedTypes.includes(ref)
      ? selectedTypes.filter((r) => r !== ref)
      : [...selectedTypes, ref];
  }

  /** A field with no `applies_to` entries applies to every type. */
  function isGlobal(f: FieldDef): boolean {
    return !f.applies_to || f.applies_to.length === 0;
  }

  /**
   * The sections the form renders, in order (#1173).
   *
   * Global fields first and ALWAYS, then one section per selected type
   * carrying the fields scoped to it. A type-specific field for a type
   * nobody selected appears nowhere — which is the point of the feature,
   * and the reason `applies_to` had to stop being dropped by FieldDef.
   *
   * `show_in_advanced_search = false` never reaches here: `filterable`
   * has already dropped it, so a hidden field cannot re-enter through a
   * type section.
   */
  const sections = $derived.by(() => {
    const out: { key: string; label: string; fields: FieldDef[] }[] = [
      {
        key: 'global',
        label: t('search.advanced_page.section_global'),
        fields: filterable.filter(isGlobal),
      },
    ];
    for (const ref of selectedTypes) {
      const scoped = filterable.filter((f) => !isGlobal(f) && f.applies_to!.includes(ref));
      if (scoped.length === 0) continue;
      out.push({
        key: `type-${ref}`,
        label: assetTypes.find((a) => a.ref === ref)?.name || String(ref),
        fields: scoped,
      });
    }
    return out.filter((s) => s.fields.length > 0);
  });

  /** Every field code currently on screen — the set a filter may be
   *  held for. See the effect below that prunes the rest. */
  const visibleCodes = $derived(new Set(sections.flatMap((s) => s.fields.map((f) => f.code))));

  /** code -> chosen values. Multiple values in one field OR together,
   *  which is the facet grammar's own rule for non-tag dimensions. */
  let chosen = $state<Record<string, string[]>>({});

  /** code -> substring, compiled to the `~` operator (#1165). */
  let contains = $state<Record<string, string>>({});

  /** code -> inclusive date bounds, compiled to `>=` and `<=` (#1165).
   *  Either end may be blank: one bound is a valid open-ended range. */
  let ranges = $state<Record<string, { from: string; to: string }>>({});

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

  function setContains(code: string, text: string) {
    contains = { ...contains, [code]: text };
  }

  function setRange(code: string, end: 'from' | 'to', value: string) {
    const cur = ranges[code] ?? { from: '', to: '' };
    ranges = { ...ranges, [code]: { ...cur, [end]: value } };
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

  /**
   * Every field constraint currently set, as `filter=` wire tokens.
   *
   * THIS IS THE ONLY PLACE THIS PAGE SPEAKS THE GRAMMAR. Both the
   * address the Search button navigates to and the count preview below
   * are built from it, so the number shown and the page reached cannot
   * be describing different queries — the #907 invariant ("a bucket says
   * 42 rows carry this value, and ticking it must return those 42
   * rows"), applied to a count that is rendered before the click rather
   * than after it.
   *
   * Only VISIBLE fields contribute. A constraint on a field whose type
   * section has been closed is not silently carried into the query — see
   * the pruning effect below, which is what makes that true rather than
   * merely intended.
   */
  function fieldTerms(): string[] {
    const out: string[] = [];
    for (const [code, values] of Object.entries(chosen)) {
      if (!visibleCodes.has(code)) continue;
      for (const v of values) out.push(`field:${code}=${v}`);
    }
    for (const [code, text] of Object.entries(contains)) {
      if (!visibleCodes.has(code)) continue;
      const s = text.trim();
      if (s) out.push(`field:${code}~${s}`);
    }
    for (const [code, r] of Object.entries(ranges)) {
      if (!visibleCodes.has(code)) continue;
      // The two ends are INDEPENDENT terms and AND together server-side
      // (facet.subGroupKey). Either alone is a valid open-ended range,
      // so a caller who fills one box and not the other gets the
      // half-bounded query they asked for rather than nothing.
      if (r.from?.trim()) out.push(`field:${code}>=${r.from.trim()}`);
      if (r.to?.trim()) out.push(`field:${code}<=${r.to.trim()}`);
    }
    return out;
  }

  /** `type:` terms — the resource-type scope, in the same grammar. */
  function typeTerms(): string[] {
    return selectedTypes.map((ref) => `type:${ref}`);
  }

  const activeCount = $derived(fieldTerms().length + typeTerms().length);

  /**
   * Drop constraints held for fields that are no longer on screen.
   *
   * Deselecting a type must not leave that type's filters silently
   * applied: an invisible predicate in the URL is the "looks narrowed
   * and is not" failure the whole `filter=` design exists to avoid, only
   * in the other direction — narrowed by something with no control
   * showing. `fieldTerms` already skips them, so this is about the
   * STORED state matching what is drawn: without it, re-selecting the
   * type would restore filters the caller had visibly cleared.
   */
  $effect(() => {
    const live = visibleCodes;
    const prune = <T,>(rec: Record<string, T>): Record<string, T> | null => {
      const kept = Object.fromEntries(Object.entries(rec).filter(([c]) => live.has(c)));
      return Object.keys(kept).length === Object.keys(rec).length ? null : kept;
    };
    const c = prune(chosen);
    if (c) chosen = c;
    const t2 = prune(contains);
    if (t2) contains = t2;
    const r = prune(ranges);
    if (r) ranges = r;
  });

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

  // The compiled query the builder currently holds.
  //
  // #1197 — "currently", not "last submitted". This used to update only
  // when the builder's own inner button ran, so a caller who typed a
  // condition and reached for the sticky Search below found it disabled:
  // `canSubmit` reads this value, and this value had not moved. The
  // builder now reports its compiled query on every change (its
  // `onchange` prop), so what the page holds is what the form says.
  //
  // The builder's submit ALSO passes its DSL through explicitly, and
  // that is not redundant: state written in the same tick is not
  // readable by the navigation that follows it.
  let builderDsl = $state('');

  /**
   * The query, as the parameters /search already parses.
   *
   * This is the whole "one representation" contract in one function: a
   * `dsl=` (or `q=`) plus repeated `filter=` tokens. It is shared by the
   * address the Search button navigates to and by the count preview, and
   * that sharing is not a tidiness argument — it is the ONLY reason the
   * previewed number is guaranteed to describe the page the button
   * reaches. Two functions building "the same" parameters would be two
   * things that can drift, and a count that drifts from its result set
   * is the #902/#1066 failure shape.
   *
   * `types=` is deliberately NOT set. /search defaults to every kind
   * when it is absent, and the results page reads its own kind chips
   * from this same URL — so leaving it out makes both ends default
   * identically. Setting it here would narrow the count against a
   * results page that had not been narrowed.
   */
  function searchParams(dsl: string): URLSearchParams {
    const params = new URLSearchParams();
    const d = dsl.trim();
    // `dsl` and `q` are mutually exclusive on /search — the DSL carries
    // its own free-text term, so when the builder produced something the
    // typed text is already inside it.
    if (d) params.set('dsl', d);
    else if (initialQuery.trim()) params.set('q', initialQuery.trim());
    for (const term of typeTerms()) params.append('filter', term);
    for (const term of fieldTerms()) params.append('filter', term);
    return params;
  }

  function targetURL(dsl: string): string {
    return `/search?${searchParams(dsl).toString()}`;
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
    contains = {};
    ranges = {};
    selectedTypes = [];
    builderDsl = '';
  }

  /**
   * # THE LIVE RESULT COUNT (#1173)
   *
   * ## It is the SAME QUERY, not a count of its own
   *
   * This runs `GET /api/v1/search` with the parameters the Search button
   * would navigate to and reads `total_count` off the response. It does
   * not call a count endpoint, and there is deliberately no new one.
   *
   * That is the whole safety argument, and it is worth stating plainly
   * because the tempting alternative is what goes wrong. A count is a
   * DERIVED COPY of a result set: if a post the caller may not read is
   * excluded from the results it must be excluded from the count too, or
   * the number becomes an oracle the list is not — a caller narrows a
   * filter until the count moves and has recovered a value that was
   * withheld from them, one bit at a time. That is #902 on the search
   * plane and #1066 on the similarity plane, and both were caused by a
   * second path computing "the same" thing under a slightly different
   * rule.
   *
   * The engine already holds the two together: `runAssetQuery` splices
   * ONE predicate string into both its hits statement and its count
   * statement (search/query.go — `matchFrag + visFrag + matureFrag +
   * selFrag`, identical in both), so `total_count` is narrowed by
   * exactly what narrows the array, for whoever is asking. Borrowing
   * that number inherits the property instead of re-deriving it. A
   * dedicated count endpoint would have been a second place for the read
   * rule to live, and read rules that live in two places are the thing
   * ADR 0070 records as certain to disagree eventually.
   *
   * `credentials: 'include'` so the count is computed for THIS viewer.
   * An anonymous preview and a signed-in one legitimately differ; what
   * must never differ is the count and the results for one viewer.
   *
   * ## Debounced, and superseded rather than raced
   *
   * `countGen` is checked after every await, so a slow response for an
   * older form state cannot overwrite a newer one — the same
   * generation-guard the results page uses. Without it a count from two
   * keystroke ago can land last and sit there describing nothing.
   */
  let resultCount = $state<number | null>(null);
  let countCapped = $state(false);
  let countLoading = $state(false);
  let countGen = 0;

  const COUNT_DEBOUNCE_MS = 350;

  $effect(() => {
    // Read every dependency SYNCHRONOUSLY, before the timer: Svelte
    // collects an effect's dependencies from the reads it makes in its
    // own frame, and anything read inside the setTimeout callback would
    // not register — the count would then go stale on exactly the edits
    // it is supposed to track.
    const dsl = builderDsl;
    const params = searchParams(dsl);
    const runnable = canSubmit;

    if (!runnable) {
      resultCount = null;
      countCapped = false;
      countLoading = false;
      return;
    }
    const gen = ++countGen;
    countLoading = true;
    // limit=1 because only the number is wanted. It does not change the
    // number: total_count counts matches, not returned rows.
    params.set('limit', '1');
    const timer = setTimeout(async () => {
      try {
        const res = await fetch(`/api/v1/search?${params.toString()}`, {
          credentials: 'include',
        });
        if (gen !== countGen) return;
        if (!res.ok) {
          resultCount = null;
          countCapped = false;
          return;
        }
        const data = (await res.json()) as { total_count?: number; total_count_capped?: boolean };
        if (gen !== countGen) return;
        resultCount = data.total_count ?? null;
        countCapped = data.total_count_capped ?? false;
      } catch {
        if (gen === countGen) resultCount = null;
      } finally {
        if (gen === countGen) countLoading = false;
      }
    }, COUNT_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  });

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

  // The resource types the type scope offers (#1173).
  //
  // `GET /asset_types` is the SAME endpoint the upload picker reads and
  // it is already visibility-governed at the server: anonymous callers
  // see every unrestricted type, authenticated ones additionally see
  // restricted types they hold a read ACL on. So this list needs no gate
  // of its own, and offering a scope the caller could not have searched
  // is not reachable from here.
  //
  // A failure is SILENT rather than an error banner: the type scope is
  // an optional narrowing, and a page that refuses to render its field
  // filters because a secondary list did not load would be broken by
  // something it does not need.
  $effect(() => {
    let cancelled = false;
    (async () => {
      try {
        const { data } = await api.GET('/asset_types', {});
        if (cancelled || !Array.isArray(data)) return;
        assetTypes = data as AssetTypeDef[];
      } catch {
        /* the scope is optional; the field filters stand without it */
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
        onchange={(dsl) => (builderDsl = dsl)}
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
        <!-- THE RESOURCE-TYPE SCOPE (#1173).

             Two jobs in one control, which is why it sits at the top of
             the filters rather than beside the kind chips on /search:
             it narrows the search (`filter=type:<ref>`) AND it decides
             which type-specific field sections exist below. Those are
             the same fact, so they are the same control. -->
        {#if assetTypes.length > 0}
          <div class="mb-5" data-testid="advanced-type-scope">
            <div class="mb-1.5 text-sm font-medium text-fg">
              {t('search.advanced_page.types_heading')}
            </div>
            <p class="mb-2 text-xs text-fg-muted">{t('search.advanced_page.types_hint')}</p>
            <div class="flex flex-wrap gap-1.5">
              {#each assetTypes as ty (ty.ref)}
                {@const on = selectedTypes.includes(ty.ref)}
                <button
                  type="button"
                  onclick={() => toggleType(ty.ref)}
                  aria-pressed={on}
                  data-testid="advanced-type-{ty.ref}"
                  class="min-h-11 rounded-full border px-3 py-1 text-xs transition-colors sm:min-h-0
                         {on
                    ? 'border-accent bg-accent text-on-accent'
                    : 'border-border bg-surface text-fg-muted hover:bg-state-hover hover:text-fg'}"
                >
                  {ty.name || ty.ref}
                </button>
              {/each}
            </div>
          </div>
        {/if}

        <div class="flex flex-col gap-6">
          {#each sections as sec (sec.key)}
            <div data-testid="advanced-section-{sec.key}">
              <!-- The global section keeps no heading when it is the only
                   one: a lone "General" label above every field is chrome
                   that names nothing the caller chose. Headings appear as
                   soon as there is a second section to tell it apart
                   from. -->
              {#if sections.length > 1}
                <div class="mb-2 border-b border-border pb-1 text-xs font-semibold uppercase tracking-wide text-fg-muted">
                  {sec.label}
                </div>
              {/if}
              <div class="flex flex-col gap-4">
                {#each sec.fields as f (f.id)}
                  {@const family = familyOf(f)}
                  {@const vocab = family === 'vocab' ? vocabFor(f) : []}
                  {@const opts =
                    family === 'vocab' ? selectableOptions(vocab, chosen[f.code] ?? []) : []}
                  <!-- A vocabulary field with no vocabulary has nothing to
                       offer and is skipped, exactly as before. A text or
                       date field has no vocabulary by nature, so the same
                       test must not hide it. -->
                  {#if family !== 'vocab' || opts.length > 0}
                    <div data-testid="field-filter-{f.code}">
                      <div id="field-filter-label-{f.code}" class="mb-1.5 text-sm font-medium text-fg">
                        {f.label}
                      </div>
                      {#if family === 'text'}
                        <!-- #1165 — compiles to `field:<code>~<text>`, a
                             case-insensitive substring. -->
                        <input
                          type="search"
                          value={contains[f.code] ?? ''}
                          oninput={(e) => setContains(f.code, e.currentTarget.value)}
                          aria-labelledby="field-filter-label-{f.code}"
                          placeholder={t('search.advanced_page.contains_placeholder')}
                          data-testid="field-contains-{f.code}"
                          class="min-h-11 w-full rounded-md border border-border-strong bg-surface px-3 py-2 text-sm"
                        />
                      {:else if family === 'date'}
                        <!-- #1165 — two INDEPENDENT bounds compiling to
                             `>=` and `<=`. They AND together server-side,
                             and either alone is a valid open-ended range,
                             so neither input requires the other. -->
                        <div class="flex flex-wrap items-center gap-2">
                          <label class="flex items-center gap-1.5 text-xs text-fg-muted">
                            {t('search.advanced_page.range_from')}
                            <input
                              type="date"
                              value={ranges[f.code]?.from ?? ''}
                              oninput={(e) => setRange(f.code, 'from', e.currentTarget.value)}
                              data-testid="field-from-{f.code}"
                              class="min-h-11 rounded-md border border-border-strong bg-surface px-2 py-1.5 text-sm"
                            />
                          </label>
                          <label class="flex items-center gap-1.5 text-xs text-fg-muted">
                            {t('search.advanced_page.range_to')}
                            <input
                              type="date"
                              value={ranges[f.code]?.to ?? ''}
                              oninput={(e) => setRange(f.code, 'to', e.currentTarget.value)}
                              data-testid="field-to-{f.code}"
                              class="min-h-11 rounded-md border border-border-strong bg-surface px-2 py-1.5 text-sm"
                            />
                          </label>
                        </div>
                      {:else if usesTypeahead(f, opts)}
                        <!-- Same picker the upload row and the collection
                             field editor use, with its create arm off:
                             this field's vocabulary is fixed here, and
                             search must never be able to mint a term.
                             What it contributes is what a long chip wall
                             cannot — filter as you type, arrow keys, and
                             tokens that come back off. -->
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
            </div>
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
    <!-- The live result count (#1173). It says how many results the
         button beside it will land on, and it is the SAME query — see
         the effect that fetches it for why that has to be true rather
         than merely intended.

         It renders only when there is a query to count, so an untouched
         form shows no number rather than a misleading zero. -->
    {#if canSubmit}
      <span class="text-xs text-fg-muted" data-testid="advanced-result-count">
        {#if countLoading && resultCount === null}
          {t('search.advanced_page.count_loading')}
        {:else if resultCount !== null}
          <span class:opacity-50={countLoading}>
            {countCapped
              ? t('search.advanced_page.count_capped', { count: String(resultCount) })
              : t('search.advanced_page.count', { count: String(resultCount) })}
          </span>
        {/if}
      </span>
    {/if}
  </div>
</div>

<!-- #1157's residual is CLOSED (#1165). `text`/`longtext` fields have a
     contains box and `date`/`datetime` a range, and they got there the
     way that residual said they had to: by an operator in the shared
     grammar (facet.FieldOp — `~`, `>=`, `<=`) and a case in
     dimensionSQL, not by a widget compiling to something of its own.

     RESIDUAL, recorded where the next author will see it:

     - `number` fields (`rating`, `polycount`, `pixel_width`) still have
       no control. The bounds read `value_date`; a numeric range needs
       its own operator pair against `value_num`. The grammar has room
       for it — one more FieldOp, one more case — and the shape to copy
       is right there.
     - `boolean` and `reference` likewise: both are equality at heart,
       but a checkbox that means "unset" and "false" differently, and a
       picker that resolves a UUID to a name, are each a design question
       rather than a missing operator.
     - The count is capped at search.TotalCountCap (10,000). Past that
       it reports the cap rather than the true total, which is the
       engine's existing policy and is why the string differs. -->
