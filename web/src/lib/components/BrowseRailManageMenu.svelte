<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The browse rail's ⋯ manage panel (#1113, #1123).
   *
   * # Why this is not the shared `Menu`
   *
   * `Menu` is a list of items you activate one of. This is a small
   * editor: a search field, three lists, a toggle and a handle per row.
   * Three of Menu's behaviours are actively wrong for that shape and
   * none of them is a style choice.
   *
   *   - It moves focus to the first `a, button` on open. Here the first
   *     thing a reader wants is the SEARCH FIELD, which is neither.
   *   - It owns ArrowUp/ArrowDown to walk its items. This panel needs
   *     those keys for the reorder (the #1100 separator precedent), and
   *     two owners of one key means the reorder silently does nothing.
   *   - It closes on any `<a>` click. Correct for the Explore link at
   *     the bottom, wrong for nothing else here — but the rows are
   *     buttons, so that part would have held.
   *
   * What IS copied from Menu, deliberately and to the letter:
   *
   *   - LIGHT-DISMISS ON `click`, NOT `pointerdown`. #1105 is the bill
   *     for the other choice: a panel that reflows its own contents
   *     between pointerdown and pointerup moves the control out from
   *     under the cursor and the click the reader aimed never lands.
   *     This panel has exactly that shape — hiding a team re-sorts the
   *     list under the pointer — so dismissal has to survive the reflow
   *     it causes. On `click` it does.
   *   - Escape closes and returns focus to the trigger.
   *   - A REAL-BOX TRIGGER. #1109/#1097: a `display: contents` button
   *     generates no box, cannot take focus and is skipped by Tab. The
   *     trigger below is an `inline-flex` button with the glyph inside
   *     it and a visually-hidden name, which is the shape sprint 20
   *     landed and the only thing that makes this keyboard-reachable.
   *
   * # Hiding and following are different verbs
   *
   * Every row carries both, and conflating them would be the easy
   * mistake. UNFOLLOWING changes what your "Following" feed contains
   * and what the rail sorts first. HIDING only removes the chip from
   * your rail — the team's work still reaches your feed, which #1113
   * states outright and which is why the hide list is client-applied
   * and never reaches the posts query.
   *
   * # Tag follows (#1123)
   *
   * The "Following tags" section sits BETWEEN the two team lists, with
   * a follow-a-hashtag field at its head. It carries the same row shape
   * as the followed-teams list — grip, name, hide toggle, unfollow —
   * because it is the same gesture on a different kind of chip.
   *
   * That position mirrors the RAIL's, and the two have to agree: the
   * strip draws followed teams, then followed tags, then everything
   * else, so a panel that listed tags last would have the reader
   * dragging rows in an order the chips do not appear in. It also puts
   * the only WRITE in the panel above the long all-teams list instead
   * of below it.
   *
   * ⚠️ The seam note this replaces claimed the two team lists were
   * "already `{#snippet}`-shaped around a title and a row list, so
   * adding it is a third call with a different source rather than a
   * rewrite." THEY ARE NOT AND NEVER WERE — both are inline `{#each}`
   * blocks, and the tag rows differ from them in every cell anyway (a
   * hash tile instead of an avatar, a string key instead of a uuid, a
   * different store, a different reorder primitive). The third section
   * is therefore written out like its neighbours. Extracting all three
   * into one snippet is possible and was not done: the three sections
   * share a row SHAPE and no row CONTENT, so the snippet would take a
   * parameter per cell and read worse than the repetition.
   *
   * # There is no "all tags" list, deliberately
   *
   * The teams half offers the whole visible directory to follow from.
   * The tags half cannot: the corpus is unbounded and spans posts the
   * caller cannot read, so enumerating it would be a disclosure. Hence
   * a free-text field — and hence following a not-yet-used tag being
   * legal, which is what keeps that field from being a trap.
   */
  import { tick } from 'svelte';
  import { teamFollows, type TeamSummary } from '$stores/teamFollows.svelte';
  import { browseRail } from '$stores/browseRail.svelte';
  import { tagFollows, normalizeTagInput } from '$stores/tagFollows.svelte';
  import { t } from '$stores/lang.svelte';
  import TeamAvatar from '$components/TeamAvatar.svelte';
  import GripVertical from '@lucide/svelte/icons/grip-vertical';
  import Hash from '@lucide/svelte/icons/hash';
  import X from '@lucide/svelte/icons/x';
  import Eye from '@lucide/svelte/icons/eye';
  import EyeOff from '@lucide/svelte/icons/eye-off';
  import Check from '@lucide/svelte/icons/check';
  import Plus from '@lucide/svelte/icons/plus';
  import Search from '@lucide/svelte/icons/search';
  import ArrowRight from '@lucide/svelte/icons/arrow-right';

  let open = $state(false);
  let query = $state('');
  let triggerEl: HTMLButtonElement | undefined = $state();
  let panelEl: HTMLDivElement | undefined = $state();
  let searchEl: HTMLInputElement | undefined = $state();

  function matches(team: TeamSummary): boolean {
    const q = query.trim().toLowerCase();
    if (q === '') return true;
    return team.name.toLowerCase().includes(q) || (team.slug ?? '').toLowerCase().includes(q);
  }

  const followedIds = $derived(new Set(teamFollows.items.map((c) => c.id)));
  /** The followed group in the same sequence the rail draws it, so a
   *  drag in here moves the chip the reader is looking at. */
  const followed = $derived(browseRail.followedInRailOrder().filter(matches));
  const unfollowed = $derived(
    browseRail.teams.filter((c) => !followedIds.has(c.id)).filter(matches),
  );

  /** Followed tags in rail order, filtered by the same search box.
   *
   *  INCLUDING HIDDEN ONES, like the teams list above: the hide toggle
   *  lives on the row, so a hidden chip that vanished from the panel
   *  would be one the reader could never un-hide. */
  const followedTags = $derived(
    browseRail.sortTagsByUserOrder(tagFollows.tags).filter((tag) => {
      const q = query.trim().toLowerCase();
      // The search box is shared with the teams lists, and a reader
      // typing "#" means "show me the tags". Matching a bare `#` against
      // the tag TEXT would find nothing, since the hash is drawn rather
      // than stored.
      if (q === '' || q === '#') return true;
      return tag.toLowerCase().includes(normalizeTagInput(q).toLowerCase());
    }),
  );

  // ── Follow a hashtag ───────────────────────────────────────────────
  //
  // A free-text field rather than a picker, and that is forced rather
  // than chosen: there is no endpoint that lists the tag corpus, and
  // there should not be one — the corpus spans posts the caller cannot
  // read, so any "all tags" list is a disclosure (migration 00050 makes
  // the same argument about an existence probe).
  //
  // Following a tag nobody has used yet is legal and inert server-side,
  // which is what makes a free-text field honest here instead of a
  // silent failure: the reader gets the chip they asked for, and it
  // starts matching when the corpus catches up.
  let tagInput = $state('');
  let tagBusy = $state(false);

  const pendingTag = $derived(normalizeTagInput(tagInput));
  const alreadyFollowed = $derived(pendingTag !== '' && tagFollows.isFollowing(pendingTag));

  async function submitTag(e: Event) {
    e.preventDefault();
    const tag = pendingTag;
    if (tag === '' || tagBusy) return;
    // Following something already followed is a no-op the server would
    // absorb, but clearing the field on it would look like the follow
    // worked and produced nothing. Leave the text where it is; the
    // button is disabled and says why.
    if (tagFollows.isFollowing(tag)) return;
    tagBusy = true;
    try {
      await tagFollows.toggle(tag);
      // Cleared only on the attempt completing, not before it: a failed
      // follow that had already emptied the field would lose what the
      // reader typed.
      tagInput = '';
    } finally {
      tagBusy = false;
    }
  }

  async function close(): Promise<void> {
    if (!open) return;
    open = false;
    query = '';
    triggerEl?.focus();
    // A reorder made a moment before the panel closed is still inside
    // the store's debounce; a reload here would lose it.
    await browseRail.flush();
  }

  function onDocClick(e: MouseEvent) {
    if (!open) return;
    const target = e.target as Node;
    if (panelEl?.contains(target) || triggerEl?.contains(target)) return;
    void close();
  }

  function onKeydown(e: KeyboardEvent) {
    if (!open) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      void close();
    }
  }

  $effect(() => {
    if (!open) return;
    document.addEventListener('click', onDocClick, true);
    window.addEventListener('keydown', onKeydown);
    // The search field, not the first button: this panel is an editor
    // and typing is the fastest way through a long list.
    queueMicrotask(() => searchEl?.focus());
    return () => {
      document.removeEventListener('click', onDocClick, true);
      window.removeEventListener('keydown', onKeydown);
    };
  });

  // ── Reorder ────────────────────────────────────────────────────────
  //
  // Pointer-driven, one step at a time, with a keyboard path that is
  // the SAME operation rather than a parallel implementation — both
  // call `browseRail.move`, so the persisted result cannot differ by
  // input device. That is the #1100 precedent applied: arrow keys move
  // the focused row.
  //
  // The drag is deliberately not HTML5 drag-and-drop. A `dragstart`
  // inside a panel that light-dismisses on click means a drop outside
  // the panel closes it mid-gesture, and the drag image is a ghost of a
  // row that is about to move anyway.

  let dragId = $state<string | null>(null);
  /** Which LIST the in-flight drag belongs to. Teams and tags reorder
   *  through different store primitives over different sequences, and
   *  without this a drag started on a tag row would step the team order
   *  the moment the pointer crossed into the teams section — the rows
   *  are all in one scrolling panel and `rowIndexAt` only knows about
   *  y-coordinates. */
  let dragKind = $state<'team' | 'tag' | null>(null);

  function rowIndexAt(clientY: number, selector: string): number {
    const rows = panelEl?.querySelectorAll<HTMLElement>(selector);
    if (!rows) return -1;
    for (let i = 0; i < rows.length; i++) {
      const r = rows[i].getBoundingClientRect();
      if (clientY >= r.top && clientY <= r.bottom) return i;
    }
    return -1;
  }

  function onHandleDown(e: PointerEvent, id: string, kind: 'team' | 'tag') {
    if (e.button !== 0) return;
    dragId = id;
    dragKind = kind;
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    e.preventDefault();
  }

  function onHandleMove(e: PointerEvent) {
    if (!dragId || !dragKind) return;
    const isTag = dragKind === 'tag';
    const target = rowIndexAt(
      e.clientY,
      isTag ? '[data-rail-tag-row]' : '[data-rail-follow-row]',
    );
    if (target < 0) return;
    const current = isTag
      ? followedTags.indexOf(dragId)
      : followed.findIndex((c) => c.id === dragId);
    if (current < 0 || target === current) return;
    // One step per frame toward the pointer rather than a splice to the
    // target index: the store's `move` is the single reorder primitive
    // and stepping keeps the rendered list in sync with it at every
    // intermediate position, so the row under the cursor is always the
    // row being dragged.
    const delta = target > current ? 1 : -1;
    if (isTag) browseRail.moveTag(dragId, delta);
    else browseRail.move(dragId, delta);
  }

  function onHandleUp(e: PointerEvent) {
    if (!dragId) return;
    const el = e.currentTarget as HTMLElement;
    if (el.hasPointerCapture(e.pointerId)) el.releasePointerCapture(e.pointerId);
    dragId = null;
    dragKind = null;
  }

  async function onHandleKey(e: KeyboardEvent, id: string, kind: 'team' | 'tag') {
    if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
    e.preventDefault();
    // Stop the panel's own Escape/keydown listener and anything above
    // from also acting on this key.
    e.stopPropagation();
    const delta = e.key === 'ArrowUp' ? -1 : 1;
    if (kind === 'tag') browseRail.moveTag(id, delta);
    else browseRail.move(id, delta);
    // The row moved in the DOM, taking the focused button with it —
    // except that Svelte re-renders keyed rows by moving nodes, which
    // does NOT preserve focus in every engine. Re-focusing by id after
    // the render is what makes a run of arrow presses walk a row the
    // whole way rather than stopping after the first.
    //
    // The selector is escaped because a TAG is free text and lands in an
    // attribute selector here: `CSS.escape` is what stops a tag
    // containing a quote or a bracket from either throwing
    // (SyntaxError, which would break the reorder for that row only) or
    // matching something else. Team ids are uuids and never needed it,
    // which is exactly why it would have been easy to leave out.
    await tick();
    const sel = kind === 'tag' ? 'data-reorder-tag-handle' : 'data-reorder-handle';
    panelEl?.querySelector<HTMLElement>(`[${sel}="${CSS.escape(id)}"]`)?.focus();
  }

  const ROW_CLASS =
    'flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm hover:bg-state-hover';
  const ICON_BTN_CLASS =
    'grid h-8 w-8 shrink-0 place-items-center rounded-full border border-transparent text-fg-muted ' +
    'transition-colors hover:border-border hover:bg-state-hover hover:text-fg ' +
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring';
</script>

