<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Standalone post page. Renders the same PostHost component that
  // the browse-feed overlay uses; the difference is the close
  // behavior (history.back() when entered in-app, else the browse feed
  // — see createCloseToOrigin) + standalone=true to swap the close
  // button for a back-arrow affordance.
  //
  // This route is hit by:
  //   - Shared / bookmarked URLs
  //   - Cmd/Ctrl+click on a PostCard (browser opens in a new tab,
  //     which boots the standalone page)
  //   - Direct paste into the address bar
  //
  // The /?post= overlay path (browse → modal) is preferred for
  // in-feed clicks since it preserves the feed underneath.

  import { page } from '$app/state';
  import { site } from '$stores/site.svelte';
  import PostHost from '$components/PostHost.svelte';
  import { t } from '$stores/lang.svelte';
  import { createCloseToOrigin } from '$lib/util/closeToOrigin.svelte';

  const postId = $derived(page.params.id ?? '');

  // Close policy shared with assets/[id] (#581): back to wherever the
  // user came from in-app, else the browse feed for a cold entry
  // (ADR 0067). One implementation so the two routes can't drift.
  const close = createCloseToOrigin();
</script>

<svelte:head>
  <title>{t('post.detail.title')} — {site.name}</title>
</svelte:head>

<PostHost {postId} onClose={close.handleClose} standalone />
