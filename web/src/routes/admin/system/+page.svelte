<script lang="ts">
  // /admin/system landing — same data-driven shape as every other
  // admin section. We pull the 'system' definition out of the shared
  // sections registry instead of duplicating tile lists.

  import { t } from '$stores/lang.svelte';
  import AdminSectionLanding from '$components/AdminSectionLanding.svelte';
  import { sectionBySlug } from '$lib/admin/sections';

  const section = sectionBySlug('system')!;
</script>

<svelte:head><title>{t('admin.sections.system.title')} — artist-alley</title></svelte:head>

<AdminSectionLanding {section} />
