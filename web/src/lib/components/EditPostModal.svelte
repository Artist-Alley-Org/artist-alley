<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // THE POST EDITOR (#1119): the author-facing surface "Edit post" has
  // pointed at since the post menu was built.
  //
  // # What was here before
  //
  // A `stubAction()` alert saying the feature was coming soon. Every
  // column this dialog writes has been accepted by `PATCH /posts/{id}` for
  // sprints (title, description, visibility, tags, the cover and its focal
  // pair) and no shipped client sent that request except the cover dialog, which sent
  // three of them. So an author who wanted to fix a typo in a published
  // title had the API and no product.
  //
  // # It is EditCollectionModal's shape, deliberately
  //
  // ADR 0091's one-editing-surface ruling, and #1264's reading of it:
  // "I really think we shouldn't have more than one menu to edit
  // collections. We can put all editing of collection items, including
  // all cover types, in that same modal." The post menu had grown the
  // same pair the collection menu had, a real "Cover and framing…"
  // beside a stubbed "Edit post…", so the cover becomes a SECTION here
  // exactly as CollectionCoverEditor is a section there, its own menu
  // item goes, and there is one Save for the whole surface.
  //
  // Everything that made the collection version correct is carried
  // across rather than rediscovered:
  //
  //   * THE SNAPSHOT (#1262). The `post` prop is a live view of the
  //     host's copy of the row and the host refetches into it. While
  //     this dialog is open that is a second author writing into the
  //     surface the first one is typing in, and the concurrency baseline
  //     is the worst thing for it to touch: re-seeding
  //     `if_unchanged_since` from a refetch hands the dialog the
  //     timestamp of the very write it exists to detect. So the form,
  //     the baseline and every "was this set before I started?" guard
  //     read one snapshot taken on the OPEN EDGE.
  //   * THE TRI-STATES. A clear is a companion flag, never
  //     `x: null`, because a PATCH body distinguishes absent from null
  //     only if every reader agrees on the encoding. Each guard asks the
  //     SNAPSHOT whether the thing was already set, so a rename cannot
  //     send a clear for something a third party set thirty seconds ago.
  //
  // # ⛔ WHAT THIS DIALOG DOES NOT DO
  //
  //   * It does not write `state_id`. Published-or-draft is moved by
  //     `POST /posts/{id}/publish` / `unpublish` and nothing else (ADR
  //     0091 decisions 6 + 7): those run the workflow machine, its
  //     capability gate and its audit row, and they emit the Create /
  //     Delete a peer acts on. `PATCH /posts/{id}` deliberately drops
  //     `state_id` (#949, blocked on #895/#896/#897), so saving this
  //     form CANNOT publish a draft or unpublish a published post. The
  //     publication block below is the shipped endpoint, called
  //     directly, with its own button and its own busy state.
  //   * It does not schedule. That is #1238.
  //   * It does not ADD the post to a collection. CollectionPicker
  //     already does that from the same menu, and this section is the
  //     other half of the sentence: what is holding this post, and what
  //     the author can take it off.

  import { untrack } from 'svelte';
  import { api } from '$api/client';
  import { canDelete } from '$lib/deletable';
  import { t } from '$stores/lang.svelte';
  import Modal from './Modal.svelte';
  import PostCoverEditor from './PostCoverEditor.svelte';

  // The tiers offered, widest LAST, in the order and with the labels the
  // two create surfaces already use (`create.vis_*`). Read out of that
  // catalogue rather than a second set here so the three post surfaces
  // cannot drift into describing one tier differently. That is the defect #1240
  // fixed between /create and the upload modal, arriving from a third
  // place.
  //
  // `explicit-share` is in the column's enum and is NOT offered, here or
  // on either create surface: it is not a tier an author picks, it is
  // what a post becomes when they grant access to somebody
  // (`post_acls`, reached from "Manage access…"). See `tiers` for what
  // happens to a post already sitting on it.
  type Visibility = 'org-only' | 'followers' | 'private' | 'public';
  const OFFERED_TIERS: Visibility[] = ['org-only', 'followers', 'private', 'public'];

  interface Member {
    asset_id: string;
    restricted?: boolean;
    asset?: { ladder_available?: boolean; preview_available?: boolean } | null;
  }
  interface PostShape {
    id: string;
    title?: string;
    description?: string;
    visibility?: string;
    draft?: boolean;
    author_user_ref?: number;
    tags?: string[];
    members?: Member[];
    cover_asset_id?: string | null;
    cover_focal_x?: number | null;
    cover_focal_y?: number | null;
    updated_at?: string;
  }

  interface Props {
    open: boolean;
    /** The post, live. The FORM reads the snapshot below; the publication
     *  block reads this, because a publish done from inside the dialog
     *  has to change what the dialog says about it. */
    post: PostShape;
    onclose: () => void;
    /** Fired after any successful write: the metadata save, a
     *  publication move, a membership removal.
     *
     *  ⚠️ IT CARRIES NOTHING, and that is the point. The host re-reads
     *  `GET /posts/{id}`. Several of a post's fields are derived server
     *  side (`updated_at` at minimum, the AI provenance whenever the
     *  cover changes, `search_text`, the enriched member flags), so a
     *  client that stitched its own answer out of the values it sent
     *  would show a post that never existed. Same reasoning the publish
     *  toggle and the cover save already applied. */
    onsaved?: () => void;
  }

  let { open, post, onclose, onsaved }: Props = $props();

  // ── THE ROW THIS DIALOG IS EDITING, AS IT WAS WHEN IT OPENED ──────
  //
  // `$state.raw` because it is REPLACED, never mutated. `untrack` on the
  // initial value because capturing the prop once is precisely the
  // intent, and it is also what the compiler warns about
  // (`state_referenced_locally`) since it is usually a mistake.
  let seeded = $state.raw<PostShape>(untrack(() => post));

  let title = $state('');
  let description = $state('');
  let visibility = $state<string>('org-only');
  let tags = $state<string[]>([]);
  let submitting = $state(false);
  let error = $state<string | null>(null);

  // Edit-safety: `updated_at` as it was on OPEN, sent as
  // `if_unchanged_since` so the server can refuse a save composed
  // against a row somebody has since moved.
  //
  // ⛔ ON OPEN IS THE WHOLE POINT (#1262 on the collection side). Seeded
  // from `seeded` with everything else, exactly once per open.
  let baselineUpdatedAt = $state<string>('');
  let conflict = $state<{ updatedAt: string } | null>(null);

  // The cover and its framing, held HERE rather than inside the section:
  // this component owns the post, the concurrency baseline and the one
  // PATCH, so it owns the values that PATCH sends. The section is a
  // picker and a marquee over them.
  let coverAssetId = $state<string | null>(null);
  // Deep `$state`, passed BY REFERENCE rather than `$bindable`: the
  // section mutates the proxy and this component sees it, which is what
  // makes "one Save applies the cover AND its framing" true with no sync
  // effect anywhere. Null is centre and stays distinct from 0.5, and that
  // is what makes Reset a clear (migration 00055's CHECK).
  let framing = $state<{ x: number | null; y: number | null }>({ x: null, y: null });

  /** THE CARD'S RESOLUTION ORDER: the explicit cover, else the first
   *  member the caller may picture. PostCard reads exactly this, and the
   *  crop marquee has to be drawn over the picture the tile will paint,
   *  so seeding the picker any other way would frame something else.
   *
   *  Used for the SEED and for the "did the author change the cover?"
   *  comparison, from one function, because those two answers
   *  disagreeing is how a save comes to pin a cover nobody chose. */
  function resolveCover(p: PostShape): string | null {
    if (p.cover_asset_id) return p.cover_asset_id;
    return (p.members ?? []).find((m) => !m.restricted)?.asset_id ?? null;
  }

  // ── Tag chip input (the create surfaces' behaviour, not a new one) ──
  let tagDraft = $state('');
  function commitTag() {
    const v = tagDraft.trim().toLowerCase();
    if (!v) {
      tagDraft = '';
      return;
    }
    if (!tags.includes(v)) tags = [...tags, v];
    tagDraft = '';
  }
  function removeTag(v: string) {
    tags = tags.filter((x) => x !== v);
  }
  function handleTagKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      commitTag();
    } else if (e.key === 'Backspace' && tagDraft === '' && tags.length > 0) {
      tags = tags.slice(0, -1);
    }
  }

  /** Take the snapshot and fill the form from it. Called on the OPEN
   *  EDGE only. See the effect below. */
  function seedFromPost() {
    seeded = post;
    title = seeded.title ?? '';
    description = seeded.description ?? '';
    visibility = seeded.visibility ?? 'org-only';
    // A COPY. `tags` is reassigned by the chip input, but seeding it with
    // the prop's own array would hand this component a reference into the
    // host's post object, and `tags = [...tags, v]` is only safe because
    // nothing else holds the array it replaces.
    tags = [...(seeded.tags ?? [])];
    baselineUpdatedAt = seeded.updated_at ?? '';
    coverAssetId = resolveCover(seeded);
    framing = { x: seeded.cover_focal_x ?? null, y: seeded.cover_focal_y ?? null };
    error = null;
    conflict = null;
    publishError = null;
    membershipError = null;
    void loadMemberships(seeded.id);
  }

  // ── SEED ON THE OPEN EDGE, NOT ON EVERY PROP CHANGE (#1262) ───────
  //
  // `open` is the ONLY tracked read in this effect. Everything the seed
  // touches is read inside `untrack`, which is what makes the dependency
  // set the edge rather than the row: Svelte collects dependencies
  // through the whole call frame, so putting the reads in a function does
  // nothing on its own, and `post.title` &c. are prop reads that would
  // otherwise re-run this on every refetch the host does, including the
  // refetch this dialog's own Save triggers.
  $effect(() => {
    const isOpen = open;
    untrack(() => {
      if (isOpen) seedFromPost();
    });
  });

  // A post already on a tier this page does not offer keeps that option
  // on screen, SELECTED. Hiding it would leave the radio group with
  // nothing chosen and quietly present the post as something it is not,
  // and the first save would then narrow or widen a tier the author never
  // touched. Read off the SNAPSHOT so a refetch landing mid-edit cannot
  // pull the option out from under the selected value.
  const tiers = $derived<string[]>(
    OFFERED_TIERS.includes(seeded.visibility as Visibility)
      ? OFFERED_TIERS
      : [...OFFERED_TIERS, seeded.visibility ?? 'private'],
  );
  const tierIsUnoffered = $derived(!OFFERED_TIERS.includes(visibility as Visibility));

  function tierLabel(v: string): string {
    return t(`create.vis_${v.replace('-', '_')}`);
  }

  /** The viewport width, tracked so the cover section can withhold the
   *  two-dimensional crop stage below CROP_STAGE_MIN_WIDTH.
   *
   *  Measured rather than answered with a CSS media query because the
   *  decision is not "lay this out differently", it is "do not render
   *  this control": a `hidden` stage would still be in the DOM, still
   *  loading its picture, and still reachable by a screen reader and by
   *  the tests. */
  let viewportWidth = $state(1024);

  // ── Collection membership (#1119, #882) ───────────────────────────
  //
  // ⛔ MEMBERSHIP IS COLLECTION-OWNED. Authoring the post grants nothing
  // here: removal is `DELETE /collections/{id}/posts/{post_id}`, whose
  // gate is `collections.ResolveMemberWrite` (owner, `collections.admin`
  // or `system.admin`) and an author is none of those on somebody
  // else's shelf.
  //
  // So the button's presence is not a guess and not derived from
  // `owner_user_ref` here: `GET /posts/{id}/collections` returns
  // `can_remove` per item, resolved by that same predicate server side.
  // A client that decided it locally would be a copy of an authorization
  // rule living in a browser, and it would be wrong for the two
  // capability holders the predicate also admits.
  //
  // A non-actionable membership is SHOWN, named and linkable, with a line
  // saying whose it is. Hiding it would answer "which shelves hold my
  // post" with a lie; greying out a Remove button that could never
  // succeed would be an offer we cannot keep.
  interface Membership {
    collection: { id: string; name: string; owner_user_ref: number; visibility: string };
    can_remove: boolean;
  }
  let memberships = $state<Membership[]>([]);
  let withheldCount = $state(0);
  let membershipLoading = $state(false);
  let membershipError = $state<string | null>(null);
  let removingId = $state<string | null>(null);

  async function loadMemberships(postId: string) {
    membershipLoading = true;
    membershipError = null;
    try {
      const { data, error: apiErr } = await api.GET('/posts/{id}/collections', {
        params: { path: { id: postId } },
      });
      if (apiErr || !data) {
        membershipError = t('post_edit.collections_error');
        memberships = [];
        withheldCount = 0;
        return;
      }
      memberships = (data.items ?? []) as Membership[];
      withheldCount = data.withheld_count ?? 0;
    } catch {
      membershipError = t('post_edit.collections_error');
      memberships = [];
      withheldCount = 0;
    } finally {
      membershipLoading = false;
    }
  }

  /** Un-pin the post from ONE collection.
   *
   *  Separate from Save on purpose, and not batched into it: a membership
   *  is a row in somebody else's collection, so it is not this post's
   *  data and cannot ride this post's `if_unchanged_since`. It is also
   *  the one write on this surface whose authority is not the author's.
   *
   *  ⚠️ IT TOUCHES EXACTLY ONE SHELF. The endpoint deletes one
   *  `collection_posts` row and nothing else (#882): the post survives,
   *  and every other collection referencing it is undisturbed. The list
   *  is RE-READ from the server afterwards rather than spliced locally,
   *  so what the author sees is what the rows say. */
  async function removeFrom(collectionId: string) {
    if (removingId) return;
    removingId = collectionId;
    membershipError = null;
    try {
      const { error: apiErr } = await api.DELETE('/collections/{id}/posts/{post_id}', {
        params: { path: { id: collectionId, post_id: seeded.id } },
      });
      if (apiErr) {
        membershipError = t('post_edit.collection_remove_error');
        return;
      }
      await loadMemberships(seeded.id);
      // The post's own row did not move, but a collection page or a feed
      // underneath may be showing the membership that just went.
      onsaved?.();
    } catch {
      membershipError = t('post_edit.collection_remove_error');
    } finally {
      removingId = null;
    }
  }

  // ── Publication (ADR 0091 decisions 6 + 7) ────────────────────────
  //
  // The SHIPPED endpoints, called directly. Not part of Save, not part
  // of the PATCH body, and visually its own block with its own button:
  // an author editing a title must not be able to publish a draft by
  // accident, and the form physically cannot do it because
  // `PATCH /posts/{id}` drops `state_id`.
  //
  // `canPublish` mirrors the post menu's own test, the author or a
  // global `posts.admin` / `system.admin` holder, which is the gate the
  // endpoint applies. A team-scoped holder sees no button and would be
  // refused anyway; see $lib/deletable for why that ceiling is not
  // worked around.
  let publishBusy = $state(false);
  let publishError = $state<string | null>(null);

  // `canDelete('post', …)` is not a typo and not a shortcut: it is the
  // client's EXISTING spelling of "the author, a global posts.admin, or
  // system.admin", the same three the publish endpoint accepts, and
  // the post menu's own publish item already reads it. Restating the
  // capability codes here would be a second copy of that test, which is
  // how the menu and the dialog come to disagree about who gets a button.
  const canPublish = $derived(canDelete('post', post.author_user_ref));

  async function togglePublication() {
    if (publishBusy) return;
    publishBusy = true;
    publishError = null;
    const path = post.draft ? '/posts/{id}/publish' : '/posts/{id}/unpublish';
    const { data, error: apiErr } = await api.POST(path, {
      params: { path: { id: seeded.id } },
    });
    publishBusy = false;
    if (apiErr || !data) {
      publishError =
        (apiErr as { error?: string } | undefined)?.error ?? t('post_menu.publish_failed');
      return;
    }
    // ⚠️ THE BASELINE MOVES WITH IT, and forgetting this is a bug the
    // author would have no way to understand. The transition writes
    // `state_id = $2, updated_at = NOW()` on `posts`, so publishing from
    // inside this dialog advances the very timestamp the open form is
    // holding: without this line the author publishes, presses Save, and
    // is told somebody else edited their post.
    //
    // It is sound because THIS CALLER made the write. `if_unchanged_since`
    // asks "has anyone else moved this row", and the answer after your
    // own publish is still no. The value comes from the response rather
    // than from a fresh GET for the same reason the rest of this file
    // re-reads instead of guessing: it is the server's number.
    const fresh = data as PostShape;
    if (fresh.updated_at) baselineUpdatedAt = fresh.updated_at;
    // Let the host re-read, which is what flips the label below.
    onsaved?.();
  }

  async function submit() {
    if (submitting) return;
    submitting = true;
    error = null;
    try {
      const { error: apiErr, response } = await api.PATCH('/posts/{id}', {
        params: { path: { id: seeded.id } },
        body: {
          title: title.trim(),
          description,
          visibility: visibility as Visibility,
          // A REPLACE, and the chip row is the whole value: the author
          // sees every tag this post has and the set on screen is the set
          // that is stored. `dedupeTags` server side is the backstop, not
          // the rule.
          tags,
          if_unchanged_since: baselineUpdatedAt || undefined,
          ...coverBody(),
        },
      });
      if (response.status === 409) {
        const c = apiErr as { error?: string; updated_at?: string } | undefined;
        conflict = { updatedAt: c?.updated_at ?? '' };
        return;
      }
      if (apiErr) {
        error = (apiErr as { error?: string } | undefined)?.error ?? t('post_edit.error_save');
        return;
      }
      // ⛔ THE RESPONSE BODY IS NOT ADOPTED. `onsaved` makes the host
      // re-read; see the prop's own note for why a stitched-together post
      // is the thing to avoid. The 200 body would in fact be correct
      // here, and relying on that is what makes the next write path that
      // returns less than a whole post a silent bug.
      onsaved?.();
      onclose();
    } finally {
      submitting = false;
    }
  }

  /** The cover half of the PATCH body, as exclusive branches.
   *
   *  THE SHIPPED RULES, and all three have to hold at once:
   *
   *  1. CHANGING THE COVER DISCARDS THE OLD FRAMING (#1333). A focal
   *     pair is a fraction of ONE picture's width and height, so it
   *     means nothing on the next one. The server nulls the pair when
   *     `cover_asset_id` arrives without one, so branch 2 sends no clear
   *     and needs none.
   *  2. SAVING A FRAMING PINS THE COVER. A post with no explicit
   *     `cover_asset_id` shows its FIRST MEMBER, a picture that can
   *     change under it: reorder the members and the cover moves,
   *     carrying a fraction chosen against the old one. So the picture a
   *     framing was chosen against is written down beside it. Nothing
   *     the author can see changes: what they framed is what the card
   *     was already showing.
   *  3. A CLEAR IS A FLAG, never `cover_focal_x: null`, and sending it
   *     alongside the pair is a 400 rather than a silent precedence
   *     rule. Hence exclusive branches.
   *
   *  ⛔ AND THE FOURTH, WHICH IS THIS SURFACE'S OWN. The cover section is
   *  on screen for every edit now, not behind its own menu item, so
   *  "always send `cover_asset_id`" would make renaming a post pin its
   *  cover, a write the author did not ask for, which is exactly what
   *  EditCollectionModal's `seeded.*` guards exist to prevent. The guards
   *  here ask the SNAPSHOT, so an untouched cover sends nothing at all.
   */
  function coverBody(): Record<string, unknown> {
    const framed = framing.x != null && framing.y != null;
    const wasFramed = seeded.cover_focal_x != null && seeded.cover_focal_y != null;
    const moved = coverAssetId !== null && coverAssetId !== resolveCover(seeded);

    if (framed && coverAssetId !== null) {
      return {
        cover_asset_id: coverAssetId,
        cover_focal_x: framing.x,
        cover_focal_y: framing.y,
      };
    }
    if (moved) return { cover_asset_id: coverAssetId };
    if (wasFramed) return { clear_cover_focal: true };
    return {};
  }

  // The author clicked "keep my edits and save again": advance the
  // baseline to the timestamp the refusal reported, so the next submit
  // does not 409 on the same row. The edited form values stay in place so
  // the author can keep them, merge by hand, or Cancel.
  function acknowledgeConflict() {
    if (conflict) {
      baselineUpdatedAt = conflict.updatedAt;
      conflict = null;
    }
  }
