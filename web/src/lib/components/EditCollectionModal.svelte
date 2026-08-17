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

  // The scroll target for `focusCover`. Focus moves to the section, not
  // to the first swatch: a collection can hold hundreds of choices, and
  // landing keyboard focus on an arbitrary one of them would strand the
  // caret mid-grid with no indication of where it is.
  let coverSection = $state<HTMLFieldSetElement | null>(null);

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

  $effect(() => {
    if (open) {
      name = collection.name;
      description = collection.description;
      visibility = collection.visibility as Visibility;
      baselineUpdatedAt = collection.updated_at;
      coverAssetId = collection.cover_asset_id ?? null;
      error = null;
      conflict = null;
      coverNatural = null;
      previewLadder.init();
      void loadCoverChoices(collection.id);
    }
  });

  // Scrolling is deferred to after the choices have loaded, not fired
  // on open: the cover section is the LAST thing in the form, so before
  // the grid paints it sits at a scroll offset that stops existing the
  // moment the pictures arrive. Keyed on `coverLoading` going false so
  // it runs once the layout is final.
  $effect(() => {
    if (open && focusCover && !coverLoading && coverSection) {
      coverSection.scrollIntoView({ block: 'center' });
      coverSection.focus();
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

  // A cover chosen from outside the member list still has to be shown as
  // the current selection, or saving an unrelated edit would silently
  // look like it cleared the cover.
  const coverIsExternal = $derived(
    coverAssetId !== null && !coverChoices.some((c) => c.asset_id === coverAssetId),
  );

  function coverUrl(assetId: string) {
    return `/api/v1/assets/${assetId}/variants/col`;
  }

  // ── The featured-card crop preview (#1195) ─────────────────────────
  //
  // Featured cards are locked to 890:500 (FeaturedRail's CARD_ASPECT)
  // and fill that box with `object-cover`, so a portrait cover loses its
  // top and bottom with nothing on the picker to say so. This shows the
  // whole picture with the surviving region boxed on it, beside the card
  // as it will actually be drawn.
  //
  // ⚠️ THE PREVIEW HAS TO LOAD THE SAME PICTURE THE CARD LOADS, or the
  // box is drawn over the wrong thing. FeaturedRail picks between two
  // sources and they have DIFFERENT shapes:
  //
  //   - with a ladder for this asset, a `contain` rung — the original
  //     aspect, so the card's crop is the only crop;
  //   - without one, `col` — which the server has ALREADY centre-cropped
  //     to a square, so the card crops a square, not the original.
  //
  // Previewing `col` in both cases would tell a portrait cover's curator
  // their picture crops to a wide band of a square that does not exist.
  // Hence the branch, mirroring srcsetFor/srcFor in FeaturedRail.
  //
  // Nothing here is measured from the DOM: the crop rectangle is derived
  // from the loaded image's own naturalWidth/naturalHeight, which is the
  // exact ratio `object-cover` works from. That also makes it correct
  // for the `col` case without a second branch — a square source reports
  // 1:1 and the maths follows.
  //
  // ⚠️ ONE KNOWN GAP, FOUND WHILE BUILDING THIS AND REPORTED RATHER THAN
  // FIXED HERE. `featured.ListPublicRail`'s COLLECTION arm does not read
  // `collections.cover_asset_id` at all — its cover comes from a LATERAL
  // over the newest eligible member post (featured/rail.go, under a
  // comment that still says "the explicit curator override is not in the
  // schema yet", which #1027 made untrue). So for a collection featured
  // on the strip today, the strip renders a DERIVED picture and this
  // preview shows the CHOSEN one.
  //
  // What the preview says about the crop is exact either way — the
  // geometry below was checked against a live strip card — and the
  // chosen cover is what `GET /collections/{id}/covers` and therefore
  // every collection CARD already renders. What is not yet true is the
  // rail honouring the choice; that is a backend change on a gated
  // query and belongs in its own issue.
  const coverChoiceFor = $derived(
    coverAssetId === null ? null : (coverChoices.find((c) => c.asset_id === coverAssetId) ?? null),
  );

  // An EXTERNAL cover (chosen from outside this collection) has no row
  // here, so its ladder is unknown. Unknown resolves to `col` — the same
  // fallback FeaturedRail takes when `ladder_available` is false, so the
  // preview degrades to the card's own worst case rather than to an
  // optimistic guess.
  const coverPreviewSrc = $derived.by(() => {
    if (coverAssetId === null) return null;
    if (coverChoiceFor?.ladder_available !== true) return coverUrl(coverAssetId);
    const smallest = previewLadder.smallestKey();
    return smallest ? `/api/v1/assets/${coverAssetId}/variants/${smallest}` : coverUrl(coverAssetId);
  });

  const coverPreviewSrcset = $derived(
    coverAssetId !== null && coverChoiceFor?.ladder_available === true
      ? (previewLadder.srcsetFor(coverAssetId) ?? undefined)
      : undefined,
  );

  // The natural size of whatever rendered, STAMPED WITH THE ASSET IT
  // CAME FROM.
  //
  // The stamp is what makes the box safe rather than merely usually
  // right. The obvious shape — a plain {w,h} cleared by an $effect on
  // the selection — has an ordering hazard: a picture already in the
  // browser cache fires `load` synchronously enough to beat the effect,
  // which then clears the value it just produced. Keying on the id and
  // deriving "is this measurement about the current selection?" removes
  // the question instead of timing it. A stale measurement is not
  // discarded late; it is never the current one.
  let coverNatural = $state<{ assetId: string; w: number; h: number } | null>(null);

  const coverNaturalNow = $derived(
    coverNatural && coverNatural.assetId === coverAssetId ? coverNatural : null,
  );

  function onPreviewLoad(e: Event) {
    const img = e.currentTarget as HTMLImageElement;
    if (coverAssetId && img.naturalWidth > 0 && img.naturalHeight > 0) {
      coverNatural = { assetId: coverAssetId, w: img.naturalWidth, h: img.naturalHeight };
    }
  }

  /** The featured card's aspect, as one number. Mirrors
   *  FeaturedRail.CARD_ASPECT ('890 / 500'); the strip is locked to it
   *  (#1110/#1098), so it is a constant here rather than a prop. */
  const CARD_ASPECT = 890 / 500;

  /** The surviving region, in percentages of the rendered picture.
   *
   *  `object-cover` scales the image to COVER the box and centres it
   *  (`object-position` defaults to 50% 50%), so exactly ONE axis is
   *  trimmed and it is trimmed equally at both ends. A wider-than-card
   *  picture keeps its full height; a taller one keeps its full width.
   *
   *  `trim` names which axis that is, because it is what the overlay
   *  draws: two dimmed bars on the trimmed axis and none on the other.
   *  Two absolutely-positioned rectangles rather than a `clip-path`
   *  cut-out — the same picture, with nothing depending on a fill-rule
   *  and a self-intersecting polygon rendering the way it does today.
   */
  const cropBox = $derived.by(() => {
    if (!coverNaturalNow) return null;
    const aspect = coverNaturalNow.w / coverNaturalNow.h;
    const widthPct = aspect > CARD_ASPECT ? (CARD_ASPECT / aspect) * 100 : 100;
    const heightPct = aspect > CARD_ASPECT ? 100 : (aspect / CARD_ASPECT) * 100;
    /** Nothing is lost — the picture is already card-shaped. Worth
     *  saying out loud rather than boxing the whole image and leaving
     *  the curator to work out that the box means "all of it". */
    const exact = widthPct > 99.5 && heightPct > 99.5;
    return {
      widthPct,
      heightPct,
      leftPct: (100 - widthPct) / 2,
      topPct: (100 - heightPct) / 2,
      exact,
      trim: exact ? 'none' : aspect > CARD_ASPECT ? 'x' : 'y',
    };
  });

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
<Modal title={t('collections.edit_title')} {open} {onclose} panelClass="max-w-4xl">
  <!-- The BODY scrolls, not the page behind it. A modal that grows past
       the viewport pushes its own footer out of reach, and this one can
       grow: `CollectionFieldsSection` renders however many custom fields
       the instance defines. 70vh leaves the header and footer visible at
       every height the app targets. -->
  <div class="max-h-[70vh] space-y-4 overflow-y-auto pr-1">
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

      <!-- tabindex="-1" so `focusCover` has something to move focus to.
           It is NOT in the tab order: a fieldset is not a control, and
           adding a stop here would make every keyboard user tab through
           a wrapper on the way to the swatches. -->
      <fieldset bind:this={coverSection} tabindex="-1" data-testid="collection-cover-section">
        <legend class="mb-1 block text-xs font-medium text-fg-muted">{t('collections.cover')}</legend>
        <p class="mb-2 text-xs text-fg-muted">{t('collections.cover_hint')}</p>

        <!-- The crop preview (#1195). Above the picker, because it is
             about the CURRENT selection and the picker is how you change
             it — the answer belongs beside the question, not after the
             list of alternatives. -->
        <div class="mb-3 rounded border border-border bg-surface p-2" data-testid="collection-crop-preview">
          <p class="mb-2 text-xs font-medium text-fg-muted">{t('collections.crop_heading')}</p>
          {#if coverAssetId === null || coverPreviewSrc === null}
            <p class="text-xs text-fg-muted">{t('collections.crop_mosaic_note')}</p>
          {:else}
            <div class="flex items-start gap-3">
              <!-- Left: the whole picture, with the surviving region
                   boxed on it.

                   The wrapper SHRINK-WRAPS the image (inline-block, no
                   width or height of its own) so the element and the
                   picture are the SAME rectangle. That is what lets the
                   box be positioned in percentages with nothing measured
                   from the DOM. Giving the wrapper a size instead —
                   an aspect-ratio, a fixed height — reintroduces exactly
                   the bug this preview exists to show: the image
                   letterboxes inside a box it does not fill, and the
                   overlay ends up marking a region of the BOX rather
                   than of the picture. -->
              <figure class="min-w-0 flex-1 text-center">
                <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
                  {t('collections.crop_full_label')}
                </figcaption>
                <div class="relative inline-block max-w-full align-top">
                  <img
                    src={coverPreviewSrc}
                    srcset={coverPreviewSrcset}
                    sizes="200px"
                    alt={t('collections.crop_full_alt')}
                    onload={onPreviewLoad}
                    class="block max-h-32 max-w-full rounded"
                  />
                  {#if cropBox && !cropBox.exact}
                    <!-- What gets cropped OFF is dimmed, and what
                         survives is outlined: two readings of one fact,
                         so it holds up for a colour-blind reader and in
                         a 390px screenshot alike. Exactly one axis is
                         ever trimmed (see cropBox.trim), so this is two
                         bars, never four. `pointer-events-none` because
                         it is annotation, not a control. -->
                    {#if cropBox.trim === 'x'}
                      <div class="pointer-events-none absolute inset-y-0 left-0 bg-black/60"
                           style="width: {cropBox.leftPct}%"></div>
                      <div class="pointer-events-none absolute inset-y-0 right-0 bg-black/60"
                           style="width: {cropBox.leftPct}%"></div>
                    {:else}
                      <div class="pointer-events-none absolute inset-x-0 top-0 bg-black/60"
                           style="height: {cropBox.topPct}%"></div>
                      <div class="pointer-events-none absolute inset-x-0 bottom-0 bg-black/60"
                           style="height: {cropBox.topPct}%"></div>
                    {/if}
                    <div
                      class="pointer-events-none absolute border-2 border-accent"
                      data-testid="collection-crop-box"
                      style="left: {cropBox.leftPct}%; top: {cropBox.topPct}%; width: {cropBox.widthPct}%; height: {cropBox.heightPct}%;"
                    ></div>
                  {/if}
                </div>
              </figure>

              <!-- Right: the card itself, drawn the way FeaturedRail
                   draws it — an 890:500 box with `object-cover`. Not a
                   simulation of the crop: the same two CSS properties
                   the strip uses, on the same source, so if the two ever
                   disagree it is because the strip changed. -->
              <figure class="min-w-0 flex-1">
                <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
                  {t('collections.crop_card_label')}
                </figcaption>
                <div
                  class="overflow-hidden rounded border border-border bg-surface-elevated"
                  style="aspect-ratio: 890 / 500"
                >
                  <img
                    src={coverPreviewSrc}
                    srcset={coverPreviewSrcset}
                    alt={t('collections.crop_card_alt')}
                    data-testid="collection-crop-card"
                    class="h-full w-full object-cover"
                  />
                </div>
              </figure>
            </div>
            <p class="mt-2 text-xs text-fg-muted">{t('collections.crop_hint')}</p>
          {/if}
        </div>

        {#if coverLoading}
          <p class="text-xs text-fg-muted">{t('collections.cover_loading')}</p>
        {:else if coverChoices.length === 0 && !coverIsExternal}
          <p class="text-xs text-fg-muted">{t('collections.cover_none')}</p>
        {:else}
          <!-- The choices SCROLL inside a fixed height. A collection can
               hold hundreds of members, and letting the grid size itself
               pushes the modal's Save button below the fold — the control
               the whole form exists to reach. Its own scroll container
               keeps the footer where it was for a two-member collection
               and a two-hundred-member one alike. -->
          <div class="grid max-h-56 grid-cols-4 gap-2 overflow-y-auto rounded border border-border p-1 sm:grid-cols-5">
            <!-- "Use mosaic" is a CHOICE in the same grid as the pictures,
                 not a clear button off to one side: reverting to the
                 derived cover is what the collection does by default, so
                 it reads as one of the options rather than an undo. -->
            <button
              type="button"
              onclick={() => (coverAssetId = null)}
              aria-pressed={coverAssetId === null}
              class="flex aspect-square flex-col items-center justify-center rounded border-2 bg-surface p-1 text-center text-[10px] leading-tight text-fg-muted hover:border-border-strong"
              class:border-accent={coverAssetId === null}
              class:border-border={coverAssetId !== null}
            >
              <span class="font-medium">{t('collections.cover_derived')}</span>
              <span class="mt-0.5 opacity-70">{t('collections.cover_derived_hint')}</span>
            </button>

            {#if coverIsExternal && coverAssetId}
              <button
                type="button"
                aria-pressed="true"
                title={t('collections.cover_current_external')}
                class="relative aspect-square overflow-hidden rounded border-2 border-accent"
              >
                <img
                  src={coverUrl(coverAssetId)}
                  alt={t('collections.cover_current_external')}
                  loading="lazy"
                  class="h-full w-full object-cover"
                />
              </button>
            {/if}

            {#each coverChoices as choice (choice.asset_id)}
              <button
                type="button"
                onclick={() => (coverAssetId = choice.asset_id)}
                aria-pressed={coverAssetId === choice.asset_id}
                class="relative aspect-square overflow-hidden rounded border-2 hover:border-border-strong"
                class:border-accent={coverAssetId === choice.asset_id}
                class:border-border={coverAssetId !== choice.asset_id}
              >
                <img
                  src={coverUrl(choice.asset_id)}
                  alt=""
                  loading="lazy"
                  class="h-full w-full object-cover"
                />
              </button>
            {/each}
          </div>
        {/if}
      </fieldset>
    </div>
  </div>

  {#snippet footer()}
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
