# seed/ — demo + dogfood + dev-reseed dataset

This directory holds everything needed to populate a fresh artist-alley
instance with a representative studio-shaped dataset. Four profiles are
emitted from one canonical source:

| Profile | Where it ships | Content |
|---|---|---|
| `demo.assets.json` | Phase 1.48 public demo sandboxes at `demo.artist-alley.org` | **Layer A only** — public-safe (Kenney CC0, PixelSpaces, Google Fonts, UISketch, internet-fetched CC-BY/CC0 samples) |
| `studio-a.assets.json` | Phase 1.22.I-a dogfood (Mirror Studios side) | Layer A + B — full curated set, ~600 assets, ~330 MB |
| `studio-b.assets.json` | Phase 1.22.I-a dogfood (Adventureworks side) | Layer A + B — full curated set, ~600 assets, ~610 MB |
| `dev.assets.json` | Solo developer DB re-seed | Layer A + B unified — everything in one instance |

The four profiles + supporting catalogues (users, teams, collections,
brand workspaces, field definitions, workflow states) are produced from a
single Python script that reads the existing 12,871-row metadata.csv at
`/mnt/d/Projects/unraid_management/artist-alley_dataset/`.

## Pipeline

```
/mnt/d/Projects/unraid_management/artist-alley_dataset/
├── metadata.csv         12,871 rows, 41 cols
├── groups.csv           8,050 logical groups (sibling linkage)
└── <on-disk asset tree> 21 GB raw assets
              │
              │  python3 seed/scripts/sanitize_and_assemble.py
              ▼
seed/profiles/
├── dataset.MANIFEST.json
├── dataset.users.json              30 fictional artists + reviewers
├── dataset.teams.json              9 teams seeded
├── dataset.collections.json        16 projects → collections
├── dataset.brand_workspaces.json   Echo + Mirror (the 2 promoted franchises)
├── dataset.field_definitions.json  12 custom field definitions
├── dataset.workflow.json           5 states + 6 transitions
├── studio-a.assets.json            Mirror Studios side
├── studio-b.assets.json            Adventureworks side
├── dev.assets.json                 Unified set
└── demo.assets.json                Layer A only
              │
              │  seed/scripts/fetch_gaps.py
              ▼
seed/internet-fetched/
├── sintel-30s.mp4         Blender Foundation, CC-BY
├── bbb-30s.mp4            Blender Foundation, CC-BY
├── frankenstein.epub      Project Gutenberg, public domain
├── gltf-samples/          Khronos, CC-BY
├── nasa-imagery/          NASA, public domain
└── ...
              │
              │  seed/scripts/apply.sh  (planned)
              ▼
Running AA instance with seeded content
```

### The image upgrade pass (#604)

The source `metadata.csv` describes the ORIGINAL library, which was
dominated by sprite-sheet tiles — 183 site_a images under 1 KB, 71% of a
sample under 100px. That library was rebuilt from the CC0 Kenney pack,
and the upgrade is now part of the pipeline rather than a manual step:

```
"Kenney Game Assets All-in-1 3.6.0"/          (read-only source pack)
              │
              │  kenney_hq.py build     ← rebuilds the pool from
              ▼                            seed/upgrades/kenney-hq-pool.json
<pool>/  656 images (86 large PNGs copied, 570 vectors rendered @512px)
              │
              │  apply_upgrade.py       ← run automatically at the end of
              ▼                            sanitize_and_assemble.py
seed/profiles/studio-{a,b}.assets.json    916 records repointed at the pool,
                                          72 videos + their solo posts merged
              │
              │  populate_archive.py --hq-source <pool>
              ▼
<site>/  MANIFEST.json + metadata.csv now describe the UPGRADED library
```

**Why this is a pipeline stage and not a one-off script.** `populate_archive.py`
copies the profile straight over `MANIFEST.json` and regenerates the
per-site `metadata.csv` from the profile's path map. So the per-site files
are OUTPUTS: editing them by hand is undone by the next run. The profile
is the thing that has to be correct, which is what `apply_upgrade.py`
fixes. `seed/scripts/test_dataset_upgrade.py` fails if the committed
profiles ever drift back.

Three rules are encoded in the tooling because each one already caused a
silent data bug — see the module docstrings for the full story:

