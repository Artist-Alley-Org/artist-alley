<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Per-card overflow menu (#578, was CardToolRow / #515 slice 2).
  //
  // The owner asked for the grid overlay to read artwork-first: instead
  // of a visible row of three action chips competing with the art, a
  // single ⋮ button (top-right, revealed on hover / focus; always shown
  // on touch) opens a small popover holding the same actions. Shared by
  // AssetCard + PostCard, so browse / profile / collection grids all get
  // the same affordance.
  //
  // Actions (unchanged from the row):
  //   info  — link to the detail page. Read-only, always shown.
  //   copy-link — copy the absolute permalink (mirrors ViewerMenuBar).
  //           Read-only. Named for what it does: the key used to be
  //           `card.tools.share`, which read as the access flow next to
  //           the one that actually is.
  //   add-to-collection — opens CollectionPicker. WRITE action, gated on
  //           logged-in AND not the read-only demo (site.demoMode). The
  //           demo blocks writes at the nginx edge (ADR 0060), so the tool
  //           is hidden rather than shown as a 403 dead-end.
  //           Deliberately NOT gated on ownership: collecting other
  //           people's work is the point of #882, and what may be
  //           collected is decided server-side by whether the caller can
  //           READ the item, which the card cannot know.
  //           POST CARDS ONLY (#1185). It collects the POST (`postId`),
  //           never the post's cover asset — before #882 it collected the
  //           cover, which quietly turned "save this post" into "save one
  //           image out of it" and lost the author's title, description
  //           and the rest of the carousel. An ASSET card no longer offers
  //           it at all: a collection shows posts only, so pinning a bare
  //           asset wrote a `collection_resources` row that nothing on
  //           this instance renders — a click that reported success and
  //           changed nothing the user could see. The endpoint still
  //           exists and still works; retiring it is #1161's, and the
  //           post-creating replacement ("turn these assets into a post
  //           in this collection") is #1161's too.
  //   edit  — links to the entity's edit route (#549). WRITE action, and
  //           like manage-access it appears only when the CARD hands one
  //           over: the card knows whose work it is, this menu does not.
  //           A link and not a modal because the edit surface is a route
  //           (/assets/{id}/edit), which keeps it reload-safe, linkable,
  //           and gate-able server-side on its own load.
  //   manage-access — opens ShareEntityModal on the card's entity (#880).
  //           Present only when the caller passes `manageAccess`, which
  //           the card does only for its OWNER. A SIBLING of "copy link",
  //           not a submenu under it: the two are peers (one hands out a
  //           URL, one hands out permission), and this panel is portaled
  //           and fixed-positioned from the trigger rect, so a nested
  //           popover would need a second layer of the same clamping and
  //           focus plumbing to buy nothing. AssetCard never passes it —
  //           assets have no ACL surface (that is #912).
  //
  // Why a bespoke menu and not $components/Menu.svelte: that primitive
  // positions its panel `absolute` inside the trigger's box. A grid card
  // is `overflow-hidden` (the letterboxed thumb) AND gains `hover:scale`
  // (a transform, which becomes the containing block for fixed/absolute),
  // so an inline panel is clipped by the first and re-anchored by the
  // second. This menu PORTALS its panel to <body> and positions it from
  // the trigger's viewport rect, escaping both. The a11y contract is the
  // same as Menu.svelte: aria-haspopup, Esc, click-outside, focus into
  // the panel on open, focus back to the trigger on close, arrow nav.
  //
  // The trigger + every item stopPropagation + preventDefault so opening
  // the menu or picking an action never also fires the card's
  // stretched-link navigation.

  import { tick } from 'svelte';
  import { auth } from '$stores/auth.svelte';
  import { site } from '$stores/site.svelte';
  import { t } from '$stores/lang.svelte';
  import { portal } from '$lib/util/portal';
  import CollectionPicker from './CollectionPicker.svelte';
  import ShareEntityModal from './ShareEntityModal.svelte';

  interface Props {
    /** The post this card stands for, when it IS a post card (#882).
     *  Set, the add-to-collection action collects the POST; unset, the
     *  action does not render at all.
     *
     *  This sat beside an `assetId` prop until #1185. That prop existed
     *  so an ASSET card could offer the same action against
     *  `collection_resources`; with collections showing posts only there
     *  is nothing for an asset card to collect INTO, and no other action
     *  in this menu read it — every one of them works off `detailPath`,
     *  `editPath` or `manageAccess`. So it is gone rather than kept as an
     *  ignored parameter. */
    postId?: string | null;
    /** Canonical detail path, e.g. `/assets/{id}` or `/posts/{id}`. Info
     *  navigates here; share copies `origin + detailPath`. */
    detailPath: string;
    /** The ACL-bearing entity this card stands for, when the viewer may
     *  manage it. Null / absent hides the manage-access action — which
     *  is the case for every asset card and for any post the viewer did
     *  not author. The card decides ownership; this component only
     *  renders what it is handed. */
    manageAccess?: { kind: 'post' | 'collection'; id: string } | null;
    /** Where this card's edit surface lives, when the viewer may plausibly
     *  reach it (#549). Null / absent hides the edit action. Same division
     *  of labour as `manageAccess`: the CARD owns the ownership question,
     *  this component only renders what it is handed — and the edit route
     *  itself re-answers it authoritatively, so a card that guesses
     *  generously costs a page, not a silent failure. */
    editPath?: string | null;
    /** The CALLER already reveals this control, so skip the built-in
     *  hover/focus fade (#1111).
     *
     *  The grid post card's overlay renders this menu INSIDE a block
     *  that is itself hidden at rest and revealed on hover-or-focus.
     *  Two reveal rules multiply: the outer one turns the block on, the
     *  inner `opacity-0` keeps the button off, and the ⋯ is simply
     *  missing from an overlay that is otherwise fully drawn — which is
     *  how it shipped for exactly one screenshot. Handing the decision
     *  to the parent means one rule decides, and it is the rule that
     *  knows about the block.
     *
     *  Every other caller leaves this false and keeps the behaviour
     *  #578 specified: hidden at rest on fine pointers, revealed on
     *  hover / focus, always shown on touch. */
    revealed?: boolean;
    /** Where the trigger sits (#1136).
     *
     *  `overlay` — the historical placement: absolutely positioned at
     *  the artwork's top-right on a dark translucent disc, hidden at
     *  rest. Designed to survive an unknown photograph underneath it.
     *
     *  `inline` — an ordinary flow element OUTSIDE the preview, for
     *  thumbnail's frame layout. Always visible (it is not covering
     *  anything, and `revealed` is about artwork), and it wears the
     *  theme's own colours: the black disc is camouflage for a
     *  photograph, and on a solid panel it reads as a sticker.
     *
     *  It sits at the END OF THE METADATA STACK'S LAST ROW, not in the
     *  top band — the owner's ruling, and the reason this variant is
     *  sized the way it is. See the trigger's comment below. */
    placement?: 'overlay' | 'inline';
  }

  let {
    postId = null,
    detailPath,
    manageAccess = null,
    editPath = null,
    revealed = false,
    placement = 'overlay',
  }: Props = $props();

  // What this card would put in a collection (#882). Resolved once here
  // so the menu ITEM's visibility and the modal's payload can never
  // disagree — the bug shape where the action renders and then opens a
  // picker with nothing to add.
  //
  // #1185 — a POST id or nothing. This used to fall back to an `assetId`
  // prop, which is what made every asset card offer a write whose result
  // no surface displays.
  const collectPostId = $derived(postId ?? null);
  const canCollect = $derived(!!collectPostId);

  // Write actions show only for a logged-in user on a non-demo install.
  // (No dedicated content-write capability exists — collections gate on
  // auth + ownership — so this is the correct app-consistent gate.)
  const canWrite = $derived(!!auth.user && !site.demoMode);

  let open = $state(false);
  let copied = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | null = null;
  let pickerOpen = $state(false);

  let triggerEl: HTMLButtonElement | undefined = $state();
  let panelEl: HTMLDivElement | undefined = $state();
  // Fixed-position coords for the portaled panel, from the trigger rect.
  let panelStyle = $state('');

  const PANEL_W = 200;

  function positionPanel() {
    if (!triggerEl) return;
    const r = triggerEl.getBoundingClientRect();
    // Right-align the panel to the trigger, opening downward. Clamp into
    // the viewport so a card near an edge never pushes it off-screen.
    let left = r.right - PANEL_W;
    left = Math.max(8, Math.min(left, window.innerWidth - PANEL_W - 8));
    const top = Math.min(r.bottom + 4, window.innerHeight - 8);
    panelStyle = `position:fixed;top:${top}px;left:${left}px;width:${PANEL_W}px;`;
  }

  async function toggle(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    open = !open;
    if (open) {
      positionPanel();
      await tick();
      // Focus the first item so keyboard users land inside the menu.
      panelEl?.querySelector<HTMLElement>('a,button')?.focus();
    }
  }

  function close(refocus = true) {
    if (!open) return;
    open = false;
    if (refocus) triggerEl?.focus();
  }

  function onKeydown(e: KeyboardEvent) {
    if (!open) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
      return;
    }
    if (!panelEl) return;
    const items = Array.from(panelEl.querySelectorAll<HTMLElement>('a,button'));
    if (items.length === 0) return;
    const idx = items.indexOf(document.activeElement as HTMLElement);
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      items[(idx + 1 + items.length) % items.length]?.focus();
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      items[(idx - 1 + items.length) % items.length]?.focus();
    } else if (e.key === 'Home') {
      e.preventDefault();
      items[0]?.focus();
    } else if (e.key === 'End') {
      e.preventDefault();
      items[items.length - 1]?.focus();
    }
  }

  function onDocPointer(e: Event) {
    if (!open) return;
    const target = e.target as Node;
    if (panelEl?.contains(target) || triggerEl?.contains(target)) return;
    close(false);
  }

  // While open, keep the portaled panel pinned to the trigger and wired
  // to the dismiss handlers. Scroll/resize reposition; capture-phase
  // pointerdown catches outside clicks even through stopPropagation.
  $effect(() => {
    if (!open) return;
    const reposition = () => positionPanel();
    window.addEventListener('scroll', reposition, true);
    window.addEventListener('resize', reposition);
    window.addEventListener('keydown', onKeydown);
    document.addEventListener('pointerdown', onDocPointer, true);
    return () => {
      window.removeEventListener('scroll', reposition, true);
      window.removeEventListener('resize', reposition);
      window.removeEventListener('keydown', onKeydown);
      document.removeEventListener('pointerdown', onDocPointer, true);
    };
  });

  async function share(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    const url = `${window.location.origin}${detailPath}`;
    try {
      await navigator.clipboard?.writeText(url);
      copied = true;
      if (copyTimer) clearTimeout(copyTimer);
      copyTimer = setTimeout(() => (copied = false), 1500);
    } catch {
      /* clipboard blocked (insecure context / permission) — no-op */
    }
    close();
  }

  function openInfo(e: MouseEvent) {
    // Let the <a> navigate; just close the menu after.
    e.stopPropagation();
    close(false);
  }

  function openPicker(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    pickerOpen = true;
    close(false);
  }
  function closePicker() {
    pickerOpen = false;
  }

  let shareOpen = $state(false);
  function openShare(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    shareOpen = true;
    close(false);
  }

  const item =
    'flex w-full items-center gap-2.5 px-3 py-2 text-left text-sm text-fg ' +
    'hover:bg-surface-elevated focus:bg-surface-elevated focus:outline-none';
