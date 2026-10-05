// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1269 — the shared Modal claims `aria-modal="true"`, so focus has to
// stay inside it.
//
// WHY THIS IS MEASURED RATHER THAN ASSERTED FROM THE MARKUP. The defect
// was found by counting: 45 Tab presses from the open collection edit
// dialog, 7 inside and 38 out, reaching the document body, Explore,
// Collections, Review, the nav search box, Advanced search and Upload.
// A style or attribute assertion cannot see that — the attribute was
// already there and already wrong — so this spec presses Tab and reads
// `document.activeElement` after each press, which is the same
// instrument that produced the number in the issue.
//
// ⚠️ THE COUNT OF PRESSES ONLY MEANS SOMETHING AGAINST A COUNT OF
// FOCUSABLES, and that is why this file builds its own fixture. Run on
// the seeded catalogue's deepest collection, the dialog holds 200+
// picturable tiles and 45 presses never reach the last one — every press
// lands inside, and the test passes on the BROKEN build for a reason
// that has nothing to do with containment. The empty collection created
// below yields a dialog of ~20 focusables, so 45 presses wrap it twice
// and a trap that does not wrap is caught on the first lap. The
// anti-vacuity assertion is written out rather than assumed.
//
// ⛔ THERE IS NO NATIVE INERTNESS UNDERNEATH. Modal is a portalled
// `<div role="dialog" aria-modal="true" tabindex="-1">`, not a
// `<dialog>` opened with `showModal()`, so the browser contributes
// nothing here — a premise that was wrong in #1223's body too.

import type { Page } from '@playwright/test';
import { test, expect } from '../../helpers/test';

const STAMP = Date.now();

/** Every focusable inside the TOP dialog's panel, the same way
 *  Modal.svelte enumerates them — including `sr-only` controls, which
 *  the visibility radios are and which a computed-style filter would
 *  wrongly drop. */
const FOCUSABLE_JS = `
  const dialogs = Array.from(document.querySelectorAll('[role="dialog"]'));
  const panel = dialogs.length ? dialogs[dialogs.length - 1].firstElementChild : null;
  const sel = 'a[href],area[href],input:not([disabled]),select:not([disabled]),' +
    'textarea:not([disabled]),button:not([disabled]),iframe,object,embed,' +
    '[tabindex]:not([tabindex="-1"]),[contenteditable="true"]';
  const items = panel ? Array.from(panel.querySelectorAll(sel)).filter(
    (el) => el.tabIndex >= 0 && !el.hasAttribute('inert') &&
      (el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0),
  ) : [];
`;

async function focusableCount(page: Page): Promise<number> {
  return page.evaluate(`(() => { ${FOCUSABLE_JS} return items.length; })()`) as Promise<number>;
}

/** Press Tab n times and report every press that left the top dialog. */
async function tabProbe(page: Page, presses: number, shift = false) {
  const escapes: string[] = [];
  for (let i = 0; i < presses; i += 1) {
    await page.keyboard.press(shift ? 'Shift+Tab' : 'Tab');
    const where = await page.evaluate(() => {
      const dialogs = Array.from(document.querySelectorAll('[role="dialog"]'));
      const top = dialogs[dialogs.length - 1];
      const el = document.activeElement as HTMLElement | null;
      const inside = !!(el && top && top.contains(el));
      const label =
        el?.getAttribute?.('data-testid') ??
        el?.getAttribute?.('aria-label') ??
        el?.textContent?.trim().slice(0, 28) ??
        '';
      return { inside, label: `${el?.tagName ?? 'NONE'}:${label}` };
    });
    if (!where.inside) escapes.push(`press ${i + 1} -> ${where.label}`);
  }
  return escapes;
}

