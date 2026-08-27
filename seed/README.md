# seed/ — demo + dogfood + dev-reseed dataset

> **Looking to populate an instance?** You do not need this file.
> See [`SEED_INSTRUCTIONS.md`](SEED_INSTRUCTIONS.md), or grab the
> ready-made public dataset:
> **https://www.kaggle.com/datasets/mscrnt/dam-population-seed**
>
> **This document describes how the maintainer *builds* that dataset**
> from a private source archive. Every concrete path below is one
> maintainer's local mount — they will not exist on your machine and are
> shown only to make the pipeline legible. Nothing here is required to
> use the seeder.

This directory holds everything needed to populate a fresh artist-alley
instance with a representative studio-shaped dataset. Four profiles are
emitted from one canonical source:

| Profile | Where it ships | Content |
|---|---|---|
| `demo.assets.json` | Phase 1.48 public demo sandboxes at `demo.artist-alley.org` | **Layer A only** — public-safe (Kenney CC0, PixelSpaces, Google Fonts, UISketch, internet-fetched CC-BY/CC0 samples) |
| `studio-a.assets.json`, `studio-b.assets.json`, `dev.assets.json` | Local dogfood + dev re-seed only | Layer A + B. **Not distributed** — Layer B is third-party material the project does not redistribute, so these profiles are only usable by the maintainer, who has the source archive. |

