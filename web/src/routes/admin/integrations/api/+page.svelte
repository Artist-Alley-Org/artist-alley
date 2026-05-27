<script lang="ts">
  // /admin/integrations/api — Scalar API Explorer.
  //
  // Mounts the Scalar reference UI pointed at our canonical
  // OpenAPI spec (served by /openapi.yaml/+server.ts which reads
  // the file mounted into the web container). The reference
  // renders interactive endpoint browser + Try-it-out, themed to
  // match our dark / light toggle via the `darkMode` prop.

  import { onMount, onDestroy } from 'svelte';
  import { t } from '$stores/lang.svelte';
  import { theme } from '$stores/theme.svelte';

  let container: HTMLDivElement | undefined = $state();
  let mounted = false;
  // Loose type: Scalar's surface varies across versions and we only
  // call a handful of methods. Tight typing here would bind us to
  // internals.
  type ScalarHandle = {
    unmount?: () => void;
    destroy?: () => void;
    updateConfiguration?: (cfg: Record<string, unknown>) => void;
  };
  let scalar: ScalarHandle | undefined;

  onMount(async () => {
    if (!container) return;
    // Side-effect import: pulls in Scalar's Vue runtime + styles.
    // Done dynamically so it never blocks SSR or shows up in the
    // critical path for other pages.
    await import('@scalar/api-reference/style.css');
    const { createApiReference } = await import('@scalar/api-reference');
    scalar = (createApiReference as unknown as (
      target: HTMLElement,
      config: Record<string, unknown>,
    ) => ScalarHandle)(container, {
      url: '/openapi.yaml',
      darkMode: theme.resolved === 'dark',
      hideClientButton: false,
      // Self-hosted; no telemetry beacon.
      proxyUrl: undefined,
    });
    mounted = true;
  });

  // Track theme changes so Scalar's light/dark mode follows the app.
  $effect(() => {
    if (!mounted || !scalar?.updateConfiguration) return;
    scalar.updateConfiguration({ darkMode: theme.resolved === 'dark' });
  });

  onDestroy(() => {
    scalar?.unmount?.();
    scalar?.destroy?.();
  });
</script>

<svelte:head><title>{t('admin.api_explorer.title')} — artist-alley</title></svelte:head>

<header class="mb-4">
  <h2 class="text-xl font-semibold">{t('admin.api_explorer.title')}</h2>
  <p class="mt-1 text-sm text-fg-muted">{t('admin.api_explorer.intro')}</p>
</header>

<!-- Scalar mounts inside this container. Sized to fill the admin
     content column; let Scalar manage its own internal scroll. -->
<div
  bind:this={container}
  class="scalar-host h-[calc(100vh-16rem)] min-h-[40rem] overflow-hidden rounded-lg border border-border bg-surface"
></div>

<style>
  /* Scalar ships its own design system that bleeds into descendant
     elements via custom properties. Scope a couple of overrides so
     the embed sits cleanly inside our admin chrome. */
  :global(.scalar-host .scalar-api-reference) {
    height: 100%;
  }
</style>
