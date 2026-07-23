<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Per-card hover/tap tool row (#515 slice 2). Quick actions without
  // opening the asset, shared by AssetCard + PostCard so browse, profile,
  // and collection grids all get the same row.
  //
  // It renders INSIDE CardThumb's children slot, layered ABOVE the card's
  // stretched navigation <a> (the cards moved to the stretched-link
  // pattern so these buttons aren't illegally nested in an anchor). The
  // buttons stopPropagation + preventDefault so a tool click never also
  // navigates the card.
  //
  // Actions:
  //   info  — link to the detail page (read-only, always shown).
  //   share — copy the absolute permalink to the clipboard, with brief
  //           "copied" feedback (mirrors ViewerMenuBar.copyLink). Read-only.
  //   add-to-collection — opens CollectionPicker. WRITE action, gated on
  //           logged-in AND not the read-only demo (site.demoMode). The
  //           demo blocks writes at the nginx edge (ADR 0060), so the tool
  //           is hidden there rather than shown as a 403 dead-end.
  //
  // Edit is intentionally absent this slice — there is no asset edit route
  // yet (follow-up). Zoom is absent — "open the viewer" is exactly the
  // card's default click, so a zoom tool would only duplicate it.

  import { auth } from '$stores/auth.svelte';
  import { site } from '$stores/site.svelte';
  import { t } from '$stores/lang.svelte';
  import CollectionPicker from './CollectionPicker.svelte';

  interface Props {
    /** Asset to add to a collection (cover asset for a Post). Null hides
     *  the add-to-collection tool. */
    assetId: string | null;
    /** Canonical detail path, e.g. `/assets/{id}` or `/posts/{id}`. Info
     *  navigates here; share copies `origin + detailPath`. */
    detailPath: string;
  }

  let { assetId, detailPath }: Props = $props();

  // Write tools show only for a logged-in user on a non-demo install.
  // (No dedicated content-write capability exists — collections gate on
  // auth + ownership — so this is the correct app-consistent gate.)
  const canWrite = $derived(!!auth.user && !site.demoMode);

  let copied = $state(false);
  let copyTimer: ReturnType<typeof setTimeout> | null = null;
  let pickerOpen = $state(false);

  async function share(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    const url = `${window.location.origin}${detailPath}`;
    try {
      await navigator.clipboard?.writeText(url);
      copied = true;
      if (copyTimer) clearTimeout(copyTimer);
      copyTimer = setTimeout(() => (copied = false), 1500);
    } catch {
      /* clipboard blocked (insecure context / permission) — no-op */
    }
  }

  function openPicker(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    pickerOpen = true;
  }
  function closePicker() {
    pickerOpen = false;
  }

  // Shared button chrome. The <button>/<a> is a 44px hit target (coarse
  // pointers) but the VISIBLE chip inside is 36px, so a row of three
  // doesn't blanket a narrow card's thumbnail. The chip is the styled
  // element; hover/focus on the control drives it.
  const btn =
    'pointer-events-auto group/tool inline-flex h-11 w-11 items-center justify-center ' +
    'focus-visible:outline-none';
  const chip =
    'flex h-9 w-9 items-center justify-center rounded-full bg-black/60 text-white backdrop-blur-sm ' +
    'transition-colors group-hover/tool:bg-black/80 group-focus-visible/tool:ring-2 group-focus-visible/tool:ring-white/80';
</script>

<!--
  Revealed on hover (fine pointers) + focus-within (keyboard); always shown
  on touch — hover isn't reachable there, so `(hover: none)` pins it open.
  z-20 sits above the stretched nav link + the title overlay.
-->
<div
  role="toolbar"
  aria-label={t('card.tools.row_label')}
  class="pointer-events-none absolute right-2 top-2 z-20 flex items-center gap-0.5 opacity-0
         transition-opacity duration-150 group-hover:opacity-100 focus-within:opacity-100
         [@media(hover:none)]:opacity-100"
>
  <!-- info -->
  <a href={detailPath} class={btn} aria-label={t('card.tools.info')} title={t('card.tools.info')}>
    <span class={chip}>
      <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <circle cx="12" cy="12" r="10" />
        <path d="M12 16v-4" />
        <path d="M12 8h.01" />
      </svg>
    </span>
  </a>

  <!-- share (copy permalink) -->
  <button
    type="button"
    onclick={share}
    class={btn}
    aria-label={copied ? t('card.tools.copied') : t('card.tools.share')}
    title={copied ? t('card.tools.copied') : t('card.tools.share')}
  >
    <span class={chip}>
      {#if copied}
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <polyline points="20 6 9 17 4 12" />
        </svg>
      {:else}
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M4 12v8a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-8" />
          <polyline points="16 6 12 2 8 6" />
          <line x1="12" y1="2" x2="12" y2="15" />
        </svg>
      {/if}
    </span>
  </button>

  <!-- add-to-collection (write; hidden for anon + read-only demo) -->
  {#if canWrite && assetId}
    <button
      type="button"
      onclick={openPicker}
      class={btn}
      aria-label={t('card.tools.add_to_collection')}
      title={t('card.tools.add_to_collection')}
    >
      <span class={chip}>
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M4 4h7l2 2h7a1 1 0 0 1 1 1v11a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1z" />
          <line x1="12" y1="11" x2="12" y2="17" />
          <line x1="9" y1="14" x2="15" y2="14" />
        </svg>
      </span>
    </button>
  {/if}
</div>

{#if pickerOpen && assetId}
  <CollectionPicker assetIds={[assetId]} onClose={closePicker} />
{/if}
