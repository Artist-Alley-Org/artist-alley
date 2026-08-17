<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The browse footer's ASSET-TYPE filter (#1166) — the compact icon
  // button that sits beside the Newest/Oldest sort and opens a list of
  // checkboxes.
  //
  // # The anatomy is ratified, not invented
  //
  // It reproduces a control the owner has shipped before: an icon
  // button in the sort control's toolbar spot, a checkbox dropdown of
  // "All types" plus one box per type, ALL-CHECKED MEANS NO FILTER, an
  // active-state highlight on the button when a real subset is picked,
  // and an explicit Apply. Each of those is load-bearing:
  //
  //   ALL-CHECKED = OFF is what makes the control's resting state
  //   honest. The alternative — none-checked means everything — makes
  //   the first click a de-selection of things the user never selected,
  //   and leaves no way to express "all" that looks like all.
  //
  //   APPLY, not live-commit. Ticking four boxes live would refetch the
  //   wall four times and reshuffle it under the pointer between
  //   clicks. The draft lives here; the feed only learns about it once.
  //
  //   THE BUTTON CARRIES THE STATE. A filtered wall that looks
  //   identical to an unfiltered one is how people conclude the site is
  //   broken. The button is highlighted and captioned with the count
  //   whenever a real subset is applied.
  //
  // # Where the type list comes from
  //
  // FILTERABLE_KINDS in kindIcon.ts, which derives from the same
  // exhaustive kind→icon map the card badge draws from, with labels off
  // the same i18n keys the badge names itself with. No list is written
  // here — a checkbox that said something the badge does not is exactly
  // the disagreement this control has to be checkable against.
  //
  // # Dismissal follows ViewControls' convention, deliberately
  //
  // click (not pointerdown) in the capture phase, a `pressedInside`
  // memory so a drag out of the panel is not a dismissal, and Escape
  // returning focus to the toggle. The reasoning is written out at
  // length in ViewControls.svelte's light-dismiss block (#1096 →
  // #1105); the short version is that dismissing on pointerdown
  // reflows the bar between down and up and eats the click the user
  // aimed at. NOT `:focus-within` — a mouse click focuses what it lands
  // on, which would pin the panel exactly when someone is leaving it
  // (#1020's class).

  import type { ViewKind } from './viewers/controller';
  import { FILTERABLE_KINDS, iconForKind, MultiAssetIcon } from './kindIcon';
  import { t } from '$stores/lang.svelte';

  let {
    selected,
    onapply,
    open = $bindable(false),
  }: {
    /** The APPLIED selection, owned by the page and mirrored in the URL.
     *  Empty means no filter — every type. */
    selected: readonly string[];
    /** Commit. Receives the new selection; empty array means "all", so
     *  the caller drops the query parameter entirely. */
    onapply: (kinds: string[]) => void;
    /** Bound out so the footer bar can hold itself on screen while the
     *  panel is up — the bar auto-hides on scroll and yanking it away
     *  mid-interaction would be hostile. */
    open?: boolean;
  } = $props();

  const ALL = FILTERABLE_KINDS;

  /** The applied selection as a set, for `checked` on first open. An
   *  empty applied selection means every box is ticked — that is what
   *  "all-checked = no filter" reads like from the other direction. */
  const appliedSet = $derived(new Set(selected));
  const isFiltered = $derived(selected.length > 0 && selected.length < ALL.length);

  /** The DRAFT — what the boxes show right now. Separate from `selected`
   *  because Apply is what commits; closing without applying throws the
   *  draft away, which is the only reading of a Cancel-less panel that
   *  does not silently commit a half-made choice. */
  let draft = $state<Set<ViewKind>>(new Set());

  function resetDraft() {
    draft = new Set(appliedSet.size === 0 ? ALL : ALL.filter((k) => appliedSet.has(k)));
  }

  const allChecked = $derived(draft.size === ALL.length);

  function toggle(kind: ViewKind) {
    const next = new Set(draft);
    if (next.has(kind)) next.delete(kind);
    else next.add(kind);
    draft = next;
  }

  function toggleAll() {
    // "All types" is a control, not a member: ticking it means every
    // type, un-ticking it clears the board so the next tick is a
    // deliberate single choice rather than a hunt through twelve
    // already-ticked boxes.
    draft = allChecked ? new Set() : new Set(ALL);
  }

  /**
   * DOUBLE-CLICK SOLOS (owner: "If all types is selected, and I double
   * click PDF, it should deselect all but pdf").
   *
   * # Why a whole gesture rather than a "only" link on each row
   *
   * The common intent on this panel is a single type, and reaching it
   * from the resting all-checked state costs eleven un-ticks. Solo is
   * the mixer's answer to exactly that, and it needs no pixels: the
   * rows stay a plain checkbox list.
   *
   * # Why this does not fight the single click
   *
   * A double click also delivers its two ordinary clicks, so the box
   * toggles off and back on underneath before `dblclick` arrives. That
   * is harmless because solo is an ABSOLUTE write — `new Set([kind])`,
   * not a mutation of whatever the two toggles happened to leave — so
   * the end state is the same however many clicks the browser decided
   * to forward through the <label>. Single click keeps its exact old
   * meaning; nothing is debounced and no click is swallowed.
   *
   * # Fine pointers only, and that is fine
   *
   * Touch has no reliable double-tap-to-act (the browser spends it on
   * zoom intent), so on a phone this gesture simply never fires and
   * every row stays an ordinary toggle. It is an accelerator, not the
   * only route to a single type — un-ticking "All types" and ticking
   * one box reaches the same place with taps.
   *
   * Nothing commits here either way: Apply is still what the feed
   * hears.
   */
  function solo(kind: ViewKind) {
    draft = new Set([kind]);
  }

  /** The same gesture on the "All types" row, which has no one type to
   *  solo: double-clicking it lands on every type. Its two ordinary
   *  clicks toggle it twice and land back where they started only when
   *  it began checked, so without this a double click on All from a
   *  subset would end with the board cleared — the opposite of what the
   *  row says it does. */
  function soloAll() {
    draft = new Set(ALL);
  }

  function apply() {
    // All-checked and none-checked BOTH commit as "no filter". Nothing
    // ticked is not a request for an empty wall — it is a half-made
    // selection, and answering it with zero results looks like the site
    // broke. The server would honour an empty-but-present filter with an
    // empty page, so the collapse happens here, where the user's intent
    // is still visible.
    onapply(allChecked || draft.size === 0 ? [] : ALL.filter((k) => draft.has(k)));
    open = false;
    toggleEl?.focus();
  }

  function openPanel() {
    resetDraft();
    open = true;
  }

  let panelEl = $state<HTMLDivElement | null>(null);
  let toggleEl = $state<HTMLButtonElement | null>(null);
  let pressedInside = false;

  function onWindowKey(e: KeyboardEvent) {
    if (e.key !== 'Escape' || !open) return;
    open = false;
    toggleEl?.focus();
  }

  function onWindowPointerDown(e: PointerEvent) {
    const target = e.target;
    pressedInside = target instanceof Node && panelEl?.contains(target) === true;
  }

  function onWindowClick(e: MouseEvent) {
    const target = e.target;
    const inside = pressedInside || (target instanceof Node && panelEl?.contains(target));
    pressedInside = false;
    if (inside) return;
    open = false;
  }

  $effect(() => {
    if (!open) return;
    pressedInside = false;
    window.addEventListener('keydown', onWindowKey);
    window.addEventListener('pointerdown', onWindowPointerDown, true);
    window.addEventListener('click', onWindowClick, true);
    return () => {
      window.removeEventListener('keydown', onWindowKey);
      window.removeEventListener('pointerdown', onWindowPointerDown, true);
      window.removeEventListener('click', onWindowClick, true);
    };
  });
