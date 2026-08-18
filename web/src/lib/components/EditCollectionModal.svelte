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
  import { coverPlacement } from '$lib/util/featuredCrop';

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
  let focalX = $state<number | null>(null);
  let focalY = $state<number | null>(null);
  // The COLLECTION cover's own pair, on the square destination (#1207).
  let coverFocalX = $state<number | null>(null);
  let coverFocalY = $state<number | null>(null);
  // #1212 — how far each crop is tightened. Null is the fit, and it is
  // a THIRD independent tri-state rather than a companion of the focal
  // pair: a curator can tighten without moving and move without
  // tightening, so each carries its own value and its own clear.
  let zoom = $state<number | null>(null);
  let coverZoom = $state<number | null>(null);
  let coverEditorOpen = $state(false);

  $effect(() => {
    if (open) {
      name = collection.name;
      description = collection.description;
      visibility = collection.visibility as Visibility;
      baselineUpdatedAt = collection.updated_at;
      coverAssetId = collection.cover_asset_id ?? null;
      featuredCoverAssetId = collection.featured_cover_asset_id ?? null;
      focalX = collection.featured_cover_focal_x ?? null;
      focalY = collection.featured_cover_focal_y ?? null;
      coverFocalX = collection.cover_focal_x ?? null;
      coverFocalY = collection.cover_focal_y ?? null;
      // `?? null` and NOT `|| null`: a stored zoom of 1 is a real value
      // — "framed, and the answer was the fit" — and truthiness would
      // read it as unset, silently turning an explicit choice into a
      // clear on the next save. Same trap #1081 closed on this table.
      zoom = collection.featured_cover_zoom ?? null;
      coverZoom = collection.cover_zoom ?? null;
      error = null;
      conflict = null;
      focusCoverHandled = false;
      previewLadder.init();
      void loadCoverChoices(collection.id);
    } else {
      // A dialog raised from this one must not outlive it. Left open,
      // it would sit on the modal stack after its host had gone and
      // swallow the next Escape.
      coverEditorOpen = false;
    }
  });

  // `focusCover` (the More-actions "Set cover" entry) opens the EDITOR,
  // not a scroll position (#1207). It used to scroll the form to a
  // picker that lived in the modal; the picker is now its own dialog,
  // and the entry point that says "set cover" should land on the
  // surface that sets covers rather than on the summary of what is
  // already set.
  //
  // Deferred to after the choices have loaded, for the reason the
  // scroll was: opening the editor over an empty grid shows a stage
  // with no pictures for as long as the fetch takes, which reads as
  // "this collection has no covers to choose".
  // The guard is what makes it open ONCE. Without it the editor
  // reopens on the curator's face every time `coverLoading` settles,
  // which includes the moment they close it after a reload.
  let focusCoverHandled = $state(false);
  $effect(() => {
    if (open && focusCover && !coverLoading && !focusCoverHandled) {
      focusCoverHandled = true;
      coverEditorOpen = true;
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
  const summaryPlacement = $derived(coverPlacement(focalX, focalY, zoom));

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
          ...(focalX === null || focalY === null
            ? collection.featured_cover_focal_x != null
              ? { clear_featured_cover_focal: true }
              : {}
            : { featured_cover_focal_x: focalX, featured_cover_focal_y: focalY }),
          ...(coverFocalX === null || coverFocalY === null
            ? collection.cover_focal_x != null
              ? { clear_cover_focal: true }
              : {}
            : { cover_focal_x: coverFocalX, cover_focal_y: coverFocalY }),
          // #1212 — two more tri-states, and every test here is
          // `=== null` / `!= null` rather than truthiness. A zoom of 1
          // is a meaningful stored value that happens to render like
          // the fit, so `zoom ? … : …` would send a clear for a value
          // the curator deliberately chose.
          ...(zoom === null
            ? collection.featured_cover_zoom != null
              ? { clear_featured_cover_zoom: true }
              : {}
            : { featured_cover_zoom: zoom }),
          ...(coverZoom === null
            ? collection.cover_zoom != null
              ? { clear_cover_zoom: true }
              : {}
            : { cover_zoom: coverZoom }),
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

        <div class="flex flex-wrap items-start gap-4 rounded border border-border bg-surface p-3">
          <!-- The featured chip is drawn at the STRIP's aspect with the
               strip's own object-position, so the summary answers "what
               did my positioning do" without opening anything. A square
               chip here would have hidden the very setting the editor
               exists to make. -->
          <figure class="w-32">
            <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
              {t('collections.cover_summary_featured')}
            </figcaption>
            <div class="relative overflow-hidden rounded border border-border bg-surface-elevated"
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
                <div class="flex h-full items-center justify-center text-[10px] text-fg-muted">
                  {t('collections.cover_summary_none')}
                </div>
              {/if}
            </div>
          </figure>

          <figure class="w-20">
            <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
              {t('collections.cover_summary_cover')}
            </figcaption>
            <div class="aspect-square overflow-hidden rounded border border-border bg-surface-elevated">
              {#if coverAssetId}
                <img src={coverUrl(coverAssetId)} alt="" data-testid="cover-summary-collection"
                     class="h-full w-full object-cover" />
              {:else}
                <div class="flex h-full items-center justify-center text-center text-[10px] text-fg-muted">
                  {t('collections.cover_summary_none')}
                </div>
              {/if}
            </div>
          </figure>

          <button
            type="button"
            onclick={() => (coverEditorOpen = true)}
            data-testid="collection-cover-edit-button"
            class="rounded border border-border-strong bg-surface-elevated px-3 py-1.5 text-sm font-medium hover:bg-surface focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          >
            {t('collections.cover_editor_open')}
          </button>
        </div>
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

<!-- Declared OUTSIDE the form modal rather than nested inside its body.
     Both portal to the same place, so the DOM is identical either way —
     but a dialog declared inside the scrolling body of another dialog
     is a dialog whose lifetime is tangled with a scroll container, and
     the one thing this surface must not do is disappear mid-drag.

     Escape steps back exactly one level: Modal only answers the key
     when it is on top of the shared stack (see modalStack.ts), which
     this is the first surface to need. Everything it edits is bound
     back to the state above, so closing it commits nothing and Cancel
     on the form still discards the lot. -->
<CollectionCoverEditor
  open={coverEditorOpen}
  onclose={() => (coverEditorOpen = false)}
  choices={coverChoices}
  loading={coverLoading}
  collectionVisibility={visibility}
  bind:coverAssetId
  bind:featuredCoverAssetId
  bind:focalX
  bind:focalY
  bind:coverFocalX
  bind:coverFocalY
  bind:zoom
  bind:coverZoom
/>
