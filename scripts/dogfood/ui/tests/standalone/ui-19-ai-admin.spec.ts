// ui-19-ai-admin.spec.ts
//
// Phase 1.14.A — AI inference admin surface.
//
// Three scenarios:
//   1. /admin/ai/config renders the form with all five concerns
//      represented in routing + fallback chains + privacy lock +
//      budget cards.
//   2. Toggling the AI enabled checkbox and saving round-trips
//      through the backend.
//   3. /admin/ai/usage renders the cost dashboard with the period
//      picker; empty period shows the empty state.
//
// The /admin/ai/config endpoint validates server-side; a bad
// config returns 422 with structured findings. Test 4 exercises
// that: setting privacy lock + clearing the local providers list
// should surface the findings banner inline.
//
// All tests reset the config back to migration defaults on
// afterEach so the run is idempotent.

import { test, expect } from '../../helpers/test';
import { loginAsAdminViaUI } from '../../helpers/auth';

const DEFAULT_CONFIG = {
  enabled: false,
  routing: {
    tag: 'ollama',
    caption: 'claude',
    embed: 'clip_local',
    transcribe: 'whisper_local',
    complete: 'claude',
  },
  fallback_chains: {
    complete: ['claude', 'openai', 'ollama'],
    embed: ['clip_local', 'ollama', 'openai'],
    transcribe: ['whisper_local', 'openai'],
    tag: ['ollama', 'gemini', 'openai'],
    caption: ['claude', 'openai', 'ollama'],
  },
  privacy: {
    lock_sensitive_to_local: true,
    local_providers: ['ollama', 'vllm', 'whisper_local', 'clip_local'],
  },
  default_budget: { soft_warning_usd: 0, hard_cap_usd: 0 },
};

test.describe('UI-19 AI inference admin surface', () => {
  // #535: run serially. Every test here mutates the SAME singleton
  // /admin/ai/config row — and the afterEach restore PUTs it back. Under
  // parallel workers (local `workers: 2`) those writes race: one test's
  // afterEach reset (or another test's save) clobbers the enabled=true
  // this test just saved, so the reload-and-verify below reads back
  // `unchecked` and fails. The round-trip is real; the flake was
  // cross-test interference on shared global state. Serial keeps the
  // config mutations from overlapping. (CI runs workers=1, so it never
  // hit this — but the suite must be green locally at retries: 0 too.)
  test.describe.configure({ mode: 'serial' });

  test.beforeEach(async ({ page }) => {
    await loginAsAdminViaUI(page);
  });

  test.afterEach(async ({ request }) => {
    // Best-effort restore of migration-default values.
    await request.put('/api/v1/admin/ai/config', { data: DEFAULT_CONFIG }).catch(() => undefined);
  });

  test('/admin/ai/config renders the inference config form', async ({ page }) => {
    await page.goto('/admin/ai/config');
    await expect(page.getByTestId('ai-config-form')).toBeVisible();
    await expect(page.getByTestId('ai-config-enabled')).toBeVisible();
    // All five concerns get a routing input.
    for (const c of ['complete', 'embed', 'transcribe', 'tag', 'caption']) {
      await expect(page.getByTestId(`ai-config-routing-${c}`)).toBeVisible();
    }
    await expect(page.getByTestId('ai-config-privacy-lock')).toBeVisible();
    await expect(page.getByTestId('ai-config-budget-soft')).toBeVisible();
    await expect(page.getByTestId('ai-config-budget-hard')).toBeVisible();
    await expect(page.getByTestId('ai-config-save')).toBeVisible();
  });

  test('toggling enabled + save round-trips through the backend', async ({ page }) => {
    await page.goto('/admin/ai/config');
    const enabled = page.getByTestId('ai-config-enabled');
    await expect(enabled).toBeVisible();
    // The migration-default ships disabled; flip on, save, reload,
    // verify the new value persisted.
    if (!(await enabled.isChecked())) {
      await enabled.check();
    }
    await page.getByTestId('ai-config-save').click();
    // #535 (same class as #505): inherit the config's global 15s
    // expect.timeout instead of an explicit 5s cap. The save PATCH +
    // re-fetch + banner render can exceed 5s under CI load, flaking the
    // run; the assertion is unchanged, so a save that never confirms
    // still fails (at 15s).
    await expect(page.getByTestId('ai-config-saved')).toBeVisible();

    // Reload + verify the value persisted across the round trip.
    await page.goto('/admin/ai/config');
    await expect(page.getByTestId('ai-config-enabled')).toBeChecked();
  });

  test('config with privacy-lock + empty local providers surfaces validator findings', async ({ page }) => {
    await page.goto('/admin/ai/config');
    // Privacy lock is on by default; clear the local providers to
    // trigger the privacy_lock_with_empty_local_list finding.
    const localInput = page.getByTestId('ai-config-privacy-local');
    await localInput.fill('');
    await page.getByTestId('ai-config-save').click();
    // The 422 response is rendered inline as the findings banner.
    // #535: inherit the global 15s expect.timeout (the validate round-trip
    // can exceed an explicit 5s under load); assertion unchanged.
    await expect(page.getByTestId('ai-config-findings')).toBeVisible();
    await expect(page.getByTestId('ai-config-findings')).toContainText('privacy_lock_with_empty_local_list');
  });

  test('/admin/ai/usage renders the cost dashboard', async ({ page }) => {
    await page.goto('/admin/ai/usage');
    await expect(page.getByTestId('ai-usage-period')).toBeVisible();
    await expect(page.getByTestId('ai-usage-total')).toBeVisible();
    // Default period might have no rows (no providers configured
    // for the test stack); empty state is the typical render.
    const empty = page.getByTestId('ai-usage-empty');
    const table = page.getByTestId('ai-usage-table');
    const showsSomething = (await empty.isVisible()) || (await table.isVisible());
    expect(showsSomething).toBeTruthy();
  });
});
