<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The operator-defined field values of one subject, edited.
  //
  // ONE component for BOTH subject kinds (#1119, #1389). It began as
  // CollectionFieldsSection, which was the only surface in the app that
  // could edit a field value at all: /assets/{id}/edit wrapped PATCH
  // /assets/{id} and submitted six members, deferring the rest to "the
  // per-field editor (#552)" — an issue that closed in v0.9.0 having
  // shipped a card-display flag and never built one. So `required`,
  // `read_only` and `regexp_filter` were reachable on a collection and
  // unreachable on an asset, and the two surfaces would have drifted
  // the moment they were two files.
  //
  // # THE SAVE MODEL, and the three cases it has to tell apart
  //
  // The shipped model saved every dirty field together through
  // Promise.allSettled and mapped a null value to `undefined`, so an
  // emptied control produced a typed PUT with its value member OMITTED
  // — which both validators then refused ("value_text required for
  // field type text"). Emptying a field was therefore impossible from
  // any surface in the product, and the generic save error was all
  // anybody saw.
  //
  // A dirty field is now one of three things, decided against what was
  // LOADED rather than against what is typed:
  //
  //   non-empty value          -> a typed Set
  //   empty, a value existed   -> a CLEAR, the operation that exists
  //                               for exactly this and had zero callers
  //   empty, none existed      -> NO REQUEST. An untouched blank input
  //                               is not a deletion.
  //
  // # THE BASELINE, and why it is per field
  //
  // Each field carries the `set_at` of the value row it was loaded
  // from, or ABSENCE. That token is the concurrency guard: a mutation
  // sends `if_unchanged_since` for a value that existed and
  // `if_absent: true` for one that did not, and NEVER invents a
  // timestamp for a row nobody wrote. A successful write re-baselines
  // from the response, so a second save from the same open form guards
  // against the version that save produced — not the one the form
  // opened with.
  //
  // On 409 the baseline moves to what the server reports and the user's
  // input is KEPT. There is no silent retry and no fall-back to an
  // unguarded write; falling back would turn the guard into a delay
  // before the clobber it exists to prevent.
  //
  // Members are INDEPENDENT. One field's 409 does not roll back a
  // sibling that already saved: these are per-field operations, not a
  // batch transaction, and inventing all-or-nothing form semantics here
  // would make a conflict on one field lose the work done on another.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { isFieldValueEmpty } from '$lib/fieldDisplay';
  import { fieldPatternViolated } from '$lib/fieldRules';
  import { conditionShows, type ConditionController } from '$lib/displayCondition';
  import { bucketFields, tabStripVisible, resolveTabSelection, groupFields } from '$lib/fieldTabs';
  import FieldValueInput from './FieldValueInput.svelte';
  import FormTabs from './FormTabs.svelte';

  type FieldType =
    | 'text' | 'longtext' | 'rich_text' | 'number' | 'boolean'
    | 'date' | 'datetime' | 'select' | 'multi_select' | 'tree' | 'reference';

  interface FieldDef {
    id: string;
    code: string;
    label: string;
    /** Rendered as input guidance under the control (#1173). */
    description?: string;
    type: FieldType;
    required: boolean;
    display_group?: string;
    options?: Record<string, unknown>;
    /** Drives whether a multi_select offers to create a typed term (#830). */
    open_vocabulary?: boolean;
    write_capability?: string | null;
    /** The system owns this field's values; a person may not write it (#1173). */
    read_only?: boolean;
    /** A pattern the whole value must match; text and longtext only (#1173). */
    regexp_filter?: string | null;
    /**
     * The definition is a VIEW onto a column of `assets` (#822). Such a
     * field is EXCLUDED here, never merely deprioritised: the surface
     * hosting this section already renders the column with a first-class
     * control, and offering a second editor for one value is how the two
     * planes reach states each other calls invalid.
     */
    mirrors_column?: string | null;
    /**
     * Which tab of this form the field sits in (#1173, ADR 0092 §3).
     * `null` = unassigned, which lands in the DEFAULT bucket. Stored and
     * validated since 18b and read by nothing until now.
     */
    edit_tab?: string | null;
    /** The ordering source, and what orders the tab strip too. */
    display_order?: number;
    /**
     * When this field should be offered at all (#1119, ADR 0099).
     * `null`/absent = always. See $lib/displayCondition for the grammar
     * and, more importantly, for the WHOLE-CONDITION FAIL-OPEN rule.
     */
    display_condition?: string[] | null;
  }

  interface Value {
    value_text?: string | null;
    value_num?: number | null;
    value_date?: string | null;
    value_options?: string[] | null;
    value_ref?: string | null;
  }

  /**
   * What the editor believes is stored for a field right now: the token
   * of the value it holds, or `null` for ABSENT. Never `undefined` for a
   * loaded field — "we do not know" is not one of the states, because a
   * guard cannot be chosen without an answer.
   */
  type Baseline = { setAt: string } | null;

  interface Props {
    subjectKind: 'asset' | 'collection';
    subjectId: string;
    /**
     * The asset's resource type, so the server can apply `applies_to`.
     * Already on the loaded row, so no second request and no client-side
     * copy of the extension table. Ignored for collections, which are
     * not applies_to-scoped.
     */
    assetType?: number;
    canEdit?: boolean;
  }
  let { subjectKind, subjectId, assetType, canEdit = true }: Props = $props();

  // Testids stay per subject kind so a spec can say which surface it is
  // driving, and so the collection ids the shipped suite asserts on are
  // untouched.
  const tid = $derived(subjectKind === 'collection' ? 'collection-fields' : 'asset-fields');
  const i18nPrefix = $derived(subjectKind === 'collection' ? 'collection_fields' : 'asset_fields');

  let definitions = $state<FieldDef[]>([]);
  /**
   * SERVER-DERIVED effective readability, field id -> boolean, from
   * `GET /{subject}/{id}/field-composition` (#1173, ADR 0099 §5).
   *
   * ⛔ THIS CANNOT BE DERIVED IN THE BROWSER AND MUST NOT BE ATTEMPTED.
   * `auth.caps` carries GLOBALLY held capability codes only, so a grant
   * scoped to the team that owns this asset is invisible here — the asset
   * edit page's own source has said exactly that since #939. Inferring
   * readability from it would answer "no" for precisely the operator the
   * field was configured for.
   *
   * It also cannot be inferred from whether a VALUE arrived. The value
   * endpoints drop unreadable rows, so "withheld" and "never set" look
   * identical there, and those two have OPPOSITE consequences: withheld
   * makes a term unevaluable and SHOWS the dependent, while a readable
   * controller holding nothing is a real FALSE and HIDES it. Telling them
   * apart is the whole reason this second request exists.
   *
   * A missing entry (the request failed, or the definition is not in the
   * composition set) reads as UNRESOLVABLE and fails the condition open.
   */
  let readable = $state<Record<string, boolean>>({});
  let values = $state<Record<string, Value>>({});
  let baselines = $state<Record<string, Baseline>>({});
  let dirty = $state<Record<string, true>>({});
  let conflicts = $state<Record<string, string>>({});
  let fieldErrors = $state<Record<string, string>>({});
  let loading = $state(true);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state<string | null>(null);

  onMount(() => void load());

  async function loadDefinitions(): Promise<FieldDef[]> {
    if (subjectKind === 'collection') {
      const res = await api.GET('/fields', {
        params: { query: { subject_kind: 'collection' } },
      });
      return (res.data ?? []) as FieldDef[];
    }
    // NO `status`, deliberately. That is the live-schema reading (#528):
    // active + deprecated, archived excluded. A DEPRECATED definition is
    // one an operator stopped wanting NEW values in, and existing rows
    // may still legitimately hold one — hiding it would drop live data
    // out of the editor and, worse, out of a person's mental model of
    // what this record contains. An ARCHIVED definition is a tombstone
    // and is not offered; a value stored against one is left alone, and
    // not rendering it must never be what clears it.
    //
    // The composer surfaces (upload, /create) ask the opposite question
    // and pass `status=active`, which is why widening this cannot widen
    // them.
    //
    // NO asset_type, NO fields. `Asset.asset_type` is optional in the
    // schema (only `id` and `restricted` are required), and omitting
    // the parameter would take the OTHER branch of GET /fields, which
    // has no subject-kind filter at all: the page would render every
    // collection-scoped definition on an asset. Refusing to ask is the
    // safe answer, and `applies_to` cannot be evaluated without it
    // anyway.
    if (assetType == null) return [];
    const res = await api.GET('/fields', {
      params: { query: { asset_type: assetType } },
    });
    return (res.data ?? []) as FieldDef[];
  }

  /**
   * The per-field readability map for THIS subject.
   *
   * Failure is swallowed on purpose and leaves the map empty, which makes
   * every controller unresolvable and every condition fail OPEN — every
   * field is shown. That is the safe direction: a condition is form
   * composition and never authorization, so the worst outcome of not
   * knowing is a form with more boxes on it than the operator intended,
   * while the opposite default would hide fields because a request
   * failed.
   */
  async function loadReadability(): Promise<Record<string, boolean>> {
    const res =
      subjectKind === 'collection'
        ? await api.GET('/collections/{id}/field-composition', {
            params: { path: { id: subjectId } },
          })
        : await api.GET('/assets/{id}/field-composition', { params: { path: { id: subjectId } } });
    const out: Record<string, boolean> = {};
    for (const r of (res.data ?? []) as Array<{ field_id: string; readable: boolean }>) {
      out[r.field_id] = r.readable;
    }
    return out;
  }

  async function loadValues(): Promise<Array<{ field_id: string; set_at?: string } & Value>> {
    const res =
      subjectKind === 'collection'
        ? await api.GET('/collections/{id}/fields', { params: { path: { id: subjectId } } })
        : await api.GET('/assets/{id}/fields', { params: { path: { id: subjectId } } });
    return (res.data ?? []) as Array<{ field_id: string; set_at?: string } & Value>;
  }

  async function load() {
    loading = true;
    try {
      const [defs, vals, readableMap] = await Promise.all([
        loadDefinitions(),
        loadValues(),
        loadReadability(),
      ]);
      readable = readableMap;
      // Mirrored definitions are dropped here rather than being merely
      // ordered last. GET /assets/{id}/fields DOES return title and
      // description (the list path merges the mirrored columns in), so
      // without this filter the asset edit page would render a second
      // title box beside its own.
      definitions = defs.filter((d) => !d.mirrors_column);
      const keep = new Set(definitions.map((d) => d.id));

      const initialValues: Record<string, Value> = {};
      const initialBaselines: Record<string, Baseline> = {};
      for (const v of vals) {
        if (!keep.has(v.field_id)) continue;
        initialValues[v.field_id] = {
          value_text: v.value_text ?? null,
          value_num: v.value_num ?? null,
          value_date: v.value_date ?? null,
          value_options: v.value_options ?? null,
          value_ref: v.value_ref ?? null,
        };
        // The token is the value row's OWN set_at, kept as the STRING
        // the server sent. Re-parsing and re-formatting a timestamp is
        // how a guard starts failing on microsecond rounding.
        initialBaselines[v.field_id] = v.set_at ? { setAt: v.set_at } : null;
      }
      for (const d of definitions) {
        if (!(d.id in initialBaselines)) initialBaselines[d.id] = null;
      }
      values = initialValues;
      baselines = initialBaselines;
      dirty = {};
      conflicts = {};
      fieldErrors = {};
    } catch {
      error = t(`${i18nPrefix}.save_error`);
    } finally {
      loading = false;
    }
  }

  function onFieldChange(fieldId: string, patch: Value) {
    values[fieldId] = { ...(values[fieldId] ?? {}), ...patch };
    dirty[fieldId] = true;
    // A fresh edit supersedes the last verdict on this field. The
    // baseline does NOT move: it is what is stored, not what is typed.
    delete conflicts[fieldId];
    delete fieldErrors[fieldId];
    saved = false;
  }

  /**
   * The typed Set body: the ONE value_* member the field's type uses,
   * plus the guard.
   *
   * Sending all five with `undefined` for the unused ones is what the
   * shipped model did, and it is why an emptied control produced a PUT
   * with no value member at all. One member, chosen by type, cannot
   * express that mistake.
   */
  function setBody(def: FieldDef, v: Value, base: Baseline): Record<string, unknown> {
    const body: Record<string, unknown> = {};
    switch (def.type) {
      case 'text':
      case 'longtext':
      case 'rich_text':
      case 'select':
      case 'tree':
        body.value_text = v.value_text ?? '';
        break;
      case 'number':
      case 'boolean':
        body.value_num = v.value_num;
        break;
      case 'date':
      case 'datetime':
        body.value_date = v.value_date;
        break;
      case 'multi_select':
        body.value_options = v.value_options ?? [];
        break;
      case 'reference':
        body.value_ref = v.value_ref;
        break;
    }
    if (base) body.if_unchanged_since = base.setAt;
    else body.if_absent = true;
    return body;
  }

  type Outcome =
    | { kind: 'set'; setAt: string; value: Value }
    | { kind: 'cleared' }
    | { kind: 'conflict'; present: boolean; setAt?: string; message: string }
    | { kind: 'error'; message: string };

  function conflictMessage(present: boolean): string {
    return present ? t('field_value.conflict_changed') : t('field_value.conflict_removed');
  }

  async function submitSet(def: FieldDef, v: Value, base: Baseline): Promise<Outcome> {
    const body = setBody(def, v, base) as never;
    const res =
      subjectKind === 'collection'
        ? await api.PUT('/collections/{id}/fields/{field_id}', {
            params: { path: { id: subjectId, field_id: def.id } },
            body,
          })
        : await api.PUT('/assets/{id}/fields/{field_id}', {
            params: { path: { id: subjectId, field_id: def.id } },
            body,
          });
    if (res.response.status === 409) {
      const c = res.error as { present?: boolean; current?: { set_at?: string } | null } | undefined;
      const present = c?.present === true;
      return {
        kind: 'conflict',
        present,
        setAt: present ? c?.current?.set_at : undefined,
        message: conflictMessage(present),
      };
    }
    if (res.error || !res.data) {
      return { kind: 'error', message: serverMessage(res.error) };
    }
    const d = res.data as { set_at: string } & Value;
    return {
      kind: 'set',
      setAt: d.set_at,
      value: {
        value_text: d.value_text ?? null,
        value_num: d.value_num ?? null,
        value_date: d.value_date ?? null,
        value_options: d.value_options ?? null,
        value_ref: d.value_ref ?? null,
      },
    };
  }

  async function submitClear(def: FieldDef, base: Baseline): Promise<Outcome> {
    // A Clear only ever runs against a value the editor LOADED, so the
    // guard is always a timestamp. `if_absent` has no meaning here:
    // "remove it only if it is not there" has nothing to remove.
    const query = base ? { if_unchanged_since: base.setAt } : {};
    const res =
      subjectKind === 'collection'
        ? await api.DELETE('/collections/{id}/fields/{field_id}', {
            params: { path: { id: subjectId, field_id: def.id }, query },
          })
        : await api.DELETE('/assets/{id}/fields/{field_id}', {
            params: { path: { id: subjectId, field_id: def.id }, query },
          });
    if (res.response.status === 409) {
      const c = res.error as { present?: boolean; current?: { set_at?: string } | null } | undefined;
      const present = c?.present === true;
      return {
        kind: 'conflict',
        present,
        setAt: present ? c?.current?.set_at : undefined,
        message: conflictMessage(present),
      };
    }
    if (res.response.status === 204) return { kind: 'cleared' };
    return { kind: 'error', message: serverMessage(res.error) };
  }

  /**
   * The SERVER's own sentence, not a generic one. `required`,
   * `read_only` and `pattern_mismatch` all arrive here, and each names
   * the field and says what to do about it — which a single "failed to
   * save one or more fields" cannot.
   */
  function serverMessage(err: unknown): string {
    const m = (err as { error?: string } | undefined)?.error;
    return m && m.trim() ? m : t(`${i18nPrefix}.save_error`);
  }

  async function save() {
    if (saving) return;
    const ids = Object.keys(dirty);
    if (ids.length === 0) return;
    saving = true;
    saved = false;
    error = null;

    // NO MUTATION: a field that is empty and was already absent is
    // simply not dirty in any meaningful sense. Dropped before any
    // request, so an untouched blank control never manufactures a
    // DELETE.
    const planned: Array<{ def: FieldDef; base: Baseline; clear: boolean }> = [];
    for (const id of ids) {
      const def = definitions.find((d) => d.id === id);
      if (!def) {
        delete dirty[id];
        continue;
      }
      // ⛔ A HIDDEN FIELD IS NEVER SUBMITTED, AND ITS DRAFT IS NEVER
      // DESTROYED (ADR 0099 §4). It stays dirty and its typed value stays
      // in `values`, so revealing it brings the draft back; it is simply
      // not part of THIS save. `delete dirty[id]` here would silently
      // discard a person's typing because an unrelated control changed.
      //
      // This is also what makes hiding non-destructive on the server
      // side: no Set, no Clear and no empty row is emitted for it, so the
      // persisted value is byte-identical afterwards.
      if (!visible[id]) continue;
      const v = values[id] ?? {};
      const base = baselines[id] ?? null;
      const empty = isFieldValueEmpty(def.type, v);
      if (empty && base === null) {
        delete dirty[id];
        continue;
      }
      planned.push({ def, base, clear: empty });
    }
    if (planned.length === 0) {
      saving = false;
      saved = true;
      return;
    }

    try {
      const results = await Promise.allSettled(
        planned.map((p) =>
          p.clear ? submitClear(p.def, p.base) : submitSet(p.def, values[p.def.id] ?? {}, p.base),
        ),
      );

      let anyFailed = false;
      results.forEach((r, i) => {
        const { def } = planned[i];
        if (r.status === 'rejected') {
          anyFailed = true;
          fieldErrors[def.id] = t(`${i18nPrefix}.save_error`);
          return;
        }
        const out = r.value;
        switch (out.kind) {
          case 'set':
            // Re-baselined from the RESPONSE, so the next save from
            // this still-open form guards against the version this one
            // produced. Keeping the pre-save token here is what would
            // make a second save conflict with its own first.
            baselines[def.id] = { setAt: out.setAt };
            values[def.id] = out.value;
            delete dirty[def.id];
            delete conflicts[def.id];
            delete fieldErrors[def.id];
            break;
          case 'cleared':
            baselines[def.id] = null;
            delete dirty[def.id];
            delete conflicts[def.id];
            delete fieldErrors[def.id];
            break;
          case 'conflict':
            anyFailed = true;
            // The user's input STAYS, and stays dirty: the save was
            // refused, not undone. The baseline moves to what the
            // server reports, so pressing save again is a deliberate
            // write over the newer value rather than another 409.
            baselines[def.id] = out.present && out.setAt ? { setAt: out.setAt } : null;
            conflicts[def.id] = out.message;
            break;
          case 'error':
            anyFailed = true;
            fieldErrors[def.id] = out.message;
            break;
        }
      });

      // The section-level line stays, because it is what a person sees
      // first — but the per-field sentences are the ones that say what
      // happened, and a member that succeeded is NOT rolled back
      // because a sibling did not.
      if (anyFailed) error = t(`${i18nPrefix}.save_error`);
      else saved = true;
    } finally {
      saving = false;
    }
  }

  // A dirty field whose value breaks its own pattern is refused HERE,
  // before the round trip. Convenience only: the pattern may change
  // after this form loaded, and the server's 422 renders on the field
  // like any other refusal when that happens.
  const patternBlocked = $derived(
    Object.keys(dirty).some((id) => {
      const def = definitions.find((d) => d.id === id);
      if (!def) return false;
      // A hidden field is not submitted, so its draft cannot block a
      // save. Otherwise a pattern violation in a box nobody can see would
      // disable the button with no visible cause.
      if (!visible[id]) return false;
      return fieldPatternViolated(def, values[id]?.value_text ?? '');
    }),
  );

  // Counts only what this save would actually SEND. A hidden dirty field
  // is real and is kept, but enabling the button for it would promise a
  // write that never happens.
  const dirtyCount = $derived(Object.keys(dirty).filter((id) => visible[id]).length);

  // ── conditional visibility (#1119, ADR 0099 §4) ───────────────────
  //
  // ⛔ THE CONTROLLER'S VALUE IS READ FROM THE IN-FORM STATE, not from
  // what was loaded. A person changing `work_type` has to see the
  // dependent appear or disappear as they type; reading the persisted
  // value would only re-evaluate after a save.
  //
  // ⛔ AND IT IS READ REGARDLESS OF THE CONTROLLER'S OWN VISIBILITY. A
  // controller hidden by its own condition STILL CONTRIBUTES its value:
  // visibility is a rendering decision and does not remove a value from
  // the model. Skipping hidden controllers here would make a chain of
  // conditions collapse the moment its first link was hidden.
  function resolveController(code: string): ConditionController | undefined {
    const def = definitions.find((d) => d.code === code);
    // Not in the composition set at all: unresolvable, fails open. This
    // is also what an ARCHIVED controller looks like from here, which is
    // exactly the later-archive drift behaviour — the stored condition is
    // untouched and the dependent is simply shown again.
    if (!def) return undefined;
    const r = readable[def.id];
    if (r === undefined) return undefined;
    const v = values[def.id] ?? {};
    return {
      type: def.type,
      readable: r,
      text: v.value_text ?? '',
      options: v.value_options ?? [],
    };
  }

  /** Field ids currently SHOWN. Absent from a condition means shown. */
  const visible = $derived.by(() => {
    const out: Record<string, boolean> = {};
    for (const d of definitions) {
      out[d.id] = conditionShows(d.display_condition, resolveController);
    }
    return out;
  });

  // ── tab buckets (#1173, ADR 0099 §9) ──────────────────────────────
  //
  // ⛔ POLICY B: the buckets come from `definitions`, the composition
  // ELIGIBLE set, and NOT from what is currently visible. A tab whose
  // every field is hidden keeps its chrome and its selection and shows an
  // empty-state line. Deriving the strip from visible controls would make
  // a tab vanish from under the selection as a side effect of typing in a
  // different tab, taking any unsaved work in it out of reach.
  const buckets = $derived(bucketFields(definitions));
  const showTabs = $derived(tabStripVisible(buckets));

  let selectedTab = $state<string | null>(null);
  // Only ever moves when the selected tab STOPS EXISTING, which is a
  // configuration change. A tab merely emptying out still matches, so the
  // selection stays put — that is the Policy B half a naive reset breaks.
  const activeTab = $derived(resolveTabSelection(selectedTab, buckets));

  const tabItems = $derived(
    buckets.map((b) => ({ id: b.id, label: b.name ?? t('field_tabs.default') })),
  );

  /** The definitions of the selected tab, or all of them with no strip. */
  const tabDefs = $derived.by(() => {
    if (buckets.length === 0) return [] as FieldDef[];
    const b = buckets.find((x) => x.id === activeTab) ?? buckets[0];
    return b.fields;
  });

  /**
   * `display_group` fieldsets INSIDE the selected tab.
   *
   * Tabs are coarser than groups and the nesting goes one way only: a
   * group never spans two tabs, because a tab is chosen first.
   *
   * The grouping itself is `$lib/fieldTabs`'s `groupFields`, which
   * `/create` also calls. It was derived inline here and nowhere else,
   * which is why the create page rendered a flat list; one function is
   * what stops the two surfaces disagreeing about what a form looks like.
   *
   * Hidden fields are dropped HERE and not from `buckets`, which is the
   * Policy B split: the tab exists because the DEFINITION does, and the
   * control is absent because the CONDITION is false.
   */
  const grouped = $derived(groupFields(tabDefs.filter((d) => visible[d.id])));

  /** Nothing to draw in the selected tab, but the tab still exists. */
  const tabIsEmpty = $derived(buckets.length > 0 && grouped.length === 0);
