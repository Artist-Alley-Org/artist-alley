# Seeding instructions for the apply-side agent

Read this before writing the apply script. The seed data has been
assembled and laid out on the Synology archive; your job is to drive
artist-alley's API to materialize it into a running instance's database.

## Where the data lives

The assembled seed sits at one of two paths on the Synology archive
(mounted at `/mnt/blackbox_archives/datasets/artist_alley/` on the
developer workstation):

```
/mnt/blackbox_archives/datasets/artist_alley/
├── site_a/                      LAYER A ONLY — public-safe (demo source)
│   ├── 3d/...                   Khronos glTF + Kenney 3D + game-rip refs
│   ├── audio/...                Kenney RPG audio packs
│   ├── documents/...            Project Gutenberg + Vue README + Kenney readmes
│   ├── fonts/...                OFL fonts (Sono, Wellfleet, Playwrite, Kenney)
│   ├── images/...               Kenney CC0 + NASA + Polyhaven HDR + Met Museum
│   ├── videos/...               BBB + Sintel trailer + open-source game videos
│   ├── comics/                  (empty for now — archive.org blocks us)
│   ├── metadata.csv             original CSV filtered + path-rewritten
│   ├── groups.csv               sibling linkage (one row per logical group)
│   └── MANIFEST.json            **the apply contract** — see schema below
└── site_b/                      LAYER A + B — local-only (full dataset)
    ├── (same folder structure)
    └── ...
```

`site_a` is the demo / Studio A federation peer. `site_b` is the local
dev re-seed / Studio B federation peer. Start with site_a if you're
seeding one for now (smaller, cleaner, demo-targeted).

## The MANIFEST.json contract

This is the source of truth for what to seed. Each entry is one asset:

```json
{
  "id": "7b46896d-6e07-a1cb-f2a3-36d55b66c2f7",
  "asset_type": "image",
  "title": "Img Char Clyde Metallic Decoded (raster)",
  "description": "Raster source for ...",
  "file_path": "images/Browser Games - Nitro Control - Characters - Clyde/IMG_CHAR_clyde_metallic_decoded.png",
  "source_path": "Browser Games - .../IMG_CHAR_clyde_metallic_decoded.png",
  "source_root": "local",
  "file_extension": "png",
  "file_size_bytes": 53009,
  "sensitivity_tier": "restricted",
  "archive_state": "draft",
  "owner_username": "akira.tanaka",
  "collection_name": "Art Research",
  "team_name": "Characters",
  "brand_workspace": null,
  "tags": ["characters", "franchise:reference"],
  "workflow_state": "approved",
  "metadata": {
    "filename": "...",
    "kind": "raster",
    "license": "Internal",
    "usage_rights": "Reference Only",
    "acquisition_source": "The Models Resource",
    "attribution": "Community rip — reference use only",
    "group_id": "grp-00006"
  },
  "field_values": {
    "pipeline_stage": "Final",
    "version": "v2",
    "revision_count": 7,
    "rating": 4,
    "texture_resolution": "256x256",
    "color_space": "sRGB",
    "engine_compatibility": "Unreal 5",
    "target_platforms": ["PC"],
    "naming_compliant": true
  },
  "external_id": "AA-5653",
  "review_notes": "Tighten the value contrast in the next iteration.",
  "reviewer_username": "olaf.lindgren",
  "created_at": "2025-12-12T21:55:27Z",
  "updated_at": "2026-05-14T23:08:15Z",
  "last_reviewed_at": "2026-05-15T06:49:45Z",
  "license": "Internal",
  "attribution": "Community rip — reference use only",
  "layer": "B",
  "studio": "a"
}
```

**Key fields:**
- `id` — pre-generated stable UUID. Use this as the AA `assets.id`.
- `file_path` — relative to the site root. Resolve as
  `<archive>/site_<x>/<file_path>` to find the bytes.
- `created_at` / `updated_at` / `last_reviewed_at` — **preserve these**.
  Don't let the DB stamp them at insert time. The records are sorted in
  the MANIFEST by `created_at`, so iterating in order produces a
  time-realistic ingest pattern.

## Supporting catalogues

At `/mnt/d/Projects/artist-alley/seed/profiles/` (the repo, not the
archive) there are 7 sidecar JSONs that define the upstream entities
the assets reference:

| File | Contents |
|---|---|
| `dataset.users.json` | 30 fictional users with usernames, full names, roles, primary teams. Seed these first. |
| `dataset.teams.json` | 9 teams (Animation, Audio, Characters, Environment, Marketing Art, Props, Reference, UI, VFX). |
| `dataset.collections.json` | 16 projects mapped to AA collections, with `studio_membership` (e.g. `["a","b"]` means both sites have it). |
| `dataset.brand_workspaces.json` | Echo + Mirror brand workspaces (per ADR 0025). Both owned by studio_a. |
| `dataset.field_definitions.json` | 12 custom metadata field definitions (pipeline_stage / version / texture_resolution / etc.). |
| `dataset.workflow.json` | 5 workflow states (draft → in_review → approved → final → archived) + 6 transitions. |
| `dataset.MANIFEST.json` | Top-level summary stats; not the per-asset data. |