| | rule | what breaks without it |
|---|---|---|
| 1 | Name pool files by an 8-char hash of the **source path**, never the basename | Packs ship identical basenames in sibling directories (four packs contain `Tilesheet/tilesheet_complete_2X.png`). Slugging by basename silently overwrote 48 assets, then 65. The manifest still validated; the bytes were wrong. |
| 2 | Gate quality on **dimensions**, never on byte size | Flat-colour vector renders compress hard: 415 upgraded assets are under 10 KB *and* ≥512px. A byte threshold rejects exactly what the upgrade produces. |
| 3 | **Weight the sample explicitly** (`PACK_WEIGHTS`) | `Icons/Input Prompts` alone is 1,504 near-identical button glyphs, ~29% of every vector in the pack. Sampled evenly, browse looks like a settings screen. |

Rebuilding the pool needs the renderer installed once (no sudo):

```bash
cd seed/scripts && npm install sharp
```

## Studio split

Two studios with deliberately overlapping but distinct identities, so the
federation scenarios (Like, Share, Defederate, Restricted) have something
real to act on.

### Studio A — "Mirror Studios"

A VFX + cinematics + open-world shop. Owns the Echo + Mirror brand workspaces.

| Project | Source pack(s) | Notes |
|---|---|---|
| Project Mirror | light-masks-1.0 | VFX-heavy reference |
| Project Echo | retro-fantasy-kit + retro-textures-fantasy + rpg-audio + ui-pack-rpg-expansion + kenney_music-jingles + kenney_voiceover-pack | Fantasy RPG |
| Project Citylight | rpg-urban-pack | Top-down urban |
| Project Compass | minimap-pack | Open-world minimap |
| Art Research | The-Models-Resource game rips | Cinematic reference (Mario, Sonic, MvC2) |
| Snapdex | `/mnt/d/Projects/Snapdex/datasets/cards/` (50 cards) | Marketing reference |
| Engine Core (subset) | development-essentials + prototype-textures + kenney-fonts + format3d + font families | Mirror-flavoured tooling |
| Studio Library (subset) | Personal photos + Dresden Files (6 issues sampled) + reference docs | Format-coverage |

### Studio B — "Adventureworks"

A character + casual + UI shop. Owns no franchise brand workspaces, but
participates in the shared Echo brand.