`demo` and `dev` are byte-for-byte **aliases** of the two dogfood
profiles. They used to be written before the upgrade pass ran, so
every upgrade since #604 landed on `studio-{a,b}` and missed its own
aliases — `demo.assets.json` shipped 971 records against `studio-a`'s
1,007, and a demo re-seed would have dropped all 36 added videos with
nothing to notice it. `sanitize_and_assemble.py` now re-copies them
after the upgrade, and `test_dataset_upgrade.py` asserts the equality
(#572).

The four profiles + supporting catalogues (users, teams, collections,
brand workspaces, field definitions, workflow states) are produced from a
single Python script that reads the existing 12,871-row metadata.csv at
`$DATASET_SRC/`.

## Pipeline

```
$DATASET_SRC/
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
├── studio-a.assets.json            local dogfood peer A
├── studio-b.assets.json            local dogfood peer B
├── dev.assets.json                 unified local set
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

**`--check` is the pre-publish gate, and it now covers every pass (#1295).**
It re-runs the whole upgrade in memory and exits non-zero if anything would
change, naming only the passes that actually fired. ⚠️ The replacement pass
was invisible to it until 2026-08-26, because `apply_replacements` returned
records *processed* — `260/260 records repointed at the HQ pool` on every
run, upgraded or not — and a number that is never zero cannot be a drift
signal. It now returns processed **and** modified; the progress line prints
both (`260/260 … (0 modified)`) and the gate reads the second. That gap is
how studio-b carried 86 stale `file_size_bytes` past the gate for weeks
while `file_path` — the only field the older test compared — was correct on
every one of them.

### ⛔ The publish guard (#1275)

That "the per-site files are OUTPUTS" property has a sharp edge, and it
drew blood. The per-site `MANIFEST.json` kept being edited by hand, so by
2026-08-26 the **published** site_a was ahead of the committed profile by
one whole asset and **12,096 values across 1,947 records** — and the next
ordinary `populate_archive.py` run would have deleted every one of them
without printing a word.

Two things now stand in the way:

- **`manifest_guard.py`** compares the profile against `<dest>/MANIFEST.json`
  and `<dest>/posts.json` **before any write, in `--dry-run` too**, and
  refuses if the destination holds records or values the profile does not.
  A missing key, an emptied value and a *different* value are three cases:
  the first two are losses and refuse; the third is an edit and is allowed,
  because a profile that cannot correct what it has published is useless.
  `--allow-regression` overrides it when the removal really is the point.
  Duplicate ids in the source are refused outright and are **not**
  overridable — a manifest cannot hold two records under one id, and
  `aa seed` silently takes whichever it reads last.
- **`seed/upgrades/manifest-reconcile.site_a.json`** carries the published
  site's content back into the profile so the guard can ever pass. The pass
  that applies it (`apply_manifest_reconcile`) **can only add, never
  replace**, which is what makes it safe to re-run over a later edit.

⛔ **Do not hand-edit a per-site `MANIFEST.json` again.** It is an output.
Put the change in `seed/upgrades/` and let `apply_upgrade.py` fold it into
the profile, which is the only side the pipeline reads.

### ⚠️ A byte count is a MEASUREMENT, not content (#1294)

The rule above is about *content* — which records exist and what values
they carry. `file_size_bytes` on a pool record is a different kind of
thing: it is the size of a file the pipeline **produces**, so there is
only ever one right answer and the profile's job is to describe it.

Nothing re-derived those numbers. `newSize` in
`kenney-hq-replacements.<site>.json` was measured once, by hand, on
whatever pool existed that day — and a pool file is a *render*, so every
rasteriser fix invalidated a slice of them silently. #630 and #685 both
changed what frame a vector is drawn into; #685 alone took
`vector_backgrounds` from an 8.8%-of-the-artwork crop to the whole
drawing, roughly tripling its bytes.

Measured 2026-08-26 against a pool rebuilt from the committed manifest:
**150 of site_a's 260 replacement rows and 472 of site_b's 656** named a
size the file does not have. The repository was the stale side —
site_a's published share agreed with the *rebuilt* pool on 776 of 777
records, and `balance-assets.site_a.json` (emitted after the fixes) had
been contradicting the replacements docs on **115 pool files** the whole
time, entirely inside the repo.

Re-measuring is a command, not a procedure:

```bash
python3 seed/scripts/kenney_hq.py build \
    --pack "$DATASETS/Kenney Game Assets All-in-1 3.6.0" --out /tmp/kenney-hq
python3 seed/scripts/kenney_hq.py sizes --pool /tmp/kenney-hq \
    --replacements seed/upgrades/kenney-hq-replacements.site_a.json \
    --replacements seed/upgrades/kenney-hq-replacements.site_b.json --write
python3 seed/scripts/apply_upgrade.py --site site_a \
    --profile seed/profiles/studio-a.assets.json \
    --posts   seed/profiles/studio-a.posts.json      # then site_b
```

⚠️ **The pool is BUILT, not shipped.** There is no `hq` directory on the
archive share; `--out` has no default and the build takes the CC0 pack
plus `npm install sharp` in `seed/scripts/`. Without `--write`, `sizes`
reports and exits non-zero, so it can stand as a gate.

⛔ **Two committed documents describing the same pool file must agree**,
and that is now a test (`TestPoolSizesAgreeAcrossDocuments`) which needs
neither the pool nor the share. It was red on 115 files before this
repair.

### The corpus carries every AI state (#1290)

`ai_provenance` has four states and the dataset declared **one**.
`assisted` and `none` existed only as soft-deleted test fixtures, so
neither had ever been rendered for a human — and `none` is the state a
wrong rendering damages most, because it must never become a visible
"no AI" claim.

Re-labelling existing records was not available. Every other asset here is
a third party's work or one of #1260's 45 Stable Diffusion plates that
already declare `generated` with their provenance saying so on the same
row; writing `none` over somebody's photograph is the fabricated
disclosure ADR 0094 forbids, and re-declaring an SD plate `assisted` would
contradict its own `acquisition_source` — the #1260 error exactly. So the
corpus gained **two new in-house plates** instead:

| plate | declares | why that is true of it |
|---|---|---|
| `studio-colour-chart.png` | `none` | 24 patches, a 21-step ramp and registration marks, every pixel placed arithmetically. No model anywhere in it. |
| `reference-mood-board.png` | `assisted` | One #1260 SD plate as its left panel and a palette sampled from that plate's own pixels, with the swatch grid and rules drawn here. Part model, part not — neither `generated` nor `none` would be true. |

⚠️ **The repo carries the recipe, not the bytes**, same as `kenney_hq.py
build`. Run this once against the dataset source before the next
`populate_archive.py`:

```bash
python3 seed/scripts/authored_plates.py build \
    --generated-source $DATASET_SRC/aurora-generated \
    --out             $DATASET_SRC/aurora-authored
```

Deterministic — the same inputs give byte-identical output, so a rebuild
does not churn `file_size_bytes`. Stdlib only, so a machine without
`sharp` can still run it.

⛔ `AI_DECLARABLE_SOURCE_PREFIXES` (Python) and
`AIDeclarableSourcePrefixes` (Go, `app/internal/seed/catalogues.go`) gate
**every** state, `none` included, and must be widened together — they are
deliberately separate checks over different files, which also means they
can drift.

Three rules are encoded in the tooling because each one already caused a
silent data bug — see the module docstrings for the full story:

| | rule | what breaks without it |
|---|---|---|
| 1 | Name pool files by an 8-char hash of the **source path**, never the basename | Packs ship identical basenames in sibling directories (four packs contain `Tilesheet/tilesheet_complete_2X.png`). Slugging by basename silently overwrote 48 assets, then 65. The manifest still validated; the bytes were wrong. |
| 2 | Gate quality on **dimensions**, never on byte size | Flat-colour vector renders compress hard: 415 upgraded assets are under 10 KB *and* ≥512px. A byte threshold rejects exactly what the upgrade produces. |
| 3 | **Weight the sample explicitly** (`PACK_WEIGHTS`) | `Icons/Input Prompts` alone is 1,504 near-identical button glyphs, ~29% of every vector in the pack. Sampled evenly, browse looks like a settings screen. |
| 4 | Record a **direct media URL**, not the page you found it on | The 30 Pexels videos stored `https://www.pexels.com/video/…/` — an HTML page behind Cloudflare. Verify-don't-copy kept working as long as the archive share was mounted, so nothing ever failed here; a rebuild on a clean machine simply could not reconstruct them (#602). |

### The per-team balance pass (#572)

site_a shipped 1,007 assets across 11 teams with **Animation and
Characters at zero**, Marketing Art at 3, Textures at 8 — and
Environment holding **47.3%** of the whole library. Clicking a studio
either showed nothing or showed the dataset.

The issue assumed this needed a cross-studio rewrite because "Animation
has 2 assets globally". That was true of the *manifest*, not the
*source*: those 1,007 records are drawn from a 78,441-file CC0 bundle of
which ~1.3% was in use. So the fix is additive, in two levers:

| lever | what it does | effect |
|---|---|---|
| `team-corrections.site_a.json` | moves 55 records the source CSV mis-teams — 34 minimap icons tagged `ui`, 18 texture plates tagged `texture`, 3 voiceover clips | Environment 476 → **421** |
| `balance-assets.site_a.json` | 895 new pack-sourced records for the starved teams | everyone else up; Environment's SHARE 47.3% → **21.6%** |

Nothing is deleted. Posts, collections and group siblings all reference
those ids, and #604's "swap the file, keep the record" rule exists
because removing records from this dataset breaks composition silently.

**The floor is 60, and it comes from the product.** `/search` returns 25
results per page and the browse rails render 24 tiles, so a team whose
entire library fits in one response has nothing to scroll and nothing to
narrow — it reads as a stub even when it is technically non-empty. 60 is
two and a half pages. The smallest team lands at 116.

```bash
# what would change, per team, writing nothing
python3 seed/scripts/studio_balance.py plan --pack "$DATASETS/Kenney Game Assets All-in-1 3.6.0"

# write the upgrade docs (pool entries + assets + posts + corrections)
python3 seed/scripts/studio_balance.py emit --pack "$DATASETS/..."
# then build the pool, then record the render sizes it produced
python3 seed/scripts/studio_balance.py sizes --pool "$DATASETS/kenney-hq-pool"

# offline gate — no network, no share
python3 seed/scripts/studio_balance.py check
```

A fifth source root, `pack`, copies bytes verbatim out of the bundle
(3D models, audio, already-large PNGs). Vectors still go through the
kenney-hq pool so they are RENDERED by the pipeline's own rasteriser
(#679); `studio_balance.py emit` appends them to `kenney-hq-pool.json`
so `kenney_hq.py build` reproduces them like any other pool file.

### Provenance for bundle-sourced records (#572)

The All-in-1 bundle is a paid itch.io download, so until #572 the ~90%
of the library that is Kenney had **no internet provenance at all** —
the same hole #602 closed for 30 Pexels videos, larger and quieter,
because verify-don't-copy never fails while the mount is up.

Kenney also publishes every pack individually as a free CC0 zip, and
those zips are byte-identical to the bundle. So each added record
carries:

| key | what it is |
|---|---|
| `metadata.fetched_from` | `kenney.nl/assets/<slug>` — the pack page, where the CC0 dedication lives |
| `metadata.source_archive.url` | the pack's free zip |
| `metadata.source_archive.member` | the path inside that zip |
| `metadata.source_archive.sha256` | that member's bytes |

**Not `media_url`.** A `media_url` means "a URL that serves exactly
`file_size_bytes`", and `populate_archive.py` refuses a mismatch. A zip
serves the whole pack, so writing one there would either break that
check or disable it. The per-member sha256 does the job the byte count
does for a direct URL, and it is checked *before* the bytes are staged —
a pack that moved upstream fails loudly instead of quietly substituting
different art under an unchanged manifest entry.

If a pack has no public standalone download it does not go in the
dataset (`kenney_pack_sources.NOT_PUBLISHED_STANDALONE`); that rule cost
Characters ~50 records it could otherwise have had from the "Animated
Characters Bundle", and re-fetchability won.

```bash
# resolve pages + zip URLs for every pack the recipes name (network)
python3 seed/scripts/kenney_pack_sources.py resolve --packs-from-recipes

# offline gate: recipes vs recorded provenance
python3 seed/scripts/kenney_pack_sources.py check

# prove every SHIPPED record reproduces from its zip (network)
python3 seed/scripts/kenney_pack_sources.py verify-records \
    --records seed/upgrades/balance-assets.site_a.json
```

### Provenance: `fetched_from` vs `media_url` (#602)

Every internet-sourced record carries **both**, and they answer different
questions:

| key | what it is | who uses it |
|---|---|---|
| `metadata.fetched_from` | the page a human was looking at | attribution, licence evidence, ATTRIBUTIONS.md / Kaggle paperwork |
| `metadata.media_url` | the direct, unauthenticated URL of the exact bytes | `populate_archive.py` re-fetch, any from-scratch rebuild |

For most sources they are the same URL. For Pexels they are not, and that
is the entire point — the page is where the licence lives, the CDN path is
where the mp4 lives. A `media_url` is only ever written after a HEAD
returns `Content-Length` equal to the record's `file_size_bytes`, so it is
evidence rather than a plausible-looking string.

```bash
# resolve + write (needs network; FLARESOLVERR_URL for the Cloudflare-guarded pages)
python3 seed/scripts/resolve_media_urls.py --write \
    seed/upgrades/added-assets.site_a.json seed/profiles/studio-a.assets.json

# offline gate — no network, no share
python3 seed/scripts/resolve_media_urls.py --check seed/profiles/*.assets.json

# prove the round trip against the staged copies
python3 seed/scripts/resolve_media_urls.py --refetch /tmp/out \
    --against $SEED_SITE \
    seed/profiles/studio-a.assets.json
```

`populate_archive.py` uses `media_url` on its own: a pre-staged record
that is absent at the destination is **downloaded** instead of reported
MISSING. That only happens for records that would otherwise fail, so a
normal run over a populated share does no network I/O. `--no-refetch`
restores the old verify-only behaviour.

Rebuilding the pool needs the renderer installed once (no sudo):

```bash
cd seed/scripts && npm install sharp
```

### What is on the share but not in the catalogue (#722)

A file on a site that no `MANIFEST.json` record names is **three
different situations**, and answering "what is uncatalogued?" without
separating them produces a confident wrong number every time. site_a has
461 such files:

| kind | site_a | correct response |
|---|---|---|
| **companion** — reachable from a catalogued `.gltf` (`images[].uri`, `buffers[].uri`) or `.obj` (`mtllib` → `.mtl` → `map_*`) | 201 | **nothing.** `Runner.applyAssetCompanions` registers these against their parent asset at seed time. A record of its own would double-count the bytes and detach the texture from its model. |
| **superseded** — the `old` file of a #604 HQ replacement | 260 | **delete.** The record still exists; it was repointed at a `kenney-hq` render and the original was left behind. |
| **orphan** — neither | **0** | catalogue it, via an upgrade doc (never by editing `MANIFEST.json` — see above). |

The companion walk is **two hops**. Stop at `mtllib` and every OBJ
texture reads as an orphan, which over-counts site_a's gap by ~200 files.
`audit_uncatalogued.py` delegates to
`populate_archive.resolve_model_companions`, the Python twin of the Go
`format3d.ResolveCompanions` the seed runner registers with, so the
audit cannot drift from what the running instance believes.

**#722 was filed as "260 assets were never catalogued".** They were: that
set is *exactly* the `old` column of
`kenney-hq-replacements.site_a.json` — 260 replacements, 260 stranded
files, byte-identical sets. `apply_upgrade.apply_replacements` repoints
the record's `file_path`; nothing deletes the file it used to point at,
and `populate_archive.py` only removes unwanted files under `--prune`. So
every replacement leaves one file behind that is indistinguishable from a
never-catalogued asset unless you read that column. Cataloguing them
would give 260 pieces of content two records each and re-introduce what
the upgrade removed — including a 1×1 white pixel and 169 files under
64 px.

```bash
# classify; exit 1 only on a REAL gap
python3 seed/scripts/audit_uncatalogued.py detect --site site_a \
    --site-root "$DATASETS/site_a" --fail-on-orphans

# delete the superseded originals (reports only, until --apply)
python3 seed/scripts/audit_uncatalogued.py prune --site site_a \
    --site-root "$DATASETS/site_a" --apply
```

`prune` re-derives the live `file_path` set from the manifest and refuses
to delete anything still in it, so a reverted replacement cannot be
turned into data loss by a stale doc.

## Brand workspace decision

Two of the dataset's five `franchise` values get promoted to full
brand_workspaces (per [ADR 0025](../docs/adr/0025-brand-workspaces.md)):

| Franchise | Treatment | Reason |
|---|---|---|
| **Echo** | brand_workspace | Shippable fantasy RPG IP — warrants brand kit, guidelines, style references |
| **Mirror** | brand_workspace | Shippable VFX-heavy game IP — same |
| Engine | tag (`franchise:engine`) | Internal tooling — not a customer-facing brand |
| Reference | tag (`franchise:reference`) | Third-party reference, not our brand |
| Third-party franchises | tag (`franchise:<name>`) | External IP, not our brand |

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
- 45 images generated in-house with Stable Diffusion 3.5 Large (CC0),
  the only entries in the corpus that declare `ai_provenance` (#1260)

### Layer B — local-only, never published

Some source material is **not** publicly redistributable: third-party
game/card IP, licensed fonts, and licensed comics. Those records are
tagged `layer = "B"` and are used only for local dogfood and dev
re-seeding, where the operator controls both peers.

**Layer B never ships.** It is excluded from `site_a`, from the demo, and
from the published dataset. This file does not enumerate it — publishing
an inventory of material we deliberately do not distribute serves nobody,
and the tagging is what enforces the boundary, not the list.

If you are assembling your own dataset, the same discipline applies:
tag anything you cannot redistribute and keep it out of whatever you
publish.

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
| — | `assets.mature` | Manifest `mature` (bool), a CONTENT RATING and a second axis — never derived from the tier (#1217, ADR 0090). Twelve public-domain classical nudes carry `true`; see ATTRIBUTIONS.md for the list and the reasoning. Posts get theirs from a DB trigger off membership, so `posts.json` says nothing about it |
| — | `assets.ai_provenance` | Manifest `ai_provenance` (string), the MAKER'S declaration (#1251, ADR 0094). The key's ABSENCE is `NULL` = "nobody was asked", never `none` — writing `none` over a work nobody was asked about would fabricate that maker's disclaimer. 45 in-house generated images declare `generated`; nothing else in the corpus declares anything (#1260, and see ATTRIBUTIONS.md for why the four rows that used to are gone). Posts get `ai_provenance` + `ai_pure` from DB triggers off contributors, so `posts.json` says nothing about it |
| `license` + `usage_rights` + `attribution` | `assets.metadata.rights` jsonb | |
| `source` | `assets.metadata.acquisition_source` | Also drives Layer A/B |
| — | `assets.metadata.fetched_from` | Source PAGE — attribution + licence evidence (#602) |
| — | `assets.metadata.media_url` | Direct byte URL — what a rebuild GETs (#602) |
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
    "<source-pack-name>": 50,     # per-pack row cap
    "UISketch": 200,
    # … see sanitize_and_assemble.py for the live values
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
    --source $DATASET_SRC \
    --out    $REPO/seed/profiles \
    --dry-run

# Actual run
python3 seed/scripts/sanitize_and_assemble.py \
    --source $DATASET_SRC \
    --out    $REPO/seed/profiles
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

# 2. copy assets + regenerate MANIFEST.json / posts.json / metadata.csv
python3 seed/scripts/populate_archive.py \
    --local-source    $DATASET_SRC \
    --internet-source seed/internet-fetched \
    --hq-source       "$DATASETS/kenney-hq-pool" \
    --pack-source     "$DATASETS/Kenney Game Assets All-in-1 3.6.0" \
    --profile         seed/profiles/studio-a.assets.json \
    --posts           seed/profiles/studio-a.posts.json \
    --dest            "$DATASETS/site_a"
```

**Pass `--posts`.** `aa seed` reads `posts.json` next to
`MANIFEST.json`, and until #572 nothing staged it — it was copied by
hand, so site_a served 584 posts against a profile holding 859 and the
only symptom was a thinner browse wall than the dataset claimed.

`--pack-source` is optional: a record whose bundle file is absent is
downloaded from its `metadata.source_archive` zip instead, so a machine
that has never seen the bundle can still build the site.

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
  Targets Layer A. Tracked in [Phase 1.22.I-a](https://github.com/Artist-Alley-Org/artist-alley/issues/98).
- **`apply.sh` script.** Reads a profile JSON + asset bytes + drives AA's
  API to materialize the seeded instance. Tracked in same phase.
- **R2 mirror of large assets.** Eventually move the source asset bytes
  off the unraid via Cloudflare tunnel and onto an R2 bucket addressed by
  content hash. For now, the script reads from the local path.
- **Per-asset attribution surface in the UI.** Attribution is preserved
  in `assets.metadata.attribution`. A "Demo content licenses" admin page
  collecting all attributions is planned.