</script>

<!--
  ⋮ trigger, top-right. Hidden at rest on fine pointers (the resting grid
  reads as art, not chrome); revealed on hover / focus-within; always
  shown on touch, where hover is unreachable. 44px hit target, 36px
  visible chip — same sizing the row used. z-20 sits above the card's
  stretched nav link + the title overlay.

  INLINE placement now lives at the END OF THE METADATA STACK'S LAST
  ROW — the date · likes · comments line — and not in the top band. The
  owner's ruling: "I like the menu bottom right. Asset type icon and
  count top left and checkbox top right. The padding around the menu
  button should be smaller and rectangle instead of circle."

  THE ROW IT JOINS IS 16px TALL and must stay that way, which is what
  sizes this variant. That row is `text-xs` — a 16px line box — so a
  44px control dropped into it would nearly triple the row's height and
  push the preview up on every card. The plate is therefore 24px and
  carries `-my-1`, so its MARGIN box is exactly the 16px the line box
  already occupied and the row does not move. The wrapper is `flex` in
  this variant precisely so that arithmetic is a flex item's margin box
  rather than an atomic inline's baseline alignment, which would have
  re-introduced the leading this is trying to avoid.

  24px is also the floor, not a shortcut: WCAG 2.5.8 AA asks for 24x24
  for pointer input and this is exactly that, on the modality that has
  a cursor to aim with. COARSE POINTERS KEEP THE FULL 44px and the row
  grows to fit them — the same trade #1171 made for the checkbox, and
  the honest one: a phone has no cursor, and 20px of card height is
  worth less than a tap target that can be hit.

  RECTANGULAR, NOT A DISC. The plate is `rounded-md` at the same 24px
  the checkbox's box uses, so the card's two controls are one family
  seen at two corners instead of a square and a circle. The disc is
  kept for `overlay`, where it is camouflage over an unknown
  photograph and has a different job.
