<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // ToolPanelShell — the asset viewer's right-pane chrome. Owns:
  //
  //   header — minimal: a small active-tool indicator (icon +
  //            name) on the left and a collapse chevron on the
  //            right. NO dropdown — the tool picker lives in the
  //            top menubar's Tools menu so the side panel keeps
  //            its width for actual tool content.
  //   body   — scroll area that mounts the active tool's Body.
  //   footer — pinned TipsSection rendering the active tool's
  //            Tips component. Standardised across every tool —
  //            no tool owns its own footer chrome.
  //
  // The shell never inspects the asset kind directly. Tools
  // advertise their own availability via ToolDef.isAvailable(ctx);
  // the shell filters + sorts + renders the one the caller picked.
  // Selection is driven externally via bind:activeToolId so the
  // menubar Tools menu can switch tools without the panel owning
  // any picker UI of its own. Adding a tool means appending to the
  // registry; the menubar surfaces it automatically.

  import { onMount, type Snippet } from 'svelte';
  import TipsSection from './TipsSection.svelte';
  import type { ToolContext, ToolDef } from './tools/contract';

  interface Props {
    /** Live tool context the shell threads into every tool. The
     *  shell re-evaluates isAvailable() whenever this changes. */
    ctx: ToolContext;
    /** Tools registered globally + any host-injected tools merged
     *  in by the caller. The shell sorts by .order, filters by
     *  isAvailable(ctx), and renders the active one. */
    tools: ToolDef[];
    /** Externally-controlled active tool id. Bindable so the
     *  menubar's Tools menu can flip it. When the bound id isn't
     *  available for the current asset (sprite-only id while
     *  viewing a video, etc.), the shell auto-falls-back to the
     *  first available tool by .order and writes that back. */
    activeToolId?: string;
    /** Pane open state — caller renders the collapsed rail
     *  separately (so the layout column animation lives in the
     *  caller, not split across two files). Bindable. */
    paneCollapsed?: boolean;
    onTogglePane: () => void;
    /** Compact-rail state — shell shrinks to an icon-rail when
     *  true AND the active tool advertises supportsCompact.
     *  Bindable so the caller can persist the preference. */
    paneCompact?: boolean;
    /** Optional snippet — caller renders nav hotkey legend (A/D,
     *  ←/→, etc.) appended below the active tool's tips inside
     *  the same TipsSection so the visual rhythm stays one footer. */
    extraTips?: Snippet;
  }

  let {
    ctx,
    tools,
    activeToolId = $bindable('details'),
    paneCollapsed = $bindable(false),
    paneCompact = $bindable(false),
    onTogglePane,
    extraTips,
  }: Props = $props();

  // Persist paneCompact per-tab. Caller can still bind through to
  // surface the value elsewhere (e.g. for analytics) but the shell
  // is the source-of-truth localStorage owner — same pattern as
  // activeToolId. Empty try/catch around localStorage so private-
  // browsing throws don't crash the shell.
  const COMPACT_KEY = 'aa.viewer.paneCompact';
  onMount(() => {
    try {
      if (localStorage.getItem(COMPACT_KEY) === '1') paneCompact = true;
    } catch { /* ignore */ }
  });
  $effect(() => {
    try {
      localStorage.setItem(COMPACT_KEY, paneCompact ? '1' : '0');
    } catch { /* ignore */ }
  });

  // Filter + sort once per ctx change. $derived so the dep graph
  // re-evaluates when sessions appear / disappear or the asset
  // swaps under us.
  const availableTools = $derived(
    tools.filter((t) => t.isAvailable(ctx)).sort((a, b) => a.order - b.order),
  );

  // Auto-fallback. Writes the corrected id back through the
  // binding so the menubar's checkmark + persistence stay in
  // sync with what the shell actually renders.
  $effect(() => {
    if (availableTools.length === 0) return;
    if (!availableTools.some((t) => t.id === activeToolId)) {
      activeToolId = availableTools[0].id;
    }
  });

  const activeTool = $derived(
    availableTools.find((t) => t.id === activeToolId) ?? availableTools[0],
  );

  // Tools that opt out of collapsing (Whiteboard — losing the
  // toolbox mid-stroke is bad UX) override paneCollapsed. The
  // full-collapse chevron is hidden for those.
  const collapseLocked = $derived(!!activeTool?.noCollapse);
  const effectiveCollapsed = $derived(paneCollapsed && !collapseLocked);
  // Compact only applies when the active tool advertises support.
  // Other tools ignore the flag — selecting them implicitly
  // restores full width.
  const compactActive = $derived(paneCompact && !!activeTool?.supportsCompact);

  // Decorate ctx with the active tool id (so SnippetBody /
  // SnippetTips can resolve their host snippet) AND with the
  // resolved compact flag on shellState so Body components can
  // switch their layout. Cloning shellState too so we don't
  // mutate the upstream object.
  const activeCtx = $derived(
    activeTool
      ? {
          ...ctx,
          activeToolId: activeTool.id,
          shellState: ctx.shellState
            ? { ...ctx.shellState, paneCompact: compactActive }
            : undefined,
        }
      : ctx,
  );
