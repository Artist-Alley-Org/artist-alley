# Seed manifest schema

The complete contract `aa seed` consumes. Six files across two
directories; a runnable instance of every one of them is in
[`example/`](example/).

Parsing is Go `encoding/json` with no strict mode, so **an unrecognised
key is silently ignored** — a typo'd field name is not an error, it is a
value that never arrives. Check spelling against the tables below when
something you authored does not show up.

Everything is UTF-8 JSON. Timestamps are RFC 3339 (`2026-03-02T09:14:00Z`);
an unparseable timestamp becomes NULL rather than failing the run.
Every `id` must be a valid UUID — any UUID, but the same one on a re-run,
because ids are what make a re-seed idempotent.

---

## Catalogue directory (`--catalogue`)

Four files. All four must exist; each holds a JSON array.

### `dataset.users.json`

The fictional (or real) people who own content. Each gets an approved
account and a federation keypair. Passwords are not set — these are
content owners, not logins.

| Key | Type | Required | Notes |
|---|---|---|---|
| `username` | string | **yes** | The join key. `owner_username`, `author_username` and `reviewer_username` all resolve against it. |
| `full_name` | string | no | Display name. |
| `email` | string | no | Use a domain you control or a reserved one (`.invalid`, `.local`). |
| `primary_team` | string | no | Must match a `name` in `dataset.teams.json`, or no membership is created. |

The bootstrap admin is not listed here. It already exists, and it owns
every seeded collection.

### `dataset.teams.json`

| Key | Type | Required | Notes |
|---|---|---|---|
| `id` | UUID | **yes** | Stable id. |
| `name` | string | **yes** | The join key for `primary_team` and `team_name`. The slug is derived from it. |

### `dataset.collections.json`

| Key | Type | Required | Notes |
|---|---|---|---|
| `id` | UUID | **yes** | Stable id. |
| `name` | string | **yes** | The join key for `collection_name`. |
| `featured` | bool | no | Places the collection on the public rail and in `/admin/featured`. Default `false`. |
| `visibility` | enum | no | `public`, `org-only`, `private`, `followers`, `explicit-share`. Default `org-only`. A value outside this set violates a database CHECK and fails the run — this is the one enum the seeder does not quietly coerce. |

Two behaviours worth knowing:

- **A collection with no assets is not created at all.** The catalogue
  usually lists collections across several sites; creating the empty
  ones would put hollow shells on the front rail.
- **`featured` does not widen access** (ADR 0065). Featuring an
  `org-only` collection puts it on the public rail where an anonymous
  visitor sees an empty tile. If you feature it, publish it.

### `dataset.field_definitions.json`

Custom metadata fields (ADR 0012). Values land in `asset_field_value`.

| Key | Type | Required | Notes |
|---|---|---|---|
| `name` | string | **yes** | The stable code. `field_values` keys must match it exactly. |
| `label` | string | no | What the UI shows. |
| `type` | enum | **yes** | See the table below. |
| `options` | string[] | for `select` / `multi_select` | Allowed values. |
| `extraction_source` | string | no | Wire the field to an extractor's canonical field name. Empty means operator-managed, which is what a hand-authored dataset wants. |
| `extraction_mode` | string | no | Only meaningful alongside `extraction_source`. |

| `type` | What `field_values` must supply |
|---|---|
| `text`, `longtext`, `rich_text`, `tree` | string |
| `select` | string, one of `options` |
| `multi_select` | array of strings from `options` (a bare string is accepted as a one-element list) |
| `number` | JSON number — **not** a quoted string; a string is dropped |
| `boolean` | `true` / `false` |
| `date`, `datetime` | RFC 3339 string; unparseable is dropped |
| `reference` | UUID string |

---

## Site directory (`--site`)

### `MANIFEST.json` — one entry per file

