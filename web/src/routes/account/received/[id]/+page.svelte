<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /account/received/[id]: one asset another studio shared with this
  // instance (#1574). The record, its files, claims and download link are
  // ReceivedAssetView's. It is handed the route's id reactively: a
  // client navigation to another received asset reuses this page, and
  // the view clears the previous record and loads the new one.
  import { page } from '$app/state';
  import { site } from '$stores/site.svelte';
  import { t } from '$stores/lang.svelte';
  import ReceivedAssetView from '$lib/received/ReceivedAssetView.svelte';
</script>

<svelte:head><title>{t('account.received.title')} — {site.name}</title></svelte:head>

<p class="mb-4 text-sm">
  <a href="/account/received" class="text-accent hover:underline">{t('account.received.back')}</a>
</p>

<ReceivedAssetView id={page.params.id ?? ''} />
