<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { untrack, onMount, tick } from 'svelte';
  import { site } from '$stores/site.svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import FeaturedRail from '$components/FeaturedRail.svelte';
  import PromoBand from '$components/PromoBand.svelte';
  import BrowseRail from '$components/BrowseRail.svelte';
  import TeamFollowButton from '$components/TeamFollowButton.svelte';
  import { browseRail } from '$stores/browseRail.svelte';
  import { tagFollows } from '$stores/tagFollows.svelte';
  import PostCard from '$components/PostCard.svelte';
  import type { CardCoverAsset } from '$components/cardAsset';
  import PostParamHost from '$components/PostParamHost.svelte';
  import BrowseFooter from '$components/BrowseFooter.svelte';
  import PostListTable from '$components/PostListTable.svelte';
  import ContentGrid from '$components/ContentGrid.svelte';
  import SelectionBar from '$components/SelectionBar.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import { t } from '$stores/lang.svelte';
  import { createScrollSnapshot } from '$lib/util/scrollSnapshot';
  import { createMarquee } from '$lib/util/marquee.svelte';
  import { createInfiniteScroll } from '$lib/util/infiniteScroll.svelte';
  import { resetResultsScroll } from '$lib/util/resultsScroll';
  import type { components } from '$api/schema';

  onMount(() => { browseView.init(); });

  // Browse page — feed of Posts (per Phase 1.13.D-2's model change).
  // Each Post wraps 1+ assets; the card renders the cover. Grid mode
  // (default); 1.13.E adds Masonry / Thumbnail / List + a switcher.
  //
  // Search is sourced from the URL's `?q=` so refreshes and shared
  // links reproduce the same result set. The search input itself
  // lives in the global navbar (see +layout.svelte) and goto()s
  // here with the updated query string. Server-side the match runs
  // against the TSVECTOR `search_text` column on posts (built from
  // title + description + tags + member-asset search_text by the
  // 00014 migration trigger).

  // Member assets use the shared card feed contract (#595). The local
  // type this replaces declared only `id`, `file_hash?` and
  // `preview_available?` — it never mentioned file_extension or
  // thumbhash at all, and browse rendered its video / 3D badges and
  // sprite-scrub previews purely because the runtime objects carried
  // fields the type had no opinion about. That is the same silence that
  // let the collection page drop them for real.
  interface PostMember {
    asset_id: string;
    sort_order: number;
    asset: CardCoverAsset;
  }
  interface Post {
    id: string;
    author_user_ref: number;
    title: string;
    description: string;
    visibility: 'private' | 'followers' | 'public';
    cover_asset_id?: string | null;
    posted_at: string;
    like_count: number;
    comment_count: number;
    tags: string[];
    members: PostMember[];
    created_at: string;
    updated_at: string;
  }

  const PAGE = 36;

  const query = $derived(page.url.searchParams.get('q') ?? '');

  /** The team the feed is filtered to (#1113), or null for the
   *  unfiltered default.
   *
   *  The URL is the ONE owner of this state — not the rail, and not a
   *  local `let`. It is what makes the filter survive a reload, travel
   *  in a shared link, and answer the back button, and it is the same
   *  shape `?q=` and `?post=` already use on this route.
   *
   *  Deliberately NOT validated against the rail's team list here. A
   *  `?team=` naming something this reader cannot see is answered by
   *  the server — `GET /posts` composes the team filter with the
   *  visibility predicate, so the feed comes back empty rather than
   *  leaking the team's existence. Second-guessing it client-side would
   *  be a copy of a rule that already has one home. */
  const activeTeamId = $derived(page.url.searchParams.get('team'));

  /** The active team's row, for the heading and its follow button. Null
   *  while the rail is still loading, or when `?team=` names something
   *  outside this reader's visible set — the heading falls back to the
   *  unfiltered title rather than printing a raw uuid. */
  const activeTeam = $derived(
    activeTeamId ? (browseRail.teams.find((c) => c.id === activeTeamId) ?? null) : null,
  );

  /** The tag the feed is filtered to (#1123), or null.
   *
   *  Same URL ownership as `?team=`, and the same deliberate absence of
   *  client-side validation: a `?tag=` naming something nobody has used
   *  comes back as an empty feed from the server, which is the honest
   *  answer and costs no round trip to decide here. Unlike `?team=` it
   *  cannot leak anything either — a tag is a string, not a row whose
   *  existence is worth hiding. */
  const activeTag = $derived(page.url.searchParams.get('tag'));

  /** Apply or clear the team filter. `noScroll` + `keepFocus` because
   *  this is a filter, not a navigation: the reader stays where they
   *  are and the feed changes underneath the rail they just clicked.
   *
   *  Picking a team CLEARS any tag filter, and `selectTag` clears the
   *  team. The rail is a single-select strip (see its component note),
   *  and this page owning both params is what makes that true — a rail
   *  that only set its own would leave the other pressed chip live in
   *  the URL with nothing on screen saying so. */
  async function selectTeam(id: string | null) {
    const target = new URL(page.url);
    target.searchParams.delete('tag');
    if (id) target.searchParams.set('team', id);
    else target.searchParams.delete('team');
    await goto(target.pathname + target.search, { keepFocus: true, noScroll: true });
  }

  /** Apply or clear the tag filter. Mirror of `selectTeam`. */
  async function selectTag(tag: string | null) {
    const target = new URL(page.url);
    target.searchParams.delete('team');
    if (tag) target.searchParams.set('tag', tag);
    else target.searchParams.delete('tag');
    await goto(target.pathname + target.search, { keepFocus: true, noScroll: true });
  }

  /** The footer's asset-type filter (#1166), read straight off the URL
   *  like `?team=` and `?tag=` above it — one owner for every narrowing
   *  control on this page, so a filtered wall is shareable and the back
   *  button walks the filters.
   *
   *  The wire form is what the server parses (comma-joined) and what the
   *  URL shows, so there is no second representation to keep in step.
   *  An empty string means no filter, which is what "all types" is.
   *
   *  Unlike `?team=` this one is INDEPENDENT of the rail: a reader can
   *  be looking at one studio's videos. The rail's two chips clear each
   *  other because the rail is single-select; the type filter is a
   *  different axis and clears nothing. */
  const activeKinds = $derived(page.url.searchParams.get('kind') ?? '');
  const activeKindList = $derived(activeKinds === '' ? [] : activeKinds.split(','));

  /** Commit a type selection. In place, like the rail's chips — this is
   *  a filter, not a navigation. An empty list drops the parameter
   *  rather than writing `kind=`, so "all types" leaves no trace in a
   *  URL somebody is about to share. */
  async function selectKinds(next: string[]) {
    const target = new URL(page.url);
    if (next.length > 0) target.searchParams.set('kind', next.join(','));
    else target.searchParams.delete('kind');
    await goto(target.pathname + target.search, { keepFocus: true, noScroll: true });
  }

  // #891 shipped a one-line note here — "items you don't have access to
  // are hidden by your preferences", with a link to change it — because
  // hiding was an opt-in and an opted-in reader's grid was shorter than
  // the one everyone else saw. #921 made hiding the DEFAULT, and the
  // note stopped being true on both halves: it is not the reader's
  // preference (they set nothing), and the feed is not shorter than
  // anyone else's (it is the feed). A line that fires for every reader
  // on every browse paint is chrome, not an explanation, so it is gone.
  //
  // If a "there is more here you cannot open" affordance is wanted, it
  // is a fresh design question rather than this note inverted — the
  // mirror ("you are seeing work you can't open") would fire only for
  // the few who opted in, who already know.

  let items = $state<Post[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(false);
  let initialLoaded = $state(false);
  let error = $state<string | null>(null);
  // Signed-out visitor on the members-only feed — an expected state,
  // deliberately kept distinct from `error`.
  let guestFeed = $state(false);
  let sentinel: HTMLElement | undefined = $state();

  let generation = 0;

  async function fetchPage(
    q: string,
    team: string | null,
    tag: string | null,
    kinds: string,
    cursor: string | null,
    reset: boolean,
  ) {
    loading = true;
    error = null;
    guestFeed = false;
    const gen = ++generation;
    let appended = 0;
    try {
      const params: Record<string, string | number | string[]> = { limit: PAGE };
      if (q.trim() !== '') params.q = q.trim();
      // #1113 — the same parameter the team page has always sent
      // (routes/teams/[id]/+page.svelte). The filter is the server's,
      // so it composes with the feed pill, the sort direction and the
      // visibility predicate without this page arranging anything.
      if (team) params.team_id = team;
      // #1123 — `?tag=` has been a /posts parameter since the tag filter
      // shipped; the rail chip just gives it a control. It intersects
      // with `q` and the feed pill server-side for the same reason
      // `team_id` does: they are all parameters of one query.
      //
      // Sent as a ONE-ELEMENT ARRAY since #1251 slice 2 made the
      // parameter repeatable (`?tag=a&tag=b`, meaning AND). The rail is
      // single-select and this page's `?tag=` is still one value, so the
      // request on the wire is byte-identical to what it always was;
      // what changed is that the server can now be asked for more.
      if (tag) params.tag = [tag];
      // #1166 — the footer's type filter. Comma-joined, straight from
      // the URL, and a plain parameter of the same query for the same
      // reason `team_id` and `tag` are: composition is the server's
      // job, so a studio's videos is one request and not an
      // intersection this page computes.
      if (kinds) params.kind = kinds;
      if (!reset && cursor) params.cursor = cursor;
      // Feed filter + direction from the BrowseFooter store.
      //
      // `filter` is now a straight pass-through to the server's typed
      // `feed` enum, because since #691 `FeedFilter` and that enum hold
      // the same two values. This used to be a MAPPING — every pill
      // went out as an undeclared `filter=` param for "observability"
      // and only `following` was translated into `feed=`, so the
      // client-only `team` and `trending` pills produced a request the
      // server read as plain `latest`. There is nothing left to map, so
      // there is nothing left to silently swallow: a pill the server
      // can't serve now fails to typecheck here.
      params.feed = browseView.filter;
      params.dir = browseView.feedDir;
      // #1251 slice 3 — the footer's "Hide AI-made work" toggle. ON
      // sends `ai=not_pure`; OFF sends NOTHING, which is what
      // `aiParam` returning null means. The mapping lives on the store
      // beside the flag, not here, so the one place that knows what the
      // toggle means is the one that owns it.
      //
      // ⚠️ ONLY the purely-AI posts go. A post mixing AI and human
      // contributors stays on the wall — the server decides that on
      // `posts.ai_pure`, and this page must never try to reproduce the
      // rule locally, which would be the second query language ADR 0093
      // exists to refuse.
      //
      // A plain parameter of the same query as `team_id`, `tag` and
      // `kind`, for the same reason: composition is the server's job,
      // so "this studio's videos, minus the AI ones" is one request.
      const ai = browseView.aiParam;
      if (ai) params.ai = ai;
      // #1292: the CONTENT category's Mature row, ADR 0090's layer 3.
      // UNTICKED sends `mature=not_mature`; ticked sends NOTHING, which
      // is what `matureParam` returning null means. Resolved on the
      // store for the same reason `ai` is, and with a stronger one:
      // that getter also holds the availability cascade, so a device
      // carrying the flag from a session that offered the row cannot
      // keep filtering on one that does not.
      //
      // ⛔ IT NARROWS AND NEVER CONSENTS. The three conjuncts still
      // decide whether this reader may be shown mature rows at all;
      // this can only subtract from what survives them, and there is no
      // value of it that asks for more.
      const mature = browseView.matureParam;
      if (mature) params.mature = mature;

      const { data, error: apiErr } = await api.GET('/posts', {
        params: { query: params as never },
      });

      if (gen !== generation) return;

      if (apiErr || !data) {
        // A signed-out visitor can still get 401 here, and it is
        // EXPECTED (#416) — but since #1181 it means exactly ONE thing:
        // this install has public mode OFF. The feed is a public-mode
        // surface now, so with the toggle ON a guest gets a 200 and the
        // public tier, and only a members-only install refuses them.
        //
        // Rendering "authentication required" in a red alert told a
        // guest something had broken. Nothing had; they are looking at
        // a members-only install, so it gets an empty state, not an
        // error. The OTHER anonymous empty state — 200 with nothing
        // public to show — is `guestEmpty` further down.
        if (!auth.user) {
          guestFeed = true;
          return;
        }
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? t('common.failed_to_load'),
        );
      }

      const pageItems = (data.items ?? []) as Post[];
      items = reset ? pageItems : [...items, ...pageItems];
      nextCursor = (data.next_cursor as string | null) ?? null;
      appended = pageItems.length;
    } catch (e) {
      error = e instanceof Error ? e.message : t('common.failed_to_load');
    } finally {
      if (gen === generation) {
        loading = false;
        initialLoaded = true;
      }
    }
    // #1159 — top the buffer back up. See `pumpFeed` for why the
    // IntersectionObserver alone cannot do this once the lookahead is
    // deeper than one page is tall.
    //
    // `appended > 0` is the anti-runaway guard and it is load-bearing:
    // a feed that answers with an empty page and a non-null cursor
    // (a filter that thins a page to nothing server-side) would
    // otherwise leave the buffer permanently short and pump forever.
    // One empty page stops the chase; the observer still re-arms it
    // when the reader scrolls.
    if (gen !== generation || appended === 0) return;
    await tick();
    requestAnimationFrame(pumpFeed);
  }

  // Identity of the result set currently on screen. Reading all three
  // inputs here is what subscribes the effect below to them.
  //
  // The separator is U+001F (INFORMATION SEPARATOR ONE), written as an
  // escape rather than as a literal byte. It only has to be something a
  // user cannot type into the query box, so that ("a","b") and ("ab","")
  // cannot collide. It used to be a raw NUL, which did that job but also
  // made git, grep and ripgrep classify this whole file as binary —
  // searches for anything in it silently returned nothing, and every
  // diff rendered as "Binary file not shown" (#925). Do not put a raw
  // control byte back in here.
  //
  // ⚠️ EVERY INPUT `fetchPage` READS MUST BE IN HERE. The AI toggle
  // (#1251 slice 3) was the first that is neither in the URL nor
  // already keyed for some other reason, and #1292's mature row is the
  // second; both are in the key for the same argument: flipping it changes
  // which posts the request returns, so a key that ignored it would
  // leave the previous wall on screen — a control that visibly does
  // nothing, which is #691's defect in a different costume. It also has
  // to be here for the SNAPSHOT to stay honest, since `capture` and
  // `restore` compare this exact string: pages captured with the toggle
  // OFF must not be handed back to a page loaded with it ON, or the
  // hidden posts come straight back on a back-navigation.
  const feedKey = () =>
    `${query}\u001f${browseView.filter}\u001f${browseView.feedDir}\u001f${activeTeamId ?? ''}` +
    `\u001f${activeTag ?? ''}\u001f${activeKinds}\u001f${browseView.aiParam ?? ''}` +
    `\u001f${browseView.matureParam ?? ''}`;

  /** The feedKey whose first page we've already loaded (or restored).
   *  Guards the effect against re-fetching a set we already hold —
   *  which is what makes back-navigation restoration work regardless
   *  of whether `snapshot.restore` lands before or after mount. */
  let loadedKey: string | null = null;

  // Reset and refetch every time the query, feed filter, or feed
  // direction changes.
  //
  // ⭐ AND PUT THE READER AT THE FIRST ROW (#1298, ADR 0056 §3c's
  // 2026-08-28 amendment). Refining is a NEW address, so the offset
  // measured against the old wall describes posts that are not coming
  // back.
  //
  // ⚠️ THIS WALL LOOKED CORRECT WITHOUT IT, AND THAT IS THE ARGUMENT
  // FOR ADDING IT RATHER THAN AGAINST. Measured on a 900-card wall at
  // 29457px refined by kind: the offset went to 0, at 1080p and at
  // 390px. But nothing here decided that. `items = []` collapses the
  // wall to zero height in the same frame, `<main>` briefly has nothing
  // to scroll, and the BROWSER clamps — so the landing is correct only
  // while the chrome above the wall stays shorter than the viewport,
  // which is a coincidence of the featured rail's height and one this
  // route must not go on depending on. `/search` is the same shape with
  // the coincidence absent: it swaps its hits in place, and the same
  // refine lands the reader at the bottom of a 25-hit list.
  //
  // Ordered before the fetch, not after it: at offset 0 Chrome's scroll
  // anchoring has nothing to compensate, so the reset survives the
  // content growing back underneath it without being re-asserted.
  $effect(() => {
    const key = feedKey();
    untrack(() => {
      if (key === loadedKey) return;
      const refine = loadedKey !== null;
      loadedKey = key;
      items = [];
      nextCursor = null;
      initialLoaded = false;
      // Only on a REFINE. The first key of a page load is not one, and
      // a route that scrolled itself on mount would fight the snapshot
      // restore that back-navigation is about to run.
      if (refine) resetResultsScroll(wallEl);
      void fetchPage(query, activeTeamId, activeTag, activeKinds, null, true);
    });
  });

  // Scroll + loaded-pages restoration on back-navigation (#584).
  //
  // The feed is the one surface where restoring the offset alone would
  // be actively worse than not restoring it: come back holding only
  // page 1 and a 1500px offset sits inside the sentinel's lookahead
  // (#1159 made that deeper still, so the argument only got stronger),
  // so the loader fires, the content grows, and the user is
  // parked somewhere they never were. Handing back the accumulated
  // pages puts the offset back over the same posts and leaves the
  // sentinel where it belongs — off screen.
  //
  // `restore` has no ordering guarantee against the mount effect above,
  // so it covers both: setting `loadedKey` stops a fetch that hasn't
  // started, and bumping `generation` cancels one already in flight
  // (fetchPage's own guard then drops the response — including its
  // `loading = false`, hence the explicit reset here, or the infinite
  // scroll would stay wedged).
  interface FeedSnapshot {
    key: string;
    items: Post[];
    cursor: string | null;
  }
  export const snapshot = createScrollSnapshot<FeedSnapshot>({
    capture: () => ({ key: feedKey(), items, cursor: nextCursor }),
    restore: (saved) => {
      if (!saved || saved.key !== feedKey() || saved.items.length === 0) return;
      generation++;
      items = saved.items;
      nextCursor = saved.cursor;
      loadedKey = saved.key;
      initialLoaded = true;
      loading = false;
    },
  });

  // ── Infinite scroll: stay ahead of the reader (#1159) ─────────────
  //
  // The rig moved to `$lib/util/infiniteScroll.svelte` in #1354, so
  // /search inherits it CORRECT rather than growing a second observer
  // with the default root. Everything this route used to spell inline —
  // the scrollport-rooted observer, the geometry predicate the observer
  // alone cannot replace, and the resize re-arm — is there, with the
  // measurements that produced it.
  const feedScroll = createInfiniteScroll({
    sentinel: () => sentinel,
    more: () => nextCursor !== null,
    busy: () => loading,
    load: () => void fetchPage(query, activeTeamId, activeTag, activeKinds, nextCursor, false),
  });
  const pumpFeed = () => feedScroll.pump();

  // ── Marquee drag-select (#1127) ───────────────────────────────────
  //
  // Attached to the WALL, not to <main>: the band should not start from
  // the rail, the featured strip or the page gutters, all of which are
  // chrome with their own gestures. `orderedIds` is the loaded feed in
  // feed order — the same array the grid renders from — which is what
  // makes a range "everything between these two posts" rather than
  // "everything between these two positions in some column".
  //
  // LOADED POSTS ONLY, and that falls out of using `items` rather than
  // asking the server: a range can only ever name rows the reader can
  // see, so there is no phantom selection of an unfetched page.
  let wallEl = $state<HTMLElement | null>(null);
  const orderedIds = () => items.map((p) => p.id);
  const marquee = createMarquee(() => wallEl, { ordered: orderedIds });

  const hasMore = $derived(nextCursor !== null);
  // guestFeed has its own empty state below; without this the generic
  // "nothing here yet" block would render underneath it.
  const showEmpty = $derived(initialLoaded && items.length === 0 && !error && !guestFeed);

  // #1176 — the OTHER anonymous empty state, and the one nothing
  // handled. `guestFeed` above covers the 401: public mode off, the
  // feed refuses a signed-out caller, and the honest answer is "the
  // feed is for members". With public mode ON the same visitor gets a
  // 200 and a page of whatever is `public` — which, until this sprint,
  // was nothing at all, because no seeded post and no compose-form post
  // could reach that tier. The wall then fell through to the generic
  // "No posts yet — once posts are uploaded they'll appear here",
  // which tells a signed-out visitor the instance is empty when what is
  // actually true is that they are only being shown a slice of it, and
  // offers them no way in.
  //
  // A 200 with zero items can only reach a signed-out caller when
  // public mode is on — with it off, GET /posts answers 401 and the
  // guestFeed branch takes it — so this needs no separate flag for the
  // mode. It reads `auth.user` rather than the loader's `publicMode`
  // for that reason: the condition that matters is who is looking.
  const guestEmpty = $derived(showEmpty && !auth.user);

  // ── #1190: an empty wall must say WHAT emptied it ─────────────────
  //
  // The bug this closes is not the filter. It is that a reader who
  // narrowed the feed and landed on nothing was told "No posts yet —
  // once posts are uploaded they'll appear here", which is a statement
  // about the INSTANCE. The owner picked E-book, got that sentence, and
  // read it as the type filter being broken. It was not: the feed pill
  // was on Following, `?kind=ebook` intersected with the follow graph,
  // and the intersection was legitimately empty
  // (`?kind=ebook&feed=following` → 0 while `?kind=ebook` → 4 on the
  // same session and the same stack). An honestly empty page that
  // describes itself as an empty instance is indistinguishable from a
  // broken one.
  //
  // WHICH narrowings get named here, and why not all of them: the type
  // filter and the feed scope are the two that leave no trace on the
  // page. `?team=` and `?tag=` already print themselves in the feed
  // HEADING directly above this block — a reader looking at an empty
  // wall under "Props" or "#fantasy" can see what they asked for — and
  // `?q=` has had its own "No matches / try a different search term"
  // since long before this. Repeating those here would be a second
  // label for a fact already on screen; the two that are invisible are
  // the ones that get a sentence.
  //
  // The names come from the SAME i18n keys the controls draw with —
  // `card.fallback.kind.*` is what the checkbox list and the card badge
  // are both labelled from — so the sentence cannot name a type the
  // dropdown spells differently.
  //
  // A name with no label is DROPPED rather than printed. `t()` falls
  // back to the key, so `?kind=nonsense` — which the server answers with
  // an empty page on purpose — would otherwise put the literal string
  // `card.fallback.kind.nonsense` in front of a reader. An unnameable
  // filter degrades to the plain empty state, which is the honest
  // answer: the page cannot say what it was narrowed to.
  const activeKindLabels = $derived(
    activeKindList
      .map((k) => ({ k, label: t(`card.fallback.kind.${k}`) }))
      .filter(({ k, label }) => label !== `card.fallback.kind.${k}`)
      .map(({ label }) => label)
      .join(', '),
  );
  const followingScope = $derived(browseView.filter === 'following');
  const emptyTitle = $derived(
    query
      ? t('browse.empty.no_matches')
      : activeKindLabels && followingScope
        ? t('browse.empty.kind_following_title', { types: activeKindLabels })
        : activeKindLabels
          ? t('browse.empty.kind_title', { types: activeKindLabels })
          : followingScope
            ? t('browse.empty.following_title')
            : t('browse.empty.no_posts_yet'),
  );
  const emptyHint = $derived(
    query
      ? t('browse.empty.try_different')
      : activeKindLabels && followingScope
        ? t('browse.empty.kind_following_hint')
        : activeKindLabels
          ? t('browse.empty.kind_hint')
          : followingScope
            ? t('browse.empty.following_hint')
            : t('browse.empty.uploaded_appear_here'),
  );

  // ?post={uuid} → overlay the post on top of the feed. The feed stays
  // mounted (no scroll loss, no re-fetch). The watcher, the close
  // policy and the ← / → walk all live in PostParamHost since #1130 —
  // this route only says what "sibling" means here and what to do when
  // the walk runs off the loaded end.
  //
  // `orderedIds` is already the marquee's ordering (the loaded feed in
  // feed order, the same array the grid renders from), so the arrows
  // and the range-selection gesture cannot disagree about what comes
  // next.

  // Past the end of what is loaded: kick off the next page so the walk
  // can spill into it. Fire-and-forget — the new post does not
  // auto-open, because which id is "next" is not known until the fetch
  // resolves; the user sees it on the next press.
  function loadMoreForSiblingWalk() {
    if (nextCursor && !loading) {
      void fetchPage(query, activeTeamId, activeTag, activeKinds, nextCursor, false);
    }
  }

  // ── The operator promo band (#1118) ───────────────────────────────
  //
  // A full-width strip between two feed pages. It is inserted at the
  // PAGE layer, between two grid instances — NOT into the wall.
  //
  // # Why the split is here and not in the placer
  //
  // The obvious reading of "a band between pages" is a slot in the feed
  // sequence, which is ADR 0079's in-grid model and would mean teaching
  // `masonryPlacement` about a full-width position. It is the wrong
  // model twice over. The feed is ONE flat array rendered by ONE grid —
  // there is no "wall / band / wall" seam in the data to hang a slot on
  // — and a full-width strip is ADR 0030's banner geometry, which sits
  // BETWEEN content rather than inside the packing. So the feed is
  // sliced and the switch is rendered twice, and every line of
  // `masonryPlacement.ts` is untouched: no spanning rule, no dense flow,
  // no new coordinate to reserve.
  //
  // # Why the slice cannot move a tile
  //
  // `bandAt` is a fixed index — `after_page × PAGE` — decided before the
  // first tile is placed and never recomputed from the loaded length. So
  // the head wall reaches exactly that many items and then stops
  // growing, and every append lands in the tail wall. MasonryColumns'
  // own append check (`placedIds.every(...)`) sees the head's list
  // unchanged and re-places nothing; the tail grows the way an unsplit
  // wall does. The band appears when page 2 arrives, at which moment the
  // tail's tiles have never been rendered, so nothing on screen moves.
  //
  // # Why both walls are always mounted
  //
  // The tail is rendered even while empty. Toggling between one grid and
  // two would unmount and rebuild a wall the reader is looking at, which
  // is a re-place of every tile in it — the exact instability #651
  // removed, bought back through a mounting decision instead of a
  // placement one.
  let band = $state<components['schemas']['PromoBand'] | null>(null);

  onMount(() => {
    void (async () => {
      // No error state, and no `finally` that reveals one: the band is
      // supplementary chrome on a page that has its own content, so a
      // failed fetch must leave the page looking un-curated rather than
      // broken. Same posture as FeaturedRail's loader.
      //
      // A 401 is the ordinary answer for an anonymous visitor on an
      // install that has not opened its public surface (#445), and it
      // arrives here as `error` with no data — which is "no band",
      // which is what an anonymous visitor on a closed install should
      // see.
      const { data } = await api.GET('/featured/promo');
      band = (data as { band?: components['schemas']['PromoBand'] } | undefined)?.band ?? null;
    })();
  });

  /** Where the band falls, as an index into the loaded feed.
   *
   *  `after_page` counts whole PAGES because that is what an operator
   *  can predict without knowing the API's limit; the page size is this
   *  client's, so the multiplication belongs here and nowhere else.
   *
   *  null = no band, and the page renders exactly the single wall it
   *  always did. */
  const bandAt = $derived(band ? band.after_page * PAGE : null);

  /** The band only renders once the feed has actually reached its
   *  position. Before that there is no "between" to sit in, and
   *  rendering it below a short feed would put an operator's strip
   *  somewhere they did not choose.
   *
   *  ⚠️ `>` and not `>=`: with exactly `bandAt` items loaded the band
   *  would sit under the last tile with nothing beneath it — a footer,
   *  not a band. It appears when the page after it exists. */
  const showBand = $derived(bandAt !== null && items.length > bandAt);

  const headItems = $derived(bandAt === null ? items : items.slice(0, bandAt));
  const tailItems = $derived(bandAt === null ? [] : items.slice(bandAt));