</script>

{#if effectiveCollapsed}
  <!-- Collapsed rail — single button to re-open. Kept inside the
       shell so the pane is always recoverable from the panel side
       (the menubar also has a Tools-menu "Show panel" entry the
       host wires separately). -->
  <aside class="flex h-full w-7 shrink-0 flex-col items-center border-l border-border bg-surface">
    <button
      type="button"
      onclick={onTogglePane}
      class="mt-2 inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-surface-elevated hover:text-fg"
      title="Expand panel"
      aria-label="Expand panel"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <path d="m15 18-6-6 6-6" />
      </svg>
    </button>
  </aside>
{:else if availableTools.length > 0}
  <aside
    class="flex h-full shrink-0 flex-col overflow-hidden border-l border-border bg-surface text-fg shadow-2xl"
    class:w-96={!compactActive}
    class:w-14={compactActive}
    class:max-w-[40vw]={!compactActive}
    aria-label="Asset tools"
  >
    <!-- Minimal header. Full-width: icon + active-tool name +
         right-side chevron. Compact-width: just the chevron centred
         so the rail can host body icons. The chevron's behaviour
         depends on the tool's flags:
           supportsCompact → toggles compact / full width
           !noCollapse + !supportsCompact → toggles full collapse
           noCollapse + !supportsCompact → chevron hidden -->
    <header
      class="flex shrink-0 items-center border-b border-border bg-surface-elevated"
      class:gap-2={!compactActive}
      class:px-3={!compactActive}
      class:px-1={compactActive}
      class:py-1.5={!compactActive}
      class:py-1={compactActive}
      class:justify-center={compactActive}
    >
      {#if !compactActive}
        {#if activeTool}
          {@const ActiveIcon = activeTool.Icon}
          {@const label = activeTool.labelFn ? activeTool.labelFn(activeCtx) : activeTool.label}
          <ActiveIcon ctx={activeCtx} />
          <span class="min-w-0 flex-1 truncate text-sm font-medium text-fg">{label}</span>
        {:else}
          <span class="min-w-0 flex-1 truncate text-sm text-fg-muted">No tool</span>
        {/if}
      {/if}
      {#if activeTool?.supportsCompact}
        <button
          type="button"
          onclick={() => (paneCompact = !paneCompact)}
          class="inline-flex h-7 w-7 shrink-0 items-center justify-center rounded text-fg-muted hover:bg-surface hover:text-fg"
          title={compactActive ? 'Expand panel' : 'Shrink to rail'}
          aria-label={compactActive ? 'Expand panel' : 'Shrink to rail'}
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            {#if compactActive}
              <path d="m15 18-6-6 6-6" />
            {:else}
              <path d="m9 18 6-6-6-6" />
            {/if}
          </svg>
        </button>
      {:else if !collapseLocked}
        <button
          type="button"
          onclick={onTogglePane}
          class="inline-flex h-7 w-7 shrink-0 items-center justify-center rounded text-fg-muted hover:bg-surface hover:text-fg"
          title="Collapse panel"
          aria-label="Collapse panel"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d="m9 18 6-6-6-6" />
          </svg>
        </button>
      {/if}
    </header>

    <!-- Body: scrolls. Each tool's Body owns its own padding +
         section dividers so the shell doesn't impose layout. -->
    <div class="min-h-0 flex-1 overflow-y-auto">
      {#if activeTool}
        {@const Body = activeTool.Body}
        <Body ctx={activeCtx} />
      {/if}
    </div>

    <!-- Footer: pinned tips. Renders any combination of the active
         tool's Tips + the host-provided extraTips (nav shortcuts).
         Hidden in compact mode (no room on the rail) AND when
         neither source has content. -->
    {#if !compactActive && (activeTool?.Tips || extraTips)}
      <TipsSection>
        {#if activeTool?.Tips}
          {@const ActiveTips = activeTool.Tips}
          <ActiveTips ctx={activeCtx} />
        {/if}
        {#if extraTips}{@render extraTips()}{/if}
      </TipsSection>
    {/if}
  </aside>
{/if}
