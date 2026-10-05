// #1191 — the advanced search page at DAM scale, and in plain words.
//
// Two regressions this file exists to catch.
//
// # 1. A field with a real vocabulary must not render as a chip wall
//
// Every field row on /search/advanced used to print its ENTIRE
// vocabulary as chips. That is fine at the dozen values the seed
// ships and unusable at the hundreds a production catalogue carries —
// so past CHIP_LIMIT (16, in the page) a field becomes a typeahead
// instead. The seed has no such field, so this spec MAKES one: an
// asset field with 240 values, created through the ordinary admin API
// and archived again when the file finishes.
//
// The fixture is idempotent by design. `DELETE /fields/{id}` archives
// rather than erases (it is a soft delete — the values on assets
// survive), so a second run finds the tombstone and revives it with a
// PATCH rather than colliding on the unique code. That also means a
// crashed run leaves an ARCHIVED row, which /search/advanced already
// filters out — a failed cleanup cannot leak the probe onto the page.
//
// # 2. No developer vocabulary on the rendered page
//
// The page shipped saying "Compiled DSL" and "the server-side DSL
// parser" at people who are looking for artwork. The guard is a grep
// over the RENDERED text rather than over the source, because the
// source legitimately says "DSL" all over its comments and the
// question is only ever what a person can read.

import { test, expect } from '../../helpers/test';
import type { APIRequestContext, Page } from '@playwright/test';
import { loginAsAdminViaAPI } from '../../helpers/auth';
import { tid } from '../../helpers/testids';

const PROBE_CODE = 'probe_1191_large_vocab';
const PROBE_LABEL = 'Pigment (1191 probe)';

/**
 * 240 values — comfortably past the page's 16, and past the 50 rows
 * the combobox will paint at once, so the "keep typing to narrow it
 * down" tail is exercised too.
 *
 * Built from hues × shades rather than from a counter so that typing
 * is a real filter with real cardinalities: "cobalt" admits exactly
 * 20 of them and "cobalt 07" exactly one. A vocabulary of
 * `value-001…value-240` would let a broken filter pass by matching
 * everything.
 */
const HUES = [
  'cobalt', 'ochre', 'vermilion', 'viridian', 'umber', 'sienna',
  'cerulean', 'magenta', 'indigo', 'teal', 'crimson', 'amber',
];
const SHADES = 20;
const VALUES = HUES.flatMap((hue) =>
  Array.from({ length: SHADES }, (_, i) => {
    const n = String(i + 1).padStart(2, '0');
    return {
      value: `${hue}-${n}`,
      label: `${hue[0].toUpperCase()}${hue.slice(1)} ${n}`,
    };
  }),
);

/** What the page must never say out loud. */
const JARGON = /\b(DSL|parser|parsing|grammar|compiled|compile|syntax|regex|boolean|predicate|tokenis|tokeniz|serialise|serialize|endpoint|backend|schema)\b/i;

async function findProbe(request: APIRequestContext, status?: string) {
  const url = status ? `/api/v1/fields?status=${status}` : '/api/v1/fields';
  const r = await request.get(url);
  expect(r.ok(), `GET ${url} → ${r.status()}`).toBeTruthy();
  const rows = (await r.json()) as { id: string; code: string }[];
  return rows.find((f) => f.code === PROBE_CODE);
}

/** Create the probe field, or revive the tombstone a previous run left. */
async function ensureProbeField(request: APIRequestContext): Promise<string> {
  await loginAsAdminViaAPI(request);
  const options = { values: VALUES };
  const revive = async (id: string) => {
    const r = await request.patch(`/api/v1/fields/${id}`, {
      data: { status: 'active', label: PROBE_LABEL, options, searchable: true },
    });
    expect(r.ok(), `revive probe field → ${r.status()} ${await r.text()}`).toBeTruthy();
    return id;
  };

  const existing = (await findProbe(request)) ?? (await findProbe(request, 'archived'));
  if (existing) return revive(existing.id);

  const r = await request.post('/api/v1/fields', {
    data: {
      code: PROBE_CODE,
      label: PROBE_LABEL,
      type: 'multi_select',
      subject_kind: 'asset',
      options,
      searchable: true,
      // Last on the page, so the seeded rows above it keep the
      // positions the "small vocabularies are unchanged" screenshot
      // was taken at.
      display_order: 9000,
    },
  });
  if (r.status() === 201) return ((await r.json()) as { id: string }).id;
  // Lost a race with another worker (the code is unique). Whoever won
  // has created exactly the field this one wanted, so adopt it.
  const raced = (await findProbe(request)) ?? (await findProbe(request, 'archived'));
  expect(raced, `create probe field → ${r.status()} ${await r.text()}`).toBeTruthy();
  return revive(raced!.id);
}

