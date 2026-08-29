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
  // # ⭐ TWO CATEGORIES, ONE MENU (#1251 slice 3 → #1292, owner rulings)
  //
  // Under the type rows sits a CONTENT category holding two more rows.
  // It shipped as a `HIDE` section carrying one switch, and the owner
  // sent that back: "I don't like how Hide AI-made work is a different
  // section in the filter menu. It should be like the others, but just
  // in a different category. 'AI-Content' with a checkbox. Not in hide,
  // but content. Should include mature in there too."
  //
  // ⭐ SO EVERY TICK IN THIS MENU MEANS SHOW, TOP TO BOTTOM. That is
  // the whole of #1292 and it is a behaviour change, not a restyle. A
  // `HIDE` heading was the only thing telling a reader that one row's
  // tick meant the opposite of the eleven above it, so moving the row
  // into the same visual grammar WITHOUT flipping it would have been
  // strictly worse than leaving it alone: identical rows that disagree
  // about what a tick does, and nothing on screen saying so.
  //
  // ⚠️ THE FLIP IS AT THE BOUNDARY, AND THE STORED VALUES DID NOT MOVE.
  // `hideAI` and `hideMature` still mean HIDE, on the props, in the
  // store and in localStorage; this component renders `!hide` and
  // reports `!checked` back. It is presentation, so there is nothing to
  // migrate and no device to catch up: a browser carrying
  // `aa_browse_hide_ai=1` from before this change gets the same wall it
  // had, now drawn as an unticked row. The alternative, renaming the
  // key to a permissive `show_ai`, would have made an ABSENT key read
  // as "do not show" and inverted the default for every reader who has
  // never touched the control. See readHideAI and readHideMature for
  // why the zero value is the safe answer only under the restrictive
  // name on these two axes.
  //
  // ⭐ THE TWO CONTENT ROWS ARE NOT ONE MECHANISM, and the code must
  // not pretend they are just because the reader is answering one
  // question. AI is device-local, client-resolved, and by ADR 0094 §4
  // NEVER gates: everyone gets that row, signed in or not. Mature is
  // ADR 0090's layer 3, a VIEW filter over rows a three-conjunct gate
  // has already allowed, so its row appears only when the instance
  // allows mature content AND this reader can actually RECEIVE mature
  // rows, and both rungs are ABSENCE rather than disablement. A ticked
  // box that silently does nothing because the instance forbids it is
  // the failure the cascade exists to make unreachable.
  //
  // ⛔ AND THE MATURE ROW NEVER CONSENTS. Consent is layer 2, on
  // /account/preferences. This row can only subtract from what that
  // consent already allowed, which is why it defaults to INCLUDED and
  // why `matureParam` has no "include" value to send.
  //
  // ⛔ THE TWO AXES PERSIST IN DIFFERENT PLACES, ON PURPOSE, AND SHARING
  // A MENU DOES NOT MERGE THEM. This is signed off, not an oversight
  // waiting to be tidied up:
  //
  //   TYPES  → the URL (`?kind=`), owned by the page. A type-filtered
  //            wall is a thing you SEND someone, so the back button has
  //            to walk it and a direct load has to reproduce it.
  //   CONTENT → `browseView` → localStorage, owned by the footer. "I
  //            would rather not look at AI work" describes the READER,
  //            not the page: it should survive every navigation, and
  //            pasting it into somebody else's browser would impose your
  //            preference on them under cover of sharing a link.
  //
  // Which is why this component takes them as separate props and
  // separate callbacks rather than one bag of filter state. It owns
  // none of them; it renders a draft of each and hands them back on
  // Apply, and the HOST decides where each one lands.
  //
  // ⭐ THE THIRD TOGGLE WAS ASKED THE SAME QUESTION, which is what the
  // note here used to promise. Mature is NOT the URL, for the reason
  // above: it describes the reader, and a shared link that narrowed
  // somebody else's wall would be imposing a preference under cover of
  // sharing. It is NOT the account either, and that is the stronger
  // half of the answer rather than an inherited one:
  // `user_preferences.mature_content.show` is ADR 0090's layer 2, the
  // CONSENT, and writing it from this popover would make unticking a
  // row a revocation and re-ticking it a consent. Layer 3 narrows and
  // never consents, so it lands beside the AI flag on the device, by a
  // different route. readHideMature carries the argument in full.
  //
  // # ⭐ ONE Apply COMMITS BOTH
  //
  // The hide toggle is drafted like the checkboxes and committed by the
  // same button, and that is the point rather than a convenience. A
  // panel with an Apply button in it makes exactly one promise —
  // "nothing you have touched has happened yet" — and a control inside
  // it that committed live would break that promise for every other
  // control on the panel. Dismissing without applying throws BOTH drafts
  // away, which is the Cancel-less panel's existing contract.
  //
  // The multi-tick churn argument that made Apply necessary for the
  // checkboxes does not apply to a single boolean, so this is not that
  // argument repeated — it is the panel's contract being kept intact.
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
  // ⭐ `Bot` is the AI-generated glyph the card badge and the viewer
  // already draw (#1243, AiProvenanceBadge). Reused rather than picked
  // so the menu row and the badge cannot disagree about what "AI" looks
  // like. `Sparkles` was the HIDE switch's glyph and went with it.
  import Bot from '@lucide/svelte/icons/bot';
  // The mature glyph, and two candidates were rejected before it.
  // `ShieldAlert` loses on ADR 0090's own thesis: a shield is a
  // CLEARANCE metaphor, and "rating ⊥ clearance" is the one
  // conflation this axis exists to prevent. A crossed-out eye loses on
  // the row's polarity, which is the whole sprint: it would sit on a
  // row whose tick means SHOW and draw the reader toward the opposite
  // reading, then have to un-cross itself to stay honest. A warning
  // triangle is a CONTENT ADVISORY, which is what a rating is, and it
  // says the same thing ticked or not.
  import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
  import { t } from '$stores/lang.svelte';

  let {
    selected,
    onapply,
    hideAI = false,
    onhide,
    hideMature = false,
    onhidemature,
    matureAvailable = false,
    open = $bindable(false),
  }: {
    /** The APPLIED selection, owned by the page and mirrored in the URL.
     *  Empty means no filter — every type. */
    selected: readonly string[];
    /** Commit. Receives the new selection; empty array means "all", so
     *  the caller drops the query parameter entirely. */
    onapply: (kinds: string[]) => void;
    /** The APPLIED "hide AI-made work" state (#1251 slice 3), owned by
     *  the host and persisted in `browseView` → localStorage.
     *
     *  ⚠️ IT STILL MEANS HIDE. The row draws `!hideAI` and reports
     *  `!checked` back; the flip is presentation and stops at this
     *  component's edge, so no stored value moved when #1292 changed
     *  what the tick looks like. */
    hideAI?: boolean;
    /** Commit for the AI row. Separate callback from `onapply` because
     *  the two axes persist in DIFFERENT PLACES; see the two-owners
     *  note in the header. Receives the HIDE value, not the tick. */
    onhide?: (next: boolean) => void;
    /** The APPLIED "leave mature content out of these results" state
     *  (#1292), ADR 0090's layer 3. Means HIDE, like `hideAI`. */
    hideMature?: boolean;
    /** Commit for the mature row. Receives the HIDE value. */
    onhidemature?: (next: boolean) => void;
    /** Whether the mature row is offered AT ALL, resolved by the host
     *  from ADR 0090's layer-3 cascade: the instance allows mature
     *  content AND this reader can receive mature rows — which is
     *  consent OR the §2 moderation exemption since #1345.
     *
     *  ⚠️ THE HOST ANSWERS IT, AND THIS COMPONENT MUST NOT RE-DERIVE IT.
     *  `browseView.matureFilterAvailable` is the one definition and it
     *  is the same predicate `matureParam` gates the request on; a
     *  second spelling here is how the row and the request come to
     *  disagree.
     *
     *  ⛔ ABSENCE, NEVER DISABLEMENT, and it defaults to false so a
     *  host that forgets to answer offers nothing. A disabled row would
     *  advertise a filter this reader can never use and name a class of
     *  content the instance may have switched off entirely; a missing
     *  row says nothing at all, which is the honest rendering of "this
     *  does not apply to you". */
    matureAvailable?: boolean;
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
  const kindsFiltered = $derived(selected.length > 0 && selected.length < ALL.length);

  /** Does the button need to say "something is narrowing this wall"?
   *
   *  EITHER AXIS COUNTS. #1166's third rule — "a filtered wall that
   *  looks identical to an unfiltered one is how people conclude the
   *  site is broken" — is not about types, it is about the wall, and it
   *  bites HARDER on the AI axis: on most instances a wall with the
   *  purely-AI work removed IS the wall you would have got anyway, so
   *  the button is the only place the state can show. */
  /** The mature filter only narrows anything while the row is offered.
   *  A device carrying the flag from a session that HAD the row must
   *  not light the button up on one that does not, or the button states
   *  a filter the menu cannot show and nothing can turn off. It is the
   *  same predicate `matureParam` gates the request on, for the same
   *  reason, and it is read from the props so this component still owns
   *  no state. */
  const matureNarrowing = $derived(matureAvailable && hideMature);

  const isFiltered = $derived(kindsFiltered || hideAI || matureNarrowing);

  /** The DRAFT — what the boxes show right now. Separate from `selected`
   *  because Apply is what commits; closing without applying throws the
   *  draft away, which is the only reading of a Cancel-less panel that
   *  does not silently commit a half-made choice. */
  let draft = $state<Set<ViewKind>>(new Set());
  /** The AI row's draft. It goes through Apply like everything else in
   *  this panel; see the commit note in the header.
   *
   *  ⚠️ IT HOLDS THE HIDE VALUE, not the tick. The row renders
   *  `!hideDraft`; keeping the draft in the prop's own polarity is what
   *  makes `hideDraft !== hideAI` a correct change test. */
  let hideDraft = $state(false);
  /** The mature row's draft, in the same polarity. */
  let matureDraft = $state(false);

  function resetDraft() {
    draft = new Set(appliedSet.size === 0 ? ALL : ALL.filter((k) => appliedSet.has(k)));
    hideDraft = hideAI;
    matureDraft = hideMature;
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
    // ONE Apply COMMITS BOTH AXES — see the commit note in the header.
    // Fired only on a real change so an untouched hide toggle does not
    // rewrite localStorage every time somebody applies a type filter.
    if (hideDraft !== hideAI) onhide?.(hideDraft);
    // Same rule for the mature row, and it is ALSO guarded on
    // availability: a panel that never drew the row has no draft the
    // reader touched, so committing one would write a filter nobody
    // asked for from a control they never saw.
    if (matureAvailable && matureDraft !== hideMature) onhidemature?.(matureDraft);
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

        <!-- ⭐ The CONTENT category (#1292). Inside the same scroll box
             as the types, under its own divider and heading, because
             the owner's ruling is one menu and not two. The heading is
             what says these rows are a different axis from the types
             above; without it the first of them reads as a thirteenth
             type.

             ⭐ PLAIN CHECKBOXES, and TICKED MEANS SHOW, exactly like
             every row above. The AI row was a `role="switch"` whose
             tick meant HIDE, under a `HIDE` heading that was the only
             thing saying so. Both went: a menu where one row's tick
             means the opposite of the others is the complaint this
             category answers, and a switch beside eleven checkboxes was
             half of what made the row look like an exception.

             The polarity flip is here and nowhere else. `hideAI` and
             `hideMature` still mean HIDE; these rows render `!hide` and
             report `!checked` back, so nothing stored had to move and a
             device carrying the old key gets the wall it had. -->
        <div class="my-1 border-t border-border"></div>
        <p class="px-2.5 pb-1 pt-1.5 text-xs font-semibold uppercase tracking-wide text-fg-muted">
          {t('browse.filter.content.label')}
        </p>
        <label
          title={t('browse.filter.ai.hint')}
          class="flex min-h-11 cursor-pointer select-none items-center gap-2.5 rounded-xl px-2.5
                 text-sm text-fg hover:bg-surface-hover"
        >
          <input
            type="checkbox"
            data-testid="ai-filter-toggle"
            checked={!hideDraft}
            onchange={() => (hideDraft = !hideDraft)}
            class="h-4 w-4 shrink-0 accent-accent"
          />
          <Bot size={15} strokeWidth={2} aria-hidden="true" />
          <span>{t('browse.filter.ai.label')}</span>
        </label>

        <!-- ⭐ The mature row is ABSENT, not disabled, for a reader the
             cascade does not offer it to (ADR 0090's 2026-08-26
             amendment, widened 2026-08-28). The host answers
             `matureAvailable` from the two rungs: the instance allows
             mature content, and this reader can RECEIVE mature rows —
             by consent, or by the §2 moderation exemption that lets a
             moderator see what the instance switch hid. A signed-out
             reader fails both, because an anonymous viewer can neither
             consent nor hold a capability.

             So a ticked mature row on an install with the feature off
             is not reachable: there is no row. Rendering it disabled
             would advertise a filter this reader can never use and name
             a class of content the operator may have switched off
             entirely. -->
        {#if matureAvailable}
          <label
            title={t('browse.filter.mature.hint')}
            class="flex min-h-11 cursor-pointer select-none items-center gap-2.5 rounded-xl px-2.5
                   text-sm text-fg hover:bg-surface-hover"
          >
            <input
              type="checkbox"
              data-testid="mature-filter-toggle"
              checked={!matureDraft}
              onchange={() => (matureDraft = !matureDraft)}
              class="h-4 w-4 shrink-0 accent-accent"
            />
            <TriangleAlert size={15} strokeWidth={2} aria-hidden="true" />
            <span>{t('browse.filter.mature.label')}</span>
          </label>
        {/if}
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
       and only when there IS one.

       Since #1251 slice 3 it states EVERY axis at once: the accent fill
       means "something in here is narrowing the wall", the count means
       types, the bot glyph means AI work is hidden, and the warning
       glyph means mature work is. Independent signals rather than one,
       because a reader who has hidden AI work and filtered to video
       needs to be able to tell which of them is responsible for a thin
       wall; a single "active" dot would send them into the menu to find
       out.

       ⚠️ THE MATURE GLYPH FOLLOWS THE ROW'S AVAILABILITY, not the
       stored flag. A device that narrowed on an install which offers
       the row must not light this up on one that does not, or the
       button states a filter the menu has no control for. -->
  <!-- svelte-ignore a11y_no_redundant_roles -->
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
    {#if kindsFiltered}
      <span class="tabular-nums font-semibold">{selected.length}</span>
    {/if}
    {#if hideAI}
      <Bot size={14} strokeWidth={2.5} data-testid="ai-filter-active" aria-hidden="true" />
    {/if}
    {#if matureNarrowing}
      <TriangleAlert size={14} strokeWidth={2.5} data-testid="mature-filter-active" aria-hidden="true" />
    {/if}
  </button>
</div>
