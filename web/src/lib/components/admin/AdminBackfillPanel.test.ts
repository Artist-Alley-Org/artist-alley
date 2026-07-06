// Semantic unit tests for AdminBackfillPanel (Phase 1.16.B-followup-2).
//
// Snapshot suite (AdminBackfillPanel.snapshot.test.ts) catches
// structural regressions in the rendered HTML. These tests catch
// interaction semantics — Start disabled tooltip fires, Cancel
// invokes the callback with the right run, empty state renders
// with the expected colspan, etc. Together they cover the
// component's contract with parent pages.

import { render, fireEvent } from '@testing-library/svelte';
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import AdminBackfillPanel, { type BaseRun } from './AdminBackfillPanel.svelte';

const NOW = new Date('2026-07-06T18:00:00.000Z').getTime();

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function activeRun(id = '11111111-1111-1111-1111-111111111111'): BaseRun {
  return {
    id,
    started_at: new Date(NOW - 30_000).toISOString(),
    processed: 5,
    succeeded: 5,
    failed: 0,
    isActive: true,
    completed_at: null,
    cancelled_at: null,
    last_error: null,
  };
}

function doneRun(id = '22222222-2222-2222-2222-222222222222'): BaseRun {
  return {
    id,
    started_at: new Date(NOW - 300_000).toISOString(),
    processed: 100,
    succeeded: 98,
    failed: 2,
    isActive: false,
    completed_at: new Date(NOW - 60_000).toISOString(),
    cancelled_at: null,
    last_error: null,
  };
}

const baseProps = {
  title: 'Test backfill',
  runs: [] as BaseRun[],
  loading: false,
  starting: false,
  error: '',
  onStart: vi.fn(),
  onRefresh: vi.fn(),
};

// ---- Start button state ----

describe('AdminBackfillPanel > Start button', () => {
  it('is enabled by default when no run active', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: { ...baseProps, onStart: vi.fn(), onRefresh: vi.fn() },
    });
    const btn = getByTestId('backfill-start') as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    expect(btn.title).toBe('');
  });

  it('is disabled + shows tooltip when startDisabledReason is set', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        startDisabledReason: 'Sidecar not registered',
      },
    });
    const btn = getByTestId('backfill-start') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.title).toBe('Sidecar not registered');
  });

  it('is disabled with startActiveLabel when a run is active and disableStartWhenActive=true', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [activeRun()],
        disableStartWhenActive: true,
        startActiveLabel: 'Run active',
      },
    });
    const btn = getByTestId('backfill-start') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.textContent?.trim()).toBe('Run active');
  });

  it('stays enabled when a run is active and disableStartWhenActive=false', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [activeRun()],
        disableStartWhenActive: false,
      },
    });
    const btn = getByTestId('backfill-start') as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
  });

  it('shows startingLabel while starting=true', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        starting: true,
        startingLabel: 'Starting…',
      },
    });
    const btn = getByTestId('backfill-start') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.textContent?.trim()).toBe('Starting…');
  });

  it('invokes onStart on click', async () => {
    const onStart = vi.fn();
    const { getByTestId } = render(AdminBackfillPanel, {
      props: { ...baseProps, onStart, onRefresh: vi.fn() },
    });
    await fireEvent.click(getByTestId('backfill-start'));
    expect(onStart).toHaveBeenCalledOnce();
  });
});

// ---- Refresh button ----

describe('AdminBackfillPanel > Refresh button', () => {
  it('is disabled while loading', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        loading: true,
      },
    });
    const btn = getByTestId('backfill-refresh') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.textContent?.trim()).toBe('Loading…');
  });

  it('invokes onRefresh on click', async () => {
    const onRefresh = vi.fn();
    const { getByTestId } = render(AdminBackfillPanel, {
      props: { ...baseProps, onStart: vi.fn(), onRefresh },
    });
    await fireEvent.click(getByTestId('backfill-refresh'));
    expect(onRefresh).toHaveBeenCalledOnce();
  });
});

// ---- Cancel button ----