</script>

<svelte:head>
  <title>{query ? `${t('browse.title_search', { query })} — ${site.name}` : `${t('browse.title')} — ${site.name}`}</title>
</svelte:head>

<div class="w-full px-4 py-4 space-y-4 sm:px-6">
  {#if query}
    <p class="text-sm text-fg-muted">
      {t('browse.results_for', { query })}
    </p>
  {/if}

  <!-- Multi-select indicator (#515 slice 3). Sticky under the navbar so
       the count stays visible while scrolling a long feed; the full
       bulk-action bar is #39. Renders only while a selection is active. -->
  <div class="sticky top-2 z-30 empty:hidden">
    <SelectionBar />
  </div>

  <!-- #417 — the curated rail sits ABOVE both branches below. For a
       guest it is the entire landing page (posts are members-only);
       for a member it is a curated strip over their feed. Rendering it
       outside the guest/member split is what makes it the same surface
       for both, which is the point of curating it.

       #908 — but only when the page is BROWSE. `?q=` (the navbar search
       box submits here) turns this route into a result surface, and a
       strip of curated collections that has nothing to do with the
       query is then just unrelated content pinned above the answer.
       `query` is the route's only narrowing param, so it is the whole
       condition: everything else — the feed pill, the sort direction,
       the view mode — rearranges the same set rather than asking a
       question, and the rail belongs over all of those.

       ADR 0065 / #417 is why this is `{#if !query}` and not a
       guest/member check: unfiltered browse keeps the rail for BOTH,
       including the signed-out visitor for whom it is the only thing on
       the page. -->
  <!-- #577 — the teams rail sits BELOW FeaturedRail deliberately. The
       featured strip is the operator's curation and is the whole page
       for a guest; the teams rail renders nothing at all for a guest,
       so putting it second keeps the signed-out layout identical to
       what it was. -->
  {#if !query}
    <FeaturedRail />
  {/if}

  <!-- The teams rail is NOT under the `{#if !query}` gate the featured
       strip is under, and the difference is the point of #1113.
       #908 pulled both strips off the result surface because a CURATED
       strip over an answer is unrelated content pinned above it. That
       argument holds for the featured slider and inverts for this one:
       the rail is now the feed's FILTER CONTROL, and `?q=` composes
       with `team_id` server-side (both are `/posts` parameters), so
       hiding it over a query would leave a reader who searched while
       filtered with a narrowed result set and no visible way to widen
       it again — the filter would be live, in the URL, and unreachable.
       Keeping it also makes "search inside this team" a thing the page
       can express. -->
  <BrowseRail {activeTeamId} {activeTag} onselect={selectTeam} onselecttag={selectTag} />

  {#if !query || activeTeam || activeTag}
    <!-- The feed's own heading (#1113). It names what the wall below
         IS — the filtered team, or "All Teams" when nothing is picked —
         and it belongs to the FEED rather than to the rail: it scrolls
         away with the content while the rail pins above it.

         The follow button is inline beside it so the reader can follow
         the team they just filtered to without leaving browse. It is
         the SAME TeamFollowButton the team page and the directory use,
         reading the same `teamFollows` store, so following here moves
         the rail's sort on the next paint with nothing plumbed between
         them.

         `activeTeam` is null while the rail is still loading and for a
         `?team=` this reader cannot see, and both fall back to the
         unfiltered heading rather than printing a raw uuid. -->
    <!-- Three states, one heading (#1123 adds the third): the filtered
         team's name, the filtered tag in its `#fantasy` form, or the
         unfiltered title. The hash is PRINTED HERE and not stored —
         `?tag=fantasy` is what the corpus matches, and the heading is
         the one place the reader should see the notation the rail chip
         draws with a glyph.

         `activeTag` is used raw rather than resolved through a store,
         unlike `activeTeam`. There is nothing to resolve: a tag IS its
         string, so there is no id-to-name lookup that could fail and no
         raw-uuid fallback to guard against. -->
    <div class="flex flex-wrap items-center gap-3">
      <h2 class="text-2xl font-semibold text-fg" data-testid="browse-feed-heading">
        {#if activeTeam}
          {activeTeam.name}
        {:else if activeTag}
          #{activeTag}
        {:else}
          {t('teams.rail_all_heading')}
        {/if}
      </h2>
      {#if activeTeam}
        <TeamFollowButton team={activeTeam} />
      {:else if activeTag}
        <!-- The tag's own follow toggle, beside its heading, for the
             same reason the team one is: a reader who filtered to a tag
             from a link or a post's tag list should be able to keep it
             without hunting for the ⋯ panel. Following here moves the
             rail on the next paint because both read `tagFollows`. -->
        <button
          type="button"
          onclick={() => void tagFollows.toggle(activeTag)}
          disabled={tagFollows.isPending(activeTag)}
          aria-pressed={tagFollows.isFollowing(activeTag)}
          data-testid="browse-tag-follow"
          class="inline-flex items-center gap-1.5 rounded-full border px-3 py-1.5 text-sm
                 font-medium transition-colors disabled:opacity-60 {tagFollows.isFollowing(
            activeTag,
          )
            ? 'border-accent bg-accent-container text-on-accent-container'
            : 'border-border bg-surface-elevated text-fg hover:border-border-strong hover:bg-state-hover'}"
        >
          {tagFollows.isFollowing(activeTag)
            ? t('tags.following')
            : t('tags.follow')}
        </button>
      {/if}
    </div>
  {/if}

  {#if guestFeed}
    <!-- Calm empty state, not an alert. See the !auth.user branch in
         the loader. -->
    <div
      class="rounded-xl border border-dashed border-border p-12 text-center"
      data-testid="guest-feed-empty"
    >
      <p class="text-base font-medium text-fg">{t('user_menu.guest_feed_title')}</p>
      <p class="mx-auto mt-1 max-w-md text-sm text-fg-muted">{t('user_menu.guest_feed_hint')}</p>
      <div class="mt-4 flex flex-wrap items-center justify-center gap-2">
        <a
          href="/collections"
          class="inline-flex items-center rounded-md border border-border px-4 py-2 text-sm font-medium text-fg hover:border-border-strong"
        >
          {t('user_menu.guest_browse_collections')}
        </a>
        <a
          href="/login"
          class="inline-flex items-center rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent"
        >
          {t('user_menu.sign_in')}
        </a>
      </div>
    </div>
  {:else if error}
    <div role="alert" class="rounded-md border border-danger/40 bg-danger-container px-4 py-3 text-sm text-danger">
      {error}
    </div>
  {/if}

  {#if guestEmpty}
    <!-- #1176 — signed out, public mode on, nothing public to show.
         Same calm shape as the guestFeed block above, and the same two
         affordances: collections (still browsable) and a way in. -->
    <div
      class="rounded-xl border border-dashed border-border p-12 text-center"
      data-testid="guest-public-empty"
    >
      <p class="text-base font-medium text-fg">
        {query ? t('browse.empty.no_matches') : t('browse.empty.guest_title')}
      </p>
      <p class="mx-auto mt-1 max-w-md text-sm text-fg-muted">
        {query
          ? t('browse.empty.guest_no_matches_hint')
          : t('browse.empty.guest_hint')}
      </p>
      <div class="mt-4 flex flex-wrap items-center justify-center gap-2">
        <a
          href="/collections"
          class="inline-flex min-h-11 items-center rounded-md border border-border px-4 py-2 text-sm font-medium text-fg hover:border-border-strong"
        >
          {t('user_menu.guest_browse_collections')}
        </a>
        <a
          href="/login"
          class="inline-flex min-h-11 items-center rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent"
          data-testid="guest-public-empty-signin"
        >
          {t('user_menu.sign_in')}
        </a>
      </div>
    </div>
  {:else if showEmpty}
    <!-- #1190 — the sentence names the narrowing that is not otherwise
         visible on the page. See `emptyTitle` for which ones those are
         and why the team/tag chips are deliberately not among them. -->
    <div
      class="rounded-xl border border-dashed border-border p-12 text-center text-fg-muted"
      data-testid="browse-empty"
    >
      <p class="font-medium text-fg" data-testid="browse-empty-title">{emptyTitle}</p>
      <p class="mt-1 text-sm" data-testid="browse-empty-hint">{emptyHint}</p>
    </div>
  {:else}
    <!--
      Layout is driven by browseView (footer switcher + localStorage).
        grid / thumbnail → auto-fill grid, tiles ≥ --tile-min
        masonry          → multi-column flow, columns ≥ --tile-min
        feed             → single column, image full-bleed
        list             → sortable table
      `--tile-min` is set inline because it's user state, not a design
      token — it changes per interaction, so it can't be a class.
      Column COUNT is never computed: see browseView.svelte.ts.
    -->
    <!-- The marquee's surface. `relative` is what the band positions
         against; `select-none` stops a drag across the wall painting a
         browser text selection over every title it crosses, which is
         the one visual artefact a rubber band must not have. -->
    <!-- `data-testid` is here for the #1138 drag guard: the marquee is
         the third `nativeDrag` consumer and the only one whose surface
         is not already addressable, so the per-consumer drag test had
         nothing to press on. -->
    <!-- The card and table snippets are declared HERE, at the page, and
         handed to ContentGrid as props rather than written inside its
         tag. A promo band renders the switch TWICE (head wall, band,
         tail wall) and both walls must draw the identical card — a
         second inline copy would be a second thing to keep in step, on
         the two halves of one feed. -->
    {#snippet cardSnippet(item: unknown, mode: typeof browseView.mode)}
      {@const post = item as Post}
      <PostCard {post} {mode} feed={mode === 'feed'} tileSizes={browseView.tileSizes} {orderedIds} />
    {/snippet}
    {#snippet listSnippet()}
      <PostListTable {items} {loading} {orderedIds} />
    {/snippet}

    <div
      bind:this={wallEl}
      {...marquee.handlers}
      class="relative select-none"
      data-testid="browse-wall"
    >
      {#if bandAt === null || browseView.mode === 'list'}
        <!-- The unsplit feed: no band configured, or LIST mode.

             List is deliberately excluded and the exclusion is a design
             call, not an oversight. The list view is one `role="grid"`
             with a sticky header, drag-resizable columns and rows that
             are `role="row"` children of it. A promo band cannot go
             between two halves of that: splitting the table gives the
             reader two sticky headers and two independently sorted
             halves, and putting the band INSIDE the grid puts a
             non-row child in an ARIA grid. The band is a discovery
             surface for a WALL of art; the list is a working table, and
             the honest answer for it is nothing rather than something
             misplaced. -->
        <ContentGrid
          mode={browseView.mode}
          {items}
          tileMin={browseView.tileMin}
          {loading}
          card={cardSnippet}
          list={listSnippet}
        />
      {:else}
        <!-- Head wall — a FIXED slice, so it stops growing at the band
             and every append lands below. `setSize` is the whole feed:
             a wall that announced its own length would tell a screen
             reader "36 of 36" halfway down a 108-post feed (ADR 0079's
             aria consequence). -->
        <ContentGrid
          mode={browseView.mode}
          items={headItems}
          tileMin={browseView.tileMin}
          loading={false}
          setSize={items.length}
          card={cardSnippet}
          list={listSnippet}
        />

        {#if showBand && band}
          <PromoBand {band} />
        {/if}

        <!-- Tail wall. Always mounted, even while empty — see the note
             on `band` above for why toggling its existence would
             re-place a wall the reader is looking at. It owns the
             loading skeletons, because it is where the next page
             lands. -->
        <ContentGrid
          mode={browseView.mode}
          items={tailItems}
          tileMin={browseView.tileMin}
          {loading}
          posOffset={bandAt}
          setSize={items.length}
          card={cardSnippet}
          list={listSnippet}
        />
      {/if}

      {#if marquee.rect}
        <!-- Fixed, not absolute: the rect is computed in viewport space
             for painting (the store keeps document space for the maths),
             so it stays put while edge-autoscroll moves the wall
             underneath it. pointer-events-none or the band would
             hit-test itself and swallow the pointerup. -->
        <div
          aria-hidden="true"
          data-testid="marquee-band"
          class="pointer-events-none fixed z-30 rounded-sm border border-accent bg-accent/20"
          style="left:{marquee.rect.left}px; top:{marquee.rect.top}px; width:{marquee.rect.width}px; height:{marquee.rect.height}px;"
        ></div>
      {/if}
    </div>

    {#if hasMore}
      <div bind:this={sentinel} class="h-px w-full" aria-hidden="true"></div>
    {/if}

    {#if !hasMore && items.length > 0}
      <p class="text-center text-xs text-fg-muted py-4">{t('browse.end_of_feed')}</p>
    {/if}
  {/if}
</div>

<PostParamHost ordered={orderedIds} onEndReached={loadMoreForSiblingWalk} />

<!-- Floating browse controls: view switcher + back-to-top. Stays
     mounted alongside the feed so the user can change layouts without
     losing scroll position. -->
<BrowseFooter kinds={activeKindList} onkinds={(next) => void selectKinds(next)} />

<!-- The grid / masonry / feed / list layouts moved to the shared
     ContentGrid component (#511) so the profile + post-by-asset pages
     render modes identically. No page-local layout CSS remains here. -->
