<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import AccountStubPage from '$components/AccountStubPage.svelte';

  let { data } = $props();
</script>

<AccountStubPage slug={data.item.slug} />
