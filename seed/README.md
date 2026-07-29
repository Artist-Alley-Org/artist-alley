# seed/ — populate an instance with content

`aa seed` fills a fresh Artist Alley instance with users, teams,
collections, custom fields, assets and posts. It writes **straight to
Postgres and the storage backend** through the app's own service layer:
no running server, no admin login, no HTTP.

This directory holds what the seeder consumes and how to produce it.
There are two ways to get content in:

1. **[Bring your own assets](#1-bring-your-own-assets)** — write a
   manifest describing files you already have. Start from
   [`example/`](example/), which is a complete, runnable dataset.
2. **[Start from the public dataset](#2-start-from-the-public-dataset)** —
   download a licensed, attributed studio-shaped dataset and point the
   seeder at it. *(Not published yet — see that section.)*

The full field-by-field contract is in **[SCHEMA.md](SCHEMA.md)**.
Operational detail — flags, phase order, gotchas — is in
**[SEED_INSTRUCTIONS.md](SEED_INSTRUCTIONS.md)**.

## What the seeder reads

Two directories, and nothing else:

```
--catalogue <dir>            --site <dir>
├── dataset.users.json       ├── MANIFEST.json     the asset records
├── dataset.teams.json       ├── posts.json        the post records
├── dataset.collections.json └── <asset bytes>     laid out however you like;
└── dataset.field_definitions.json                 file_path is relative to here
```

The **catalogue** describes the people and structure — who exists, what
teams and collections they work in, which custom fields the instance
defines. The **site** describes the content — one record per file, plus
the files themselves.

They are separate because the same catalogue usually serves several
sites. Nothing stops you putting both in one directory; the example
does, and passes the same path to both flags.

Everything else — likes, follows, comments-on-posts, featured
placements — is derived. You do not author it.

## 1. Bring your own assets

`example/` is six assets, three posts, three users, two teams, two
collections and four custom fields. It covers image / document / audio /
3D, three of the four sensitivity tiers, draft and active, four field
types (select, number, multi-select, boolean), tags, and two review
notes that become comments. It is
deliberately small enough to read in one sitting — 44 KB in total, every
byte generated or CC0.

Run it against a dev stack (the compose file sets
`AA_BOOTSTRAP_DEFAULT_ADMIN=1`, which creates the `admin` account that
owns seeded collections):

```bash
docker compose run --rm --no-deps \
    -v "$PWD/seed/example:/seed/example:ro" \
    app seed --site /seed/example --catalogue /seed/example/profiles
```

Expected output:

```
seed complete: users=4 teams=2 collections=2 assets=6 posts=3 comments=5
```

Four users, not three: the count includes the bootstrap admin the
seeder resolved. Five comments: two forged from the manifest's
`review_notes`, three derived onto posts. Six assets from six manifest
entries, five preview jobs — the Markdown document has no preview
handler and is deliberately not queued.

Then open the instance. The three posts are on the home feed with real
previews — the SVG sketch, an audio waveform, a 3D turntable — and
**Harbour District** sits on the featured rail above them. `/collections`
shows both collections with their visibility badges.

### Making it yours

1. Copy `example/` somewhere outside the repo.
2. Replace `files/` with your own files, in whatever layout you like.
3. Edit `MANIFEST.json` — one entry per file. `file_path` is relative to
   the `--site` root; `id` must be a UUID; `owner_username`,
   `team_name` and `collection_name` must match the catalogue, or that
   link is silently skipped. [SCHEMA.md](SCHEMA.md) lists every field,
   which are required, and the accepted values for each enum.
4. Edit `posts.json` — a post groups assets by id. A post whose assets
   all failed to load is dropped.
5. Edit `profiles/` — your users, teams, collections and fields.
6. Re-run the command above with your paths.

Re-run against an instance you have already seeded and add `--reset`,
which truncates the content tables and drops the non-admin users first.

For a large set, `--limit-per-extension N` keeps at most N assets per
file extension (and drops posts that referenced a cut asset). It is what
CI uses to shrink a thousand-asset dataset to something that seeds in
under a minute while still covering every format.

### Generating a manifest

There is no importer that walks a directory and writes `MANIFEST.json`
for you — the interesting fields (owner, team, collection, workflow
state, custom values) are not recoverable from a filesystem. For a
directory of loose files, a dozen lines of Python that emit one record
per file with sensible defaults will get you a browsable instance; the
example manifest is the shape to emit.

## 2. Start from the public dataset

> **Status: not published yet.**
> `kaggle.com/datasets/mscrnt/artist-alley-demo-seed` currently 404s.
> The data exists but the copy on disk still contains renders from
> before the rasteriser fix in #713, so publishing it now would hand
> third parties artwork we already know is cropped. It goes up after the
> re-render in #714 verifies clean. Until then, use option 1.

The planned dataset is a studio-shaped archive under a CC-BY-SA 4.0
aggregate licence: images, audio, 3D models, video, fonts, documents and
ebooks, in the format coverage a small studio's library actually has.
Every source is CC0, CC-BY, OFL or public domain, and every one is
credited in [ATTRIBUTIONS.md](ATTRIBUTIONS.md), which ships inside the
download.

Nothing in this repository is needed to consume it. Download it however
you like — the browser button, the `kaggle` CLI, or `kagglehub` — unpack
it, and point the seeder at the unpacked directory:

```bash
docker compose run --rm --no-deps \
    -v "/path/to/artist-alley-demo-seed:/seed/site:ro" \
    -v "$PWD/seed/profiles:/seed/profiles:ro" \
    app seed --site /seed/site --catalogue /seed/profiles
```

The download ships its own `MANIFEST.json`, `posts.json` and asset
bytes; the catalogue it expects is the one committed here in
[`profiles/`](profiles/), which is why that mount is separate.

> **Licence obligation.** If you redistribute any of it, keep
> `ATTRIBUTIONS.md` with it. Per-asset `license` and `attribution` in
> `MANIFEST.json` are authoritative where the two differ.

## Nothing here hardcodes a path

Every root is a flag. The dataset root differs between machines, and a
value baked into a script is wrong everywhere except the box it was
written on. A CI gate (`scripts/check-seed-paths.sh`) fails the build if
an absolute host path appears under `seed/`.

## What is not here

The tooling that *builds* the public dataset — acquisition, sampling,
rasterising, provenance resolution, publishing — lives in the private
`artist-alley-demo` repository under `pipeline/`. It describes one
specific machine's dataset and helps nobody seed their own instance.
Consuming the published result, as above, needs none of it.
