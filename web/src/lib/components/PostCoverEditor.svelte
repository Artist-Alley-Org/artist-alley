<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // THE COVER BLOCK of the post editor: the post's cover, and where its
  // square crop is centred (#1210, folded into the editor for #1119).
  //
  // # It is no longer a dialog, and that was always the plan
  //
  // This shipped as a standalone `<dialog>` raised from its own menu
  // item, with its own PATCH, because "Edit post" was a stub and a
  // stubbed menu item is not a surface. The note it carried said what
  // would happen when the real editor arrived: "this becomes a section
  // of it exactly as CollectionCoverEditor is a section of
  // EditCollectionModal (ADR 0091's one-editing-surface ruling)". That
  // editor has arrived, so this is that section.
  //
  // What went with the dialog is the second door. #1264 settled that
  // argument on the collection side ("I really think we shouldn't have
  // more than one menu to edit collections"), and the post menu had
  // grown the same pair: "Cover and framing…" beside "Edit post…", one
  // of them real and one of them an alert(). One surface now, one Save,
  // one concurrency baseline.
  //
  // What did NOT change is the framing semantics, and none of it may:
  //   * the resolution order is the CARD's (cover_asset_id, else first
  //     member), because the marquee is drawn over the picture the tile
  //     will paint;
  //   * a cover with no CONTAIN rung cannot be framed and says so,
  //     rather than offering a marquee whose result the card drops
  //     (ADR 0088's fall-back-rather-than-blank rule, editor half);
  //   * changing the cover DISCARDS the old framing, because a focal
  //     fraction is a statement about one particular picture;
  //   * the two-dimensional stage is withheld below CROP_STAGE_MIN_WIDTH
  //     while choosing the cover keeps working.
  //
  // # Why the framing lives HERE and not in the upload composer
  //
  // A focal point can only be honoured over a CONTAIN rung, because the
  // fractions are measured against the original picture and `col` is a
  // square the server already cropped at the centre. At compose time the
  // members were uploaded seconds ago and their raster pass has not
  // drained, so the contain rungs do not exist yet and the stage would
  // be positioning a marquee over a square nothing will paint. Framing
  // is also the thing you do AFTER seeing the card, which is when the
  // clipping is visible.

  import { onMount } from 'svelte';
  import { t } from '$stores/lang.svelte';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { CROP_STAGE_MIN_WIDTH } from '$lib/util/featuredCrop';
  import CoverCropStage from './CoverCropStage.svelte';

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
    /** The post being edited, as the host SNAPSHOTTED it on open
     *  (#1262's rule, applied here): the members are the picker's
     *  options and the stored cover is what "unchanged" means, and both
     *  have to come from the same instant as the host's
     *  `if_unchanged_since`. */
    post: PostShape;
    /** The chosen cover. Null only before the host has seeded it; a post
     *  with members always resolves to one, because the card does. */
    coverAssetId: string | null;
    /** The framing, owned by the host and mutated in place through the
     *  deep `$state` proxy. The same by-reference arrangement
     *  CollectionCoverEditor uses, and for the same reason: it is what
     *  makes "one Save applies the cover AND its framing" true with no
     *  sync effect anywhere.
     *
     *  Null is CENTRE and is distinct from 0.5, which is what makes
     *  Reset a clear rather than a re-set (migration 00055's CHECK). */
    framing: { x: number | null; y: number | null };
    /** Viewport width, measured by the host. Below CROP_STAGE_MIN_WIDTH
     *  the two-dimensional half is withheld. See the stage block. */
    viewportWidth: number;
  }

  let { post, coverAssetId = $bindable(), framing, viewportWidth }: Props = $props();

  // ⚠️ THE LADDER HAS TO BE ASKED FOR HERE, and this is not defensive
  // tidying. Every other consumer of `previewLadder` sits on a page full
  // of cards, and CardThumb's own `onMount` has already fetched
  // `/previews` by the time it reads the rungs. A post page mounts no
  // cards at all, so without this the store stays empty, `smallestKey()`
  // is null, and the stage silently falls back to `col`, a SQUARE the
  // server already cropped. Driven in a browser that is unmistakable:
  // the marquee comes up square over a 2.4:1 picture, there is no travel
  // on either axis, and Save never enables because nothing can move.
  //
  // `init()` is idempotent and shares one flight, so calling it from
  // here costs nothing where a card already asked.
  //
  // ⛔ IT IS NOT IN A $effect. That is the #1262 trap from the collection
  // side: `previewLadder.init()`'s first statement reads a `$state`
  // guard, so an effect that calls it depends on `previewLadder.loaded`
  // and re-runs when the fetch lands. `onMount` runs once, untracked.
  onMount(() => previewLadder.init());

  // THE SAME RESOLUTION ORDER THE CARD USES, and it has to be: the
  // marquee is drawn over the picture the tile will paint, so a
  // different fallback here would frame something else. PostCard reads
  // `cover_asset_id`, falling back to the first member.
  // The picker's options. ⚠️ THE HOST RESOLVES THE INITIAL COVER FROM
  // THE SAME LIST (`resolveCover`), in PostCard's order: cover_asset_id
  // then first member. So the section never seeds itself, and the value
  // the author sees is the value the host will compare against.
  const memberIds = $derived(
    (post.members ?? []).filter((m) => !m.restricted).map((m) => m.asset_id),
  );

  // ⚠️ CHANGING THE COVER CLEARS THE FRAMING (#1333). A focal fraction
  // is a statement about ONE picture; carrying it onto a different one
  // keeps a number that was chosen against something else, and it would
  // land wherever that number happens to fall on the new subject.
  // Clearing is the honest answer, and it is recoverable in one drag.
  //
  // The SERVER does the same thing to the stored columns for the same
  // reason, so this is the client half of one rule rather than a second
  // one: send `cover_asset_id` without a pair and the pair goes to null.
  // The host's save relies on that; see its `coverBody`.
  function choose(id: string) {
    if (id === coverAssetId) return;
    coverAssetId = id;
    framing.x = null;
    framing.y = null;
  }

  // ── Which source the stage loads, ASKED rather than assumed ───────
  //
  // The stage must load the picture the CARD loads or the marquee is
  // drawn over the wrong thing: with a ladder the card paints a contain
  // rung at the original aspect, without one it paints `col`, which the
  // server already centre-cropped to a square. A member row carries
  // `ladder_available`, so unlike the collection editor there is nothing
  // to go and ask for: every choice here is a member.
  function ladderFor(assetId: string | null): boolean {
    if (assetId === null) return false;
    return (
      (post.members ?? []).find((m) => m.asset_id === assetId)?.asset?.ladder_available === true
    );
  }

  function colUrl(assetId: string) {
    return `/api/v1/assets/${assetId}/variants/col`;
  }

  const stageSrc = $derived.by(() => {
    if (coverAssetId === null) return null;
    if (!ladderFor(coverAssetId)) return colUrl(coverAssetId);
    const smallest = previewLadder.smallestKey();
    return smallest ? `/api/v1/assets/${coverAssetId}/variants/${smallest}` : colUrl(coverAssetId);
  });
  const stageSrcset = $derived(
    coverAssetId !== null && ladderFor(coverAssetId)
      ? (previewLadder.srcsetFor(coverAssetId) ?? undefined)
      : undefined,
  );

  // ⚠️ A COVER WITH NO CONTAIN RUNG CANNOT BE FRAMED, and the section
  // says so instead of offering a marquee whose result the card will
  // drop. CardThumb reaches the same conclusion from the same fact and
  // falls back to `col` centred; this is the editor half of it.
  const framable = $derived(coverAssetId !== null && ladderFor(coverAssetId));
  const cropOffered = $derived(viewportWidth >= CROP_STAGE_MIN_WIDTH);

  // Bound so CoverCropStage has somewhere to write, and NEVER sent:
  // `zoomOffered={false}` means nothing can move it off null, and a post
  // has no zoom column to send it to (a collection has two). It is local
  // rather than part of the host's `framing` object precisely because the
  // host must not be able to put it in a PATCH body. See the prop's own
  // note for why the slider is withheld rather than ignored.
  let zoom = $state<number | null>(null);
