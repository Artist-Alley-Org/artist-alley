<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // "Where is this file used" — the OWNER's view (#1237, ADR 0091
  // decision 5).
  //
  // An asset is personal storage until it is posted. Its owner therefore
  // has a question nothing else in the product answers: where has my
  // file ended up, including in posts other people wrote — which a
  // shared team library makes ordinary. `GET /assets/{id}/posts` answers
  // it; until this page there was no caller.
  //
  // # Why this is NOT /posts/by-asset/{id}
  //
  // That route asks the READER's question — which posts featuring this
  // asset may I read — and a post the caller may not read is simply
  // ABSENT there. To a reader that is the right answer. To an owner it
  // is the wrong one: it says their file is nowhere.
  //
  // So this page renders the same posts through the same PostCard, and
  // then states the REST as a count. The two halves are the disclosure
  // rule made visible: everything you are entitled to arrives whole, and
  // everything you are not is one integer.
  //
  // ⚠️ THE COUNT IS PROSE, AND THAT IS A CONTRACT, NOT A LAYOUT CHOICE.
  // No placeholder cards, nothing clickable, focusable or hoverable, and
  // NO PER-ITEM ELEMENT of any kind. The placeholder pattern #883 uses
  // elsewhere is for rows that have a handle — an id the reader may ask
  // about — and this count deliberately has none: the API returns no
  // ids, no titles, no authors, no timestamps and no cursor, because any
  // of them would let the integer be walked back into the posts behind
  // it (#902's count-leak shape). Rendering N somethings would rebuild
  // the enumeration the API refused, in the DOM, where a scraper reads
  // it just as well.
  //
  // # No redirect on a single item
  //
  // /posts/by-asset/{id} goes straight to the post when exactly one is
  // visible (ADR 0070 §4). This page must not, and the reason is the
  // case that looks most like it: ONE readable post beside a positive
  // withheld count is exactly when the sentence below is the thing the
  // owner came for. Redirecting past it would answer "your file is in
  // this post" when the truth is "this post, and four you cannot see".

  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { t } from '$stores/lang.svelte';
  import { browseView } from '$stores/browseView.svelte';
  import ViewControls from '$components/ViewControls.svelte';
  import ContentGrid from '$components/ContentGrid.svelte';
  import PostCard from '$components/PostCard.svelte';
  import PostListTable from '$components/PostListTable.svelte';
  import PostParamHost from '$components/PostParamHost.svelte';
  import type { PageData } from './$types';

  let { data }: { data: PageData } = $props();

  // Both halves come off the load, so there is no loading state to
  // render and no window in which the page is blank. The redirect the
  // sibling route makes is what forced ITS fetch into onMount.
  const posts = $derived((data.usage.items ?? []) as any[]);
  const withheld = $derived(data.usage.withheld_count ?? 0);
  const assetTitle = $derived((data.asset.title ?? '').trim());

  // Sort is honoured client-side by reversing the (bounded) result — the
  // server returns newest-first. Same coupling to the ONE global browse
  // preference the sibling route has, deliberately: a viewer who set
  // their grid to lists on browse should not find a different chrome
  // here.
  const sortedPosts = $derived(
    browseView.feedDir === 'asc' ? [...posts].reverse() : posts,
  );

  // onMount and not $effect, matching the sibling route. `init()` reads
  // `hydrated` and `auth.user` and then writes several $state fields, so
  // inside an effect it would register both as dependencies and re-run
  // on its own write and again when the session resolves. It is
  // self-guarding, so that terminates rather than loops — but "reads
  // state inside a callee, then writes it" is the exact shape that cost
  // a 1,694-request storm elsewhere, and this needs to happen once.
  onMount(() => {
    browseView.init(); // inherit the user's view + tile-size preference
  });

  // The withheld sentence has two forms, and they are different
  // sentences rather than one with a conditional prefix. "It is ALSO in
  // 3 posts you cannot open" only parses when something was shown; with
  // an empty grid above it, "also" refers to nothing and the page reads
  // as though it failed to load. The all-withheld state is the one most
  // likely to render wrong, so it gets its own words.
  const withheldText = $derived(
    posts.length === 0
      ? withheld === 1
        ? t('asset_usage.withheld_only_one')
        : t('asset_usage.withheld_only', { count: String(withheld) })
      : withheld === 1
        ? t('asset_usage.withheld_one')
        : t('asset_usage.withheld', { count: String(withheld) }),
  );
</script>

<svelte:head>
  <title>{t('asset_usage.title')} — Artist Alley</title>
</svelte:head>

<div class="w-full px-4 py-8 sm:px-6">
  <h1 class="text-xl font-bold text-fg" data-testid="asset-usage-heading">
    {t('asset_usage.heading')}
  </h1>
  <p class="mt-1 text-sm text-fg-muted" data-testid="asset-usage-sub">
    {assetTitle
      ? t('asset_usage.sub', { title: assetTitle })
      : t('asset_usage.sub_untitled')}
  </p>
  <a
    href="/assets/{page.params.id}"
    class="mt-2 inline-block text-sm text-accent hover:underline"
    data-testid="asset-usage-back">{t('asset_usage.back')}</a
  >

  {#if posts.length > 0}
    <div class="mt-6">
      <ContentGrid mode={browseView.mode} items={sortedPosts} tileMin={browseView.tileMin}>
        {#snippet card(item, mode)}
          <PostCard post={item} {mode} feed={mode === 'feed'} tileSizes={browseView.tileSizes} />
        {/snippet}
        {#snippet list()}
          <PostListTable items={sortedPosts} loading={false} />
        {/snippet}
      </ContentGrid>
    </div>
  {/if}

  <!-- The withheld remainder. ONE <p>, however large the number: see the
       contract note at the top of this file. It is deliberately not
       inside the grid — a grid is a list of things, and this is not a
       list of anything. -->
  {#if withheld > 0}
    <p class="mt-6 text-sm text-fg" data-testid="asset-usage-withheld">
      {withheldText}
    </p>
    <p class="mt-1 text-sm text-fg-muted" data-testid="asset-usage-withheld-why">
      {t('asset_usage.withheld_why')}
    </p>
  {/if}

  <!-- The zero state: in no post at all, which is not an error and is a
       perfectly ordinary thing for a file to be. Distinct from
       all-withheld above, which prints a count instead. -->
  {#if posts.length === 0 && withheld === 0}
    <p class="mt-6 text-fg-muted" data-testid="asset-usage-none">{t('asset_usage.none')}</p>
    <p class="mt-1 text-sm text-fg-muted">{t('asset_usage.none_hint')}</p>
  {/if}
</div>

{#if posts.length > 0}
  <ViewControls />
{/if}

<!-- #1130's sweep. PostCard and PostListTable both write `?post=` onto
     this url; without a host the click dead-ends here exactly as it did
     on the collection route. `ordered` is the READABLE result only — the
     withheld posts have no ids to walk to, which is the point. -->
<PostParamHost ordered={() => posts.map((p) => p.id)} />
