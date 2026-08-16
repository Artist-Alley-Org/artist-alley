<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Spreadsheet-style list view for the browse feed.
  //
  // Renders posts as rows in a CSS grid (grid-template-columns from
  // each column's track). Sort runs client-side against the loaded
  // items — server-side sort lands when /posts grows a `sort` param.
  // Column visibility and column WIDTHS are both owned by the
  // browseView store; this component owns the drag gesture only.

  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { browseView, columnMinPx, type ListColumnDef } from '$stores/browseView.svelte';
  import { selection } from '$stores/selection.svelte';
  import { auth } from '$stores/auth.svelte';
  import { site } from '$stores/site.svelte';
  import { t } from '$stores/lang.svelte';
  import ColumnPicker from '$components/ColumnPicker.svelte';

  interface AssetSummary {
    id: string;
    file_hash?: string | null;
  }
  interface PostMember {
    asset_id: string;
    sort_order: number;
    /** ⚠️ OPTIONAL, and this table never reads it (#1137).
     *
     *  It was declared REQUIRED, which was false twice over. On the
     *  wire it is absent for a member the caller may not see — #883
     *  omits the object and sets `restricted` instead — so a table
     *  demanding it was demanding something the API does not always
     *  send. And nothing here consults it: the row thumbnail resolves
     *  from `cover_asset_id` or `members[0].asset_id`, both of which
     *  are present either way.
     *
     *  The cost of that false requirement was not hypothetical. It is
     *  what made this table unusable from the collection page, whose
     *  own (correct) member type has `asset?` — TypeScript reported a
     *  shape mismatch between two descriptions of the SAME payload, and
     *  the surface went without a list view rather than with a cast.
     *  That is the #1099 trap from the component side: a local
     *  interface that over-declares is a compatibility barrier around a
     *  component that was always compatible. Declare from the schema,
     *  not from convenience. */
    asset?: AssetSummary;
  }
  // The renderable identity behind `author_user_ref` (#557). Already on
  // every /posts payload — `enrichAuthors` stamps it per request from
  // `users.LookupAuthors`, which resolves `display_name` through
  // `users.ResolveDisplayName`: the ONE home of the display-name ladder,
  // with its authenticated rung (fullname) and its anonymous arm.
  //
  // ⛔ Do not re-derive a name here. #1023 exists because that ladder
  // had been transcribed four times and three copies were wrong; a
  // `display_name || fullname || username` in this component would be
  // the fifth. Render what the server resolved.
  //
  // OPTIONAL, and the absence is meaningful: an author who took ADR
  // 0024's opt-out is omitted for anonymous callers rather than
  // returned redacted, as is a hard-deleted account. So "no author
  // object" means "no name to show", never "look it up another way".
  interface PostAuthor {
    ref: number;
    username: string;
    display_name: string;
    avatar_url?: string | null;
  }
  interface Post {
    id: string;
    // Kept alongside `author` — it is the stable identifier the row
    // needs for a profile link, and it is present even when the
    // identity is withheld.
    author_user_ref: number;
    author?: PostAuthor | null;
    title: string;
    description: string;
    visibility: string;
    cover_asset_id?: string | null;
    posted_at: string;
    like_count: number;
    comment_count: number;
    tags: string[];
    members: PostMember[];
    created_at: string;
    updated_at: string;
  }

  interface Props {
    items: Post[];
    loading?: boolean;
    /** Feed-order ids for range selection (#1127).
     *
     *  ⚠️ Deliberately NOT used as this table's range order. The list
     *  view SORTS client-side, so the sequence on screen is
     *  `sortedItems`, not the feed — and "everything between these two
     *  rows" has to mean the rows between them, or Shift+click selects
     *  things the reader cannot see between the two they clicked.
     *  Accepted so the browse page can hand one prop to both branches
     *  of ContentGrid; see `rangeOrder` below. */
    orderedIds?: () => string[];
  }
  let { items, loading = false }: Props = $props();

  // ── Column-value getter. Returns a sort key (string | number) for
  // header click sorting. Rendering is handled inline in the template
  // because the JSX-style shape would require many helper components.
  function getValue(post: Post, colId: string): string | number | null {
    switch (colId) {
      case 'title':       return post.title?.toLowerCase() ?? '';
      // Sorts by what the eye sees. It sorted by `author_user_ref`
      // until #1099 — a number the column never displayed, so clicking
      // the header reordered the table by an invisible value (roughly
      // account age). A withheld author sorts as '' and groups
      // together, which is the only honest place for a row with no
      // name.
      case 'author':      return (post.author?.display_name ?? '').toLowerCase();
      case 'visibility':  return post.visibility ?? '';
      case 'tags':        return (post.tags ?? []).join(', ').toLowerCase();
      case 'members':     return post.members?.length ?? 0;
      case 'likes':       return post.like_count ?? 0;
      case 'comments':    return post.comment_count ?? 0;
      case 'posted_at':   return post.posted_at ?? '';
      case 'description': return post.description?.toLowerCase() ?? '';
      default:            return null;
    }
  }

  const sortedItems = $derived.by(() => {
    const { col, dir } = browseView.sort;
    if (!col) return items;
    const factor = dir === 'asc' ? 1 : -1;
    const copy = items.slice();
    copy.sort((a, b) => {
      const va = getValue(a, col);
      const vb = getValue(b, col);
      if (va === null && vb === null) return 0;
      if (va === null) return 1 * factor;
      if (vb === null) return -1 * factor;
      if (typeof va === 'number' && typeof vb === 'number') return (va - vb) * factor;
      return String(va).localeCompare(String(vb)) * factor;
    });
    return copy;
  });

  const visibleColumns = $derived(browseView.visibleColumns);

  // Build the CSS grid-template-columns value. Each track is the user's
  // dragged width if they have set one, otherwise the registry default
  // — the store resolves that, so hiding and re-showing a column keeps
  // whatever width its owner left it at (#1100).
  const gridTemplate = $derived(visibleColumns.map((c) => browseView.columnTrack(c)).join(' '));

  // ── Column resizing (#1100) ────────────────────────────────────────
  //
  // The handle is a SIBLING of the header cell's button, not a child.
  // Nesting a focusable separator inside the sort <button> would be
  // invalid (interactive content inside a button), and worse, every
  // press on the handle would also fire the sort. So each header cell
  // is a positioned wrapper carrying `role="columnheader"`, with the
  // sort control and the handle side by side inside it.
  //
  // The gesture uses POINTER CAPTURE rather than window listeners: the
  // handle keeps receiving move/up events even when the pointer leaves
  // it, which is the entire point of a drag, and there is no global
  // state to leak if the component unmounts mid-drag.

  /** How far one arrow press moves an edge. 16px is a visible step
   *  without making the keyboard path take fifty presses to cross a
   *  column; Shift multiplies it for coarse adjustment. */
  const KEY_STEP_PX = 16;
  const KEY_STEP_LARGE_PX = 64;

  let drag = $state<{ id: string; startX: number; startW: number } | null>(null);

  /** The rendered width of the cell this handle belongs to. Measured
   *  from the DOM rather than read from the store, because a column
   *  nobody has dragged yet has no stored width at all — its track is
   *  an `fr` or a rem, and the number the drag has to start from is
   *  what the browser actually painted. */
  function cellWidth(handle: HTMLElement): number | null {
    const cell = handle.parentElement;
    return cell ? cell.getBoundingClientRect().width : null;
  }

  function startResize(e: PointerEvent, col: ListColumnDef) {
    const handle = e.currentTarget as HTMLElement;
    const w = cellWidth(handle);
    if (w === null) return;
    // Stop the press from reaching the header (text selection, and the
    // sort control next door) — but only the press. Nothing about the
    // rest of the page is prevented.
    e.preventDefault();
    e.stopPropagation();
    drag = { id: col.id, startX: e.clientX, startW: w };
    handle.setPointerCapture(e.pointerId);
  }

  function moveResize(e: PointerEvent) {
    if (!drag) return;
    browseView.setColumnWidth(drag.id, drag.startW + (e.clientX - drag.startX));
  }

  function endResize(e: PointerEvent) {
    if (!drag) return;
    const handle = e.currentTarget as HTMLElement;
    if (handle.hasPointerCapture?.(e.pointerId)) handle.releasePointerCapture(e.pointerId);
    drag = null;
  }

  /** Keyboard parity for the drag.
   *
   *  Arrows move the edge; `Home` takes the column to its floor, which
   *  is the splitter convention. `Enter` RESETS to the registry
   *  default — the keyboard twin of double-clicking the handle, and the
   *  closest thing the splitter pattern has to a defined key for
   *  "return to the position you started from". Without it the double
   *  click is a mouse-only escape hatch and a keyboard user who drags a
   *  column to 80px has no way back. */
  function onHandleKey(e: KeyboardEvent, col: ListColumnDef) {
    const handle = e.currentTarget as HTMLElement;
    const w = cellWidth(handle);
    if (w === null) return;
    const step = e.shiftKey ? KEY_STEP_LARGE_PX : KEY_STEP_PX;
    if (e.key === 'ArrowLeft') {
      e.preventDefault();
      browseView.setColumnWidth(col.id, w - step);
    } else if (e.key === 'ArrowRight') {
      e.preventDefault();
      browseView.setColumnWidth(col.id, w + step);
    } else if (e.key === 'Home') {
      e.preventDefault();
      browseView.setColumnWidth(col.id, columnMinPx(col.id));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      browseView.resetColumnWidth(col.id);
    }
  }

  function alignClass(c: ListColumnDef): string {
    return c.align === 'right' ? 'text-right justify-end' : c.align === 'center' ? 'text-center justify-center' : 'text-left justify-start';
  }

  // Pick the asset id to render the row thumbnail. Mirrors PostCard's
  // fallback chain: explicit cover → first member → none.
  function coverAssetId(post: Post): string | null {
    if (post.cover_asset_id) return post.cover_asset_id;
    return post.members?.[0]?.asset_id ?? null;
  }

  // Use the raw /file endpoint — it returns the original bytes for
  // any asset with a stored file. Variant URLs (col, thumb, etc.)
  // 404 until the image pipeline (Phase 1.15) generates them.
  function thumbUrl(post: Post): string | null {
    const id = coverAssetId(post);
    return id ? `/api/v1/assets/${id}/file` : null;
  }

  // Svelte action: hides the <img> on load failure (404, corrupt
  // bytes, network error). Direct DOM mutation — since the action
  // never updates and src isn't re-derived after mount, Svelte's
  // reactive rebinding never overwrites the inline `display:none`.
  // alt="" + display:none means broken images leave no glyph
  // behind; the parent cell's background shows through cleanly.
  function hideOnError(node: HTMLImageElement) {
    const onError = () => { node.style.display = 'none'; };
    node.addEventListener('error', onError);
    return { destroy() { node.removeEventListener('error', onError); } };
  }

  function fmtDate(s: string): string {
    if (!s) return '';
    const d = new Date(s);
    if (Number.isNaN(d.getTime())) return s;
    return d.toLocaleString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
  }

  async function openPost(id: string) {
    const target = new URL(page.url);
    target.searchParams.set('post', id);
    await goto(target.pathname + target.search, { keepFocus: true, noScroll: true });
  }

  // ── Selection (#1127) ──────────────────────────────────────────────
  //
  // The list gains the checkbox column the other four views have had
  // since #515, plus the desktop-list keyboard idiom: Space toggles the
  // focused row, Shift+Space extends from the anchor, arrows walk.
  //
  // THE RANGE ORDER IS THE SORTED ORDER, not the feed's. This is the one
  // view where the two differ: click "Title" and the wall re-orders
  // client-side, so a Shift+click range computed against feed order
  // would select rows scattered through the table rather than the block
  // the reader dragged across. Everywhere else the two are identical and
  // the distinction never comes up.
  const rangeOrder = () => sortedItems.map((p) => p.id);

  const canSelect = $derived(!!auth.user && !site.demoMode);

  function toggleRow(id: string, shift: boolean) {
    if (shift) {
      selection.extendTo(id, rangeOrder());
      return;
    }
    selection.toggle(id);
    selection.setAnchor(id);
  }

  /** Move focus by `delta` rows, wrapping at neither end.
   *
   *  Focus moves WITHOUT changing the selection, which is the
   *  desktop-list convention the issue names: arrows navigate, Space
   *  commits. (The other convention — arrows also select — makes it
   *  impossible to reach a row without selecting everything on the way,
   *  and there is no modifier here to escape it with.) */
  function moveFocus(from: string, delta: number, el: HTMLElement) {
    const order = rangeOrder();
    const i = order.indexOf(from);
    const next = i + delta;
    if (i < 0 || next < 0 || next >= order.length) return;
    const root = el.closest('[data-list-rows]');
    root?.querySelector<HTMLElement>(`[data-row-id="${CSS.escape(order[next])}"]`)?.focus();
  }

  function onRowKey(e: KeyboardEvent, id: string) {
    const el = e.currentTarget as HTMLElement;
    switch (e.key) {
      case ' ':
      case 'Spacebar':
        // Space is the SELECT key on a grid row, not the activate key —
        // that is Enter. Prevented because Space on a focusable div also
        // scrolls the page.
        if (!canSelect) return;
        e.preventDefault();
        toggleRow(id, e.shiftKey);
        break;
      case 'Enter':
        e.preventDefault();
        void openPost(id);
        break;
      case 'ArrowDown':
        e.preventDefault();
        moveFocus(id, 1, el);
        break;
      case 'ArrowUp':
        e.preventDefault();
        moveFocus(id, -1, el);
        break;
    }
  }
