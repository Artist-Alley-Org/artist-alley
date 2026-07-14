<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  import { t } from '$stores/lang.svelte';
  import { site } from '$stores/site.svelte';
  import AdminSectionLanding from '$components/AdminSectionLanding.svelte';

  let { data } = $props();
</script>

<svelte:head><title>{t(`admin.sections.${data.section.slug}.title`)} — {site.name}</title></svelte:head>

<AdminSectionLanding section={data.section} />
