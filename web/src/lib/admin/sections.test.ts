// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Admin tile visibility for the read-only Auditor tier (#961).
//
// Why this file exists — the failure it encodes:
//
// The whole point of the per-tile `cap` in sections.ts is the invariant
// stated on the type: "A read-cap holder sees exactly the tiles whose
// `cap` they hold, so no tile 403s on click." Nothing checked that
// against a real capability set. It was checked one tile at a time, by
// hand, against `canSeeTile` with a literal object — which cannot catch
// the two mistakes that matter: a live tile whose `cap` is a code no
// role can hold (#958, eleven of them at once), and a tile that names
// no cap and therefore silently means superuser-only.
//
// So this asserts the tier as a set: what an Auditor sees, what it does
// not, and that the cap-less tiles stay superuser-only.
//
// The capability list below is the resolved Auditor set, mirrored from
// app/internal/db/auditor_admin_read_caps_migration_test.go. It is
// duplicated deliberately rather than imported — there is no build-time
// path from the Go migration set to the browser bundle, and a hardcoded
// copy that drifts fails loudly HERE, whereas no copy at all means the
// frontend never asserts the tier exists.

import { afterEach, describe, expect, it } from 'vitest';

import { ADMIN_SECTIONS, ADMIN_TILE_CAPS, sectionBySlug } from './sections';
import { auth } from '$stores/auth.svelte';

// Exactly what migration 00039 + 00040 resolve for a user holding only
// the Auditor role (its own nine, plus Base's ten through parent_id).
const AUDITOR_CAPS = [
  'ai.use',
  'assets.submit',
  'caps.read',
  'comments.delete.own',
  'featured.read',
  'federation.read',
  'mcp.client.use',
  'posts.comment',
  'posts.like',
  'profile.update_self',
  'requests.read',
  'roles.read',
  'system.activities.read',
  'system.audit.read',
  'system.jobs.read',
  'system.license.read',
  'system.metadata_extraction.read',
  'system.storage.read',
  'teams.read',
];

afterEach(() => {
  auth.caps = [];
  auth.capsStatus = 'resolved';
  auth.user = null;
});

function liveTiles(slug: string) {
  const section = sectionBySlug(slug);
  if (!section) throw new Error(`no admin section '${slug}'`);
  return section.tiles.filter((t) => t.status === 'live');
}

describe('Auditor tile visibility (#961)', () => {
  it('opens every live tile in jobs, storage and audit', () => {
    auth.caps = AUDITOR_CAPS;

    // Six jobs tiles + four storage tiles gate on the two caps 00040
    // grants; `render_farm` is `future` and has no href to open.
    for (const tile of liveTiles('jobs')) {
      expect(auth.canSeeTile(tile), `jobs/${tile.key}`).toBe(true);
    }

    // `trash` is the exception in `storage`: it is a live tile with no
    // cap, i.e. superuser-only, and it is asserted hidden below.
    for (const tile of liveTiles('storage').filter((t) => t.cap)) {
      expect(auth.canSeeTile(tile), `storage/${tile.key}`).toBe(true);
    }

    const audit = liveTiles('automation').find((t) => t.key === 'audit');
    expect(audit?.cap).toBe('system.audit.read');
    expect(auth.canSeeTile(audit!)).toBe(true);
  });

  it('still refuses every cap-less (superuser-only) live tile', () => {
    auth.caps = AUDITOR_CAPS;

    // This negative is reachable, not decorative: these tiles exist
    // today and an Auditor holds none of `system.admin`. Assert the set
    // is non-empty first, so the loop cannot pass vacuously if every
    // tile later grows a cap.
    const superuserOnly = ADMIN_SECTIONS.flatMap((s) =>
      s.tiles.filter((t) => t.status === 'live' && !t.cap && !t.public).map((t) => `${s.slug}/${t.key}`),
    );
    expect(superuserOnly.length).toBeGreaterThan(0);

    for (const s of ADMIN_SECTIONS) {
      for (const tile of s.tiles) {
        if (tile.status !== 'live' || tile.cap || tile.public) continue;
        expect(auth.canSeeTile(tile), `${s.slug}/${tile.key} must stay superuser-only`).toBe(false);
      }
    }
  });

  it('can see the admin shell at all', () => {
    auth.caps = AUDITOR_CAPS;
    expect(auth.canSeeAdmin).toBe(true);
  });
});

describe('asset_types tile (#961)', () => {
  const tile = () => {
    const t = liveTiles('content').find((x) => x.key === 'asset_types');
    if (!t) throw new Error('asset_types tile missing from the content section');
    return t;
  };

  it('names the capability its ACL endpoints actually enforce', () => {
    // The three /asset_types/{ref}/acls endpoints gate on this code
    // (assettype/acls_handler.go). The tile's index GET does not, which
    // is why the tile used to carry no cap at all.
    expect(tile().cap).toBe('system.asset_types.admin');
  });

  it('is hidden from a read-only Auditor, who cannot use it', () => {
    auth.caps = AUDITOR_CAPS;
    expect(auth.canSeeTile(tile())).toBe(false);
  });

  it('is visible to an operator handed exactly that capability', () => {
    auth.caps = ['system.asset_types.admin'];
    expect(auth.canSeeTile(tile())).toBe(true);
    // ...and the hand-out is enough to reach the admin shell, or the
    // tile would be visible on a page they cannot open.
    expect(auth.canSeeAdmin).toBe(true);
    expect(ADMIN_TILE_CAPS).toContain('system.asset_types.admin');
  });

  it('is still visible to system.admin (the wildcard short-circuits)', () => {
    auth.caps = ['system.admin'];
    expect(auth.canSeeTile(tile())).toBe(true);
  });
});