</script>

<div bind:this={panelEl} class="pointer-events-auto relative">
  {#if open}
    <!-- Anchored ABOVE the button: this bar lives at the bottom of the
         viewport, so a panel dropping downward would open off-screen.
         `right-0` keeps it inside the right edge at 390px, where the
         button itself is only ~44px from it. -->
    <div
      data-testid="kind-filter-panel"
      role="group"
      aria-label={t('browse.filter.type.label')}
      class="absolute bottom-full right-0 z-30 mb-2 w-56 max-w-[calc(100vw-2rem)]
             rounded-2xl border border-border bg-surface-elevated p-2 shadow-xl"
    >
      <div class="max-h-[60vh] overflow-y-auto">
        <!-- `select-none`: a double click is a gesture here, and the
             browser's default answer to one on a label is to select its
             text. -->
        <!-- eslint-disable-next-line -->
        <label
          ondblclick={soloAll}
          class="flex min-h-11 cursor-pointer select-none items-center gap-2.5 rounded-xl px-2.5
                 text-sm font-semibold text-fg hover:bg-surface-hover"
        >
          <input
            type="checkbox"
            data-testid="kind-filter-all"
            checked={allChecked}
            onchange={toggleAll}
            class="h-4 w-4 shrink-0 accent-accent"
          />
          <MultiAssetIcon size={15} strokeWidth={2} aria-hidden="true" />
          <span>{t('browse.filter.type.all')}</span>
        </label>

        <div class="my-1 border-t border-border"></div>

        {#each ALL as kind (kind)}
          {@const Icon = iconForKind(kind)}
          <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
          <label
            ondblclick={() => solo(kind)}
            title={t('browse.filter.type.solo_hint')}
            class="flex min-h-11 cursor-pointer select-none items-center gap-2.5 rounded-xl px-2.5
                   text-sm text-fg hover:bg-surface-hover"
          >
            <input
              type="checkbox"
              data-testid="kind-filter-option"
              data-kind={kind}
              checked={draft.has(kind)}
              onchange={() => toggle(kind)}
              class="h-4 w-4 shrink-0 accent-accent"
            />
            <Icon size={15} strokeWidth={2} aria-hidden="true" />
            <span>{t(`card.fallback.kind.${kind}`)}</span>
          </label>
        {/each}
      </div>

      <div class="mt-1 border-t border-border pt-2">
        <button
          type="button"
          data-testid="kind-filter-apply"
          onclick={apply}
          class="inline-flex h-11 w-full items-center justify-center rounded-xl bg-accent px-3
                 text-sm font-semibold text-accent-fg transition-colors hover:opacity-90
                 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {t('browse.filter.type.apply')}
        </button>
      </div>
    </div>
  {/if}

  <!-- The toggle. Compact — icon only — because it shares the right
       cluster with the sort control, which already carries a word. The
       applied count is the one thing that earns text beside the glyph,
       and only when there IS one. -->
  <button
    bind:this={toggleEl}
    type="button"
    data-testid="kind-filter-toggle"
    onclick={() => (open ? (open = false) : openPanel())}
    title={t('browse.filter.type.label')}
    aria-label={t('browse.filter.type.label')}
    aria-expanded={open}
    aria-haspopup="true"
    data-active={isFiltered ? 'true' : undefined}
    class="inline-flex h-11 min-w-11 items-center justify-center gap-1.5 rounded-full border
           px-3 text-sm shadow-lg transition-colors focus-visible:outline-none
           focus-visible:ring-2 focus-visible:ring-ring
           {isFiltered
      ? 'border-accent bg-accent text-accent-fg hover:opacity-90'
      : 'border-border bg-surface-elevated text-fg hover:bg-surface-hover'}"
  >
    <MultiAssetIcon size={16} strokeWidth={2} aria-hidden="true" />
    {#if isFiltered}
      <span class="tabular-nums font-semibold">{selected.length}</span>
    {/if}
  </button>
</div>
