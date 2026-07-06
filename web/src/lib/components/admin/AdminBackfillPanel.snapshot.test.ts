// Snapshot suite for AdminBackfillPanel (Phase 1.16.B-followup-2,
// closes #186).
//
// Fifteen fixture-driven cells covering the (subsystem × state)
// matrix the three consuming admin pages together span:
//
//   - metadata-extraction / reindex / visual-backfill × 5 states
//     (empty / active-running / just-completed / just-cancelled /
//     just-failed)
//
// Each cell renders the component with fixture props that match the
// current shape each consumer would pass. The captured innerHTML
// becomes the compliance baseline for the retrofit — every subsequent
// commit that touches the component OR its consumers runs the suite
// and must produce identical bytes.
//
// Approach: Vitest DOM snapshots via @testing-library/svelte + happy-
// dom. Faster + more deterministic than Playwright pixel diffs; the
// codebase's Playwright infrastructure is standalone-run only (no
// visual-regression assertion pattern), so structural HTML diffing
// is the right compliance signal.

import { render } from '@testing-library/svelte';
import { describe, expect, it, beforeEach, vi, afterEach } from 'vitest';
import AdminBackfillPanel, { type BaseRun } from './AdminBackfillPanel.svelte';

// Freeze Date.now so relTime() returns deterministic bytes. Every
// fixture Run's started_at is anchored against this reference.
const NOW = new Date('2026-07-06T18:00:00.000Z').getTime();

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
});

afterEach(() => {
  vi.useRealTimers();
});

// ------------ shared fixture data ------------

function baseRun(overrides: Partial<BaseRun> = {}): BaseRun {
  return {
    id: '11111111-1111-1111-1111-111111111111',
    started_at: new Date(NOW - 5 * 60_000).toISOString(),
    processed: 42,
    succeeded: 40,
    failed: 2,
    isActive: false,
    completed_at: new Date(NOW - 60_000).toISOString(),
    cancelled_at: null,
    last_error: null,
    ...overrides,
  };
}

const activeRun = baseRun({
  id: '22222222-2222-2222-2222-222222222222',
  started_at: new Date(NOW - 30_000).toISOString(),
  isActive: true,
  completed_at: null,
  processed: 12,
  succeeded: 12,
  failed: 0,
});

const cancelledRun = baseRun({
  id: '33333333-3333-3333-3333-333333333333',
  isActive: false,
  cancelled_at: new Date(NOW - 30_000).toISOString(),
  completed_at: null,
});

const failedRun = baseRun({
  id: '44444444-4444-4444-4444-444444444444',
  isActive: false,
  completed_at: new Date(NOW - 30_000).toISOString(),
  last_error: 'sidecar unreachable',
  failed: 10,
  succeeded: 5,
});

// ------------ subsystem-shape helpers ------------

const noop = () => {};

/**
 * Baseline props for the metadata-extraction consumer's shape:
 * default `disableStartWhenActive=false` (the parent page can queue
 * runs while another is active), no gauges, no 503 gate, no extra
 * columns (Scope column is added by the consumer via
 * extraColumnHeaders + extraRowCells snippets).
 */
function metadataProps(runs: BaseRun[]) {
  return {
    title: 'Metadata extraction backfill',
    subtitle:
      'Re-extract metadata across every active image asset (or one asset type).',
    runs,
    loading: false,
    starting: false,
    error: '',
    onStart: noop,
    onRefresh: noop,
    onCancel: noop,
    startLabel: 'Start backfill',
    disableStartWhenActive: false,
    extraColumnCount: 1,
  };
}

function reindexProps(runs: BaseRun[]) {
  return {
    title: 'Search reindex',
    subtitle: 'Rebuild search vectors + re-enqueue embeddings across scoped assets.',
    runs,
    loading: false,
    starting: false,
    error: '',
    onStart: noop,
    onRefresh: noop,
    onCancel: noop,
    startLabel: 'Start reindex',
    startActiveLabel: 'Run active',
    disableStartWhenActive: true,
    extraColumnCount: 2,
  };
}

