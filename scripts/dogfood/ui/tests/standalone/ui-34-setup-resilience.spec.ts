// ui-34-setup-resilience.spec.ts
//
// Pure-logic guard for the setup project's startup resilience (#527
// fold). The setup step gates the WHOLE standalone suite, so a single
// transient blip there used to cascade into ~130 failures. These tests
// pin the two helpers that prevent that — withTransientRetry and
// waitForAppReady — with mocked request contexts, so they run without
// the app and prove the behaviour deterministically (you can't reliably
// induce an ECONNRESET against the live app).

import { test, expect } from '../../helpers/test';
import { withTransientRetry, waitForAppReady } from '../../helpers/ready';

// This spec makes no HTTP calls — opt out of the shared admin session so
// it doesn't couple to the setup project it's testing the resilience of.
test.use({ storageState: { cookies: [], origins: [] } });

test.describe('UI-34 setup resilience helpers (#527)', () => {
  test('withTransientRetry: retries a transient blip then succeeds', async () => {
    let calls = 0;
    const result = await withTransientRetry(
      'flaky',
      async () => {
        calls++;
        if (calls < 3) throw new Error('read ECONNRESET');
        return 'ok';
      },
      { baseDelayMs: 1 },
    );
    expect(result).toBe('ok');
    expect(calls).toBe(3); // failed twice, succeeded on the third
  });

  test('withTransientRetry: a non-transient error throws immediately (no retry)', async () => {
    let calls = 0;
    await expect(
      withTransientRetry(
        'real-bug',
        async () => {
          calls++;
          throw new Error('assertion failed: expected 200');
        },
        { baseDelayMs: 1 },
      ),
    ).rejects.toThrow(/assertion failed/);
    expect(calls).toBe(1); // tolerate the transient, never the persistent
  });

  test('withTransientRetry: gives up after the attempt budget', async () => {
    let calls = 0;
    await expect(
      withTransientRetry(
        'always-down',
        async () => {
          calls++;
          throw new Error('connect ECONNREFUSED 127.0.0.1:8080');
        },
        { attempts: 3, baseDelayMs: 1 },
      ),
    ).rejects.toThrow(/ECONNREFUSED/);
    expect(calls).toBe(3);
  });

  test('waitForAppReady: polls past not-ready responses until 200', async () => {
    let probes = 0;
    // Mock the shape waitForAppReady uses: request.get() → { ok(), status() }.
    const request = {
      get: async () => {
        probes++;
        const ready = probes >= 3;
        return { ok: () => ready, status: () => (ready ? 200 : 503) };
      },
    } as unknown as Parameters<typeof waitForAppReady>[0];
    await waitForAppReady(request, { timeoutMs: 5_000, intervalMs: 1 });
    expect(probes).toBe(3);
  });

  test('waitForAppReady: throws with the last status when the deadline passes', async () => {
    const request = {
      get: async () => ({ ok: () => false, status: () => 503 }),
    } as unknown as Parameters<typeof waitForAppReady>[0];
    await expect(
      waitForAppReady(request, { timeoutMs: 30, intervalMs: 5 }),
    ).rejects.toThrow(/not ready.*HTTP 503/);
  });

  test('waitForAppReady: a thrown connection error is caught, not fatal', async () => {
    let probes = 0;
    const request = {
      get: async () => {
        probes++;
        if (probes < 2) throw new Error('read ECONNRESET');
        return { ok: () => true, status: () => 200 };
      },
    } as unknown as Parameters<typeof waitForAppReady>[0];
    await waitForAppReady(request, { timeoutMs: 5_000, intervalMs: 1 });
    expect(probes).toBe(2);
  });
});
