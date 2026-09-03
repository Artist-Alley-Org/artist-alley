// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The two cells of the boundary matrix that a browser test cannot pin
// down on a real instance (#1119).
//
// N=0 is the interesting one, and it is not reachable through
// Playwright here: the dev and CI corpora both carry ~25 asset field
// definitions with an EMPTY `applies_to`, which means they apply to
// every asset type, which means no asset in either corpus has zero
// applicable fields. Archiving them mid-suite to manufacture the state
// would change the page under every other spec — the isolation failure
// #1247 is about. So the empty state is asserted here, where the
// component's input is what the test supplies.
//
// Everything else about this component is asserted through the real
// surfaces (asset-field-edit-1119.spec.ts, ui-18-collection-fields.spec.ts),
// because the save model's whole job is to produce the right REQUESTS
// and only a real endpoint can say whether it did.

import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi, beforeEach } from 'vitest';

const defs = vi.fn(async () => ({ data: [] as unknown[] }));
const vals = vi.fn(async () => ({ data: [] as unknown[] }));

vi.mock('$api/client', () => ({
  api: {
    GET: async (path: string) => (path === '/fields' ? defs() : vals()),
    PUT: async () => ({ data: undefined, error: undefined, response: { status: 500 } }),
    DELETE: async () => ({ data: undefined, error: undefined, response: { status: 500 } }),
  },
}));

vi.mock('$stores/lang.svelte', () => ({
  t: (key: string) => key,
}));

import FieldValuesSection from './FieldValuesSection.svelte';

beforeEach(() => {
  defs.mockClear();
  vals.mockClear();
});

describe('FieldValuesSection — the empty state', () => {
  it('renders NO section chrome on an asset with no applicable fields', async () => {
    render(FieldValuesSection, { subjectKind: 'asset', subjectId: 'a-1', assetType: 2 });
    // Settle the two loads.
    await vi.waitFor(() => expect(defs).toHaveBeenCalled());
    await vi.waitFor(() =>
      // Not "renders an empty-state line" — renders NOTHING. A heading
      // announcing that this asset type has no custom fields is chrome
      // charged to every install that never defined one.
      expect(document.querySelector('[data-testid="asset-fields-section"]')).toBeNull(),
    );
    expect(document.querySelector('[data-testid="asset-fields-empty"]')).toBeNull();
  });

  it('keeps the collection modal telling an admin the list is empty', async () => {
    render(FieldValuesSection, { subjectKind: 'collection', subjectId: 'c-1' });
    await vi.waitFor(() =>
      expect(screen.getByTestId('collection-fields-empty')).toBeTruthy(),
    );
    // The shipped behaviour, unchanged: an admin who opened a collection
    // editor asked to see the fields, and "there are none" is an answer.
    expect(screen.getByTestId('collection-fields-section')).toBeTruthy();
  });
});

describe('FieldValuesSection — mirrored definitions', () => {
  it('drops any definition declaring mirrors_column', async () => {
    defs.mockImplementation(async () => ({
      data: [
        { id: 'f-title', code: 'title', label: 'Title', type: 'text', required: true, mirrors_column: 'title' },
        { id: 'f-plain', code: 'plain', label: 'Plain', type: 'text', required: false },
      ],
    }));
    render(FieldValuesSection, { subjectKind: 'asset', subjectId: 'a-1', assetType: 2 });
    await vi.waitFor(() => expect(screen.getByTestId('field-input-plain')).toBeTruthy());
    // GET /assets/{id}/fields DOES return the mirrored columns, so a
    // section that rendered everything it was handed would put a second
    // title box on a page that already has one.
    expect(document.querySelector('[data-testid="field-input-title"]')).toBeNull();
  });
});
