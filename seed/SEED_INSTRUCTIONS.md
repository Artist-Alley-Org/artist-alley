# Seed instructions — how to populate an Artist Alley instance

This document is for **operators** populating an Artist Alley instance
with a seed dataset.

## Getting a dataset

**Use the public one.** A ready-made, studio-shaped archive is published on
Kaggle — 1,947 assets across images, audio, 3D, video, documents and fonts,
with owners, teams, collections, workflow states and custom fields already
populated:

> **https://www.kaggle.com/datasets/mscrnt/dam-population-seed**

Download and unpack it anywhere; the directory you unpack to is the
`--site` path below.

**Or bring your own.** `aa seed` reads any directory laid out the same way —
a `MANIFEST.json` describing each asset plus the media files it references.
See the manifest in the Kaggle dataset for the exact shape.

> **Note on licensing:** the published dataset is a mix of CC0, CC-BY,
> public-domain, Pexels-licensed and one SCEA-licensed asset. There is no
> single aggregate licence — each asset carries its own `license` and
> `attribution` in `MANIFEST.json`, and `ATTRIBUTIONS.md` lists every
> source. Honour the per-asset terms if you redistribute.

The paths shown in the examples below are **one maintainer's local mounts**.
Substitute your own — nothing here depends on those specific locations.

### Where the maintained dataset lives (#1319)

⛔ **The old source dataset is gone, and it is not coming back.** Maintainer
docs and older scripts refer to a `$DATASET_SRC` tree at
`/mnt/d/Projects/unraid_management/artist-alley_dataset`. It has been
permanently retired and no longer exists. **The maintained datasets are the
published trees** under `/mnt/blackbox_archives/datasets/artist_alley`
(`site_a`, `site_b`), and for the `local` source root they are the **only**
copy of the bytes: measured on the committed profiles, 0 of 696 site_a and 0 of
552 site_b `local` records carry a `metadata.media_url` or a
`metadata.source_archive`, so there is nothing to re-fetch or re-derive them
from.

Authority is per source root:

| root | authority |
|---|---|
| `hq`, `pack` | the external Kenney pack / the attested `metadata.source_archive` member. Still re-derivable. |
| `local` | **archive-authoritative (preserved)**: a frozen snapshot of the published tree, attested by an external hash manifest. |
| `site`, `internet`, `torrent_import` | pre-staged at the destination, as before. |

What that means for an operator running the publish tooling:

* `populate_archive.py` needs **`--preserved-roots`** to treat `local` that
  way, and the mode is **never inferred**. Omitting `--local-source` without
  the flag is an error, because a fallback cannot tell a decision from a typo.
* In that mode `--local-source` is **refused as meaningless**, and
  `metadata.csv` is **never regenerated**. It changes only the way a
  `--csv-transform` document written *before* the run says it may. Pointed at
  the archive, the old regeneration matched 0 of 907 site_a rows and 0 of 1,206
  site_b rows and wrote a header-only file.
* `groups.csv` is left untouched and any byte change fails the preservation
  check. ⛔ Do not "correct" its `asset_count`: it describes the original
  dataset, not the cut a site ships.
* A source root that **is**, contains, or sits inside `--dest` is refused
  outright. The published archive is the thing being written, so it cannot also
  be the thing being read as authority.

```bash
# 1. attest a frozen snapshot, immediately after taking it (bytes only)
python3 seed/scripts/preserved_archive.py snapshot-manifest \
    --snapshot $FROZEN/site_a --out $EVIDENCE/site_a.snapshot-manifest.json

# 2. record the ONE way metadata.csv may change, BEFORE the run
python3 seed/scripts/preserved_archive.py csv-transform \
    --site $FROZEN/site_a \
    --collapse-document seed/upgrades/asset-collapse.studio-a.json \
    --out $EVIDENCE/site_a.csv-transform.json

# 3. publish
python3 seed/scripts/populate_archive.py --preserved-roots \
    --internet-source seed/internet-fetched \
    --hq-source $POOL --pack-source "$PACK" \
    --profile seed/profiles/studio-a.assets.json \
    --posts   seed/profiles/studio-a.posts.json \
    --csv-transform     $EVIDENCE/site_a.csv-transform.json \
    --frozen-snapshot   $FROZEN/site_a \
    --snapshot-manifest $EVIDENCE/site_a.snapshot-manifest.json \
    --dest /mnt/blackbox_archives/datasets/artist_alley/site_a --dry-run

# 4. verify, read only
python3 seed/scripts/verify_site.py check \
    --profile seed/profiles/studio-a.assets.json \
    --posts   seed/profiles/studio-a.posts.json \
    --site    /mnt/blackbox_archives/datasets/artist_alley/site_a \
    --baseline $EVIDENCE/site_a.baseline.json \
    --csv-transform $EVIDENCE/site_a.csv-transform.json
```

