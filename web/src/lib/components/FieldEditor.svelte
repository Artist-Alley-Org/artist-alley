<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<!--
  Edits one field definition — its label, whether it is required, and
  (for select / multi_select / tree) its controlled vocabulary. ADR 0012
  plus its 2026-07-30 and 2026-07-31 amendments.

  Every vocabulary control is PATH-addressed (#779/#825), so it reaches
  a term at any depth. Before this, the controls were indexed against
  the top level, which made a `tree` field's nested terms visible and
  read-only: an operator could see `gb / United Kingdom` under `europe`
  and could not relabel, retire, reparent or extend it. A flat
  vocabulary is just the depth-0 case of the same renderer rather than a
  second code path that can drift from this one.

  Deletion is deliberately absent: hard-deleting an option orphans the
  values assets already store, and the orphan surfaces as a blank on an
  asset nobody edited. Retire a term with `deprecated` (stops being
  offered, keeps resolving) or `archived` (hard retire) instead. That
  holds at every depth — a leaf is retired exactly like a top-level
  term, because a `tree` value is a leaf slug and the same orphan is
  available.

  Reparenting is safe to offer without a data migration because a tree
  value stores ONE slug and slugs are unique across the whole
  vocabulary: the term keeps resolving wherever it sits. The move is a
  destination PICKER rather than a drag: nested-list drag-and-drop is
  hostile to a coarse pointer at 390px, and the operation has to be
  tappable.

  The save is conflict-detectable — it sends the `updated_at` the row
  was loaded with as `if_unchanged_since` and surfaces a 409 rather
  than silently retrying, because a silent retry is the clobber this
  guard exists to prevent.

  #854 moved this editor out of a table cell and onto
  /admin/fields/{code}, and gave it the long tail the index used to
  print read-only: description, display group / order and applies-to,
  behind a collapsed Advanced disclosure. They live HERE rather than in
  a second form on the page for one reason — every write to a field
  definition carries the same `if_unchanged_since` baseline, so two
  forms saving independently would each invalidate the other's baseline
  and manufacture conflicts out of the operator's own edits.
-->
<script lang="ts">
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import {
    allOptionSlugs,
    childrenAtPath,
    flattenOptions,
    insertOptionAtPath,
    moveDestinations,
    moveOptionWithinSiblings,
    normalizeOptions,
    reparentOption,
    serializeOptions,
    slugify,
    updateOptionAtPath,
    type FieldOption,
    type OptionPath,
    type OptionStatus,
  } from '$lib/fieldOptions';

  let {
    fieldId,
    fieldType,
    initialLabel,
    initialDescription = '',
    initialRequired,
    initialOptions,
    initialOpenVocabulary = false,
    initialShowOnCard = false,
    initialShowInAdvancedSearch = true,
    initialShowOnUpload = true,
    initialEditTab = null,
    initialSearchable = true,
    initialReadOnly = false,
    initialRegexpFilter = null,
    initialDisplayCondition = null,
    initialMirrorsColumn = null,
    initialReadCapability = null,
    initialWriteCapability = null,
    initialDisplayGroup = '',
    initialDisplayOrder = 0,
    initialAppliesTo = [],
    subjectKind = 'asset',
    assetTypes = [],
    initialUpdatedAt,
    onSaved = () => {},
  }: {
    fieldId: string;
    fieldType: string;
    initialLabel: string;
    initialDescription?: string;
    initialRequired: boolean;
    initialOptions: Record<string, unknown> | undefined;
    initialOpenVocabulary?: boolean;
    initialShowOnCard?: boolean;
    /** The participation flags (#1173, ADR 0092 §3). They default TRUE
     *  and null here for the same reason the columns do: a field that
     *  has never been configured must render exactly where it renders
     *  today, so "absent" has to mean "everywhere", not "nowhere". */
    initialShowInAdvancedSearch?: boolean;
    initialShowOnUpload?: boolean;
    initialEditTab?: string | null;
    /** Whether this field's text feeds the FULL-TEXT INDEX (#1173).
     *  Defaults true, as the column does. Not a participation flag and
     *  not a filterability switch: see the section note in the markup. */
    initialSearchable?: boolean;
    /** The input rules (#1173). `read_only` refuses HUMAN writes;
     *  `regexp_filter` is the pattern a human value must match, with
     *  null meaning no constraint. */
    initialReadOnly?: boolean;
    initialRegexpFilter?: string | null;
    /**
     * The stored condition, or null for "always shown" (#1119,
     * ADR 0099). Edited as ONE TERM PER LINE, because that is the shape
     * an operator can read back: a comma-separated list is ambiguous the
     * moment a value contains a comma, and values here are operator text.
     */
    initialDisplayCondition?: string[] | null;
    /** Set when this field is a VIEW onto a column of the asset row
     *  (#822). Neither input rule can be configured on one, so the
     *  controls are replaced by the reason rather than shown dead. */
    initialMirrorsColumn?: string | null;
    /** Present only so the editor can explain WHY the card toggle is
     *  unavailable. The server refuses the combination either way. */
    initialReadCapability?: string | null;
    /** Shown, never edited — see the capabilities note in the markup. */
    initialWriteCapability?: string | null;
    initialDisplayGroup?: string;
    initialDisplayOrder?: number;
    initialAppliesTo?: number[];
    /** `applies_to` is meaningless on a collection field — the server
     *  ignores it — so the control is not offered there rather than
     *  offered and silently discarded. */
    subjectKind?: 'asset' | 'collection';
    /** Named types for the applies-to picker. A ref with no name still
     *  renders (as its ref), because a field scoped to a type this
     *  instance has since removed must stay legible. */
    assetTypes?: { ref: number; name?: string | null }[];
    initialUpdatedAt: string;
    onSaved?: () => void;
  } = $props();

  const STATUSES: OptionStatus[] = ['active', 'deprecated', 'archived'];
  // `tree` belongs here (#820). It was excluded, so the editor told the
  // operator "a tree field has no option list" — of the one shipped
  // tree field, `country`, whose whole vocabulary is its option list.
  // The claim was true of the DATA until #820 gave country a
  // vocabulary; it was never true of the TYPE.
  const hasVocabulary =
    fieldType === 'select' || fieldType === 'multi_select' || fieldType === 'tree';

  // Nesting controls are offered on `tree` alone. A select field with
  // children is not a smaller tree — selectableOptions never descends,
  // so a child added to one would be a term no picker ever offers.
  // Offering the control there would be offering a way to lose work.
  const isTree = fieldType === 'tree';

  // open_vocabulary is legal on any type and HONOURED on multi_select
  // only (openVocabularyApplies, app/internal/metadata/open_vocabulary.go).
  // The toggle is therefore offered on multi_select and nowhere else:
  // a control that saves a value the server ignores is a control that
  // lies, and a disabled one on `select` would advertise a setting the
  // operator can never reach. Opening `select` — or `tree`, which has
  // to decide WHERE a new term lands — widens one function server-side
  // and this condition with it.
  const canOpenVocabulary = fieldType === 'multi_select';

  /**
   * The stored array as editable text: ONE TERM PER LINE.
   *
   * A line, not a comma-separated list. Values here are operator text and
   * a value containing a comma is ordinary (`work_type=Poster, framed`),
   * so a comma separator would be ambiguous exactly where it mattered.
   * The term grammar itself has no line-sensitive characters, so a
   * newline is the one separator that cannot appear inside a term.
   */
  function displayConditionToText(v: string[] | null | undefined): string {
    return (v ?? []).join('\n');
  }

  /**
   * The text back to the array the API takes.
   *
   * BLANK LINES ARE DROPPED, so an operator pressing Enter twice does not
   * send a blank term the server would refuse; each surviving line is
   * trimmed, which matches what the parser does to a term anyway. An
   * empty result means the box was emptied, and the caller turns that
   * into the explicit clear rather than an empty array — the server
   * refuses `[]` so that "no condition" has exactly one representation.
   */
  function displayConditionTerms(text: string): string[] {
    return text
      .split('\n')
      .map((line) => line.trim())
      .filter((line) => line !== '');
  }

  let label = $state(initialLabel);
  let description = $state(initialDescription);
  let required = $state(initialRequired);
  let openVocab = $state(initialOpenVocabulary);
  let showOnCard = $state(initialShowOnCard);
  let showInAdvancedSearch = $state(initialShowInAdvancedSearch);
  let showOnUpload = $state(initialShowOnUpload);
  let editTab = $state(initialEditTab ?? '');
  let searchable = $state(initialSearchable);
  let readOnly = $state(initialReadOnly);
  // NOT trimmed anywhere in this file, deliberately: whitespace inside a
  // pattern is meaningful, and `\A(?:   )\z` legitimately matches three
  // spaces. The empty string is the CLEAR, and it is the only value that
  // means "no pattern" here — the server refuses a stored `""`.
  let regexpFilter = $state(initialRegexpFilter ?? '');
  // One term per LINE. Blank lines are ignored on save, so an operator
  // pressing Enter twice does not send a blank term the server would
  // refuse. NOT trimmed as a whole: the per-line trim happens at save,
  // which is also where the empty box becomes the explicit clear.
  let displayCondition = $state(displayConditionToText(initialDisplayCondition));

  // The long tail (#854). Collapsed by default: an operator opening a
  // field to relabel it or curate its vocabulary should not have to
  // scroll past grouping and scoping to reach the save button.
  let advancedOpen = $state(false);
  let displayGroup = $state(initialDisplayGroup);
  let displayOrder = $state(initialDisplayOrder);
  let appliesTo = $state<number[]>([...initialAppliesTo]);
  const appliesToAll = $derived(appliesTo.length === 0);
  const showAppliesTo = $derived(subjectKind === 'asset');

  function toggleAppliesTo(ref: number, on: boolean) {
    appliesTo = on ? [...appliesTo, ref].sort((a, b) => a - b) : appliesTo.filter((r) => r !== ref);
  }

  // A gated field cannot be an at-a-glance field (#552): a card renders on
  // browse, for a page of assets, where the server has evaluated no
  // per-field capability. The server refuses the combination with a 400, so
  // the control is REPLACED by the reason rather than shown disabled — a
  // disabled checkbox advertises a setting the operator can never reach
  // here, which is the same complaint the open_vocabulary note above makes.
  const cardGated = $derived(!!(initialReadCapability ?? '').trim());

  // A mirrored field is a view onto a column of the asset row (#822),
  // and that column has its own human write plane: the upload form
  // writes the title, the asset editor rewrites both. Neither input
  // rule can be configured on one, because only the field plane would
  // obey it — the server refuses both with a 400 and migration 00064
  // refuses them again in a CHECK. So the controls are REPLACED by the
  // reason, following the cardGated precedent above: a disabled
  // checkbox advertises a setting the operator can never reach.
  const mirrored = $derived(!!(initialMirrorsColumn ?? '').trim());

  // An input pattern is honoured for `text` and `longtext` only, which
  // is `regexpFilterApplies` server-side. `rich_text` is excluded
  // despite sharing the storage column: what lands there is sanitised
  // markup, so a pattern would match tags rather than the words the
  // operator can see. Offering the box anywhere else would be offering
  // a setting the server refuses.
  const regexpSupported = fieldType === 'text' || fieldType === 'longtext';
  let opts = $state<FieldOption[]>(normalizeOptions(initialOptions));
  // Baseline for the optimistic-concurrency guard. Re-based (not
  // reset) after a save so consecutive edits keep working.
  let baseline = $state(initialUpdatedAt);
  let saving = $state(false);
  let error = $state('');
  let savedMsg = $state('');
  let conflict = $state(false);
  let newSlug = $state('');

  // At most one inline add-form and one move-picker are open at a
  // time. Both are anchored to a PATH rather than a slug: a new term
  // has no slug until it is confirmed, and "after this node" is a
  // position, not a term.
  type PendingAdd = { parentPath: OptionPath; index: number; anchor: string; kind: 'child' | 'sibling' };
  let adding = $state<PendingAdd | null>(null);
  let addText = $state('');
  let addError = $state('');
  let moving = $state<OptionPath | null>(null);

  let snapshot = $state(JSON.stringify(serializeOptions(normalizeOptions(initialOptions))));
  let labelSnapshot = $state(initialLabel);
  let descriptionSnapshot = $state(initialDescription);
  let requiredSnapshot = $state(initialRequired);
  let openVocabSnapshot = $state(initialOpenVocabulary);
  let showOnCardSnapshot = $state(initialShowOnCard);
  let showInAdvancedSearchSnapshot = $state(initialShowInAdvancedSearch);
  let showOnUploadSnapshot = $state(initialShowOnUpload);
  let editTabSnapshot = $state(initialEditTab ?? '');
  let searchableSnapshot = $state(initialSearchable);
  let readOnlySnapshot = $state(initialReadOnly);
  let regexpFilterSnapshot = $state(initialRegexpFilter ?? '');
  let displayConditionSnapshot = $state(displayConditionToText(initialDisplayCondition));
  let displayGroupSnapshot = $state(initialDisplayGroup);
  let displayOrderSnapshot = $state(initialDisplayOrder);
  let appliesToSnapshot = $state(JSON.stringify([...initialAppliesTo]));
  const dirty = $derived(
    JSON.stringify(serializeOptions(opts)) !== snapshot ||
      label !== labelSnapshot ||
      description !== descriptionSnapshot ||
      required !== requiredSnapshot ||
      openVocab !== openVocabSnapshot ||
      showOnCard !== showOnCardSnapshot ||
      showInAdvancedSearch !== showInAdvancedSearchSnapshot ||
      showOnUpload !== showOnUploadSnapshot ||
      editTab.trim() !== (editTabSnapshot ?? '').trim() ||
      searchable !== searchableSnapshot ||
      readOnly !== readOnlySnapshot ||
      // Exact, not trimmed: " " and "" are different patterns.
      regexpFilter !== regexpFilterSnapshot ||
      // Compared through the same normalisation the SAVE uses, so
      // re-ordered whitespace or a trailing newline is not a change.
      JSON.stringify(displayConditionTerms(displayCondition)) !==
        JSON.stringify(displayConditionTerms(displayConditionSnapshot)) ||
      displayGroup !== displayGroupSnapshot ||
      displayOrder !== displayOrderSnapshot ||
      JSON.stringify(appliesTo) !== appliesToSnapshot,
  );

  // Only active terms make sense as a successor — pointing a
  // deprecation at another deprecation just moves the problem. The
  // whole vocabulary is eligible, not just the top level: a retired
  // leaf's replacement is usually another leaf. (The server accepts
  // any existing slug; the narrowing is this editor's, deliberately.)
  const successorChoices = $derived(flattenOptions(opts).filter((o) => o.status === 'active'));

  // Tree-wide, which is the rule NormalizeOptionsDoc enforces. For a
  // flat vocabulary this is the same set as the top level, so the
  // select / multi_select behaviour is unchanged.
  const takenSlugs = $derived(allOptionSlugs(opts));

  const keyOf = (p: OptionPath) => p.join('.');
  const samePath = (a: OptionPath | null, b: OptionPath) => a !== null && keyOf(a) === keyOf(b);

  /** The slug a typed term would be stored as, previewed live. */
  const addSlug = $derived(slugify(addText));

  /**
   * A term whose DISPLAY TEXT is already taken, somewhere in the tree.
   *
   * Advisory, not a block. Two terms may legitimately read the same at
   * different points in a hierarchy — Georgia the country and Georgia
   * the state — and the server allows it, because uniqueness is a rule
   * about slugs. But the label is how a term is matched on the
   * extraction and open-vocabulary paths (resolveTerm, mirroring
   * metadata.indexVocabulary), where first-writer-wins: the second
   * "Egypt" would never be the one a typed value resolves to. That is
   * worth saying out loud and not worth refusing.
   */
  const addLabelClash = $derived.by(() => {
    const typed = addText.trim().toLowerCase();
    if (!typed) return undefined;
    return flattenOptions(opts).find((o) => o.label.trim().toLowerCase() === typed);
  });

  function addOption() {
    const raw = newSlug.trim();
    if (!raw) return;
    // A tree term is named, not slugged: the operator types "United
    // Kingdom" and the stored value is `united-kingdom`, mirroring what
    // the open-vocabulary mint does server-side. A flat field keeps
    // taking the slug verbatim — that is the control operators of the
    // five shipped select fields already know.
    const slug = isTree ? slugify(raw) : raw;
    if (!slug) {
      error = t('admin.fields.options_add_unslugabble');
      return;
    }
    if (takenSlugs.has(slug)) {
      error = t('admin.fields.options_duplicate', { slug });
      return;
    }
    opts = [...opts, { value: slug, label: raw === slug ? slug : raw, status: 'active' }];
    newSlug = '';
    error = '';
  }

  function setLabel(path: OptionPath, next: string) {
    opts = updateOptionAtPath(opts, path, (o) => ({ ...o, label: next }));
  }

  function move(path: OptionPath, delta: number) {
    opts = moveOptionWithinSiblings(opts, path, delta);
  }

  function setStatus(path: OptionPath, status: OptionStatus) {
    opts = updateOptionAtPath(opts, path, (o) => {
      const next = { ...o, status };
      // A successor only means anything on a retired term.
      if (status === 'active') delete next.replaced_by;
      return next;
    });
  }

  function setReplacedBy(path: OptionPath, slug: string) {
    opts = updateOptionAtPath(opts, path, (o) => {
      const next = { ...o };
      if (slug) next.replaced_by = slug;
      else delete next.replaced_by;
      return next;
    });
  }

  function beginAdd(path: OptionPath, kind: 'child' | 'sibling', anchor: string) {
    moving = null;
    addText = '';
    addError = '';
    adding =
      kind === 'child'
        ? { parentPath: path, index: Infinity, anchor, kind }
        : { parentPath: path.slice(0, -1), index: path[path.length - 1] + 1, anchor, kind };
  }

  function confirmAdd() {
    if (!adding) return;
    const raw = addText.trim();
    if (!raw) return;
    const slug = slugify(raw);
    if (!slug) {
      addError = t('admin.fields.options_add_unslugabble');
      return;
    }
    if (takenSlugs.has(slug)) {
      addError = t('admin.fields.options_duplicate', { slug });
      return;
    }
    opts = insertOptionAtPath(opts, adding.parentPath, adding.index, {
      value: slug,
      label: raw === slug ? slug : raw,
      status: 'active',
    });
    adding = null;
    addText = '';
    addError = '';
  }

  function cancelAdd() {
    adding = null;
    addText = '';
    addError = '';
  }

  function beginMove(path: OptionPath) {
    adding = null;
    moving = samePath(moving, path) ? null : path;
  }

  function moveTo(dest: OptionPath) {
    if (!moving) return;
    opts = reparentOption(opts, moving, dest);
    moving = null;
  }

  // Everything the moving node may land under, plus the top level.
  // Its own subtree is absent — that is the self-nesting guard, and
  // filtering the list is stronger than refusing the submit because
  // the operator is never shown a destination that does not work.
  const moveOptions = $derived(moving ? moveDestinations(opts, moving) : []);

  /** The subset of a stored field definition this form owns. */
  interface FieldRow {
    updated_at: string;
    label: string;
    description?: string;
    required: boolean;
    open_vocabulary?: boolean;
    show_on_card?: boolean;
    show_in_advanced_search?: boolean;
    show_on_upload?: boolean;
    edit_tab?: string | null;
    searchable?: boolean;
    read_only?: boolean;
    regexp_filter?: string | null;
    display_condition?: string[] | null;
    display_group?: string;
    display_order?: number;
    applies_to?: number[];
    options?: Record<string, unknown>;
  }

  /**
   * Take a server row as the new truth: form values AND the snapshots
   * the dirty check + the concurrency baseline are measured against.
   *
   * One function for both the post-save re-base and the conflict
   * reload, deliberately. When those were two copies, every property
   * added to the form had to be remembered in both, and forgetting one
   * leaves a control that reads "unsaved" forever or — worse — a
   * baseline that moves without the value it belongs to.
   */
  function adopt(cur: FieldRow) {
    opts = normalizeOptions(cur.options);
    label = cur.label;
    description = cur.description ?? '';
    required = cur.required;
    openVocab = cur.open_vocabulary === true;
    showOnCard = cur.show_on_card === true;
    // `!== false` and not `=== true`: absent means TODAY'S behaviour,
    // which for a participation flag is "it appears". Reading these the
    // way `show_on_card` is read would turn a server that omitted the
    // key into a form that unticks every surface.
    showInAdvancedSearch = cur.show_in_advanced_search !== false;
    showOnUpload = cur.show_on_upload !== false;
    editTab = cur.edit_tab ?? '';
    // `!== false` for the same reason the participation flags use it:
    // absent must mean the column's default, which for `searchable` is
    // TRUE. Reading it as `=== true` would untick the box on any
    // response that omitted the key.
    searchable = cur.searchable !== false;
    readOnly = cur.read_only === true;
    regexpFilter = cur.regexp_filter ?? '';
    displayCondition = displayConditionToText(cur.display_condition);
    displayGroup = cur.display_group ?? '';
    displayOrder = cur.display_order ?? 0;
    appliesTo = [...(cur.applies_to ?? [])];
    baseline = cur.updated_at;
    snapshot = JSON.stringify(serializeOptions(opts));
    labelSnapshot = label;
    descriptionSnapshot = description;
    requiredSnapshot = required;
    openVocabSnapshot = openVocab;
    showOnCardSnapshot = showOnCard;
    showInAdvancedSearchSnapshot = showInAdvancedSearch;
    showOnUploadSnapshot = showOnUpload;
    editTabSnapshot = editTab;
    searchableSnapshot = searchable;
    readOnlySnapshot = readOnly;
    regexpFilterSnapshot = regexpFilter;
    displayConditionSnapshot = displayCondition;
    displayGroupSnapshot = displayGroup;
    displayOrderSnapshot = displayOrder;
    appliesToSnapshot = JSON.stringify(appliesTo);
  }

  // Discard the local edits and adopt the server's current state.
  // Offered on a conflict as the alternative to overwriting.
  async function reloadFromServer() {
    const { data } = await api.GET('/fields/{id}', { params: { path: { id: fieldId } } });
    if (!data) return;
    const cur = data as FieldRow;
    adopt(cur);
    conflict = false;
    error = '';
    savedMsg = '';
    cancelAdd();
    moving = null;
    onSaved();
  }

  async function save() {
    if (saving) return;
    saving = true;
    error = '';
    savedMsg = '';
    conflict = false;
    try {
      const body: Record<string, unknown> = {
        if_unchanged_since: baseline,
        label: label.trim(),
        description: description.trim(),
        required,
        display_group: displayGroup.trim(),
        display_order: Number.isFinite(displayOrder) ? Math.trunc(displayOrder) : 0,
      };
      // Scoping is an ASSET-side idea. `applies_to` names resource-type
      // refs and the server ignores it for a collection field, so
      // sending one from a surface that never rendered the control
      // would be this form asserting a value nobody chose.
      if (showAppliesTo) body.applies_to = appliesTo;
      // Only vocabulary types carry a values document; sending one for
      // a number field would overwrite its min/max constraints.
      if (hasVocabulary) body.options = { values: serializeOptions(opts) };
      // Only sent where it means something. PATCH is partial, so
      // omitting the key on a `text` field leaves the column alone
      // rather than writing a false the operator never chose.
      if (canOpenVocabulary) body.open_vocabulary = openVocab;
      // Sent only where the operator could have changed it. On a gated
      // field the control is not rendered, so sending a value would be
      // this editor asserting a setting nobody chose — and the server
      // would answer 400 for a change the operator never made.
      if (!cardGated) body.show_on_card = showOnCard;
      // Participation is always sent — unlike the two above, these
      // controls are rendered for every field, so their value is always
      // one the operator could have changed.
      body.show_in_advanced_search = showInAdvancedSearch;
      body.show_on_upload = showOnUpload;
      // "No tab" is NULL server-side and a partial update cannot say
      // NULL, so an emptied box is the explicit clear rather than a
      // blank string the server would refuse.
      if (editTab.trim()) body.edit_tab = editTab.trim();
      else body.clear_edit_tab = true;
      // Always sent: the control is rendered for every field, so its
      // value is always one the operator could have changed.
      body.searchable = searchable;
      // The input rules (#1173). Sent only where the operator could have
      // changed them, for the reason show_on_card is: on a mirrored
      // field neither control is rendered, so sending a value would be
      // this editor asserting a setting nobody chose — and the server
      // answers 400 for a change the operator never made.
      if (!mirrored) body.read_only = readOnly;
      if (!mirrored && regexpSupported) {
        // Emptying the box is the CLEAR, and the clear is explicit for
        // the same reason edit_tab's is: "no pattern" is NULL
        // server-side, a partial update cannot say NULL, and `""` is
        // refused rather than stored so the state has one
        // representation. Note the exact comparison — a pattern of
        // spaces is a real pattern and must not be mistaken for empty.
        if (regexpFilter !== '') body.regexp_filter = regexpFilter;
        else body.clear_regexp_filter = true;
      }
      // `display_condition` (#1119, ADR 0099). The FOURTH property whose
      // removal has to be said out loud, after the default, the tab and
      // the pattern: NULL is "always shown" AND "leave it alone", and the
      // server refuses `[]` as a second spelling of unset.
      //
      // Sent for every non-mirrored field, because the control is
      // rendered for every non-mirrored field. A mirrored definition
      // cannot carry one at all (its column has a second write plane), so
      // sending a value there would be this editor asserting a setting
      // nobody chose and would earn a 400 for a change the operator never
      // made — the same reasoning `read_only` and `show_on_card` use.
      if (!mirrored) {
        const terms = displayConditionTerms(displayCondition);
        if (terms.length > 0) body.display_condition = terms;
        else body.clear_display_condition = true;
      }
      const { data, error: apiErr, response } = await api.PATCH('/fields/{id}', {
        params: { path: { id: fieldId } },
        body: body as never,
      });
      if (response?.status === 409) {
        // Visible, not silent. The operator's edits stay in the form;
        // acknowledging re-baselines so a deliberate retry overwrites
        // on purpose rather than by accident.
        const c = apiErr as { updated_at?: string } | undefined;
        if (c?.updated_at) baseline = c.updated_at;
        conflict = true;
        return;
      }
      if (apiErr || !data) {
        // The server's own words when it has any. NormalizeOptionsDoc
        // rejects with a message that names the offending term
        // ("duplicate option value \"gb\"") and swallowing it for a
        // house string would leave the operator hunting a term the
        // server already identified.
        error = (apiErr as { error?: string } | undefined)?.error ?? t('admin.fields.options_save_error');
        return;
      }
      adopt(data as FieldRow);
      savedMsg = t('admin.fields.options_saved');
      cancelAdd();
      moving = null;
      onSaved();
    } finally {
      saving = false;
    }
  }
</script>

<!--
  One row, at any depth. Recursive: a term's children render through
  this same snippet, so a leaf gets the identical controls its
  grandparent has.
-->
{#snippet optionRow(o: FieldOption, path: OptionPath)}
  {@const siblings = childrenAtPath(opts, path.slice(0, -1))}
  {@const i = path[path.length - 1]}
  <li
    class="min-w-0 rounded border border-border bg-surface p-2"
    class:opacity-70={o.status !== 'active'}
    data-testid="field-option-row-{o.value}"
  >
    <div class="flex flex-wrap items-end gap-2">
      <span class="w-full font-mono text-xs text-fg-muted sm:w-auto">{o.value}</span>

      <label class="w-full min-w-0 sm:flex-1">
        <span class="block text-xs text-fg-muted">{t('admin.fields.options_label')}</span>
        <input
          type="text"
          value={o.label}
          oninput={(e) => setLabel(path, (e.currentTarget as HTMLInputElement).value)}
          data-testid="field-option-label-{o.value}"
          class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        />
      </label>

      <label class="min-w-0 flex-1 sm:flex-none">
        <span class="block text-xs text-fg-muted">{t('admin.fields.options_status')}</span>
        <select
          value={o.status}
          onchange={(e) => setStatus(path, (e.currentTarget as HTMLSelectElement).value as OptionStatus)}
          data-testid="field-option-status-{o.value}"
          class="mt-0.5 w-full max-w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none sm:w-auto"
        >
          {#each STATUSES as s (s)}
            <option value={s}>{t(`admin.fields.options_status_${s}`)}</option>
          {/each}
        </select>
      </label>

      {#if o.status !== 'active'}
        <label class="min-w-0 flex-1 sm:flex-none">
          <span class="block text-xs text-fg-muted">{t('admin.fields.options_replaced_by')}</span>
          <select
            value={o.replaced_by ?? ''}
            onchange={(e) => setReplacedBy(path, (e.currentTarget as HTMLSelectElement).value)}
            data-testid="field-option-replaced-by-{o.value}"
            class="mt-0.5 w-full max-w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none sm:w-auto"
          >
            <option value="">{t('admin.fields.options_replaced_by_none')}</option>
            {#each successorChoices as c (c.value)}
              {#if c.value !== o.value}
                <option value={c.value}>{c.path.join(' / ')}</option>
              {/if}
            {/each}
          </select>
        </label>
      {/if}

      <div class="flex flex-wrap gap-1">
        <button
          type="button"
          onclick={() => move(path, -1)}
          disabled={i === 0}
          aria-label={t('admin.fields.options_move_up')}
          data-testid="field-option-up-{o.value}"
          class="min-h-11 min-w-11 rounded border border-border px-2 text-fg-muted hover:bg-state-hover disabled:opacity-30"
        >↑</button>
        <button
          type="button"
          onclick={() => move(path, 1)}
          disabled={i === siblings.length - 1}
          aria-label={t('admin.fields.options_move_down')}
          data-testid="field-option-down-{o.value}"
          class="min-h-11 min-w-11 rounded border border-border px-2 text-fg-muted hover:bg-state-hover disabled:opacity-30"
        >↓</button>
        {#if isTree}
          <button
            type="button"
            onclick={() => beginAdd(path, 'child', o.label)}
            aria-label={t('admin.fields.options_add_child_aria', { label: o.label })}
            data-testid="field-option-add-child-{o.value}"
            class="min-h-11 min-w-11 rounded border border-border px-2 text-xs text-fg-muted hover:bg-state-hover"
          >{t('admin.fields.options_add_child')}</button>
          <button
            type="button"
            onclick={() => beginAdd(path, 'sibling', o.label)}
            aria-label={t('admin.fields.options_add_sibling_aria', { label: o.label })}
            data-testid="field-option-add-sibling-{o.value}"
            class="min-h-11 min-w-11 rounded border border-border px-2 text-xs text-fg-muted hover:bg-state-hover"
          >{t('admin.fields.options_add_sibling')}</button>
          <button
            type="button"
            onclick={() => beginMove(path)}
            aria-label={t('admin.fields.options_move_aria', { label: o.label })}
            aria-expanded={samePath(moving, path)}
            data-testid="field-option-move-{o.value}"
            class="min-h-11 min-w-11 rounded border border-border px-2 text-xs text-fg-muted hover:bg-state-hover"
          >{t('admin.fields.options_move')}</button>
        {/if}
      </div>
    </div>

    {#if samePath(moving, path)}
      <!--
        Destination picker. A flat, indented list of every term the
        node may sit under — its own subtree is not in it, so the
        move that would splice the subtree into itself cannot be
        chosen. Buttons, not a drag: this has to work with a thumb.
      -->
      <div
        class="mt-2 rounded border border-border-strong bg-bg-soft p-2"
        data-testid="field-option-move-picker"
      >
        <p class="text-xs text-fg-muted">
          {t('admin.fields.options_move_heading', { label: o.label })}
        </p>
        <div class="mt-1 flex flex-col gap-1">
          <button
            type="button"
            onclick={() => moveTo([])}
            data-testid="field-option-move-dest-root"
            class="min-h-11 rounded border border-border bg-surface px-2 py-1 text-left text-sm hover:bg-state-hover"
          >{t('admin.fields.options_move_root')}</button>
          {#each moveOptions as d (d.value)}
            <button
              type="button"
              onclick={() => moveTo(d.indexPath)}
              data-testid="field-option-move-dest-{d.value}"
              style="padding-left: {0.5 + d.depth * 0.75}rem"
              class="min-h-11 rounded border border-border bg-surface py-1 pr-2 text-left text-sm hover:bg-state-hover"
            >{d.label}</button>
          {/each}
        </div>
        <button
          type="button"
          onclick={() => (moving = null)}
          data-testid="field-option-move-cancel"
          class="mt-1 min-h-11 rounded border border-border bg-surface px-2 py-1 text-sm"
        >{t('common.cancel')}</button>
      </div>
    {/if}

    {#if o.children?.length || (adding?.kind === 'child' && keyOf(adding.parentPath) === keyOf(path))}
      <ul class="mt-2 w-full space-y-2 border-l border-border pl-3">
        {#each o.children ?? [] as c, j (c.value)}
          {@render optionRow(c, [...path, j])}
          {@render siblingSlot(path, j + 1)}
        {/each}
        {#if adding?.kind === 'child' && keyOf(adding.parentPath) === keyOf(path)}
          <li>{@render addForm()}</li>
        {/if}
      </ul>
    {/if}
  </li>
{/snippet}

<!--
  The gap between two siblings, where a "+ beside" form appears. It has
  to render in the PARENT's list rather than inside the anchor row: a
  new sibling drawn inside the box of the term it sits next to reads as
  a child, and the operator finds out it was not one only after saving.
-->
{#snippet siblingSlot(parentPath: OptionPath, index: number)}
  {#if adding?.kind === 'sibling' && keyOf(adding.parentPath) === keyOf(parentPath) && adding.index === index}
    <li>{@render addForm()}</li>
  {/if}
{/snippet}

<!--
  The inline new-term form. One instance, rendered wherever it is
  anchored: a per-node form would be twenty-nine hidden inputs on
  `country` and twenty-nine ways to leave one half-filled.
-->
{#snippet addForm()}
  <div class="mt-2 rounded border border-border-strong bg-bg-soft p-2" data-testid="field-option-inline-add">
    <label class="block">
      <span class="block text-xs text-fg-muted">
        {adding?.kind === 'child'
          ? t('admin.fields.options_add_child_label', { label: adding?.anchor ?? '' })
          : t('admin.fields.options_add_sibling_label', { label: adding?.anchor ?? '' })}
      </span>
      <!-- svelte-ignore a11y_autofocus -->
      <input
        type="text"
        bind:value={addText}
        autofocus
        oninput={() => (addError = '')}
        onkeydown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault();
            confirmAdd();
          }
          if (e.key === 'Escape') cancelAdd();
        }}
        placeholder={t('admin.fields.options_add_term_placeholder')}
        data-testid="field-option-inline-add-input"
        class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>
    {#if addSlug}
      <p class="mt-1 font-mono text-xs text-fg-muted" data-testid="field-option-inline-add-slug">
        {t('admin.fields.options_add_slug_preview', { slug: addSlug })}
      </p>
    {/if}
    {#if addLabelClash && addLabelClash.value !== addSlug}
      <p class="mt-1 text-xs text-warning" data-testid="field-option-inline-add-warning">
        {t('admin.fields.options_add_label_taken', {
          label: addLabelClash.label,
          slug: addLabelClash.value,
        })}
      </p>
    {/if}
    {#if addError}
      <p role="alert" class="mt-1 text-xs text-danger" data-testid="field-option-inline-add-error">
        {addError}
      </p>
    {/if}
    <div class="mt-1 flex flex-wrap gap-2">
      <button
        type="button"
        onclick={confirmAdd}
        disabled={!addText.trim()}
        data-testid="field-option-inline-add-confirm"
        class="min-h-11 rounded border border-border bg-surface px-3 py-1.5 text-sm disabled:opacity-40"
      >{t('admin.fields.options_add')}</button>
      <button
        type="button"
        onclick={cancelAdd}
        data-testid="field-option-inline-add-cancel"
        class="min-h-11 rounded border border-border bg-surface px-3 py-1.5 text-sm"
      >{t('common.cancel')}</button>
    </div>
  </div>
{/snippet}

<div class="min-w-0 space-y-4 text-sm" data-testid="field-editor">
  <section class="min-w-0 space-y-3 rounded border border-border bg-bg-soft p-3">
    <h3 class="text-sm font-semibold">{t('admin.fields.section_basics')}</h3>
  <div class="flex flex-wrap items-end gap-3">
    <label class="w-full min-w-0 sm:flex-1">
      <span class="block text-xs text-fg-muted">{t('admin.fields.label')}</span>
      <input
        type="text"
        bind:value={label}
        data-testid="field-edit-label"
        class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
      />
    </label>
    <label class="flex min-h-11 items-center gap-2 text-sm">
      <input
        type="checkbox"
        bind:checked={required}
        data-testid="field-edit-required"
        class="h-4 w-4 rounded border-border-strong"
      />
      <span>{t('admin.fields.edit_required')}</span>
    </label>
  </div>

  <label class="block">
    <span class="block text-xs text-fg-muted">{t('admin.fields.description')}</span>
    <textarea
      bind:value={description}
      rows="2"
      data-testid="field-edit-description"
      class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
    ></textarea>
    <span class="mt-0.5 block text-xs text-fg-muted">{t('admin.fields.description_help')}</span>
  </label>

  {#if cardGated}
    <p class="text-xs text-fg-muted" data-testid="field-edit-show-on-card-gated">
      {t('admin.fields.show_on_card_gated')}
    </p>
  {:else}
    <label class="flex min-h-11 items-start gap-2 text-sm">
      <input
        type="checkbox"
        bind:checked={showOnCard}
        data-testid="field-edit-show-on-card"
        class="mt-0.5 h-4 w-4 rounded border-border-strong"
      />
      <span class="min-w-0">
        <span class="block">{t('admin.fields.show_on_card')}</span>
        <span class="block text-xs text-fg-muted">{t('admin.fields.show_on_card_help')}</span>
      </span>
    </label>
  {/if}

  {#if canOpenVocabulary}
    <label class="flex min-h-11 items-start gap-2 text-sm">
      <input
        type="checkbox"
        bind:checked={openVocab}
        data-testid="field-edit-open-vocabulary"
        class="mt-0.5 h-4 w-4 rounded border-border-strong"
      />
      <span class="min-w-0">
        <span class="block">{t('admin.fields.open_vocabulary')}</span>
        <span class="block text-xs text-fg-muted">{t('admin.fields.open_vocabulary_help')}</span>
      </span>
    </label>
  {/if}
  </section>

  <!--
    WHAT A VALUE MUST LOOK LIKE, AND WHO MAY WRITE ONE (#1173).

    Two settings that did not exist until sprint 19, so an operator had
    no way to say "extraction owns this field" or "a shot code looks
    like AAA_0010". Both are about PEOPLE: the upload defaults and the
    extraction pipeline keep writing either way, which is what makes
    read_only useful rather than a freeze.

    Its own section rather than a line under Basics, because the
    question it answers is a different one from what the field is
    called.
  -->
  <section class="min-w-0 space-y-3 rounded border border-border bg-bg-soft p-3" data-testid="field-edit-input-rules">
    <h3 class="text-sm font-semibold">{t('admin.fields.section_input_rules')}</h3>

    {#if mirrored}
      <!--
        Replaced by the reason, not shown disabled. See the `mirrored`
        note in the script: the asset row has a second human write plane
        that would not obey either setting, so the server refuses both.
      -->
      <p class="text-xs text-fg-muted" data-testid="field-edit-input-rules-mirrored">
        {t('admin.fields.input_rules_mirrored', { column: initialMirrorsColumn ?? '' })}
      </p>
    {:else}
      <label class="flex min-h-11 items-start gap-2 text-sm">
        <input
          type="checkbox"
          bind:checked={readOnly}
          data-testid="field-edit-read-only"
          class="mt-0.5 h-4 w-4 rounded border-border-strong"
        />
        <span class="min-w-0">
          <span class="block">{t('admin.fields.read_only')}</span>
          <span class="block text-xs text-fg-muted">{t('admin.fields.read_only_help')}</span>
        </span>
      </label>

      {#if regexpSupported}
        <label class="block">
          <span class="block text-xs text-fg-muted">{t('admin.fields.regexp_filter')}</span>
          <input
            type="text"
            bind:value={regexpFilter}
            spellcheck="false"
            autocapitalize="off"
            autocorrect="off"
            placeholder={t('admin.fields.regexp_filter_placeholder')}
            data-testid="field-edit-regexp-filter"
            class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 font-mono text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          />
          <span class="mt-0.5 block text-xs text-fg-muted">{t('admin.fields.regexp_filter_help')}</span>
        </label>
      {:else}
        <p class="text-xs text-fg-muted" data-testid="field-edit-regexp-filter-type-note">
          {t('admin.fields.regexp_filter_type_note', { type: fieldType })}
        </p>
      {/if}
    {/if}
  </section>

  <!--
    WHERE THIS FIELD APPEARS (#1173, ADR 0092 §3).

    Its own section rather than a line in the collapsed "Advanced"
    block, and that is a deliberate reversal of where `display_group`
    and `display_order` sit. Those are layout: an operator can ignore
    them and get a plainer page. Participation is the answer to "why is
    this field not on the search form" — the question an operator with
    200 fields opens this page to settle — so burying it behind a
    disclosure would hide the control that the flags exist to give them.

    Both toggles render for EVERY field, including types the advanced
    page has no control for yet. A field of type `text` marked for
    advanced search does not appear there today, because that page can
    only draw a picker for a vocabulary — the help text below says which
    surfaces read the flag so far, rather than the form quietly
    withholding the toggle and leaving the operator to guess.
  -->
  <section class="min-w-0 space-y-3 rounded border border-border bg-bg-soft p-3" data-testid="field-edit-participation">
    <h3 class="text-sm font-semibold">{t('admin.fields.section_participation')}</h3>
    <p class="text-xs text-fg-muted">{t('admin.fields.participation_help')}</p>

    <label class="flex min-h-11 items-start gap-2 text-sm">
      <input
        type="checkbox"
        bind:checked={showInAdvancedSearch}
        data-testid="field-edit-show-in-advanced-search"
        class="mt-0.5 h-4 w-4 rounded border-border-strong"
      />
      <span class="min-w-0">
        <span class="block">{t('admin.fields.show_in_advanced_search')}</span>
        <span class="block text-xs text-fg-muted">{t('admin.fields.show_in_advanced_search_help')}</span>
      </span>
    </label>

    <label class="flex min-h-11 items-start gap-2 text-sm">
      <input
        type="checkbox"
        bind:checked={showOnUpload}
        data-testid="field-edit-show-on-upload"
        class="mt-0.5 h-4 w-4 rounded border-border-strong"
      />
      <span class="min-w-0">
        <span class="block">{t('admin.fields.show_on_upload')}</span>
        <span class="block text-xs text-fg-muted">{t('admin.fields.show_on_upload_help')}</span>
      </span>
    </label>

    <label class="block">
      <span class="block text-xs text-fg-muted">{t('admin.fields.edit_tab')}</span>
      <input
        type="text"
        bind:value={editTab}
        placeholder={t('admin.fields.edit_tab_placeholder')}
        data-testid="field-edit-edit-tab"
        class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none sm:w-64"
      />
      <span class="mt-0.5 block text-xs text-fg-muted">{t('admin.fields.edit_tab_help')}</span>
    </label>

    <!--
      `display_condition` (#1119, ADR 0099). It belongs in "where this
      field appears" beside the tab, because it is the same class of
      setting: both decide COMPOSITION and neither decides access.

      Not offered on a MIRRORED definition, matching `read_only` and
      `regexp_filter` above and for the same reason: a mirrored field is a
      view onto a column of the asset with its own write plane and its own
      first-class control, so only one of the two planes would obey the
      setting. The server refuses it too; rendering no control is what
      stops an operator being refused after typing.

      A textarea rather than a term builder. A builder would need the
      whole field list, the operator matrix and the type of every
      candidate controller to draw its dropdowns, and would still have to
      fall back to free text for the value — while the server already
      answers every malformed configuration with a sentence naming what is
      wrong, which is surfaced verbatim on save.
    -->
    {#if !mirrored}
      <label class="block">
        <span class="block text-xs text-fg-muted">{t('admin.fields.display_condition')}</span>
        <textarea
          bind:value={displayCondition}
          rows="3"
          spellcheck="false"
          placeholder={t('admin.fields.display_condition_placeholder')}
          data-testid="field-edit-display-condition"
          class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 font-mono text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        ></textarea>
        <span class="mt-0.5 block text-xs text-fg-muted">
          {#if displayConditionTerms(displayCondition).length === 0}
            <span data-testid="field-edit-display-condition-none"
              >{t('admin.fields.display_condition_none')}</span
            >
          {/if}
          {t('admin.fields.display_condition_help')}
        </span>
      </label>
    {/if}
  </section>

  <!--
    THE SEARCH INDEX (#1173).

    `searchable` has existed since the 00001 baseline, persists through
    both the create and the update API, and had NO control on any admin
    surface — so the one setting that decides whether a field's text is
    findable at all could only be reached with a hand-written PATCH.

    ITS OWN SECTION, deliberately not a fourth line inside "Where this
    field appears". Sprint 18d had to unpick exactly that conflation:
    `searchable` governs the FULL-TEXT INDEX, `show_in_advanced_search`
    governs a CONTROL, and an explicit `field:` filter obeys neither.
    Putting them under one heading is how they get read as three
    settings of one thing again, so the boundary is drawn in the layout
    as well as in the copy.
  -->
  <section class="min-w-0 space-y-3 rounded border border-border bg-bg-soft p-3" data-testid="field-edit-search-index">
    <h3 class="text-sm font-semibold">{t('admin.fields.section_search_index')}</h3>

    <label class="flex min-h-11 items-start gap-2 text-sm">
      <input
        type="checkbox"
        bind:checked={searchable}
        data-testid="field-edit-searchable"
        class="mt-0.5 h-4 w-4 rounded border-border-strong"
      />
      <span class="min-w-0">
        <span class="block">{t('admin.fields.searchable')}</span>
        <span class="block text-xs text-fg-muted">{t('admin.fields.searchable_help')}</span>
      </span>
    </label>

    <p class="text-xs text-fg-muted" data-testid="field-edit-searchable-boundary">
      {t('admin.fields.searchable_boundary')}
    </p>
  </section>

  <section class="min-w-0 space-y-3 rounded border border-border bg-bg-soft p-3">
    <h3 class="text-sm font-semibold">{t('admin.fields.section_vocabulary')}</h3>
  {#if hasVocabulary}
    <p class="text-xs text-fg-muted">{t('admin.fields.options_help')}</p>
    {#if isTree}
      <p class="text-xs text-fg-muted" data-testid="field-options-tree-help">
        {t('admin.fields.options_tree_help')}
      </p>
    {/if}

    {#if opts.length === 0}
      <p class="text-xs text-fg-muted" data-testid="field-options-empty">
        {t('admin.fields.options_none')}
      </p>
    {/if}

    <ul class="space-y-2">
      {#each opts as o, i (o.value)}
        {@render optionRow(o, [i])}
        {@render siblingSlot([], i + 1)}
      {/each}
    </ul>

    <div class="flex flex-wrap items-end gap-2">
      <label class="w-full min-w-0 sm:flex-1">
        <span class="block text-xs text-fg-muted">{t('admin.fields.options_add_label')}</span>
        <input
          type="text"
          bind:value={newSlug}
          onkeydown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              addOption();
            }
          }}
          placeholder={isTree
            ? t('admin.fields.options_add_term_placeholder')
            : t('admin.fields.options_add_placeholder')}
          data-testid="field-option-new"
          class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 font-mono text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        />
      </label>
      <button
        type="button"
        onclick={addOption}
        disabled={!newSlug.trim()}
        data-testid="field-option-add"
        class="min-h-11 rounded border border-border bg-surface px-3 py-1.5 text-sm disabled:opacity-40"
      >{t('admin.fields.options_add')}</button>
    </div>
  {:else}
  <p class="text-xs text-fg-muted" data-testid="field-options-type-note">
    {t('admin.fields.options_type_note', { type: fieldType })}
  </p>
  {/if}
  </section>

  <!--
    The long tail (#854). A <details> rather than a bespoke toggle: it
    is keyboard- and screen-reader-addressable for free, and it stays
    OPEN across a save because nothing here re-mounts the editor.
  -->
  <details
    bind:open={advancedOpen}
    class="min-w-0 rounded border border-border bg-bg-soft"
    data-testid="field-edit-advanced"
  >
    <summary
      class="flex min-h-11 cursor-pointer items-center px-3 py-2 text-sm font-semibold"
      data-testid="field-edit-advanced-toggle"
    >{t('admin.fields.section_advanced')}</summary>
    <div class="space-y-3 border-t border-border px-3 pb-3 pt-3">
      <p class="text-xs text-fg-muted">{t('admin.fields.advanced_help')}</p>

      <div class="flex flex-wrap items-end gap-3">
        <label class="w-full min-w-0 sm:flex-1">
          <span class="block text-xs text-fg-muted">{t('admin.fields.group')}</span>
          <input
            type="text"
            bind:value={displayGroup}
            placeholder={t('admin.fields.group_placeholder')}
            data-testid="field-edit-display-group"
            class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          />
        </label>
        <label class="w-full min-w-0 sm:w-40">
          <span class="block text-xs text-fg-muted">{t('admin.fields.display_order')}</span>
          <input
            type="number"
            step="1"
            bind:value={displayOrder}
            data-testid="field-edit-display-order"
            class="mt-0.5 w-full rounded border border-border-strong bg-surface px-2 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          />
        </label>
      </div>
      <p class="text-xs text-fg-muted">{t('admin.fields.display_help')}</p>

      {#if showAppliesTo}
        <fieldset data-testid="field-edit-applies-to">
          <legend class="text-xs text-fg-muted">{t('admin.fields.applies_to')}</legend>
          <p class="mt-1 text-xs text-fg-muted">
            {appliesToAll ? t('admin.fields.applies_to_all') : t('admin.fields.applies_to_help')}
          </p>
          {#if assetTypes.length === 0}
            <p class="mt-1 text-xs text-fg-muted" data-testid="field-edit-applies-to-empty">
              {t('admin.fields.applies_to_none')}
            </p>
          {:else}
            <div class="mt-2 flex flex-col gap-1">
              {#each assetTypes as ty (ty.ref)}
                <label class="flex min-h-11 items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={appliesTo.includes(ty.ref)}
                    onchange={(e) => toggleAppliesTo(ty.ref, (e.currentTarget as HTMLInputElement).checked)}
                    data-testid="field-edit-applies-to-{ty.ref}"
                    class="h-4 w-4 rounded border-border-strong"
                  />
                  <span class="min-w-0">{ty.name ?? `#${ty.ref}`}</span>
                </label>
              {/each}
            </div>
          {/if}
        </fieldset>
      {/if}

      <!--
        Capabilities are SHOWN and not edited here (#854). Two reasons,
        and neither is difficulty: a read capability decides who can see
        a field's values, so retyping one is an authorisation change
        that belongs with the access work rather than inside a layout
        sprint; and this form already refuses to send `show_on_card`
        when a read capability is present, a rule that would have to be
        re-derived live if the capability could change under it.
      -->
      <dl class="grid gap-1 text-xs sm:grid-cols-[10rem_1fr]" data-testid="field-edit-capabilities">
        <dt class="text-fg-muted">{t('admin.fields.read_capability')}</dt>
        <dd class="font-mono break-all" data-testid="field-edit-read-capability">
          {(initialReadCapability ?? '').trim() || t('admin.fields.capability_none')}
        </dd>
        <dt class="text-fg-muted">{t('admin.fields.write_capability')}</dt>
        <dd class="font-mono break-all" data-testid="field-edit-write-capability">
          {(initialWriteCapability ?? '').trim() || t('admin.fields.capability_none')}
        </dd>
      </dl>
      <p class="text-xs text-fg-muted">{t('admin.fields.capability_readonly_help')}</p>
    </div>
  </details>

  {#if conflict}
    <div
      role="alert"
      data-testid="field-options-conflict"
      class="space-y-2 rounded border border-warning/40 bg-warning-container px-3 py-2 text-sm text-warning"
    >
      <p class="font-medium">{t('admin.fields.options_conflict_heading')}</p>
      <p>{t('admin.fields.options_conflict_body')}</p>
      <div class="flex flex-wrap gap-2">
        <button
          type="button"
          onclick={() => void reloadFromServer()}
          data-testid="field-options-conflict-reload"
          class="min-h-11 rounded border border-border bg-surface px-3 py-1.5 text-sm"
        >{t('admin.fields.options_conflict_reload')}</button>
        <button
          type="button"
          onclick={() => { conflict = false; void save(); }}
          data-testid="field-options-conflict-overwrite"
          class="min-h-11 rounded border border-border bg-surface px-3 py-1.5 text-sm"
        >{t('admin.fields.options_conflict_overwrite')}</button>
      </div>
    </div>
  {/if}

  {#if error}
    <p role="alert" data-testid="field-options-error" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">{error}</p>
  {/if}
  {#if savedMsg}
    <p data-testid="field-options-saved" class="text-sm text-success">{savedMsg}</p>
  {/if}

  <button
    type="button"
    onclick={save}
    disabled={saving || !dirty}
    data-testid="field-options-save"
    class="min-h-11 rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
  >{saving ? t('common.loading') : t('admin.fields.options_save')}</button>
</div>
