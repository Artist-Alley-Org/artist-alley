<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * AdminBackfillPanel — shared UX shell for admin backfill surfaces
   * (Phase 1.16.B-followup-2, closes #186).
   *
   * Consumed by three admin pages that shipped independently between
   * PRs #160, #182, #205:
   *
   *   - /admin/metadata-extraction/backfills
   *   - /admin/search/reindex
   *   - /admin/search/visual-backfill
   *
   * Component is deliberately API-agnostic: no fetch() calls, no
   * subsystem-specific branches. Parents own the API layer; the
   * component consumes typed callbacks (onStart / onCancel / onRefresh)
   * + already-loaded state (runs / loading / error).
   *
   * Divergences between the three current pages are handled via
   * snippets + optional props — NOT via `{#if page === 'X'}`
   * conditionals inside the component. If a future subsystem needs
   * something the component doesn't expose, grow the snippet API,
   * don't leak subsystem knowledge inward.
   *
   * BaseRun type below is the row shape the base columns render.
   * Extra columns per-subsystem (Scope / Target / Progress %) come
   * in via extraColumnHeaders + extraRowCells snippets; callers
   * pass their own richer Run type + the snippets typecast inside.
   */

  import type { Snippet } from 'svelte';

  export type BaseRun = {
    id: string;
    started_at: string;
    processed: number;
    succeeded: number;
    failed: number;
    /**
     * True while the run is neither completed nor cancelled. Parents
     * compute this from whatever their backend surfaces (metadata
     * derives from timestamps; reindex + visual read a boolean
     * field). Kept explicit in the base type so the component
     * doesn't have to know each backend's shape.
     */
    isActive: boolean;
    /** Terminal timestamps used to classify the status badge. */
    completed_at?: string | null;
    cancelled_at?: string | null;
    /**
     * When set on a completed run, the row's status becomes "failed"
     * (danger colour class). Currently only visual-backfill surfaces
     * this; the other two treat every terminal state as "done".
     */
    last_error?: string | null;
  };

  interface Props {
    /** H1 heading text. */
    title: string;
    /**
     * Optional prose subtitle rendered under the H1. For rich content
     * with links or `<code>` tags, use the `headerDescription` snippet
     * instead — string is plain text only.
     */
    subtitle?: string;
    /** Richer subtitle content; overrides the plain-text `subtitle`. */
    headerDescription?: Snippet;

    /** Runs to render in the recent-runs table. */
    runs: BaseRun[];
    /** True while a load or refresh is in flight (disables Refresh). */
    loading: boolean;
    /** True while a start POST is in flight (disables Start). */
    starting: boolean;
    /** Error text shown in the red banner. Empty = hidden. */
    error: string;

    /** Start button click handler. Required. */
    onStart: () => void | Promise<void>;
    /** Refresh button click handler. Required. */
    onRefresh: () => void | Promise<void>;
    /**
     * Cancel handler for one active run. Optional; when omitted, the
     * Actions column stays but the Cancel button never renders.
     */
    onCancel?: (run: BaseRun) => void | Promise<void>;

    /** Label on the Start button. Default: "Start". */
    startLabel?: string;
    /** Label while starting. Default: "Starting…". */
    startingLabel?: string;
    /**
     * When non-empty, the Start button is disabled + the string is
     * shown as its title (hover tooltip). Used by visual-backfill for
     * the sidecar-not-registered case (PR #205).
     */
    startDisabledReason?: string;
    /**
     * When true (default), the Start button is also disabled while
     * any run is active. Reindex + visual-backfill rely on this
     * ("Run active" label appears); metadata-extraction can queue
     * runs while another is active + sets this to false.
     */
    disableStartWhenActive?: boolean;
    /** Label to show on Start when disabled by an active run. */
    startActiveLabel?: string;

    /**
     * Extra table column headers (as <th> elements) appended after
     * the "Started" column, before Processed/Succeeded/Failed. Each
     * subsystem uses this differently:
     *   - metadata: Scope
     *   - reindex: Scope + Target
     *   - visual-backfill: (none; uses extraRowCellsAfterFailed instead)
     */
    extraColumnHeaders?: Snippet;
    /** Matching <td> cells for extraColumnHeaders. */
    extraRowCells?: Snippet<[BaseRun]>;

    /**
     * Extra <th> cells appended AFTER "Failed" (before Actions).
     * Used by visual-backfill's Progress column.
     */
    extraColumnHeadersAfterFailed?: Snippet;
    /** Matching <td> cells for extraColumnHeadersAfterFailed. */
    extraRowCellsAfterFailed?: Snippet<[BaseRun]>;

    /**
     * Top-of-page gauge tiles. Currently only visual-backfill uses
     * this; when the fourth backfill surface lands with its own
     * gauges, it drops in without further component changes.
     */
    gauges?: Snippet;

    /**
     * Extra controls rendered inside the "Start a run" section
     * before the Start button. Used for form inputs — asset-type
     * filter (metadata), scope/target picker (reindex), etc.
     */
    controls?: Snippet;

    /** Warning text rendered in the Start section (e.g. sidecar not registered). */
    warning?: string;

    /** Confirmation message shown before invoking onCancel. */
    cancelConfirmMessage?: (run: BaseRun) => string;

    /** Empty-state row text. Default: "No runs yet." */
    emptyText?: string;

    /**
     * Hint: how many <th> cells the extraColumnHeaders snippet
     * emits. Used only for empty-state row colspan. Svelte's
     * templating doesn't tell us the count of a rendered snippet
     * so callers pass a hint.
     */
    extraColumnCount?: number;
    /** Same but for extraColumnHeadersAfterFailed. */
    extraColumnCountAfterFailed?: number;
  }

  let {
    title,
    subtitle = '',
    headerDescription,
    runs,
    loading,
    starting,
    error,
    onStart,
    onRefresh,
    onCancel,
    startLabel = 'Start',
    startingLabel = 'Starting…',
    startDisabledReason = '',
    disableStartWhenActive = true,
    startActiveLabel = 'Run active',
    extraColumnHeaders,
    extraRowCells,
    extraColumnHeadersAfterFailed,
    extraRowCellsAfterFailed,
    gauges,
    controls,
    warning = '',
    cancelConfirmMessage,
    emptyText = 'No runs yet.',
    extraColumnCount = 0,
    extraColumnCountAfterFailed = 0,
  }: Props = $props();

  const anyActive = $derived(runs.some((r) => r.isActive));

  const startDisabled = $derived(
    starting ||
      startDisabledReason.length > 0 ||
      (disableStartWhenActive && anyActive)
  );

  const startButtonLabel = $derived.by(() => {
    if (starting) return startingLabel;
    if (disableStartWhenActive && anyActive) return startActiveLabel;
    return startLabel;
  });

  /**
   * Column count for the empty-state colspan. Sums the fixed base
   * columns (Status, Started, Processed, Succeeded, Failed, Actions
   * = 6) with the caller's extra-column snippets. When Cancel is
   * unsupported, the Actions column still renders (empty), so the
   * count doesn't shift.
   *
   * Callers that render N extraColumnHeaders/extraColumnHeadersAfterFailed
   * `<th>` cells need the base count bumped by N. Because Svelte
   * doesn't tell us how many <th> cells a snippet emits, callers
   * pass a hint via extraColumnCount + extraColumnCountAfterFailed.
   */

  const baseColumns = 6;
  const emptyColspan = $derived(
    baseColumns + extraColumnCount + extraColumnCountAfterFailed
  );

  function statusLabel(r: BaseRun): string {
    if (r.cancelled_at) return 'cancelled';
    if (r.completed_at) return r.last_error ? 'failed' : 'done';
    return 'running';
  }

  function statusClass(r: BaseRun): string {
    if (r.cancelled_at) return 'bg-fg-muted/15 text-fg-muted border-fg-muted/40';
    if (r.completed_at)
      return r.last_error
        ? 'bg-danger/15 text-danger border-danger/40'
        : 'bg-success/15 text-success border-success/40';
    return 'bg-info/15 text-info border-info/40';
  }

  function relTime(s: string): string {
    const ms = Date.now() - new Date(s).getTime();
    if (ms < 60_000) return 'just now';
    const m = Math.floor(ms / 60_000);
    if (m < 60) return `${m}m ago`;
    const h = Math.floor(m / 60);
    if (h < 24) return `${h}h ago`;
    return `${Math.floor(h / 24)}d ago`;
  }

  async function handleCancel(run: BaseRun) {
    if (!onCancel) return;
    if (cancelConfirmMessage) {
      const msg = cancelConfirmMessage(run);
      // eslint-disable-next-line no-alert
      if (!confirm(msg)) return;
    }
    await onCancel(run);
  }
</script>

<header class="mb-4">
  <h1 class="font-display mb-2 text-2xl font-semibold">{title}</h1>
  {#if headerDescription}
    {@render headerDescription()}
  {:else if subtitle}
    <p class="text-sm text-fg-muted">{subtitle}</p>
  {/if}
</header>

{#if gauges}
  <section class="mb-6 grid grid-cols-1 gap-3 sm:grid-cols-3" data-testid="backfill-gauges">
    {@render gauges()}
  </section>
{/if}

<section class="mb-6 rounded border border-border bg-bg-soft p-4" data-testid="backfill-controls">
  <h2 class="mb-3 text-xs font-semibold uppercase tracking-wide text-fg-muted">Start a run</h2>
  <div class="flex flex-wrap items-end gap-3">
    {#if controls}
      {@render controls()}
    {/if}
    <button
      onclick={onStart}
      disabled={startDisabled}
      title={startDisabledReason || undefined}
      class="rounded bg-accent px-3 py-1.5 text-sm font-medium text-accent-fg disabled:opacity-50"
      data-testid="backfill-start"
    >{startButtonLabel}</button>
    <button
      onclick={onRefresh}
      disabled={loading}
      class="rounded border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-bg disabled:opacity-50"
      data-testid="backfill-refresh"
    >{loading ? 'Loading…' : 'Refresh'}</button>
    {#if warning}
      <span class="text-xs text-danger" data-testid="backfill-warning">{warning}</span>
    {/if}
  </div>
  {#if error}
    <div class="mt-3 rounded border border-danger/40 bg-danger/10 p-3 text-sm text-danger" data-testid="backfill-error">{error}</div>
  {/if}
</section>

<section class="rounded border border-border bg-bg-soft" data-testid="backfill-runs">
  <header class="border-b border-border px-3 py-2 text-sm font-medium text-fg-muted">
    Recent runs ({runs.length})
  </header>
  <table class="w-full text-sm">
    <thead class="border-b border-border bg-bg/60 text-fg-muted">
      <tr>
        <th class="px-3 py-2 text-left font-medium">Status</th>
        <th class="px-3 py-2 text-left font-medium">Started</th>
        {#if extraColumnHeaders}
          {@render extraColumnHeaders()}
        {/if}
        <th class="px-3 py-2 text-right font-medium">Processed</th>
        <th class="px-3 py-2 text-right font-medium">Succeeded</th>
        <th class="px-3 py-2 text-right font-medium">Failed</th>
        {#if extraColumnHeadersAfterFailed}
          {@render extraColumnHeadersAfterFailed()}
        {/if}
        <th class="px-3 py-2 text-right font-medium">Actions</th>
      </tr>
    </thead>
    <tbody>
      {#each runs as r (r.id)}
        <tr class="border-b border-border/40 last:border-0">
          <td class="px-3 py-2">
            <span class="inline-flex items-center rounded border px-2 py-0.5 text-xs font-medium {statusClass(r)}">{statusLabel(r)}</span>
          </td>
          <td class="px-3 py-2 text-fg-muted" title={r.started_at}>{relTime(r.started_at)}</td>
          {#if extraRowCells}
            {@render extraRowCells(r)}
          {/if}
          <td class="px-3 py-2 text-right tabular-nums">{r.processed}</td>
          <td class="px-3 py-2 text-right tabular-nums text-success">{r.succeeded}</td>
          <td class="px-3 py-2 text-right tabular-nums {r.failed > 0 ? 'text-danger' : 'text-fg-muted'}">{r.failed}</td>
          {#if extraRowCellsAfterFailed}
            {@render extraRowCellsAfterFailed(r)}
          {/if}
          <td class="px-3 py-2 text-right">
            {#if r.isActive && onCancel}
              <button
                onclick={() => handleCancel(r)}
                class="rounded border border-danger/60 px-2 py-1 text-xs text-danger hover:bg-danger hover:text-on-danger"
                data-testid="backfill-cancel"
              >Cancel</button>
            {/if}
          </td>
        </tr>
      {/each}
      {#if runs.length === 0 && !loading}
        <tr>
          <td colspan={emptyColspan} class="px-3 py-6 text-center text-fg-muted" data-testid="backfill-empty">{emptyText}</td>
        </tr>
      {/if}
    </tbody>
  </table>
</section>