<div class="relative inline-block">
  <!-- Real-box trigger (see the component note). `inline-flex`, a
       visually-hidden NAME as content — not an `aria-label` on the
       inner span, which ARIA 1.2 forbids naming because a bare span
       maps to role `generic`. -->
  <button
    bind:this={triggerEl}
    type="button"
    class="inline-flex rounded-full"
    aria-haspopup="dialog"
    aria-expanded={open}
    data-testid="teams-rail-manage"
    onclick={() => (open ? void close() : (open = true))}
  >
    <span
      class="flex h-12 w-12 items-center justify-center rounded-full border border-border
             bg-surface-elevated text-fg-muted transition-colors hover:border-border-strong
             hover:bg-state-hover hover:text-fg"
      class:border-border-strong={open}
      class:text-fg={open}
    >
      <svg
        xmlns="http://www.w3.org/2000/svg"
        width="20"
        height="20"
        viewBox="0 0 24 24"
        fill="currentColor"
        aria-hidden="true"
      >
        <circle cx="5" cy="12" r="2" />
        <circle cx="12" cy="12" r="2" />
        <circle cx="19" cy="12" r="2" />
      </svg>
      <span class="sr-only">{t('teams.rail_manage')}</span>
    </span>
  </button>

  {#if open}
    <!-- `role="dialog"` rather than `menu`: it holds a text field and
         two editable lists, and a screen reader told "menu" will
         announce the rows as menu items and expect first-letter
         navigation to jump between them, which would fight the search
         field for every keystroke. -->
    <div
      bind:this={panelEl}
      role="dialog"
      aria-label={t('teams.rail_manage')}
      data-testid="teams-rail-manage-menu"
      class="absolute left-0 z-40 mt-1 max-h-[70vh] w-80 max-w-[calc(100vw-2rem)] overflow-y-auto
             rounded-lg border border-border bg-surface p-2 shadow-lg"
    >
      <div class="relative mb-2">
        <span class="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-fg-muted">
          <Search size={16} aria-hidden="true" />
        </span>
        <input
          bind:this={searchEl}
          bind:value={query}
          type="search"
          placeholder={t('teams.rail_search')}
          aria-label={t('teams.rail_search')}
          data-testid="teams-rail-search"
          class="w-full rounded-md border border-border bg-surface-elevated py-1.5 pl-8 pr-2 text-sm
                 text-fg placeholder:text-fg-muted focus:border-border-strong focus:outline-none"
        />
      </div>

      {#if followed.length > 0}
        <p class="px-2 pb-1 pt-1 text-xs font-medium uppercase tracking-wide text-fg-muted">
          {t('teams.rail_section_following')}
        </p>
        <ul data-testid="teams-rail-followed-list">
          {#each followed as team (team.id)}
            {@const hidden = browseRail.isHidden(team.id)}
            <li
              data-rail-follow-row
              data-team-id={team.id}
              class="flex items-center gap-1 {dragId === team.id ? 'opacity-60' : ''}"
            >
              <!-- The handle is a BUTTON, not a decorative grip: it is
                   the keyboard's only way into the reorder, and
                   `aria-describedby` is what tells a screen reader that
                   the arrow keys do something here. -->
              <button
                type="button"
                class="{ICON_BTN_CLASS} cursor-grab touch-none"
                data-reorder-handle={team.id}
                aria-label={t('teams.rail_reorder', { name: team.name })}
                aria-describedby="rail-reorder-hint"
                onpointerdown={(e) => onHandleDown(e, team.id, 'team')}
                onpointermove={onHandleMove}
                onpointerup={onHandleUp}
                onpointercancel={onHandleUp}
                onkeydown={(e) => onHandleKey(e, team.id, 'team')}
              >
                <GripVertical size={16} aria-hidden="true" />
              </button>
              <span class="flex min-w-0 flex-1 items-center gap-2 {ROW_CLASS}">
                <TeamAvatar {team} class="h-7 w-7 rounded-full" textClass="text-[10px]" />
                <span class="truncate {hidden ? 'text-fg-muted line-through' : 'text-fg'}"
                  >{team.name}</span
                >
              </span>
              <button
                type="button"
                class={ICON_BTN_CLASS}
                aria-pressed={hidden}
                aria-label={hidden ? t('teams.rail_show') : t('teams.rail_hide')}
                title={hidden ? t('teams.rail_show') : t('teams.rail_hide')}
                data-testid="teams-rail-hide-toggle"
                onclick={() => browseRail.toggleHidden(team.id)}
              >
                {#if hidden}
                  <EyeOff size={16} aria-hidden="true" />
                {:else}
                  <Eye size={16} aria-hidden="true" />
                {/if}
              </button>
              <!-- The followed-checkmark. Pressed = following; clicking
                   unfollows, which is the reference design's gesture. -->
              <button
                type="button"
                class="{ICON_BTN_CLASS} text-accent"
                aria-pressed={true}
                aria-label={t('teams.unfollow')}
                title={t('teams.unfollow')}
                data-testid="teams-rail-follow-toggle"
                disabled={teamFollows.isPending(team.id)}
                onclick={() => void teamFollows.toggle(team)}
              >
                <Check size={16} aria-hidden="true" />
              </button>
            </li>
          {/each}
        </ul>
        <p id="rail-reorder-hint" class="sr-only">{t('teams.rail_reorder_hint')}</p>
      {/if}

      <!-- ═══ #1123: followed tags ═══════════════════════════════════
           Its own bordered block rather than a third bare list, because
           the field at its head is a WRITE and the two lists above are
           not: a text input flush against a list of team rows reads as
           a second search box. The border is what says "this part is
           about tags".

           ALWAYS RENDERED, unlike the two team sections, which hide
           when empty. The follow field IS this section's empty state —
           a reader following no tags yet is exactly who needs it, and
           hiding the only way to follow a first tag until you have
           followed one is the trap the team sections cannot fall into
           (the instance's teams exist whether you follow them or not). -->
      <div class="mt-2 border-t border-border pt-2">
        <p class="px-2 pb-1 text-xs font-medium uppercase tracking-wide text-fg-muted">
          {t('tags.rail_section')}
        </p>

        <!-- A real <form>, so Enter submits without a keydown handler
             racing the panel's own Escape/keydown listener. -->
        <form class="flex items-center gap-1 px-2 pb-1.5" onsubmit={submitTag}>
          <div class="relative min-w-0 flex-1">
            <span
              class="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-fg-muted"
              aria-hidden="true"
            >
              <Hash size={14} />
            </span>
            <input
              bind:value={tagInput}
              type="text"
              autocomplete="off"
              autocapitalize="none"
              spellcheck="false"
              maxlength="200"
              placeholder={t('tags.rail_follow_placeholder')}
              aria-label={t('tags.rail_follow_label')}
              data-testid="browse-rail-tag-input"
              class="w-full rounded-md border border-border bg-surface-elevated py-1.5 pl-7 pr-2
                     text-sm text-fg placeholder:text-fg-muted focus:border-border-strong
                     focus:outline-none"
            />
          </div>
          <button
            type="submit"
            class={ICON_BTN_CLASS}
            disabled={pendingTag === '' || alreadyFollowed || tagBusy}
            aria-label={t('tags.rail_follow_submit')}
            title={alreadyFollowed
              ? t('tags.rail_already_following', { tag: pendingTag })
              : t('tags.rail_follow_submit')}
            data-testid="browse-rail-tag-follow"
          >
            <Plus size={16} aria-hidden="true" />
          </button>
        </form>

        {#if followedTags.length > 0}
          <ul data-testid="browse-rail-tag-list">
            {#each followedTags as tag (tag)}
              {@const hidden = browseRail.isTagHidden(tag)}
              <li
                data-rail-tag-row
                data-tag={tag}
                class="flex items-center gap-1 {dragId === tag && dragKind === 'tag'
                  ? 'opacity-60'
                  : ''}"
              >
                <button
                  type="button"
                  class="{ICON_BTN_CLASS} cursor-grab touch-none"
                  data-reorder-tag-handle={tag}
                  aria-label={t('tags.rail_reorder', { tag })}
                  aria-describedby="rail-reorder-hint"
                  onpointerdown={(e) => onHandleDown(e, tag, 'tag')}
                  onpointermove={onHandleMove}
                  onpointerup={onHandleUp}
                  onpointercancel={onHandleUp}
                  onkeydown={(e) => onHandleKey(e, tag, 'tag')}
                >
                  <GripVertical size={16} aria-hidden="true" />
                </button>
                <span class="flex min-w-0 flex-1 items-center gap-2 {ROW_CLASS}">
                  <!-- The 28px hash disc stands where a team's avatar
                       does, so the two row kinds line up down the panel
                       instead of the tag names sitting 28px left of the
                       team names. -->
                  <span
                    class="flex h-7 w-7 shrink-0 items-center justify-center rounded-full
                           bg-state-hover text-fg-muted"
                    aria-hidden="true"
                  >
                    <Hash size={14} />
                  </span>
                  <span class="truncate {hidden ? 'text-fg-muted line-through' : 'text-fg'}"
                    >{tag}</span
                  >
                </span>
                <button
                  type="button"
                  class={ICON_BTN_CLASS}
                  aria-pressed={hidden}
                  aria-label={hidden ? t('teams.rail_show') : t('teams.rail_hide')}
                  title={hidden ? t('teams.rail_show') : t('teams.rail_hide')}
                  data-testid="browse-rail-tag-hide"
                  onclick={() => browseRail.toggleTagHidden(tag)}
                >
                  {#if hidden}
                    <EyeOff size={16} aria-hidden="true" />
                  {:else}
                    <Eye size={16} aria-hidden="true" />
                  {/if}
                </button>
                <!-- UNFOLLOW, not hide. An X rather than the teams
                     list's pressed checkmark: a tag row is only ever
                     here BECAUSE it is followed (there is no "all tags"
                     list for it to move into), so the control is a
                     removal rather than a toggle between two states the
                     panel can show. -->
                <button
                  type="button"
                  class="{ICON_BTN_CLASS} hover:text-danger"
                  aria-label={t('tags.rail_unfollow', { tag })}
                  title={t('tags.rail_unfollow', { tag })}
                  data-testid="browse-rail-tag-unfollow"
                  disabled={tagFollows.isPending(tag)}
                  onclick={() => void tagFollows.toggle(tag)}
                >
                  <X size={16} aria-hidden="true" />
                </button>
              </li>
            {/each}
          </ul>
          <!-- The reorder hint is shared with the teams list above and
               is rendered there. When the teams list is empty but this
               one is not, nothing would render it — so this arm covers
               that case, and the `{#if}` stops the id being duplicated
               when both lists are on screen. -->
          {#if followed.length === 0}
            <p id="rail-reorder-hint" class="sr-only">{t('teams.rail_reorder_hint')}</p>
          {/if}
        {:else if query.trim() === ''}
          <p class="px-2 pb-1 text-xs text-fg-muted" data-testid="browse-rail-tag-empty">
            {t('tags.rail_empty')}
          </p>
        {/if}
      </div>

      {#if unfollowed.length > 0}
        <p class="px-2 pb-1 pt-2 text-xs font-medium uppercase tracking-wide text-fg-muted">
          {t('teams.rail_section_all')}
        </p>
        <ul data-testid="teams-rail-all-list">
          {#each unfollowed as team (team.id)}
            {@const hidden = browseRail.isHidden(team.id)}
            <li class="flex items-center gap-1">
              <span class="flex min-w-0 flex-1 items-center gap-2 {ROW_CLASS}">
                <TeamAvatar {team} class="h-7 w-7 rounded-full" textClass="text-[10px]" />
                <span class="truncate {hidden ? 'text-fg-muted line-through' : 'text-fg'}"
                  >{team.name}</span
                >
              </span>
              <button
                type="button"
                class={ICON_BTN_CLASS}
                aria-pressed={hidden}
                aria-label={hidden ? t('teams.rail_show') : t('teams.rail_hide')}
                title={hidden ? t('teams.rail_show') : t('teams.rail_hide')}
                onclick={() => browseRail.toggleHidden(team.id)}
              >
                {#if hidden}
                  <EyeOff size={16} aria-hidden="true" />
                {:else}
                  <Eye size={16} aria-hidden="true" />
                {/if}
              </button>
              <button
                type="button"
                class={ICON_BTN_CLASS}
                aria-pressed={false}
                aria-label={t('teams.follow')}
                title={t('teams.follow')}
                data-testid="teams-rail-follow-add"
                disabled={teamFollows.isPending(team.id)}
                onclick={() => void teamFollows.toggle(team)}
              >
                <Plus size={16} aria-hidden="true" />
              </button>
            </li>
          {/each}
        </ul>
      {/if}

      {#if followed.length === 0 && unfollowed.length === 0 && followedTags.length === 0}
        <p class="px-2 py-3 text-sm text-fg-muted" data-testid="teams-rail-search-empty">
          {t('teams.rail_search_empty')}
        </p>
      {/if}

      <div class="mt-2 border-t border-border pt-2">
        <a
          href="/teams"
          class="flex items-center gap-2 rounded-md px-2 py-1.5 text-sm font-medium text-accent
                 hover:bg-state-hover"
          data-testid="teams-rail-explore"
          onclick={() => void close()}
        >
          <ArrowRight size={16} aria-hidden="true" />
          {t('teams.rail_explore')}
        </a>
      </div>
    </div>
  {/if}
</div>
