<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/storage/checksums — the DB->disk sweep (#403).
  import { site } from '$stores/site.svelte';
  import StorageSweepPanel from '$components/admin/StorageSweepPanel.svelte';
</script>

<svelte:head><title>Checksum verification — {site.name}</title></svelte:head>

<StorageSweepPanel
  kind="checksum_verify"
  title="Checksum verification"
  intro="Confirms every object the database expects is present on the backend and that its bytes still hash to the key they are stored under. Storage is content-addressed, so the hash is the expected checksum."
  costWarning={'This sweep reads EVERY BYTE of every stored object, so it is slow and I/O heavy — roughly 26 seconds for 21,000 variants on a dev install, and proportionally longer on a large one.\n\nPrefer an off-peak window on a production instance.'}
/>