// One worker for the whole file. `beforeAll` runs once PER WORKER, and
// with the config's parallel workers that meant two of them racing to
// create the same uniquely-coded field. Serial mode also keeps the
// probe's lifetime to a single contiguous window, which is what makes
// it safe to add an asset field to a shared instance mid-suite.
test.describe.configure({ mode: 'serial' });

let probeId = '';

test.beforeAll(async ({ request }) => {
  probeId = await ensureProbeField(request);
});

test.afterAll(async ({ request }) => {
  if (!probeId) return;
  await loginAsAdminViaAPI(request);
  await request.delete(`/api/v1/fields/${probeId}`);
});

/** The probe's row, waited for — the page fetches /fields on mount. */
async function openAdvanced(page: Page) {
  await page.goto('/search/advanced');
  await expect(page.locator(tid('advanced-search-page'))).toBeVisible();
  await expect(page.locator(`[data-testid="field-filter-${PROBE_CODE}"]`)).toBeVisible();
}

const combobox = `[data-testid="vocab-combobox-${PROBE_CODE}"]`;
const input = `[data-testid="vocab-input-${PROBE_CODE}"]`;
const option = `[data-testid="vocab-option-${PROBE_CODE}"]`;
const chip = `[data-testid="vocab-chip-${PROBE_CODE}"]`;

