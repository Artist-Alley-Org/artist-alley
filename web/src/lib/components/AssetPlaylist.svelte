<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // AssetPlaylist — generic viewer shell.
  //
  // Takes a source-agnostic PlaylistSource and renders:
  //   - the AssetViewer pinned to source.items[source.cursor]
  //   - bottom thumb-strip filmstrip for cursor navigation
  //   - floating prev/next arrows + position indicator
  //   - keyboard navigation (← / →, i, Esc)
  //   - close button (× for overlay, back-arrow for standalone)
  //   - host-supplied contextSlot threaded into AssetViewer's
  //     metadataSlot (used for post-as-playlist's author header,
  //     collection-as-playlist's description, etc.)
  //   - host-supplied toolbarActions threaded into the top bar
  //
  // Filmstrip + nav arrows + position indicator AUTO-HIDE when
  // items.length <= 1 — that makes a "playlist of 1" (a standalone
  // asset link) collapse to the same shell with no chrome wasted.
  //
  // The shell owns: dialog plumbing, cursor state, keyboard nav,
  // pane-collapsed persistence, review-mode toggle, strip-collapsed
  // persistence. The shell does NOT own: data fetching, social
  // metadata, comments — those belong to the host.
  //
  // Renamed + carved out from the original PostModal (commit history
  // in feat/viewer-polish). The "post" surface is now a thin host
  // (PostHost.svelte) that builds a PostPlaylistSource and provides
  // a contextSlot for the post-specific sidebar (author / likes /
  // comments / tags / edit / delete / etc).

  import { onDestroy, onMount, tick, type Snippet } from 'svelte';
  import AssetViewer from './viewers/AssetViewer.svelte';
  import { kindForAsset } from './viewers/controller';
  import { snippetToolHookKey, type ToolDef } from './viewers/tools/contract';
  import type { WhiteboardSession } from '$lib/whiteboard/session.svelte';
  import type { PlaylistSource } from '$lib/playlist/types';
  import { t } from '$stores/lang.svelte';
  import { auth } from '$stores/auth.svelte';
  import { chromeScroll } from '$stores/chromeScroll.svelte';
  import RequestAccessDialog from './RequestAccessDialog.svelte';
  import ConfirmDeleteDialog from './ConfirmDeleteDialog.svelte';
  import { toasts } from '$stores/toasts.svelte';
  import { canDelete, deleteEntity, restoreEntity, shouldAskReason } from '$lib/deletable';

  interface Props {
    source: PlaylistSource;
    /** Centered title bar content. Rendered in the middle zone of the
        top toolbar (between the File/Edit/About menus and the window
        controls), replacing the default filename strip. Post hosts
        pass a snippet rendering "<post title> — by <author>"; other
        hosts can omit and fall back to the filename. */
    titleSlot?: Snippet;
    /** Pass-through to AssetViewer's canvasOverlay slot — host
        renders a brush/annotation surface over the asset canvas
        without losing the sidebar or top toolbar. */
    canvasOverlay?: Snippet;
    /** Called when the user closes the playlist (× / ESC / backdrop).
     *
     *  May be async, and the standalone routes' is: /assets/{id} and
     *  /posts/{id} both pass the close-to-origin policy, which `goto`s.
     *  Anything that must happen AFTER the surface is really gone has
     *  to await it — see confirmDelete, where not awaiting left the
     *  delete toast parented into a dialog that was already unmounting
     *  and rendered nowhere. */
    onClose: () => void | Promise<void>;
    /** True when the playlist is a full-page route (e.g. /posts/[id])
        rather than an overlay over the browse feed. Drives the close
        button affordance — back-arrow vs ×. */
    standalone?: boolean;
    /** Optional sibling-playlist navigator. ← / → call this with
        the requested direction so the shell can jump to the next /
        previous *playlist* in the surrounding context (next post in
        the browse feed, next collection in a curator's stash, next
        search result, etc). Hosts that lack sibling context (e.g.
        the standalone /posts/{id} route hit by direct nav) omit
        the prop and ← / → become no-op. Within-playlist asset
        navigation stays bound to A / D. */
    onNavigateSibling?: (dir: 'prev' | 'next') => void;
    /** Optional per-asset host hooks threaded into AssetViewer's
        Edit / File menus. The host supplies these to wire actions
        that target the *current* asset (the one the cursor is on).
        When omitted the corresponding menu item stays disabled with
        a "coming soon" tooltip — keeps the menu shape stable across
        hosts that don't yet implement a given action. */
    onAddToCollection?: (assetId: string) => void;
    onRecreatePreviews?: (assetId: string) => void;
    onEditTags?: (assetId: string) => void;
    onEditMetadata?: (assetId: string) => void;
    onDownloadVariant?: (assetId: string) => void;
    onShareAsset?: (assetId: string) => void;
    /** Extra rows to append to the global TipsSection footer at the
        bottom of the side panel. Hosts that own mode-specific tool
        surfaces (whiteboard, annotation, future review modes) pass
        a snippet that renders `<dt>/<dd>` rows + an uppercase
        section header so every tool's tips stay in one consolidated
        reference inside the shell's single Tips footer. */
    extraTips?: Snippet;
    /** Host-injected tools — appended to the registry at shell
        mount. Hosts that own rich detail surfaces (PostHost's post
        details / likes / comments / cover-picker) register their
        own ToolDef with the right order. The built-in Details
        tool stays in the dropdown alongside. */
    customTools?: ToolDef[];
    /** Whiteboard session passed through to the WhiteboardTool
        when the host has wired one (post-anchored today). */
    whiteboardSession?: WhiteboardSession;
    /** Host hook bag forwarded into the ToolContext. See
        AssetViewer's prop docs for the conventional namespaces
        (hostHooks.whiteboard, hostHooks.details, ...). */
    hostHooks?: Record<string, unknown>;
  }

  let {
    source,
    titleSlot,
    canvasOverlay,
    onClose,
    standalone = false,
    onNavigateSibling,
    onAddToCollection,
    onRecreatePreviews,
    onEditTags,
    onEditMetadata,
    onDownloadVariant,
    onShareAsset,
    extraTips,
    customTools = [],
    whiteboardSession,
    hostHooks,
  }: Props = $props();

  // ---- Local state ---------------------------------------------------------

  let dialogEl: HTMLDialogElement | undefined = $state();

  // Review mode — passed through to AssetViewer. When on, the viewer
  // captures input (orbit / pan / scrub) and swaps the right pane to
  // its kind-aware tools panel. Toggled by the Review button or by
  // double-clicking the asset.

  // Pane open/closed state for AssetViewer's right pane. Bindable
  // through so we can drive it from the 'i' hotkey here and persist
  // it across sessions. Default open; restored from localStorage.
  let paneCollapsed = $state(false);

  // Footer thumb strip collapsed state. Persists per-browser. The
  // collapsed state shows just a chevron + "n / total" so the
  // viewer always knows where they are even with the strip hidden.
  let stripCollapsed = $state(false);

  // Footer thumb-strip height in CSS pixels (expanded state only).
  // The user can drag the top edge of the strip up/down to resize.
  // Capped at 25% of viewport height so it never eats the viewer.
  // Floor matches the default — shrinking below that is what the
  // collapse chevron is for. Persists per-browser via localStorage.
  const STRIP_MIN = 96;
  const STRIP_DEFAULT = 96;
  let stripHeight = $state(STRIP_DEFAULT);
  function stripMax(): number {
    // Re-evaluated on every drag start + window resize — 25vh tracks
    // the live viewport, so resizing the browser doesn't trap the
    // user with a strip that's now too tall to fit.
    return Math.floor(window.innerHeight * 0.25);
  }

  // Window-chrome state. The shell renders like a modern app window
  // (Photoshop / VS Code / Figma vibe): rounded corners, drop
  // shadow, sits under the global navbar so links stay reachable.
  // The maximize button covers the navbar for a full-bleed view.
  //
  // Defaults:
  //   - standalone (full-page /posts/{id})  → maximized
  //   - overlay    (?post= over the feed)   → windowed
  //
  // Persisted per-browser so a user who prefers maximized always
  // gets it back on the next open. Standalone never falls below
  // maximized=true since "windowed" inside a route with no
  // underlying page is a worse UX.
  let maximized = $state<boolean>(standalone);

  // ---- Derived -------------------------------------------------------------

  const currentItem = $derived(source.items[source.cursor] ?? null);
  const hasMultipleItems = $derived(source.items.length > 1);

  // #918 — the host's Details body, reached directly rather than through
  // AssetViewer's tool registry, for the one state where no AssetViewer
  // exists to carry it: a playlist with nothing in it. See the empty
  // branch in the markup below. Undefined for hosts that contribute no
  // details pane, which then get the bare message they always had.
  const emptyHostPane = $derived(
    (hostHooks?.[snippetToolHookKey('details')] as { body?: Snippet } | undefined)?.body,
  );
  // When every member of the playlist is an audiobook (.m4b /
  // asset_type=11), the AudiobookTool's Tracks section in the side
  // panel becomes the canonical navigator and the strip below
  // duplicates it (every MP3 gets its own waveform thumb). Hide
  // the strip in that case; ↑/↓ + the Tracks list both still
  // navigate so the user loses no muscle memory.
  const allAudiobook = $derived(
    source.items.length > 0
    && source.items.every((it) => {
      const a = it.asset;
      if (a?.asset_type === 11) return true;
      const ext = (a?.file_extension ?? '').toLowerCase().replace(/^\./, '');
      return ext === 'm4b' || ext === 'aax';
    }),
  );
  // Current asset's kind drives the contextual hotkey legend below —
  // we surface different shortcut sections for video/audio (timeline
  // playback + loop) vs static kinds. Falls back to 'placeholder'
  // when no item is mounted yet.
  const currentKind = $derived(
    currentItem ? kindForAsset(currentItem.asset) : 'placeholder',
  );
  const isTimelineKind = $derived(currentKind === 'video' || currentKind === 'audio');

  // Windowed mode's top edge is pure CSS: --aa-chrome-bottom, published
  // by the root layout from the measured chrome layer (banners +
  // header, auto-hide folded in). See the .windowed rule at the bottom
  // of this file, and +layout.svelte for why the layout owns it. There
  // is deliberately no measuring code here — this component used to run
  // its own ResizeObserver on <header> and read the header's HEIGHT,
  // which is the chrome's bottom edge only while the header is the
  // topmost bar; the demo banner pushed it down and the viewer's own top
  // chrome ended up behind the navbar (#688).

  // ---- Lifecycle -----------------------------------------------------------

  // Audiobook auto-advance bridge — AudiobookView fires this when
  // the current track ends and there's a next sibling. We translate
  // the asset id into a cursor jump so the playlist swaps to the
  // next member; the {#key asset.id} on AssetViewer remounts the
  // body so the new track loads + plays its resume position.
  function onAudiobookAdvance(e: Event) {
    const ce = e as CustomEvent<{ assetId: string }>;
    const id = ce.detail?.assetId;
    if (!id) return;
    const idx = source.items.findIndex((it) => it.id === id);
    if (idx >= 0) {
      e.preventDefault?.();
      goTo(idx);
    }
  }

  onMount(() => {
    // Restore pane + strip prefs first so the initial open matches
    // last session.
    if (localStorage.getItem('assetPlaylist.paneCollapsed') === '1') paneCollapsed = true;
    if (localStorage.getItem('assetPlaylist.stripCollapsed') === '1') stripCollapsed = true;
    const savedHeight = parseInt(localStorage.getItem('assetPlaylist.stripHeight') ?? '', 10);
    if (Number.isFinite(savedHeight) && savedHeight >= STRIP_MIN) {
      // Re-clamp on restore — the user could have shrunk the viewport
      // since the height was saved, making it now exceed 25vh.
      stripHeight = Math.min(savedHeight, stripMax());
    }
    if (!standalone) {
      // Overlay mode: read the windowed/maximized pref. Standalone
      // stays at its default (always maximized) regardless.
      const pref = localStorage.getItem('assetPlaylist.maximized');
      if (pref === '1') maximized = true;
      else if (pref === '0') maximized = false;
    }
    openDialog();
    // overflow-hidden on the body only when we're covering the whole
    // viewport — windowed mode leaves the navbar interactive so the
    // body scroll behaviour should stay normal.
    if (maximized) document.body.classList.add('overflow-hidden');
    window.addEventListener('aa-audiobook-advance', onAudiobookAdvance as EventListener);
  });

  onDestroy(() => {
    window.removeEventListener('aa-audiobook-advance', onAudiobookAdvance as EventListener);
  });

  // Once-per-close guard — see fireClose() below (#581). Declared here
  // because openDialog() re-arms it.
  let closing = false;

  // Open the dialog in the right mode. showModal() blocks page
  // interaction (correct for maximized = "this is the world now");
  // show() doesn't (correct for windowed = "I'm sitting on top, but
  // the navbar behind me is still clickable").
  function openDialog() {
    if (!dialogEl) return;
    // Re-arm the once-per-close guard (#581) so a playlist that is
    // closed and shown again — the maximize toggle below does exactly
    // that — can still close.
    closing = false;
    if (maximized) {
      dialogEl.showModal();
    } else {
      dialogEl.show();
    }
  }

  function toggleMaximize() {
    maximized = !maximized;
    if (!standalone) {
      localStorage.setItem('assetPlaylist.maximized', maximized ? '1' : '0');
    }
    // Un-maximizing means "give me the chrome back" (#635). Without
    // this the button reads as broken whenever the navbar had already
    // auto-hidden before the viewer opened: windowed resolves
    // --aa-chrome-bottom to 0 (correct, #628/#629 — there is no navbar
    // to sit below), so windowed and maximized are pixel-identical and
    // the only difference is the invisible modal/non-modal swap.
    //
    // reveal() rather than a sticky flag, same as the view switcher
    // (#554): it clears `hidden` and lets the NEXT scroll-down hide the
    // chrome again. Nothing re-hides it while the viewer is open —
    // `main` is not the scroll context then — and it's a no-op under
    // reduced-motion, where the chrome never hides.
    //
    // Deliberately only this direction: maximizing does not force-hide
    // the navbar. Covering it is the point; yanking it away would be a
    // new behaviour, and the un-maximize above restores it either way.
    if (!maximized) chromeScroll.reveal();
    // Swap the dialog mode by closing + reopening — there's no
    // showModal()/show() in-place switch.
    if (dialogEl?.open) dialogEl.close();
    openDialog();
    if (maximized) document.body.classList.add('overflow-hidden');
    else document.body.classList.remove('overflow-hidden');
  }

  $effect(() => {
    localStorage.setItem('assetPlaylist.paneCollapsed', paneCollapsed ? '1' : '0');
  });

  onDestroy(() => {
    document.body.classList.remove('overflow-hidden');
  });

  // Clamp the cursor if the source's items array shrinks while open
  // (host deleted an item, for example). Never let cursor point past
  // items.length-1.
  $effect(() => {
    const n = source.items.length;
    if (n === 0) return;
    if (source.cursor > n - 1) source.cursor = n - 1;
    if (source.cursor < 0) source.cursor = 0;
  });

  // Infinite-scroll hook for search/gallery sources. When the cursor
  // approaches the last item and the source has a loadMore handler,
  // fire it so navigation can spill into the next page.
  $effect(() => {
    if (!source.loadMore) return;
    if (source.loading) return;
    if (source.items.length === 0) return;
    if (source.cursor >= source.items.length - 2) {
      void source.loadMore();
    }
  });

  // ---- Handlers ------------------------------------------------------------

  // onClose must fire EXACTLY ONCE per close (#581). Three paths can
  // reach it — the close button, a backdrop click, and the <dialog>'s
  // native close event (Esc) — and the button path hit two of them:
  // handleClose() called dialogEl.close(), which fires the dialog's
  // `close` event → handleDialogClose → onClose(), and then called
  // onClose() itself.
  //
  // That double-fire was invisible while the standalone routes closed
  // with goto('/'), because navigating to the same URL twice is
  // idempotent. Once close became history.back() it stopped being
  // harmless: two backs skip past the page the user came from and land
  // an entry too far (measured — profile → post → close landed on the
  // collection BEFORE the profile).
  //
  // The guard is per-close rather than per-instance: openDialog()
  // re-arms it, so a playlist that is closed and shown again still
  // closes correctly.
  function fireClose() {
    if (closing) return;
    closing = true;
    onClose();
  }

  function handleDialogClose() {
    if (dialogEl?.open === false) fireClose();
  }

  function handleBackdropClick(e: MouseEvent) {
    if (e.target === dialogEl) fireClose();
  }

  function handleClose() {
    dialogEl?.close();
    fireClose();
  }

  /** Jump to a cursor position. Clamps to [0, items.length-1]; just
      mutates source.cursor — the {#key} on AssetViewer remounts the
      view body so each asset gets a fresh viewer instance (important
      for 3D / video tear-down). */
  function goTo(idx: number) {
    const n = source.items.length;
    if (n === 0) return;
    source.cursor = Math.max(0, Math.min(n - 1, idx));
  }

  function handleKeydown(e: KeyboardEvent) {
    // Ignore key handling while focus is in a text input — comment
    // composers / search boxes inside contextSlot need to type freely.
    const t = e.target as HTMLElement | null;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
      return;
    }
    // A modal opened FROM the shell owns the keyboard while it is up
    // (#881). Without this, Escape closes the whole playlist out from
    // under the request dialog instead of dismissing it, and the user
    // loses their place as well as what they typed.
    if (askOpenFor) return;
    switch (e.key) {
      // ← / → navigate between sibling PLAYLISTS in the surrounding
      // context (next post in the feed, next collection, etc). The
      // host wires this when it has a sibling concept; no-op
      // otherwise.
      case 'ArrowLeft':
        if (onNavigateSibling) {
          e.preventDefault();
          onNavigateSibling('prev');
        }
        break;
      case 'ArrowRight':
        if (onNavigateSibling) {
          e.preventDefault();
          onNavigateSibling('next');
        }
        break;
      // ↑ / ↓ navigate between assets WITHIN the current playlist.
      // Separated from ← / → so users in a feed-overlay context can
      // both flip through posts (← →) AND scrub through a multi-
      // asset post (↑ ↓) without losing their place in the feed.
      // The two axes (horizontal = sibling playlists, vertical = items
      // within a playlist) mirror how users mentally model the feed:
      // posts in a row, assets stacked beneath each post.
      case 'ArrowUp':
        if (hasMultipleItems) {
          e.preventDefault();
          goTo(source.cursor - 1);
        }
        break;
      case 'ArrowDown':
        if (hasMultipleItems) {
          e.preventDefault();
          goTo(source.cursor + 1);
        }
        break;
      case 'i':
      case 'I':
        e.preventDefault();
        paneCollapsed = !paneCollapsed;
        break;
      case 'Escape':
        // <dialog> only handles ESC natively when opened via
        // showModal() (maximized mode). Windowed mode uses show(),
        // which has no native ESC. Close explicitly so the gesture
        // works the same in both modes.
        e.preventDefault();
        handleClose();
        break;
    }
  }

  function toggleStrip() {
    stripCollapsed = !stripCollapsed;
    localStorage.setItem('assetPlaylist.stripCollapsed', stripCollapsed ? '1' : '0');
  }

  // Drag-to-resize the bottom thumb strip. Tracks mouse delta from
  // mousedown position (drag up = positive Δ = grow); clamps to the
  // viewport-relative range each frame so resizing the browser mid-
  // drag stays honest. Persist on mouseup so a partial drag the user
  // bails out of (Esc) doesn't pollute their saved preference.
  function startStripResize(e: MouseEvent) {
    e.preventDefault();
    const startY = e.clientY;
    const startHeight = stripHeight;
    const move = (mv: MouseEvent) => {
      const dy = startY - mv.clientY;
      stripHeight = Math.max(STRIP_MIN, Math.min(stripMax(), startHeight + dy));
    };
    const up = () => {
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);
      localStorage.setItem('assetPlaylist.stripHeight', String(stripHeight));
    };
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
  }

  function colVariantUrl(assetId: string): string {
    return `/api/v1/assets/${assetId}/variants/col`;
  }

  // #881 — "request access" state for restricted members.
  //
  // Keyed by asset id rather than a single boolean because a playlist
  // walks between members: a flat `asked` flag would follow the cursor
  // and label the next restricted member as already-asked. The set is
  // session-local optimism over the server's answer; a reload re-reads
  // the truth from /account/requests.
  let askOpenFor: string | null = $state(null);
  let askedFor: string[] = $state([]);

  // ── Delete the asset under the cursor (#981, #987) ─────────────────
  //
  // WHY THE SHELL AND NOT THE HOST. #981 wired "Delete asset" into
  // PostHost, which is a POST-shaped host — so the affordance existed
  // only where an asset happened to be inside a post, and /assets/{id}
  // (the asset-shaped route, ADR 0067) had no delete at all (#987).
  // The menu seam was never the problem: both routes render this shell,
  // which threads one `onDeleteAsset` into AssetViewer → ViewerMenuBar.
  // What PostHost owned that the standalone route could not reach was
  // the FLOW — the confirm dialog, the request, the toast.
  //
  // None of that flow is post-shaped. It asks about the asset under the
  // cursor, which is the one thing every host of this shell has by
  // definition, so it belongs here and the hosts get it for free. The
  // host prop is gone rather than kept as an override: PostHost was its
  // only caller, and a second way to spell the same action is the drift
  // that becomes the bug.
  //
  // The confirm dialog is declared INSIDE this component's <dialog>
  // (down with RequestAccessDialog) and that placement is load-bearing
  // for the same reason PostHost's comment gives: Modal portals to the
  // nearest OPEN <dialog> resolved from where it was DECLARED, and this
  // shell calls showModal() when maximized. Declared on the page
  // instead, the confirm would land on <body> and render UNDERNEATH the
  // viewer's top layer — present, correct, and invisible.

  // itemId and assetId are two DIFFERENT things and both are needed.
  // PlaylistItem.id is the position key — for most sources it IS the
  // asset id, but the contract allows an envelope id so a source can
  // carry the same asset twice (a favourites playlist with dupes). The
  // endpoint takes the asset id; the splice below matches on the
  // position key. Carrying one value for both is correct today and
  // silently wrong for the first source that uses envelopes.
  let deleteTarget = $state<{
    itemId: string;
    assetId: string;
    title: string;
    ownerRef: number | null | undefined;
  } | null>(null);
  let deleteBusy = $state(false);
  let deleteError = $state<string | null>(null);

  // Owner, or a holder of the global assets.admin — canDelete's ceiling
  // and its reasoning are documented in $lib/deletable. A restricted
  // member is excluded because the shell was handed a placeholder: it
  // has no owner_user_ref to judge, so the honest answer is no button.
  // The server gates the request regardless of what we draw here.
  const canDeleteCurrent = $derived(
    !!currentItem &&
      !currentItem.restricted &&
      canDelete('asset', currentItem.asset?.owner_user_ref),
  );

  // Reads the item under the cursor rather than taking an id and
  // looking it back up: the menu item can only ever mean "this one",
  // and a lookup by id is where the itemId/assetId confusion above
  // would enter.
  function openDeleteDialog() {
    const it = currentItem;
    if (!it) return;
    deleteError = null;
    deleteTarget = {
      itemId: it.id,
      assetId: it.asset.id,
      title: it.asset.title ?? '',
      ownerRef: it.asset.owner_user_ref ?? null,
    };
  }

  function closeDeleteDialog() {
    if (deleteBusy) return;
    deleteTarget = null;
    deleteError = null;
  }

  async function confirmDelete(reason: string) {
    const target = deleteTarget;
    if (!target || deleteBusy) return;
    deleteBusy = true;
    deleteError = null;
    const err = await deleteEntity('asset', target.assetId, reason);
    deleteBusy = false;
    if (err) {
      // Keep the dialog open so the message lands beside the button
      // that produced it.
      deleteError = err;
      return;
    }
    deleteTarget = null;

    // Drop the member from the live playlist rather than re-fetching the
    // source. A reload would work, but it resets the cursor to the first
    // item — deleting item 7 of 9 would silently jump the user back to
    // item 1. The 204 is authority enough that the row is gone; #920 /
    // #935 already invalidated every other surface's cached copy of it.
    const idx = source.items.findIndex((i) => i.id === target.itemId);
    if (idx >= 0) {
      source.items.splice(idx, 1);
      // Clamp: deleting the last item would otherwise leave the cursor
      // one past the end and render nothing at all.
      if (source.cursor > source.items.length - 1) {
        source.cursor = Math.max(0, source.items.length - 1);
      }
    }

    // Nothing left to look at — the surface showing it cannot stay. For
    // a playlist of 1 (the /assets/{id} route) that is EVERY delete, and
    // onClose there is the close-to-origin policy, so the page navigates
    // away; for an overlay it closes back onto whatever was underneath.
    //
    // BEFORE the toast, and AWAITED — the order is load-bearing and is
    // the #985 lesson. A toast raised while this dialog is still open is
    // parented INTO it (it has to be: the dialog owns the top layer).
    // The dialog is then torn down and a detached node renders nowhere;
    // removing an element fires no `close`, so the portal's re-home
    // cannot save it either. Closing first leaves no modal to adopt the
    // toast, and it lands on the body — where a message about a page you
    // have just left belongs.
    //
    // The `await` is the whole fix and it is easy to lose: `onClose` is
    // typed `() => void | Promise<void>` and the standalone route's is
    // the async close-to-origin policy. Called without awaiting, the
    // navigation is merely STARTED, `tick()` resolves long before it
    // finishes, and the toast is pushed while the dialog is still up —
    // which is the exact bug this comment describes, reintroduced.
    // Driven in a browser it looks like the delete silently doing
    // nothing to acknowledge itself.
    if (source.items.length === 0) {
      await onClose();
      await tick();
    }

    toasts.push({
      message: t('delete_confirm.deleted_asset'),
      href: '/account/trash',
      linkLabel: t('delete_confirm.view_trash'),
      action: {
        label: t('delete_confirm.undo'),
        run: () => undoDelete(target.assetId),
      },
    });
  }

  // The Undo in the delete toast. Safe to offer unconditionally: we just
  // performed the delete, so we ARE the deleter, and CanRestoreDeleted
  // grants restore to the deleter. It can never be an affordance the
  // server refuses.
  //
  // No re-insert into `source.items` on success. The restored asset's
  // position comes from the server's ordering, not from where it sat in
  // the array we spliced — and on the standalone route the shell has
  // already navigated away, so there is no list left to put it back
  // into. The toast links to /account/trash, which is the surface that
  // can show the result either way.
  async function undoDelete(id: string) {
    const err = await restoreEntity('asset', id);
    if (err) {
      toasts.push({
        message: t('delete_confirm.undo_error'),
        tone: 'error',
        href: '/account/trash',
        linkLabel: t('delete_confirm.view_trash'),
      });
      return;
    }
    toasts.push({ message: t('delete_confirm.undone') });
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<dialog
  bind:this={dialogEl}
  data-testid="asset-playlist"
  onclose={handleDialogClose}
  onclick={handleBackdropClick}
  class="asset-playlist m-0 bg-transparent p-0 outline-none"
  class:max={maximized}
  class:windowed={!maximized}
  aria-labelledby="asset-playlist-title"
>
  <div
    class="relative flex h-full w-full flex-col overflow-hidden bg-surface text-fg shadow-2xl"
    role="presentation"
  >

    {#if source.loading && source.items.length === 0}
      <!-- Skeleton — only on the very first load (no items yet). On
           subsequent re-targets (e.g. browse-feed ←/→ swapping to
           the next post) the source's load() keeps the previous
           items on screen until the new data arrives, so the
           AssetViewer + ViewerMenuBar stay mounted and there's no
           chrome flicker. -->
      <div class="flex flex-1">
        <div class="flex-1 animate-pulse bg-black/30"></div>
      </div>
    {:else if source.error}
      <div class="flex flex-1 items-center justify-center p-8">
        <div role="alert" class="max-w-md rounded-md border border-danger/40 bg-danger-container px-4 py-3 text-sm text-danger">
          {source.error}
        </div>
      </div>
    {:else if currentItem}
      <div class="relative flex flex-1 overflow-hidden bg-black">
        {#if currentItem.restricted}
          <!-- #883 — the viewer may not see this member. FIRST branch:
               everything below reads asset fields the server did not
               send, and would render an untitled, preview-less asset
               that looks like a failed render rather than a refusal.
               No AssetViewer is mounted, so no byte request is made. -->
          <div class="flex h-full w-full flex-col items-center justify-center gap-3 px-6 text-center text-white/70">
            <svg xmlns="http://www.w3.org/2000/svg" width="72" height="72" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="11" width="18" height="11" rx="2" />
              <path d="M7 11V7a5 5 0 0 1 10 0v4" />
            </svg>
            <p class="text-sm font-medium uppercase tracking-widest text-white">
              {t('card.restricted.label')}
            </p>
            <p class="text-sm">
              {currentItem.ownerDisplayName
                ? t('card.restricted.owner', { owner: currentItem.ownerDisplayName })
                : t('card.restricted.owner_unknown')}
            </p>
            <!-- #881 — the same ask as the grid tile, at the one other
                 place a restricted member is rendered. Nothing here
                 names the asset: the id is posted, never printed, and
                 the label is a fixed string. -->
            {#if auth.user}
              <button
                type="button"
                data-testid="request-access-open"
                disabled={askedFor.includes(currentItem.asset.id)}
                aria-label={askedFor.includes(currentItem.asset.id)
                  ? t('card.restricted.asked')
                  : t('card.restricted.ask')}
                class="mt-1 rounded-md border border-white/35 px-3 py-1.5 text-sm text-white
                       hover:bg-white/10 disabled:cursor-default disabled:opacity-60"
                onclick={() => (askOpenFor = currentItem.asset.id)}
              >
                {askedFor.includes(currentItem.asset.id)
                  ? t('card.restricted.asked')
                  : t('card.restricted.ask')}
              </button>
            {/if}
          </div>
        {:else if currentItem.asset.file_hash}
          <!-- AssetViewer owns the canvas double-click gesture
               (toggles reviewMode). Wrapping it in another dblclick
               handler here would fight the toggle. -->
          <div class="flex h-full w-full items-center justify-center">
            <AssetViewer
              asset={currentItem.asset}
              active={true}
              bind:paneCollapsed
              {customTools}
              {whiteboardSession}
              {hostHooks}
              {titleSlot}
              {canvasOverlay}
              extraTips={playlistHotkeys}
              {maximized}
              onToggleMaximize={toggleMaximize}
              onClose={handleClose}
              onAddToCollection={onAddToCollection
                ? () => onAddToCollection(currentItem.asset.id)
                : undefined}
              onRecreatePreviews={onRecreatePreviews
                ? () => onRecreatePreviews(currentItem.asset.id)
                : undefined}
              onEditTags={onEditTags
                ? () => onEditTags(currentItem.asset.id)
                : undefined}
              onEditMetadata={onEditMetadata
                ? () => onEditMetadata(currentItem.asset.id)
                : undefined}
              onDownloadVariant={onDownloadVariant
                ? () => onDownloadVariant(currentItem.asset.id)
                : undefined}
              onShareAsset={onShareAsset
                ? () => onShareAsset(currentItem.asset.id)
                : undefined}
              onDeleteAsset={canDeleteCurrent ? openDeleteDialog : undefined}
            />
          </div>
        {:else}
          <div class="flex h-full w-full items-center justify-center text-fg-muted">
            <svg xmlns="http://www.w3.org/2000/svg" width="96" height="96" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="3" width="18" height="18" rx="2" />
              <circle cx="9" cy="9" r="2" />
              <path d="m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21" />
            </svg>
          </div>
        {/if}

        <!-- Member nav arrows: visible only when there's >1 item.
             These shift left when the pane is open so they don't
             vanish behind it. -->
        {#if hasMultipleItems}
          <button
            type="button"
            onclick={() => goTo(source.cursor - 1)}
            disabled={source.cursor === 0}
            class="absolute left-3 top-1/2 z-20 inline-flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/50 text-white backdrop-blur-sm transition-colors hover:bg-black/75 disabled:opacity-30 disabled:hover:bg-black/50"
            aria-label={t('viewer_playlist.prev_asset')}
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
              <path d="m15 18-6-6 6-6" />
            </svg>
          </button>
          <button
            type="button"
            onclick={() => goTo(source.cursor + 1)}
            disabled={source.cursor === source.items.length - 1}
            class="absolute top-1/2 z-20 inline-flex h-10 w-10 -translate-y-1/2 items-center justify-center rounded-full bg-black/50 text-white backdrop-blur-sm transition-[right] duration-200 hover:bg-black/75 disabled:opacity-30 disabled:hover:bg-black/50"
            class:right-3={paneCollapsed}
            class:right-[25rem]={!paneCollapsed}
            aria-label={t('viewer_playlist.next_asset')}
          >
            <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
              <path d="m9 18 6-6-6-6" />
            </svg>
          </button>
          <!-- Position indicator (n / total) — bottom-center. -->
          <div class="pointer-events-none absolute bottom-3 left-1/2 -translate-x-1/2 rounded-full bg-black/60 px-3 py-1 text-xs font-medium text-white backdrop-blur-sm">
            {source.cursor + 1} / {source.items.length}
          </div>
        {/if}
      </div>

      <!-- Bottom thumb strip — only when >1 item. Collapsible AND
           drag-resizable: the user can grab the top edge of the strip
           and pull it upward to grow the thumbnails (capped at 25vh
           so the viewer never gets squeezed out). Two separate
           affordances:
             - The chevron row toggles collapsed (no thumbs at all);
             - The handle above it resizes the expanded strip.
           Both persist independently in localStorage. -->
      {#if hasMultipleItems && !allAudiobook}
        <div
          class="flex shrink-0 flex-col border-t border-border bg-surface-elevated"
          style={stripCollapsed ? '' : `height: ${stripHeight}px`}
        >
          {#if !stripCollapsed}
            <!-- Resize handle — thin draggable bar at the very top of
                 the strip. cursor: ns-resize signals the gesture; the
                 visible grip widens + tints on hover so the affordance
                 isn't entirely cursor-based (accessibility). -->
            <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
            <div
              role="separator"
              aria-orientation="horizontal"
              aria-label={t('viewer_playlist.resize_strip')}
              aria-valuenow={stripHeight}
              aria-valuemin={STRIP_MIN}
              onmousedown={startStripResize}
              class="group flex h-1.5 shrink-0 cursor-ns-resize items-center justify-center hover:bg-accent/30"
            >
              <div class="h-0.5 w-10 rounded-full bg-fg-muted/40 group-hover:bg-accent/70"></div>
            </div>
          {/if}
          <button
            type="button"
            onclick={toggleStrip}
            class="flex w-full shrink-0 items-center justify-center gap-1 py-1 text-xs text-fg-muted hover:bg-surface"
            aria-expanded={!stripCollapsed}
            aria-label={stripCollapsed ? t('viewer_playlist.show_strip') : t('viewer_playlist.hide_strip')}
          >
            {#if stripCollapsed}
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m18 15-6-6-6 6" /></svg>
              <span>{source.cursor + 1} / {source.items.length}</span>
            {:else}
              <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6" /></svg>
            {/if}
          </button>
          {#if !stripCollapsed}
            <!-- Thumbnails fill the remaining vertical space inside
                 the sized wrapper (flex-1 + min-h-0). aspect-square
                 keeps them square; h-full sizes them off the row so
                 dragging the handle scales every thumb in lockstep. -->
            <div class="flex min-h-0 flex-1 gap-2 overflow-x-auto overflow-y-hidden px-2 pb-2">
              {#each source.items as item, i (item.id)}
                <button
                  type="button"
                  onclick={() => goTo(i)}
                  class="relative aspect-square h-full shrink-0 overflow-hidden rounded border-2 transition-all"
                  class:border-accent={i === source.cursor}
                  class:opacity-100={i === source.cursor}
                  class:border-transparent={i !== source.cursor}
                  class:opacity-50={i !== source.cursor}
                  class:hover:opacity-100={i !== source.cursor}
                  aria-label={t('viewer_playlist.show_asset_n', { n: i + 1 })}
                  aria-current={i === source.cursor ? 'true' : undefined}
                >
                  {#if item.restricted}
                    <!-- #883 — a lock, not the generic empty frame: the
                         strip is where a viewer counts what is in the
                         post, so a restricted member must read as
                         withheld rather than as not-yet-rendered. -->
                    <div class="flex h-full w-full items-center justify-center bg-surface text-fg-muted/60">
                      <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
                        <rect x="3" y="11" width="18" height="11" rx="2" />
                        <path d="M7 11V7a5 5 0 0 1 10 0v4" />
                      </svg>
                    </div>
                  {:else if item.asset.file_hash && item.asset.preview_available}
                    <img
                      src={colVariantUrl(item.asset.id)}
                      alt=""
                      loading="lazy"
                      class="h-full w-full object-cover"
                    />
                  {:else}
                    <div class="flex h-full w-full items-center justify-center bg-surface text-fg-muted/40">
                      <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
                        <rect x="3" y="3" width="18" height="18" rx="2" />
                      </svg>
                    </div>
                  {/if}
                </button>
              {/each}
            </div>
          {/if}
        </div>
      {/if}
    {:else}
      <!-- Loaded but no items.
           The message alone used to be the WHOLE of this branch, and
           that is the other half of #918. Everything a host contributes
           — the post header, the author, the ⋮ menu with edit / delete /
           manage access — is threaded in as AssetViewer's Details tool,
           and AssetViewer is only mounted by the `currentItem` branch
           above. So a post whose last member was soft-deleted, or one
           that never had a member (an article, ADR 0073), rendered one
           grey sentence and nothing else, to its own author included.
           Ungating the menu inside PostHost cannot help while the pane
           carrying it is never mounted.
           So render the host's pane beside the message. Same snippet,
           same hook key AssetViewer resolves it under — this is the
           fallback mount, not a second copy.

           Stacks below 640px: side-by-side at 390px left the pane about
           240px to render a post header, a ⋮ menu and a comment box in,
           and clipped all three. The message is one line and can sit
           above it. -->
      <div class="flex flex-1 flex-col overflow-hidden sm:flex-row">
        <div class="flex shrink-0 items-center justify-center p-8 text-center text-fg-muted sm:flex-1">
          <p>{t('viewer_playlist.empty')}</p>
        </div>
        {#if emptyHostPane}
          <aside
            class="w-full flex-1 overflow-y-auto border-t border-border bg-surface sm:max-w-sm sm:flex-none sm:border-l sm:border-t-0"
            data-testid="asset-playlist-empty-pane"
          >
            {@render emptyHostPane()}
          </aside>
        {/if}
      </div>
    {/if}
  </div>

  <!-- #881 — INSIDE the <dialog>, and that is load-bearing.
       This shell is a native `<dialog open>`, which the browser puts in
       the TOP LAYER. A fixed-position overlay mounted as a sibling
       renders beneath it and cannot be clicked: driven in a browser, the
       request dialog was invisible and every click on its Send button
       was swallowed by the playlist behind it. A descendant of the
       <dialog> shares its top-layer stacking context and behaves.

       Keyed on the asset id it was opened for, so walking to another
       restricted member cannot leave a dialog pointed at the previous
       one. -->
  {#if askOpenFor}
    <RequestAccessDialog
      assetId={askOpenFor}
      ownerName={currentItem?.ownerDisplayName ?? null}
      open={true}
      onclose={() => (askOpenFor = null)}
      onsubmitted={() => {
        if (askOpenFor) askedFor = [...askedFor, askOpenFor];
      }}
    />
  {/if}

  <!-- Same placement rule, same reason (#981, #987): the delete confirm
       is raised from a menu that lives inside this dialog, so it has to
       be declared where Modal's portal can find that dialog. -->
  <ConfirmDeleteDialog
    open={deleteTarget !== null}
    kind="asset"
    title={deleteTarget?.title}
    askReason={deleteTarget ? shouldAskReason(deleteTarget.ownerRef) : false}
    busy={deleteBusy}
    error={deleteError}
    onconfirm={confirmDelete}
    onclose={closeDeleteDialog}
  />
</dialog>

<!-- Hotkey legend — passed through AssetViewer and rendered as the
     pinned footer of the right pane. The keys shown are filtered by
     context: A/D only appear in multi-asset playlists; ←/→ only when
     the host wired sibling-nav (browse-feed overlay does; standalone
     /posts/{id} doesn't). -->
<!-- Nav / shell hotkey rows. Emits `<dt>/<dd>` pairs directly into
     the shell's TipsSection <dl> grid (no outer <details> — the
     shell owns the accordion). Section dividers use the
     col-span-2 sub-heading convention. The host's own
     extraTips (e.g. PostHost's mode-specific shortcuts) appends
     below this via AssetViewer's extraTips prop. -->
{#snippet playlistHotkeys()}
  <dt class="col-span-2 mt-1 text-fg-muted/70">{t('viewer_hotkeys.title')}</dt>
  {#if hasMultipleItems}
    <dt class="font-mono text-fg">↑ · ↓</dt>
    <dd class="text-fg-muted">{t('viewer_hotkeys.prev_asset')} · {t('viewer_hotkeys.next_asset')}</dd>
  {/if}
  <!-- Rows below are filtered by who actually owns the key right now
       (#885). AssetViewer stops propagation for keys it acts on, so a
       row that is true for an image is a lie for a video: ← / → step
       frames there and never reach this component. Same for I, which
       marks a loop-in on a timeline asset instead of toggling the
       pane. And while the whiteboard overlay is up AssetViewer bails
       out entirely — arrows and I come back to the playlist, while F
       and R belong to the whiteboard (fit / rectangle). -->
  {#if onNavigateSibling && (whiteboardSession || !isTimelineKind)}
    <dt class="font-mono text-fg">← · →</dt>
    <dd class="text-fg-muted">{t('viewer_hotkeys.prev_post')} · {t('viewer_hotkeys.next_post')}</dd>
  {/if}
  {#if whiteboardSession || !isTimelineKind}
    <dt class="font-mono text-fg">I</dt><dd class="text-fg-muted">{t('viewer_hotkeys.toggle_panel')}</dd>
  {/if}
  {#if !whiteboardSession}
    <dt class="font-mono text-fg">F</dt><dd class="text-fg-muted">{t('viewer_hotkeys.fullscreen')}</dd>
    <dt class="font-mono text-fg">R</dt><dd class="text-fg-muted">{t('viewer_hotkeys.reset_view')}</dd>
  {/if}
  <dt class="font-mono text-fg">Esc</dt><dd class="text-fg-muted">{t('viewer_hotkeys.close')}</dd>
  {#if isTimelineKind}
    <dt class="col-span-2 mt-1 text-fg-muted/70">{t('viewer_hotkeys.section_playback')}</dt>
    <dt class="font-mono text-fg">Space · K</dt><dd class="text-fg-muted">{t('viewer_hotkeys.play_pause')}</dd>
    <dt class="font-mono text-fg">J · L</dt><dd class="text-fg-muted">{t('viewer_hotkeys.rewind_forward')}</dd>
    <!-- ← / → are listed here, not under navigation: on a timeline
         asset the player claims them for frame stepping (#885). -->
    <dt class="font-mono text-fg">← · → · , · .</dt><dd class="text-fg-muted">{t('viewer_hotkeys.step_back_forward')}</dd>
    <dt class="font-mono text-fg">⇧ + ← · →</dt><dd class="text-fg-muted">{t('viewer_hotkeys.step_back_forward_10')}</dd>
    <dt class="font-mono text-fg">1 – 5</dt><dd class="text-fg-muted">{t('viewer_hotkeys.speed_range')}</dd>
    <dt class="font-mono text-fg">G</dt><dd class="text-fg-muted">{t('viewer_hotkeys.goto_frame')}</dd>
    <dt class="font-mono text-fg">I · O</dt><dd class="text-fg-muted">{t('viewer_hotkeys.loop_in_out')}</dd>
    <dt class="font-mono text-fg">⌫</dt><dd class="text-fg-muted">{t('viewer_hotkeys.loop_clear')}</dd>
    <dt class="font-mono text-fg">Ctrl/⌘ + wheel</dt><dd class="text-fg-muted">{t('viewer_hotkeys.zoom_scrubber')}</dd>
  {/if}
  {#if currentKind === 'audio'}
    <dt class="col-span-2 mt-1 text-fg-muted/70">{t('viewer_hotkeys.section_waveform')}</dt>
    <dt class="font-mono text-fg">Click</dt><dd class="text-fg-muted">{t('viewer_hotkeys.wave_seek')}</dd>
    <dt class="font-mono text-fg">⇧ + drag</dt><dd class="text-fg-muted">{t('viewer_hotkeys.wave_select_loop')}</dd>
    <dt class="font-mono text-fg">Ctrl/⌘ + wheel</dt><dd class="text-fg-muted">{t('viewer_hotkeys.wave_zoom')}</dd>
  {/if}
  {#if extraTips}
    {@render extraTips()}
  {/if}
{/snippet}

<style>
  dialog.asset-playlist {
    border: none;
    outline: none;
    /* Sit above any in-page chrome (the BrowseFooter is z-20, theme
       toaster z-30, etc). Native non-modal <dialog> doesn't auto-
       promote to the top layer the way showModal() does, so we
       enforce the layering ourselves. */
    z-index: 40;
  }
  /* Maximized: full viewport, modal backdrop. */
  dialog.asset-playlist.max {
    top: 0;
    right: 0;
    bottom: 0;
    left: 0;
    width: 100%;
    height: 100%;
    max-width: none;
    max-height: none;
  }
  dialog.asset-playlist.max::backdrop {
    background: rgba(0, 0, 0, 0.8);
    backdrop-filter: blur(4px);
  }
  /* Windowed: flush against the bottom of the global chrome + the
     viewport edges. The viewer behaves like a route below the chrome —
     covers the BrowseFooter and any other in-page chrome, leaves the
     navbar interactive (non-modal dialog). --aa-chrome-bottom is
     published by the root layout from the measured chrome layer
     (banners + header), so a new bar up there, a taller navbar (advanced
     search drawer, etc.) or the scroll-away hide all move this edge with
     no change here. Falls back to 0 for the frame before the layout's
     effect has run — a wrong-by-a-few-pixels gap would be a magic
     number; flush is at least a state the viewer legitimately has. */
  dialog.asset-playlist.windowed {
    top: var(--aa-chrome-bottom, 0px);
    /* The navbar's hide is animated (transition-transform duration-200
       ease-out in the layout); matching it here means the viewer's top
       edge chases the navbar instead of snapping while the navbar is
       still mid-slide (#628). */
    transition: top 200ms ease-out;
    right: 0;
    bottom: 0;
    left: 0;
    width: 100%;
    height: auto;
    max-width: none;
    max-height: none;
  }
  dialog.asset-playlist:not([open]) {
    display: none;
  }
  /* Collapse-section chevron — points right when closed, rotates 90°
     down when the parent <details> is open. Same idiom PostHost uses
     for the Metadata section; centralising it here so any future
     <details class="aa-collapse"> rendered inside this component
     gets the same affordance for free. */
  :global(details.aa-collapse[open] > summary .aa-chevron) {
    transform: rotate(90deg);
  }
  :global(details.aa-collapse > summary::-webkit-details-marker) {
    display: none;
  }
</style>
