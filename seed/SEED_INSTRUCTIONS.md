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
   app gives a duplicate re-upload — so site_a's 1,947 manifest rows
   yield **1,946** assets (see #339).
8. **applyPosts** — post row + members (asset_ids) + tags + collection
   linkage. A post whose referenced assets were all dropped (dedup or
   `--limit-per-extension`) is skipped; site_a lands **847** posts from
   859 `posts.json` entries — 12 of those entries repeat an id already
   seen (8 distinct ids, one of them five times), and a repeat inserts
   once and is counted once.
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
- **Don't hand-dedupe the catalogue by content hash.** site_a + site_b
  deliberately carry byte-identical files for CAS dedup testing.
  `aa seed` inserts every catalogue asset and lets the storage layer +
  the `(owner_user_ref, file_hash)` unique index collapse same-owner
  duplicates exactly as a real re-upload would — that's the behaviour
  under test, not something to pre-empt.
- **Don't follow the `external_id` field.** It's the original CSV-row
  ID (`AA-XXXX`) — preserved as metadata; the primary key is the `id`
  UUID.
