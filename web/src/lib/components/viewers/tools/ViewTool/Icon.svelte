<script lang="ts">
  // Magnifier-and-dot — reads as "zoom / view controls". Used for
  // both the 2D zoom-preset variant and the 3D render-controls
  // variant (same conceptual slot: "how I view this asset").
  import type { ToolContext } from '../contract';
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  let { ctx: _ctx }: { ctx: ToolContext } = $props();
</script>

<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
  <circle cx="11" cy="11" r="7" />
  <line x1="16.5" y1="16.5" x2="21" y2="21" />
  <line x1="11" y1="8" x2="11" y2="14" />
  <line x1="8" y1="11" x2="14" y2="11" />
</svg>
