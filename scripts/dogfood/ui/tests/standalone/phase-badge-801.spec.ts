// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #801 — internal phase numbers must not render anywhere an operator or
// user can see. Drives the actual admin console + account area in a
// browser and inspects the RENDERED text (not the source), which is the
// only check that catches runtime-built strings and ignores dev-facing
// source comments.
//
// FALSE-PASS TRAP this test is written to avoid: the admin layout shows a
// "no permission" panel until the caps request resolves (caps load
// async, soft-fail to empty). A naive "scan main for phase numbers"
// passes trivially against that panel. So every check waits for the
// section to actually RENDER (its own heading, which only appears once
// caps loaded and canSeeAdmin flipped true) before scanning.

import { test, expect, type Page } from '@playwright/test';
import path from 'node:path';

const SHOT_DIR = process.env.SHOT_DIR ?? '/tmp';
const NO_PERMISSION = "You don't have permission to view this page.";

// Internal roadmap identifiers that must never reach a rendered surface:
//   "Phase 1.12", "phase 1.22.C", "1.18.B-12", "C-1.14b", "1.22.G"
const INTERNAL_ID = /\b(?:phase\s+)?\d+\.\d+(?:\.[A-Za-z0-9-]+)?\b|\bC-\d+\.\d+[a-z]?\b/i;

async function contentText(page: Page): Promise<string> {
  const main = page.locator('main').first();
  await main.waitFor({ state: 'visible', timeout: 15_000 });
  return (await main.innerText()).trim();
}

// Open an /admin/[section] landing and block until it has actually
// rendered — the section's H2 title appears only when caps have loaded,
// so this both warms the store and defeats the no-permission false pass.
async function openSection(page: Page, slug: string, title: string): Promise<string> {
  await page.goto(`/admin/${slug}`);
  await expect(page.getByRole('heading', { level: 2, name: title })).toBeVisible({ timeout: 15_000 });
  const text = await contentText(page);
  expect(text, `${slug} still showed the permission panel`).not.toContain(NO_PERMISSION);
  return text;
}

test('admin future tiles render with no phase badge', async ({ page }) => {
  const text = await openSection(page, 'automation', 'Workflow & automation');
  // Positive proof future tiles rendered: their titles are on screen.
  await expect(page.getByRole('heading', { name: 'Triggers' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Webhooks' })).toBeVisible();
  const m = text.match(INTERNAL_ID);
  expect(m, `/admin/automation rendered an internal identifier: ${m?.[0]}`).toBeNull();
  await page.screenshot({
    path: path.join(SHOT_DIR, '801-admin-future-tiles-no-phase-badge.png'),
    fullPage: true,
  });
});

test('future-bearing sections carry no internal identifiers', async ({ page }) => {
  const sections: Array<[string, string]> = [
    ['content', 'Content & metadata'],
    ['system', 'System'],
    ['reports', 'Reports & analytics'],
    ['search', 'Search & discovery'],
    ['storage', 'Storage & assets'],
  ];
  for (const [slug, title] of sections) {
    const text = await openSection(page, slug, title);
    expect(text.length, `${slug} rendered empty`).toBeGreaterThan(20);
    const m = text.match(INTERNAL_ID);
    expect(m, `/admin/${slug} rendered an internal identifier: ${m?.[0]}`).toBeNull();
  }
});

test('federation copy reads in plain language, no aa:Share / phase ids', async ({ page }) => {
  await page.goto('/admin/federation/peers');
  const text = await contentText(page);
  expect(text, 'federation peers showed the permission panel').not.toContain(NO_PERMISSION);
  expect(text.length, 'federation page rendered empty').toBeGreaterThan(20);
  expect(text).not.toMatch(/aa:Share/);
  const m = text.match(INTERNAL_ID);
  expect(m, `federation page rendered an internal identifier: ${m?.[0]}`).toBeNull();
  await page.screenshot({
    path: path.join(SHOT_DIR, '801-federation-plain-language.png'),
    fullPage: true,
  });
});

test('account area shows no internal phase numbers to users', async ({ page }) => {
  await page.goto('/account');
  const text = await contentText(page);
  expect(text.length, 'account page rendered empty').toBeGreaterThan(20);
  const m = text.match(INTERNAL_ID);
  expect(m, `/account rendered an internal identifier: ${m?.[0]}`).toBeNull();
});