## Recommended seeding order

The dependency graph dictates the order. Do not parallelize across these
phases — each later phase reads UUIDs created in an earlier one.

```
1. Workflow states + transitions       (dataset.workflow.json)
2. Field definitions                    (dataset.field_definitions.json)
3. Teams                                (dataset.teams.json)
4. Users + team memberships             (dataset.users.json)
5. Brand workspaces                     (dataset.brand_workspaces.json)
6. Collections                          (dataset.collections.json)
                                        — filter by studio_membership
                                          if seeding only one site
7. Assets (the MANIFEST, in JSON order) — one record at a time:
   a. Upload bytes via storage API:
      POST /api/uploads + body = file bytes from <archive>/site_<x>/<file_path>
   b. Create asset row:
      POST /api/assets with id, title, description, asset_type,
           file_extension, file_size_bytes, owner_user_ref,
           sensitivity_tier, archive_state, metadata
   c. Set workflow state (PATCH /api/assets/{id}/workflow_state)
   d. Add to collection (POST /api/collections/{id}/assets)
   e. Attach to brand workspace if non-null (POST /api/brand_workspaces/{id}/assets)
   f. Add tags (POST /api/assets/{id}/tags)
   g. Set custom field values (POST /api/assets/{id}/field_values)
   h. If review_notes is non-null, create a comment from reviewer_username
   i. Backfill timestamps (PATCH /api/assets/{id}/timestamps) —
      override created_at / updated_at / last_reviewed_at with the
      manifest values so the feed looks time-realistic
```

## Idempotency

The asset `id` is deterministic — re-running with the same MANIFEST
should not duplicate. Strategy:
- Check if the asset id exists → if yes, skip (or update in place)
- Same for users, teams, collections, brand workspaces (their ids are
  also stable UUIDs derived from name)

## Verification

After a successful seed, the database should contain:

| Entity | Expected count for site_a |
|---|---|
| Users | 30 |
| Teams | 9 |
| Collections | 6 (Project Mirror / Echo / Citylight / Compass / Engine Core / Studio Library — though E.C. and S.L. are mostly site_b-only) |
| Brand workspaces | 2 (Echo, Mirror — both owned by site_a) |
| Field definitions | 12 |
| Workflow states | 5 |
| Assets | 963 (matches `find site_a -type f | wc -l` minus 3 metadata files) |

`MANIFEST.json` at the top level has the same stats — query it post-seed
to verify the targets.

## Things to NOT do

- **Don't auto-stamp created_at.** The manifest values are the truth.
- **Don't dedupe assets by content hash at seed time.** site_a +
  site_b deliberately have 110 byte-identical files for CAS dedup
  testing across federation. Hash dedup is what AA's storage layer does
  at upload; the seed script just trusts the manifest.
- **Don't bulk-upload all images first.** Iterate in MANIFEST order
  (sorted by created_at) so the seeded feed shows interlaced variety.
- **Don't follow the `external_id` field.** It's the old CSV-row id
  (`AA-XXXX` format) — preserve as metadata but use the `id` UUID for
  the AA row's primary key.
- **Don't drop Layer B from site_b.** That's the local dev set; it has
  to keep the IP/personal content. Only site_a is Layer A only.

## If something goes wrong

- Asset file not found: the manifest had a path that doesn't exist on
  the archive. Log and skip — don't crash. There shouldn't be any of
  these (populate_archive.py verified all paths) but defense in depth.
- Wikimedia / fetch failures landed null bytes: shouldn't happen
  because fetch_gaps.py verifies size > 0, but a final sanity check
  on file size is cheap.
- API rate limits / 5xx: retry with exponential backoff. Don't blast
  through the seed without backoff or you'll get cut off.

## Site_a quick-start command (when ready)

```bash
# Adjust to your apply script's invocation
python3 seed/scripts/apply.sh \
  --site /mnt/blackbox_archives/datasets/artist_alley/site_a \
  --catalogue /mnt/d/Projects/artist-alley/seed/profiles \
  --api http://localhost:8080/api \
  --token $AA_ADMIN_TOKEN
```

Open issues to be aware of (tracked in the failed array of
`internet-fetched/MANIFEST.json`):
- site_a has 0 comics — no Layer A comic source worked yet
- site_a has 0 internet audio — LibriVox via archive.org blocked
- Some classic art / NASA imagery + Wikimedia VG screenshots failed
  fetch (5–7 items) — won't appear in MANIFEST, no action needed