test.describe('advanced search — vocabulary scale + plain language (#1191)', () => {
  test('a 240-value field is a typeahead; a small one is still chips', async ({ page }) => {
    await openAdvanced(page);

    // The big one became a combobox, and did NOT paint 240 buttons.
    await expect(page.locator(combobox)).toBeVisible();
    await expect(
      page.locator(`[data-testid^="field-option-${PROBE_CODE}-"]`),
    ).toHaveCount(0);

    // Some other field is still a chip row — the threshold is a
    // threshold, not a migration. Asserted on the CHIP buttons rather
    // than on a named field, and not as a COUNT of comboboxes: an
    // instance whose open `keywords` vocabulary has grown past 20 is
    // entitled to a second one, and that is the feature rather than a
    // failure.
    const chipButtons = page.locator('[data-testid^="field-option-"]');
    expect(await chipButtons.count()).toBeGreaterThan(0);

    // Whatever else is on the page, every field row carries EXACTLY ONE
    // control.
    //
    // ⚠️ WIDENED BY #1165, and deliberately not weakened. This used to
    // read "chips XOR combobox", which was a complete description of the
    // page while vocabulary fields were the only kind it could draw. The
    // operator grammar gave `text` fields a contains box and `date`
    // fields a range, so those rows now legitimately have NEITHER — and
    // the old assertion failed on them, which is the correct behaviour
    // for a spec that pinned a page that no longer exists.
    //
    // Widening it to "at most one" would have been the lazy repair and
    // would have stopped catching anything: a row with no control at all
    // is a field the caller can see and cannot use, which is exactly
    // what #1157 shipped for text and date. So the rule is ONE, counted
    // across every control family the page knows how to render.
    const rows = page.locator('[data-testid^="field-filter-"]');
    for (let i = 0; i < (await rows.count()); i++) {
      const row = rows.nth(i);
      const code = (await row.getAttribute('data-testid'))!.replace('field-filter-', '');
      const controls = {
        chips: await row.locator('[data-testid^="field-option-"]').count(),
        combobox: await row.locator('[data-testid^="vocab-combobox-"]').count(),
        contains: await row.locator('[data-testid^="field-contains-"]').count(),
        // A range is one control with two ends, so its bounds count once.
        range: Math.min(
          await row.locator('[data-testid^="field-from-"]').count(),
          await row.locator('[data-testid^="field-to-"]').count(),
        ),
      };
      const kinds = Object.entries(controls).filter(([, n]) => n > 0);
      expect(
        kinds.length,
        `field row "${code}" rendered ${kinds.length} control kinds ` +
          `(${JSON.stringify(controls)}). Zero means a field is offered with no way to ` +
          'filter on it; more than one means two controls disagree about the same value.',
      ).toBe(1);
    }
  });

  test('typing filters the list in the browser', async ({ page }) => {
    await openAdvanced(page);
    await page.locator(input).click();

    // Unfiltered: capped at the control's 50 rows with the "keep
    // typing" tail, NOT 240.
    await expect(page.locator(option)).toHaveCount(50);
    await expect(page.locator(`[data-testid="vocab-truncated-${PROBE_CODE}"]`)).toBeVisible();

    await page.locator(input).pressSequentially('cobalt');
    await expect(page.locator(option)).toHaveCount(SHADES);
    for (const text of await page.locator(option).allInnerTexts()) {
      expect(text.toLowerCase()).toContain('cobalt');
    }

    await page.locator(input).pressSequentially(' 07');
    await expect(page.locator(option)).toHaveCount(1);
    await expect(page.locator(option)).toHaveAttribute('data-value', 'cobalt-07');

    // A term this closed field does not have is refused in words, not
    // offered as something to create.
    await page.locator(input).fill('not-a-pigment');
    await expect(page.locator(`[data-testid="vocab-blocked-${PROBE_CODE}"]`)).toBeVisible();
    await expect(page.locator(`[data-testid="vocab-create-${PROBE_CODE}"]`)).toHaveCount(0);
  });

  test('tokens add and come back off', async ({ page }) => {
    await openAdvanced(page);
    await page.locator(input).click();
    await page.locator(input).pressSequentially('viridian 03');
    await page.locator(option).first().click();

    await expect(page.locator(chip)).toHaveCount(1);
    await expect(page.locator(chip)).toHaveAttribute('data-value', 'viridian-03');
    // The page's own running total counts it.
    await expect(page.locator(tid('advanced-active-count'))).toContainText('1');

    await page.locator(`[data-testid="vocab-chip-remove-${PROBE_CODE}"]`).click();
    await expect(page.locator(chip)).toHaveCount(0);
    await expect(page.locator(tid('advanced-active-count'))).toHaveCount(0);
  });

  test('keyboard alone picks a value and runs the search', async ({ page }) => {
    await openAdvanced(page);
    // focus() reaches the control; every INTERACTION below is a real
    // key event dispatched by the browser, which is the half that can
    // regress.
    await page.locator(input).focus();
    await page.keyboard.type('amber 12');
    await expect(page.locator(option)).toHaveCount(1);
    await page.keyboard.press('ArrowDown');
    await expect(page.locator(option).first()).toHaveAttribute('aria-selected', 'true');
    await page.keyboard.press('Enter');
    await expect(page.locator(chip)).toHaveAttribute('data-value', 'amber-12');

    // Escape closes the list without disturbing what was chosen.
    await page.keyboard.press('Escape');
    await expect(page.locator(`[data-testid="vocab-list-${PROBE_CODE}"]`)).toHaveCount(0);
    await expect(page.locator(chip)).toHaveCount(1);

    // And the selection composes into the same URL a chip would have
    // produced — one `filter=field:<code>=<value>` term, on /search.
    await page.locator(tid('advanced-submit')).press('Enter');
    await expect(page).toHaveURL(
      new RegExp(`/search\\?.*filter=field%3A${PROBE_CODE}%3Damber-12`),
    );
  });

  test('the rendered page carries no developer vocabulary', async ({ page }) => {
    await openAdvanced(page);
    // Open the typeahead too, so the dropdown's own strings are in the
    // text being read.
    await page.locator(input).click();
    await expect(page.locator(option).first()).toBeVisible();

    const text = await page.locator('body').innerText();
    const hit = text.match(JARGON);
    expect(hit, `rendered page says "${hit?.[0]}"`).toBeNull();

    // The preview of what will run is still there — this issue renamed
    // it, it did not delete it.
    await expect(page.locator(tid('advanced-compiled'))).toBeVisible();
  });

  test('the preview still round-trips a built query', async ({ page }) => {
    await openAdvanced(page);
    await page.locator(tid('advanced-row-value')).first().fill('lighthouse');
    await expect(page.locator(tid('advanced-compiled'))).toHaveText('title:lighthouse');
    // The CONDITIONS section's own Search. The sticky bar's button is
    // the one for a field-filter selection; a condition typed but not
    // yet submitted has not reached the page's state, which is
    // pre-existing #1157 behaviour this issue does not change.
    await page.locator('[data-testid="advanced-conditions"] button[type="submit"]').click();
    await expect(page).toHaveURL(/\/search\?.*dsl=title%3Alighthouse/);
  });
});
