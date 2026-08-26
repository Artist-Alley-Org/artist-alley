<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Edit existing collection metadata. Wraps PATCH /collections/{id}.

  import { untrack } from 'svelte';
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
  // The retired `CollectionResource` shape joined the asset's columns in
  // FLAT, not nested under an `asset` object — and on a member the
  // reader may not see, #883 made every one of them ABSENT rather than
  // null, with `restricted` as the flag saying which shape the row was.
  // The post members this picker reads now are nested instead (see
  // loadCoverChoices), but the optionality is the same on both, so both
  // fields below stay optional and both have to be checked.
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
  }

  // ── ONE SURFACE (#1264) ────────────────────────────────────────────
  //
  // There is no `focusCover` any more, and no "Set cover" entry to feed
  // it. The owner's ruling: "I really think we shouldn't have more than
  // one menu to edit collections. We can put all editing of collection
  // items, including all cover types, in that same modal." A deep-link
  // that opens THIS dialog pointing at a part of itself is a second
  // door onto one room; the room is now open, so the door is gone.
  let { open, collection, onclose, onsaved }: Props = $props();

  // ── THE ROW THIS DIALOG IS EDITING, AS IT WAS WHEN IT OPENED ──────
  //
  // #1262. The `collection` PROP is a live view of the page's copy of
  // the row, and the page reassigns it whenever it refetches
  // (collections/[id]/+page.svelte — `collection = data` after load(),
  // and again in handleSaved). While the dialog is open that is a
  // second author writing into the surface the curator is typing in,
  // and every one of the three things the seed does was wrong for it:
  // the unsaved field values were replaced with the stored ones, the
  // concurrency baseline was advanced to the value SOMEONE ELSE had
  // just written, and a conflict message the curator was reading was
  // cleared.
  //
  // So the dialog takes a SNAPSHOT on open and reads that. Not a guard
  // on the seeding effect: the tri-states in submit() ask "was this
  // set before I started?" and the answer has to come from the same
  // instant `baselineUpdatedAt` does, or a save composed from the
  // curator's edits carries a clear for something a third party set
  // thirty seconds ago. One instant, one row, one answer.
  //
  // `$state.raw` because it is REPLACED, never mutated — a deep proxy
  // over a row nothing writes to buys nothing.
  //
  // `untrack` on the initial value is not decoration: capturing the
  // prop's value once is precisely the intent here, and it is also what
  // the compiler warns about (`state_referenced_locally`) because it is
  // usually a mistake. Saying so explicitly keeps the warning list
  // meaningful for the cases where it is one.
  let seeded = $state.raw<Collection>(untrack(() => collection));

  let name = $state('');
  let description = $state('');
  let visibility = $state<Visibility>('private');
  let submitting = $state(false);
  let error = $state<string | null>(null);
  // Phase 1.16 edit-safety: capture updated_at on open so the
  // PATCH can detect "someone else edited this while my modal
  // was open" + show a reload-prompt instead of silently
  // clobbering.
  //
  // ⛔ ON OPEN IS THE WHOLE POINT, and until #1262 this was re-seeded
  // on every change of the `collection` prop — so a refetch that
  // landed mid-edit handed the modal the timestamp of the very write
  // it was supposed to detect, and the save that should have 409'd
  // went through and silently overwrote it. Seeded from `seeded`
  // above, with everything else, exactly once per open.
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

  // ── ONE SURFACE, NOT TWO PAGES (#1264) ────────────────────────────
  //
  // #1213 made this one dialog with two PAGES: identity/visibility/
  // fields on page 1, one cover slot in full on page 2. That removed
  // the stacked `<dialog>` #1207 had raised, and the owner's next
  // finding is that the page split is the same complaint one level in —
  // "when I click set cover, it shows a limited view of what I can
  // set… when I close it, I see the edit collection modal" — with a
  // measured 896x586 → 1536x1036 jump between the two.
  //
  // So there are no pages. Everything the dialog can set is on screen
  // at once: the collection's identity in one column, BOTH cover slots
  // in the other, one Save for all of it. What survives from #1213 is
  // the part that was right — the crop surface is one block with a
  // SLOT parameter, and the two chips that used to be doors onto page 2
  // are now the control that points that block at a slot. Both chips
  // stay visible with their own pictures, so "which cover am I looking
  // at" and "what is the other one" are answered at the same time.
  //
  // ⚠️ THE EDITOR IS MOUNTED ONCE AND NEVER SWAPPED. Its per-slot
  // picker state — source tab, search text, results — belongs to that
  // component, and pointing it at the other slot must not be a
  // remount: a curator who searched their files for one cover and
  // glanced at the other has to come back to that search rather than to
  // an empty grid. #1213 achieved this by HIDING an inactive page;
  // there is no inactive page now, and the same promise is kept by the
  // component simply staying where it is.
  let coverSlot = $state<CoverSlot>('collection');

  /** Take the snapshot and fill the form from it. Called on the OPEN
   *  EDGE only — see the effect below. */
  function seedFromCollection() {
    seeded = collection;
    name = seeded.name;
    description = seeded.description;
    visibility = seeded.visibility as Visibility;
    baselineUpdatedAt = seeded.updated_at;
    coverAssetId = seeded.cover_asset_id ?? null;
    featuredCoverAssetId = seeded.featured_cover_asset_id ?? null;
    // `?? null` and NOT `|| null`: a stored zoom of 1 is a real value
    // — "framed, and the answer was the fit" — and truthiness would
    // read it as unset, silently turning an explicit choice into a
    // clear on the next save. Same trap #1081 closed on this table.
    framing = {
      featured: {
        x: seeded.featured_cover_focal_x ?? null,
        y: seeded.featured_cover_focal_y ?? null,
        zoom: seeded.featured_cover_zoom ?? null,
      },
      collection: {
        x: seeded.cover_focal_x ?? null,
        y: seeded.cover_focal_y ?? null,
        zoom: seeded.cover_zoom ?? null,
      },
    };
    // Which slot the cover block is pointed at is reset here with
    // everything else: every open starts on the collection's own cover,
    // which is the cover a collection HAS — the featured one is a
    // refinement for a strip the collection may never appear on, and it
    // is one click away with its own picture already on screen.
    coverSlot = 'collection';
    error = null;
    conflict = null;
    previewLadder.init();
    void loadCoverChoices(seeded.id);
  }

  // ── SEED ON THE OPEN EDGE, NOT ON EVERY PROP CHANGE (#1262) ───────
  //
  // `open` is the ONLY tracked read in this effect. Everything the
  // seed touches is read inside `untrack`, which is what makes the
  // dependency set the edge rather than the row: Svelte collects
  // dependencies through the whole call frame (so putting the reads in
  // a function does nothing on its own), and `collection.name` &c. are
  // prop reads that would otherwise re-run this on every refetch.
  //
  // The file used to carry a note explaining why `page` alone was
  // moved out of the seeded set — because a mid-edit re-seed snapped
  // the curator back to page 1. That note described the mechanism of
  // this bug and treated one field as the whole of it. The other seven
  // were doing the same thing, one of them to the concurrency token.
  //
  // ⛔ THE SEED IS STILL THE OPEN EDGE AND NOTHING ELSE (#1262). This
  // survives the #1264 restructure unchanged and deliberately: the bug
  // was that `previewLadder.init()`'s first statement reads a `$state`
  // guard, so the effect depended on `previewLadder.loaded` and re-ran
  // one `GET /previews` after every open — silently reverting a
  // curator's unsaved edits, INCLUDING a restriction. `open` is the
  // only tracked read here; everything else is inside `untrack`.
  $effect(() => {
    const isOpen = open;
    untrack(() => {
      if (isOpen) seedFromCollection();
    });
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
  //
  // Read off the SNAPSHOT (#1262). A refetch that moved the stored tier
  // off `public` mid-edit would otherwise pull the option out of the
  // row while `visibility` still held it, leaving the radio group with
  // nothing selected — the exact state this exception exists to
  // prevent, arriving from the other side.
  const publicOffered = $derived(site.publicModeEnabled || seeded.visibility === 'public');
  const tiers = $derived(publicOffered ? ALL_TIERS : ALL_TIERS.filter((v) => v !== 'public'));
  const publicIsInert = $derived(!site.publicModeEnabled && visibility === 'public');

  // What is IN the collection is the picker's options. The API accepts
  // ANY asset the curator may picture — that is what lets a cover
  // outlive the thing it was chosen from — but "the pictures already in
  // this collection" is the set that can be shown without inventing a
  // whole asset browser in a modal, and "upload a banner, add it, pick
  // it" reaches the rest.
  //
  // ⚠️ THE SOURCE CHANGED, and this is the load-bearing half of #1161
  // on the client. It read `GET /collections/{id}/resources` — the
  // collection's pinned ASSETS — and ADR 0091 retired the endpoints
  // that put assets there, so on any collection made from now on that
  // list is permanently empty and the picker has nothing to offer. The
  // ADR's own consequence line calls for replacing this read.
  //
  // That move is also what falsified the reason the read endpoint was
  // kept: it survived #1161 on the note "the cover picker uses it",
  // which this function had already stopped doing. #1236 retired it.
  //
  // It reads the collection's POSTS instead, and flattens their
  // members. That is the same sentence the old code meant — "the
  // pictures in this collection" — said in the model that now holds:
  // a collection contains posts, a post contains assets. No new
  // endpoint and no server change; `GET /collections/{id}/posts`
  // already returns hydrated posts whose members carry the same
  // readability marks this filter reads.
  //
  // A member this reader may not picture is dropped rather than shown
  // disabled: the server would refuse it, and offering a control that
  // cannot succeed is worse than not offering it. Duplicates are
  // collapsed — one asset can be a member of several posts in the same
  // collection, and the picker should offer each picture once.
  async function loadCoverChoices(id: string) {
    coverLoading = true;
    try {
      const { data } = await api.GET('/collections/{id}/posts', {
        params: { path: { id }, query: { limit: 60 } },
      });
      // ⚠️ `preview_available` and `ladder_available` sit on the
      // member's ASSET here, not on the member itself. On the old
      // `CollectionResource` shape they were top-level, and reading
      // them at the wrong depth silently yields `undefined` — which
      // this filter treats as "not picturable" and hands back an empty
      // picker that looks exactly like a collection with no pictures
      // in it. Flattened onto the CoverChoice the picker already
      // renders, so the depth is dealt with once, here.
      type PostMemberRow = {
        asset_id?: string;
        restricted?: boolean;
        asset?: { preview_available?: boolean; ladder_available?: boolean };
      };
      const seen = new Set<string>();
      const out: CoverChoice[] = [];
      for (const post of (data?.items ?? []) as Array<{ members?: PostMemberRow[] }>) {
        for (const m of post.members ?? []) {
          if (!m.asset_id || seen.has(m.asset_id)) continue;
          if (m.restricted || m.asset?.preview_available !== true) continue;
          seen.add(m.asset_id);
          out.push({
            asset_id: m.asset_id,
            restricted: false,
            preview_available: true,
            ladder_available: m.asset?.ladder_available,
          });
        }
      }
      coverChoices = out;
    } catch {
      // A picker that failed to load must not block renaming the
      // collection, so this is not surfaced as a form error.
      coverChoices = [];
    } finally {
      coverLoading = false;
    }
  }

  // ── The two chips: state AND selector (#1207 → #1264) ─────────────
  //
  // These began as a SUMMARY beside a button that opened an editor,
  // became the doors onto page 2 in #1213, and are now the control that
  // points the one editor at one slot. Each still draws its own slot's
  // picture at its own destination shape, so both covers are legible at
  // once — which is the half of "one surface" a slot switch on its own
  // would not deliver.
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
        params: { path: { id: seeded.id } },
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
            ? seeded.cover_asset_id
              ? { clear_cover: true }
              : {}
            : { cover_asset_id: coverAssetId }),
          // #1207 — the same tri-state twice more. The featured cover
          // is its own value/clear pair; the focal POINT is one flag
          // over two coordinates, because half a point is not a
          // positioning the server can honour.
          //
          // The `seeded.*` guards are what keep an unrelated edit
          // — a rename — from sending a clear for something that was
          // never set. A clear on an already-null column is harmless
          // today, but it is also a write the curator did not ask for,
          // and it would advance `updated_at` on every save.
          //
          // ⛔ THEY READ THE SNAPSHOT, NOT THE PROP (#1262), and this
          // is the half of that fix that is not about the form. Each
          // guard asks "was this already set when I started?", and the
          // body it composes is validated against `if_unchanged_since`
          // — a timestamp from that same instant. Read off the live
          // prop, a refetch landing mid-edit answers about a row NOBODY
          // in this dialog has seen: a curator who only renamed the
          // collection would send `clear_cover: true` for a cover a
          // third party had just chosen.
          ...(featuredCoverAssetId === null
            ? seeded.featured_cover_asset_id
              ? { clear_featured_cover: true }
              : {}
            : { featured_cover_asset_id: featuredCoverAssetId }),
          ...(framing.featured.x === null || framing.featured.y === null
            ? seeded.featured_cover_focal_x != null
              ? { clear_featured_cover_focal: true }
              : {}
            : {
                featured_cover_focal_x: framing.featured.x,
                featured_cover_focal_y: framing.featured.y,
              }),
          ...(framing.collection.x === null || framing.collection.y === null
            ? seeded.cover_focal_x != null
              ? { clear_cover_focal: true }
              : {}
            : { cover_focal_x: framing.collection.x, cover_focal_y: framing.collection.y }),
          // #1212 — two more tri-states, and every test here is
          // `=== null` / `!= null` rather than truthiness. A zoom of 1
          // is a meaningful stored value that happens to render like
          // the fit, so `zoom ? … : …` would send a clear for a value
          // the curator deliberately chose.
          ...(framing.featured.zoom === null
            ? seeded.featured_cover_zoom != null
              ? { clear_featured_cover_zoom: true }
              : {}
            : { featured_cover_zoom: framing.featured.zoom }),
          ...(framing.collection.zoom === null
            ? seeded.cover_zoom != null
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

<svelte:window bind:innerWidth={viewportWidth} />

<!-- ONE SURFACE, ONE TITLE, ONE WIDTH (#1264).
     The width no longer follows a page, because there are no pages. It
     is the width page 2 already had — `max-w-[min(96rem,95vw)]`, which
     measured 1536px at 1080p — and the owner's instruction was exactly
     that: "the edit collections modal is too small. It should be the
     same size as the set cover modal." A Tailwind size step would not
     do: "big enough to judge a picture by" is a proportion of the
     screen, and `max-w-7xl` on a 4k display is the cramped modal again
     with more whitespace around it.

     ⚠️ WIDTH IS FIXED, HEIGHT IS DRIVEN BY CONTENT — and that split is
     #1264's own instruction ("one sizing rule for both pages"), read
     together with #1220. The width is what the size JUMP was made of,
     so it is settled once and stops moving. The height is what #1220 is
     about: a dialog that reserves 1190px for a grid that fills 790 is
     an empty band, so nothing inside takes a definite height any more —
     every region is content-sized under a cap, and the cap is the
     viewport minus the chrome.

     `onclose` is `onclose`: Escape, the backdrop and the header X close
     the dialog, because there is nowhere left to step back to. -->
<Modal
  title={t('collections.edit_title')}
  {open}
  {onclose}
  panelClass="max-w-[min(96rem,95vw)]"
>
  <!-- THE BODY IS ONE SCROLLER WITH A CAP, AND NOTHING INSIDE IT TAKES
       A DEFINITE HEIGHT (#1220 + #1264).
       #1218 gave the cover page `height: min(calc(100vh - 12rem),
       68rem)` so its regions could grow into a stated box. #1220 is the
       mirror of that finding and it is the one that decides the rule:
       an ALLOCATED box a sparse grid cannot fill is a dead band —
       measured at ~400px on a collection with 59 picturable tiles. A
       cap does both jobs: the content grows to it when there is
       something to fill it, and stops short of it when there is not, so
       the dialog is as tall as what it holds and never taller.

       THE CAP IS THE VIEWPORT MINUS THE CHROME, not a fraction of the
       viewport. The chrome this dialog has to leave room for is a
       header, a footer and the backdrop's padding — 12rem, MEASURED
       rather than estimated (an earlier attempt guessed 9rem and put a
       1084px panel in a 1080px viewport). 80vh, the ceiling before
       that, is the wrong SHAPE of rule: it pushes Save off the bottom of
       a 600px window. The 68rem second cap is the one taste judgement —
       past it the crop stage is bigger than any decision it supports.

       ⚠️ ONE SCROLLER, NOT NESTED ONES. Each region under this cap is
       bounded by its own `max-height` and nothing claims `height: 100%`,
       because a percentage of an indefinite height is indefinite — the
       exact trap #1218 recorded, arriving from the other side. The
       picker grid keeps its own `overflow-y` so a DENSE grid scrolls
       inside itself rather than making the whole dialog thousands of
       pixels tall; a sparse one simply ends. -->
  <div
    class="grid items-start gap-x-6 gap-y-5 overflow-y-auto pr-1 lg:grid-cols-[minmax(0,24rem)_minmax(0,1fr)]"
    style="max-height: min(calc(100vh - 12rem), 68rem)"
    data-testid="collection-edit-body"
  >
    <!-- WHAT THE COLLECTION IS, in one column: the two text fields, the
         visibility ladder and whatever custom fields the instance
         defines. It was half of a `md:grid-cols-2` split that put the
         cover summary in the other half and left it empty below —
         #1264's "dead column at 896px". The column is narrow on purpose:
         a name and a description do not get better with more width, and
         everything the extra width buys goes to the cover editor, which
         is the control that needed it. -->
    <div class="min-w-0 space-y-3" data-testid="collection-edit-details-page">
    {#if error}
      <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
        {error}
      </p>
    {/if}
    {#if conflict}
      <div role="alert" data-testid="collection-edit-conflict"
           class="rounded border border-warning/40 bg-warning/10 px-3 py-2 text-sm">
        <p class="font-medium text-warning">{t('collections.conflict_heading')}</p>
        <p class="mt-1 text-xs text-fg-muted">{t('collections.conflict_body')}</p>
        <button
          type="button"
          onclick={acknowledgeConflict}
          data-testid="collection-edit-conflict-ack"
          class="mt-2 rounded border border-warning/60 px-2 py-1 text-xs font-medium text-warning hover:bg-warning/20"
        >{t('collections.conflict_overwrite')}</button>
      </div>
    {/if}

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
        <CollectionFieldsSection collectionId={seeded.id} />
    </div>

    <!-- HOW THE COLLECTION LOOKS — both slots, one editor, no page
         switch (#1264).
         The chips are the SELECTOR and the state at once: each draws its
         own slot's picture at its own destination shape, and the pressed
         one says which slot the editor below is pointed at. Two chips
         side by side is what makes "every cover type is in this surface"
         legible without a second control to find. -->
    <section
      data-testid="collection-cover-section"
      aria-label={t('collections.cover')}
      class="min-w-0 space-y-3"
    >
      <div class="flex flex-wrap items-start gap-4">
        <!-- The featured chip is drawn at the STRIP's aspect with the
             strip's own placement, so it answers "what did my framing
             do" for the slot that is NOT currently being edited. A
             square chip here would have hidden the very setting the
             editor exists to make. -->
        <button
          type="button"
          onclick={() => (coverSlot = 'featured')}
          aria-pressed={coverSlot === 'featured'}
          data-testid="collection-cover-edit-featured"
          class="w-32 rounded text-left focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        >
          <span class="mb-1 block text-[10px] uppercase tracking-wide"
                class:text-accent={coverSlot === 'featured'}
                class:text-fg-muted={coverSlot !== 'featured'}>
            {t('collections.cover_summary_featured')}
          </span>
          <span class="relative block overflow-hidden rounded border-2 bg-surface-elevated hover:border-border-strong"
                class:border-accent={coverSlot === 'featured'}
                class:border-border={coverSlot !== 'featured'}
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
          onclick={() => (coverSlot = 'collection')}
          aria-pressed={coverSlot === 'collection'}
          data-testid="collection-cover-edit-button"
          class="w-20 rounded text-left focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        >
          <span class="mb-1 block text-[10px] uppercase tracking-wide"
                class:text-accent={coverSlot === 'collection'}
                class:text-fg-muted={coverSlot !== 'collection'}>
            {t('collections.cover_summary_cover')}
          </span>
          <span class="block aspect-square overflow-hidden rounded border-2 bg-surface-elevated hover:border-border-strong"
                class:border-accent={coverSlot === 'collection'}
                class:border-border={coverSlot !== 'collection'}>
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

      <!-- ⛔ MOUNTED ONCE, POINTED AT A SLOT — never keyed on the slot.
           `{#key coverSlot}` here would remount it, and the picker's
           source tab, search text and results live inside it: the
           curator would come back to an empty grid every time they
           glanced at the other cover. That is the regression #1213
           recorded as passing every other assertion in the spec. -->
      <CollectionCoverEditor
        {coverSlot}
        choices={coverChoices}
        loading={coverLoading}
        collectionVisibility={visibility}
        bind:coverAssetId
        bind:featuredCoverAssetId
        {framing}
        {viewportWidth}
      />
    </section>
  </div>

  {#snippet footer()}
    <!-- ONE COMMIT FOR THE WHOLE SURFACE. Save applies the name, the
         description, the visibility, the custom fields and BOTH slots'
         covers and framing in the single PATCH. There is no page to be
         standing on and no Back to press. -->
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
