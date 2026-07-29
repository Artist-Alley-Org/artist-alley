# Seed instructions — running `aa seed`

Operational reference for the loader: flags, phase order, and the things
that have bitten people. For *what to put in the files*, see
[SCHEMA.md](SCHEMA.md); for *where to get content*, see
[README.md](README.md).

`aa seed` is a subcommand of the app binary (#321). It writes **straight
to Postgres and the storage backend** through the app's own service
layer: no running server, no admin login, no HTTP. It reads its
`config.Load()` environment — DB credentials, `AA_STORAGE_*`, keys —
exactly like the server, so the simplest way to run it is a one-off
`docker compose run` off the app image: same database, same storage
volume, same network as the instance you are seeding.

## Quick start

Against a running dev stack. `AA_BOOTSTRAP_DEFAULT_ADMIN=1` in
`docker-compose.yml` creates the bootstrap admin that owns seeded
collections.

```bash
docker compose run --rm --no-deps \
    -v "$PWD/seed/example:/seed/example:ro" \
    app seed --site /seed/example --catalogue /seed/example/profiles
```

Swap the two mounts for your own dataset and catalogue. Both are bind
mounts of *host* paths — `docker compose run -v` resolves the source on
the host daemon, which is why they are arguments and not constants.

## Flags

| Flag | Default | What it does |
|---|---|---|
| `--site` | — | **Required.** The site directory: `MANIFEST.json`, `posts.json`, and the asset bytes. |
| `--catalogue` | `seed/profiles` | The catalogue directory: the four `dataset.*.json` files. |
| `--reset` | off | TRUNCATE the content tables and drop the fictional (non-admin) users before loading, so a re-run starts clean. The bootstrap admin and the baseline lookups survive. Omit on a fresh database. |
| `--limit-per-extension N` | `0` (no limit) | Keep at most N assets per file extension, cascade-dropping posts that referenced a cut asset. CI uses `3` to shrink a ~970-asset dataset to ~45 while keeping every extension and enough rows to exercise pagination. |
| `--previews` | `true` | Enqueue a preview job per asset so the seed produces derivatives — card thumbnails, video hover sprites. `false` gives a fast metadata-only seed with no thumbnails. |

Jobs are enqueued at backfill priority, so a bulk seed can never preempt
interactive work. The running app's worker pool drains them; a seed run
against a stopped app leaves the queue full until it starts.

`aa seed` owns its own prerequisites — it migrates the schema and
bootstraps the admin before loading, both idempotent and both race-safe,
so it works against a database nothing has ever touched (#574).

## Phases

Thirteen, dependency-ordered. It exits non-zero on any phase failure,
and that exit code is the verification gate.

1. **resolveLookups** — read the baseline `workflow_states` and
   `asset_types`, resolve the bootstrap admin's ref.
2. **applyUsers** — insert each catalogue user plus a federation
   keypair. Idempotent on username.
3. **applyTeams** — teams and their self-closure rows; slug from name.
4. **applyMemberships** — link each user to their `primary_team`.
5. **applyFollows** — derived follow graph.
6. **applyFields** — custom field definitions, idempotent on code.
7. **applyCollections** — one per catalogue entry *that has content*,
   owned by the bootstrap admin.
8. **applyFeatured** — one public placement per created collection
   flagged `featured`.
9. **applyAssets** — bytes into the content-addressed store, then the
   asset row, tags, collection membership, companions, typed field
   values, preview job.
10. **applyPosts** — post row, members, tags, collection linkage.
11. **applyLikes** — derived.
12. **applyComments** — a reviewer comment per asset with a
    `review_notes` + `reviewer_username` pair, threaded onto the first
    post containing that asset.
13. **applyPostComments** — derived.

Timestamps are written inline at insert time, so there is no separate
backfill pass. Each phase logs its counts; the run ends with a
`seed.complete` tally.

## Wrappers

`scripts/dogfood/seed.sh --site <dir>` runs the same call against the
`app-b` service for the dogfood second stack.

## Operational gotchas

### Do not run a seed and the test suite at once

`./scripts/test.sh` runs against a disposable `<dev>_test` database, but
the federation LISTEN/NOTIFY dispatcher cannot share a database with a
federation test manipulating it. Do not point `aa seed` at the same
target while the suite is running.

### Paired instances need the same `AA_MASTER_KEY`

For cross-instance encrypted federation the at-rest key wraps each
user's private key, so two instances that federate must share the value
to decrypt each other's wrapped peer keys. An operator concern, not the
seeder's.

### Seed before boot vs after boot

Run **after** the app boots and the dispatcher's LISTEN/NOTIFY picks up
each insert naturally. Run **before** boot — fresh database, then start
the app — and the dispatcher's startup probe sweeps the whole activities
table in one burst. Neither is wrong: after-boot matches production
shape, before-boot is faster end to end.

### `--reset` is destructive and means it

It truncates the content tables and deletes non-admin users. There is no
confirmation prompt. It is for re-seeding a dev or demo instance, not
for anything you would miss.

## Where the loader lives

| Path | What |
|---|---|
| `app/cmd/aa/main.go` | The `seed` subcommand: flag parsing, migrate, bootstrap, `--reset`. |
| `app/internal/seed/catalogues.go` | Catalogue + manifest structs and loading. |
| `app/internal/seed/runner.go` | Every phase. |
| `app/internal/seed/reset.go` | What `--reset` truncates. |
| `seed/profiles/` | The catalogue the public dataset expects. |
| `seed/example/` | A complete, runnable worked example. |
