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
  import { t } from '$stores/lang.svelte';
  import ColumnPicker from '$components/ColumnPicker.svelte';

  interface AssetSummary {
    id: string;
    file_hash?: string | null;
  }
  interface PostMember {
    asset_id: string;
    sort_order: number;
    asset: AssetSummary;
  }
  interface Post {
    id: string;
    author_user_ref: number;
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
  }
  let { items, loading = false }: Props = $props();

  // ── Column-value getter. Returns a sort key (string | number) for
  // header click sorting. Rendering is handled inline in the template
  // because the JSX-style shape would require many helper components.
  function getValue(post: Post, colId: string): string | number | null {
    switch (colId) {
      case 'title':       return post.title?.toLowerCase() ?? '';
      case 'author':      return post.author_user_ref;
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
    <div class="overflow-x-auto">
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
                <span class="truncate">{t(col.labelKey)}</span>
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
            {#if i < visibleColumns.length - 1}
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
                class="absolute inset-y-0 -right-1 z-20 w-2 cursor-col-resize touch-none
                       hover:bg-accent/50 focus-visible:bg-accent focus-visible:outline-none
                       [@media(pointer:coarse)]:hidden"
                class:bg-accent={drag?.id === col.id}
                data-testid={`list-col-resize-${col.id}`}
                onpointerdown={(e) => startResize(e, col)}
                onpointermove={moveResize}
                onpointerup={endResize}
                onpointercancel={endResize}
                ondblclick={() => browseView.resetColumnWidth(col.id)}
                onkeydown={(e) => onHandleKey(e, col)}
              ></div>
            {/if}
          </div>
        {/each}
      </div>

      <!-- Body rows -->
      {#each sortedItems as post (post.id)}
        <button
          type="button"
          onclick={() => openPost(post.id)}
          class="grid w-full border-b border-border-subtle text-left text-sm transition-colors hover:bg-state-hover focus-visible:bg-state-hover focus-visible:outline-none"
          style="grid-template-columns: {gridTemplate}"
          role="row"
        >
          {#each visibleColumns as col (col.id)}
            <div
              class={`flex items-center border-r border-border-subtle px-3 py-2 ${alignClass(col)} ${col.align === 'right' ? 'font-mono-tabular tabular-nums' : ''} truncate`}
              role="cell"
            >
              {#if col.id === 'thumbnail'}
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
                <span class="truncate text-fg-muted">@{post.author_user_ref}</span>
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
        </button>
      {/each}

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
