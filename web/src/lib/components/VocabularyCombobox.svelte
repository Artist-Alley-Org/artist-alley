<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<!--
  The entry control for a multi-value controlled vocabulary: chips for
  what is chosen, a text box that filters the terms on offer, and — on
  an OPEN vocabulary (#830/#846) — an explicit row that creates the
  term the operator typed.

  # Why this exists

  #846 taught the API to mint terms on an open field, and nothing could
  send it one. The upload row rendered a multi_select as a checkbox list
  over the terms that already existed and the collection editor used a
  native `<select multiple>`; both can only ever re-send a term the
  field already had. A `keywords` field that grows from the material was
  reachable exclusively through the admin options editor, one term at a
  time.

  # Standalone on purpose

  Two surfaces mount it today (the upload row and the collection field
  editor) and a third — the asset metadata panel, #549 — will. Writing
  it into either caller would have meant writing it twice, and the two
  copies would answer "does this term already exist" differently, which
  is the whole question this control exists to answer.

  # Creating is never silent

  An unmatched term does NOT become a chip because the operator pressed
  Enter near it. It becomes a chip because they picked a row that says,
  in words, that it will create the term — and shows the slug it will
  be stored as. A vocabulary that grows by accident is the duplicate
  mess ("sunset", "Sunset", "sunsets") an open vocabulary has to avoid
  to be worth having, and a typo is exactly what silent minting
  immortalises.

  # Matching mirrors the server

  resolveTerm ($lib/fieldOptions) is the browser's copy of
  indexVocabulary + resolveOrMint. Typing "LANDSCAPE" where `landscape`
  exists offers the existing term and shows NO create row, because that
  is what the server would do with it. A preview that disagrees with the
  write is worse than no preview.

  # What is emitted

  A chip for an EXISTING term emits its slug. A chip for a term being
  created emits the operator's TEXT, unchanged — the server mints
  `{value: slugify(text), label: text}`, so sending the slug instead
  would store a term labelled `macro-detail` where "Macro Detail"
  belongs. Callers pass the array straight through to `value_options`.
-->
<script lang="ts">
  import { t } from '$stores/lang.svelte';
  import {
    findOption,
    resolveTerm,
    selectableOptions,
    slugify,
    type FieldOption,
  } from '$lib/fieldOptions';

  interface Props {
    /** The field's whole vocabulary, normalised. */
    options: FieldOption[];
    /**
     * What is chosen. Slugs for existing terms; raw text for a term
     * this control is about to create (see the header note).
     */
    value: string[];
    /** The field's `open_vocabulary` flag. False = no create row, ever. */
    open?: boolean;
    disabled?: boolean;
    /** Accessible name. One of these is required for the input. */
    label?: string;
    labelledBy?: string;
    placeholder?: string;
    /** Suffix for the data-testid hooks, normally the field code. */
    testid?: string;
    onchange: (value: string[]) => void;
  }

  let {
    options,
    value,
    open = false,
    disabled = false,
    label,
    labelledBy,
    placeholder,
    testid = 'field',
    onchange,
  }: Props = $props();

  // A long vocabulary is a scroll trap, not a picker. Cap the list and
  // say so, rather than rendering nine hundred rows a phone has to
  // paint.
  const MAX_ROWS = 50;

  let draft = $state('');
  let listOpen = $state(false);
  let highlight = $state(-1);
  let inputEl = $state<HTMLInputElement | null>(null);
  let rootEl = $state<HTMLDivElement | null>(null);

  const uid = $props.id();
  const listId = `vocab-list-${uid}`;
  const rowId = (i: number) => `vocab-row-${uid}-${i}`;

  // Lifecycle: a deprecated term stops being OFFERED but a value
  // already holding it stays legible and re-savable. Same rule the
  // native pickers this replaces used.
  const offerable = $derived(selectableOptions(options, value));

  const chosen = $derived(new Set(value));

  /** How a chosen entry reads. Falls back to the entry itself, which
      is exactly right for a term being created — there, the text IS
      the label. */
  function chipLabel(entry: string): string {
    const opt = findOption(options, entry);
    if (!opt) return entry;
    return opt.status === 'active' ? opt.label : t('common.option_deprecated', { label: opt.label });
  }

  /** True for an entry that has no term behind it yet. */
  function isNew(entry: string): boolean {
    return !findOption(options, entry);
  }

  const query = $derived(draft.trim().toLowerCase());

  /**
   * Every term the typed prefix admits, best match first, minus what
   * is already chosen.
   *
   * Ranked rather than merely filtered: an exact hit belongs above a
   * term that merely contains the text, so "type it and press Enter"
   * lands on the term the operator meant. Substring matching is
   * deliberately looser than the server's exact rule — this is the
   * BROWSE list. Whether a term matches for the purpose of WRITING is
   * resolveTerm's question, below, and it is the strict one.
   */
  const ranked = $derived.by(() => {
    const pool = offerable.filter((o) => !chosen.has(o.value));
    if (!query) return pool;
    const rank = (o: FieldOption): number => {
      const v = o.value.toLowerCase();
      const l = o.label.toLowerCase();
      if (v === query || l === query) return 0;
      if (v.startsWith(query) || l.startsWith(query)) return 1;
      if (v.includes(query) || l.includes(query)) return 2;
      return 3;
    };
    return pool
      .map((o, i) => ({ o, r: rank(o), i }))
      .filter((x) => x.r < 3)
      .sort((a, b) => a.r - b.r || a.i - b.i)
      .map((x) => x.o);
  });

  const matches = $derived(ranked.slice(0, MAX_ROWS));
  const truncated = $derived(ranked.length > MAX_ROWS);

  /**
   * What the typed term resolves to. Drives the three tails the
   * dropdown can have: nothing (it matched an offered term, which is
   * already in the list), a create row, or a refusal note for a term
   * that exists but may not be chosen.
   */
  const resolution = $derived(draft.trim() ? resolveTerm(options, draft) : null);

  /** The create row, or null. Absent on a closed field, always. */
  const creatable = $derived.by(() => {
    if (!open || !resolution) return null;
    if (resolution.matched) return null;
    if (!resolution.slug) return null; // no addressable form ("!!!")
    if (chosen.has(draft.trim())) return null;
    return { term: draft.trim(), slug: resolution.slug };
  });

  /**
   * A term that exists but cannot be picked: retired, or — on a closed
   * field — not a term at all. Said out loud rather than shown as an
   * empty list, because "no matches" and "that term is retired" are
   * different problems with different fixes.
   */
  const blocked = $derived.by(() => {
    if (!resolution) return null;
    if (resolution.matched) {
      const opt = resolution.option;
      if (!opt || opt.status === 'active') return null;
      if (chosen.has(opt.value)) return null;
      return t('vocabulary.term_retired', { label: opt.label });
    }
    if (open) return resolution.slug ? null : t('vocabulary.term_unslugifiable');
    return t('vocabulary.term_unknown', { term: draft.trim() });
  });

  /** Rows the keyboard walks: the matches, then the create row. */
  const rowCount = $derived(matches.length + (creatable ? 1 : 0));

  function openList() {
    if (disabled) return;
    listOpen = true;
  }

  function closeList() {
    listOpen = false;
    highlight = -1;
  }

  /**
   * Take a chosen term and reset for the next one.
   *
   * The list CLOSES on a pick, and that is not a UX preference — an
   * open list overlays whatever sits beneath the control, and a
   * browser dispatches `click` to the common ancestor of mousedown and
   * mouseup. So with the list still up, the first click on the Save
   * button underneath it lands on nothing at all: mousedown hits the
   * option, mouseup hits Save, and neither element sees a click. The
   * collection field editor's own Save button is directly below this
   * control and was exactly that unreachable.
   *
   * Focus stays in the input, so the next term is one keystroke away
   * and typing reopens the list.
   */
  function commit(next: string[]) {
    onchange(next);
    draft = '';
    closeList();
    inputEl?.focus();
  }

  function add(entry: string) {
    if (disabled) return;
    if (chosen.has(entry)) {
      draft = '';
      return;
    }
    commit([...value, entry]);
  }

  function remove(entry: string) {
    if (disabled) return;
    onchange(value.filter((v) => v !== entry));
    inputEl?.focus();
  }

  /** Pick row `i` of the keyboard walk. */
  function pickRow(i: number) {
    if (i < 0) return;
    if (i < matches.length) {
      add(matches[i].value);
      return;
    }
    if (creatable) add(creatable.term);
  }

  /**
   * Enter with nothing highlighted. Takes the first row — which is the
   * best match when one exists and the create row when none does — so
   * the common "type it, press Enter" gesture works without ever
   * creating a term that a real one would have satisfied.
   */
  function commitDraft() {
    if (!draft.trim()) return;
    if (highlight >= 0) {
      pickRow(highlight);
      return;
    }
    if (rowCount > 0) pickRow(0);
  }

  function onKeydown(e: KeyboardEvent) {
    if (disabled) return;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      openList();
      if (rowCount === 0) return;
      highlight =
        e.key === 'ArrowDown'
          ? (highlight + 1) % rowCount
          : highlight <= 0
            ? rowCount - 1
            : highlight - 1;
      return;
    }
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      openList();
      commitDraft();
      return;
    }
    if (e.key === 'Escape') {
      e.preventDefault();
      if (listOpen) closeList();
      else draft = '';
      return;
    }
    if (e.key === 'Backspace' && draft === '' && value.length > 0) {
      // Same gesture the row's tag input uses.
      remove(value[value.length - 1]);
    }
  }

  function onInput() {
    openList();
    highlight = -1;
  }

  /** Close only when focus genuinely left the control — not when it
      moved from the input to one of our own rows. */
  function onFocusOut(e: FocusEvent) {
    const next = e.relatedTarget;
    if (next instanceof Node && rootEl?.contains(next)) return;
    closeList();
  }
