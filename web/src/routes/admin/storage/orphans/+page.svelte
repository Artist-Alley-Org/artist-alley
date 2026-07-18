<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/storage/orphans — the disk->DB sweep (#403).
  import { site } from '$stores/site.svelte';
  import StorageSweepPanel from '$components/admin/StorageSweepPanel.svelte';
</script>

<svelte:head><title>Orphaned objects — {site.name}</title></svelte:head>

<StorageSweepPanel
  kind="orphan_scan"
  title="Orphaned objects"
  intro="Finds bytes on the storage backend that nothing in the database references — silent waste. Reads object metadata only, so it is cheap to run."
/>
