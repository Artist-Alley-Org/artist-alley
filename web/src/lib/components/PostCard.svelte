<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Grid-mode tile for a Post. The thumbnail treatment (thumbhash
  // placeholder, col variant, sprite scrub, typed-doc + icon fallbacks,
  // RS matte frame) lives in the shared CardThumb component (#515 slice
  // 1) — the same one AssetCard renders — so browse / profile /
  // post-by-asset all show one identical treatment. This card derives
  // the COVER asset from the post's members, wraps the link, and adds
  // the multi-asset badge + hover overlay.
  //
  // Click behavior (Phase 1.13.F-1): default left-click intercepts
  // navigation and updates the URL to /?post={id}, which opens the
  // modal as an overlay over the still-mounted feed. Modifier-key
  // clicks (cmd/ctrl/shift, middle-click) fall through to the native
  // href so users still get new-tab / new-window behavior.

  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import CardThumb from './CardThumb.svelte';
  import CardMenu from './CardMenu.svelte';
  import CardCheckbox from './CardCheckbox.svelte';
  import { auth } from '$stores/auth.svelte';
  import { site } from '$stores/site.svelte';
  import { selection } from '$stores/selection.svelte';
  import { cardTooltip } from '$stores/cardTooltip.svelte';
  import { t } from '$stores/lang.svelte';
  import { DEFAULT_TILE_SIZES, type ViewMode } from '$stores/browseView.svelte';
  import { masonryLayout, masonryOverlayTier } from '$stores/masonryLayout.svelte';
  import { api } from '$api/client';
  import type { CardCoverAsset, ContentOrigin } from '$components/cardAsset';
  import { kindForAsset } from './viewers/controller';
  import { thumbhashMatteColor } from '$lib/util/thumbhash';
  import CardKindBadge from './CardKindBadge.svelte';
  import CardAuthorLink from './CardAuthorLink.svelte';

  // Cover-asset shape is the shared card feed contract (#595) — its
  // presentation fields are REQUIRED so a surface cannot hand-map a
  // narrower object and silently lose the media-type badge + sprite
  // scrub. See cardAsset.ts.
  // #883 — `asset` is ABSENT on a member the viewer may not see, and
  // `restricted` says so. Optional here for exactly that reason; every
  // read below already goes through `?.`, and the cover branch checks
  // the flag rather than inferring from the missing object.
  interface PostMemberSummary {
    asset_id: string;
    asset?: CardCoverAsset;
    restricted?: boolean;
    owner_display_name?: string;
  }
  interface Post {
    id: string;
    title: string;
    description?: string;
    /** Required on the API's `Post` schema; optional here only because
     *  a couple of hand-built card feeds narrow the object. Absent means
     *  "not provably mine", which hides the manage-access action rather
     *  than offering one that 403s. */
    author_user_ref?: number;
    /** The renderable author, resolved server-side (#557).
     *
     *  ABSENT MEANS "NOT DISCLOSED", NOT "NO AUTHOR" — the author opted
     *  out of anonymous exposure and this reader is anonymous (ADR 0070
     *  §3 / ADR 0024), or the account is gone. The feed card then draws
     *  NO author header. Deliberately not a placeholder: "someone who
     *  opted out posted this" still discloses that they posted. */
    author?: PostAuthorSummary | null;
    /** The peer a remote post came from — the post-level twin of
     *  `asset.origin`. Remote work uses this same card and layout;
     *  what it must not be is unattributed. */
    origin?: ContentOrigin | null;
    /** Whether THIS reader has liked the post (#557). Server-resolved
     *  for the whole page, so the heart is right on first paint without
     *  a request per card. */
    liked?: boolean;
    /** The head of the thread — the newest few top-level comments, in
     *  the order the thread shows them (#1047). Server-resolved for the
     *  whole page like `author` and `liked`, and for the same reason: a
     *  card that fetched its own comments would be one request per post
     *  on a browse surface.
     *
     *  Optional and possibly empty, and the feed card renders nothing
     *  when it is. A commenter's `author` is absent when they are not
     *  disclosed to this reader (ADR 0024) — see PostAuthor and the
     *  server's `enrichTopComments`; the card draws the words with no
     *  name rather than a placeholder identity, which is the same
     *  answer the post's own header gives for a withheld author. */
    comments_preview?: PostCommentPreview[];
    cover_asset_id?: string | null;
    created_at: string;
    like_count: number;
    comment_count: number;
    members: PostMemberSummary[];
  }

  interface PostAuthorSummary {
    ref: number;
    username: string;
    display_name: string;
    avatar_url?: string | null;
  }

  /** One comment as the feed card shows it — a name, a line of text and
   *  when. The server's allow-list shape (#1047), not a trimmed
   *  `Comment`: threading, reactions and annotations are what opening
   *  the post is for. */
  interface PostCommentPreview {
    id: string;
    body: string;
    created_at: string;
    author?: PostAuthorSummary | null;
    /** A comment that arrived from a paired peer has no local user; the
     *  cached remote name rides here instead of in `author`. */
    remote_display_name?: string | null;
  }

  interface Props {
    post: Post;
    /** Feed mode: the card is the full column width, so the image is
     *  rendered far larger than a grid tile. Only affects `sizes` —
     *  the card treatment is deliberately identical. */
    feed?: boolean;
    /** The slot width to advertise in `<img sizes>` — browseView's
     *  `tileSizes`, which mirrors the tile clamp rather than its
     *  ceiling (#639). Never a `clamp()` / `min()`: `sizes` is not CSS
     *  and discards the whole attribute when it sees one. */
    tileSizes?: string;
    /** Active view mode (#515 slice 4). Grid = clean dense wall (no frame,
     *  hover-only title); thumbnail = framed "details" tile with a
     *  persistent metadata footer. */
    mode?: ViewMode;
    /** The true size of the post's membership, when the caller's row
     *  carries fewer members than the post has (#850).
     *
     *  A search hit ships ONE member — the cover — because a tile renders
     *  one image and joining a whole membership per hit would be an
     *  unbounded query for pixels nobody looks at. The multi-asset badge
     *  still has to tell the truth, so the count travels beside the
     *  member instead of being inferred from the array's length.
     *  Undefined ⇒ the array IS the membership, which is the case on
     *  every list endpoint. */
    memberCount?: number;
    /** Feed-order ids for range selection (#1127). A thunk — see
     *  CardCheckbox's prop of the same name. */
    orderedIds?: () => string[];
  }

  let {
    post,
    feed = false,
    tileSizes = DEFAULT_TILE_SIZES,
    mode = 'grid',
    memberCount: memberCountProp,
    orderedIds,
  }: Props = $props();

  /** Selection is gated exactly as CardCheckbox gates it — same two
   *  conditions, because a shift-click that selected on a surface with
   *  no visible checkbox would be an invisible mode. */
  const canSelect = $derived(!!auth.user && !site.demoMode);

  // ── Per-density posture (#1047) ──────────────────────────────────
  //
  // The two WALL densities — grid and masonry — wear no card chrome, and
  // masonry joined grid in this pass. See the twin in AssetCard for the
  // argument: a rounded, bordered, elevated panel is metadata about a
  // card, on a surface whose stated identity is "maximum art per page".
  const framed = $derived(mode !== 'grid' && mode !== 'masonry');
  const detailed = $derived(mode === 'thumbnail');

  // ── Masonry's two postures (#652, split by scale in #1047) ────────
  //
  // MASONRY IS MINIMAL WHEN ITS TILES ARE SMALL AND FULL WHEN THEY ARE
  // BIG, and the switch is the tile's RENDERED BOX, not the rung the
  // reader picked (owner amendment; #1025's lesson — a rung is a clamp,
  // and the same clamp yields different sizes at different viewports).
  // `masonryLayout` carries the measured box and the calibration notes
  // for all three thresholds.
  //
  // THREE POSTURES, NOT TWO (#1139). Width alone was the gate and it let
  // through the one shape it could not describe: a wide tile is SHORT by
  // construction, so a two-column 5.33:1 piece cleared 280px easily and
  // then cut the artist's avatar and name off at its bottom edge. Height
  // now qualifies too, and between the two heights the overlay
  // COMPRESSES to the title alone rather than clipping — a clipped
  // identity row states less than the tooltip it replaced while still
  // covering the art.
  //
  // Below both, the tile can be as short as the 60px control floor, so
  // it holds the ⋮ menu and the checkbox and nothing else and its facts
  // live in the hover tooltip.
  const overlayTier = $derived(
    mode === 'masonry' ? masonryOverlayTier(masonryLayout.box(post.id)) : 'minimal',
  );
  const masonryWide = $derived(mode === 'masonry' && overlayTier !== 'minimal');
  const compact = $derived(mode === 'masonry' && !masonryWide);

  // ── Feed: the social card (#557) ─────────────────────────────────
  //
  // Feed used to be a grid tile rendered one per row and slightly
  // bigger — a wider wall, not a feed. This is the mode where the card
  // becomes a POST rather than a thumbnail of one: author header on
  // top, media at its own shape, actions underneath.
  //
  // The other four modes are untouched and stay untouched. Grid is a
  // contact sheet (#555), thumbnail is a details tile (#556), masonry
  // packs unequal heights (#640) — all of them are walls of artwork
  // where the author is at most a footnote. Feed is the one place a
  // reader is looking at posts one at a time, which is the only place
  // an author header, an action bar and a caption earn their vertical
  // space.
  const social = $derived(mode === 'feed');

  // Absent author = withheld, not missing (see the `author` prop). The
  // header is dropped entirely rather than rendered with a placeholder.
  const author = $derived(post.author ?? null);
  const authorInitials = $derived(initialsOf(author?.display_name ?? ''));

  function initialsOf(name: string): string {
    const parts = name.trim().split(/\s+/).filter(Boolean);
    if (parts.length === 0) return '?';
    if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
    return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
  }

  const origin = $derived(post.origin ?? null);

  // The head of the thread (#1047). Rendered only in the social view —
  // the other four densities are walls of artwork where a conversation
  // is a count at most.
  const commentsPreview = $derived(post.comments_preview ?? []);

  /** The commenter's name, or "" when there is none to print.
   *
   *  THREE CASES, and only the first two produce a name: a local
   *  commenter the server disclosed, a remote one whose peer has
   *  shipped a display hint, and a commenter who is NOT disclosed to
   *  this reader (the ADR 0024 opt-out, a deleted account, or a peer
   *  with no hint yet). The third renders the comment with no name —
   *  never a placeholder, never the actor URI's host, because both
   *  would be this client inventing an identity the server declined to
   *  send (#1023). */
  function commenterName(c: PostCommentPreview): string {
    if (c.author) return c.author.display_name;
    return c.remote_display_name ?? '';
  }

  // Like state. Seeded from the payload (#557) so the heart is correct
  // on first paint — no per-card `GET /posts/{id}/like`, which on a
  // 20-card page is exactly the N+1 the author object exists to remove.
  //
  // `$state` initialised from a `$derived` would freeze at the first
  // value, so the local optimistic state is reconciled against the prop
  // instead: `likeOverride` is null until the reader touches the button,
  // and the moment they do it wins.
  let likeOverride = $state<{ liked: boolean; delta: number } | null>(null);
  let likeBusy = $state(false);
  const liked = $derived(likeOverride ? likeOverride.liked : !!post.liked);
  const likeCount = $derived(Math.max(post.like_count + (likeOverride?.delta ?? 0), 0));

  // Writes need a signed-in reader on a non-demo install — the same gate
  // CardMenu applies to its write actions, for the same reason: the demo
  // blocks writes at the nginx edge (ADR 0060), so an enabled control
  // there is a 403 dead end. Signed out, the heart still RENDERS with
  // its count (the count is public information and the card would look
  // broken without it) but does not offer to toggle.
  const canLike = $derived(!!auth.user && !site.demoMode);

  async function toggleLike(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    if (!canLike || likeBusy) return;
    likeBusy = true;
    const was = liked;
    const baseDelta = likeOverride?.delta ?? 0;
    // Optimistic: flip locally, and reverse exactly this flip on failure
    // rather than recomputing from a count the server may have moved.
    likeOverride = { liked: !was, delta: baseDelta + (was ? -1 : 1) };
    try {
      const params = { path: { id: post.id } };
      const { error } = was
        ? await api.DELETE('/posts/{id}/like', { params })
        : await api.POST('/posts/{id}/like', { params });
      if (error) likeOverride = { liked: was, delta: baseDelta };
    } catch {
      likeOverride = { liked: was, delta: baseDelta };
    } finally {
      likeBusy = false;
    }
  }

  // "Comment" opens the post — the thread lives in the modal
  // (CommentsThread), and this card deliberately does not grow a second
  // comment surface. Same navigation the card's own link performs, so
  // the reader lands where the conversation is.
  async function openComments(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    await handleClick(e);
  }

  // Share = copy the permalink, the same act CardMenu's copy-link
  // performs and the same wording. A read-only action, so it is offered
  // to anonymous readers too.
  let shared = $state(false);
  let shareTimer: ReturnType<typeof setTimeout> | null = null;
  async function sharePost(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    try {
      await navigator.clipboard.writeText(`${location.origin}/posts/${post.id}`);
      shared = true;
      if (shareTimer) clearTimeout(shareTimer);
      shareTimer = setTimeout(() => (shared = false), 1500);
    } catch {
      // Clipboard denied (insecure context / permission). Nothing to
      // recover — the permalink is also the card's own href.
    }
  }

  // Only the author gets the manage-access action (#880). The API gate
  // is `canMutatePost` — author, `posts.admin` or `system.admin` — and
  // this deliberately checks only the first: the card has the author ref
  // and not the caller's capabilities, so widening the client-side test
  // would mean guessing. An admin who needs the dialog opens the post,
  // where PostHost asks the same question with the post loaded. Guessing
  // wide here would show admins-in-waiting a menu item that 403s.
  const isAuthor = $derived(
    !!auth.user && post.author_user_ref !== undefined && post.author_user_ref === auth.user.ref,
  );

  // Pick the cover asset id (explicit cover → first member → nothing),
  // then resolve its summary from members. CardThumb turns these into
  // the framed thumbnail + preview_available gating.
  const coverAssetId = $derived(
    post.cover_asset_id ?? (post.members.length > 0 ? post.members[0].asset_id : null),
  );
  const coverMember = $derived(post.members.find((m) => m.asset_id === coverAssetId));
  const coverAsset = $derived(coverMember?.asset);
  // #883 — the cover itself can be a member the viewer may not see. The
  // tile then states the restriction and the owner instead of degrading
  // to the generic no-preview plate, which would be indistinguishable
  // from "this asset has no thumbnail yet".
  const coverRestricted = $derived(!!coverMember?.restricted);
  const coverOwnerName = $derived(coverMember?.owner_display_name ?? null);
  const coverThumbhash = $derived(coverAsset?.thumbhash ?? null);
  const coverHasFile = $derived(!!coverAsset?.file_hash);
  const coverPreviewAvailable = $derived(!!coverAsset?.preview_available);
  const coverFileExtension = $derived(coverAsset?.file_extension ?? null);

  // The `ladder_available` signal arrived in #610, so the responsive
  // srcset these two props were parked for is live (#502/#589) — the
  // `void` lines that dead-coded them are gone.
  //
  // `sizes` tells the browser how wide the slot actually is, which is
  // what stops a 200px tile pulling `hires`. Feed is one full-width
  // column capped at the `measure` (see ContentGrid); every other mode
  // is a tile at the user's chosen rung.
  const coverLadderAvailable = $derived(!!coverAsset?.ladder_available);

  // The hover-scrub gate (#835) — see CardThumb.scrubAvailable.
  const coverScrubAvailable = $derived(!!coverAsset?.scrub_available);

  // The cover's recorded dimensions (#640) — what lets a masonry tile
  // reserve its height before the image loads. `?? null` because a post
  // whose cover member is missing has no asset at all, which is the same
  // answer as "no dimensions recorded".
  const coverPixelWidth = $derived(coverAsset?.pixel_width ?? null);
  const coverPixelHeight = $derived(coverAsset?.pixel_height ?? null);
  // Feed is one column capped at a 46rem MEASURE (ContentGrid's
  // `.posts-feed { max-width: min(100%, 46rem) }`), so the slot is the
  // viewport up to that cap and the cap above it.
  //
  // Was `min(100vw, 46rem)`, which — unlike the tile hint — was already
  // telling the truth: measured in Chromium it resolves to 736px against
  // a 737px feed column. This is the same value in the portable spelling
  // plus the `auto` first entry, so feed and tiles share one shape and
  // neither depends on a CSS math function parsing inside a non-CSS
  // attribute. See browseView.tileSizes for the whole argument.
  const FEED_SIZES = 'auto, (max-width: 46rem) 100vw, 46rem';
  const sizesHint = $derived(feed || social ? FEED_SIZES : tileSizes);

  // ── Uniform feed sizing (#1047) ──────────────────────────────────
  //
  // THE FEED'S MEDIA BOX IS THE SAME SHAPE ON EVERY POST. #557 sized it
  // from the cover's own aspect ratio with a 4:5 portrait cap, which is
  // right for a wall and wrong for a column: scrolling a single-column
  // feed whose every card is a different height means the like button,
  // the caption and the comments land somewhere new on every post, and
  // the reader re-finds them each time. The owner's density table asks
  // for "uniform post sizing (thumbnail-view-like), not aspect-driven",
  // and that is what a social feed's grid of controls needs.
  //
  // A SQUARE, and letterboxed rather than cropped, so the whole work is
  // still shown — CardThumb's rule everywhere `fill` is off. The
  // alternative, cropping to the box, is what the commercial feeds do
  // and what this codebase deliberately does not do outside grid's
  // contact sheet.
  //
  // `variableAspect` and `ratioFloor` are therefore both off in feed
  // now, and the 4:5 cap they existed to apply here goes with them: a
  // fixed box needs no floor. CardThumb's `aspect-square` default is
  // the box.

  // Hover state lives on the interactive <a> and feeds CardThumb's
  // sprite-scrub animation.
  let hovering = $state(false);

  // Selected state (#515 slice 3). A browse card contributes its POST id
  // to the shared selection.
  const selected = $derived(selection.has(post.id));

  // #555 — grid is a zero-gap CONTACT SHEET: drop the card chrome
  // (rounded / border / elevated bg) so tiles butt into one unbroken
  // wall, and let hover lift + enlarge the tile. The z-lift matters at
  // zero gap: without it a scaling tile slides under its neighbours.
  // Selection uses an INSET ring here so it reads inside the tile
  // instead of bleeding over the adjacent one.
  //
  // #596 — per-tile 2px inset + 4px radius, filled with the matte, so
  // neighbours put 4px between their images while the GRID stays flush.
  // See the twin in AssetCard for the full reasoning (and why these are
  // literal px rather than Tailwind's rem scale).
  const wrapperClass = $derived(
    framed
      ? `rounded-lg bg-surface-elevated border ${selected ? 'border-accent ring-2 ring-accent' : 'border-border hover:border-fg-muted/60'}`
      : `p-[2px] rounded-[4px] bg-thumb-matte hover:z-10 hover:scale-[1.03] ${selected ? 'z-10 ring-2 ring-inset ring-accent' : ''}`,
  );

  const memberCount = $derived(memberCountProp ?? post.members.length);

  /** The extensions of the members this reader may actually READ, in
   *  the normalised form the band prints.
   *
   *  READABLE MEMBERS ONLY, and that is a rule about disclosure, not
   *  tidiness. A restricted member ships with `restricted: true` and no
   *  `asset` at all (#883) — there is nothing to read an extension off,
   *  and there must not be: if a hidden .glb could flip a wall of three
   *  visible PNGs from "png" to "mixed", the band would have announced
   *  the existence and the foreignness of a file this reader was
   *  refused. Recomputing over all members is exactly that leak, and it
   *  is the #902/#1066 derived-copy class arriving on the card instead
   *  of in a query.
   *
   *  Under the default feed (#921) restricted members are dropped from
   *  `members` upstream and this filter is a no-op; with the
   *  show-restricted preference on they arrive as placeholders and it
   *  is the thing doing the work. Both paths read the SAME `restricted`
   *  flag the server wrote from its one readability decision — this is
   *  not a second opinion about who may see what. */
  const readableExtensions = $derived(
    post.members
      .filter((m) => !m.restricted && m.asset)
      .map((m) => (m.asset?.file_extension ?? '').replace(/^\./, '').toLowerCase()),
  );

  /** Does this payload carry the whole membership?
   *
   *  A search hit ships a cover row with the real total beside it
   *  (`memberCount`), so `readableExtensions` there describes one
   *  member out of four and "they all share an extension" would be a
   *  sentence about a set this card never received. Uniformity is
   *  UNKNOWABLE on a truncated payload, and unknowable is spelled as no
   *  band text rather than as a guess in either direction.
   *
   *  Restricted members do NOT make a payload truncated: they arrive as
   *  placeholder entries (or are dropped along with the count that
   *  described them), so the totals still line up. */
  const membersComplete = $derived(memberCount <= post.members.length);

  /** The one extension every readable member shares, or null when they
   *  disagree — the MIXED case — or when there is nothing to compare. */
  const uniformExtension = $derived(
    readableExtensions.length > 0 && new Set(readableExtensions).size === 1
      ? readableExtensions[0] || null
      : null,
  );

  /** The extension the thumbnail's band shows, or null to show none.
   *
   *  SINGLE ASSET — the owner's original refinement: "For thumbnails on
   *  posts with only one asset, it can show the extension." That arm is
   *  unchanged; it reads the cover's own extension.
   *
   *  MULTI-ASSET (#1190) — the owner's follow-up: "if a multi asset post
   *  contains all the same extension (glb, png, etc...) we can place the
   *  extension on the thumbnail. Not if it's mixed. Maybe we can put
   *  (mixed) for the extension instead?"
   *
   *  This retires the old blanket suppression rather than bending it.
   *  The reason a set used to show nothing was that the COVER's
   *  extension is not the SET's fact — a carousel of a PNG, a PSD and an
   *  MP4 labelled "png" says something false about the other two, the
   *  same failure #1111 named when it made the badge state the set
   *  ("4 ⬠") instead of one member's kind. A pack whose members are all
   *  .glb has no such problem: "glb" is true of every one of them. So
   *  the rule is not "one asset" but "one ANSWER", and where there is no
   *  single answer the band says so in words instead of printing one
   *  member's and hoping.
   *
   *  `memberCount` and not `post.members.length` decides which arm runs,
   *  which is the truth rule from the props block.
   *
   *  Withheld entirely when the COVER is restricted. The band already
   *  suppresses the kind badge on that condition — a card that hides the
   *  icon and then prints "psd", or even "mixed", beside the gap has
   *  disclosed something about what it just withheld. */
  const bandExtension = $derived(
    coverRestricted
      ? null
      : memberCount <= 1
        ? (coverFileExtension ?? '').replace(/^\./, '') || null
        : !membersComplete || readableExtensions.length === 0
          ? null
          : (uniformExtension ?? t('card.band.mixed')),
  );

  /** True when `bandExtension` is the WORD rather than an extension.
   *  The span reads as an extension by position and by casing, so the
   *  accessible name spells out what it means, and the attribute gives
   *  the placement tests something to assert that is not the English. */
  const bandMixed = $derived(
    bandExtension !== null && !coverRestricted && memberCount > 1 && uniformExtension === null,
  );

  // ── #1111: the grid card's overlay ──────────────────────────────────
  //
  // At rest a grid tile is IMAGE ONLY — the reference's discovery-wall
  // posture. Everything below appears on hover AND on keyboard focus,
  // over the same gradient scrim the title already used.
  //
  // What moves, and where:
  //   top-left     the asset KIND, as an icon. Multi-asset posts show
  //                the count then the Shapes glyph instead.
  //   bottom-left  the title, then a 40px avatar + the author's name.
  //   right of it  the ⋯ menu, relocated out of the top-right corner.
  //   top-right    the select checkbox, which inherits the corner ⋯
  //                vacated (see CardCheckbox.corner).
  //
  // The kind icon replaces CardThumb's `video` / `3D` TEXT chip, which
  // is switched off for this mode only (`kindBadge` below) — #1111 is
  // explicit that no text kind badges survive in grid.

  /** The cover's kind, resolved through the SAME function CardFallback
   *  and the viewer router use. Not a second extension table: a PNG
   *  uploaded as a sprite atlas is a sprite sheet, and only
   *  `kindForAsset` knows that. */
  const coverKind = $derived(
    kindForAsset({ asset_type: coverAsset?.asset_type ?? null, file_extension: coverFileExtension }),
  );

  /** Grid, and a WIDE masonry tile (#1047) — the two discovery-wall
   *  postures, which the owner's amendment makes one language at scale.
   *
   *  Only when the tile is a real card: a restricted cover states its
   *  own restriction and must not also be labelled by kind, since the
   *  kind of something you may not see is not yours to know. */
  const showOverlay = $derived((mode === 'grid' || masonryWide) && !coverRestricted);

  // The corner conflict was a THREE-way fight, not the two the brief
  // described (#578). Top-left already hosts the select checkbox AND
  // CardThumb's persistent video/3D media-type badge; top-right now hosts
  // the ⋮ menu. Putting the multi-asset indicator top-left too overlapped
  // the type badge on every video/3D multi-asset tile (measured). So it
  // goes BOTTOM-left — the one persistently-clear corner — still always
  // visible, still the single piece of resting chrome.

  /** The letterbox matte's colour in THUMBNAIL only (#1136) — sampled
   *  from the cover's thumbhash, which the card already holds for its
   *  loading placeholder. Null everywhere else and for any cover with
   *  no hash, and CardThumb then paints its neutral token. */
  const matteColor = $derived(detailed ? thumbhashMatteColor(coverThumbhash) : null);

  const created = $derived(new Date(post.created_at));
  const createdShort = $derived(
    created.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }),
  );

  // Tooltip payload (#652) — the facts the compact overlay drops, in
  // scan order. Multi-asset count is here because the badge that used
  // to carry it is one of the things `compact` removes: it sits
  // bottom-right, which on a 60px tile is the SAME 44px band as the ⋮
  // menu top-right. Likes/comments deliberately stay out — they are
  // engagement, not identification, and this is not the details card.
  const tipMeta = $derived(
    [
      coverFileExtension ? coverFileExtension.replace(/^\./, '').toUpperCase() : null,
      coverPixelWidth && coverPixelHeight ? `${coverPixelWidth} × ${coverPixelHeight}` : null,
      memberCount > 1 ? t('card.multi.badge_label', { count: String(memberCount) }) : null,
      createdShort,
    ].filter((v): v is string => !!v),
  );

  // ── #1126: the grid overlay's full-title tooltip ────────────────────
  //
  // ONLY WHEN THE TITLE IS ACTUALLY CLIPPED. A tooltip that repeats a
  // title already legible on the card is noise on every hover, so the
  // gate is the DOM's own answer — `scrollWidth > clientWidth` on the
  // truncating element — rather than a character count, which cannot
  // know the rendered font, the tile width or the reader's zoom.
  //
  // Measured at hover time, not cached: the tile's width changes with
  // the `--tile-min` slider and the viewport, so a value taken at mount
  // would be stale for the whole session. One layout read per hover on
  // one element is not a cost worth caching against.
  let titleEl = $state<HTMLElement | null>(null);
  function titleIsClipped(): boolean {
    return !!titleEl && titleEl.scrollWidth > titleEl.clientWidth;
  }

  const tipTitle = $derived(post.title || 'Untitled');

  function tipEnter(e: MouseEvent) {
    hovering = true;
    if (compact) {
      cardTooltip.enter(post.id, { title: tipTitle, meta: tipMeta }, e);
    } else if (showOverlay && titleIsClipped()) {
      // No meta line: masonry's tooltip exists to carry the facts its
      // stripped overlay dropped, this one exists to finish a sentence
      // the card started. Adding dimensions here would answer a
      // question the reader did not ask.
      cardTooltip.enter(post.id, { title: tipTitle, meta: [], placement: 'follow' }, e);
    }
  }
  function tipMove(e: MouseEvent) {
    if (compact || showOverlay) cardTooltip.move(post.id, e);
  }
  function tipLeave() {
    hovering = false;
    if (compact || showOverlay) cardTooltip.leave(post.id);
  }

  /** The keyboard arm. Focusing the card shows the same full title
   *  pinned under the tile — see CardTooltip for why this is not also an
   *  `aria-describedby` (the stretched link's accessible name already
   *  carries the untruncated string; truncation is pixels, not
   *  semantics). */
  function tipFocus(e: FocusEvent) {
    if (!showOverlay || !titleIsClipped()) return;
    cardTooltip.showFor(post.id, { title: tipTitle, meta: [] }, e.currentTarget as HTMLElement);
  }
  function tipBlur() {
    if (showOverlay) cardTooltip.leave(post.id);
  }

  async function handleClick(e: MouseEvent) {
    // SHIFT IS NOW A SELECTION GESTURE, not a browser one (#1127).
    // Shift+click used to fall through to the native href, which opens
    // a new WINDOW — a behaviour nobody reaches for on a wall of art,
    // and the modifier every file manager reserves for range selection.
    // ctrl/cmd (new tab) and alt (download) are untouched, so nothing
    // people actually use is taken away.
    //
    // preventDefault FIRST: without it the navigation happens whatever
    // the selection does, which is the trap #1127 names explicitly.
    if (e.shiftKey && !e.metaKey && !e.ctrlKey && !e.altKey && e.button === 0 && canSelect) {
      e.preventDefault();
      e.stopPropagation();
      selection.extendTo(post.id, orderedIds ? orderedIds() : [post.id]);
      return;
    }
    // Modifier-key / non-primary clicks fall through to the native
    // <a href>. Standard browser behavior: new tab, new window,
    // download — all preserved.
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) {
      return;
    }
    e.preventDefault();
    const target = new URL(page.url);
    target.searchParams.set('post', post.id);
    await goto(target.pathname + target.search, {
      keepFocus: true,
      noScroll: true,
    });
  }
