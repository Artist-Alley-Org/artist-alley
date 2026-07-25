<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Standalone asset page (#475, ADR 0067). The asset-shaped twin of
  // posts/[id]: it renders the same AssetPlaylist shell the in-context
  // modal uses, here as a "playlist of 1" with no sibling context
  // (prev/next through a collection is a v0.6.0 follow-up via
  // CollectionHost). This route is what every /assets/{id} link —
  // collection tiles, search results, any AssetCard — resolves to, and
  // it makes asset URLs shareable + reload-safe.
  //
  // The +page.ts load has already fetched + 404-gated the asset; we
  // seed the source from it so first paint pays no extra round-trip.

  import { page } from '$app/state';
  import { site } from '$stores/site.svelte';
  import { t } from '$stores/lang.svelte';
  import AssetPlaylist from '$components/AssetPlaylist.svelte';
  import { createAssetPlaylistSource } from '$lib/playlist/assetSource.svelte';
  import { createCloseToOrigin } from '$lib/util/closeToOrigin.svelte';

  let { data } = $props();

  const assetId = $derived(page.params.id ?? '');

  // Seed from the load's asset. Recreate the source when the route
  // param changes (client-side nav between two /assets/{id} URLs) so
  // the viewer re-targets.
  let src = $derived.by(() => createAssetPlaylistSource(data.asset.id, data.asset));

  // Close policy shared with posts/[id] (#581): back to wherever the
  // user came from in-app, else the browse feed for a cold entry
  // (ADR 0067). One implementation so the two routes can't drift.
  const close = createCloseToOrigin();
</script>

<svelte:head>
  <title>{data.asset.title || t('common.untitled')} — {site.name}</title>
</svelte:head>

{#key assetId}
  <AssetPlaylist source={src.source} onClose={close.handleClose} standalone />
{/key}