</script>

<div
  bind:this={rootEl}
  class="relative"
  onfocusout={onFocusOut}
  data-testid="vocab-combobox-{testid}"
>
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div
    class="mt-0.5 flex flex-wrap items-center gap-1 rounded border border-border-strong bg-surface px-1.5 py-1 focus-within:ring-2 focus-within:ring-ring"
    class:opacity-50={disabled}
    onclick={() => { inputEl?.focus(); openList(); }}
  >
    {#each value as entry (entry)}
      <span
        class="inline-flex min-h-7 items-center gap-1 rounded-full bg-surface-elevated px-2 py-0.5 text-xs text-fg"
        class:border={isNew(entry)}
        class:border-accent={isNew(entry)}
        data-testid="vocab-chip-{testid}"
        data-value={entry}
      >
        {chipLabel(entry)}
        {#if isNew(entry)}
          <span class="text-[10px] uppercase tracking-wide text-accent">{t('vocabulary.chip_new')}</span>
        {/if}
        <button
          type="button"
          {disabled}
          onclick={(e) => { e.stopPropagation(); remove(entry); }}
          class="inline-flex h-5 w-5 items-center justify-center rounded-full text-fg-muted hover:bg-state-hover hover:text-fg"
          aria-label={t('vocabulary.remove_term', { label: chipLabel(entry) })}
          data-testid="vocab-chip-remove-{testid}"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
            <path d="M18 6 6 18M6 6l12 12" />
          </svg>
        </button>
      </span>
    {/each}
    <input
      bind:this={inputEl}
      bind:value={draft}
      type="text"
      role="combobox"
      autocomplete="off"
      {disabled}
      oninput={onInput}
      onkeydown={onKeydown}
      onfocus={openList}
      aria-label={labelledBy ? undefined : (label ?? t('vocabulary.input_aria'))}
      aria-labelledby={labelledBy}
      aria-expanded={listOpen}
      aria-controls={listId}
      aria-autocomplete="list"
      aria-activedescendant={listOpen && highlight >= 0 ? rowId(highlight) : undefined}
      placeholder={placeholder ?? (value.length === 0 ? t('vocabulary.placeholder') : '+')}
      data-testid="vocab-input-{testid}"
      class="min-h-9 min-w-[7rem] flex-1 rounded border border-transparent bg-transparent px-1.5 py-0.5 text-sm text-fg placeholder:text-fg-muted/60 focus:outline-none disabled:cursor-not-allowed"
    />
  </div>

  {#if listOpen}
    <div
      id={listId}
      role="listbox"
      aria-label={label ?? t('vocabulary.input_aria')}
      class="absolute left-0 right-0 top-full z-20 mt-1 max-h-64 overflow-y-auto rounded-md border border-border bg-surface shadow-lg"
      data-testid="vocab-list-{testid}"
    >
      {#each matches as opt, i (opt.value)}
        <button
          type="button"
          role="option"
          id={rowId(i)}
          aria-selected={i === highlight}
          onpointerdown={(e) => e.preventDefault()}
          onclick={() => add(opt.value)}
          onmousemove={() => (highlight = i)}
          class="flex min-h-11 w-full items-center justify-between gap-2 px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
          class:bg-surface-elevated={i === highlight}
          data-testid="vocab-option-{testid}"
          data-value={opt.value}
        >
          <span class="truncate"
            >{opt.status === 'active' ? opt.label : t('common.option_deprecated', { label: opt.label })}</span
          >
          <span class="shrink-0 font-mono text-[10px] text-fg-muted">{opt.value}</span>
        </button>
      {/each}

      {#if creatable}
        <button
          type="button"
          role="option"
          id={rowId(matches.length)}
          aria-selected={highlight === matches.length}
          onpointerdown={(e) => e.preventDefault()}
          onclick={() => add(creatable.term)}
          onmousemove={() => (highlight = matches.length)}
          class="flex min-h-11 w-full items-center justify-between gap-2 border-t border-border px-3 py-1.5 text-left text-sm text-accent hover:bg-surface-elevated"
          class:bg-surface-elevated={highlight === matches.length}
          data-testid="vocab-create-{testid}"
        >
          <span class="truncate">{t('vocabulary.create_term', { term: creatable.term })}</span>
          <span class="shrink-0 font-mono text-[10px] text-fg-muted">{creatable.slug}</span>
        </button>
      {/if}

      {#if blocked}
        <p class="border-t border-border px-3 py-2 text-xs text-fg-muted" data-testid="vocab-blocked-{testid}">
          {blocked}
        </p>
      {:else if matches.length === 0 && !creatable}
        <p class="px-3 py-2 text-xs text-fg-muted" data-testid="vocab-empty-{testid}">
          {draft.trim() ? t('vocabulary.no_matches') : t('vocabulary.no_terms')}
        </p>
      {/if}

      {#if truncated}
        <p class="border-t border-border px-3 py-1.5 text-[10px] text-fg-muted" data-testid="vocab-truncated-{testid}">
          {t('vocabulary.truncated', { count: MAX_ROWS })}
        </p>
      {/if}
    </div>
  {/if}
</div>