</script>

<!--
  Stretched-link pattern (#515 slice 2): container div (not an <a>) so the
  tool row's <button>s aren't nested in an anchor. The whole-card <a>
  keeps the /?post={id} modal intercept (handleClick) with /posts/{id} as
  the modifier-click / new-tab fallback.
-->
<!-- `data-select-id` is what the marquee hit-tests against (#1127). It
     rides the CARD ROOT rather than the checkbox because the band
     selects a card when it touches the card, not when it happens to
     clip a 24px control in one corner. -->
<div
  data-select-id={post.id}
  class="group relative block overflow-hidden transition duration-200 {wrapperClass}"
>
  {#if social}
    <!-- Feed HEADER (#557) — avatar, name, time, ⋯.
         The author leads the card because in a feed the question is
         "who made this", where in a grid it is "what is this".

         NO AUTHOR = NO HEADER ROW'S LEFT SIDE, not a placeholder
         identity. See the `author` prop: the absence is the opt-out
         working. The row itself stays so the ⋯ menu keeps its corner —
         a card with no overflow menu would be the only one in the app. -->
    <!-- `relative` + `pr-11` are both load-bearing, not spacing taste.
         CardMenu's root is `absolute right-2 top-2`, so it anchors to
         the nearest POSITIONED ancestor: without `relative` here it
         escapes the header and pins itself to the card, which happens
         to look almost right and is not. And because it is absolute it
         reserves no flex space, so the author's name would run
         underneath it on a long display name — `pr-11` is the 44px
         control it cannot otherwise claim. -->
    <header class="relative flex items-center gap-2.5 px-3 py-2.5 pr-11">
      {#if author}
        <a
          href="/users/by-username/{author.username}"
          class="flex min-w-0 flex-1 items-center gap-2.5 rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          title={t('card.feed.author_profile', { name: author.display_name })}
          data-testid="feed-author"
        >
          <span
            class="flex h-9 w-9 shrink-0 items-center justify-center overflow-hidden rounded-full bg-accent/20 text-xs font-semibold text-accent"
          >
            {#if author.avatar_url}
              <img src={author.avatar_url} alt="" class="h-full w-full object-cover" />
            {:else}
              {authorInitials}
            {/if}
          </span>
          <span class="min-w-0 flex-1">
            <span class="block truncate text-sm font-semibold text-fg hover:underline">
              {author.display_name}
            </span>
            <span class="block truncate text-xs text-fg-muted">
              @{author.username}
              <span aria-hidden="true">·</span>
              {createdShort}
              {#if origin}
                <!-- Provenance rides the feed card the same way it rides
                     every asset density (#552). Remote work gets the
                     same card and the same layout — attributed, not
                     marked lesser. Never the bare UUID. -->
                <span aria-hidden="true">·</span>
                <span data-testid="card-origin" title={t('card.origin_label')}>
                  ↗ {t('card.origin_from', { peer: origin.display_name })}
                </span>
              {/if}
            </span>
          </span>
        </a>
      {:else}
        <!-- Withheld identity: the space stays empty. The post's own
             date is still its own fact and is not the author's. -->
        <span class="min-w-0 flex-1 truncate text-xs text-fg-muted">{createdShort}</span>
      {/if}
      <!-- ⋯ lives in the header here rather than floating over the art.
           CardMenu portals its panel and positions from the trigger
           rect, so it works identically outside CardThumb. -->
      <CardMenu
        postId={post.id}
        detailPath="/posts/{post.id}"
        manageAccess={isAuthor ? { kind: 'post', id: post.id } : null}
      />
    </header>
  {/if}

  {#if detailed}
    <!-- ═══ #1136: the TOP CHROME BAND ═════════════════════════════
         THE OWNER'S PLACEMENT GRAMMAR, top row: format on the left, type
         icon on the right — above the preview, never over it.

         This REPLACES #556's title header, and the swap is the point of
         the issue rather than a casualty of it. #556 put the title up
         here on "there should be a top to the thumbnail cards"; the
         reference panel answers that same request with a FORMAT band and
         puts the title with the rest of the metadata below the picture,
         where the one-fact-per-row stack reads as a record. The card
         still has a top; it now says what KIND of thing this is, which
         is the question a working shelf asks first and the one the
         filename half-answers.

         #1158 — THE EXTENSION IS GONE, and the band now holds exactly
         TWO things: the KIND BADGE left, the CHECKBOX right. The ⋯ menu
         has left it entirely (#1171 follow-up, owner's ruling: "I like
         the menu bottom right. Asset type icon and count top left and
         checkbox top right"). See the metadata stack's last row, which
         is where it went, and CardMenu's trigger comment for the
         sizing that move required.

         #1136 opened this band as a FORMAT band and #1144 narrowed the
         extension to "where it is unambiguous" (single-asset, ≥sm). The
         owner's ruling retires that whole line: the extension text never
         renders in thumbnail view at all. The icon already answers "what
         kind of thing is this?" exactly — for all thirteen ViewKinds, at
         every width, with #1144's tooltip spelling the type out in words
         — and the extension was a second, coarser answer to the same
         question sitting next to it. Two labels for one fact is what the
         density pass has been removing everywhere else (#1047, #1124).

         The ordering is the other half. The band held CONTENT (kind) and
         CONTROLS (checkbox, ⋯) interleaved, with the checkbox first, so
         the eye met a widget before it met the fact. Now it is one fact
         and one control: what the card IS on the left, and the one
         control that belongs to READING a shelf — the checkbox — on the
         right. The checkbox moving to the right is a MOVE, not a second
         checkbox: same `CardCheckbox`, same `orderedIds`, so
         shift-range and the marquee's passthrough behave as before.

         ONE GAP, AND IT IS THE ONLY ONE. #1158's band held a two-control
         CLUSTER and needed a note about not spreading it; with the ⋯
         gone there is no cluster left to hold together, so the elastic
         span simply pushes the checkbox to the edge. The band keeps its
         `gap-2` for the degenerate case where a restricted cover
         suppresses the badge and the checkbox is all there is. -->
    <div
      class="flex items-center gap-2 border-b border-border px-1.5 py-0.5"
      data-testid="thumb-band-top"
    >
      {#if !coverRestricted}
        <CardKindBadge kind={coverKind} count={memberCount} variant="inline" tooltipKey={post.id} />
      {/if}
      {#if bandExtension}
        <!-- The format, in one of three readings — see `bandExtension`.
             Single-asset: that file's extension. Multi-asset with one
             shared extension: that extension, because it is true of
             every member. Multi-asset and mixed: the word, styled
             identically so the band keeps ONE slot with one meaning
             ("what format is this?") rather than growing a second.

             Same type scale and same position as AssetCard's band,
             because on a one-asset post this card IS showing a file and
             there is no reason for the two to look different when they
             are saying the same thing. -->
        <span
          class="min-w-0 truncate text-[11px] font-medium uppercase tracking-wide text-fg-muted"
          data-testid="thumb-band-extension"
          data-mixed={bandMixed ? 'true' : undefined}
          aria-label={bandMixed ? t('card.band.mixed_label') : undefined}
        >{bandExtension}</span>
      {/if}
      <span class="flex-1"></span>
      <CardCheckbox id={post.id} placement="inline" {orderedIds} />
    </div>
  {/if}

  <CardThumb
    assetId={coverAssetId}
    title={post.title || 'Untitled'}
    thumbhash={coverThumbhash}
    fileExtension={coverFileExtension}
    hasFileHash={coverHasFile}
    previewAvailable={coverPreviewAvailable}
    scrubAvailable={coverScrubAvailable}
    ladderAvailable={coverLadderAvailable}
    {sizesHint}
    {hovering}
    framed={framed && !social}
    fill={mode === 'grid'}
    variableAspect={mode === 'masonry'}
    {compact}
    pixelWidth={coverPixelWidth}
    pixelHeight={coverPixelHeight}
    titleAdjacent={detailed}
    {matteColor}
    restricted={coverRestricted}
    restrictedOwnerName={coverOwnerName}
  >
    <!-- Whole-card navigation target (modal intercept + permalink
         fallback). Hover here drives CardThumb's sprite-scrub and, in
         masonry, the shared hover tooltip (#652). -->
    <a
      href="/posts/{post.id}"
      onclick={handleClick}
      onmouseenter={tipEnter}
      onmousemove={tipMove}
      onmouseleave={tipLeave}
      onfocus={tipFocus}
      onblur={tipBlur}
      class="absolute inset-0 z-[1]"
      aria-label={post.title || 'Untitled'}
      data-marquee-passthrough
    ></a>

    <!-- Multi-select checkbox. Top-left everywhere except the #1111
         grid overlay, where top-left carries the kind icon and the ⋯
         menu has vacated top-right.
         NOT IN THUMBNAIL (#1136): that density's chrome lives in the
         frame, so the checkbox is an inline control in the bottom band
         below and nothing at all sits over the preview. -->
    {#if !detailed}
      <CardCheckbox id={post.id} corner={showOverlay ? 'right' : 'left'} {orderedIds} />
    {/if}

    <!-- Kind / multi-asset indicator. BOTTOM-right, PERSISTENT — the one
         piece of chrome that stays at rest outside grid, so a wall of art
         still signals what each tile IS and which posts hold a set. Every
         other corner is claimed: checkbox top-left, ⋮ menu top-right,
         and the hover title overlay is bottom-LEFT-aligned.

         ONE BADGE, ONE NOTATION (#1047). This was two: a hand-rolled
         stacked-squares SVG here for the non-grid densities, and #1111's
         Shapes glyph in the grid overlay — the same fact drawn two ways
         in two corners, which is half of what PR #1124 flagged. And it
         said nothing at all about a SINGLE-asset post, whose kind was
         left to CardThumb's two-of-thirteen text chip. CardKindBadge
         answers both: the count plus Shapes for a set, the kind glyph
         for one.

         Suppressed under `compact` (#652). On a 60px masonry tile
         "bottom-right" and "top-right" are the same 44px band, so the
         badge and the ⋮ menu sit on top of each other — the owner's
         "only keep the options and checkbox" resolves that collision.
         The facts are in the hover tooltip there instead. This is the
         one place #580's "persistent at rest" property is traded away;
         if the owner wants it back in masonry, the tile floor has to
         grow.

         NOT IN THUMBNAIL (#1136): the same badge draws in the top
         chrome band instead, which is where the reference panel puts
         its type indicator and which leaves the artwork untouched. -->
    {#if !compact && !showOverlay && !coverRestricted && !detailed}
      <CardKindBadge
        kind={coverKind}
        count={memberCount}
        class="absolute bottom-2 right-2 z-[2]" tooltipKey={post.id} />
    {/if}

    {#if showOverlay}
      <!-- ═══ #1111: the grid overlay ═══════════════════════════════
           Two corner blocks and a BOTTOM-ONLY scrim, revealed together.

           NO GRADIENT ACROSS THE ARTWORK (owner correction, 2026-08-15).
           This shipped for one screenshot as a single full-card gradient
           — `from-black/85 via-transparent to-black/45` over `inset-0` —
           and on a flat saturated field like a flag it STEPPED visibly:
           an 8-bit alpha ramp stretched over 378px is roughly one level
           every two rows, and flat colour is exactly where that reads as
           horizontal scanlines. It looked like a filter and was a
           gradient over too long a run.

           The tint itself is fine — the owner's follow-up allows a
           subtle uniform darkening, and it earns its place by making the
           corner icon read against a light cover. What it must be is
           FLAT. A single-alpha fill has one value and therefore cannot
           band, at any card size, over any artwork; the moment it
           becomes a ramp the length of the card, it can. That is the
           rule this file is keeping, not a particular opacity.

           So: one flat wash over the whole card, plus a short gradient
           scrim confined to the bottom band where the type actually
           sits. The scrim is a ramp too, but over ~45% of the height and
           ending in a region already going black, which is a short
           enough run for its steps to fall below the eye's threshold —
           the same treatment the featured rail and the old grid overlay
           have always used without complaint.

           HOVER **AND** FOCUS. Hover alone leaves a keyboard reader
           tabbing through a wall of unlabelled pictures — at rest this
           card shows NOTHING but the image, so without the focus arm
           the keyboard path has no title at all. The card's stretched
           `<a>` is the focus target and lives inside this same `.group`,
           so the rule is "the group contains a focus-visible element".

           WHY THE RULE IS IN <style> AND NOT IN THE CLASS LIST.
           `group-focus-within:` is the obvious spelling and is the
           WRONG one: `:focus-within` fires for a MOUSE click too, so
           clicking a card would pin its overlay open after the pointer
           left — the #1020 regression, exactly. `:focus-visible` is the
           keyboard-only half, and the ancestor form of it
           (`:has(:focus-visible)`) has no Tailwind variant that is
           guaranteed to compile; a variant that silently fails to
           compile leaves an overlay a keyboard reader can never see,
           which no type check and no DOM assertion would catch. So the
           two selectors are written out, where they cannot half-apply.

           `pointer-events-none` on the overlay, `pointer-events-auto`
           re-enabled only on the ⋯ trigger (CardMenu does that itself)
           and, since #1126, on the AUTHOR LINK.

           THE AUTHOR IS NOW A LINK, reversing #1111's call (owner
           direction, 2026-08-15). The old note read: "the card is one
           stretched anchor to the post, and nesting a profile link
           inside it would put two targets in one tile". The nesting
           half was right and is still obeyed — this anchor is a SIBLING
           of the card's stretched link, not a descendant of it, exactly
           as the ⋯ menu already was. Nested anchors are invalid HTML and
           unreachable by keyboard; two SIBLING anchors in one tile are
           the ordinary card pattern the feed card has always used.

           Stacking is what makes a sibling work: the card's stretched
           `<a>` is `absolute inset-0` at z-[1], this overlay is z-[2],
           and the identity block inside it is z-[1] of that stacking
           context — so the author link sits ON TOP of the card link and
           takes the click in its own box, while every other pixel of
           the tile still falls through to "open this post". -->
      <div
        class="grid-overlay pointer-events-none absolute inset-0 z-[2] flex flex-col justify-between
               bg-black/20 p-2.5 opacity-0 transition-opacity duration-200"
        data-testid="post-card-overlay"
      >
        <!-- The bottom scrim. `-m-2.5` cancels the container's padding:
             a scrim that stops 10px short of the edges is a dark
             rectangle sitting on the picture instead of the bottom of
             the picture going dark. -->
        <div
          class="pointer-events-none absolute inset-x-0 bottom-0 -m-2.5 h-[45%]
                 bg-gradient-to-t from-black/80 via-black/40 to-transparent"
          aria-hidden="true"
          data-testid="post-card-scrim"
        ></div>
        <!-- TOP-LEFT: the kind, as an icon and never as a word.
             A multi-asset post states the SET instead of any one
             member's kind, with the count to the LEFT of the glyph —
             #1111's spelling, and the right way round: the number is
             read first and the glyph qualifies it.

             `memberCount` is the truth rule from the props block: a
             search hit ships one member and carries its real size
             beside it, so this never becomes `members.length` and never
             becomes an unbounded query for a badge. -->
        <div class="relative flex items-start justify-between gap-2">
          <CardKindBadge kind={coverKind} count={memberCount} tooltipKey={post.id} />
        </div>

        <!-- BOTTOM-LEFT: identity. Title, then the author.
             `relative` + `pr-11` are load-bearing exactly as they are on
             the feed header: CardMenu anchors `absolute right-2 top-2`
             to the nearest positioned ancestor and reserves no flex
             space, so without both the ⋯ escapes to the card and a long
             display name runs underneath it. -->
        <!-- `min-h-11` is the ⋯ menu's own tap target, and it is
             load-bearing rather than cosmetic (#1139). CardMenu is
             `absolute right-2 top-2` inside this block and reserves NO
             flex space, so on the compressed tier — where the block is
             one 20px title line — a 44px control would hang 32px below
             the picture. That is the same clipping bug this issue is
             about, one element smaller. On the full tier the block is
             67px and this changes nothing. -->
        <div
          class="relative z-[1] flex min-h-11 items-end pr-11"
          data-testid="post-card-identity"
          data-overlay-tier={overlayTier}
        >
          <div class="min-w-0 flex-1">
            <!-- No `title` attribute: the styled tooltip replaces the
                 native one for this element (#1126), and keeping both
                 would show two tooltips a second apart saying the same
                 thing. The full string is still on the card link's
                 `aria-label`, which is where a screen reader reads it.
                 `bind:this` is what lets the hover handler ask whether
                 this element is actually clipped. -->
            <p bind:this={titleEl} class="truncate text-sm font-semibold text-white">
              {post.title || 'Untitled'}
            </p>
            {#if author && overlayTier !== 'compressed'}
              <!-- DROPPED ON THE COMPRESSED TIER (#1139), which is the
                   whole point of that tier: a wide, short tile has room
                   for a caption and not for a 40px avatar under it. The
                   choice is not "identity or nothing" — it is "a title
                   that reads, or an identity row cut in half by the
                   bottom of the picture". The artist is still one hover
                   away in the tooltip, exactly as on the minimal tier.

                   The identity block now lives in CardAuthorLink (#1047)
                   so grid, thumbnail and AssetCard draw ONE artist block
                   rather than three that drift. Everything that made it
                   work here is carried there: the `w-fit` that stops the
                   anchor swallowing clicks meant for the post, the
                   `pointer-events-auto` that revives it inside a
                   `pointer-events-none` overlay, the initials fallback
                   (the common case on a fresh install), and printing the
                   server's `display_name` verbatim (#1023). -->
              <div class="mt-1">
                <CardAuthorLink {author} variant="overlay" />
              </div>
            {/if}
          </div>
          <!-- ⋯ right of the identity block. NOT a second menu: the
               top-right instance below is skipped in this mode, so the
               card still has exactly one. -->
          <CardMenu
            postId={post.id}
            detailPath="/posts/{post.id}"
            manageAccess={isAuthor ? { kind: 'post', id: post.id } : null}
            revealed
          />
        </div>
      </div>
    {:else if !detailed && !compact && !social}
      <!-- List: hover-only title overlay (clicks fall to the link).
           Grid takes the #1111 overlay above; a grid tile whose cover is
           RESTRICTED also lands here, because that plate states its own
           restriction and must not be captioned by kind as well.
           Thumbnail shows a persistent footer below instead; masonry
           shows the hover tooltip instead (#652); feed prints the title
           as a PERSISTENT caption under the media (#557) — a social card
           is read, not scanned, so hiding the title until hover would
           make the one mode with room for it the one that withholds it. -->
      <div
        class="pointer-events-none absolute inset-x-0 bottom-0 z-[2] bg-gradient-to-t from-black/85 via-black/50 to-transparent
               p-3 opacity-0 group-hover:opacity-100 transition-opacity duration-200"
      >
        <p class="text-sm font-medium text-white line-clamp-2">{post.title || 'Untitled'}</p>
        <p class="text-xs text-white/70 mt-0.5">
          {createdShort}{post.like_count > 0 ? ` · ♥ ${post.like_count}` : ''}
        </p>
      </div>
    {/if}

    {#if !social && !showOverlay && !detailed}
      <!-- Overflow menu. ONE affordance in every mode, including
           thumbnail — owner amendment 2026-07-25 to #556. Thumbnail now
           renders it INLINE in the bottom band (#1136) rather than over
           the artwork; it is still exactly one per card. Skipped under
           the #1111 grid overlay, which renders the same component
           beside the identity block instead — one ⋯ per card, always.
           add-to-collection targets the cover asset.

           Feed renders the SAME component in its header instead, so the
           card still has exactly one ⋯ and it sits where a social card's
           overflow belongs: beside the author, not over the artwork. -->
      <CardMenu
        postId={post.id}
        detailPath="/posts/{post.id}"
        manageAccess={isAuthor ? { kind: 'post', id: post.id } : null}
      />
    {/if}
  </CardThumb>

  {#if social}
    <!-- Feed ACTION BAR + caption (#557).
         Like / comment / share, then the post's own words. Counts come
         off the payload; `liked` does too, so the heart is right on
         first paint instead of after twenty round trips. -->
    <div class="px-3 pb-3 pt-2.5">
      <div class="flex items-center gap-1">
        <button
          type="button"
          onclick={toggleLike}
          disabled={!canLike || likeBusy}
          aria-pressed={liked}
          data-testid="feed-like"
          class="inline-flex h-9 items-center gap-1.5 rounded-md px-2 text-sm transition-colors
                 hover:bg-surface disabled:cursor-default disabled:hover:bg-transparent
                 {liked ? 'text-danger' : 'text-fg-muted'}"
          title={liked ? t('card.feed.unlike') : t('card.feed.like')}
          aria-label={t('card.feed.likes', { count: String(likeCount) })}
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill={liked ? 'currentColor' : 'none'} stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z" />
          </svg>
          <span class="tabular-nums">{likeCount}</span>
        </button>

        <button
          type="button"
          onclick={openComments}
          data-testid="feed-comment"
          class="inline-flex h-9 items-center gap-1.5 rounded-md px-2 text-sm text-fg-muted transition-colors hover:bg-surface hover:text-fg"
          title={t('card.feed.comment')}
          aria-label={t('card.feed.comments', { count: String(post.comment_count) })}
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
          </svg>
          <span class="tabular-nums">{post.comment_count}</span>
        </button>

        <button
          type="button"
          onclick={sharePost}
          data-testid="feed-share"
          class="ml-auto inline-flex h-9 items-center gap-1.5 rounded-md px-2 text-sm transition-colors hover:bg-surface
                 {shared ? 'text-accent' : 'text-fg-muted hover:text-fg'}"
          title={shared ? t('card.feed.shared') : t('card.feed.share')}
          aria-label={shared ? t('card.feed.shared') : t('card.feed.share')}
        >
          {#if shared}
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="20 6 9 17 4 12" /></svg>
          {:else}
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <circle cx="18" cy="5" r="3" /><circle cx="6" cy="12" r="3" /><circle cx="18" cy="19" r="3" />
              <line x1="8.59" y1="13.51" x2="15.42" y2="17.49" /><line x1="15.41" y1="6.51" x2="8.59" y2="10.49" />
            </svg>
          {/if}
        </button>
      </div>

      <!-- Caption. Title always; description when there is one, clamped
           so a long one cannot make the card unscrollable. -->
      <a
        href="/posts/{post.id}"
        onclick={handleClick}
        class="mt-1 block px-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
      >
        <p class="text-sm font-medium text-fg">{post.title || 'Untitled'}</p>
        {#if post.description}
          <p class="mt-0.5 line-clamp-3 whitespace-pre-wrap text-sm text-fg-muted">{post.description}</p>
        {/if}
      </a>

      {#if commentsPreview.length > 0}
        <!-- ── The comments snippet (#1047) ──────────────────────────
             The HEAD OF THE THREAD, not a summary: the same top-level
             rows in the same order `GET /posts/{id}/comments` returns,
             so what is under the picture is literally the top of what
             opening the post shows.

             Two, and never a scrollable list: a feed card's comments
             are a signal that a conversation exists, and the
             conversation itself is a click away. `TopCommentsPerPost`
             on the server is what decides; this renders what arrived.

             NO NAME IS NOT A MISSING NAME. `author` is absent when the
             commenter is not disclosed to this reader (ADR 0024) —
             the same absence the post's own header answers by drawing
             no author — so the words render on their own rather than
             beside a placeholder identity. A remote comment carries a
             cached peer name instead and takes the same shape. -->
        <ul class="mt-1.5 space-y-0.5 px-2" data-testid="feed-comments">
          {#each commentsPreview as c (c.id)}
            <li class="line-clamp-2 text-sm text-fg-muted">
              {#if commenterName(c)}
                <span class="font-medium text-fg" data-testid="feed-comment-author"
                  >{commenterName(c)}</span
                >
                <span> </span>
              {/if}<span data-testid="feed-comment-body">{c.body}</span>
            </li>
          {/each}
        </ul>
        {#if post.comment_count > commentsPreview.length}
          <!-- The link the count has always been able to draw, now that
               there is something above it to be "all" of. Same
               navigation the card itself performs — the modal, with the
               thread in it. -->
          <a
            href="/posts/{post.id}"
            onclick={handleClick}
            class="mt-0.5 block px-2 text-xs text-fg-subtle hover:text-fg focus-visible:outline-none
                   focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
            data-testid="feed-comments-all"
          >
            {t('card.feed.view_all_comments', { count: String(post.comment_count) })}
          </a>
        {/if}
      {/if}
    </div>
  {/if}

  {#if detailed}
    <!-- Details FOOTER — thumbnail's PERSISTENT chrome (#1047).
         "Information at a glance, preview still clear" (owner's density
         table): grid's #1111 vocabulary, drawn AROUND the image instead
         of over it, and never hover-gated. The kind icon is already
         persistent bottom-right on the thumb above; this row carries the
         artist and the engagement facts.

         The ARTIST is the addition, and it is the same component grid's
         overlay uses. A thumbnail is where someone works through a shelf
         of other people's work, so "whose is this" belongs at rest here
         even though grid hides it until hover.

         The title stays in the header, where #556 put it. -->
    <div class="space-y-1 px-3 py-2" data-testid="thumb-metadata">
      <!-- ROW 1 — the title. Moved down from #556's header (see the top
           band's note): one fact per row, in the reference panel's own
           order, reads as a record rather than as a caption strip. -->
      <a href="/posts/{post.id}" onclick={handleClick} class="block focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring">
        <p class="truncate text-sm font-medium text-fg" title={post.title || 'Untitled'}>
          {post.title || 'Untitled'}
        </p>
      </a>
      {#if author}
        <!-- ROW 2 — the artist. SIBLING link, not nested in the card's
             anchor (#1126). -->
        <CardAuthorLink {author} size="sm" />
      {/if}
      <!-- ROW 3 — the date, the engagement facts, and the ⋯ MENU. One
           row rather than one per fact: they are a single "how has this
           been received" reading, and four rows of one number each
           would push the preview off a 200px card.

           THE MENU IS A SIBLING OF THE LINK, NOT A CHILD OF IT. The
           owner's ruling puts the ⋯ at this row's end, and the obvious
           way to write that — dropping the button inside the anchor
           that already spans the row — is invalid HTML (interactive
           content inside an <a>) and would fire the card's navigation
           on the way to opening the menu, since the anchor's click
           handler sits between the button and the document. So the row
           becomes a flex box holding two siblings: the anchor, which
           takes the elastic space and stays the whole row's click
           target, and the trigger at its end. Same rule the artist row
           follows for the same reason (#1126).

           The anchor keeps `min-w-0` because its `<p>` truncates the
           origin peer name; without it the flex item refuses to shrink
           below its content and the menu is pushed off the card. -->
      <div class="flex items-center gap-2">
        <a href="/posts/{post.id}" onclick={handleClick} class="block min-w-0 flex-1 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring">
          <p class="flex items-center gap-2 text-xs text-fg-muted">
            <span>{createdShort}</span>
            {#if post.like_count > 0}
              <span class="inline-flex items-center gap-1" title={t('card.footer.likes', { count: String(post.like_count) })}>
                <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M12 21s-6.7-4.35-9.33-8.24C.9 10.06 1.6 6.5 4.6 5.4c2-.73 3.9.2 4.9 1.7l.5.75.5-.75c1-1.5 2.9-2.43 4.9-1.7 3 1.1 3.7 4.66 1.93 7.36C18.7 16.65 12 21 12 21z"/></svg>
                {post.like_count}
              </span>
            {/if}
            {#if post.comment_count > 0}
              <span class="inline-flex items-center gap-1" title={t('card.footer.comments', { count: String(post.comment_count) })}>
                <svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>
                {post.comment_count}
              </span>
            {/if}
            {#if origin}
              <!-- Provenance rides EVERY density (#552) — see the feed
                   header and AssetCard's footer for the same line. -->
              <span class="inline-flex items-center gap-1 truncate" data-testid="card-origin" title={t('card.origin_label')}>
                <span aria-hidden="true">↗</span>
                <span class="truncate">{t('card.origin_from', { peer: origin.display_name })}</span>
              </span>
            {/if}
          </p>
        </a>
        <CardMenu
          postId={post.id}
          detailPath="/posts/{post.id}"
          manageAccess={isAuthor ? { kind: 'post', id: post.id } : null}
          placement="inline"
        />
      </div>
    </div>

  {/if}
</div>

<style>
  /* The #1111 grid overlay's reveal rule. See the markup comment for why
     it is here and not in the class list.

     `:global` on the ancestor because `.group` is the card wrapper's
     class in this same file but Tailwind-authored — Svelte's scoper does
     not stamp it, so the compiler would prune the rule as unmatched.
     The `.grid-overlay` half stays scoped, so this cannot reach any
     other component's overlay. */
  :global(.group:hover) .grid-overlay,
  :global(.group:has(:focus-visible)) .grid-overlay {
    opacity: 1;
  }

  /* Touch has neither of those. A coarse-pointer device cannot hover and
     is not tabbing, so both selectors above are dead there — and this
     overlay now CONTAINS the ⋯ menu, which used to carry its own
     `[@media(hover:none)]:opacity-100` and was therefore always
     reachable. Without this arm, moving the menu inside the overlay
     would have taken every grid card's only action affordance away from
     every phone, silently.

     So on touch the overlay is PERSISTENT rather than revealed. That is
     the same trade #578 made for the same reason, and it is the honest
     one: "revealed on hover" is not a design a device without a pointer
     can express. */
  @media (hover: none) {
    .grid-overlay {
      opacity: 1;
    }
  }
</style>
