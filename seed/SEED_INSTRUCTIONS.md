# Seed instructions — how to populate an Artist Alley instance

This document is for **operators** running the seed loader against a
running Artist Alley instance. It assumes the seed data has already
been assembled (see `seed/scripts/sanitize_and_assemble.py` for that
process) and is sitting on the Synology archive at one of
`/mnt/blackbox_archives/datasets/artist_alley/{site_a,site_b}`.

The loader is **`seed/scripts/apply.py`** — a gold-standard Python
script (stdlib + `requests`) that walks the dependency graph and
populates the target instance via its HTTP API.

## Quick start

For a freshly-booted AA dev instance (uses `AA_BOOTSTRAP_DEFAULT_ADMIN=1`
from `docker-compose.yml` so the bootstrap admin is `admin /
ArtistAlleyMogul`):

```bash
python3 seed/scripts/apply.py \
    --site /mnt/blackbox_archives/datasets/artist_alley/site_a \
    --api http://localhost:8080
```

For a production-shape instance (random password lands in the boot
log + `/var/lib/artist-alley/bootstrap-admin.txt`):

```bash
# Recover the bootstrap admin password
pw=$(python3 seed/scripts/recover_admin.py \
        --admin-file /var/lib/artist-alley/bootstrap-admin.txt)

# Drop into env + run apply
AA_ADMIN_PASSWORD=$pw python3 seed/scripts/apply.py \
    --site /mnt/blackbox_archives/datasets/artist_alley/site_a \
    --api https://demo.artist-alley.org
```

For paired-instance dogfood (two AA instances; same seed catalogue,
different site profiles):

```bash
python3 seed/scripts/apply.py \
    --site /mnt/blackbox_archives/datasets/artist_alley/site_a \
    --api http://studio-a.local:8080

python3 seed/scripts/apply.py \
    --site /mnt/blackbox_archives/datasets/artist_alley/site_b \
    --api http://studio-b.local:8080
```

## What apply.py does

12 dependency-ordered phases (see `apply.py --help` for the full list):

1. **resolve_workflow_states** — `GET /workflow/states`; build the
   (domain, name) → UUID map. The seed data uses richer state names
   than AA's pre-seeded states (draft / in_review / approved / final /
   archived vs draft / pending_review / published / archived); apply
   collapses them via `WORKFLOW_STATE_ALIASES`. **Not stored
   server-side — derived at every run from what AA currently has.**
2. **resolve_asset_types** — `GET /asset_types`; map the seed data's
   typed-folder names (image / audio / 3d / video / document / font /
   comic) onto AA's int64 asset_type refs.
3. **users** — `POST /admin/seed/users` for each fictional user.
   Idempotent on `username`.
4. **teams** — `POST /teams` for each team. Slug derived from name.
5. **team_memberships** — `POST /teams/{id}/members` linking each
   user to their `primary_team`.
6. **fields** — `POST /fields` for each custom field definition.
   Idempotent on `code`.
7. **collections** — `POST /collections` for each project.
   Visibility=org-only by default.
8. **assets** — for each asset (parallel byte uploads, sequential
   entity creates):
   - `POST /storage/objects` (raw bytes; AA returns content hash +
     deduped flag)
   - `POST /assets` with `file_hash` + `state_id` + tags + metadata
   - `POST /collections/{id}/resources` to add to collection
9. **posts** — `POST /posts` with `members` (asset_ids), `tags`,
   `collection_id`, `state_id`. Posts whose members aren't all
   already created get skipped (caller logs the count).
10. **comments** — `POST /admin/seed/comments` for each asset whose
    `review_notes` is non-empty. Author = `reviewer_username`'s
    user ref, target = the first post containing that asset.
    Idempotent via deterministic comment UUID (sha256 of
    `comment|asset_id|post_id`).
11. **timestamps** — `POST /admin/seed/timestamps` in batches of
    1000 to backfill the 14-month interlaced timeline across all
    seeded assets + posts.
12. **verify** — re-reads counts from the state tracker + the
    catalogue + asserts they match within tolerance (default 10%,
    soft fail; pass `--strict-verify` for hard fail).

## Resume + idempotency

Apply writes per-target state to `./apply-state-<host>-<site>.json` in
the working directory. Phase completions + per-entity ID maps are
persisted incrementally.

- **Crashed mid-run:** re-run with the same args. Completed phases are
  skipped; in-progress phases pick up where they left off (each
  entity-create checks the state-tracker first).
- **Want to redo a phase:** `--phases <comma-separated-list>` to limit
  which phases run. Combine with `--reset-state` to start fresh.
- **Different target instance:** the state file is keyed by host+site;
  apply against a different `--api` writes a separate state file
  automatically.

## Brand workspaces — deferred per ADR 0025

The seed dataset includes `dataset.brand_workspaces.json` (Echo +
Mirror), but AA's brand-workspace API surface hasn't shipped yet
(ADR 0025 is its own phase, deferred until post-1.22 federation work
stabilises). **apply.py currently doesn't try to seed brand
workspaces**; when the feature ships, the existing catalogue file
will be ready to use.

Posts that reference `brand_workspace` in the seed data have that
field preserved in their metadata but no server-side workspace linkage
is created. Federation Share scenarios involving brand kits (per
`federation_shares.object_kind` enum value `brand_kit`) will be
exercised once the feature lands.

## What's where