-->
<div
  class="pointer-events-none transition-opacity duration-150
         {placement === 'inline'
    ? 'flex items-center opacity-100'
    : 'absolute right-2 top-2 z-20 ' +
      (revealed
        ? 'opacity-100'
        : 'opacity-0 group-hover:opacity-100 focus-within:opacity-100 [@media(hover:none)]:opacity-100')}
         {open ? 'opacity-100' : ''}"
>
  <button
    bind:this={triggerEl}
    type="button"
    onclick={toggle}
    aria-haspopup="menu"
    aria-expanded={open}
    aria-label={t('card.tools.menu_label')}
    title={t('card.tools.menu_label')}
    data-testid="card-menu-trigger"
    class="pointer-events-auto group/tool inline-flex h-11 w-11 cursor-pointer items-center
           justify-center focus-visible:outline-none
           {placement === 'inline'
      ? '[@media(pointer:fine)]:-my-1 [@media(pointer:fine)]:h-6 [@media(pointer:fine)]:w-6'
      : ''}"
  >
    <!-- #1126: the hover state, made visible.
         `cursor-pointer` first, because there was none — a `<button>`
         computes `cursor: default` (measured), and Tailwind's preflight
         does not change it, so the one control on the card that IS a
         button was the one that did not look clickable.

         The chip's hover step was `bg-black/60` → `bg-black/80`, which
         over artwork is a 20% alpha change on an already-dark disc:
         present in the stylesheet, invisible on screen. It now brightens
         to a near-opaque disc AND takes a hairline ring, so the state
         reads by two channels rather than one — the same reason the rail
         chips pair a border change with a background change. Scaling was
         the alternative and is wrong here: the ⋯ sits 8px from the tile
         corner, and a control that grows on hover would cross the edge. -->
    <span
      class="flex shrink-0 items-center justify-center ring-1 ring-transparent
             transition-colors
             {placement === 'inline'
        ? 'h-6 w-6 rounded-md ' +
          'text-fg-muted hover:bg-state-hover group-hover/tool:bg-state-hover group-hover/tool:text-fg ' +
          'group-focus-visible/tool:ring-2 group-focus-visible/tool:ring-ring ' +
          (open ? 'bg-state-hover text-fg' : '')
        : 'h-9 w-9 rounded-full bg-black/60 text-white backdrop-blur-sm ' +
          'group-hover/tool:bg-black/85 group-hover/tool:ring-white/40 ' +
          'group-focus-visible/tool:ring-2 group-focus-visible/tool:ring-white/80 ' +
          (open ? 'bg-black/85 ring-white/40' : '')}"
    >
      <!-- 16px in the 24px inline plate, 18px on the 36px disc: the
           glyph keeps the same proportion in both, rather than a
           full-size ⋯ crowding the smaller box. The class wins over the
           width/height attributes, which stay as the overlay's size. -->
      <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true" class={placement === 'inline' ? 'h-4 w-4' : ''}>
        <circle cx="12" cy="5" r="1.75" />
        <circle cx="12" cy="12" r="1.75" />
        <circle cx="12" cy="19" r="1.75" />
      </svg>
    </span>
  </button>
