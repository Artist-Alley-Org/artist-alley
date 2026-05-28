<script lang="ts">
  import { t } from '$stores/lang.svelte';
  import AdminSectionLanding from '$components/AdminSectionLanding.svelte';

  let { data } = $props();
</script>

<svelte:head><title>{t(`admin.sections.${data.section.slug}.title`)} — artist-alley</title></svelte:head>

<AdminSectionLanding section={data.section} />
