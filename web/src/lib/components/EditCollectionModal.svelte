<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Edit existing collection metadata. Wraps PATCH /collections/{id}.

  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import Modal from './Modal.svelte';
  import CollectionFieldsSection from './CollectionFieldsSection.svelte';
  import CollectionCoverEditor from './CollectionCoverEditor.svelte';
  import { coverPlacement, type CoverSlot } from '$lib/util/featuredCrop';

  // The tiers a collection can hold, widest first.
  //
  // `public` is here as of #1195. It has been in the column's CHECK
  // constraint since migration 00008 and in the collection read rule
  // (visibility.Predicate's EntityCollection arm admits `visibility =
  // 'public'`) ever since; the OpenAPI enum and therefore this union
  // were the only things left saying otherwise, so the tier existed
  // everywhere except in the one control that could set it. Same defect
  // #1176 fixed on the post side.
  //
  // Ordered widest → narrowest rather than in the order they were added.
  // A visibility control is a ladder and reads as one; the previous
  // order (private, org-only, followers, explicit-share) put the two
  // narrowest tiers at opposite ends of the row.
  type Visibility = 'public' | 'org-only' | 'followers' | 'explicit-share' | 'private';
  const ALL_TIERS: Visibility[] = ['public', 'org-only', 'followers', 'explicit-share', 'private'];

  interface Collection {
    id: string;
    name: string;
    description: string;
    visibility: string;
    owner_user_ref: number;
    created_at: string;
    updated_at: string;
    // #1027 — the curator's CHOSEN cover, or null for the derived
    // mosaic. Read off the collection rather than off `covers`: the two
    // answer different questions, and `covers` deliberately falls back
    // to the mosaic for a reader who may not picture the chosen asset,
    // so a picker seeded from it would show the wrong thing to exactly
    // the people allowed to change it.
    cover_asset_id?: string | null;
    // #1207 — the featured strip's own cover and the focal point that
    // positions its crop. Read off the collection for the same reason
    // `cover_asset_id` is: this is the curator's SETTING, and the rail's
    // render answer deliberately differs from it for a reader who may
    // not picture the chosen asset.
    featured_cover_asset_id?: string | null;
    featured_cover_focal_x?: number | null;
    featured_cover_focal_y?: number | null;
    cover_focal_x?: number | null;
    cover_focal_y?: number | null;
    // #1212 — how far each crop is tightened. Null is the fit.
    featured_cover_zoom?: number | null;
    cover_zoom?: number | null;
  }

  // One choosable picture in the cover picker.
  //
  // CollectionResource joins the asset's columns in FLAT, not nested
  // under an `asset` object — and on a member this reader may not see,
  // #883 makes every one of them ABSENT rather than null, with
  // `restricted` as the flag saying which shape the row is. So both
  // fields below are optional and both have to be checked.
  //
  // `ladder_available` joins them for the crop preview (#1195): it is
  // what decides which picture the featured card actually loads, and
  // therefore what gets cropped. See coverPreviewSrc.
  interface CoverChoice {
    asset_id: string;
    restricted?: boolean;
    preview_available?: boolean;
    ladder_available?: boolean;
  }

  interface Props {
    open: boolean;
    collection: Collection;
    onclose: () => void;
    onsaved?: (c: Collection) => void;
    /**
     * Open with the cover picker scrolled to and focused (#1027).
     *
     * The More-actions menu has a "Set cover" entry, which is where a
     * curator looks first; it opens this same modal rather than a
     * second one, so there is ONE edit surface and one save path. The
     * flag only decides where the modal starts, never what it can do —
     * every field stays editable either way.
     */
    focusCover?: boolean;
  }

  let { open, collection, onclose, onsaved, focusCover = false }: Props = $props();

  let name = $state('');
  let description = $state('');
  let visibility = $state<Visibility>('private');
  let submitting = $state(false);
  let error = $state<string | null>(null);
  // Phase 1.16 edit-safety: capture updated_at on open so the
  // PATCH can detect "someone else edited this while my modal
  // was open" + show a reload-prompt instead of silently
  // clobbering.
  let baselineUpdatedAt = $state<string>('');
  let conflict = $state<{ updatedAt: string } | null>(null);

  // #1027 — cover choice. `null` means "use the derived mosaic", which
  // is a real selection the curator can make, not merely the absence of
  // one, so it is a nullable value rather than an undefined.
  let coverAssetId = $state<string | null>(null);
  let coverChoices = $state<CoverChoice[]>([]);
  let coverLoading = $state(false);

  // #1207 — the other two settings the cover editor writes, held HERE
  // rather than inside it. The editor is a viewing surface with two
  // pickers; this component owns the collection, the concurrency
  // baseline and the one PATCH, so it owns the values that PATCH sends.
  let featuredCoverAssetId = $state<string | null>(null);
  // ── The framing, ONE object keyed by SLOT (#1213) ─────────────────
  //
  // Six loose variables until the cover editor became a PAGE of this
  // dialog rather than a dialog of its own. The page is now one block
  // rendered twice with a `slot` parameter — the owner's shape, "the
  // modal can have different pages" — and a block rendered twice cannot
  // bind to a differently-named variable each time. `framing[slot].x`
  // is a valid assignment target, so the page binds straight through to
  // whichever slot it was opened for and there is no second copy of the
  // marquee, the picker or the drag arithmetic to keep in step.
  //
  // Deep `$state`, passed BY REFERENCE rather than `$bindable`: the
  // page mutates the proxy and this component sees it, which is what
  // makes "one save applies both pages" true without a sync effect
  // anywhere.
  //
  // Each slot's three values are independent tri-states. Null focal is
  // centre, null zoom is the fit, and both are distinct from the
  // neutral numbers — see migrations 00055 and 00056.
  let framing = $state<Record<CoverSlot, { x: number | null; y: number | null; zoom: number | null }>>(
    { featured: { x: null, y: null, zoom: null }, collection: { x: null, y: null, zoom: null } },
  );

  // ── ONE DIALOG, TWO PAGES (#1213) ─────────────────────────────────
  //
  // This used to raise CollectionCoverEditor as a SECOND `<dialog>` on
  // top of itself, and the owner's finding was the obvious one: "why is
  // edit collection and edit collection cover two different modals that
  // overlap. Why not one modal?"
  //
  // The stack existed for a reason that has since expired. #1207 needed
  // a near-full-viewport cover surface while this modal was still
  // `max-w-lg`, so nesting was the only way to get room; #1195 then
  // widened this one to `max-w-4xl`, which left the second dialog
  // buying nothing but the Escape-ownership problem #1208 had to solve.
  //
  // PAGES rather than an expanding section, which is the owner's call
  // and the right one: the crop stage is the tall element on this
  // surface, and a section that expands inside a page which also
  // carries name, description, visibility and custom fields is the
  // cramped-modal complaint arriving by a different route. One page at
  // a time gives the stage the whole panel — the only shape that works
  // at 390px without cramming.
  let page = $state<'details' | 'cover'>('details');
  /** Which slot page 2 is showing. TWO VISITS WITH A DIFFERENT
   *  PARAMETER, not two components: the picker, the marquee, the zoom
   *  control and the upload arm are one block, and this is the only
   *  thing they disagree about. */
  let coverSlot = $state<CoverSlot>('collection');

  function openCoverPage(slot: CoverSlot) {
    coverSlot = slot;
    page = 'cover';
  }

  /** Escape, the backdrop and the header's X all arrive here.
   *
   *  ON PAGE 2 THEY GO BACK, NOT OUT. That is what a paged sheet does
   *  everywhere else, and it is also the safe direction: page 2 holds
   *  unsaved framing work, and a dismiss gesture that discards it is the
   *  keystroke #1208 was fixing in the stacked version. The second press
   *  closes the dialog, so the two-press path out is unchanged from the
   *  curator's point of view — the difference is that there is now one
   *  dialog answering both presses instead of two racing for one.
   *
   *  The backdrop and the X take the same route deliberately: three
   *  gestures that mean "dismiss this" should not disagree about how far
   *  back "this" goes. */
  function requestClose() {
    if (page === 'cover') {
      page = 'details';
      return;
    }
    onclose();
  }

  $effect(() => {
    if (open) {
      name = collection.name;
      description = collection.description;
      visibility = collection.visibility as Visibility;
      baselineUpdatedAt = collection.updated_at;
      coverAssetId = collection.cover_asset_id ?? null;
      featuredCoverAssetId = collection.featured_cover_asset_id ?? null;
      // `?? null` and NOT `|| null`: a stored zoom of 1 is a real value
      // — "framed, and the answer was the fit" — and truthiness would
      // read it as unset, silently turning an explicit choice into a
      // clear on the next save. Same trap #1081 closed on this table.
      framing = {
        featured: {
          x: collection.featured_cover_focal_x ?? null,
          y: collection.featured_cover_focal_y ?? null,
          zoom: collection.featured_cover_zoom ?? null,
        },
        collection: {
          x: collection.cover_focal_x ?? null,
          y: collection.cover_focal_y ?? null,
          zoom: collection.cover_zoom ?? null,
        },
      };
      // ⚠️ `page` IS NOT RESET HERE, and that is deliberate rather than
      // an omission. This effect re-seeds whenever the `collection` prop
      // changes identity, not only when the dialog opens — so a reset in
      // this branch snaps the curator back to page 1 mid-edit if
      // anything upstream refetches the row. Driven, not reasoned about:
      // the 390px spec passed on its own and failed under the full
      // parallel suite, where the refetch had time to land between two
      // assertions, and the symptom was a picker that was in the DOM
      // with a zero-sized box.
      //
      // The `else` branch below resets it on CLOSE instead, which gives
      // the same guarantee — every open starts on page 1 — from an edge
      // that fires once. It is also what lets `focusCover` open the
      // dialog straight onto the cover page without the seed stealing it
      // back.
      error = null;
      conflict = null;
      focusCoverHandled = false;
      previewLadder.init();
      void loadCoverChoices(collection.id);
    } else {
      // Closed: back to page 1, so the next open starts where every
      // open starts. There is no second dialog to tear down any more —
      // that clean-up existed because a nested `<dialog>` left open
      // would sit on the modal stack after its host had gone and
      // swallow the next Escape (#1208). One dialog, one owner, and
      // the page is just state.
      page = 'details';
    }
  });

  // `focusCover` (the More-actions "Set cover" entry) opens the dialog
  // ON THE COVER PAGE (#1207, re-shaped by #1213). It used to scroll the
  // form to a picker; the entry point that says "set cover" should land
  // on the surface that sets covers rather than on the summary of what
  // is already set.
  //
  // The COLLECTION slot, because that is the cover a collection has:
  // the featured one is a refinement for a strip the collection may
  // never appear on, and it is one Back and one click away.
  //
  // Deferred to after the choices have loaded, for the reason the
  // scroll was: opening the cover page over an empty grid shows a stage
  // with no pictures for as long as the fetch takes, which reads as
  // "this collection has no covers to choose".
  // The guard is what makes it happen ONCE. Without it the page jumps
  // back under the curator every time `coverLoading` settles, which
  // includes the moment they press Back.
  let focusCoverHandled = $state(false);
  $effect(() => {
    if (open && focusCover && !coverLoading && !focusCoverHandled) {
      focusCoverHandled = true;
      openCoverPage('collection');
    }
  });

  // ── Visibility: which tiers this instance can honour (#1195) ────────
  //
  // `public` is offered only where it MEANS something. On an install
  // with anonymous browsing off there are no anonymous readers, so the
  // option would promise a reach the instance cannot deliver — the same
  // resolved-capability rule #1163 applied to the reverse-image arm.
  // `site.publicModeEnabled` is that resolved flag, read off the boot
  // payload every client already fetches (the switch itself sits behind
  // system.config.read, which a curator does not have).
  //
  // The exception is the one that keeps the control honest: a
  // collection ALREADY at `public` keeps the option on screen whatever
  // the instance switch says. Hiding it would leave the radio group
  // with no selected value and quietly present the collection as
  // something it is not — a picker that lies about the current state is
  // worse than one that offers a tier with a caveat. The caveat is
  // printed beneath it.
  const publicOffered = $derived(site.publicModeEnabled || collection.visibility === 'public');
  const tiers = $derived(publicOffered ? ALL_TIERS : ALL_TIERS.filter((v) => v !== 'public'));
  const publicIsInert = $derived(!site.publicModeEnabled && visibility === 'public');

  // The members are the picker's options. The API accepts ANY asset the
  // curator may picture — that is what lets a cover outlive the member
  // it was chosen from — but the members are the set that can be shown
  // as pictures without inventing a whole asset browser in a modal, and
  // "upload a banner, add it, pick it" reaches the rest.
  //
  // A member this reader may not picture is dropped rather than shown
  // disabled: the server would refuse it, and offering a control that
  // cannot succeed is worse than not offering it.
  async function loadCoverChoices(id: string) {
    coverLoading = true;
    try {
      const { data } = await api.GET('/collections/{id}/resources', {
        params: { path: { id }, query: { limit: 60 } },
      });
      coverChoices = ((data?.items ?? []) as unknown as CoverChoice[]).filter(
        (m) => !m.restricted && m.preview_available === true,
      );
    } catch {
      // A picker that failed to load must not block renaming the
      // collection, so this is not surfaced as a form error.
      coverChoices = [];
    } finally {
      coverLoading = false;
    }
  }

  // ── The cover SUMMARY (#1207) ──────────────────────────────────────
  //
  // #1195 put a live crop preview here, and #1207's first finding was
  // that it was too small to judge a crop by. The preview did not get
  // bigger; it MOVED, into a dialog with room for it, and what stays in
  // the form is a summary: the two pictures as they will actually be
  // cropped, and the button that opens the editor.
  //
  // Two thumbnails rather than a line of text, because "which picture
  // is on the strip" is a question about pictures. They are small on
  // purpose — small enough to recognise a choice, not to make one,
  // which is exactly the division of labour that fixes the complaint.
  const featuredEffectiveId = $derived(featuredCoverAssetId ?? coverAssetId);

  function coverUrl(assetId: string) {
    return `/api/v1/assets/${assetId}/variants/col`;
  }

  // `col` for both summary thumbs, deliberately, where the EDITOR
  // branches on the ladder. The summary is a square 96px chip and a
  // contain rung would be a second, larger fetch for a picture nobody
  // is judging a crop by; the editor, which IS judging one, loads what
  // the strip loads. Different questions, different sources, and the
  // difference is stated here so a later edit does not "unify" them.
  const summaryPlacement = $derived(
    coverPlacement(framing.featured.x, framing.featured.y, framing.featured.zoom),
  );

  /** The viewport width, tracked so the cover page can withhold the
   *  two-dimensional crop stage below CROP_STAGE_MIN_WIDTH.
   *
   *  Measured here rather than answered with a CSS media query because
   *  the decision is not "lay this out differently", it is "do not
   *  render this control" — a `hidden` stage would still be in the DOM,
   *  still loading its picture, and still reachable by a screen reader
   *  and by the tests, which is precisely the confusion this is meant
   *  to remove. `bind:innerWidth` keeps it live through a rotation. */
  let viewportWidth = $state(1024);

  async function submit() {
    if (!name.trim() || submitting) return;
    submitting = true;
    error = null;
    try {
      const { data, error: apiErr, response } = await api.PATCH('/collections/{id}', {
        params: { path: { id: collection.id } },
        body: {
          name: name.trim(),
          description,
          visibility,
          if_unchanged_since: baselineUpdatedAt || undefined,
          // The tri-state, spelled out. `cover_asset_id: null` cannot
          // mean "remove" on a partial update — null is already "leave
          // alone" for every other property — so removal is the
          // companion flag, exactly as `clear_default` works on a field
          // definition. Sending BOTH is a 400, so these are exclusive
          // branches rather than two independent keys.
          ...(coverAssetId === null
            ? collection.cover_asset_id
              ? { clear_cover: true }
              : {}
            : { cover_asset_id: coverAssetId }),
          // #1207 — the same tri-state twice more. The featured cover
          // is its own value/clear pair; the focal POINT is one flag
          // over two coordinates, because half a point is not a
          // positioning the server can honour.
          //
          // The `collection.*` guards are what keep an unrelated edit
          // — a rename — from sending a clear for something that was
          // never set. A clear on an already-null column is harmless
          // today, but it is also a write the curator did not ask for,
          // and it would advance `updated_at` on every save.
          ...(featuredCoverAssetId === null
            ? collection.featured_cover_asset_id
              ? { clear_featured_cover: true }
              : {}
            : { featured_cover_asset_id: featuredCoverAssetId }),
          ...(framing.featured.x === null || framing.featured.y === null
            ? collection.featured_cover_focal_x != null
              ? { clear_featured_cover_focal: true }
              : {}
            : {
                featured_cover_focal_x: framing.featured.x,
                featured_cover_focal_y: framing.featured.y,
              }),
          ...(framing.collection.x === null || framing.collection.y === null
            ? collection.cover_focal_x != null
              ? { clear_cover_focal: true }
              : {}
            : { cover_focal_x: framing.collection.x, cover_focal_y: framing.collection.y }),
          // #1212 — two more tri-states, and every test here is
          // `=== null` / `!= null` rather than truthiness. A zoom of 1
          // is a meaningful stored value that happens to render like
          // the fit, so `zoom ? … : …` would send a clear for a value
          // the curator deliberately chose.
          ...(framing.featured.zoom === null
            ? collection.featured_cover_zoom != null
              ? { clear_featured_cover_zoom: true }
              : {}
            : { featured_cover_zoom: framing.featured.zoom }),
          ...(framing.collection.zoom === null
            ? collection.cover_zoom != null
              ? { clear_cover_zoom: true }
              : {}
            : { cover_zoom: framing.collection.zoom }),
        },
      });
      if (response.status === 409) {
        const c = apiErr as { error?: string; updated_at?: string } | undefined;
        conflict = { updatedAt: c?.updated_at ?? '' };
        return;
      }
      if (apiErr || !data) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('collections.error_save');
        return;
      }
      onsaved?.(data as Collection);
      onclose();
    } finally {
      submitting = false;
    }
  }

  // Operator clicked "reload + keep my edits" — refresh the
  // baseline timestamp so the next submit doesn't 409 again.
  // The edited form values stay in-place so the operator can
  // choose to keep them, merge manually, or hit Cancel.
  function acknowledgeConflict() {
    if (conflict) {
      baselineUpdatedAt = conflict.updatedAt;
      conflict = null;
    }
  }
