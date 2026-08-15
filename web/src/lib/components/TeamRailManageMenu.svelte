<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The teams rail's ⋯ manage panel (#1113).
   *
   * # Why this is not the shared `Menu`
   *
   * `Menu` is a list of items you activate one of. This is a small
   * editor: a search field, two lists, a toggle and a handle per row.
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
   * # Seam for tag follows (#1123)
   *
   * The reference design's "+ Follow a hashtag" is a THIRD section
   * under these two, with the same row shape and its own store. It is
   * deliberately not stubbed here — an empty section is worse than no
   * section — but the two lists below are already
   * `{#snippet}`-shaped around a title and a row list, so adding it is
   * a third call with a different source rather than a rewrite.
   */
  import { tick } from 'svelte';
  import { teamFollows, type TeamSummary } from '$stores/teamFollows.svelte';
  import { teamRail } from '$stores/teamRail.svelte';
  import { t } from '$stores/lang.svelte';
  import TeamAvatar from '$components/TeamAvatar.svelte';
  import GripVertical from '@lucide/svelte/icons/grip-vertical';
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
  const followed = $derived(teamRail.followedInRailOrder().filter(matches));
  const unfollowed = $derived(
    teamRail.teams.filter((c) => !followedIds.has(c.id)).filter(matches),
  );

  async function close(): Promise<void> {
    if (!open) return;
    open = false;
    query = '';
    triggerEl?.focus();
    // A reorder made a moment before the panel closed is still inside
    // the store's debounce; a reload here would lose it.
    await teamRail.flush();
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
  // call `teamRail.move`, so the persisted result cannot differ by
  // input device. That is the #1100 precedent applied: arrow keys move
  // the focused row.
  //
  // The drag is deliberately not HTML5 drag-and-drop. A `dragstart`
  // inside a panel that light-dismisses on click means a drop outside
  // the panel closes it mid-gesture, and the drag image is a ghost of a
  // row that is about to move anyway.

  let dragId = $state<string | null>(null);

  function rowIndexAt(clientY: number): number {
    const rows = panelEl?.querySelectorAll<HTMLElement>('[data-rail-follow-row]');
    if (!rows) return -1;
    for (let i = 0; i < rows.length; i++) {
      const r = rows[i].getBoundingClientRect();
      if (clientY >= r.top && clientY <= r.bottom) return i;
    }
    return -1;
  }

  function onHandleDown(e: PointerEvent, id: string) {
    if (e.button !== 0) return;
    dragId = id;
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    e.preventDefault();
  }

  function onHandleMove(e: PointerEvent) {
    if (!dragId) return;
    const target = rowIndexAt(e.clientY);
    if (target < 0) return;
    const current = followed.findIndex((c) => c.id === dragId);
    if (current < 0 || target === current) return;
    // One step per frame toward the pointer rather than a splice to the
    // target index: the store's `move` is the single reorder primitive
    // and stepping keeps the rendered list in sync with it at every
    // intermediate position, so the row under the cursor is always the
    // row being dragged.
    teamRail.move(dragId, target > current ? 1 : -1);
  }

  function onHandleUp(e: PointerEvent) {
    if (!dragId) return;
    const el = e.currentTarget as HTMLElement;
    if (el.hasPointerCapture(e.pointerId)) el.releasePointerCapture(e.pointerId);
    dragId = null;
  }

  async function onHandleKey(e: KeyboardEvent, id: string) {
    if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
    e.preventDefault();
    // Stop the panel's own Escape/keydown listener and anything above
    // from also acting on this key.
    e.stopPropagation();
    teamRail.move(id, e.key === 'ArrowUp' ? -1 : 1);
    // The row moved in the DOM, taking the focused button with it —
    // except that Svelte re-renders keyed rows by moving nodes, which
    // does NOT preserve focus in every engine. Re-focusing by id after
    // the render is what makes a run of arrow presses walk a row the
    // whole way rather than stopping after the first.
    await tick();
    panelEl?.querySelector<HTMLElement>(`[data-reorder-handle="${id}"]`)?.focus();
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
            {@const hidden = teamRail.isHidden(team.id)}
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
                onpointerdown={(e) => onHandleDown(e, team.id)}
                onpointermove={onHandleMove}
                onpointerup={onHandleUp}
                onpointercancel={onHandleUp}
                onkeydown={(e) => onHandleKey(e, team.id)}
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
                onclick={() => teamRail.toggleHidden(team.id)}
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

      {#if unfollowed.length > 0}
        <p class="px-2 pb-1 pt-2 text-xs font-medium uppercase tracking-wide text-fg-muted">
          {t('teams.rail_section_all')}
        </p>
        <ul data-testid="teams-rail-all-list">
          {#each unfollowed as team (team.id)}
            {@const hidden = teamRail.isHidden(team.id)}
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
                onclick={() => teamRail.toggleHidden(team.id)}
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

      {#if followed.length === 0 && unfollowed.length === 0}
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