| Key | Type | Required | Notes |
|---|---|---|---|
| `id` | UUID | **yes** | Stable id. `posts.json` references it. |
| `file_path` | string | **yes** | Relative to the `--site` root. A file that cannot be opened is logged and skipped — the run does not fail. |
| `file_extension` | string | **yes** | No leading dot. Drives the MIME type and which preview job is enqueued. |
| `asset_type` | enum | **yes** | `image`, `audio`, `video`, `3d`, `document`, `font`, `comic`. Anything else falls back to image. |
| `title` | string | no | Defaults to `Untitled`. |
| `description` | string | no | |
| `sensitivity_tier` | enum | no | `public`, `team`, `restricted`, `embargo`. Anything else, including absent, becomes `public`. |
| `archive_state` | enum | no | `active`, `archived`; anything else, including absent, becomes `draft`. |
| `workflow_state` | enum | no | `draft`, `in_review`, `approved`, `final`, `archived`. Absent or unknown lands on published. |
| `owner_username` | string | no | Must match a catalogue user or the asset is left unowned. |
| `team_name` | string | no | Must match a catalogue team or no team is set. |
| `collection_name` | string | no | Must match a catalogue collection or **the asset is silently uncollected**. |
| `tags` | string[] | no | Deduplicated. |
| `metadata` | object | no | Free-form JSON, stored verbatim on the asset. Absent becomes `{}`. |
| `field_values` | object | no | `{ "<field name>": value }`. A key with no matching field definition, or a value of the wrong shape, is dropped silently. |
| `review_notes` | string | no | Non-empty **and** a resolvable `reviewer_username` **and** membership of some post ⇒ a reviewer comment is forged on that post. All three, or nothing happens. |
| `reviewer_username` | string | no | See above. |
| `created_at` | RFC 3339 | no | Written through to the row — this is what makes a seeded instance look lived-in rather than born today. |
| `updated_at` | RFC 3339 | no | Defaults to `created_at`. |

`workflow_state` maps onto the instance's actual states rather than
creating new ones:

| Manifest value | Asset state |
|---|---|
| `draft` | draft |
| `in_review` | pending_review |
| `approved`, `final` | published |
| `archived` | archived |

**Duplicate bytes collapse.** A byte-identical file already owned by the
same user is refused by the `(owner_user_ref, file_hash)` unique index —
the same refusal the app gives a duplicate re-upload. The seeder counts
it as deduped and moves on, so a manifest of N entries can legitimately
produce fewer than N assets.

**Multi-file 3D models self-wire.** For `.gltf` and `.obj`, the seeder
parses declared external resources (glTF `buffers[].uri` /
`images[].uri`, OBJ `mtllib` → MTL `map_*`) and registers each sibling
that exists next to the main file as a companion. Ship the siblings and
the viewer resolves them; omit them and the model renders untextured
rather than failing.

### `posts.json` — one entry per post

| Key | Type | Required | Notes |
|---|---|---|---|
| `id` | UUID | **yes** | Stable id. |
| `asset_ids` | UUID[] | **yes** | Manifest ids. Unknown ids are dropped; a post left with **no** members is skipped entirely. The first surviving member becomes the cover. |
| `title` | string | no | Defaults to `Untitled`. |
| `description` | string | no | |
| `author_username` | string | no | Falls back to the bootstrap admin. |
| `collection_name` | string | no | Must match a catalogue collection or the link is skipped. |
| `team_name` | string | no | Must match a catalogue team. |
| `tags` | string[] | no | Deduplicated. |
| `created_at` | RFC 3339 | no | Also the floor for derived like and comment timestamps. |
| `updated_at` | RFC 3339 | no | Defaults to `created_at`. |

Seeded posts are created `org-only`. `workflow_state` is accepted on a
post entry but every seeded post is published — the field is there for
symmetry with the manifest, not because it selects a state.

---

## What you do not author

Four phases synthesise their own data from what is already loaded, so
there is no file to write and no way to pin the values:

| Derived | How |
|---|---|
| **Follows** | A deterministic edge set over the catalogue's usernames, timestamped inside the content's date span. |
| **Likes** | Per post: a small baseline count, plus a large bump for roughly one post in five, drawn from users other than the author and timestamped after the post. |
| **Post comments** | Zero to three top-level comments per post from non-authors, with deterministic ids so a re-run does not duplicate them. |
| **Featured placements** | One public placement per collection flagged `featured` that actually got created. |

All four are seeded from stable hashes, so the same input gives the same
output on every machine and every re-run.

---

## Reading the source

If this document and the code disagree, the code wins:

| What | Where |
|---|---|
| The structs these files unmarshal into | `app/internal/seed/catalogues.go` |
| Every phase, in order, and the enum mappings | `app/internal/seed/runner.go` |
| Flags and the reset path | `app/cmd/aa/main.go` |