</div>

{#if open}
  <!-- Portaled to <body> so the card's overflow-hidden + hover:scale
       can't clip or re-anchor it. Positioned from the trigger rect. -->
  <div
    bind:this={panelEl}
    use:portal
    role="menu"
    aria-label={t('card.tools.menu_label')}
    data-testid="card-menu-panel"
    style={panelStyle}
    class="z-50 overflow-hidden rounded-lg border border-border bg-surface py-1 shadow-lg"
  >
    <a href={detailPath} role="menuitem" onclick={openInfo} class={item}>
      <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <circle cx="12" cy="12" r="10" /><path d="M12 16v-4" /><path d="M12 8h.01" />
      </svg>
      {t('card.tools.info')}
    </a>

    <button type="button" role="menuitem" onclick={share} class={item}>
      {#if copied}
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <polyline points="20 6 9 17 4 12" />
        </svg>
        {t('card.tools.copied')}
      {:else}
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d="M4 12v8a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-8" /><polyline points="16 6 12 2 8 6" /><line x1="12" y1="2" x2="12" y2="15" />
        </svg>
        {t('card.tools.copy_link')}
      {/if}
    </button>

    {#if canWrite && editPath}
      <a href={editPath} role="menuitem" onclick={openInfo} data-testid="card-edit" class={item}>
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d="M12 20h9" /><path d="M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4Z" />
        </svg>
        {t('card.tools.edit')}
      </a>
    {/if}

    {#if canWrite && manageAccess}
      <button type="button" role="menuitem" onclick={openShare} data-testid="card-manage-access" class={item}>
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" /><circle cx="9" cy="7" r="4" /><line x1="19" y1="8" x2="19" y2="14" /><line x1="22" y1="11" x2="16" y2="11" />
        </svg>
        {t('card.tools.manage_access')}
      </button>
    {/if}

    {#if canWrite && canCollect}
      <button type="button" role="menuitem" onclick={openPicker} data-testid="card-add-to-collection" class={item}>
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d="M4 4h7l2 2h7a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1z" /><line x1="12" y1="11" x2="12" y2="17" /><line x1="9" y1="14" x2="15" y2="14" />
        </svg>
        {t('card.tools.add_to_collection')}
      </button>
    {/if}
  </div>
{/if}

{#if pickerOpen && canCollect}
  <CollectionPicker postIds={collectPostId ? [collectPostId] : []} onClose={closePicker} />
{/if}

{#if manageAccess}
  <ShareEntityModal
    open={shareOpen}
    kind={manageAccess.kind}
    id={manageAccess.id}
    onclose={() => (shareOpen = false)}
  />
{/if}
