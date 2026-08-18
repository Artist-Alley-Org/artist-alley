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

import { ADMIN_ENTRY_CAPS, ADMIN_SECTIONS, sectionBySlug } from './sections';
import enDict from '$lib/i18n/en.json';
import { auth } from '$stores/auth.svelte';

// The seeded `Base` role's ten capabilities, resolved (it has no
// parent). Mirrored from 00001_baseline_v0_1.sql's role_capabilities
// inserts for role 80ec6003-…-d26d39169d42, and from 00039's own prose
// listing of the same ten. Every ordinary signed-in account holds at
// least these.
const BASE_CAPS = [
  'ai.use',
  'assets.submit',
  'caps.read',
  'comments.delete.own',
  'mcp.client.use',
  'posts.comment',
  'posts.like',
  'profile.update_self',
  'roles.read',
  'teams.read',
];

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
    expect(ADMIN_ENTRY_CAPS).toContain('system.asset_types.admin');
  });

  it('is still visible to system.admin (the wildcard short-circuits)', () => {
    auth.caps = ['system.admin'];
    expect(auth.canSeeTile(tile())).toBe(true);
  });
});

// #962 — admin ENTRY is narrower than tile visibility.
//
// The failure this encodes: `canSeeAdmin` used to be "holds any cap
// that any live tile names". The seeded `Base` role carries
// `roles.read` and `teams.read`, and those are exactly the caps of the
// /admin/roles and /admin/teams tiles — so the test was true for every
// authenticated account on a stock install, and the `{:else if
// !canSeeAdmin}` refusal branch in routes/admin/+layout.svelte could
// not execute for a signed-in user at all.
//
// Two properties, and both matter:
//   entry  — a Base cap set does NOT open the shell
//   tiles  — narrowing entry did NOT hide a tile from anyone who was
//            already entitled to it (the regression control)
describe('admin entry vs tile visibility (#962)', () => {
  it('does not let an ordinary Base account into the admin shell', () => {
    auth.caps = BASE_CAPS;
    auth.capsStatus = 'resolved';
    // This is the assertion that makes the layout's refusal branch
    // reachable: without it that branch is dead code with a comment
    // claiming otherwise.
    expect(auth.canSeeAdmin).toBe(false);
  });

  it('keeps roles.read / teams.read out of the entry set but on their tiles', () => {
    // Guard the premise: these really are the two tiles' caps, so a
    // future rename cannot make this test pass vacuously.
    const roles = liveTiles('identity').find((t) => t.key === 'roles');
    const groups = liveTiles('identity').find((t) => t.key === 'groups');
    expect(roles?.cap).toBe('roles.read');
    expect(groups?.cap).toBe('teams.read');

    expect(ADMIN_ENTRY_CAPS).not.toContain('roles.read');
    expect(ADMIN_ENTRY_CAPS).not.toContain('teams.read');

    // …and a Base holder inside the shell still sees both tiles. The
    // fix is a gate change, not a permission change.
    auth.caps = BASE_CAPS;
    auth.capsStatus = 'resolved';
    expect(auth.canSeeTile(roles!)).toBe(true);
    expect(auth.canSeeTile(groups!)).toBe(true);
  });

  it('still admits every admin-standing cap', () => {
    // The regression control, as a set: exactly the two flagged tiles
    // are withheld from entry, and nothing else was swept up with them.
    const liveCaps = new Set(
      ADMIN_SECTIONS.flatMap((s) => s.tiles)
        .filter((t) => t.status === 'live' && t.cap)
        .map((t) => t.cap as string),
    );
    const withheld = [...liveCaps].filter((c) => !ADMIN_ENTRY_CAPS.includes(c));
    expect(withheld.sort()).toEqual(['roles.read', 'teams.read']);

    // Each remaining cap, held alone, opens the shell — a visible tile
    // on a page nobody can reach would be the mirror-image bug.
    for (const cap of ADMIN_ENTRY_CAPS) {
      auth.caps = [cap];
      auth.capsStatus = 'resolved';
      expect(auth.canSeeAdmin, cap).toBe(true);
    }
  });

  it('still admits an Auditor and a system.admin', () => {
    auth.caps = AUDITOR_CAPS;
    auth.capsStatus = 'resolved';
    expect(auth.canSeeAdmin).toBe(true);

    auth.caps = ['system.admin'];
    auth.capsStatus = 'resolved';
    expect(auth.canSeeAdmin).toBe(true);
  });
});

// Tile PLACEMENT, and the i18n key that has to move with it (#1179).
//
// A tile's label is looked up at `admin.sections.<slug>.tiles.<key>`
// (AdminSectionLanding), so the copy is keyed by the section the tile
// sits in. Moving a tile between sections without moving its key
// renders the raw key string on the landing page — a silent break that
// no type and no existing test could see, because both halves are
// individually valid.
describe('tile placement and its section-keyed copy', () => {
  it('every live tile has a title + blurb under its OWN section', () => {
    const sections = (enDict as Record<string, any>).admin?.sections ?? {};
    let checked = 0;
    for (const s of ADMIN_SECTIONS) {
      for (const tile of s.tiles) {
        if (tile.status !== 'live') continue;
        const copy = sections[s.slug]?.tiles?.[tile.key];
        expect(copy, `admin.sections.${s.slug}.tiles.${tile.key} is missing from en.json`)
          .toBeTruthy();
        expect(typeof copy.title, `${s.slug}/${tile.key}.title`).toBe('string');
        expect(typeof copy.blurb, `${s.slug}/${tile.key}.blurb`).toBe('string');
        checked++;
      }
    }
    // Guard against the loop passing vacuously if `live` ever stops
    // being the status string.
    expect(checked).toBeGreaterThan(20);
  });

  it('mature content is a moderation tile, not a system one (#1179)', () => {
    const inSection = (slug: string) =>
      (sectionBySlug(slug)?.tiles ?? []).some((t) => t.key === 'mature_content');

    expect(inSection('moderation'), 'mature_content must live under Community & moderation').toBe(true);
    expect(inSection('system'), 'mature_content must be gone from System').toBe(false);

    // The page did NOT move — the tile is a front door, the same way
    // `anonymous` points at /admin/system/site. A deep link that used to
    // work still has to.
    const tile = sectionBySlug('moderation')!.tiles.find((t) => t.key === 'mature_content')!;
    expect(tile.href).toBe('/admin/system/mature-content');
    expect(tile.cap).toBe('system.config.read');
  });
});