</script>

{#if !(subjectKind === 'asset' && !loading && definitions.length === 0)}
  <!--
    N=0 on an ASSET renders nothing at all — no heading, no empty-state
    line. An asset type with no configured fields is the ordinary case,
    and a section announcing its own emptiness on every asset edit page
    would be chrome charged to every install that never defined a field.
    The collection modal keeps its message, which is what its shipped
    spec asserts and what an admin opening a collection editor expects.
  -->
  <section class="space-y-3" data-testid="{tid}-section">
    <h3 class="text-sm font-semibold text-fg">{t(`${i18nPrefix}.section_title`)}</h3>
    {#if loading}
      <p class="text-sm text-fg-muted">{t('common.loading')}</p>
    {:else if definitions.length === 0}
      <p class="text-sm text-fg-muted" data-testid="{tid}-empty">{t(`${i18nPrefix}.no_fields`)}</p>
    {:else}
      <!--
        THE TAB STRIP. Drawn only from two buckets up: one bucket is not a
        choice, and a single-segment strip is chrome that says nothing.
        The buckets come from DEFINITIONS (Policy B), so this strip does
        not move while a person types.
      -->
      {#if showTabs}
        <FormTabs
          tabs={tabItems}
          active={activeTab ?? ''}
          onSelect={(id) => (selectedTab = id)}
          label={t('field_tabs.aria_label')}
          panelId="{tid}-tabpanel"
          idPrefix="{tid}-tab"
          testid="{tid}-tabs"
        />
      {/if}
      <div
        id="{tid}-tabpanel"
        role={showTabs ? 'tabpanel' : undefined}
        aria-labelledby={showTabs ? `${tid}-tab-${activeTab ?? ''}` : undefined}
        class="space-y-3"
      >
      <!--
        The tab is here and has nothing to show: every field in it is
        hidden by a condition. Policy B keeps the tab and the selection
        and says so, rather than removing the tab under the person.
      -->
      {#if tabIsEmpty}
        <p class="text-sm text-fg-muted" data-testid="{tid}-tab-empty">{t('field_tabs.empty')}</p>
      {/if}
      {#each grouped as group (group.name)}
        <fieldset class="rounded border border-border bg-surface p-3">
          <legend class="px-1 text-xs font-medium uppercase tracking-wider text-fg-muted">{group.name}</legend>
          <!--
            Two columns once there is room for them. An install with
            twenty-five configured fields is ordinary, and stacking them
            in one narrow column on a 1440px screen makes the person
            scroll past everything to reach the save button.
          -->
          <div class="grid grid-cols-1 gap-x-6 gap-y-3 md:grid-cols-2">
          {#each group.fields as def (def.id)}
            <div data-testid="{tid}-row-{def.code}">
              <FieldValueInput
                {def}
                value={values[def.id] ?? {}}
                disabled={!canEdit}
                onchange={(v) => onFieldChange(def.id, v)}
              />
              <!--
                PER-FIELD outcomes. A conflict on one field must be
                visible AS that field's conflict: a single section-level
                banner cannot say which of three dirty fields was
                refused, and the user's other two saved fine.
              -->
              {#if conflicts[def.id]}
                <p
                  role="alert"
                  data-testid="field-conflict-{def.code}"
                  class="mt-1 rounded border border-warning/40 bg-warning/10 px-2 py-1 text-xs text-warning"
                >
                  {conflicts[def.id]}
                </p>
              {/if}
              {#if fieldErrors[def.id]}
                <p
                  role="alert"
                  data-testid="field-error-{def.code}"
                  class="mt-1 rounded border border-danger/40 bg-danger-container px-2 py-1 text-xs text-danger"
                >
                  {fieldErrors[def.id]}
                </p>
              {/if}
            </div>
          {/each}
          </div>
        </fieldset>
      {/each}
      </div>

      {#if error}
        <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger" data-testid="{tid}-error">{error}</p>
      {/if}
      {#if saved}
        <p class="rounded border border-success/40 bg-success-container px-3 py-2 text-sm text-success" data-testid="{tid}-saved">{t(`${i18nPrefix}.saved`)}</p>
      {/if}

      {#if canEdit}
        <button
          type="button"
          onclick={save}
          disabled={saving || dirtyCount === 0 || patternBlocked}
          data-testid="{tid}-save"
          class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
        >
          {saving ? t('common.loading') : t(`${i18nPrefix}.save`)}
        </button>
      {/if}
    {/if}
  </section>
{/if}