| Project | Source pack(s) | Notes |
|---|---|---|
| Project Heroes | blocky-characters_20 | Stylized character RPG |
| Project Zoo | animal-pack-remastered | Kids / casual |
| Project Adventure | ui-pack-adventure | Adventure UI |
| Project Jumpstart | pixel-line-platformer | 2D pixel platformer |
| Project Toybox | IconKitchen-Output + icons + PixelSpaces Free Pack + PixelSpaces Full Pack + UISketch | UI kit + branding + wireframes |
| Hearthstone Archive | hearthstone_archive/* (100 groups) | Card-game reference |
| MTG Archive | mtg_img_archive/* (150 cards) | Card-game reference |
| Heroes Archive | heroes/* (30 portraits across 3 heroes) | Character reference |
| Engine Core (subset) | (different files from Mirror's subset) | Adventureworks tooling |
| Studio Library (subset) | (different files from Mirror's subset) | Format-coverage |

### Why this split

- **Both studios get format diversity.** Each has images (lots), audio
  (Kenney packs split between them), 3D (Mirror gets game-rip references;
  Adventureworks gets Kenney 3D), video (Studio Library is split by file
  hash), fonts (Engine Core has the font families, split between).
- **Both have restricted content.** Each studio's Art Research / Reference
  material is `sensitivity_tier=restricted` (community rips, internal
  attribution) — drives Scenarios 04 + 05 of the federation regression
  catalogue (pre-1.22.I refusal vs post-1.22.I encrypted delivery).
- **Engine Core overlap stress-tests CAS dedup.** Both studios have
  "Engine Core" but with non-overlapping file subsets. The handful of
  files that appear in both with identical content hashes exercise the
  storage-layer dedup across federation.
- **Brand workspace ownership is asymmetric.** Studio A owns the Echo
  brand workspace; Studio B works on Echo via the shared workspace. This
  is the canonical "publisher + supporting studio" relationship that
  motivates aa:Share + aa:Approve activities in the federation protocol.

## Brand workspace decision

Two of the dataset's five `franchise` values get promoted to full
brand_workspaces (per [ADR 0025](../docs/adr/0025-brand-workspaces.md)):

| Franchise | Treatment | Reason |
|---|---|---|
| **Echo** | brand_workspace | Shippable fantasy RPG IP — warrants brand kit, guidelines, style references |
| **Mirror** | brand_workspace | Shippable VFX-heavy game IP — same |
| Engine | tag (`franchise:engine`) | Internal tooling — not a customer-facing brand |
| Reference | tag (`franchise:reference`) | Third-party reference, not our brand |
| Snapdex | tag (`franchise:snapdex`) | External IP (Pokemon TCG), not our brand |

The promoted workspaces become demonstrable: opening the Echo workspace
shows the brand kit, guidelines doc, and asset library. The tag-only
franchises appear as metadata but don't carry workspace UI.

## Layer A vs Layer B (licensing tiers)

Not every asset in the source dataset is publicly redistributable. The
script tags every record with `layer = "A"` or `layer = "B"`:

### Layer A — public-safe

Ships in the Phase 1.48 demo sandboxes that anyone can spin up at
`demo.artist-alley.org`. Sources:

- Kenney.nl (CC0)
- PixelSpaces.io (CC0 / free pack)
- Google Fonts (Open Font License)
- UISketch dataset (CC0)
- Project Gutenberg (public domain, internet-fetched)
- NASA media library (public domain, internet-fetched)
- Blender Foundation films — Sintel, BBB, Tears of Steel (CC-BY, internet-fetched)
- Polyhaven HDRs (CC0, internet-fetched)
- Khronos glTF sample models (CC-BY, internet-fetched)
- LibriVox audiobook samples (public domain, internet-fetched)

### Layer B — local-only

Used in dogfood + dev re-seed where you control both peers. Sources:

- Snapdex (TCG) — Pokemon IP (Nintendo)
- Scryfall / Wizards of the Coast — MTG IP
- Hearthstone (Blizzard) — Blizzard IP
- Overwatch (Blizzard) — Blizzard IP
- The Models Resource — community game rips
- Type foundry license — proprietary fonts
- Dabel Brothers Publishing — Dresden Files comics

## Schema mapping (CSV → AA)

The CSV has 41 columns; AA's data model has its own shape. Mapping summary
([sanitize_and_assemble.py](scripts/sanitize_and_assemble.py) for the full
transform):

### Direct field mappings

| CSV column | AA target | Notes |
|---|---|---|
| `asset_id` (`ast-XX`) | `assets.id` (UUID gen) + metadata.external_id | Deterministic UUID via stable hash |
| `group_id` (`grp-NNNNN`) | `asset_companions` (siblings) | Sibling linkage |
| `file_path` | filesystem ingest path → `storage_objects.hash` | Hashed at ingest |
| `filename` | `assets.metadata.filename` | |
| `title` | `assets.title` | |
| `description` | `assets.description` | |
| `kind` | `assets.asset_type` | `raster/vector/design-source→image`, others 1:1 |
| `file_format` | `assets.file_extension` | |
| `file_size_bytes` | `assets.file_size_bytes` | |
| `tags` | `asset_tag` join | Split comma-separated |
| `team` | `teams.name` + `team_memberships` | Pre-seed 9 teams |
| `project` | `collections.name` | One collection per project |
| `franchise` | `brand_workspace.name` OR tag | Echo/Mirror = workspace; rest = tag |
| `artist` | `users.username` + `assets.owner_user_ref` | |
| `approver` | `users.username` + review actor | |
| `status` | `workflow_states.name` | Maps to 5-state workflow |
| `confidentiality` | `assets.sensitivity_tier` | Public→public, Internal→team, Restricted→restricted |
| `license` + `usage_rights` + `attribution` | `assets.metadata.rights` jsonb | |
| `source` | `assets.metadata.acquisition_source` | Also drives Layer A/B |
| `is_published` | `assets.archive_state` | true→active, false→draft |
| `archived_reason` | `assets.metadata.archive_reason` | When status=Archived |
| `external_id` | `assets.metadata.external_id` | Perforce/Jira ref |
| `review_notes` | `comments` row, scoped to approver | One per group's note |
| `created_at` / `updated_at` / `last_reviewed_at` | preserved as-is | |

### Custom field mappings (per ADR 0012)

The 12 columns that don't have a direct AA-model home become user-defined
field_definitions, with values landing in `asset_field_value`:

| CSV column | Field type | Options |
|---|---|---|
| `pipeline_stage` | select | Greybox / Pass 1 / Polish / Final / Locked |
| `version` | text | v1 / v2 / v3 |
| `revision_count` | number | |
| `rating` | number | 3-5 |
| `polycount` | number | 3D only |
| `texture_resolution` | select | 256x256 / 512x512 / 1024x1024 / 2048x2048 / 4096x4096 |
| `color_space` | select | sRGB / Linear / Raw / N/A |
| `loop_seconds` | number | Audio only |
| `runtime_seconds` | number | Video only |
| `engine_compatibility` | select | Unreal 5 / Unity 2022 / Godot 4 / All / N/A |
| `target_platforms` | multi_select | PC / Console / Mobile / All |
| `naming_compliant` | boolean | |

## Trimming caps (tunable in the script)

Three layers of caps enforce the 10 GB budget while preserving format-
coverage balance. All caps are tunable at the top of
`sanitize_and_assemble.py`.

### Pack trims (`TRIMS`)

Per-source-pack maximum row counts. Reduces card-archive + sprite-archive
floods.

```python
TRIMS = {
    "Snapdex (TCG)": 50,
    "hearthstone": 100,
    "mtg_img_archive": 150,
    "heroes": 30,
    "UISketch": 200,
    "Dresden Files Comics": 6,
}
```

### Project-level row caps (`PROJECT_ROW_CAPS`)

```python
PROJECT_ROW_CAPS = {
    "Studio Library": 60,
}
```

### Asset-type budget caps (`ASSET_TYPE_CAPS`)

Format-coverage enforcement. Applied after the schema transform, with a
deterministic shuffle so the cap pulls from across all packs.

```python
ASSET_TYPE_CAPS = {
    "image": 800,
    "audio": 200,
    "3d": 200,
    "video": 15,
    "comic": 6,
    "document": 50,
    "font": 30,
}
```

### Hard drops + size cap

- `HARD_DROP_PATH_PATTERNS`: substrings that mark a file as never-keep.
  Currently the 4.5 GB "Ramen Cooking Class" video and GoPro `.LRV`
  low-res variants.
- `MAX_FILE_SIZE_BYTES = 500 MB`: any single file above this cap is
  dropped unless explicitly whitelisted in `LARGE_FILE_WHITELIST`.

## Regenerating

```bash
# Dry-run — see summary stats without writing files
python3 seed/scripts/sanitize_and_assemble.py \
    --source /mnt/d/Projects/unraid_management/artist-alley_dataset \
    --out    /mnt/d/Projects/artist-alley/seed/profiles \
    --dry-run

# Actual run
python3 seed/scripts/sanitize_and_assemble.py \
    --source /mnt/d/Projects/unraid_management/artist-alley_dataset \
    --out    /mnt/d/Projects/artist-alley/seed/profiles
```

Re-running with the same inputs produces the same outputs (deterministic
UUIDs via stable hash; deterministic shuffle via fixed seed). The run ends
with the #604 upgrade pass, so the regenerated profiles describe the
upgraded library — `--skip-upgrade` reproduces the pre-upgrade assembly
and should never be used for a real site build.

Then publish to a site. Note `--hq-source`: without it the run refuses to
start rather than skipping 916 assets and exiting 0.

```bash
# 1. rebuild the image pool (needs `npm install sharp` once)
python3 seed/scripts/kenney_hq.py build \
    --pack "$DATASETS/Kenney Game Assets All-in-1 3.6.0" \
    --out  "$DATASETS/kenney-hq-pool"

# 2. copy assets + regenerate MANIFEST.json / metadata.csv for a site
python3 seed/scripts/populate_archive.py \
    --local-source    /mnt/d/Projects/unraid_management/artist-alley_dataset \
    --internet-source seed/internet-fetched \
    --hq-source       "$DATASETS/kenney-hq-pool" \
    --profile         seed/profiles/studio-a.assets.json \
    --dest            "$DATASETS/site_a"
```

`$DATASETS` is wherever the dataset share is mounted — it differs between
a workstation and a CI runner, so nothing in the tooling hardcodes it. If
the path looks empty, the share dropped: check `mountpoint` and remount
before concluding the data is gone.

Verify without changing anything:

```bash
python3 seed/scripts/test_dataset_upgrade.py          # 31 tests, no share needed
python3 seed/scripts/apply_upgrade.py --site site_a \
    --profile seed/profiles/studio-a.assets.json \
    --posts   seed/profiles/studio-a.posts.json --check
```

## What's not in here yet

These are tracked as follow-ups:

- **Internet-fetched gap-fillers.** `seed/scripts/fetch_gaps.py` will pull
  Sintel + BBB clips, Project Gutenberg EPUBs, Polyhaven HDRs, NASA
  imagery, Khronos glTF samples, LibriVox audiobook chapter samples.
  Targets Layer A. Tracked in [Phase 1.22.I-a](https://github.com/mscrnt/artist-alley/issues/98).
- **`apply.sh` script.** Reads a profile JSON + asset bytes + drives AA's
  API to materialize the seeded instance. Tracked in same phase.
- **R2 mirror of large assets.** Eventually move the source asset bytes
  off the unraid via Cloudflare tunnel and onto an R2 bucket addressed by
  content hash. For now, the script reads from the local path.
- **Per-asset attribution surface in the UI.** Attribution is preserved
  in `assets.metadata.attribution`. A "Demo content licenses" admin page
  collecting all attributions is planned.
