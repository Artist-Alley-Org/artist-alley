<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Small circular avatar — image when available, falls back to a
  // colored disc with the user's initials. Single source of truth so
  // the navbar, post sidebar, and menu trigger render identically.

  interface Props {
    name: string;
    src?: string | null;
    /** Tailwind size class, e.g. `h-8 w-8`. Default: `h-8 w-8`. */
    sizeClass?: string;
    /** Optional alt text override. Default: `name`. */
    alt?: string;
  }

  let { name, src, sizeClass = 'h-8 w-8', alt }: Props = $props();

  const initials = $derived.by(() => {
    const parts = name.trim().split(/\s+/).slice(0, 2);
    return parts.map((p) => p[0]?.toUpperCase() ?? '').join('') || '?';
  });

  let imgError = $state(false);
</script>

{#if src && !imgError}
  <img
    {src}
    alt={alt ?? name}
    class={`${sizeClass} shrink-0 rounded-full object-cover`}
    onerror={() => (imgError = true)}
  />
{:else}
  <span
    class={`${sizeClass} inline-flex shrink-0 items-center justify-center rounded-full bg-accent/20 text-xs font-semibold text-accent`}
    aria-hidden="true"
  >
    {initials}
  </span>
{/if}