test.describe('#1269 the shared Modal keeps focus inside itself', () => {
  test.describe.configure({ mode: 'serial' });

  let collectionId: string | undefined;

  test.beforeAll(async ({ request }) => {
    // ⚠️ AN EMPTY COLLECTION IS THE FIXTURE, not an incidental one. The
    // dialog's focusable count has to be BELOW the press count for the
    // probe to exercise a wrap — see the file header.
    const created = await request.post('/api/v1/collections', {
      data: { name: `#1269 focus trap ${STAMP}`, description: 'fixture for #1269' },
    });
    expect(created.status(), 'fixture collection must be created').toBe(201);
    collectionId = ((await created.json()) as { id: string }).id;
  });

  test.afterAll(async ({ request }) => {
    if (collectionId) {
      await request.delete(`/api/v1/collections/${collectionId}`).catch(() => undefined);
    }
  });

  /** Open the collection edit dialog — the surface the issue measured. */
  async function openEditDialog(page: Page) {
    await page.goto(`/collections/${collectionId}`);
    await page.getByTestId('collection-detail-more-button').first().click();
    await page.getByTestId('collection-detail-edit-menuitem').first().click();
    // The dialog seeds itself in an effect on open; measuring focusables
    // before that has finished counts a half-built panel.
    await expect(page.getByTestId('collection-cover-section')).toBeVisible();
    return page.locator('[role="dialog"]').last();
  }

  for (const vp of [
    { label: '1080p', width: 1920, height: 1080 },
    { label: '390px', width: 390, height: 844 },
  ] as const) {
    test(`${vp.label} — 45 Tab presses, none escapes`, async ({ page }) => {
      await page.setViewportSize({ width: vp.width, height: vp.height });
      const dialog = await openEditDialog(page);

      // The attribute that makes containment a PROMISE rather than a
      // nicety. It must be true or gone; this file is what makes it
      // true, so it must be here.
      await expect(dialog).toHaveAttribute('aria-modal', 'true');

      // ANTI-VACUITY. 45 presses through 200 focusables never reaches an
      // edge, so the probe would pass on the broken build.
      const count = await focusableCount(page);
      expect(
        count,
        `the dialog holds ${count} focusable controls, so 45 Tab presses never reach the last ` +
          'one and the probe below cannot see a missing wrap. The fixture is supposed to be an ' +
          'EMPTY collection.',
      ).toBeLessThan(45);
      expect(count, 'the dialog has nothing to tab through at all').toBeGreaterThan(3);

      const escapes = await tabProbe(page, 45);
      expect(
        escapes.length,
        `${escapes.length} of 45 Tab presses left the dialog. #1269 measured 38 of 45 on the ` +
          `pre-fix build, reaching the app chrome behind a dialog that claims aria-modal. ` +
          `Where they went: ${escapes.join(' | ')}`,
      ).toBe(0);
    });
  }

  test('Shift+Tab wraps backwards from the first focusable', async ({ page }) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    await openEditDialog(page);

    // Land on the first focusable deliberately rather than by tabbing to
    // it — the assertion is about the boundary, and getting there by
    // wrapping forwards would be asserting the same thing twice.
    await page.evaluate(`(() => { ${FOCUSABLE_JS} items[0].focus(); })()`);
    await page.keyboard.press('Shift+Tab');

    const landed = await page.evaluate(`(() => {
      ${FOCUSABLE_JS}
      const el = document.activeElement;
      return {
        inside: !!(panel && panel.contains(el)),
        isLast: el === items[items.length - 1],
        label: (el && (el.getAttribute('data-testid') || el.textContent.trim().slice(0, 28))) || '',
      };
    })()`) as { inside: boolean; isLast: boolean; label: string };

    expect(landed.inside, `Shift+Tab from the first control left the dialog (${landed.label})`).toBe(
      true,
    );
    expect(
      landed.isLast,
      `Shift+Tab from the first control landed on "${landed.label}" rather than on the last one ` +
        '— a trap that only handles forward Tab is half a trap',
    ).toBe(true);

    // And backwards from there stays inside for a full lap.
    const escapes = await tabProbe(page, 25, true);
    expect(escapes.length, `Shift+Tab escaped: ${escapes.join(' | ')}`).toBe(0);
  });

  // ── THE STACKED CASE ───────────────────────────────────────────────
  //
  // `modalStack` pops BY IDENTITY because dialogs do not reliably close
  // in reverse order, and the Escape handler reads it so that the RIGHT
  // dialog closes. Containment inherits the same problem: a trap that
  // followed the first-mounted instance would hold focus in the dialog
  // BEHIND the one the curator is looking at.
  //
  // The second dialog is raised with `dispatchEvent` because the edit
  // dialog's backdrop covers the Share button — this is producing a
  // stack, not testing the Share control.
  test('with two dialogs open, Tab cycles within the TOP one', async ({ page }) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    await openEditDialog(page);
    await page.getByTestId('collection-detail-share-button').dispatchEvent('click');

    await expect
      .poll(async () => page.locator('[role="dialog"]').count(), {
        message: 'the second dialog never opened, so there is no stack to follow',
      })
      .toBe(2);

    const count = await focusableCount(page);
    expect(count, 'the top dialog has nothing to tab through').toBeGreaterThan(2);
    expect(count, 'the top dialog is too deep for 30 presses to reach its edge').toBeLessThan(30);

    const escapes = await tabProbe(page, 30);
    expect(
      escapes.length,
      `Tab left the TOP dialog ${escapes.length} times — either the trap is following the ` +
        `dialog underneath or it is not running at all: ${escapes.join(' | ')}`,
    ).toBe(0);

    // And the Escape ownership the trap borrows its topness from is
    // still intact: one press closes exactly one dialog.
    await page.keyboard.press('Escape');
    await expect
      .poll(async () => page.locator('[role="dialog"]').count(), {
        message: 'one Escape closed something other than exactly the top dialog',
      })
      .toBe(1);
  });

  // ── FOCUS-RESTORE IS PRESERVED ─────────────────────────────────────
  //
  // The trap moves focus; the close path has to put it back. Driven
  // through the Share button rather than the More menu deliberately: a
  // menu ITEM is unmounted when the menu closes, so the element the
  // dialog captured no longer exists and focus lands on the body. That
  // is pre-existing behaviour of Menu + Modal together and is reported
  // separately — what this asserts is the property the issue names, on
  // an opener that is still there to receive it.
  test('closing returns focus to the control that opened the dialog', async ({ page }) => {
    await page.setViewportSize({ width: 1920, height: 1080 });
    await page.goto(`/collections/${collectionId}`);
    const opener = page.getByTestId('collection-detail-share-button');
    await opener.click();
    await expect(page.locator('[role="dialog"]')).toBeVisible();

    // Move focus with the trap in play, so the restore is undoing a real
    // move rather than a no-op.
    await page.keyboard.press('Tab');
    await page.keyboard.press('Tab');
    await page.keyboard.press('Escape');
    await expect(page.locator('[role="dialog"]')).toBeHidden();

    const restored = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      return el?.getAttribute('data-testid') ?? el?.tagName ?? 'NONE';
    });
    expect(
      restored,
      'closing the dialog did not put focus back on the button that opened it',
    ).toBe('collection-detail-share-button');
  });
});