Every evidence document lives **outside** every site tree, and the emit
commands refuse an `--out` under the tree they describe.

The loader is **`aa seed`** — a subcommand of the app binary (#321).
It writes **straight to postgres + the storage backend** via the app's
own service layer: no running server, no admin login, no HTTP. It reads
its `config.Load()` environment (DB creds, `AA_STORAGE_*`, keys) exactly
like the server, so the simplest way to run it is a one-off `docker
compose run` off the app image — same DB, same storage volume, same
network as the instance you're seeding.

## Quick start

Against a running dev stack (`AA_BOOTSTRAP_DEFAULT_ADMIN=1` in
`docker-compose.yml` gives the bootstrap admin `admin /
ArtistAlleyMogul`, which owns the seeded collections):

```bash
# wherever you unpacked the dataset
export SEED_SITE=/path/to/dam-population-seed

docker compose run --rm --no-deps \
    -v "$SEED_SITE:/seed/site:ro" \
    -v "$PWD/seed/profiles:/seed/profiles:ro" \
    app seed --site /seed/site --catalogue /seed/profiles
```

Flags:

- `--site` — the populated site dir (`MANIFEST.json` + `posts.json` +
  the asset bytes). Required.
- `--catalogue` — the profiles dir (`seed/profiles`). Default
  `seed/profiles`.
- `--reset` — TRUNCATE the content tables + drop the fictional
  (non-admin) users before loading, so a re-run starts clean. The
  bootstrap admin + baseline lookups survive. Omit on a fresh DB.
- `--profile ci` — seed a **coverage-complete subset** instead of the
  whole catalogue (#768). Use this for CI; use the default (`full`) for
  the demo and for human review.
- `--coverage-depth N` — with `--profile ci`, the minimum posts per
  collection and assets per extension, bounded by what the catalogue
  holds. Default `8`. This, not the set-cover, is what sizes the seed.
- `--limit-per-extension N` — the older shrink: keep at most N assets
  per file extension, cascade-dropping any post that referenced a cut
  asset. Superseded by `--profile ci` and **mutually exclusive** with
  it. Kept for the nightly, which still passes `3`.

For the dogfood studio-b stack, `scripts/dogfood/seed.sh --site <dir>`
wraps the same call against the `app-b` service.

### Why `--profile ci` and not `--limit-per-extension`

They select on opposite axes, and the axis is the whole point.

`--limit-per-extension` keeps N assets per extension, then keeps a post
only if **every** asset it references survived. Posts are where the
relations live — author, team, collection, tags, multi-asset ordering,
comments — so it sheds precisely what a UI suite needs. Measured against
`site_a`'s catalogue:

| N | assets | posts (of 859) | collections with posts | bytes |
|---|---|---|---|---|
| 3 | 44 | 32 | 5 / 7 | 2.99 GiB |
| 8 | 100 | 64 | 6 / 7 | 4.03 GiB |
| 20 | 198 | 112 | 7 / 7 | 4.27 GiB |

Note the bytes column: it barely moves, because the giant video files are
the only members of their extensions and so survive every N. The
extension limit sheds relations, not wall clock.

`--profile ci` selects **posts** and closes over their assets, so every
kept post is whole by construction. It runs a greedy set-cover over a
universe of coverage dimensions built from the catalogue itself
(extension, asset type, workflow state, sensitivity tier, collection,
team, post kind, typed field code, plus the relation classes:
has-companions, has-tags, has-review-notes), then applies the depth
floor, then adds back any extension no post can reach.

Against `site_a` (1,947 assets / 859 posts) at the default depth:

| | full | `--profile ci` |
|---|---|---|
| assets | 1,946 | 148 |
| posts | 847 | ~87 |
| bytes read + hashed + re-written | 4.88 GiB | 2.89 GiB |
| extensions | 18 / 18 | 18 / 18 |
| collections with content | 7 / 7 | 7 / 7 |
| coverage dimensions | 113 / 113 | 113 / 113 |
| seed step, same machine + storage, cold volumes | 145s | ~30s |

The 179-test Playwright `standalone` suite passes against both.

### The depth floor is bounded by render cost, not just supply

`aa seed --previews` (the default) **enqueues** preview jobs and
returns; the app's worker pool renders them afterwards. Timing every job
to completion against this profile before that was accounted for:

| job type | jobs | total CPU | slowest |
|---|---|---|---|
| `preview.video` | 20 | 1141.7s | 426.6s |
| `preview.3d` | 40 | 326.0s | 16.9s |
| `preview.raster` | 65 | 57.1s | 11.3s |
| all other types | 32 | 12.4s | 1.3s |

Video sprite generation was **74% of all render CPU from 13% of the
assets**, and the render tail ran for **627s after `aa seed` exited** —
landing on whatever ran next. So video extensions get a lower floor
(`videoExtensionFloor`) than the rest: coverage needs the extension
present and a second asset to show it is not a fluke, not eight of them.
Images supply the grid density the floor exists for at a thousandth of
the cost.

CI additionally waits for the preview queue to drain between the seed
and the suite — bounded, and non-fatal on expiry. See
`.github/workflows/ui-pr.yml`.

Selection is byte-aware: among candidates that cover the same thing it
takes the cheapest, because the seed step's wall clock goes on reading,
hashing and re-writing bytes, and `site_a`'s byte weight is extremely
skewed (its twenty largest assets are 85% of 4.88 GiB).

That skew is also why the byte reduction is only 1.6x against a 12x
reduction in rows: **2.65 GiB of the 3.09 GiB is four files** — the only
`.mkv`, the only `.mov`, and both `.avi`s in the catalogue. They are
irreducible as long as every extension must stay covered. Re-encoding
those four to short clips is a **dataset** change that would take the CI
seed to roughly 0.44 GiB; no amount of selection cleverness can.

It **fails the run** — it does not warn — when the mounted catalogue
cannot supply a declared coverage class (see `requiredDims` in
`app/internal/seed/coverage.go`). A silently-degraded CI fixture is what
let an untextured 3D catalogue ship green twice (#750, #753); per ADR
0068 a fixture must be able to exercise what it claims to cover.

## What `aa seed` does

Nine dependency-ordered phases (mirrors the retired apply.py, minus the
HTTP round-trips and the separate timestamp-backfill pass):

1. **resolveLookups** — read the baseline-seeded `workflow_states` +
   `asset_types` and build the maps the later phases resolve against.
   The bootstrap admin's `user.ref` is looked up here (owns collections).
2. **applyUsers** — insert each fictional user (+ a federation keypair),
   idempotent on username.
3. **applyTeams** — teams + self-closure rows; slug derived from name.
4. **applyMemberships** — link each user to their `primary_team`.
5. **applyFields** — custom field definitions, idempotent on code.
6. **applyCollections** — one per project, owned by the bootstrap admin.
7. **applyAssets** — for each MANIFEST asset: write the bytes into the
   content-addressed store (hash), then insert the asset row + tags +
   collection membership + typed field values. A byte-identical asset
   the same owner already holds is collapsed by the
   `(owner_user_ref, file_hash)` unique index — the same refusal the
   app gives a duplicate re-upload.

   Measured 2026-09-22 on the committed corpus: `studio-a.assets.json`
   holds **2,006** rows, all ids distinct, and carries **no** same-owner
   produced-byte duplicate, so it yields **2,006** assets. It held one
   such duplicate until #1319 retired it by document. The live
   pre-republish `site_a/MANIFEST.json` is still the earlier build at
   **2,005** rows; the profile is the source of truth (ADR 0098) and
   those two converge at the next publish.
8. **applyPosts** — post row + members (asset_ids) + tags + collection
   linkage. A post whose referenced assets were all dropped (dedup or
   `--limit-per-extension`) is skipped.

   Measured 2026-09-22 on the committed corpus: `studio-a.posts.json`
   holds **863** posts, all ids distinct (the duplicate-id rows #1275
   describes are collapsed by `apply_upgrade.py`'s `dedupe_posts` pass
   before the profile is written), and **0** of them lose every member,
   so all 863 land. The live pre-republish `site_a/posts.json` is the
   earlier build at 861.
9. **applyComments** — forge a reviewer comment for each asset with
   non-empty `review_notes`, threaded onto the first post containing
   that asset. Deterministic comment UUID → idempotent.

Timestamps are written inline at insert time (each asset/post carries
its dataset `created_at`/`updated_at` directly), so no separate backfill
phase is needed. `aa seed` logs per-phase counts and a final
`seed.complete` tally, and exits non-zero on any phase failure — that
exit code is the verification gate (it replaces apply.py's
`--strict-verify`).

Full site_a parity: **31 users / 11 teams / 18 collections / 970 assets
/ 633 posts / 143 comments.**

## Brand workspaces — deferred per ADR 0025

The dataset includes `dataset.brand_workspaces.json` (Echo + Mirror),
but the brand-workspace API surface hasn't shipped (ADR 0025, deferred
until post-1.22 federation work stabilises). `aa seed` doesn't create
workspaces; posts that reference `brand_workspace` keep it in metadata,
and the catalogue file is ready for when the feature lands.

## What's where

| File | Purpose |
|---|---|
| `app/internal/seed/` | The `aa seed` loader — Runner + phases + sqlc queries, exercised by `handler_test.go` / the Go suite (`./scripts/test.sh --go`). |
| `app/cmd/aa/main.go` | The `seed` subcommand dispatch + flag parsing + `--reset`. |
| `seed/profiles/dataset.users.json` | 31 fictional artists + reviewers — usernames, full names, primary team. |
| `seed/profiles/dataset.teams.json` | 11 teams. |
| `seed/profiles/dataset.collections.json` | 18 projects. |
| `seed/profiles/dataset.field_definitions.json` | 12 custom field defs. |
| `seed/profiles/dataset.workflow.json` | Documents the seed's expected workflow states (mapped onto AA's actual states at runtime). |
| `seed/profiles/dataset.brand_workspaces.json` | Echo + Mirror — **not applied yet** (deferred). |
| `seed/profiles/dataset.MANIFEST.json` | Dataset inventory summary (per-site expected asset counts). |
| `<site>/MANIFEST.json` | Site-specific asset records (primary source). |
| `<site>/posts.json` | Site-specific post records (primary source). |
| `<site>/ATTRIBUTIONS.md` | Per-source licensing for the public dataset distribution. |

## Operational gotchas

### 1. Federation tests vs running app — mutually exclusive

`scripts/test.sh` stops the dev app container before running federation
tests (the LISTEN/NOTIFY dispatcher can't share a database with a
federation test manipulating it). Don't run `aa seed` concurrently with
`scripts/test.sh` against the same target.

### 2. AA_MASTER_KEY must be identical across paired instances

For cross-instance encrypted federation (1.22.I), the at-rest key wraps
the per-user private keys — so site_a and site_b need the **same**
`AA_MASTER_KEY` to decrypt each other's wrapped peer keys. Set the same
value on both instances (an operator concern, not the seeder's).

### 3. Seed-before-boot vs seed-after-boot

Run **after** the app boots and the dispatcher's LISTEN/NOTIFY catches
each insert naturally. Run **before** boot (fresh DB, then start the
app) and the dispatcher's startup probe processes the whole activities
table in one sweep — a single large "dispatch backlog" burst on first
boot. Neither is wrong; the after-boot path matches production shape,
the before-boot path is faster end-to-end.

## Things to NOT do

- **Don't drop Layer B from site_b.** That's the local dev set; it
  keeps the IP/personal content. Only site_a is Layer A only.
- **Don't hand-dedupe CROSS-OWNER identical bytes.** site_a + site_b
  deliberately carry byte-identical files owned by DIFFERENT users for
  CAS dedup testing: two asset rows over one storage object, which is
  the storage layer doing its job. That is the behaviour under test and
  it must not be pre-empted.
- **A SAME-OWNER produced-byte duplicate is a catalogue defect, and it
  is retired by document.** It is the opposite case, and #1319 is what
  separated them. Identity is `(owner_user_ref, file_hash)` and no
  `DedupBehavior` value relaxes it (ADR 0011), so the second record can
  never exist: no id, no declaration, no size, no field values, and the
  post naming it silently ships with one member fewer. `aa seed` counts
  it `deduped` and carries on; the verifier fails it and names the
  survivor. The corpus retires the loser onto that survivor with
  `seed/upgrades/asset-collapse.<stem>.json`, which enumerates every
  value the retirement costs and whose produced bytes are re-derived
  from the source roots at publish (`seed/scripts/asset_collapse.py`,
  ADR 0097). Do not fix one by editing a profile or a historical
  upgrade document by hand: the next assembly undoes it.
- **Don't follow the `external_id` field.** It's the original CSV-row
  ID (`AA-XXXX`) — preserved as metadata; the primary key is the `id`
  UUID.
