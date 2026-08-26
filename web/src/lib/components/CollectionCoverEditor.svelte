<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // THE COVER BLOCK of the collection edit modal: one cover slot, in
  // full (#1207, re-shaped by #1213, un-paged by #1264).
  //
  // WHY THE PICTURE NEEDS ROOM. #1207's finding was that the cover
  // surface "is too small and I can't really see how the cover images
  // will be cropped". Cropping is a judgement about a picture, and
  // making it in a 200px thumbnail is the defect.
  //
  // ⚠️ IT IS NEITHER A DIALOG NOR A PAGE ANY MORE. #1207 got that room
  // by raising a second `<dialog>` over the edit modal, which the owner
  // questioned directly — "why is edit collection and edit collection
  // cover two different modals that overlap. Why not one modal?" —
  // and #1213 answered it by making this page 2 of that one dialog.
  // #1264 is the same finding one level in: a page that is reached and
  // left is still a second surface, and stepping between the two
  // measured a 896x586 → 1536x1036 jump. So this is a BLOCK of the one
  // surface now — always on screen, beside the collection's identity,
  // sharing its Save. Everything the second dialog was for survives;
  // the second `<dialog>`, its portal-over-portal, its share of #1208's
  // Escape race and the page switch do not.
  //
  // ONE SLOT PER VISIT, and that is what makes it one block instead of
  // two. A collection cover and a featured-rail cover want different
  // pictures (#1200) and different destination shapes, but they want
  // the SAME surface: pick, crop, position, zoom. The `coverSlot` prop is
  // the only thing they disagree about, and everything derived from it
  // — the aspect, which asset id is being written, which framing triple
  // is bound, which picker state is in play — is resolved once, below.
  // Two copies of this markup is how the two slots drifted apart the
  // first time (one had a "same as cover" tile where the other had "use
  // mosaic", and nothing else differed).
  //
  // THE FEATURED SLOT DEFAULTS TO THE COLLECTION COVER rather than
  // starting empty. "No separate choice" is the common case and it is
  // also what the rail's fallback chain does, so the editor shows the
  // picture the rail would actually use instead of an empty box that
  // means "look elsewhere".
  //
  // IT OWNS NO SAVE. Every value here belongs to EditCollectionModal,
  // which has the collection, the concurrency baseline and the single
  // PATCH. One dialog now means one save, applying both pages in one
  // action — a page that wrote its own two fields would give the
  // curator two commits with different failure modes and two chances to
  // hit a 409.

  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { DEFAULT_ASSET_TYPE } from '$stores/upload.svelte';
  import { putStorageObject } from '$lib/util/storageUpload';
  import CoverCropStage from './CoverCropStage.svelte';
  import {
    COVER_SLOT_ASPECT,
    CROP_STAGE_MIN_WIDTH,
    type CoverSlot,
  } from '$lib/util/featuredCrop';

  /** One choosable picture. Mirrors EditCollectionModal's CoverChoice —
   *  the same rows, handed down rather than re-fetched, so both slots
   *  and the summary behind them are looking at one list. */
  interface CoverChoice {
    asset_id: string;
    restricted?: boolean;
    preview_available?: boolean;
    ladder_available?: boolean;
  }

  interface Props {
    /** WHICH SLOT THIS VISIT IS FOR. The page's single parameter.
     *  Named `coverSlot` rather than `slot`: `slot` is a global HTML
     *  attribute and was Svelte 4's own element name, and it is also
     *  what the picker's per-slot state was called — three meanings for
     *  one identifier on one surface is how the wrong one gets read. */
    coverSlot: CoverSlot;
    choices: CoverChoice[];
    loading: boolean;
    /** The collection's own tier. It decides what an uploaded cover's
     *  `status` has to be for the collection's audience to see it, and
     *  whether picking a narrower asset is worth warning about. */
    collectionVisibility: string;
    /** The collection cover, null for the derived mosaic. */
    coverAssetId: string | null;
    /** The featured-rail cover, null for "same as the collection cover". */
    featuredCoverAssetId: string | null;
    /** BOTH SLOTS' framing, keyed by slot, owned above and mutated in
     *  place through the deep `$state` proxy.
     *
     *  Keyed rather than passed one slot at a time so the binding target
     *  can be written once — `framing[coverSlot].x` — instead of the page
     *  needing a differently-named prop per visit, which is exactly the
     *  thing that would have forced two copies of the markup.
     *
     *  A focal pair is null for centre and a zoom is null for the fit;
     *  both stay distinct from the neutral numbers (migrations 00055
     *  and 00056), which is what makes Reset a clear. */
    framing: Record<CoverSlot, { x: number | null; y: number | null; zoom: number | null }>;
    /** Viewport width, measured by the host. Below CROP_STAGE_MIN_WIDTH
     *  the two-dimensional half of this page is withheld — see the
     *  stage block in the markup for the reflow argument. */
    viewportWidth: number;
  }

  let {
    coverSlot,
    choices,
    loading,
    collectionVisibility,
    coverAssetId = $bindable(),
    featuredCoverAssetId = $bindable(),
    framing,
    viewportWidth,
  }: Props = $props();

  // ── Which picture each slot is showing ─────────────────────────────
  //
  // The featured slot falls back to the collection cover exactly as the
  // RAIL does. That is the point: what this box shows is what the strip
  // will show, and if the fallback were spelled out differently here
  // the editor would be a second opinion about the rail's own rule.
  const featuredEffectiveId = $derived(featuredCoverAssetId ?? coverAssetId);
  const featuredIsInherited = $derived(featuredCoverAssetId === null && coverAssetId !== null);

  // ── EVERYTHING THE SLOT DECIDES, RESOLVED ONCE (#1213) ────────────
  //
  // The whole difference between the two visits, in one place, so the
  // markup below reads as one surface with parameters rather than as
  // two surfaces that happen to look alike. `isFeatured` appears in
  // exactly three places past this point — the inherited hint, the
  // "back to the collection cover" action and which id the picker
  // writes — and each is a real product difference rather than a
  // divergence to tidy away.
  const isFeatured = $derived(coverSlot === 'featured');
  const aspect = $derived(COVER_SLOT_ASPECT[coverSlot]);
  /** The id the PICKER is choosing for. Null on the featured slot means
   *  "inherit the collection cover", which is a real selection. */
  const selectedId = $derived(isFeatured ? featuredCoverAssetId : coverAssetId);
  /** The id actually SHOWN, after the featured slot's fallback. */
  const shownId = $derived(isFeatured ? featuredEffectiveId : coverAssetId);
  function assign(id: string | null) {
    if (isFeatured) featuredCoverAssetId = id;
    else coverAssetId = id;
  }

  function choiceFor(assetId: string | null): CoverChoice | null {
    if (assetId === null) return null;
    return choices.find((c) => c.asset_id === assetId) ?? null;
  }

  function colUrl(assetId: string) {
    return `/api/v1/assets/${assetId}/variants/col`;
  }

  // ⚠️ THE EDITOR HAS TO LOAD THE SAME PICTURE THE CARD LOADS, or the
  // marquee is drawn over the wrong thing. FeaturedRail picks between
  // two sources with DIFFERENT shapes:
  //
  //   - with a ladder for this asset, a `contain` rung — the original
  //     aspect, so the card's crop is the only crop;
  //   - without one, `col` — which the server has ALREADY centre-cropped
  //     to a square, so the card crops a square, not the original.
  //
  // Previewing `col` in both cases would tell a portrait cover's curator
  // their picture crops to a wide band of a square that does not exist.
  // Carried forward from #1195's preview, and load-bearing twice over
  // now that the curator is POSITIONING against it: a focal fraction
  // chosen against the wrong source lands somewhere else on the strip.
  //
  // ── Which source, ASKED rather than assumed (#1207/#1074) ──────────
  //
  // `ladder_available` decides the branch above, and a member row
  // carries it. A NON-MEMBER does not — and since #1074 opened the
  // picker to the curator's whole library, and to pictures that did not
  // exist a second ago, the non-member case is the ordinary one.
  //
  // Guessing "no ladder" for those was wrong in a way that is invisible
  // until you compare the editor with the strip, which is exactly what
  // driving this in a browser did: the editor drew its marquee over
  // `col` — a 320px SQUARE the server centre-cropped — while the rail
  // loaded a contain rung at the picture's ORIGINAL 2.4:1, so the
  // curator positioned a crop on one picture and the strip cropped
  // another. The focal fraction is meaningless across that.
  //
  // So the flag is resolved from the server for anything the local rows
  // cannot answer. `ladderRequested` is a PLAIN Set, deliberately not
  // `$state`: the effect below reads the reactive map, and a reactive
  // in-flight marker would be a write the effect depends on — the
  // 1,694-request feedback loop shape.
  const ladderKnown = $state<Record<string, boolean>>({});
  const ladderRequested = new Set<string>();

  function ladderFor(assetId: string | null): boolean | undefined {
    if (assetId === null) return undefined;
    const member = choiceFor(assetId);
    if (member) return member.ladder_available === true;
    const found =
      featuredPicker.results.find((r) => r.id === assetId) ??
      coverPicker.results.find((r) => r.id === assetId);
    if (found?.ladder_available !== undefined) return found.ladder_available;
    return ladderKnown[assetId];
  }

  async function ensureLadder(assetId: string) {
    if (ladderRequested.has(assetId)) return;
    ladderRequested.add(assetId);
    try {
      const { data } = await api.GET('/assets/{id}', { params: { path: { id: assetId } } });
      ladderKnown[assetId] = (data as { ladder_available?: boolean } | undefined)?.ladder_available === true;
    } catch {
      // Unknown resolves to `col`, the card's own worst case — an
      // optimistic guess would request rungs that 404.
      ladderKnown[assetId] = false;
    }
  }

  $effect(() => {
    for (const id of [featuredEffectiveId, coverAssetId]) {
      if (id && ladderFor(id) === undefined) void ensureLadder(id);
    }
  });

  /** Bumped when a freshly uploaded picture's renditions arrive, purely
   *  to defeat a cached 404 on an unchanged URL. Applied to the STAGE
   *  source only — the picker's thumbnail grid is not what the curator
   *  is waiting on, and busting every thumb would re-fetch the whole
   *  grid for one picture's sake. */
  let reloadNonce = $state(0);

  function withNonce(url: string): string {
    return reloadNonce === 0 ? url : `${url}?r=${reloadNonce}`;
  }

  function srcFor(assetId: string | null): string | null {
    if (assetId === null) return null;
    if (ladderFor(assetId) !== true) return withNonce(colUrl(assetId));
    const smallest = previewLadder.smallestKey();
    return smallest
      ? withNonce(`/api/v1/assets/${assetId}/variants/${smallest}`)
      : withNonce(colUrl(assetId));
  }

  function srcsetFor(assetId: string | null): string | undefined {
    if (assetId === null || ladderFor(assetId) !== true) return undefined;
    return previewLadder.srcsetFor(assetId) ?? undefined;
  }

  const stageSrc = $derived(srcFor(shownId));
  const stageSrcset = $derived(srcsetFor(shownId));

  /** A cover chosen from outside the member list still has to show as
   *  the current selection, or an unrelated edit would look like it had
   *  cleared the cover. */
  function isExternal(assetId: string | null) {
    return assetId !== null && !choices.some((c) => c.asset_id === assetId);
  }

  // ── Beyond the members: my files, and a fresh upload (#1207/#1074) ─
  //
  // The API has ALWAYS accepted any asset the curator may picture — the
  // write gate is CallerMayPictureAsset, not a membership check, and
  // #1027 chose that deliberately so a cover can outlive the member it
  // was picked from. What did not exist was a way to SAY it: the picker
  // offered members only, so the documented capability was reachable by
  // curl and not by curator. That is #1074, and closing it needs no
  // backend change at all — verified against the handler, not inferred
  // from the issue text.
  //
  // Two more sources, then:
  //
  //   members  — the collection's own, still the first thing offered
  //   mine     — a search over the curator's own assets
  //   upload   — a picture that does not exist yet
  //
  // "Mine" rather than "everything": an instance-wide asset browser is
  // a different product (#1074 says so), and the owner's framing is
  // personal storage (#1161) — the pictures a curator would reach for
  // are their own. `owner_ref` + `q` are both already on GET /assets.
  type PickerSource = 'members' | 'mine' | 'upload';

  interface MineResult {
    id: string;
    title?: string;
    status?: string;
    /** Carried through from GET /assets so a picked non-member gets the
     *  SAME source the rail will load — see ladderFor. */
    ladder_available?: boolean;
  }

  /** Per-slot picker state. Two slots, two independent searches: the
   *  curator is choosing two different pictures and a shared query box
   *  would reset one while they were using the other. */
  interface SlotPicker {
    source: PickerSource;
    query: string;
    results: MineResult[];
    searching: boolean;
    uploading: boolean;
    uploadError: string | null;
  }

  function newSlotPicker(): SlotPicker {
    return {
      source: 'members',
      query: '',
      results: [],
      searching: false,
      uploading: false,
      uploadError: null,
    };
  }

  const featuredPicker = $state<SlotPicker>(newSlotPicker());
  const coverPicker = $state<SlotPicker>(newSlotPicker());

  async function searchMine(pick: SlotPicker) {
    const me = auth.user?.ref;
    if (me == null) return;
    pick.searching = true;
    try {
      const { data } = await api.GET('/assets', {
        params: {
          query: {
            owner_ref: me,
            // An empty `q` is OMITTED rather than sent as '': the
            // server ANDs whitespace-separated tokens through
            // plainto_tsquery, and an empty query there matches
            // nothing — so sending it would make "show me my files"
            // return an empty grid and read as "you have no files".
            ...(pick.query.trim() ? { q: pick.query.trim() } : {}),
            limit: 48,
          },
        },
      });
      pick.results = ((data?.items ?? []) as unknown as MineResult[]) ?? [];
    } catch {
      // A search that failed must not take the dialog down with it —
      // the member grid and the marquee are still usable.
      pick.results = [];
    } finally {
      pick.searching = false;
    }
  }

  /** Upload a picture and choose it, in one gesture.
   *
   *  Two calls, both already in the codebase: the bytes go through the
   *  SHARED storage-upload primitive (see $lib/util/storageUpload —
   *  extracted here rather than copied, because the X-Content-Type
   *  header and `withCredentials` are invisible from a call site and a
   *  second copy is a second place to lose one), and the asset row is
   *  an ordinary POST /assets. No bespoke cover-upload endpoint, which
   *  is the same argument migration 00046 makes about the pointer: an
   *  ordinary asset inherits storage, renditions, permissions,
   *  federation and GC for free.
   *
   *  ⚠️ `status` IS THE VISIBILITY DECISION, and it is the axis that
   *  actually governs here.
   *
   *  The owner's rule is that an uploaded cover should match the
   *  collection's reach, so it does not silently fall back for wider
   *  audiences. The axis that decides this is NOT `sensitivity` —
   *  `assets.sensitivity` defaults to `public`, the widest tier, and no
   *  API sets it (the `SetAssetSensitivity` query has zero Go callers).
   *  It is `status`: the ANONYMOUS asset predicate requires
   *  `status = 'active'`, while the authenticated arm requires only
   *  that the row is not deleted.
   *
   *  So:
   *    public collection    → 'active'. Anonymous readers are the
   *                           audience, and a draft cover is exactly
   *                           the silent fallback the rule exists to
   *                           prevent.
   *    anything narrower    → 'draft'. Its audience is signed in and
   *                           sees the picture either way, so the
   *                           conservative status is free — and it
   *                           keeps a banner for a private collection
   *                           out of anonymous reach.
   *
   *  The upload queue's own default is 'draft' for every file, which is
   *  right for a queue whose files are on their way to becoming posts
   *  and wrong for a picture whose entire purpose is to be looked at.
   */
  async function uploadAndChoose(pick: SlotPicker, file: File, choose: (id: string) => void) {
    pick.uploading = true;
    pick.uploadError = null;
    try {
      const { hash } = await putStorageObject(file, {
        networkMessage: t('upload.err_network'),
        abortMessage: t('upload.err_aborted'),
      });
      const { data, error } = await api.POST('/assets', {
        body: {
          title: file.name.replace(/\.[^.]+$/, '') || file.name,
          asset_type: DEFAULT_ASSET_TYPE,
          status: collectionVisibility === 'public' ? 'active' : 'draft',
          file_hash: hash,
          file_extension: file.name.includes('.') ? file.name.split('.').pop() : undefined,
          mature: false,
        },
      });
      if (error || !data) {
        pick.uploadError =
          (error as { error?: string } | undefined)?.error ?? t('collections.cover_editor_upload_failed');
        return;
      }
      choose(data.id);
      // Straight back to the member grid: the picture is chosen, and
      // leaving the upload pane open would imply there is another step.
      pick.source = 'members';
      // Renditions are produced asynchronously, so the picture the
      // curator just chose does not exist yet and neither does its
      // ladder. Poll until it does, so the stage stops saying "still
      // being prepared" and — the part that matters — switches to the
      // SAME rung the strip will load, instead of leaving the marquee
      // over a `col` square while the rail crops the original.
      void awaitRenditions(data.id);
    } catch (e) {
      pick.uploadError = e instanceof Error ? e.message : t('collections.cover_editor_upload_failed');
    } finally {
      pick.uploading = false;
    }
  }

  /** Wait for a freshly uploaded picture's renditions, bounded.
   *
   *  Bounded because a failed raster job never becomes ready and a
   *  loop with no end would poll for the life of the dialog. When it
   *  gives up the editor keeps showing "still being prepared", which is
   *  the honest state: the picture IS stored and IS the chosen cover,
   *  and the strip will show it whenever the worker succeeds. */
  async function awaitRenditions(assetId: string) {
    for (let attempt = 0; attempt < 20; attempt++) {
      await new Promise((r) => setTimeout(r, 1500));
      try {
        const { data } = await api.GET('/assets/{id}', { params: { path: { id: assetId } } });
        const a = data as { preview_available?: boolean; ladder_available?: boolean } | undefined;
        if (a?.preview_available) {
          ladderKnown[assetId] = a.ladder_available === true;
          // Bump the nonce so the stage RE-REQUESTS. Resolving the
          // ladder usually changes the source on its own (a `contain`
          // rung replaces `col`), but not always — an asset whose ladder
          // stays unavailable keeps the same `col` URL, and a browser
          // that has already cached that 404 will not retry a URL it
          // believes it knows the answer for. The nonce is what makes
          // the retry unconditional.
          reloadNonce += 1;
          return;
        }
      } catch {
        return;
      }
    }
  }

  function onFilePicked(e: Event, pick: SlotPicker, choose: (id: string) => void) {
    const input = e.currentTarget as HTMLInputElement;
    const file = input.files?.[0];
    // Cleared so choosing the SAME file twice fires `change` again —
    // an input that keeps its value is silent on a re-pick, which reads
    // as "the upload button stopped working".
    input.value = '';
    if (file) void uploadAndChoose(pick, file, choose);
  }

  /** Would a picked asset be invisible to some of the collection's
   *  audience?
   *
   *  ⚠️ PARTIAL, AND SAID SO RATHER THAN OVERSTATED. A public
   *  collection is read by anonymous visitors, whose asset predicate
   *  requires `status = 'active'` AND `sensitivity = 'public'` AND
   *  `processing_status = 'ready'`. This can only check the first: the
   *  Asset schema carries `status` and deliberately carries no
   *  `sensitivity`, so a team-tier asset picked as a public
   *  collection's cover still falls back on the rail with no warning
   *  here. Answering that exactly needs a derived
   *  "anonymously picturable" flag from the server; it is NOT inferred
   *  client-side, because inferring it is how a warning starts lying in
   *  the reassuring direction.
   *
   *  Nothing is BLOCKED by this. The rail's per-rung fallback is the
   *  safety property and it is unchanged — this only makes the fallback
   *  explicable instead of mysterious. */
  function narrowerThanCollection(assetId: string | null, pick: SlotPicker): boolean {
    if (assetId === null || collectionVisibility !== 'public') return false;
    const found =
      pick.results.find((r) => r.id === assetId) ??
      // A member row carries no status, so a member picked from the
      // first grid produces no warning either way. That is the honest
      // answer, not a silent pass: the check has nothing to read.
      null;
    return found !== null && found.status !== undefined && found.status !== 'active';
  }

  /** The picker state for THIS visit. Declared here rather than beside
   *  the other slot-derived values because the two picker objects are
   *  themselves declared further down — and they are per-slot so that a
   *  curator who searched their files for one cover comes back to that
   *  search, not to an empty grid, after visiting the other slot. */
  const activePicker = $derived(isFeatured ? featuredPicker : coverPicker);

  const warning = $derived(narrowerThanCollection(selectedId, activePicker));

  // ── The two-dimensional half, and when it is offered (#1213) ──────
  const cropOffered = $derived(viewportWidth >= CROP_STAGE_MIN_WIDTH);

  // ── TWO REGIONS, AND WHICH ONE IS GROWING (#1218 → #1220) ─────────
  //
  // #1218's finding on the shipped page: "we are still not using the
  // space properly" — a 720px dialog in a 1130px viewport, a whole row
  // spent on one sentence of hint, and a picker clipped to one row of
  // thumbnails with the next peeking, while the lower half sat empty
  // because the crop stage's space was reserved for a picture nobody
  // had chosen yet. The answer was a column that FILLED a dialog of
  // stated height, with exactly one of its two regions growing.
  //
  // ⛔ #1220 IS THE MIRROR OF THAT AND IT OVERTURNS THE MECHANISM, not
  // the shape. Growing INTO a stated box is only right while there is
  // something to fill it: a collection yielding 59 picturable tiles
  // filled ~790px of an allocated ~1190px and left a ~400px dead band
  // under the last row. So the rule is "sized by content, up to a cap",
  // and nothing here takes a definite height any more. What survives
  // unchanged is which region is the big one:
  //
  //   nothing chosen  → there is no stage, and the picker is the grid.
  //                     Reserving room for a picture that does not
  //                     exist is what produced the dead half.
  //   a picture chosen→ the stage takes a real budget and the picker
  //                     collapses to a rail. The stage is the reason
  //                     this block exists; the picker's job is done the
  //                     moment it is used, but it stays reachable for a
  //                     change of mind.
  //
  // `stageShown` is the single answer both regions read, so they cannot
  // disagree about which one is big — the failure that would give the
  // dialog two scrollbars or none.
  const stageShown = $derived(shownId !== null && stageSrc !== null && cropOffered);

  /** The choice grid, in its two states.
   *
   *  GRID: `max-height` and NOT `flex-1` (#1220). A cap lets a dense
   *  grid scroll inside itself — 204 tiles must not make the dialog
   *  thousands of pixels tall — while a sparse one simply ends where
   *  its last row does, which is what leaves no band to be dead.
   *  `auto-rows-min` keeps the tiles square: without it the implicit
   *  rows stretch and a thumbnail of a square rendition is drawn as a
   *  tall rectangle. `content-start` is its companion for the sparse
   *  case — otherwise a single row of tiles is centred in whatever room
   *  the box happens to have.
   *
   *  ⚠️ `auto-fit`, NOT A COLUMN COUNT, and that is the horizontal half
   *  of #1220. A fixed `lg:grid-cols-10` gives five tiles five tenths of
   *  the row and leaves the other half of a bordered box empty — the
   *  same dead area the issue is about, turned ninety degrees. `auto-fit`
   *  collapses the tracks nothing occupies, so a sparse collection's few
   *  pictures are shown LARGE across the width they have, and a dense
   *  one packs to the 5rem minimum exactly as a column count would.
   *
   *  RAIL: one row, scrolling sideways. `max-h-40` (the old fixed
   *  window, kept for exactly this state) would show one row and a
   *  sliver of the next, which is the "clipped" reading the owner
   *  reported; a rail shows one row and says so. */
  function gridClass(grow: boolean): string {
    return grow
      ? 'grid max-h-[44vh] auto-rows-min content-start gap-2 overflow-y-auto rounded ' +
        'border border-border p-1 grid-cols-[repeat(auto-fit,minmax(5rem,1fr))]'
      : 'flex shrink-0 gap-2 overflow-x-auto rounded border border-border p-1';
  }

  /** One tile. In the grid the column decides the width and
   *  `aspect-square` decides the height; in the rail there is no column
   *  to take a width from, so both are stated. */
  function tileClass(grow: boolean): string {
    return grow ? 'relative aspect-square' : 'relative h-16 w-16 shrink-0';
  }