</script>

<div class="space-y-2">
  <!-- Toolbar: row count on the left, column picker on the right. -->
  <div class="flex items-center justify-between gap-2">
    <p class="text-xs text-fg-muted">
      {sortedItems.length} {sortedItems.length === 1 ? t('browse.list.post_singular') : t('browse.list.post_plural')}
    </p>
    <ColumnPicker />
  </div>

  <!-- Table. Uses CSS grid so column widths follow the registry without
       needing colgroup math. Header is sticky to the section scroll
       container. -->
  <div class="overflow-hidden rounded-lg border border-border bg-surface-elevated">
    <!-- `role="grid"`, added with #1127's selection column. The rows
         below are focusable `role="row"` elements with `aria-selected`,
         and `row` is only valid inside a table/grid/treegrid — this
         markup carried `role="row"` and `role="cell"` with NO such
         ancestor, so the roles were being dropped. Naming the container
         is what makes the whole keyboard idiom (arrows, Space,
         Shift+Space) legible to a screen reader rather than a set of
         divs that happen to respond to keys. -->
    <div class="overflow-x-auto" role="grid" aria-rowcount={sortedItems.length}>
      <!-- Header row -->
      <div
        class="sticky top-0 z-10 grid border-b border-border bg-surface-elevated text-xs font-semibold text-fg-muted uppercase tracking-wide"
        style="grid-template-columns: {gridTemplate}"
        role="row"
      >
        {#each visibleColumns as col, i (col.id)}
          {@const active = browseView.sort.col === col.id}
          <!-- The grid item is a positioned WRAPPER, and the
               `columnheader` role rides it rather than the control
               inside — see the resize comments in the script block for
               why the handle cannot live inside the sort button. -->
          <div
            class="relative flex min-w-0 border-r border-border-subtle"
            role="columnheader"
            aria-sort={!col.sortable
              ? undefined
              : active
                ? browseView.sort.dir === 'asc'
                  ? 'ascending'
                  : 'descending'
                : 'none'}
          >
            {#if col.sortable}
              <button
                type="button"
                onclick={() => browseView.cycleSort(col.id)}
                class={`flex min-w-0 flex-1 items-center gap-1 px-3 py-2.5 ${alignClass(col)} ${active ? 'text-fg' : 'hover:text-fg'}`}
              >
                <span class="truncate">{t(col.labelKey)}</span>
                {#if active}
                  <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
                    {#if browseView.sort.dir === 'asc'}
                      <polyline points="6 15 12 9 18 15" />
                    {:else}
                      <polyline points="6 9 12 15 18 9" />
                    {/if}
                  </svg>
                {/if}
              </button>
            {:else}
              <div class={`flex min-w-0 flex-1 items-center px-3 py-2.5 ${alignClass(col)}`}>
                <!-- The selection column's label is VISUALLY HIDDEN, not
                     absent (#1127). At its fixed 44px "Select" renders
                     as "S…", which is noise where a column of checkboxes
                     needs no caption — the convention in every desktop
                     list is a blank header there. The string stays in the
                     accessibility tree, because `role="columnheader"`
                     with no accessible name is a column a screen reader
                     cannot announce when walking the row. -->
                <span class={col.id === 'select' ? 'sr-only' : 'truncate'}>{t(col.labelKey)}</span>
              </div>
            {/if}

            <!-- Resize handle on this column's trailing edge. Not on the
                 LAST visible column: there is no boundary there, only
                 the table's own edge, and a handle that drags the table
                 wider than its container is a scroll bar with extra
                 steps.

                 ⛔ HIDDEN UNDER A COARSE POINTER, deliberately. A 8px
                 drag target on a touch screen is not a control, it is a
                 trap laid across the header of a table whose columns
                 are already reduced at that width — and its two
                 neighbours are the sort control and the next column's
                 sort control, so every miss does something. `display:
                 none` also takes it out of the tab order, so the
                 keyboard path disappears with it rather than leaving a
                 focusable target nobody can see. The list view is
                 readable on a phone and configured on a desktop.

                 `-right-1` centres the grab zone ON the border rather
                 than beside it, which is where the pointer aims. -->
            {#if i < visibleColumns.length - 1 && col.resizable !== false}
              <!-- The two suppressions are the ARIA "window splitter"
                   pattern, which svelte-check does not model: a
                   `separator` is non-interactive UNTIL it is focusable,
                   at which point it is a widget and takes both a
                   tabindex and key handlers. AssetPlaylist's strip
                   resizer is the same role with the same first
                   suppression — it simply never became focusable, so it
                   is mouse-only. This one is not. -->
              <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
              <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
              <div
                role="separator"
                aria-orientation="vertical"
                aria-label={t('browse.resize_column')}
                aria-valuenow={browseView.columnWidths[col.id]}
                aria-valuemin={columnMinPx(col.id)}
                tabindex="0"
                class="group/handle absolute inset-y-0 -right-1 z-20 flex w-2 cursor-col-resize
                       touch-none items-stretch justify-center focus-visible:outline-none
                       [@media(pointer:coarse)]:hidden"
                data-testid={`list-col-resize-${col.id}`}
                onpointerdown={(e) => startResize(e, col)}
                onpointermove={moveResize}
                onpointerup={endResize}
                onpointercancel={endResize}
                ondblclick={() => browseView.resetColumnWidth(col.id)}
                onkeydown={(e) => onHandleKey(e, col)}
              >
                <!-- ⛔ THE RESTING GRIP (owner rider on #1127).
                     The handle used to paint NOTHING at rest — no
                     background, no rule — so the only thing marking a
                     draggable edge was the cell's own
                     `border-border-subtle`, the quietest divider in the
                     system. Measured in dark mode that is
                     oklch(22%) against an oklch(~20%) header: a line you
                     can find with the cursor and not with the eye. The
                     report named dark mode; light was the same defect at
                     oklch(92% vs 97%), which is why both are fixed here
                     rather than only the half that was reported.

                     `border-strong` is the correct token and app.css
                     says why: it is the AFFORDANCE tier, the boundary
                     that IS a control, specified to clear 3:1 against
                     every surface in BOTH themes — which is exactly what
                     a drag target is under WCAG 1.4.11. `border` and
                     `border-subtle` are explicitly documented as
                     carrying no information, and are therefore the wrong
                     tier for the one line in the header that does.

                     A 1px rule rather than washing the whole 8px zone:
                     the grab area is deliberately wider than the visual
                     edge (it straddles the border so the pointer can aim
                     at what it sees), and painting all 8px at rest would
                     put a fat bar between every pair of columns. -->
                <span
                  aria-hidden="true"
                  class="w-px self-stretch transition-colors
                         {drag?.id === col.id
                    ? 'bg-accent'
                    : 'bg-border-strong group-hover/handle:bg-accent group-focus-visible/handle:bg-accent'}"
                ></span>
              </div>
            {/if}
          </div>
        {/each}
      </div>

      <!-- Body rows.
           ⚠️ THE ROW IS NO LONGER A <button> (#1127). It could not stay
           one: the selection column puts a checkbox inside every row,
           and interactive content inside a button is invalid HTML and
           unreachable by keyboard — the very trap the `author` cell's
           own comment names as the reason it has no profile link.

           So the row is a focusable `role="row"` inside a `role="grid"`,
           which is the ARIA pattern for a tabular widget whose rows are
           both activatable and selectable, and the pattern the keyboard
           idiom (#1127 asks for Space / Shift+Space / arrows) is defined
           against. Enter opens, matching what the button did on Enter
           and Space; Space now selects instead, which is the grid
           convention and the reason the row had to stop being a button
           rather than an accident of it. -->
      <div data-list-rows>
      {#each sortedItems as post (post.id)}
        {@const selected = selection.has(post.id)}
        <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
        <div
          role="row"
          tabindex="0"
          data-row-id={post.id}
          data-select-id={post.id}
          aria-selected={canSelect ? selected : undefined}
          onclick={(e) => {
            // Shift+click selects rather than opens, exactly as it does
            // on a card (#1127). Checked before the open, or the modal
            // would be halfway up before the selection landed.
            if (e.shiftKey && canSelect) {
              e.preventDefault();
              toggleRow(post.id, true);
              return;
            }
            void openPost(post.id);
          }}
          onkeydown={(e) => onRowKey(e, post.id)}
          class="grid w-full cursor-pointer border-b border-border-subtle text-left text-sm transition-colors
                 hover:bg-state-hover focus-visible:bg-state-hover focus-visible:outline-none
                 {selected ? 'bg-accent-container/40' : ''}"
          style="grid-template-columns: {gridTemplate}"
        >
          {#each visibleColumns as col (col.id)}
            <div
              class={`flex items-center border-r border-border-subtle px-3 py-2 ${alignClass(col)} ${col.align === 'right' ? 'font-mono-tabular tabular-nums' : ''} truncate`}
              role="gridcell"
            >
              {#if col.id === 'select'}
                <!-- Gated exactly as CardCheckbox is — same two
                     conditions, so the list cannot offer a selection the
                     other four views withhold. The cell stays (the grid
                     template reserves its track either way) so the
                     columns still line up with the header. -->
                {#if canSelect}
                  <button
                    type="button"
                    role="checkbox"
                    aria-checked={selected}
                    tabindex="-1"
                    aria-label={selected ? t('card.select.deselect') : t('card.select.label')}
                    data-testid="list-row-checkbox"
                    onclick={(e) => {
                      // The row's own onclick would open the post
                      // underneath this one.
                      e.preventDefault();
                      e.stopPropagation();
                      toggleRow(post.id, e.shiftKey);
                    }}
                    class="grid h-6 w-6 shrink-0 cursor-pointer place-items-center rounded border-2
                           transition-colors {selected
                      ? 'border-accent bg-accent text-on-accent'
                      : 'border-border-strong bg-surface text-transparent hover:border-accent'}"
                  >
                    <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                      <polyline points="20 6 9 17 4 12" />
                    </svg>
                  </button>
                {/if}
                <!-- `tabindex="-1"`: the ROW is the tab stop, and the
                     checkbox is reached with Space from it. Two stops per
                     row would double the length of the tab path through
                     a 36-row table to reach the same two actions. -->
              {:else if col.id === 'thumbnail'}
                {@const url = thumbUrl(post)}
                <!-- Cell renders a sized placeholder square; the <img>
                     sits on top and either fills it on load or hides
                     itself on error, letting the placeholder show. -->
                <div class="relative h-8 w-8 shrink-0 overflow-hidden rounded border border-border bg-surface">
                  {#if url}
                    <img
                      src={url}
                      alt=""
                      loading="lazy"
                      class="absolute inset-0 h-full w-full object-cover"
                      use:hideOnError
                    />
                  {/if}
                </div>
              {:else if col.id === 'title'}
                <span class="truncate text-fg" title={post.title || ''}>{post.title || t('browse.list.untitled')}</span>
              {:else if col.id === 'author'}
                <!-- The name, not the ref (#1099). `@14` was what this
                     rendered before, because the component's local Post
                     type omitted `author` — the payload has carried the
                     resolved identity since #557.

                     No nested link to the profile: the whole row is a
                     <button> that opens the post, and interactive
                     content inside a button is invalid and unreachable
                     by keyboard. The handle rides the tooltip instead,
                     and `author_user_ref` stays on the type for
                     whenever the row layout can host a real link. -->
                <span
                  class="truncate text-fg-muted"
                  title={post.author ? '@' + post.author.username : ''}
                >{post.author?.display_name || '—'}</span>
              {:else if col.id === 'visibility'}
                <span class="rounded bg-surface px-1.5 py-0.5 text-xs text-fg-muted capitalize">{post.visibility}</span>
              {:else if col.id === 'tags'}
                <span class="truncate text-fg-muted" title={post.tags?.join(', ')}>
                  {post.tags?.length ? post.tags.join(', ') : '—'}
                </span>
              {:else if col.id === 'members'}
                <span class="tabular-nums text-fg-muted">{post.members?.length ?? 0}</span>
              {:else if col.id === 'likes'}
                <span class="tabular-nums text-fg-muted">{post.like_count ?? 0}</span>
              {:else if col.id === 'comments'}
                <span class="tabular-nums text-fg-muted">{post.comment_count ?? 0}</span>
              {:else if col.id === 'posted_at'}
                <span class="tabular-nums text-fg-muted">{fmtDate(post.posted_at)}</span>
              {:else if col.id === 'description'}
                <span class="truncate text-fg-muted" title={post.description}>
                  {post.description || '—'}
                </span>
              {/if}
            </div>
          {/each}
        </div>
      {/each}
      </div>

      {#if loading}
        {#each Array(4) as _, i (i)}
          <div
            class="grid border-b border-border-subtle"
            style="grid-template-columns: {gridTemplate}"
            aria-hidden="true"
          >
            {#each visibleColumns as col (col.id)}
              <div class="border-r border-border-subtle px-3 py-2">
                <div class="h-4 w-3/4 animate-pulse rounded bg-surface"></div>
              </div>
            {/each}
          </div>
        {/each}
      {/if}
    </div>
  </div>
</div>
