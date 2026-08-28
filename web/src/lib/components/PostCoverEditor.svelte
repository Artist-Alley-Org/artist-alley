<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The post's cover, and where its square crop is centred (#1210).
  //
  // # Why this surface exists at all
  //
  // A post's cover is chosen once, in the upload composer's thumbnail
  // picker, and after that there was nothing: "Edit post" in the post
  // menu is a stub, and no shipped client sends `PATCH /posts/{id}` at
  // all even though the endpoint has accepted `cover_asset_id` since
  // #946. So an author who wanted a different cover, or wanted the
  // grid tile to stop clipping a face, had no way to say so.
  //
  // # Why the framing lives HERE and not in the composer
  //
  // A focal point can only be honoured over a CONTAIN rung, because the
  // fractions are measured against the original picture and `col` is a
  // square the server already cropped at the centre. At compose time
  // the members were uploaded seconds ago and their raster pass has not
  // drained, so the contain rungs do not exist yet and the stage would
  // be positioning a marquee over a square nothing will paint. Framing
  // is also the thing you do AFTER seeing the card, which is when the
  // clipping is visible.
  //
  // # Scope, stated
  //
  // The cover and its framing, nothing else. This is not the post
  // editor, and it does not pretend to be one: the menu item names the
  // cover, and title / description / tags / visibility are untouched by
  // the PATCH this sends. When the real post editor is built, this
  // becomes a section of it exactly as CollectionCoverEditor is a
  // section of EditCollectionModal (ADR 0091's one-editing-surface
  // ruling); until then a stubbed menu item is not a surface.

  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { t } from '$stores/lang.svelte';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { CROP_STAGE_MIN_WIDTH } from '$lib/util/featuredCrop';
  import CoverCropStage from './CoverCropStage.svelte';
  import Modal from './Modal.svelte';

  interface MemberAsset {
    ladder_available?: boolean;
    preview_available?: boolean;
  }
  interface Member {
    asset_id: string;
    restricted?: boolean;
    asset?: MemberAsset | null;
  }
  interface PostShape {
    id: string;
    title?: string;
    cover_asset_id?: string | null;
    cover_focal_x?: number | null;
    cover_focal_y?: number | null;
    members?: Member[];
    updated_at?: string;
  }

  interface Props {
    post: PostShape;
    open: boolean;
    onclose: () => void;
    /** Called after a successful save so the host can re-read the post.
     *  The host owns the reload because it owns the post object; this
     *  dialog reports that something changed and nothing more. */
    onsaved?: () => void;
  }

  let { post, open, onclose, onsaved }: Props = $props();

  // ⚠️ THE LADDER HAS TO BE ASKED FOR HERE, and this is not defensive
  // tidying. Every other consumer of `previewLadder` sits on a page
  // full of cards, and CardThumb's own `onMount` has already fetched
  // `/previews` by the time it reads the rungs. A post page mounts no
  // cards at all, so without this the store stays empty, `smallestKey()`
  // is null, and the stage silently falls back to `col`, a SQUARE the
  // server already cropped. Driven in a browser that is unmistakable:
  // the marquee comes up square over a 2.4:1 picture, there is no travel
  // on either axis, and Save never enables because nothing can move.
  //
  // `init()` is idempotent and shares one flight, so calling it from a
  // dialog costs nothing where a card already asked.
  onMount(() => previewLadder.init());

  // THE SAME RESOLUTION ORDER THE CARD USES, and it has to be: the
  // marquee is drawn over the picture the tile will paint, so a
  // different fallback here would frame something else. PostCard reads
  // `cover_asset_id`, falling back to the first member.
  const memberIds = $derived((post.members ?? []).filter((m) => !m.restricted).map((m) => m.asset_id));
  const initialCover = $derived(post.cover_asset_id ?? memberIds[0] ?? null);

  // Draft state, seeded on open. Seeded from a $effect keyed on `open`
  // rather than at construction: the dialog is mounted with the host
  // and reopened many times, and a Cancel has to leave the post as it
  // was rather than leaving this component holding the abandoned edit.
  let selectedId = $state<string | null>(null);
  let focalX = $state<number | null>(null);
  let focalY = $state<number | null>(null);
  // Bound so CoverCropStage has somewhere to write, and never sent:
  // `zoomOffered={false}` means nothing can move it off null. See the
  // prop's own note for why the slider is withheld rather than ignored.
  let zoom = $state<number | null>(null);
  let saving = $state(false);
  let error = $state<string | null>(null);
  let seededFor = $state<string | null>(null);

  $effect(() => {
    if (!open) {
      seededFor = null;
      return;
    }
    const key = `${post.id}:${post.updated_at ?? ''}`;
    if (seededFor === key) return;
    seededFor = key;
    selectedId = initialCover;
    focalX = post.cover_focal_x ?? null;
    focalY = post.cover_focal_y ?? null;
    zoom = null;
    error = null;
  });

  // ⚠️ CHANGING THE COVER CLEARS THE FRAMING. A focal fraction is a
  // statement about ONE picture; carrying it onto a different one keeps
  // a number that was chosen against something else, and it would land
  // wherever that number happens to fall on the new subject. Clearing
  // is the honest answer, and it is recoverable in one drag.
  function choose(id: string) {
    if (id === selectedId) return;
    selectedId = id;
    focalX = null;
    focalY = null;
  }

  // ── Which source the stage loads, ASKED rather than assumed ───────
  //
  // The stage must load the picture the CARD loads or the marquee is
  // drawn over the wrong thing: with a ladder the card paints a contain
  // rung at the original aspect, without one it paints `col`, which the
  // server already centre-cropped to a square. A member row carries
  // `ladder_available`, so unlike the collection editor there is
  // nothing to go and ask for: every choice here is a member.
  function ladderFor(assetId: string | null): boolean {
    if (assetId === null) return false;
    return (post.members ?? []).find((m) => m.asset_id === assetId)?.asset?.ladder_available === true;
  }

  function colUrl(assetId: string) {
    return `/api/v1/assets/${assetId}/variants/col`;
  }

  const stageSrc = $derived.by(() => {
    if (selectedId === null) return null;
    if (!ladderFor(selectedId)) return colUrl(selectedId);
    const smallest = previewLadder.smallestKey();
    return smallest ? `/api/v1/assets/${selectedId}/variants/${smallest}` : colUrl(selectedId);
  });
  const stageSrcset = $derived(
    selectedId !== null && ladderFor(selectedId)
      ? (previewLadder.srcsetFor(selectedId) ?? undefined)
      : undefined,
  );

  // ⚠️ A COVER WITH NO CONTAIN RUNG CANNOT BE FRAMED, and the dialog
  // says so instead of offering a marquee whose result the card will
  // drop. CardThumb reaches the same conclusion from the same fact and
  // falls back to `col` centred; this is the editor half of it.
  const framable = $derived(selectedId !== null && ladderFor(selectedId));

  let viewportWidth = $state(0);
  const cropOffered = $derived(viewportWidth >= CROP_STAGE_MIN_WIDTH);

  const dirty = $derived(
    selectedId !== initialCover ||
      (focalX ?? null) !== (post.cover_focal_x ?? null) ||
      (focalY ?? null) !== (post.cover_focal_y ?? null),
  );

  async function save() {
    if (selectedId === null || saving) return;
    saving = true;
    error = null;
    // The pair travels together or the clear flag does, never both:
    // that is the exclusivity rule the server enforces, mirrored here
    // so a save cannot produce a 400 the author cannot act on.
    const framingBody =
      focalX != null && focalY != null
        ? { cover_focal_x: focalX, cover_focal_y: focalY }
        : post.cover_focal_x != null
          ? { clear_cover_focal: true }
          : {};
    try {
      // ⚠️ SAVING PINS THE COVER, even when the author only re-framed.
      // A post with no explicit `cover_asset_id` shows its FIRST MEMBER,
      // which is a picture that can change under it: reorder the
      // members and the cover moves, taking a fraction chosen against
      // the old one with it. A focal point is a statement about ONE
      // picture, so the picture it was chosen against is written down
      // beside it. What the author framed is what the card was already
      // showing, so nothing they can see changes.
      const { error: apiErr } = await api.PATCH('/posts/{id}', {
        params: { path: { id: post.id } },
        body: { cover_asset_id: selectedId, ...framingBody },
      });
      if (apiErr) {
        error = t('post_cover.save_error');
        return;
      }
      onsaved?.();
      onclose();
    } catch {
      error = t('post_cover.save_error');
    } finally {
      saving = false;
    }
  }