| File | Purpose |
|---|---|
| `seed/scripts/apply.py` | The loader. Single Python file, ~1200 LOC, stdlib + `requests`. |
| `seed/scripts/verify.py` | Standalone post-seed validation (re-fetches counts via API, compares to catalogue). |
| `seed/scripts/recover_admin.py` | Extracts the bootstrap admin password from boot logs or `bootstrap-admin.txt`. |
| `seed/scripts/test_apply.py` | 36 tests covering state tracker, retry/backoff, content-type mapping, stable UUID, password recovery. Runs in 15ms (no live AA needed). |
| `seed/profiles/dataset.users.json` | 30 fictional artists + reviewers — usernames, full names, primary team |
| `seed/profiles/dataset.teams.json` | 9 teams (Characters / Environment / Props / Audio / VFX / UI / Animation / Marketing Art / Reference) |
| `seed/profiles/dataset.collections.json` | 16 projects mapped to collections with `studio_membership` |
| `seed/profiles/dataset.brand_workspaces.json` | Echo + Mirror — **not applied yet** (deferred) |
| `seed/profiles/dataset.field_definitions.json` | 12 custom field defs (pipeline_stage / version / texture_resolution / engine / etc.) |
| `seed/profiles/dataset.workflow.json` | Documentation of the seed's expected workflow states (apply maps onto AA's actual states at runtime) |
| `seed/profiles/studio-a.assets.json` / `studio-b.assets.json` | Per-site asset records (used as fallback; `<site>/MANIFEST.json` wins) |
| `seed/profiles/studio-a.posts.json` / `studio-b.posts.json` | Per-site post records (used as fallback; `<site>/posts.json` wins) |
| `<site>/MANIFEST.json` | Site-specific asset records (primary source) |
| `<site>/posts.json` | Site-specific post records (primary source) |
| `<site>/ATTRIBUTIONS.md` | Per-source licensing for the public Kaggle distribution |

## Operational gotchas

These came back from the 1.22.D federation work. Flag them in your
runbook or you'll hit them once paired-instance dogfood starts.

### 1. Federation tests vs running app — mutually exclusive

`scripts/test.sh` stops the dev app container before running federation
tests (1.22.D-b-6 requirement — the listen/notify dispatcher can't
share a database with a federation test that's manipulating it). Don't
run apply.py concurrently with `scripts/test.sh` against the same
target.

### 2. AA_MASTER_KEY must be identical across paired instances (for 1.22.I)

Today (pre-1.22.I) the master key is per-instance and only used for
at-rest encryption of secrets. It can differ between site_a and site_b.

When 1.22.I lands (X25519 keypair-per-user + cross-instance encrypted
federation), the at-rest key wraps the private keys — so site_a and
site_b need the SAME `AA_MASTER_KEY` to decrypt each other's wrapped
peer keys, or operators need a documented rotation flow.

This isn't apply.py's responsibility — it's the operator's. Document
the values you set on both instances.

### 3. Seed-before-boot vs seed-after-boot

If apply.py runs **after** the app boots, the dispatcher's
LISTEN/NOTIFY catches each insert naturally (one notify per row).

If apply.py runs **before** the app boots (e.g. seed into a fresh DB
via `seed.sh`, then start the app), the dispatcher's initial
`RunOnce` startup probe will see a populated activities table and
process hundreds of rows in one sweep before catching up. The
operator log will show a single large "dispatch backlog" burst on
first boot.

Neither is wrong. The "seed-after-boot" path is incremental + matches
production-shape behaviour; the "seed-before-boot" path is faster
end-to-end (no per-row notify overhead) but the log burst can look
alarming. apply.py supports both.

## Verification

After apply.py finishes, run `verify.py` to confirm:

```bash
python3 seed/scripts/verify.py \
    --site /mnt/blackbox_archives/datasets/artist_alley/site_a \
    --api http://localhost:8080
```

verify.py fetches counts via the API + compares to the catalogue with
a 10% tolerance. Exit code 0 = pass, 1 = divergence.

For a deeper check, also run apply.py's own verify phase only:

```bash
python3 seed/scripts/apply.py \
    --site /mnt/blackbox_archives/datasets/artist_alley/site_a \
    --api http://localhost:8080 \
    --phases verify
```

That re-reads the state tracker (so it knows what apply attempted to
create) + compares against the catalogue. Useful when you want to know
how many `apply` itself claims to have created, separate from what
the API listing endpoints report.

## Tests

apply.py + recover_admin.py have 36 unit tests in
`seed/scripts/test_apply.py`. Run before any change:

```bash
cd seed/scripts
python3 -m unittest test_apply -v
```

The tests cover:
- State tracker persistence + resume + reset semantics
- HTTP client retry on 5xx + 429 with Retry-After respect
- Terminal 4xx errors don't retry
- Stable UUID matches sanitize_and_assemble.py's algorithm
- Content-type mapping across image / video / audio / 3D / document
- Catalogue loader (site MANIFEST.json wins over studio-*.assets.json)
- Workflow state alias mapping covers all 5 seed states
- Bootstrap admin password recovery from dev banner + prod log patterns

There's no integration test against a live AA yet — that needs CI
infrastructure to bring up an AA container per test run, which is
slow + Phase 1.18-level work. For now: dry-run against the real
catalogue (`apply.py --dry-run`) is the smoke test.

## Things to NOT do

- **Don't drop Layer B from site_b.** That's the local dev set; it
  has to keep the IP/personal content. Only site_a is Layer A only.
- **Don't run apply.py from a directory you can't write to** — it
  needs to write the state file. Use `--state-file` to override.
- **Don't dedupe assets by content hash at seed time.** site_a +
  site_b deliberately have ~110 byte-identical files for CAS dedup
  testing across federation. Hash dedup is what AA's storage layer
  does at upload; apply just trusts the manifest.
- **Don't follow the `external_id` field.** It's the original CSV-row
  ID (`AA-XXXX` format) — apply preserves it as metadata but the
  primary key is the `id` UUID.