</script>

<!-- Wider than the shared default (#1195). This form carries four
     sections — identity, visibility, cover, custom fields — and at
     `max-w-lg` they stacked into a column tall enough that the cover
     picker and the Save button were never on screen together. The width
     buys a two-column split at `md`, which is what actually fixes it:
     the picker sits beside the text fields instead of below them. -->
<svelte:window bind:innerWidth={viewportWidth} />

<!-- ONE DIALOG, TWO PAGES (#1213).
     The title and the width follow the page: page 2 is the crop
     surface and it gets the panel #1207 needed a second `<dialog>` to
     obtain. `max-w-[min(96rem,95vw)]` rather than a Tailwind size step
     because "big enough to judge a picture by" is a proportion of the
     screen — `max-w-7xl` on a 4k display is the cramped modal again
     with more whitespace around it.

     `onclose` is `requestClose`, so Escape, the backdrop and the header
     X all step BACK from page 2 and only close from page 1. -->
<Modal
  title={page === 'cover'
    ? coverSlot === 'featured'
      ? t('collections.cover_editor_featured_heading')
      : t('collections.cover_editor_cover_heading')
    : t('collections.edit_title')}
  {open}
  onclose={requestClose}
  panelClass={page === 'cover' ? 'max-w-[min(96rem,95vw)]' : 'max-w-4xl'}
>
  <!-- The BODY scrolls, not the page behind it. A modal that grows past
       the viewport pushes its own footer out of reach, and this one can
       grow: `CollectionFieldsSection` renders however many custom fields
       the instance defines. 70vh leaves the header and footer visible at
       every height the app targets — 80vh on the cover page, where the
       tall element is the point.

       ⚠️ BOTH PAGES ARE HIDDEN, NOT UNMOUNTED, and that is the mechanism
       that makes Back lossless. A pending cover choice, a dragged focal
       and a zoom all live in THIS component and would survive either
       way — but the cover page's picker state does not: its source tab,
       its search text and its results belong to the page, and a paged
       modal that makes the curator re-run their search every time they
       glance at the other slot is worse than the stack it replaces.
       Driven, not assumed: unmounting the inactive page passed every
       other assertion in the spec and failed exactly that one.

       Hoisting the picker state up here was the alternative and was
       rejected: it would move a search box's scratch state into the
       component that owns the collection, to serve a lifetime question
       that `hidden` answers directly.

       `hidden` is `display: none`, so the inert page is out of the tab
       order and out of the accessibility tree — not a visual trick. Its
       one cost is that the cover page's stage fetches its picture when
       the dialog opens rather than when page 2 is first shown, which is
       one request for a picture the summary chip is already showing,
       and it makes the first visit instant. -->
  <div class="space-y-4 overflow-y-auto pr-1"
       style="max-height: {page === 'cover' ? 80 : 70}vh">
    <div class:hidden={page !== 'details'} data-testid="collection-edit-details-page">
    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
        {error}
      </p>
    {/if}
    {#if conflict}
      <div role="alert" class="rounded border border-warning/40 bg-warning/10 px-3 py-2 text-sm">
        <p class="font-medium text-warning">{t('collections.conflict_heading')}</p>
        <p class="mt-1 text-xs text-fg-muted">{t('collections.conflict_body')}</p>
        <button
          type="button"
          onclick={acknowledgeConflict}
          class="mt-2 rounded border border-warning/60 px-2 py-1 text-xs font-medium text-warning hover:bg-warning/20"
        >{t('collections.conflict_overwrite')}</button>
      </div>
    {/if}

    <!-- Two columns from `md` up, one below it. The split is by SUBJECT,
         not by length: everything about what the collection IS on the
         left, everything about how it LOOKS on the right. -->
    <div class="grid items-start gap-x-6 gap-y-4 md:grid-cols-2">
      <div class="space-y-3">
        <label class="block">
          <span class="mb-1 block text-xs font-medium text-fg-muted">{t('collections.name')}</span>
          <input
            type="text"
            bind:value={name}
            maxlength="200"
            class="w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          />
        </label>
        <label class="block">
          <span class="mb-1 block text-xs font-medium text-fg-muted">{t('collections.description')}</span>
          <textarea
            bind:value={description}
            rows="4"
            maxlength="2000"
            class="w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          ></textarea>
        </label>
        <fieldset>
          <legend class="mb-1 block text-xs font-medium text-fg-muted">{t('collections.visibility')}</legend>
          <div class="grid grid-cols-2 gap-2 sm:grid-cols-3">
            {#each tiers as v (v)}
              <label class="cursor-pointer rounded border border-border bg-surface px-3 py-2 text-center text-sm hover:border-border-strong"
                     class:border-accent={visibility === v}
                     class:text-accent={visibility === v}>
                <input type="radio" name="vis_edit" value={v} bind:group={visibility} class="sr-only" />
                {t(`collections.vis_${v.replace('-', '_')}`)}
              </label>
            {/each}
          </div>
          <!-- The caveat that keeps the option honest. Shown only when
               `public` is SELECTED and the instance cannot honour it,
               which is the one case where the control and the outcome
               disagree. -->
          {#if publicIsInert}
            <p class="mt-2 text-xs text-warning" data-testid="collection-public-inert-note">
              {t('collections.vis_public_off_note')}
            </p>
          {/if}
        </fieldset>
        <!-- Custom fields sit in the LEFT column rather than spanning
             the full width beneath both. They are more of what the
             collection IS, which is this column's subject, and putting
             them here balances a split that otherwise left a column of
             empty space beside a tall cover picker on every collection
             with no fields defined. -->
        <CollectionFieldsSection collectionId={collection.id} />
      </div>

      <!-- The cover SUMMARY, and the door to the editor (#1207).

           This used to be the whole cover surface: a picker grid, a
           crop preview and a mosaic option, in one column of a modal
           that also carries identity, visibility and custom fields.
           The owner's finding was that it left the crop illegible, and
           the fix is not a wider column — it is a surface with room,
           which is CollectionCoverEditor below. What belongs HERE is
           what the form is for: showing the state and reaching the
           control that changes it. -->
      <fieldset data-testid="collection-cover-section">
        <legend class="mb-1 block text-xs font-medium text-fg-muted">{t('collections.cover')}</legend>
        <p class="mb-2 text-xs text-fg-muted">{t('collections.cover_hint')}</p>

        <!-- THE THUMBNAILS ARE THE DOORS (#1213). Each chip is the
             button that opens page 2 for its own slot, so "which cover
             am I editing" is answered by the thing you clicked rather
             than by a control you have to find afterwards. The separate
             "Edit covers" button that used to sit beside them is gone:
             it opened a dialog containing both slots, which is the shape
             this replaced. -->
        <div class="flex flex-wrap items-start gap-4 rounded border border-border bg-surface p-3">
          <!-- The featured chip is drawn at the STRIP's aspect with the
               strip's own object-position, so the summary answers "what
               did my positioning do" without opening anything. A square
               chip here would have hidden the very setting the editor
               exists to make. -->
          <!-- The featured chip is drawn at the STRIP's aspect with the
               strip's own placement, so the summary answers "what did
               my framing do" without opening anything. A square chip
               here would have hidden the very setting page 2 exists to
               make. -->
          <button
            type="button"
            onclick={() => openCoverPage('featured')}
            data-testid="collection-cover-edit-featured"
            class="w-32 rounded text-left focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          >
            <span class="mb-1 block text-[10px] uppercase tracking-wide text-fg-muted">
              {t('collections.cover_summary_featured')}
            </span>
            <span class="relative block overflow-hidden rounded border border-border bg-surface-elevated hover:border-border-strong"
                  style="aspect-ratio: 890 / 500">
              {#if featuredEffectiveId}
                <img
                  src={coverUrl(featuredEffectiveId)}
                  alt=""
                  data-testid="cover-summary-featured"
                  class="object-cover"
                  style={summaryPlacement}
                />
              {:else}
                <span class="flex h-full items-center justify-center text-[10px] text-fg-muted">
                  {t('collections.cover_summary_none')}
                </span>
              {/if}
            </span>
          </button>

          <button
            type="button"
            onclick={() => openCoverPage('collection')}
            data-testid="collection-cover-edit-button"
            class="w-20 rounded text-left focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          >
            <span class="mb-1 block text-[10px] uppercase tracking-wide text-fg-muted">
              {t('collections.cover_summary_cover')}
            </span>
            <span class="block aspect-square overflow-hidden rounded border border-border bg-surface-elevated hover:border-border-strong">
              {#if coverAssetId}
                <img src={coverUrl(coverAssetId)} alt="" data-testid="cover-summary-collection"
                     class="h-full w-full object-cover" />
              {:else}
                <span class="flex h-full items-center justify-center text-center text-[10px] text-fg-muted">
                  {t('collections.cover_summary_none')}
                </span>
              {/if}
            </span>
          </button>
        </div>
      </fieldset>
      </div>
    </div>

    <div class:hidden={page !== 'cover'} data-testid="collection-edit-cover-page">
      <CollectionCoverEditor
        {coverSlot}
        onback={() => (page = 'details')}
        choices={coverChoices}
        loading={coverLoading}
        collectionVisibility={visibility}
        bind:coverAssetId
        bind:featuredCoverAssetId
        {framing}
        {viewportWidth}
      />
    </div>
  </div>

  {#snippet footer()}
    <!-- BACK sits on the left of the action row, away from Cancel and
         Save, because it is a NAVIGATION and they are COMMITS. Its
         absence on page 1 is what tells the curator where they are. -->
    {#if page === 'cover'}
      <button
        type="button"
        onclick={() => (page = 'details')}
        data-testid="collection-cover-back"
        class="mr-auto rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated"
      >
        {t('common.back')}
      </button>
    {/if}
    <!-- Cancel and Save are on BOTH pages. One dialog means one commit:
         Save applies the name, the description, the visibility and both
         slots' covers and framing in the single PATCH, wherever the
         curator happens to be standing when they decide they are done.
         Making them walk back to page 1 to save would be the surface
         asking them to remember which page owns their work — which is
         exactly the confusion the two dialogs caused. -->
    <button
      type="button"
      onclick={onclose}
      class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated"
    >
      {t('common.cancel')}
    </button>
    <button
      type="button"
      onclick={submit}
      disabled={!name.trim() || submitting}
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {submitting ? t('collections.saving') : t('common.save')}
    </button>
  {/snippet}
</Modal>