describe('AdminBackfillPanel > Cancel button', () => {
  it('renders on active runs when onCancel is provided', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [activeRun()],
        onCancel: vi.fn(),
      },
    });
    expect(getByTestId('backfill-cancel')).toBeDefined();
  });

  it('does not render when onCancel is undefined', () => {
    const { queryByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [activeRun()],
      },
    });
    expect(queryByTestId('backfill-cancel')).toBeNull();
  });

  it('does not render on completed runs', () => {
    const { queryByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [doneRun()],
        onCancel: vi.fn(),
      },
    });
    expect(queryByTestId('backfill-cancel')).toBeNull();
  });

  it('invokes onCancel with the correct run when no confirm prompt configured', async () => {
    const onCancel = vi.fn();
    const run = activeRun();
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [run],
        onCancel,
      },
    });
    await fireEvent.click(getByTestId('backfill-cancel'));
    expect(onCancel).toHaveBeenCalledWith(run);
  });

  it('skips onCancel when the confirm prompt returns false', async () => {
    const onCancel = vi.fn();
    // happy-dom doesn't provide window.confirm by default; stub it
    // via stubGlobal so vi.fn() has the mock semantics we need.
    const confirmMock = vi.fn().mockReturnValue(false);
    vi.stubGlobal('confirm', confirmMock);
    const run = activeRun();
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [run],
        onCancel,
        cancelConfirmMessage: (r: BaseRun) => `Cancel ${r.id}?`,
      },
    });
    await fireEvent.click(getByTestId('backfill-cancel'));
    expect(confirmMock).toHaveBeenCalledWith(`Cancel ${run.id}?`);
    expect(onCancel).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });
});

// ---- Empty state ----

describe('AdminBackfillPanel > Empty state', () => {
  it('renders default empty text with default colspan (6)', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: { ...baseProps, onStart: vi.fn(), onRefresh: vi.fn() },
    });
    const cell = getByTestId('backfill-empty') as HTMLTableCellElement;
    expect(cell.textContent?.trim()).toBe('No runs yet.');
    expect(cell.colSpan).toBe(6);
  });

  it('respects custom emptyText', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        emptyText: 'No backfill runs yet.',
      },
    });
    expect(getByTestId('backfill-empty').textContent?.trim()).toBe('No backfill runs yet.');
  });

  it('bumps colspan by extraColumnCount + extraColumnCountAfterFailed', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        extraColumnCount: 2,
        extraColumnCountAfterFailed: 1,
      },
    });
    const cell = getByTestId('backfill-empty') as HTMLTableCellElement;
    expect(cell.colSpan).toBe(6 + 2 + 1);
  });

  it('hides the empty row while loading (to avoid flicker)', () => {
    const { queryByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        loading: true,
      },
    });
    expect(queryByTestId('backfill-empty')).toBeNull();
  });
});

// ---- Error banner ----

describe('AdminBackfillPanel > Error banner', () => {
  it('is hidden when error is empty', () => {
    const { queryByTestId } = render(AdminBackfillPanel, {
      props: { ...baseProps, onStart: vi.fn(), onRefresh: vi.fn() },
    });
    expect(queryByTestId('backfill-error')).toBeNull();
  });

  it('renders when error is non-empty', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        error: 'load failed: 500',
      },
    });
    expect(getByTestId('backfill-error').textContent?.trim()).toBe('load failed: 500');
  });
});

// ---- Warning span ----

describe('AdminBackfillPanel > Warning', () => {
  it('is hidden when warning is empty', () => {
    const { queryByTestId } = render(AdminBackfillPanel, {
      props: { ...baseProps, onStart: vi.fn(), onRefresh: vi.fn() },
    });
    expect(queryByTestId('backfill-warning')).toBeNull();
  });

  it('renders when warning is set (e.g. sidecar not registered)', () => {
    const { getByTestId } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        warning: 'Sidecar not registered — enable search.visual.enabled.',
      },
    });
    expect(getByTestId('backfill-warning').textContent).toContain('Sidecar not registered');
  });
});

// ---- Status classification (via rendered classes) ----

describe('AdminBackfillPanel > Status badge', () => {
  it('shows "running" + info class for active runs', () => {
    const { container } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [activeRun()],
      },
    });
    const badge = container.querySelector('tbody span');
    expect(badge?.textContent?.trim()).toBe('running');
    expect(badge?.className).toContain('text-info');
  });

  it('shows "done" + success class for completed runs without last_error', () => {
    const { container } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [doneRun()],
      },
    });
    const badge = container.querySelector('tbody span');
    expect(badge?.textContent?.trim()).toBe('done');
    expect(badge?.className).toContain('text-success');
  });

  it('shows "failed" + danger class for completed runs with last_error', () => {
    const run: BaseRun = { ...doneRun(), last_error: 'sidecar unreachable' };
    const { container } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [run],
      },
    });
    const badge = container.querySelector('tbody span');
    expect(badge?.textContent?.trim()).toBe('failed');
    expect(badge?.className).toContain('text-danger');
  });

  it('shows "cancelled" + muted class for cancelled runs', () => {
    const run: BaseRun = {
      ...doneRun(),
      completed_at: null,
      cancelled_at: new Date(NOW - 60_000).toISOString(),
    };
    const { container } = render(AdminBackfillPanel, {
      props: {
        ...baseProps,
        onStart: vi.fn(),
        onRefresh: vi.fn(),
        runs: [run],
      },
    });
    const badge = container.querySelector('tbody span');
    expect(badge?.textContent?.trim()).toBe('cancelled');
    expect(badge?.className).toContain('text-fg-muted');
  });
});
