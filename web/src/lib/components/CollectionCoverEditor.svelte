<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The cover editor (#1207).
  //
  // Its own near-full-viewport dialog, raised from the collection edit
  // modal, because the owner's finding was that the cover surface "is
  // too small and I can't really see how the cover images will be
  // cropped". Cropping is a judgement about a picture; making that
  // judgement in a 200px thumbnail is the defect, and no amount of
  // re-laying a shared modal fixes it while the picture stays small.
  //
  // TWO SLOTS, because the second finding was that a collection card
  // and a featured-rail card want different pictures:
  //
  //   1. the collection cover  — every collection card, roughly square
  //   2. the featured cover    — the strip only, locked to 890:500
  //
  // Slot 2 DEFAULTS TO SLOT 1 rather than starting empty. "No separate
  // choice" is the common case and it is also what the rail's fallback
  // chain does, so the editor shows the picture the rail would actually
  // use instead of an empty box that means "look elsewhere".
  //
  // IT OWNS NO SAVE. Every value here is a bindable prop belonging to
  // EditCollectionModal, which has the collection, the concurrency
  // baseline and the single PATCH. A dialog that saved its own two
  // fields would give the curator two Save buttons with different
  // failure modes and two chances to hit a 409 — and would need its own
  // copy of the tri-state clear discipline. Closing this is "done
  // looking", not "committed".

  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import { previewLadder } from '$stores/previewLadder.svelte';
  import { DEFAULT_ASSET_TYPE } from '$stores/upload.svelte';
  import { putStorageObject } from '$lib/util/storageUpload';
  import Modal from './Modal.svelte';
  import {
    CARD_ASPECT,
    cropWindow,
    focalFromOrigin,
    hasTravel,
    marqueeOrigin,
    objectPosition,
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
    open: boolean;
    onclose: () => void;
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
    /** The focal pair, null for centre. Always moved together. */
    focalX: number | null;
    focalY: number | null;
  }

  let {
    open,
    onclose,
    choices,
    loading,
    collectionVisibility,
    coverAssetId = $bindable(),
    featuredCoverAssetId = $bindable(),
    focalX = $bindable(),
    focalY = $bindable(),
  }: Props = $props();

  // ── Which picture each slot is showing ─────────────────────────────
  //
  // The featured slot falls back to the collection cover exactly as the
  // RAIL does. That is the point: what this box shows is what the strip
  // will show, and if the fallback were spelled out differently here
  // the editor would be a second opinion about the rail's own rule.
  const featuredEffectiveId = $derived(featuredCoverAssetId ?? coverAssetId);
  const featuredIsInherited = $derived(featuredCoverAssetId === null && coverAssetId !== null);

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

  function srcFor(assetId: string | null): string | null {
    if (assetId === null) return null;
    if (ladderFor(assetId) !== true) return colUrl(assetId);
    const smallest = previewLadder.smallestKey();
    return smallest ? `/api/v1/assets/${assetId}/variants/${smallest}` : colUrl(assetId);
  }

  function srcsetFor(assetId: string | null): string | undefined {
    if (assetId === null || ladderFor(assetId) !== true) return undefined;
    return previewLadder.srcsetFor(assetId) ?? undefined;
  }

  const featuredSrc = $derived(srcFor(featuredEffectiveId));
  const featuredSrcset = $derived(srcsetFor(featuredEffectiveId));
  const coverSrc = $derived(srcFor(coverAssetId));
  const coverSrcset = $derived(srcsetFor(coverAssetId));

  // ── The picture's own proportions, STAMPED WITH THE ASSET ──────────
  //
  // The stamp is what makes the marquee safe rather than merely usually
  // right (#1195's note, kept). A plain {w,h} cleared by an effect on
  // the selection has an ordering hazard: a picture already in the
  // browser cache fires `load` synchronously enough to beat the effect,
  // which then clears the value it just produced. Keying on the id and
  // deriving "is this measurement about the current picture?" removes
  // the question instead of timing it.
  let natural = $state<{ assetId: string; w: number; h: number } | null>(null);
  const naturalNow = $derived(
    natural && natural.assetId === featuredEffectiveId ? natural : null,
  );

  function onStageLoad(e: Event) {
    const img = e.currentTarget as HTMLImageElement;
    if (featuredEffectiveId && img.naturalWidth > 0 && img.naturalHeight > 0) {
      natural = { assetId: featuredEffectiveId, w: img.naturalWidth, h: img.naturalHeight };
      stageFailed = null;
    }
  }

  /** The picture did not load, STAMPED with which one — same reason the
   *  measurement is stamped.
   *
   *  Found by driving this in a browser rather than reasoned about: a
   *  picture chosen seconds after it was uploaded has no `col` rendition
   *  yet, because renditions are produced asynchronously (the same fact
   *  CallerMayPictureAsset's doc records as its reason for NOT requiring
   *  one at the write gate). Both the stage and the live preview then
   *  404, and the editor has to say which of the two things happened —
   *  "still being made" is a wait, and it is not the same message as
   *  anything else here. */
  let stageFailed = $state<string | null>(null);
  const stageIsPending = $derived(
    stageFailed !== null && stageFailed === featuredEffectiveId,
  );

  function onStageError() {
    if (featuredEffectiveId) stageFailed = featuredEffectiveId;
  }

  const win = $derived(naturalNow ? cropWindow(naturalNow.w / naturalNow.h, CARD_ASPECT) : null);
  /** Which axis the curator can actually move on. Exactly one, ever —
   *  `object-fit: cover` trims one axis and shows the other whole. */
  const canMoveX = $derived(win !== null && hasTravel(win.w));
  const canMoveY = $derived(win !== null && hasTravel(win.h));
  const canMove = $derived(canMoveX || canMoveY);

  /** The stored pair, with null read as centre. The marquee is drawn
   *  from these and the drag writes back to them. */
  const fx = $derived(focalX ?? 0.5);
  const fy = $derived(focalY ?? 0.5);

  const marquee = $derived(
    win === null
      ? null
      : {
          left: marqueeOrigin(fx, win.w) * 100,
          top: marqueeOrigin(fy, win.h) * 100,
          width: win.w * 100,
          height: win.h * 100,
        },
  );

  /** What the strip will render. Called for the live preview AND
   *  written by the rail from the same helper, so the two cannot drift
   *  over what null means. */
  const previewPosition = $derived(objectPosition(focalX, focalY));

  // ── Dragging ───────────────────────────────────────────────────────
  //
  // Pointer events, not mouse events: one code path covers mouse, touch
  // and pen, which is what makes the marquee work at 390px on a coarse
  // pointer without a second handler to keep in step.
  //
  // `setPointerCapture` is what makes a drag survive the pointer leaving
  // the picture — and the curator dragging to an edge is not an edge
  // case here, it is how you frame the left of a wide photograph. Without
  // capture the marquee would stop the moment the pointer crossed the
  // border and the position would silently be short of what was asked.
  let stage = $state<HTMLDivElement | null>(null);
  let dragging = $state(false);
  /** Where in the marquee the pointer grabbed, as a fraction of the
   *  PICTURE. Kept so the marquee moves WITH the pointer rather than
   *  jumping its centre to it — a jump on mousedown is how a
   *  positioning control loses the position it was showing. */
  let grabOffset = { x: 0, y: 0 };

  function stageRect(): DOMRect | null {
    return stage?.getBoundingClientRect() ?? null;
  }

  function onPointerDown(e: PointerEvent) {
    const rect = stageRect();
    if (!rect || !win || !canMove) return;
    e.preventDefault();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    dragging = true;
    const px = (e.clientX - rect.left) / rect.width;
    const py = (e.clientY - rect.top) / rect.height;
    grabOffset = {
      x: px - marqueeOrigin(fx, win.w),
      y: py - marqueeOrigin(fy, win.h),
    };
  }

  function onPointerMove(e: PointerEvent) {
    if (!dragging) return;
    const rect = stageRect();
    if (!rect || !win) return;
    e.preventDefault();
    const px = (e.clientX - rect.left) / rect.width;
    const py = (e.clientY - rect.top) / rect.height;
    // Both axes are written on every move even though only one can
    // travel: focalFromOrigin answers 0.5 for a pinned axis, so the
    // pinned half stays neutral instead of accumulating pointer noise,
    // and the PAIR stays a pair — which is what the column CHECK and
    // the API's both-or-neither rule require.
    setFocal(
      focalFromOrigin(px - grabOffset.x, win.w),
      focalFromOrigin(py - grabOffset.y, win.h),
    );
  }

  function endDrag(e: PointerEvent) {
    if (!dragging) return;
    dragging = false;
    (e.currentTarget as HTMLElement).releasePointerCapture?.(e.pointerId);
  }

  function setFocal(x: number, y: number) {
    focalX = x;
    focalY = y;
  }

  /** Keyboard nudge — the same control, reachable without a pointer.
   *
   *  A step of 2% of the TRAVEL rather than of the picture, so the
   *  number of presses it takes to cross the range is the same whatever
   *  the picture's proportions; Shift takes bigger steps for a long
   *  move, and Home/End go to the ends. Arrow keys are claimed
   *  (preventDefault) because the dialog body scrolls and an unclaimed
   *  ArrowDown would scroll the page out from under the control the
   *  curator is using. */
  function onKeydown(e: KeyboardEvent) {
    if (!win || !canMove) return;
    const step = e.shiftKey ? 0.1 : 0.02;
    let x = fx;
    let y = fy;
    switch (e.key) {
      case 'ArrowLeft':
        x -= step;
        break;
      case 'ArrowRight':
        x += step;
        break;
      case 'ArrowUp':
        y -= step;
        break;
      case 'ArrowDown':
        y += step;
        break;
      case 'Home':
        x = canMoveX ? 0 : x;
        y = canMoveY ? 0 : y;
        break;
      case 'End':
        x = canMoveX ? 1 : x;
        y = canMoveY ? 1 : y;
        break;
      default:
        return;
    }
    e.preventDefault();
    setFocal(canMoveX ? clamp01(x) : 0.5, canMoveY ? clamp01(y) : 0.5);
  }

  function clamp01(v: number) {
    return v < 0 ? 0 : v > 1 ? 1 : v;
  }

  /** Back to centre — a CLEAR, not a re-set to 0.5. Null and 0.5 render
   *  identically and are stored differently on purpose (see the
   *  migration): this is what makes "the curator never positioned this"
   *  recoverable. */
  function resetFocal() {
    focalX = null;
    focalY = null;
  }

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

  async function searchMine(slot: SlotPicker) {
    const me = auth.user?.ref;
    if (me == null) return;
    slot.searching = true;
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
            ...(slot.query.trim() ? { q: slot.query.trim() } : {}),
            limit: 48,
          },
        },
      });
      slot.results = ((data?.items ?? []) as unknown as MineResult[]) ?? [];
    } catch {
      // A search that failed must not take the dialog down with it —
      // the member grid and the marquee are still usable.
      slot.results = [];
    } finally {
      slot.searching = false;
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
  async function uploadAndChoose(slot: SlotPicker, file: File, assign: (id: string) => void) {
    slot.uploading = true;
    slot.uploadError = null;
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
        slot.uploadError =
          (error as { error?: string } | undefined)?.error ?? t('collections.cover_editor_upload_failed');
        return;
      }
      assign(data.id);
      // Straight back to the member grid: the picture is chosen, and
      // leaving the upload pane open would imply there is another step.
      slot.source = 'members';
      // Renditions are produced asynchronously, so the picture the
      // curator just chose does not exist yet and neither does its
      // ladder. Poll until it does, so the stage stops saying "still
      // being prepared" and — the part that matters — switches to the
      // SAME rung the strip will load, instead of leaving the marquee
      // over a `col` square while the rail crops the original.
      void awaitRenditions(data.id);
    } catch (e) {
      slot.uploadError = e instanceof Error ? e.message : t('collections.cover_editor_upload_failed');
    } finally {
      slot.uploading = false;
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
          // Clearing the failure lets the <img> re-render and try again;
          // the src is unchanged, so this is what re-attempts the load.
          if (stageFailed === assetId) stageFailed = null;
          return;
        }
      } catch {
        return;
      }
    }
  }

  function onFilePicked(e: Event, slot: SlotPicker, assign: (id: string) => void) {
    const input = e.currentTarget as HTMLInputElement;
    const file = input.files?.[0];
    // Cleared so choosing the SAME file twice fires `change` again —
    // an input that keeps its value is silent on a re-pick, which reads
    // as "the upload button stopped working".
    input.value = '';
    if (file) void uploadAndChoose(slot, file, assign);
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
  function narrowerThanCollection(assetId: string | null, slot: SlotPicker): boolean {
    if (assetId === null || collectionVisibility !== 'public') return false;
    const found =
      slot.results.find((r) => r.id === assetId) ??
      // A member row carries no status, so a member picked from the
      // first grid produces no warning either way. That is the honest
      // answer, not a silent pass: the check has nothing to read.
      null;
    return found !== null && found.status !== undefined && found.status !== 'active';
  }

  const featuredWarning = $derived(narrowerThanCollection(featuredCoverAssetId, featuredPicker));
  const coverWarning = $derived(narrowerThanCollection(coverAssetId, coverPicker));

  const positionLabel = $derived(
    t('collections.cover_editor_position_value', {
      x: String(Math.round(fx * 100)),
      y: String(Math.round(fy * 100)),
    }),
  );
</script>

<!-- ONE picker, rendered twice (#1207/#1074).
     Both slots choose from the same three sources on the same terms;
     a second copy of the grid was already drifting (the featured one
     had a "same as cover" tile where the other had "use mosaic", and
     nothing else differed) and the search and upload arms would have
     doubled it again. `assign` is what the two slots actually
     disagree about, so it is the parameter. -->
{#snippet picker(
  slot: SlotPicker,
  selected: string | null,
  assign: (id: string | null) => void,
  testidPrefix: string,
  noneLabel: string,
  warn: boolean,
)}
  <div class="mt-3">
    <!-- The source switch. Members FIRST and selected by default: it
         is the answer most of the time, and the two new arms are
         there for the case #1074 named — a banner that is not, and
         should not become, a member of the collection. -->
    <div class="mb-2 flex flex-wrap items-center gap-1" role="group"
         aria-label={t('collections.cover_editor_source_label')}>
      {#each [['members', t('collections.cover_editor_source_members')], ['mine', t('collections.cover_editor_source_mine')], ['upload', t('collections.cover_editor_source_upload')]] as const as [key, label] (key)}
        <button
          type="button"
          data-testid="{testidPrefix}-source-{key}"
          aria-pressed={slot.source === key}
          onclick={() => {
            slot.source = key;
            // The first visit fetches; after that the results are
            // whatever the curator last searched for, which is what
            // they expect to come back to.
            if (key === 'mine' && slot.results.length === 0 && !slot.searching) {
              void searchMine(slot);
            }
          }}
          class="rounded border px-2 py-1 text-xs"
          class:border-accent={slot.source === key}
          class:text-accent={slot.source === key}
          class:border-border={slot.source !== key}
        >{label}</button>
      {/each}
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

    {#if slot.source === 'upload'}
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
          disabled={slot.uploading}
          data-testid="{testidPrefix}-upload-input"
          onchange={(e) => onFilePicked(e, slot, assign)}
          class="block w-full text-xs file:mr-2 file:rounded file:border file:border-border file:bg-surface file:px-2 file:py-1 file:text-xs"
        />
        {#if slot.uploading}
          <p class="mt-2 text-xs text-fg-muted" data-testid="{testidPrefix}-uploading">
            {t('collections.cover_editor_uploading')}
          </p>
        {/if}
        {#if slot.uploadError}
          <p role="alert" class="mt-2 text-xs text-danger">{slot.uploadError}</p>
        {/if}
      </div>
    {:else if slot.source === 'mine'}
      <div data-testid="{testidPrefix}-mine-pane">
        <form
          class="mb-2 flex gap-2"
          onsubmit={(e) => {
            e.preventDefault();
            void searchMine(slot);
          }}
        >
          <input
            type="search"
            bind:value={slot.query}
            placeholder={t('collections.cover_editor_search_placeholder')}
            data-testid="{testidPrefix}-search-input"
            class="min-w-0 flex-1 rounded border border-border-strong bg-surface px-2 py-1 text-xs focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
          />
          <button
            type="submit"
            class="rounded border border-border px-2 py-1 text-xs hover:bg-surface"
          >{t('common.search')}</button>
        </form>
        {#if slot.searching}
          <p class="text-xs text-fg-muted">{t('collections.cover_editor_searching')}</p>
        {:else if slot.results.length === 0}
          <p class="text-xs text-fg-muted">{t('collections.cover_editor_mine_empty')}</p>
        {:else}
          <div class="grid max-h-40 grid-cols-6 gap-2 overflow-y-auto rounded border border-border p-1 sm:grid-cols-10">
            {#each slot.results as r (r.id)}
              <button
                type="button"
                onclick={() => assign(r.id)}
                aria-pressed={selected === r.id}
                title={r.title ?? ''}
                data-testid="{testidPrefix}-mine-choice"
                data-asset-id={r.id}
                class="relative aspect-square overflow-hidden rounded border-2 hover:border-border-strong"
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
      <div class="grid max-h-40 grid-cols-6 gap-2 overflow-y-auto rounded border border-border p-1 sm:grid-cols-10"
           data-testid="{testidPrefix}-cover-choices">
        <button
          type="button"
          onclick={() => assign(null)}
          aria-pressed={selected === null}
          class="flex aspect-square flex-col items-center justify-center rounded border-2 bg-surface p-1 text-center text-[10px] leading-tight text-fg-muted hover:border-border-strong"
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
            class="relative aspect-square overflow-hidden rounded border-2 border-accent"
          >
            <img src={colUrl(selected)} alt={t('collections.cover_current_external')}
                 loading="lazy" class="h-full w-full object-cover" />
          </button>
        {/if}
        {#each choices as choice (choice.asset_id)}
          <button
            type="button"
            onclick={() => assign(choice.asset_id)}
            aria-pressed={selected === choice.asset_id}
            data-testid="{testidPrefix}-cover-choice"
            data-asset-id={choice.asset_id}
            class="relative aspect-square overflow-hidden rounded border-2 hover:border-border-strong"
            class:border-accent={selected === choice.asset_id}
            class:border-border={selected !== choice.asset_id}
          >
            <img src={colUrl(choice.asset_id)} alt="" loading="lazy" class="h-full w-full object-cover" />
          </button>
        {/each}
      </div>
    {/if}
  </div>
{/snippet}

<!-- Near-full-viewport. The whole point of this dialog is that the
     picture is big enough to judge, so the width is a viewport
     proportion rather than a Tailwind size step — `max-w-7xl` on a 4k
     display is the cramped modal again with more whitespace round it. -->
<Modal
  title={t('collections.cover_editor_title')}
  {open}
  {onclose}
  panelClass="max-w-[min(96rem,95vw)]"
>
  <div class="max-h-[80vh] space-y-6 overflow-y-auto pr-1" data-testid="collection-cover-editor">
    <!-- Slot 2 FIRST. It is the one with the work in it — the marquee,
         the live preview — and it is the reason the curator opened this
         dialog. The collection cover below it is a straightforward
         pick, and putting the simple thing first would push the
         positioning stage under the fold on a laptop. -->
    <section aria-labelledby="featured-slot-heading" data-testid="featured-cover-slot">
      <h3 id="featured-slot-heading" class="text-sm font-semibold">
        {t('collections.cover_editor_featured_heading')}
      </h3>
      <p class="mt-0.5 text-xs text-fg-muted">
        {featuredIsInherited
          ? t('collections.cover_editor_featured_inherited')
          : t('collections.cover_editor_featured_hint')}
      </p>

      {#if featuredEffectiveId === null || featuredSrc === null}
        <p class="mt-3 rounded border border-border bg-surface p-3 text-xs text-fg-muted">
          {t('collections.cover_editor_featured_none')}
        </p>
      {:else}
        <div class="mt-3 grid gap-4 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
          <!-- LEFT: the whole picture at size, with the marquee on it.

               The wrapper SHRINK-WRAPS the image (inline-block, no width
               or height of its own) so the element and the picture are
               the SAME rectangle. That is what lets the marquee be
               positioned in percentages with nothing measured from the
               DOM. Giving the wrapper a size instead — an aspect-ratio,
               a fixed height — reintroduces exactly the bug this
               surface exists to fix: the image letterboxes inside a box
               it does not fill, and the marquee marks a region of the
               BOX rather than of the picture. -->
          <figure class="min-w-0">
            <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
              {t('collections.cover_editor_stage_label')}
            </figcaption>
            <!-- The image below is sized by a DEFINITE WIDTH with
                 `height: auto`, and both halves of that are load-bearing.
                 It was arrived at by driving it in a browser and getting
                 each of the other two wrong first.

                 `max-height`/`max-width` alone (what #1195's small
                 preview used) never UPSCALES, so a cover with no preview
                 ladder rendered at the 320px `col` rendition inside a
                 1500px dialog — the original complaint, in the case
                 #1074 just made ordinary.

                 A definite HEIGHT with `max-width` is worse: when the
                 max binds, the browser honours both and SQUASHES the
                 picture. The marquee is computed from
                 naturalWidth/naturalHeight, so it then marks a region of
                 a shape that is not on screen. A definite width with
                 `height: auto` and a `max-height` cannot distort — the
                 auto axis always follows the aspect.

                 The wrapper stays SHRINK-WRAPPED around the image, which
                 is what lets the marquee be positioned in percentages
                 with nothing measured from the DOM. Giving the wrapper a
                 size of its own — a width, an aspect-ratio — puts the
                 overlay over the BOX instead of over the picture, which
                 is the exact bug this surface exists to reveal. -->
            <div class="flex justify-center rounded border border-border bg-surface p-2">
              <div bind:this={stage} class="relative inline-block max-w-full align-top">
                <img
                  src={featuredSrc}
                  srcset={featuredSrcset}
                  sizes="(max-width: 640px) 90vw, 55vw"
                  alt={t('collections.cover_editor_stage_alt')}
                  onload={onStageLoad}
                  onerror={onStageError}
                  data-testid="cover-editor-stage-image"
                  class="block select-none rounded"
                  draggable="false"
                  style="width: clamp(17rem, 40vw, 38rem); height: auto; max-height: 52vh;"
                />
                {#if marquee}
                  <!-- What gets cropped OFF is dimmed and what survives
                       is outlined: two readings of one fact, so it holds
                       up for a colour-blind reader and in a 390px
                       screenshot alike. Four bars rather than #1195's
                       two, because a marquee that has MOVED is off
                       centre and the two offcuts are no longer equal —
                       the symmetric pair was only ever correct for a
                       centred crop. `pointer-events-none` because they
                       are annotation; the drag belongs to the marquee. -->
                  <div class="pointer-events-none absolute inset-x-0 top-0 bg-black/60"
                       style="height: {marquee.top}%"></div>
                  <div class="pointer-events-none absolute inset-x-0 bottom-0 bg-black/60"
                       style="height: {100 - marquee.top - marquee.height}%"></div>
                  <div class="pointer-events-none absolute left-0 bg-black/60"
                       style="top: {marquee.top}%; height: {marquee.height}%; width: {marquee.left}%"></div>
                  <div class="pointer-events-none absolute right-0 bg-black/60"
                       style="top: {marquee.top}%; height: {marquee.height}%; width: {100 - marquee.left - marquee.width}%"></div>

                  <!-- The marquee is a BUTTON: focusable, announced, and
                       obviously interactive without inventing a role for
                       a two-axis positioner that ARIA does not have one
                       for. Its accessible name carries the current
                       position, so a screen-reader user gets the same
                       feedback a sighted one gets from watching it move.

                       `touch-action: none` is what makes the drag work
                       on a phone: without it the browser claims the
                       gesture for scrolling and the marquee never sees
                       a pointermove. -->
                  <button
                    type="button"
                    data-testid="cover-editor-marquee"
                    aria-label={t('collections.cover_editor_marquee_label')}
                    aria-describedby="cover-editor-position"
                    disabled={!canMove}
                    onpointerdown={onPointerDown}
                    onpointermove={onPointerMove}
                    onpointerup={endDrag}
                    onpointercancel={endDrag}
                    onkeydown={onKeydown}
                    class="absolute border-2 border-accent shadow-[0_0_0_9999px_rgba(0,0,0,0)] focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring {canMove
                      ? dragging
                        ? 'cursor-grabbing'
                        : 'cursor-grab'
                      : 'cursor-default'}"
                    style="left: {marquee.left}%; top: {marquee.top}%; width: {marquee.width}%; height: {marquee.height}%; touch-action: none;"
                  ></button>
                {/if}
              </div>
            </div>
            <p
              id="cover-editor-position"
              class="mt-2 text-xs text-fg-muted"
              data-testid="cover-editor-position"
              data-focal-x={focalX === null ? '' : String(focalX)}
              data-focal-y={focalY === null ? '' : String(focalY)}
            >
              <!-- THREE STATES, and the middle one is the whole point
                   of this block. "Nothing to move" is a statement about
                   a picture's PROPORTIONS, so it must not be printed
                   before the proportions are known — which is what
                   happened the first time this was driven in a browser
                   with a freshly uploaded cover whose rendition did not
                   exist yet: the editor confidently announced that a
                   2.4:1 picture was already card-shaped, about an image
                   it had failed to load. -->
              {#if stageIsPending}
                {t('collections.cover_editor_stage_pending')}
              {:else if win === null}
                {t('collections.crop_no_dimensions')}
              {:else if !canMove}
                {t('collections.cover_editor_no_travel')}
              {:else}
                {t('collections.cover_editor_drag_hint')} — {positionLabel}
              {/if}
            </p>
          </figure>

          <!-- RIGHT: the card itself, drawn the way FeaturedRail draws
               it. Not a simulation of the crop: the same three CSS
               properties the strip uses (the aspect box, object-cover,
               and object-position from the same helper), on the same
               source. If the two ever disagree it is because the strip
               changed. -->
          <figure class="min-w-0">
            <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
              {t('collections.cover_editor_card_label')}
            </figcaption>
            <div
              class="overflow-hidden rounded-lg border border-border bg-surface-elevated"
              style="aspect-ratio: 890 / 500"
            >
              <img
                src={featuredSrc}
                srcset={featuredSrcset}
                sizes="(max-width: 640px) 90vw, 35vw"
                alt={t('collections.cover_editor_card_alt')}
                data-testid="cover-editor-card-preview"
                class="h-full w-full object-cover"
                style="object-position: {previewPosition}"
              />
            </div>
            <div class="mt-2 flex flex-wrap items-center gap-2">
              <button
                type="button"
                onclick={resetFocal}
                disabled={focalX === null && focalY === null}
                data-testid="cover-editor-reset-focal"
                class="rounded border border-border px-2 py-1 text-xs hover:bg-surface disabled:cursor-not-allowed disabled:opacity-40"
              >
                {t('collections.cover_editor_reset')}
              </button>
              {#if featuredCoverAssetId !== null}
                <button
                  type="button"
                  onclick={() => (featuredCoverAssetId = null)}
                  data-testid="cover-editor-clear-featured"
                  class="rounded border border-border px-2 py-1 text-xs hover:bg-surface"
                >
                  {t('collections.cover_editor_use_collection_cover')}
                </button>
              {/if}
            </div>
          </figure>
        </div>
      {/if}

      {@render picker(
        featuredPicker,
        featuredCoverAssetId,
        (id: string | null) => (featuredCoverAssetId = id),
        'featured',
        t('collections.cover_editor_same_as_cover'),
        featuredWarning,
      )}
    </section>

    <section aria-labelledby="cover-slot-heading" data-testid="collection-cover-slot"
             class="border-t border-border pt-5">
      <h3 id="cover-slot-heading" class="text-sm font-semibold">
        {t('collections.cover_editor_cover_heading')}
      </h3>
      <p class="mt-0.5 text-xs text-fg-muted">{t('collections.cover_hint')}</p>

      <div class="mt-3 flex items-start gap-4">
        <!-- The collection card is roughly square and takes a centre
             crop, so this slot needs no marquee — there is nothing to
             position that the curator did not already choose by picking
             the picture. Showing the square preview is the whole
             feedback the slot owes them. -->
        <figure class="w-40 shrink-0">
          <figcaption class="mb-1 text-[10px] uppercase tracking-wide text-fg-muted">
            {t('collections.cover_editor_cover_preview_label')}
          </figcaption>
          <div class="aspect-square overflow-hidden rounded border border-border bg-surface-elevated">
            {#if coverAssetId !== null && coverSrc !== null}
              <img src={coverSrc} srcset={coverSrcset} sizes="160px"
                   alt={t('collections.cover_editor_cover_preview_alt')}
                   data-testid="cover-editor-collection-preview"
                   class="h-full w-full object-cover" />
            {:else}
              <div class="flex h-full items-center justify-center p-2 text-center text-[10px] text-fg-muted">
                {t('collections.cover_derived')}
              </div>
            {/if}
          </div>
        </figure>

        <div class="min-w-0 flex-1">
          {@render picker(
            coverPicker,
            coverAssetId,
            (id: string | null) => (coverAssetId = id),
            'collection',
            t('collections.cover_derived'),
            coverWarning,
          )}
        </div>
      </div>
    </section>
  </div>

  {#snippet footer()}
    <!-- ONE button, and it says Done rather than Save. Nothing here is
         written until the form behind this dialog is submitted, and a
         Save button that only closed a dialog would be a promise this
         surface cannot keep. -->
    <button
      type="button"
      onclick={onclose}
      data-testid="cover-editor-done"
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent"
    >
      {t('collections.cover_editor_done')}
    </button>
  {/snippet}
</Modal>
