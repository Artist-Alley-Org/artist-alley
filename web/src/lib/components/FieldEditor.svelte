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
    initialRequired,
    initialOptions,
    initialOpenVocabulary = false,
    initialShowOnCard = false,
    initialReadCapability = null,
    initialUpdatedAt,
    onSaved = () => {},
  }: {
    fieldId: string;
    fieldType: string;
    initialLabel: string;
    initialRequired: boolean;
    initialOptions: Record<string, unknown> | undefined;
    initialOpenVocabulary?: boolean;
    initialShowOnCard?: boolean;
    /** Present only so the editor can explain WHY the card toggle is
     *  unavailable. The server refuses the combination either way. */
    initialReadCapability?: string | null;
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

  let label = $state(initialLabel);
  let required = $state(initialRequired);
  let openVocab = $state(initialOpenVocabulary);
  let showOnCard = $state(initialShowOnCard);

  // A gated field cannot be an at-a-glance field (#552): a card renders on
  // browse, for a page of assets, where the server has evaluated no
  // per-field capability. The server refuses the combination with a 400, so
  // the control is REPLACED by the reason rather than shown disabled — a
  // disabled checkbox advertises a setting the operator can never reach
  // here, which is the same complaint the open_vocabulary note above makes.
  const cardGated = $derived(!!(initialReadCapability ?? '').trim());
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
  let requiredSnapshot = $state(initialRequired);
  let openVocabSnapshot = $state(initialOpenVocabulary);
  let showOnCardSnapshot = $state(initialShowOnCard);
  const dirty = $derived(
    JSON.stringify(serializeOptions(opts)) !== snapshot ||
      label !== labelSnapshot ||
      required !== requiredSnapshot ||
      openVocab !== openVocabSnapshot ||
      showOnCard !== showOnCardSnapshot,
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

  // Discard the local edits and adopt the server's current state.
  // Offered on a conflict as the alternative to overwriting.
  async function reloadFromServer() {
    const { data } = await api.GET('/fields/{id}', { params: { path: { id: fieldId } } });
    if (!data) return;
    const cur = data as {
      updated_at: string;
      label: string;
      required: boolean;
      open_vocabulary?: boolean;
      show_on_card?: boolean;
      options?: Record<string, unknown>;
    };
    opts = normalizeOptions(cur.options);
    label = cur.label;
    required = cur.required;
    openVocab = cur.open_vocabulary === true;
    showOnCard = cur.show_on_card === true;
    baseline = cur.updated_at;
    snapshot = JSON.stringify(serializeOptions(opts));
    labelSnapshot = cur.label;
    requiredSnapshot = cur.required;
    openVocabSnapshot = openVocab;
    showOnCardSnapshot = showOnCard;
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
        required,
      };
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
      const saved = data as {
        updated_at: string;
        label: string;
        required: boolean;
        open_vocabulary?: boolean;
        show_on_card?: boolean;
        options?: Record<string, unknown>;
      };
      baseline = saved.updated_at;
      opts = normalizeOptions(saved.options);
      label = saved.label;
      required = saved.required;
      openVocab = saved.open_vocabulary === true;
      showOnCard = saved.show_on_card === true;
      snapshot = JSON.stringify(serializeOptions(opts));
      labelSnapshot = saved.label;
      requiredSnapshot = saved.required;
      openVocabSnapshot = openVocab;
      showOnCardSnapshot = showOnCard;
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

<div
  class="min-w-0 space-y-3 rounded border border-border bg-bg-soft p-3 text-sm"
  data-testid="field-editor"
>
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