</script>

<svelte:window bind:innerWidth={viewportWidth} />

<!-- ONE SURFACE, ONE WIDTH. The collection editor's `max-w-[min(96rem,
     95vw)]`, measured 1536px at 1080p, for the reason #1264 records:
     "big enough to judge a picture by" is a proportion of the screen, and
     a Tailwind size step on a 4k display is a cramped dialog with more
     whitespace around it. The cover marquee is the control that needs
     the width; everything else in here is a text field.

     WIDTH IS FIXED, HEIGHT IS DRIVEN BY CONTENT (#1220 + #1264). An
     ALLOCATED box a sparse form cannot fill is a dead band, so nothing
     inside takes a definite height: every region is content-sized under
     a cap, and the cap is the viewport minus the chrome. 12rem of chrome
     is MEASURED (header, footer, backdrop padding) rather than
     estimated. -->
<Modal title={t('post_edit.title')} {open} {onclose} panelClass="max-w-[min(96rem,95vw)]">
  <div
    class="grid items-start gap-x-6 gap-y-5 overflow-y-auto pr-1 lg:grid-cols-[minmax(0,26rem)_minmax(0,1fr)]"
    style="max-height: min(calc(100vh - 12rem), 68rem)"
    data-testid="post-edit-body"
  >
    <!-- WHAT THE POST IS, in one column: the text fields, the visibility
         ladder, the tags, and the publication state. The column is narrow
         on purpose: a title and a description do not get better with
         more width, and everything the extra width buys goes to the cover
         section, which is the control that needed it. -->
    <div class="min-w-0 space-y-4" data-testid="post-edit-details">
      {#if error}
        <p
          role="alert"
          data-testid="post-edit-error"
          class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger"
        >
          {error}
        </p>
      {/if}
      {#if conflict}
        <div
          role="alert"
          data-testid="post-edit-conflict"
          class="rounded border border-warning/40 bg-warning/10 px-3 py-2 text-sm"
        >
          <p class="font-medium text-warning">{t('post_edit.conflict_heading')}</p>
          <p class="mt-1 text-xs text-fg-muted">{t('post_edit.conflict_body')}</p>
          <button
            type="button"
            onclick={acknowledgeConflict}
            data-testid="post-edit-conflict-ack"
            class="mt-2 rounded border border-warning/60 px-2 py-1 text-xs font-medium text-warning hover:bg-warning/20"
          >{t('post_edit.conflict_overwrite')}</button>
        </div>
      {/if}

      <label class="block">
        <span class="mb-1 block text-xs font-medium text-fg-muted">{t('post_edit.post_title')}</span>
        <input
          type="text"
          bind:value={title}
          maxlength="500"
          data-testid="post-edit-title"
          placeholder={t('post_edit.post_title_placeholder')}
          class="w-full rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        />
      </label>
      <label class="block">
        <span class="mb-1 block text-xs font-medium text-fg-muted">{t('post_edit.description')}</span>
        <textarea
          bind:value={description}
          rows="5"
          data-testid="post-edit-description"
          placeholder={t('post_edit.description_placeholder')}
          class="w-full resize-y rounded border border-border-strong bg-surface px-3 py-1.5 text-sm focus-visible:ring-2 focus-visible:ring-ring focus:outline-none"
        ></textarea>
      </label>

      <fieldset data-testid="post-edit-visibility">
        <legend class="mb-1 block text-xs font-medium text-fg-muted">
          {t('post_edit.visibility')}
        </legend>
        <div class="grid grid-cols-2 gap-2">
          {#each tiers as v (v)}
            <label
              class="cursor-pointer rounded border border-border bg-surface px-3 py-2 text-center text-sm hover:border-border-strong"
              class:border-accent={visibility === v}
              class:text-accent={visibility === v}
            >
              <input
                type="radio"
                name="post_vis_edit"
                value={v}
                bind:group={visibility}
                data-testid="post-edit-vis-{v}"
                class="sr-only"
              />
              {tierLabel(v)}
            </label>
          {/each}
        </div>
        <p class="mt-2 text-xs text-fg-muted">{t('post_edit.visibility_help')}</p>
        {#if tierIsUnoffered}
          <p class="mt-1 text-xs text-warning" data-testid="post-edit-vis-kept-note">
            {t('post_edit.vis_kept_note')}
          </p>
        {/if}
      </fieldset>

      <div data-testid="post-edit-tags">
        <p class="mb-1 text-xs font-medium text-fg-muted">{t('post_edit.tags')}</p>
        <div
          class="flex flex-wrap items-center gap-1.5 rounded border border-border bg-surface px-2 py-1.5"
        >
          {#each tags as tag (tag)}
            <span
              class="inline-flex items-center gap-1 rounded-full bg-surface-elevated px-2 py-0.5 text-xs text-fg"
              data-testid="post-edit-tag"
              data-tag={tag}
            >
              #{tag}
              <button
                type="button"
                onclick={() => removeTag(tag)}
                class="text-fg-muted hover:text-fg"
                aria-label={t('post_edit.remove_tag_aria', { tag })}
              >
                <svg xmlns="http://www.w3.org/2000/svg" width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
                  <path d="M18 6 6 18M6 6l12 12" />
                </svg>
              </button>
            </span>
          {/each}
          <input
            type="text"
            bind:value={tagDraft}
            onkeydown={handleTagKeydown}
            onblur={commitTag}
            data-testid="post-edit-tag-input"
            placeholder={tags.length === 0 ? t('post_edit.tags_placeholder') : '+'}
            class="min-w-[8rem] flex-1 bg-transparent px-1 py-0.5 text-sm placeholder:text-fg-muted/60 focus:outline-none"
          />
        </div>
      </div>

      <!-- PUBLICATION IS ITS OWN ACT, and the block says so in the copy
           as well as in the wiring. The button below calls the shipped
           endpoint; Save above cannot reach `state_id` at all. -->
      <section
        data-testid="post-edit-publication"
        aria-label={t('post_edit.publication')}
        class="rounded border border-border bg-surface-elevated px-3 py-2"
      >
        <p class="text-xs font-medium text-fg-muted">{t('post_edit.publication')}</p>
        <p class="mt-1 text-sm text-fg" data-testid="post-edit-publication-state">
          {post.draft ? t('post_edit.state_draft') : t('post_edit.state_published')}
        </p>
        {#if canPublish}
          <button
            type="button"
            onclick={togglePublication}
            disabled={publishBusy}
            data-testid="post-edit-publish-toggle"
            class="mt-2 rounded border border-border-strong px-3 py-1.5 text-sm hover:bg-surface disabled:cursor-not-allowed disabled:opacity-50"
          >
            {post.draft ? t('post_menu.publish_post') : t('post_menu.unpublish_post')}
          </button>
        {/if}
        <p class="mt-2 text-xs text-fg-muted">{t('post_edit.publication_note')}</p>
        {#if publishError}
          <p role="alert" class="mt-1 text-xs text-danger" data-testid="post-edit-publish-error">
            {publishError}
          </p>
        {/if}
      </section>
    </div>

    <!-- HOW THE POST LOOKS, AND WHERE IT SITS. The cover section and the
         membership list share the wide column: the first needs the room,
         and the second is a short list that reads better beside the
         marquee than under a narrow form. -->
    <div class="min-w-0 space-y-5">
      <section data-testid="post-edit-cover-section" aria-label={t('post_edit.cover')}>
        <p class="mb-2 text-xs font-medium text-fg-muted">{t('post_edit.cover')}</p>
        <PostCoverEditor post={seeded} bind:coverAssetId {framing} {viewportWidth} />
      </section>

      <section data-testid="post-edit-collections" aria-label={t('post_edit.collections')}>
        <p class="mb-2 text-xs font-medium text-fg-muted">{t('post_edit.collections')}</p>
        {#if membershipError}
          <p role="alert" class="text-xs text-danger" data-testid="post-edit-collections-error">
            {membershipError}
          </p>
        {/if}
        {#if membershipLoading}
          <p class="text-xs text-fg-muted" data-testid="post-edit-collections-loading">
            {t('post_edit.collections_loading')}
          </p>
        {:else if memberships.length === 0}
          <p class="text-xs text-fg-muted" data-testid="post-edit-collections-none">
            {t('post_edit.collections_none')}
          </p>
        {:else}
          <ul class="space-y-1.5">
            {#each memberships as m (m.collection.id)}
              <li
                class="flex items-start justify-between gap-3 rounded border border-border bg-surface px-3 py-2"
                data-testid="post-edit-membership"
                data-collection-id={m.collection.id}
                data-can-remove={m.can_remove ? 'yes' : 'no'}
              >
                <span class="min-w-0">
                  <a
                    href="/collections/{m.collection.id}"
                    class="block truncate text-sm text-fg hover:text-accent"
                  >{m.collection.name}</a>
                  <!-- WHY THE CONTROL IS OR IS NOT THERE, said out loud.
                       A row with no button and no sentence reads as a
                       rendering bug; this is the difference between
                       "you cannot" and "nothing happened". -->
                  {#if m.can_remove}
                    <span class="text-[10px] uppercase tracking-wide text-fg-muted"
                          data-testid="post-edit-membership-yours">
                      {t('post_edit.collection_owner_you')}
                    </span>
                  {:else}
                    <span class="block text-xs text-fg-muted"
                          data-testid="post-edit-membership-foreign">
                      {t('post_edit.collection_foreign')}
                    </span>
                  {/if}
                </span>
                {#if m.can_remove}
                  <button
                    type="button"
                    onclick={() => removeFrom(m.collection.id)}
                    disabled={removingId !== null}
                    data-testid="post-edit-membership-remove"
                    aria-label={t('post_edit.collection_remove_aria', { name: m.collection.name })}
                    class="shrink-0 rounded border border-border-strong px-2 py-1 text-xs hover:bg-surface-elevated disabled:cursor-not-allowed disabled:opacity-50"
                  >
                    {t('post_edit.collection_remove')}
                  </button>
                {/if}
              </li>
            {/each}
          </ul>
        {/if}
        <!-- THE REMAINDER, AS PROSE AND NOTHING ELSE. No id, no name, no
             curator, no control: the integer is the whole disclosure, and
             anything per-item here would turn it back into the
             collections it counted (#1237's shape on the asset side). -->
        {#if withheldCount > 0}
          <p class="mt-2 text-xs text-fg-muted" data-testid="post-edit-collections-withheld">
            {withheldCount === 1
              ? t('post_edit.collections_withheld_one')
              : t('post_edit.collections_withheld', { count: withheldCount })}
            <span class="block text-fg-muted/80">{t('post_edit.collections_withheld_why')}</span>
          </p>
        {/if}
      </section>
    </div>
  </div>

  {#snippet footer()}
    <!-- ONE COMMIT FOR THE FORM. Save applies the title, description,
         visibility, tags, cover and framing in a SINGLE
         `PATCH /posts/{id}` carrying `if_unchanged_since`. Publication
         and membership are not in it, deliberately: neither is this
         post's own data under this author's authority. -->
    <button
      type="button"
      onclick={onclose}
      data-testid="post-edit-cancel"
      class="rounded-md border border-border bg-surface px-3 py-1.5 text-sm hover:bg-surface-elevated"
    >
      {t('common.cancel')}
    </button>
    <button
      type="button"
      onclick={submit}
      disabled={submitting}
      data-testid="post-edit-save"
      class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-on-accent disabled:cursor-not-allowed disabled:bg-accent/40"
    >
      {submitting ? t('common.saving') : t('common.save')}
    </button>
  {/snippet}
</Modal>
