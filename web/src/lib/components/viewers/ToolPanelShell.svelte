<script lang="ts">
  // ToolPanelShell — the asset viewer's right-pane chrome. Owns:
  //
  //   header  — active-tool dropdown (uses the Menu primitive every
  //             other dropdown in the app uses) + collapse button.
  //   body    — scroll area that mounts the active tool's Body.
  //   footer  — pinned TipsSection rendering the active tool's Tips
  //             snippet. Standardised across every tool — no tool
  //             owns its own footer chrome.
  //
  // The shell never inspects the asset kind directly. Tools advertise
  // their own availability via ToolDef.isAvailable(ctx); the shell
  // filters + sorts + renders. Adding a tool means appending to the
  // registry, not editing this file.
  //
  // The selected tool persists in localStorage per-tab so a user who
  // prefers Sprite over Details on sprite assets gets their pick on
  // every subsequent sprite. The persisted id is global (not per-
  // asset): the user picks a workflow, not a per-file preference.
  // When the persisted id isn't available for the current asset
  // (e.g. selected "sprite" then navigates to a video), the shell
  // falls back to the first available tool by .order.

  import { onMount, type Snippet } from 'svelte';
  import Menu from '$lib/components/Menu.svelte';
  import TipsSection from './TipsSection.svelte';
  import type { ToolContext, ToolDef } from './tools/contract';

  interface Props {
    /** Live tool context the shell threads into every tool. The
     *  shell re-evaluates isAvailable() whenever this changes. */
    ctx: ToolContext;
    /** Tools registered globally + any host-injected tools merged
     *  in by the caller. The shell sorts by .order, filters by
     *  isAvailable(ctx), and renders. */
    tools: ToolDef[];
    /** Pane open state — shell renders a small chevron header on
     *  collapse so the user can re-open. Bindable so the caller can
     *  persist the preference. */
    paneCollapsed?: boolean;
    onTogglePane: () => void;
    /** Optional shell-owned snippet — used by AssetViewer to render
     *  navigation hotkey legend (A/D, ←/→) below the tool's own
     *  tips. Lives inside the same TipsSection so the visual rhythm
     *  stays one footer block. */
    extraTips?: Snippet;
  }

  let { ctx, tools, paneCollapsed = $bindable(false), onTogglePane, extraTips }: Props = $props();

  const SELECTED_TOOL_KEY = 'aa.viewer.activeTool';

  // Filter + sort once per ctx change. $derived so component
  // re-evaluates when sessions appear / disappear or the asset
  // swaps under us.
  const availableTools = $derived(
    tools.filter((t) => t.isAvailable(ctx)).sort((a, b) => a.order - b.order),
  );

  let selectedId = $state<string>('details');

  onMount(() => {
    try {
      const stored = localStorage.getItem(SELECTED_TOOL_KEY);
      if (stored) selectedId = stored;
    } catch {
      /* localStorage can throw in private-browsing — ignore. */
    }
  });

  // Auto-fallback when the persisted tool isn't available for the
  // current asset. Pick the first available (which, by order, is
  // Details).
  $effect(() => {
    if (availableTools.length === 0) return;
    if (!availableTools.some((t) => t.id === selectedId)) {
      selectedId = availableTools[0].id;
    }
  });

  const activeTool = $derived(availableTools.find((t) => t.id === selectedId) ?? availableTools[0]);

  function selectTool(id: string) {
    selectedId = id;
    try {
      localStorage.setItem(SELECTED_TOOL_KEY, id);
    } catch {
      /* ignore — see onMount note */
    }
  }
</script>

{#if paneCollapsed}
  <!-- Collapsed rail — single button to re-open. The caller decides
       whether to render the shell at all (some viewers don't show a
       panel for placeholder kinds); when shown collapsed, we render
       this thin handle so the pane is always recoverable. -->
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
    class="flex h-full w-96 max-w-[40vw] shrink-0 flex-col overflow-hidden border-l border-border bg-surface text-fg shadow-2xl"
    aria-label="Asset tools"
  >
    <header class="flex shrink-0 items-center gap-1 border-b border-border bg-surface-elevated px-2 py-1.5">
      <!-- Active-tool dropdown. The trigger is the tool's label +
           chevron; menu items are every available tool, with a check
           on the active one. Mirrors the menubar Tools dropdown UX
           so the muscle memory transfers. -->
      <div class="min-w-0 flex-1">
        <Menu align="left" panelClass="min-w-[10rem]">
          {#snippet trigger({ open })}
            <span
              class={`inline-flex h-7 w-full items-center gap-1 rounded px-2 text-sm font-medium text-fg hover:bg-white/5 ${open ? 'bg-white/5' : ''}`}
              title="Switch tool"
            >
              {#if activeTool}
                {@const ActiveIcon = activeTool.Icon}
                <ActiveIcon {ctx} />
                <span class="truncate">{activeTool.label}</span>
              {:else}
                <span class="text-fg-muted">No tool</span>
              {/if}
              <svg class="ml-auto shrink-0" xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <path d="m6 9 6 6 6-6" />
              </svg>
            </span>
          {/snippet}
          {#each availableTools as t (t.id)}
            {@const ItemIcon = t.Icon}
            <button
              type="button"
              role="menuitem"
              onclick={() => selectTool(t.id)}
              class="flex w-full items-center justify-between gap-2 px-3 py-1.5 text-left text-sm text-fg hover:bg-surface-elevated"
            >
              <span class="inline-flex items-center gap-2">
                <ItemIcon {ctx} />
                <span>{t.label}</span>
              </span>
              {#if t.id === activeTool?.id}
                <span class="text-accent" aria-label="Active">●</span>
              {/if}
            </button>
          {/each}
        </Menu>
      </div>
      <button
        type="button"
        onclick={onTogglePane}
        class="inline-flex h-7 w-7 items-center justify-center rounded text-fg-muted hover:bg-surface-elevated hover:text-fg"
        title="Collapse panel"
        aria-label="Collapse panel"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="m9 18 6-6-6-6" />
        </svg>
      </button>
    </header>

    <!-- Body: scrolls. Each tool's Body owns its own padding +
         section dividers so the shell doesn't impose layout. -->
    <div class="min-h-0 flex-1 overflow-y-auto">
      {#if activeTool}
        {@const Body = activeTool.Body}
        <Body {ctx} />
      {/if}
    </div>

    <!-- Footer: pinned tips. Renders any combination of the active
         tool's Tips snippet + the shell-provided extraTips (nav
         shortcuts the host owns). When both are absent the section
         collapses to nothing so an empty footer doesn't waste rows. -->
    {#if activeTool?.Tips || extraTips}
      <TipsSection>
        {#if activeTool?.Tips}
          {@const ActiveTips = activeTool.Tips}
          <ActiveTips {ctx} />
        {/if}
        {#if extraTips}{@render extraTips()}{/if}
      </TipsSection>
    {/if}
  </aside>
{/if}