</script>

<!-- ONE picker, rendered twice (#1207/#1074).
     Both slots choose from the same three sources on the same terms;
     a second copy of the grid was already drifting (the featured one
     had a "same as cover" tile where the other had "use mosaic", and
     nothing else differed) and the search and upload arms would have
     doubled it again. `assign` is what the two slots actually
     disagree about, so it is the parameter. -->
{#snippet picker(
  pick: SlotPicker,
  selected: string | null,
  choose: (id: string | null) => void,
  testidPrefix: string,
  noneLabel: string,
  warn: boolean,
  /** GROW, or step aside (#1218). True is "no picture is chosen, so
   *  this is the whole page"; false is "the stage has it, keep a rail".
   *  The two states differ in the CHOICE GRID only — the source tabs,
   *  the search box and the upload pane are the same controls either
   *  way, and shrinking those would be taking away function to buy
   *  room the grid has already given back. */
  grow: boolean,
)}
  <div class="flex min-w-0 flex-col">
    <!-- The source switch. Members FIRST and selected by default: it
         is the answer most of the time, and the two new arms are
         there for the case #1074 named — a banner that is not, and
         should not become, a member of the collection.

         THE HINT RIDES THIS ROW (#1218). It used to be a full-width
         band of its own above the stage, which spent a whole row of a
         dialog on one sentence and separated the tabs from the grid
         they switch. It is secondary text: it belongs beside the
         control it describes, and it wraps to its own line only when
         the row runs out of width. -->
    <div class="mb-2 flex flex-wrap items-center gap-x-3 gap-y-1">
    <div class="flex flex-wrap items-center gap-1" role="group"
         aria-label={t('collections.cover_editor_source_label')}>
      {#each [['members', t('collections.cover_editor_source_members')], ['mine', t('collections.cover_editor_source_mine')], ['upload', t('collections.cover_editor_source_upload')]] as const as [key, label] (key)}
        <button
          type="button"
          data-testid="{testidPrefix}-source-{key}"
          aria-pressed={pick.source === key}
          onclick={() => {
            pick.source = key;
            // The first visit fetches; after that the results are
            // whatever the curator last searched for, which is what
            // they expect to come back to.
            if (key === 'mine' && pick.results.length === 0 && !pick.searching) {
              void searchMine(pick);
            }
          }}
          class="rounded border px-2 py-1 text-xs"
          class:border-accent={pick.source === key}
          class:text-accent={pick.source === key}
          class:border-border={pick.source !== key}
        >{label}</button>
      {/each}
    </div>
    <!-- `min-w-[16rem]` is what makes the wrap happen instead of the
         squeeze: with `flex-wrap` above, a flex item that cannot have
         its minimum gets its own line. Without it a 390px row gave the
         hint the ~40px the tabs left over and rendered one word per
         line down the side of the dialog. -->
    <p class="min-w-[16rem] flex-1 text-xs text-fg-muted" data-testid="cover-page-hint">
      {#if !cropOffered}
        {t('collections.cover_hint')}
      {:else if isFeatured}
        {featuredIsInherited
          ? t('collections.cover_editor_featured_inherited')
          : t('collections.cover_editor_featured_hint')}
      {:else}
        {coverAssetId === null
          ? t('collections.cover_hint')
          : t('collections.cover_editor_cover_crop_hint')}
      {/if}
    </p>
    </div>

    {#if warn}
      <!-- Not a refusal. The cover is stored and the rail's per-rung
           fallback keeps the tile filled for everyone; what this
           prevents is the curator wondering why the strip is showing
           something else. -->
      <p class="mb-2 rounded border border-warning/40 bg-warning/10 px-2 py-1 text-xs text-warning"
         data-testid="{testidPrefix}-narrow-warning">
        {t('collections.cover_editor_narrow_warning')}
      </p>
    {/if}

    {#if pick.source === 'upload'}
      <div class="rounded border border-border p-3" data-testid="{testidPrefix}-upload-pane">
        <!-- ONE PLAIN SENTENCE about what the upload does, which the
             owner asked for and which is the difference between a
             feature that works and one that reads as broken. It says
             what is true of THIS collection: a public collection's
             cover is uploaded active so visitors can see it, a
             private one's stays a draft in the curator's own files. -->
        <p class="mb-2 text-xs text-fg-muted">
          {collectionVisibility === 'public'
            ? t('collections.cover_editor_upload_note_public')
            : t('collections.cover_editor_upload_note_private')}
        </p>
        <input
          type="file"
          accept="image/*"
          disabled={pick.uploading}
          data-testid="{testidPrefix}-upload-input"
          onchange={(e) => onFilePicked(e, pick, choose)}
          class="block w-full text-xs file:mr-2 file:rounded file:border file:border-border file:bg-surface file:px-2 file:py-1 file:text-xs"
        />
        {#if pick.uploading}
          <p class="mt-2 text-xs text-fg-muted" data-testid="{testidPrefix}-uploading">
            {t('collections.cover_editor_uploading')}
          </p>
        {/if}
        {#if pick.uploadError}
          <p role="alert" class="mt-2 text-xs text-danger">{pick.uploadError}</p>
        {/if}
      </div>
    {:else if pick.source === 'mine'}
      <div data-testid="{testidPrefix}-mine-pane">
        <form
          class="mb-2 flex gap-2"
          onsubmit={(e) => {
            e.preventDefault();
            void searchMine(pick);
          }}
        >
          <input
            type="search"
            bind:value={pick.query}
            placeholder={t('collections.cover_editor_search_placeholder')}
            data-testid="{testidPrefix}-search-input"
            class="min-w-0 flex-1 rounded border border-border-strong bg-surface px-2 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          />
          <button
            type="submit"
            class="rounded border border-border px-2 py-1 text-xs hover:bg-surface"
          >{t('common.search')}</button>
        </form>
        {#if pick.searching}
          <p class="text-xs text-fg-muted">{t('collections.cover_editor_searching')}</p>
        {:else if pick.results.length === 0}
          <p class="text-xs text-fg-muted">{t('collections.cover_editor_mine_empty')}</p>
        {:else}
          <div class={gridClass(grow)} data-testid="{testidPrefix}-mine-choices">
            {#each pick.results as r (r.id)}
              <button
                type="button"
                onclick={() => choose(r.id)}
                aria-pressed={selected === r.id}
                title={r.title ?? ''}
                data-testid="{testidPrefix}-mine-choice"
                data-asset-id={r.id}
                class="{tileClass(grow)} overflow-hidden rounded border-2 hover:border-border-strong"
                class:border-accent={selected === r.id}
                class:border-border={selected !== r.id}
              >
                <img src={colUrl(r.id)} alt="" loading="lazy" class="h-full w-full object-cover" />
              </button>
            {/each}
          </div>
        {/if}
      </div>
    {:else if loading}
      <p class="text-xs text-fg-muted">{t('collections.cover_loading')}</p>
    {:else}
      <div class={gridClass(grow)} data-testid="{testidPrefix}-cover-choices">
        <button
          type="button"
          onclick={() => choose(null)}
          aria-pressed={selected === null}
          class="{tileClass(grow)} flex flex-col items-center justify-center rounded border-2 bg-surface p-1 text-center text-[10px] leading-tight text-fg-muted hover:border-border-strong"
          class:border-accent={selected === null}
          class:border-border={selected !== null}
        >
          <span class="font-medium">{noneLabel}</span>
        </button>
        <!-- A cover chosen from OUTSIDE the member list — which is now
             the ordinary case, not the exception — still has to show
             as the current selection, or an unrelated edit would look
             like it had cleared the cover. -->
        {#if isExternal(selected) && selected}
          <button
            type="button"
            aria-pressed="true"
            title={t('collections.cover_current_external')}
            data-testid="{testidPrefix}-external-choice"
            class="{tileClass(grow)} overflow-hidden rounded border-2 border-accent"
          >
            <img src={colUrl(selected)} alt={t('collections.cover_current_external')}
                 loading="lazy" class="h-full w-full object-cover" />
          </button>
        {/if}
        {#each choices as choice (choice.asset_id)}
          <button
            type="button"
            onclick={() => choose(choice.asset_id)}
            aria-pressed={selected === choice.asset_id}
            data-testid="{testidPrefix}-cover-choice"
            data-asset-id={choice.asset_id}
            class="{tileClass(grow)} overflow-hidden rounded border-2 hover:border-border-strong"
            class:border-accent={selected === choice.asset_id}
            class:border-border={selected !== choice.asset_id}
          >
            <img src={colUrl(choice.asset_id)} alt="" loading="lazy" class="h-full w-full object-cover" />
          </button>
        {/each}
      </div>
      <!-- THE EMPTY CASE, NAMED (#1220's "say what the surface shows").
           A collection with nothing picturable in it renders a grid
           holding one tile — the mosaic option — and then stops, which
           on its own reads as a broken picker rather than as a true
           statement about the collection. It says what is true and where
           the other two sources are, because they both still work: a
           cover has never had to be a member (#1074). -->
      {#if choices.length === 0}
        <p class="mt-2 text-xs text-fg-muted" data-testid="{testidPrefix}-members-empty">
          {t('collections.cover_editor_members_empty')}
        </p>
      {/if}
    {/if}
  </div>
{/snippet}

<!-- ONE SLOT, ONE COLUMN OF THE ONE SURFACE (#1213 → #1264).
     No `<Modal>` here and no page: this is a block of the collection
     edit dialog, rendered beside the collection's identity. The header,
     Cancel and Save all belong to that one dialog, so there is one
     Escape owner, one focus trap and one commit.

     A COLUMN SIZED BY ITS CONTENT (#1220). It used to be `h-full
     min-h-0` and spend a definite height the host handed down; the host
     now hands down a CAP instead, so each region is bounded by its own
     budget and the column is as tall as they come to. -->
<section
  aria-labelledby="cover-page-heading"
  data-testid="collection-cover-editor"
  data-cover-slot={coverSlot}
  class="flex min-w-0 flex-col gap-3"
>
  <h3 id="cover-page-heading" class="sr-only">
    {isFeatured
      ? t('collections.cover_editor_featured_heading')
      : t('collections.cover_editor_cover_heading')}
  </h3>
  {#if shownId === null || stageSrc === null}
    <!-- ONE LINE, AND NO RESERVED STAGE BEHIND IT (#1218). What this
         says is "there is nothing to crop yet", and it used to say it
         in a padded band on top of the empty room the stage had
         reserved — half a dialog of dead area to carry one sentence.
         The room goes to the picker, which is the control that answers
         the sentence. -->
    <p class="shrink-0 text-xs text-fg-muted" data-testid="cover-page-empty">
      {isFeatured
        ? t('collections.cover_editor_featured_none')
        : t('collections.crop_mosaic_note')}
    </p>
  {:else if cropOffered}
    <div class="min-w-0">
      <!-- The destination's own shape: 890:500 for the rail card
           (#1110/#1098), 4:3 for the collection tile.

           ⚠️ THE COLLECTION TILE LOOKED LIKE A SQUARE, and the
           correction is the whole lesson. `col` IS a square — `fit:
           cover` at 320px — and it is what every small collection
           thumbnail is made of, so "lock it to a square" is a very
           reasonable read of the pipeline. But `col` is a SOURCE, not a
           destination: the tile that paints it is `aspect-[4/3]`
           (CollectionCard.svelte), so a curator positioning against a
           square would have been shown a region the card never
           displays. A crop marquee locks to the dimensions of the thing
           that RENDERS it. -->
      <!-- ⚠️ THE VH BUDGET, NOT `fill` (#1220). `fill` measures the box
           the layout gave the stage, in container units — which needs a
           DEFINITE box to measure, and a `container-type: size` element
           in a content-sized column collapses to nothing. The budget is
           the right answer for a block inside a surface that flows:
           nobody has told it how much room it has, so it takes a share
           of the viewport. 44vh rather than the wide card's default 52:
           the stage now sits beside the collection's identity in half a
           dialog rather than owning a page, and the picker under it has
           to be on screen at the same time.

           A SQUARE DESTINATION GETS LESS, which is the same rule the
           prop's own comment records: the card preview is bounded by
           `maxHeightVh × aspect` as a max-WIDTH, so at one budget the
           1:1 collection card comes out nearly twice the height of the
           890:500 featured one. -->
      <CoverCropStage
        maxHeightVh={isFeatured ? 44 : 34}
        src={stageSrc}
        srcset={stageSrcset}
        sizes="(max-width: 1024px) 90vw, 45vw"
        {aspect}
        bind:focalX={framing[coverSlot].x}
        bind:focalY={framing[coverSlot].y}
        bind:zoom={framing[coverSlot].zoom}
        testidPrefix={isFeatured ? 'cover-editor' : 'collection-crop'}
        stageAlt={isFeatured
          ? t('collections.cover_editor_stage_alt')
          : t('collections.cover_editor_cover_stage_alt')}
        cardAlt={isFeatured
          ? t('collections.cover_editor_card_alt')
          : t('collections.cover_editor_cover_preview_alt')}
        cardLabel={isFeatured
          ? t('collections.cover_editor_card_label')
          : t('collections.cover_editor_cover_preview_label')}
      >
        {#snippet extraActions()}
          {#if isFeatured && featuredCoverAssetId !== null}
            <button
              type="button"
              onclick={() => (featuredCoverAssetId = null)}
              data-testid="cover-editor-clear-featured"
              class="rounded border border-border px-2 py-1 text-xs hover:bg-surface"
            >
              {t('collections.cover_editor_use_collection_cover')}
            </button>
          {/if}
        {/snippet}
      </CoverCropStage>
    </div>
  {:else}
    <!-- ⚠️ THE CROP STAGE IS WITHHELD BELOW 768px, AND ONLY THE CROP
         STAGE.
         WCAG 2.2 SC 1.4.10 (Reflow) requires content to reflow to 320px
         EXCEPT "parts of the content which require two-dimensional
         layout for usage or meaning", and names interfaces that must
         keep toolbars in view while manipulating content among its
         examples. Dragging a marquee across a picture while watching a
         live preview of the result is exactly that. What the exemption
         does not cover is switching off unrelated functionality, so
         everything below this line — the source switch, the search, the
         upload, the choice itself — renders exactly as it does on a
         desktop.
         The message NAMES THE LIMITATION rather than saying "use a
         desktop": what a narrow screen cannot do is position the crop,
         and the consequence is that the cover is centred, which is the
         null-focal/null-zoom behaviour every collection had before
         #1207 and needs no separate code path.
         ⚠️ AN EXISTING FRAMING IS NOT TOUCHED HERE. `framing` is seeded
         from the collection and this branch neither reads nor writes
         it, so a save from a phone re-sends what a desktop set. The
         opposite — clearing it because the control that edits it is not
         on screen — would be the surface silently undoing somebody
         else's work, which is worse than not offering the control. -->
    <p class="shrink-0 rounded border border-border bg-surface px-2 py-1.5 text-xs text-fg-muted"
       data-testid="cover-page-crop-unavailable">
      {t('collections.cover_editor_crop_needs_width')}
    </p>
  {/if}

  {@render picker(
    activePicker,
    selectedId,
    assign,
    isFeatured ? 'featured' : 'collection',
    isFeatured ? t('collections.cover_editor_same_as_cover') : t('collections.cover_derived'),
    warning,
    !stageShown,
  )}
</section>