</script>

<svelte:window bind:innerWidth={viewportWidth} />

<Modal
  title={t('post_cover.title')}
  {open}
  {onclose}
  panelClass="max-w-3xl"
>
  <div class="flex flex-col gap-4" data-testid="post-cover-editor">
    {#if memberIds.length === 0}
      <p class="text-xs text-fg-muted" data-testid="post-cover-no-members">
        {t('post_cover.no_members')}
      </p>
    {:else}
      {#if stageSrc !== null && framable && cropOffered}
        <!-- THE DESTINATION IS A SQUARE, and unlike the collection tile
             it really is one: grid is the only post surface that crops,
             CardThumb's `fill` is `object-fit: cover` against an
             `aspect-square` frame, and a crop marquee locks to the
             dimensions of the thing that RENDERS it. -->
        <CoverCropStage
          maxHeightVh={34}
          src={stageSrc}
          srcset={stageSrcset}
          sizes="(max-width: 1024px) 90vw, 45vw"
          aspect={1}
          bind:focalX
          bind:focalY
          bind:zoom
          zoomOffered={false}
          testidPrefix="post-crop"
          stageAlt={t('post_cover.stage_alt')}
          cardAlt={t('post_cover.card_alt')}
          cardLabel={t('post_cover.card_label')}
        />
      {:else if stageSrc !== null && !framable}
        <p class="text-xs text-fg-muted" data-testid="post-cover-unframable">
          {t('post_cover.no_contain_rung')}
        </p>
      {:else if stageSrc !== null}
        <!-- The two-dimensional half only, withheld below 768px for the
             reason CROP_STAGE_MIN_WIDTH records: at the stage's floor
             the marquee's short side is under the comfortable target
             size, and WCAG 2.2 SC 1.4.10 exempts content that needs a
             two-dimensional layout to be usable. Choosing the cover
             still works, and an existing framing is neither read nor
             written here, so a save from a phone re-sends what a
             desktop set. -->
        <p class="text-xs text-fg-muted" data-testid="post-cover-narrow">
          {t('post_cover.narrow_screen')}
        </p>
      {/if}

      <div>
        <p class="mb-2 text-[10px] uppercase tracking-wide text-fg-muted">
          {t('post_cover.pick_label')}
        </p>
        <div class="grid grid-cols-[repeat(auto-fill,minmax(4.5rem,1fr))] gap-2" data-testid="post-cover-choices">
          {#each memberIds as id (id)}
            <button
              type="button"
              onclick={() => choose(id)}
              aria-pressed={selectedId === id}
              data-testid="post-cover-choice"
              data-asset-id={id}
              class="aspect-square overflow-hidden rounded border-2 hover:border-border-strong"
              class:border-accent={selectedId === id}
              class:border-border={selectedId !== id}
            >
              <img src={colUrl(id)} alt="" loading="lazy" class="h-full w-full object-cover" />
            </button>
          {/each}
        </div>
      </div>
    {/if}

    {#if error}
      <p role="alert" class="text-xs text-danger" data-testid="post-cover-error">{error}</p>
    {/if}
  </div>

  {#snippet footer()}
    <button
      type="button"
      onclick={onclose}
      class="rounded border border-border px-3 py-1.5 text-sm hover:bg-surface"
    >
      {t('common.cancel')}
    </button>
    <button
      type="button"
      onclick={save}
      disabled={saving || !dirty || selectedId === null}
      data-testid="post-cover-save"
      class="rounded bg-accent px-3 py-1.5 text-sm text-on-accent disabled:cursor-not-allowed disabled:opacity-50"
    >
      {saving ? t('common.saving') : t('common.save')}
    </button>
  {/snippet}
</Modal>
