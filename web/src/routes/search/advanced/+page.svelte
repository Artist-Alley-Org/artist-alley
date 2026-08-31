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
  import ContributorFilter from '$components/search/ContributorFilter.svelte';
  import ExtensionFilter from '$components/search/ExtensionFilter.svelte';
  import FileSizeFilter from '$components/search/FileSizeFilter.svelte';
  import { selectableOptions, normalizeOptions, type FieldOption } from '$lib/fieldOptions';
  import { fileSizeTerm, DEFAULT_FILE_SIZE_UNIT, type FileSizeUnit } from '$lib/fileSizeBound';

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
  // like the others.
  //
  // THE THIRD TRANCHE (#1173, sprint 18d). `number` used to be absent
  // from this list, and the note here explaining why said "the grammar's
  // bounds read `value_date`, so a numeric range needs its own operator
  // pair and its own column". That was true when it was written and
  // stopped being true in 18b: ADR 0093's 2026-08-29 amendment separates
  // a bound's OPERATOR from its DOMAIN, so `>=` and `<=` now read
  // `value_num` for a field whose definition declares `number`, with the
  // compatibility check answered per term against `field_definition.type`
  // in `Selection.Authorize`. No new operator, no new widget family: a
  // numeric field renders the SAME two-bound row a date does, through the
  // same `ranges` state and the same `fieldTerms()`.
  //
  // ⭐ This is what gives `pixel_width` and `pixel_height` a control. They
  // are ordinary `number` field definitions (migration 00017) and needed
  // nothing of their own beyond being drawable.
  const TEXT_TYPES = ['text', 'longtext', 'rich_text'];
  const DATE_TYPES = ['date', 'datetime'];
  const NUMBER_TYPES = ['number'];

  /** Which operator family a field's row renders. */
  function familyOf(f: FieldDef): 'vocab' | 'text' | 'date' | 'number' | 'none' {
    if (VOCAB_TYPES.includes(f.type)) return 'vocab';
    if (TEXT_TYPES.includes(f.type)) return 'text';
    if (DATE_TYPES.includes(f.type)) return 'date';
    if (NUMBER_TYPES.includes(f.type)) return 'number';
    return 'none';
  }

  /**
   * The two shipped PIXEL-DIMENSION fields (#1173, sprint 18d).
   *
   * Resolved by CODE, which is the product's existing answer for these
   * two rather than an invention here: `db.ShippedFieldCodes` registers
   * them as shipped definitions, and `pixeldims.SelectColumnsSQL` and the
   * IIIF handler both resolve them by code already. They are grouped with
   * the file's own properties because that is what a pixel dimension IS —
   * a fact about the file, beside its type and its size — rather than
   * something an operator wrote about the work.
   *
   * ⛔ THE GROUPING NEVER OVERRIDES THE FIELD'S OWN CONFIGURATION. It
   * reads `applies_to` and moves nothing: a pixel field an operator has
   * SCOPED to a resource type is not claimed here and follows the
   * per-type sections like any other scoped field. `status`,
   * `show_in_advanced_search` and `read_capability` are already applied
   * by `filterable` upstream, so a hidden or unreadable pixel field
   * cannot re-enter through this group either.
   */
  const PIXEL_CODES = ['pixel_width', 'pixel_height'];

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
   * removed. It says what this page can DRAW, and 18d has widened it
   * again: vocabulary, text, date AND number fields all have a control
   * now. `boolean` and `reference` still do not, and that remains this
   * page's own limit rather than a statement about the field.
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
   * The pixel fields the "About the file" group claims — the shipped
   * dimensions, and only while they are GLOBAL (#1173, sprint 18d).
   *
   * A pixel field an operator has scoped with `applies_to` is not
   * claimed, so it appears in its type's section and nowhere else. Every
   * field appears EXACTLY ONCE: `global` excludes whatever this claims.
   */
  const mediaFields = $derived(
    filterable.filter((f) => PIXEL_CODES.includes(f.code) && isGlobal(f)),
  );
  const mediaCodes = $derived(new Set(mediaFields.map((f) => f.code)));

  /**
   * The sections the form renders, in order (#1173).
   *
   * "About the file" first and ALWAYS — it carries the built-in
   * dimensions (file type, file size, pixel dimensions) that exist on
   * every install regardless of what an operator has configured, which
   * is why it is the one section with no field-count condition on it.
   * Then the remaining global fields, then one section per selected type
   * carrying the fields scoped to it. A type-specific field for a type
   * nobody selected appears nowhere — which is the point of the feature,
   * and the reason `applies_to` had to stop being dropped by FieldDef.
   *
   * `show_in_advanced_search = false` never reaches here: `filterable`
   * has already dropped it, so a hidden field cannot re-enter through a
   * type section or through the media group.
   */
  const sections = $derived.by(() => {
    const out: { key: string; label: string; fields: FieldDef[]; always?: boolean }[] = [
      {
        key: 'media',
        label: t('search.advanced_page.section_media'),
        fields: mediaFields,
        always: true,
      },
      {
        key: 'global',
        label: t('search.advanced_page.section_global'),
        fields: filterable.filter((f) => isGlobal(f) && !mediaCodes.has(f.code)),
      },
    ];
    for (const ref of selectedTypes) {
      const scoped = filterable.filter((f) => !isGlobal(f) && f.applies_to!.includes(ref));
      out.push({
        key: `type-${ref}`,
        label: assetTypes.find((a) => a.ref === ref)?.name || String(ref),
        fields: scoped,
        // A selected type ALWAYS gets a section, even with no fields
        // scoped to it, because its concrete workflow states live there
        // — see the workflow block below. Before 18d a fieldless section
        // had nothing to hold and was dropped.
        always: true,
      });
    }
    return out.filter((s) => s.always || s.fields.length > 0);
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

  // ───────────────────────────────────────────────────────────────────
  // THE BUILT-IN DIMENSIONS (#1173, sprint 18d)
  //
  // Everything below composes through the SAME `filter=<dimension>:<value>`
  // grammar the field rows use. None of it is a parameter of this page's
  // own, which is what keeps the count, the submitted address and a saved
  // search describing one query — see `searchParams()`.
  // ───────────────────────────────────────────────────────────────────

  /**
   * The contributors ticked, as `{ref, label}`.
   *
   * ⛔ The REF is the identity and the label is only for drawing. Never
   * the username: `user_username_uniq_idx` is unique CASE-SENSITIVELY
   * while the `owner:` predicate matches `LOWER(fu.username)`, so two
   * users can satisfy one term; the column is nullable; and a numeric
   * username collides with the same predicate's `owner_user_ref::TEXT`
   * arm.
   */
  let selectedOwners = $state<{ ref: number; label: string }[]>([]);

  /** File extensions ticked or typed, already normalized. */
  let selectedExtensions = $state<string[]>([]);

  /** The file-size bounds, as the person typed them plus their unit. */
  let fileSize = $state<{ min: string; max: string; unit: FileSizeUnit }>({
    min: '',
    max: '',
    unit: DEFAULT_FILE_SIZE_UNIT,
  });

  /**
   * "Files with no workflow state" — `filter=workflow_state:none`.
   *
   * ⛔ ONE CHECKBOX, GLOBAL, OUTSIDE EVERY TYPE SECTION, and that is a
   * correctness claim rather than a layout preference. `none` means
   * `state_id IS NULL`, which is DOMAIN-INDEPENDENT: with Image and
   * Video both selected, a copy of this checkbox drawn under Image would
   * still return the state-less videos. A control that claims a scope
   * the wire does not have is a control that lies, so there is exactly
   * one of it and it sits where nothing implies a scope.
   *
   * It is rendered even with ZERO types selected, because it is
   * answerable then too.
   */
  let workflowNone = $state(false);

  /**
   * The CONCRETE states ticked, keyed by full `<domain>/<code>` identity.
   *
   * ⛔ NEVER by code alone. `workflow_states` is `UNIQUE (domain, code)`,
   * so two resource types can both define `published` and they are two
   * different states. Collapsing them would tick one and filter by the
   * other.
   */
  let selectedStates = $state<string[]>([]);

  type WorkflowStateDef = { id: string; domain: string; code: string; label: string };

  /** asset-type ref → the states of that type's domain. */
  let statesByType = $state<Record<number, WorkflowStateDef[]>>({});
  /** asset-type refs whose state vocabulary could not be loaded. ⛔ An
   *  error is not an empty vocabulary: `/workflow/states` refuses an
   *  anonymous caller, and rendering that as "this type has no states"
   *  would state an absence with no evidence. */
  let statesFailed = $state<number[]>([]);

  /** The workflow domain of one asset type — `workflow.AssetDomain`'s
   *  spelling, which is what the filter value carries. */
  function assetDomain(ref: number): string {
    return `asset:${ref}`;
  }

  /** The domains currently on screen. A concrete state may only be
   *  filtered on while its own type section is showing. */
  const liveDomains = $derived(new Set(selectedTypes.map(assetDomain)));

  const liveStates = $derived(
    selectedStates.filter((id) => liveDomains.has(id.slice(0, id.indexOf('/')))),
  );

  function toggleState(identity: string) {
    selectedStates = selectedStates.includes(identity)
      ? selectedStates.filter((s) => s !== identity)
      : [...selectedStates, identity];
  }

  /**
   * Every built-in constraint, as `filter=` wire tokens.
   *
   * Same rule as `fieldTerms()`: only what is ON SCREEN contributes. The
   * three always-visible controls always do; a concrete workflow state
   * contributes only while its type section is open, and the pruning
   * effect below keeps the STORED state matching that.
   */
  function builtinTerms(): string[] {
    const out: string[] = [];
    for (const o of selectedOwners) out.push(`owner:${o.ref}`);
    for (const e of selectedExtensions) out.push(`extension:${e}`);
    // The two bounds are INDEPENDENT terms carrying different operators,
    // so they land in different sub-groups and AND together server-side
    // (ADR 0093's 18b amendment §5). Either alone is a valid open-ended
    // range. An unparseable or out-of-range value emits NOTHING rather
    // than a clamped bound — see FileSizeFilter.
    const lower = fileSizeTerm(fileSize.min, fileSize.unit, 'lower');
    if (lower) out.push(lower);
    const upper = fileSizeTerm(fileSize.max, fileSize.unit, 'upper');
    if (upper) out.push(upper);
    if (workflowNone) out.push('workflow_state:none');
    for (const s of liveStates) out.push(`workflow_state:${s}`);
    return out;
  }

  const activeCount = $derived(
    fieldTerms().length + typeTerms().length + builtinTerms().length,
  );

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

  /**
   * The same rule for CONCRETE workflow states (#1173, sprint 18d).
   *
   * Deselecting a type takes its section away, so the states inside it
   * stop being reachable and must stop being applied. `builtinTerms`
   * already skips them; this is about the STORED state matching what is
   * drawn, so re-selecting the type does not restore ticks the caller
   * had visibly cleared.
   *
   * ⛔ `workflowNone` is deliberately untouched. It is global, it is
   * never inside a type section, and pruning it here would delete a
   * filter whose control is still on screen.
   */
  $effect(() => {
    const live = liveDomains;
    const kept = selectedStates.filter((id) => live.has(id.slice(0, id.indexOf('/'))));
    if (kept.length !== selectedStates.length) selectedStates = kept;
  });

  /**
   * The state vocabulary of each selected type.
   *
   * `/workflow/states` REQUIRES `domain` — it answers 400 without one —
   * so this is one request per selected type rather than one request for
   * "all states". That is also the honest shape: there is no such thing
   * as a state outside a domain.
   */
  $effect(() => {
    const refs = selectedTypes;
    let cancelled = false;
    (async () => {
      for (const ref of refs) {
        if (cancelled) return;
        if (statesByType[ref]) continue;
        try {
          const { data, error } = await api.GET('/workflow/states', {
            params: { query: { domain: assetDomain(ref) } as never },
          });
          if (cancelled) return;
          if (error || !Array.isArray(data)) {
            if (!statesFailed.includes(ref)) statesFailed = [...statesFailed, ref];
            continue;
          }
          statesByType = { ...statesByType, [ref]: data as WorkflowStateDef[] };
          statesFailed = statesFailed.filter((r) => r !== ref);
        } catch {
          if (!cancelled && !statesFailed.includes(ref)) {
            statesFailed = [...statesFailed, ref];
          }
        }
      }
    })();
    return () => {
      cancelled = true;
    };
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
    // The built-in dimensions (#1173, sprint 18d) go through the SAME
    // serializer as everything else, which is the whole reason the two
    // suggestion lookups below can be handed this exact string: the list
    // of contributors and the list of file types describe the query
    // being built, not the corpus.
    for (const term of builtinTerms()) params.append('filter', term);
    return params;
  }

  /**
   * The query the two suggestion lookups run against, serialized once.
   *
   * ⛔ IT CARRIES THIS DIMENSION'S OWN TERMS TOO, and that is deliberate.
   * `Selection.ForFacet` drops them server-side — "so an OR dimension
   * does not filter itself out of existence" — which is what keeps
   * contributor B selectable after contributor A has been ticked, and
   * what lets a selected extension legitimately leave its own bucket
   * list. Stripping them here would make the client responsible for a
   * rule the server already owns, and the guarantee would stop being
   * exercised at all.
   */
  const lookupQuery = $derived(searchParams(builderDsl).toString());

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
    // The built-in dimensions clear too (#1173, sprint 18d). "Start
    // over" that left an owner or a size bound behind would be the
    // invisible-predicate failure with a button attached to it.
    selectedOwners = [];
    selectedExtensions = [];
    // ⛔ The unit RESTORES to the default rather than being left where
    // the caller last put it: an empty box beside a remembered "GB" is a
    // form that is not actually blank.
    fileSize = { min: '', max: '', unit: DEFAULT_FILE_SIZE_UNIT };
    workflowNone = false;
    selectedStates = [];
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
        <!-- ⛔ A LOAD FAILURE RENDERS NOTHING ELSE, deliberately. The
             route is reachable anonymously and `/fields`,
             `/search/facets`, `/search/contributors` and
             `/workflow/states` all answer 401 to an anonymous caller —
             so drawing the controls here with empty lists beside them
             would state four authoritative absences the page has no
             evidence for. The existing field-load failure treatment is
             the honest one; there is deliberately no auth gate added to
             the page itself. -->
        <p class="text-sm text-danger">{fieldsError}</p>
      {:else}
        <!-- What these filters are about, said once at the top (#1173,
             sprint 18d). Every dimension in this panel narrows to FILES:
             a post is a set of members and a collection is a container,
             so neither has an extension, a size, a workflow state or a
             pixel dimension, and an asset-only filter takes them off the
             page rather than passing them through. That is the
             positive-narrowing direction ADR 0093 settles, and a person
             should not have to discover it from a result set. -->
        <p class="mb-4 text-xs text-fg-muted" data-testid="advanced-filters-scope">
          {t('search.advanced_page.filters_scope')}
        </p>

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

        <!-- ABOUT THE WORK (#1173, sprint 18d) — the two dimensions that
             are properties of the work rather than of the file it is
             stored in. -->
        <div class="mb-6 flex flex-col gap-4" data-testid="advanced-group-work">
          <div class="mb-1 border-b border-border pb-1 text-xs font-semibold uppercase tracking-wide text-fg-muted">
            {t('search.advanced_page.section_work')}
          </div>

          <ContributorFilter
            query={lookupQuery}
            selected={selectedOwners}
            onchange={(v) => (selectedOwners = v)}
          />

          <!-- ⛔ THE GLOBAL WORKFLOW-STATE CHECKBOX. One of it, here,
               outside every type section, rendered whether or not any
               type is selected. `workflow_state:none` is
               `state_id IS NULL` and carries no domain, so a per-type
               copy would claim a scope the wire does not have: with
               Image and Video both selected, ticking it under Image
               still returns the state-less videos. -->
          <label
            class="flex min-h-11 items-center gap-2 text-sm text-fg sm:min-h-0"
            data-testid="advanced-workflow-none-label"
          >
            <input
              type="checkbox"
              checked={workflowNone}
              onchange={(e) => (workflowNone = e.currentTarget.checked)}
              data-testid="advanced-workflow-none"
              class="size-4 rounded border-border-strong"
            />
            {t('search.advanced_page.workflow_none')}
          </label>
        </div>

        <div class="flex flex-col gap-6">
          {#each sections as sec (sec.key)}
            <div data-testid="advanced-section-{sec.key}">
              <!-- Every section is named now (#1173, sprint 18d). Before
                   18d the only heading-worthy distinction was "global
                   versus a type", so a lone global section suppressed its
                   own label; the panel now has built-in groups that have
                   to be told apart from each other and from the
                   operator's fields. -->
              <div class="mb-2 border-b border-border pb-1 text-xs font-semibold uppercase tracking-wide text-fg-muted">
                {sec.label}
              </div>

              {#if sec.key === 'media'}
                <!-- ABOUT THE FILE. The two built-in file dimensions sit
                     above the pixel rows because a file's type and size
                     are what a person reaches for first; the pixel
                     fields below them are ordinary `field:` rows that
                     happen to describe the same thing. -->
                <div class="mb-4 flex flex-col gap-4">
                  <ExtensionFilter
                    query={lookupQuery}
                    selected={selectedExtensions}
                    onchange={(v) => (selectedExtensions = v)}
                  />
                  <FileSizeFilter
                    min={fileSize.min}
                    max={fileSize.max}
                    unit={fileSize.unit}
                    onchange={(v) => (fileSize = v)}
                  />
                </div>
              {/if}

              {#if sec.key.startsWith('type-')}
                {@const ref = Number(sec.key.slice(5))}
                {@const domain = assetDomain(ref)}
                {@const states = statesByType[ref] ?? []}
                <!-- The CONCRETE workflow states of this type's domain.
                     ⛔ Keyed by the full `<domain>/<code>` identity:
                     `workflow_states` is UNIQUE (domain, code), so two
                     resource types can both define `published` and they
                     are two different states. Identical codes are never
                     collapsed across domains. -->
                {#if statesFailed.includes(ref)}
                  <p class="mb-3 text-xs text-danger" data-testid="advanced-workflow-error-{ref}">
                    {t('search.advanced_page.workflow_error')}
                  </p>
                {:else if states.length > 0}
                  <div class="mb-4" data-testid="advanced-workflow-states-{ref}">
                    <div class="mb-1.5 text-sm font-medium text-fg">
                      {t('search.advanced_page.workflow_heading')}
                    </div>
                    <div class="flex flex-wrap gap-1.5">
                      {#each states as st (st.id)}
                        {@const identity = `${domain}/${st.code}`}
                        {@const on = selectedStates.includes(identity)}
                        <button
                          type="button"
                          onclick={() => toggleState(identity)}
                          aria-pressed={on}
                          data-testid="advanced-workflow-state-{identity}"
                          class="min-h-11 rounded-full border px-3 py-1 text-xs transition-colors sm:min-h-0
                                 {on
                            ? 'border-accent bg-accent text-on-accent'
                            : 'border-border bg-surface text-fg-muted hover:bg-state-hover hover:text-fg'}"
                        >
                          {st.label || st.code}
                        </button>
                      {/each}
                    </div>
                  </div>
                {/if}
              {/if}

              <!-- A selected type with no scoped fields and no workflow
                   states still gets its section, because the section is
                   where those would go. It says so rather than leaving a
                   heading over nothing. -->
              {#if sec.key.startsWith('type-') && sec.fields.length === 0 && (statesByType[Number(sec.key.slice(5))] ?? []).length === 0 && !statesFailed.includes(Number(sec.key.slice(5)))}
                <p class="mb-2 text-xs text-fg-muted" data-testid="advanced-section-empty-{sec.key}">
                  {t('search.advanced_page.section_type_empty')}
                </p>
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
                      {:else if family === 'number'}
                        <!-- #1173 sprint 18d — the SAME two-bound row a
                             date renders, because it is the same pair of
                             operators over the same `ranges` state. What
                             changed is underneath: ADR 0093's 18b
                             amendment made the bound's DOMAIN a property
                             of the value rather than of the operator, so
                             `>=` reads `value_num` for a field declared
                             `number` and `value_date` for one declared
                             `date`. The two spellings are disjoint, so
                             nothing here has to say which it meant.

                             `step="any"` because a number field is not
                             an integer type — ADR 0012 stores every
                             numeric value in a DOUBLE PRECISION column,
                             and a spinner that snapped to whole numbers
                             would make a fractional bound untypeable. -->
                        <div class="flex flex-wrap items-center gap-2">
                          <label class="flex items-center gap-1.5 text-xs text-fg-muted">
                            {t('search.advanced_page.range_from')}
                            <input
                              type="number"
                              step="any"
                              value={ranges[f.code]?.from ?? ''}
                              oninput={(e) => setRange(f.code, 'from', e.currentTarget.value)}
                              data-testid="field-from-{f.code}"
                              class="min-h-11 w-32 rounded-md border border-border-strong bg-surface px-2 py-1.5 text-sm"
                            />
                          </label>
                          <label class="flex items-center gap-1.5 text-xs text-fg-muted">
                            {t('search.advanced_page.range_to')}
                            <input
                              type="number"
                              step="any"
                              value={ranges[f.code]?.to ?? ''}
                              oninput={(e) => setRange(f.code, 'to', e.currentTarget.value)}
                              data-testid="field-to-{f.code}"
                              class="min-h-11 w-32 rounded-md border border-border-strong bg-surface px-2 py-1.5 text-sm"
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

        <!-- An install with no configured field the caller may filter on
             still gets the built-in dimensions above, so this line
             reports what is genuinely absent rather than standing in for
             the whole panel the way it did before 18d. -->
        {#if filterable.length === 0}
          <p class="mt-4 text-sm text-fg-muted" data-testid="advanced-fields-empty">
            {t('search.advanced_page.fields_empty')}
          </p>
        {/if}
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

<style>
  /* THE STICKY SUBMIT BAR OCCLUDES WHATEVER SCROLLS UNDER IT (#1173,
     sprint 18d).

     The bar is `position: sticky; bottom: 0`, so it floats over the
     bottom of the viewport while the form scrolls behind it. Any
     scroll that brings a control "just into view" therefore lands that
     control UNDERNEATH the bar, where it cannot be tapped — measured at
     390px, where the file-size box came to rest at y=800 with the bar's
     top edge at y=795.

     `scroll-margin-bottom` reserves the bar's height for every
     scroll-into-view the BROWSER performs, not only the ones a test
     drives: tabbing to a control, focusing one after a validation
     failure, and following an in-page anchor all use the same
     mechanism. Fixing it here rather than by padding the page is what
     makes it hold mid-document, where the padding would not help.

     `:global` because the controls it has to reach live in child
     components (the contributor, file-type and file-size pickers), and
     a scoped rule would stop at this file's own markup. */
  :global([data-testid='advanced-search-page'] :is(input, select, textarea, button)) {
    scroll-margin-bottom: 5rem;
  }
</style>

<!-- #1157's residual is CLOSED (#1165). `text`/`longtext` fields have a
     contains box and `date`/`datetime` a range, and they got there the
     way that residual said they had to: by an operator in the shared
     grammar (facet.FieldOp — `~`, `>=`, `<=`) and a case in
     dimensionSQL, not by a widget compiling to something of its own.

     #1165's own residual is CLOSED too (#1173, sprint 18d). `number`
     fields — `pixel_width`, `pixel_height`, and whatever an operator has
     added — render the same two-bound row a date does. They needed no
     new widget: 18b separated a bound's DOMAIN from its OPERATOR, so
     `>=` reads `value_num` for a field declared `number`, and the two
     value spellings are disjoint so nothing has to say which was meant.

     18d also gave the page the BUILT-IN dimensions it had none of:
     contributor (`owner:`), file type (`extension:`), file size
     (`file_size:`) and workflow state (`workflow_state:`). All four are
     the same `filter=<dimension>:<value>` grammar, so `searchParams()`
     is still the only place this page speaks it.

     RESIDUAL, recorded where the next author will see it:

     - `boolean` and `reference` fields: both are equality at heart,
       but a checkbox that means "unset" and "false" differently, and a
       picker that resolves a UUID to a name, are each a design question
       rather than a missing operator.
     - The count is capped at search.TotalCountCap (10,000). Past that
       it reports the cap rather than the true total, which is the
       engine's existing policy and is why the string differs. -->