</script>

<div class="flex flex-col gap-4" data-testid="post-cover-editor">
  {#if memberIds.length === 0}
    <p class="text-xs text-fg-muted" data-testid="post-cover-no-members">
      {t('post_cover.no_members')}
    </p>
  {:else}
    {#if stageSrc !== null && framable && cropOffered}
      <!-- THE DESTINATION IS A SQUARE, and unlike the collection tile it
           really is one: grid is the only post surface that crops,
           CardThumb's `fill` is `object-fit: cover` against an
           `aspect-square` frame, and a crop marquee locks to the
           dimensions of the thing that RENDERS it. -->
      <CoverCropStage
        maxHeightVh={34}
        src={stageSrc}
        srcset={stageSrcset}
        sizes="(max-width: 1024px) 90vw, 45vw"
        aspect={1}
        bind:focalX={framing.x}
        bind:focalY={framing.y}
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
           reason CROP_STAGE_MIN_WIDTH records: at the stage's floor the
           marquee's short side is under the comfortable target size, and
           WCAG 2.2 SC 1.4.10 exempts content that needs a
           two-dimensional layout to be usable. Choosing the cover still
           works, and an existing framing is neither read nor written
           here, so a save from a phone re-sends what a desktop set. -->
      <p class="text-xs text-fg-muted" data-testid="post-cover-narrow">
        {t('post_cover.narrow_screen')}
      </p>
    {/if}

    <div>
      <p class="mb-2 text-[10px] uppercase tracking-wide text-fg-muted">
        {t('post_cover.pick_label')}
      </p>
      <div
        class="grid grid-cols-[repeat(auto-fill,minmax(4.5rem,1fr))] gap-2"
        data-testid="post-cover-choices"
      >
        {#each memberIds as id (id)}
          <button
            type="button"
            onclick={() => choose(id)}
            aria-pressed={coverAssetId === id}
            data-testid="post-cover-choice"
            data-asset-id={id}
            class="aspect-square overflow-hidden rounded border-2 hover:border-border-strong"
            class:border-accent={coverAssetId === id}
            class:border-border={coverAssetId !== id}
          >
            <img src={colUrl(id)} alt="" loading="lazy" class="h-full w-full object-cover" />
          </button>
        {/each}
      </div>
    </div>
  {/if}
</div>