function visualProps(runs: BaseRun[]) {
  return {
    title: 'Visual-embedding backfill',
    subtitle:
      "Generate CLIP visual embeddings for image assets that don't yet have one.",
    runs,
    loading: false,
    starting: false,
    error: '',
    onStart: noop,
    onRefresh: noop,
    onCancel: noop,
    startLabel: 'Start backfill',
    startActiveLabel: 'Run active',
    disableStartWhenActive: true,
    extraColumnCountAfterFailed: 1,
  };
}

// ------------ helpers ------------

function normaliseHtml(html: string): string {
  // happy-dom emits some data-svelte-h="..." attributes for
  // component-boundary hydration hints. Strip them so snapshot
  // bytes don't shift when component composition changes.
  return html
    .replace(/\sdata-svelte-h="[^"]*"/g, '')
    .replace(/\s+data-testid=/g, ' data-testid=');
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function renderInnerHTML(component: typeof AdminBackfillPanel, props: any): string {
  const { container } = render(component, { props });
  return normaliseHtml(container.innerHTML);
}

// ------------ the fifteen cells ------------

describe('AdminBackfillPanel — snapshot compliance (Phase 1.16.B-followup-2)', () => {
  // ---- metadata-extraction × 5 ----

  it('metadata: empty state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, metadataProps([]));
    expect(html).toMatchSnapshot();
  });

  it('metadata: active-running state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, metadataProps([activeRun]));
    expect(html).toMatchSnapshot();
  });

  it('metadata: just-completed state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, metadataProps([baseRun()]));
    expect(html).toMatchSnapshot();
  });

  it('metadata: just-cancelled state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, metadataProps([cancelledRun]));
    expect(html).toMatchSnapshot();
  });

  it('metadata: just-failed state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, metadataProps([failedRun]));
    expect(html).toMatchSnapshot();
  });

  // ---- reindex × 5 ----

  it('reindex: empty state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, reindexProps([]));
    expect(html).toMatchSnapshot();
  });

  it('reindex: active-running state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, reindexProps([activeRun]));
    expect(html).toMatchSnapshot();
  });

  it('reindex: just-completed state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, reindexProps([baseRun()]));
    expect(html).toMatchSnapshot();
  });

  it('reindex: just-cancelled state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, reindexProps([cancelledRun]));
    expect(html).toMatchSnapshot();
  });

  it('reindex: just-failed state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, reindexProps([failedRun]));
    expect(html).toMatchSnapshot();
  });

  // ---- visual-backfill × 5 ----

  it('visual-backfill: empty state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, visualProps([]));
    expect(html).toMatchSnapshot();
  });

  it('visual-backfill: active-running state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, visualProps([activeRun]));
    expect(html).toMatchSnapshot();
  });

  it('visual-backfill: just-completed state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, visualProps([baseRun()]));
    expect(html).toMatchSnapshot();
  });

  it('visual-backfill: just-cancelled state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, visualProps([cancelledRun]));
    expect(html).toMatchSnapshot();
  });

  it('visual-backfill: just-failed state', () => {
    const html = renderInnerHTML(AdminBackfillPanel, visualProps([failedRun]));
    expect(html).toMatchSnapshot();
  });

  // ---- 503-gate state (visual-backfill's sidecar-not-registered) ----

  it('visual-backfill: sidecar not registered (Start disabled + warning)', () => {
    const props = visualProps([]);
    props.starting = false;
    (props as { startDisabledReason?: string; warning?: string }).startDisabledReason =
      'Visual encoder sidecar not registered.';
    (props as { warning?: string }).warning =
      'Sidecar not registered — enable search.visual.enabled in sysconfig.';
    const html = renderInnerHTML(AdminBackfillPanel, props);
    expect(html).toMatchSnapshot();
  });
});
