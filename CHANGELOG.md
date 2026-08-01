# Changelog

All notable user-facing + wire-format changes to artist-alley.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions track the ArchivePub federation spec ([docs/protocol/archivepub.md](docs/protocol/archivepub.md))
where applicable, otherwise note "no-spec-impact."

## [Unreleased]

### Security

- **SSO provider credentials are no longer readable.** `GET /admin/system/auth`
  returned each provider's whole configuration blob verbatim — which is where the
  OAuth client secret, the LDAP bind password and the SAML service-provider private
  key live. Setting one of those needs `system.auth.write`; reading the endpoint only
  needs `system.config.read`, so the narrower write capability was protecting nothing.
  The three secrets are now write-only: the response carries `client_secret_set`,
  `bind_password_set` and `sp_private_key_set` booleans and never the values, to any
  capability, `system.admin` included. Everything that is not a credential still comes
  back in full — including the OAuth client ID, the LDAP bind DN and the IdP
  certificate, which look like secrets and are not (#718).

  The blob was free-form, and that is the part that got fixed rather than patched.
  Denylisting the field names we know today fails open on the first one somebody adds,
  so `config` is now a closed, typed, per-kind schema — OAuth, LDAP and SAML fields
  named after their own protocol vocabulary, with no free-form remainder in which a
  credential could hide. The AI provider record's `config` had the identical shape and
  is closed the same way; #711 had made its `api_key` write-only but left the map
  beside it returned in full.

  Two knock-on changes come with it. Saving the auth settings now merges each
  provider's stored secrets in from the database when the request omits them (or sends
  them empty), because the admin page can no longer echo back what it never received —
  without the merge, the first display-name edit would have wiped every stored
  credential. And a failure to read the current settings during that save now aborts
  the write instead of being tolerated, since it is the merge input rather than only an
  audit record. The auth page grew the per-provider fields it never had; a configured
  secret shows as "configured" and stays untouched unless you type a new one.
  No-spec-impact.

### Added

- **A field can now be a hierarchy, and the sample library exercises every kind of field
  there is.** Fields come in eleven kinds — plain text, long text, formatted text, numbers,
  yes/no, dates, timestamps, pick-one and pick-many lists, a hierarchical category, and a
  pointer to another asset. Six of those had never carried a value in any sample library, and
  the hierarchical kind could not even be *described* in one: the file that defines the sample
  fields could only express a flat list of choices, so a category with sub-categories was
  unwritable. That is fixed, and the sample library now defines and fills all eleven (#808).

  The sample media was regenerated to match: every video trimmed to two minutes, keeping its
  format and audio and subtitle tracks intact, and a deliberately rotated photograph added so
  that orientation handling is exercised by something real rather than by a square test image
  no camera would produce (#805).

  **This immediately paid for itself.** Filling in kinds of field that had never held data
  exposed four faults that no test could have found, because the situations they occur in did
  not exist: a malformed pointer was being stored as an empty value rather than rejected;
  formatted text is displayed as raw markup; a date is shown a day early for anyone west of
  UTC; and a pointer shows an internal identifier instead of the thing it points at. The first
  is fixed, and the other three are now tracked.

- **Fields can fill themselves in.** A field can carry a default that is applied when an
  asset is uploaded, and a team can override that default for its own uploads — so a studio's
  work lands tagged as that studio's without anyone typing it. A default is either a fixed value
  or one of a short list the server works out for itself, like whoever is uploading or today's
  date. There is no scripting: a default is a value, not a formula (#793).

  It never overwrites anything. A value you typed stays, and so does one read out of the file
  itself — a default only fills a blank. A field offering a retired option cannot be given that
  option as its default.

- **Profiling can be turned on when something needs investigating.** Set `AA_PPROF_ADDR` and
  the server exposes Go's standard profiling endpoints on that address — off by default, on its
  own listener, published by no deployment file, and it warns loudly if pointed anywhere other
  than loopback. Profile data can contain anything the process is holding, so it is deliberately
  not something that is simply on (#781).

- **Controlled vocabularies are editable.** The options behind a `select` or
  `multi_select` field could only be set when the field was created — after that they were
  frozen, and `/admin/fields` had no edit surface at all. There is one now: add a term,
  relabel one, reorder them.

  **Terms are retired, not deleted.** Deleting an option that assets already reference would
  orphan those values, and the orphan shows up as a blank on an asset nobody touched — so the
  editor does not offer deletion. Instead an option can be marked *deprecated*, which stops it
  being offered for new values while everything already carrying it keeps resolving and
  displaying normally, and it can name a successor that the editor then suggests in its place.
  *Archived* is the harder retire, for terms that were mistakes rather than terms that were
  superseded (#737).

  Saving is now conflict-checked. Changing one term rewrites the whole vocabulary, so two
  admins editing different terms used to silently overwrite each other — the loser never found
  out. The save now carries the version it was based on and is rejected if the field moved
  underneath it, offering to reload or to overwrite deliberately. Fields whose options nobody
  has edited are left byte-for-byte as they were.

- **Operators can set their own instance logo.** The mark in the navbar and on the
  sign-in page is no longer fixed to the icon that ships. Upload a PNG, JPEG, GIF or
  WebP under `Admin → Themes & branding` and it applies everywhere immediately; the
  shipped default returns the moment you clear it, and is always one click away
  (#517).

  The last five logos are kept so a previous mark can be picked back up without
  re-uploading it — including when the original file is long gone, which is the case
  the list exists for. Re-selecting one moves it to the front rather than adding a
  duplicate, and a sixth upload drops the oldest. Every listed logo is retained in
  storage for exactly as long as it is listed, so an entry the picker offers is an
  entry that actually still works. If a logo's image data does go missing anyway —
  a database restored against a fresh bucket, say — the picker says so in words and
  refuses to apply it, rather than showing a broken thumbnail or swapping your
  working logo for one.

  Uploads are treated as hostile input, because a logo is an operator-supplied file
  rendered on every page: the file must decode as a real raster image, its type is
  taken from decoding it rather than from anything the browser claimed, and it is
  capped at 2 MB and 1024px per edge. **SVG is not accepted** — it is an executable
  document format, and accepting one safely needs a sanitiser that is its own piece
  of work rather than a detail of this change. Rasterize vector art before uploading
  it. No-spec-impact.

### Changed

- **The browse feed's "Team" and "Trending" buttons are gone; the filter is now
  Latest / Following.** Both removed buttons were decoration: neither value was ever
  in the server's `feed` enum, so clicking them sent a query param the API ignored and
  handed back the plain latest feed — the same posts, under a label that promised
  something else. Nothing is lost because nothing was there. Neither returns by
  re-adding a button: `trending` needs a ranking model (recency against engagement,
  and how fast it decays) decided first, and `team` returns with the teams browse
  surface (#684), where the team-scoped query gets designed once. A browser that
  remembers "Team" or "Trending" from before opens on Latest (#691).

### Fixed

- **A seed run now says when it throws a field value away.** Loading a catalogue could
  discard a value in two different situations — the file named a field that does not
  exist, or it carried something that field's type cannot hold — and both were silent.
  The run reported success, the field was simply missing afterwards, and any check that
  counted rows agreed that everything was fine. A seeded instance could therefore be
  quietly missing data that nobody had any way to notice.

  Both cases now report themselves, and they report themselves *differently*, because
  the two need different fixes: one is a mismatch between the catalogue and the
  definitions, the other is a bad value. A run ends with a plain-language note saying
  how many values were dropped and which fields they belonged to. A single misconfigured
  field no longer floods the log either — a few examples are shown, while the count
  stays exact (#807).

  A date field also accepts an ordinary calendar date now, such as `2026-03-14`.
  Previously it required a full timestamp and would discard anything else — without
  saying so, which is precisely the trap above.

  While fixing this, one worse case turned up: a reference field given a malformed
  identifier was not being discarded at all. It was accepted, and stored a value that
  was empty in every respect — a field that reads as "deliberately set to nothing"
  rather than as absent, and the one shape a row count cannot detect. It is now refused
  like any other bad value.

- **A portrait phone photo would have tiled as a landscape one.** Two different parts of
  the system were writing the tile shape a browse page reserves for an image, and they
  disagreed for exactly one kind of file: a photo taken in portrait, which cameras store
  landscape with a tag telling the viewer to turn it. The preview pipeline recorded the
  shape you actually see; the metadata reader recorded the shape on disk, and it wrote
  last, so the wrong one won. Only the preview pipeline records it now (#765).

  Nothing in the catalogue was visibly wrong yet — the six affected rows were test
  images — so this is a trap removed before the first real phone photo hit it rather
  than a repair. The shape is now measured in one place, from the same image the
  thumbnails are built from, which is also the only place that can answer the question
  for the half of the catalogue with no source pixels at all: a 3D model, a font, an
  audio file and a document each produce exactly one picture on the way through, and its
  shape is what a tile reserves. No-spec-impact.

- **Portrait video previews were squashed into landscape.** The strip of thumbnails you
  scrub through was built at a fixed widescreen shape whatever the video actually was, so
  anything shot on a phone came out stretched. Thumbnails now keep the source's proportions —
  including the case that matters most in practice, a clip whose file says it is landscape and
  which plays portrait because of how the camera recorded it (#761).

  The same squash was happening on the browse grid, where hovering a video card scrubs through
  the same strip.

  **And in the viewer, the hover preview was not appearing at all** — it was being clipped to
  the height of the scrub bar itself, twelve pixels, ever since the scrubber gained zoom. It is
  a separate fix, and it is why the problem looked like it only affected the grid.

  Existing videos keep their old thumbnails until their previews are rebuilt.

- **Yes/no fields never worked either, and one of them failed louder than blank.** A field can
  be declared as a yes/no checkbox, and the parts of the system that write one disagreed about
  how. Setting one on an asset stored a number, while the panel that displays it looked for the
  words "true" and "false" — so it showed nothing. Setting one on a collection stored the words,
  in a different place from where an asset's went. And ticking the box in the upload window sent
  the words to a destination that only ever accepted the number, so that write was **rejected
  outright** rather than merely rendering blank — the one part of this that would have shown a
  user an error rather than an empty row.

  Ten places, and the two that were right were the ones nobody looked at. It survived for the
  same reason the hierarchy bug below did: no yes/no field has ever existed on a real instance,
  so the whole path had never once been run. A number, 1 or 0, everywhere now — which is what
  the design document said before any of the disagreeing code was written. Nothing to migrate:
  there was no stored value anywhere, in either form (#791).

  "Not set" and "no" stay different: an unset field shows nothing, a field set to no shows "No".

  The test that guards this used to check only *where* a value is stored. Two writers can agree
  on that and still disagree about what they put there, which is exactly what happened, so it
  now drives the writers with the same input and compares what each one actually produces. The
  list of tolerated exceptions is gone rather than emptied — an exception list is somewhere to
  put the next one.

  With this, all eleven field types agree across every writer and every screen, and the two that
  had never been exercised end-to-end now have a test that creates one, sets it, and reads it
  back out of the database.

- **Hierarchical fields never worked, and nothing had noticed.** A field can be declared as
  a hierarchy — country / region / city — and every part of the system disagreed about where
  such a value was stored. An asset put it in one place, a collection in another, and the panel
  that displays it read a third, so the value came back blank whichever way it had been entered.
  Two more places could not resolve a nested term at all, having only ever looked at the top
  level of the list.

  It survived because no hierarchy has ever held a value — a fresh install ships one, wired to
  read the country out of a photo's embedded metadata, and it had simply never fired. Settled
  now: a value is the single term it points at, and the path above it is worked out on the way
  out. Renaming a term, or moving one to a different parent, leaves every asset untouched (#778).

  There is a test that fails if any of the eight places disagrees again.

- **Five screens showed internal key names instead of English.** The AI configuration and
  AI usage pages under Admin displayed text like `admin.ai_inference.budget_hard_label` where
  their labels and help text should have been; the similar-assets panel, the collection field
  editor and the tag-source tooltips in the viewer had the same problem. The wording had been
  written all along — the screens were simply asking for it under the wrong name. Forty-six
  strings, now resolving (#774).

  Spanish and French translations moved with them, so nothing regressed to English.

  The reason this survived: the test meant to catch it only checked that text was *marked for
  translation*, never that the translation existed — so a screen asking for a name nothing
  answered to passed cleanly. It now checks that every requested name resolves, which is what
  turned up two of the five screens nobody had reported.

- **The server could be killed by its own memory limit while building previews.** Go decides
  when to collect garbage from how much memory is already in use, and it has no idea a container
  limit exists — so on a machine with plenty of RAM it would let the heap grow past the
  container's ceiling and be killed for it. Generating previews for large images is where that
  bit: resizing one holds a scratch buffer proportional to the image's dimensions, and several
  run at once. Symptom was the whole instance going unresponsive for a minute or two mid-render,
  health checks included, then recovering.

  The runtime is now told the container's own limit at startup, read from the container rather
  than written down somewhere that could drift out of step with it. Requests that previously took
  nearly five seconds during a heavy render now take a seventh of a second (#781).

  The default deployment also gains a memory limit, which it never had — only the CI
  configuration capped anything. That meant the failure was reproducible in CI and invisible to
  operators. Override with `AA_APP_MEM_LIMIT` if your host warrants something other than the 4 GB
  default.

- **Per-job-type concurrency limits were not actually limiting anything.** An operator can
  cap how many jobs of a given kind run at once — transcription at one, video and 3D previews
  at two — precisely because those are the jobs heavy enough to hurt a machine when several run
  together. The check and the counter were separated: a worker asked "is there room?", and the
  answer was only recorded once the job had been claimed. Every worker that asked during that
  window got the same stale answer and the same yes, so the real ceiling was however many
  workers happened to be polling — not the configured number. Observed in the wild: five
  concurrent 3D renders against a limit of two. The reservation is now taken at the moment the
  check passes, so a limit of two means two (#777).

  The old comment described this as a window that "can let one extra job through." A test that
  releases sixteen workers into it against a limit of two saw **all sixteen** admitted.

- **Vocabulary values showed their internal slug instead of their label.** A term is
  stored by slug so that renaming it is free and rewrites nothing on any asset — but only the
  editing screens ever turned that slug back into the label, because they happen to load the
  field definition to build their dropdown. Everywhere else, including the post and asset
  detail panels most people actually read, the raw slug came through. Labels now resolve on
  the server, so every surface gets them and none has to know a controlled vocabulary is
  involved. A term with no label of its own still shows its slug, unchanged (#775).

  This also completes the deprecation marking added above: a retired term now reads as
  deprecated on the detail panel, not only inside the picker.

- **Long metadata values were unreadable on a phone.** The detail panel's two-column
  layout gave the value column roughly two characters at 390px, so `N/A` broke across lines.
  It stacks below the small breakpoint now (#775).

- **Every dropdown in the collection field editor was empty.** The vocabularies were
  stored correctly and the editor could not read them: it expected each option to be an object
  and they are stored as plain strings, so it rendered one blank row per term. The upload
  form, which read the same data the other way, worked — which is why this survived. Both
  now accept either form (#737).

- **The admin tables were unusable on a phone.** At 390px the fields table was wider than
  the screen with nothing to scroll it, so the overflow was not merely off-screen — it was
  unreachable. The Save button sat at a negative x-coordinate with the document reporting no
  horizontal overflow at all. The tables scroll now (#737).

- **Masonry browse laid every tile out as a square.** The layout is supposed to respect each
  asset's real proportions, and it could not: nothing in the system had ever recorded a
  source's pixel dimensions. The `pixel_width` / `pixel_height` field definitions existed and
  every value was null across the whole catalogue, so the estimator fell through to 1:1 for
  every tile and masonry rendered as a plain grid. The preview pipeline now records the source
  dimensions as it decodes — it is the only producer that runs for *every* asset type, so this
  also covers the tiles EXIF could never describe: audio waveforms, video posters, SVG and 3D
  turntables, which are precisely the ones that looked worst squared off (#757).

  The same missing data was breaking IIIF for the entire catalogue. `info.json` is built from
  the asset's dimensions, and a 0×0 asset is rejected as unsupported — so every IIIF request
  had been 404-ing. It serves now. The UI test covering that endpoint had been asserting a
  JSON content type and passing *because* of the 404, which is a test defending the bug rather
  than the behaviour; it now asserts the IIIF media type.

- **nginx kept routing to a dead container address after any recreate.** Hostnames inside an
  `upstream` block are resolved once, when the config loads, and a `resolver` directive does
  not change that — the `resolve` parameter that would is NGINX Plus only. So when the app
  container came back with a different IP, nginx went on proxying to the old one. On the
  two-instance dev stack this surfaced as the worst possible symptom: traffic for one site
  silently served from the other, which reads as data corruption rather than a routing fault.
  The `upstream` block is gone; `proxy_pass` now goes through a variable, which defers
  resolution to request time (#756).

  Removing the block also removes connection reuse to the app — `keepalive` cannot be
  configured without it. That cost is recorded in the config beside the change rather than
  left to be rediscovered.

- **3D thumbnails rendered untextured while the viewer showed the materials.** ADR 0069
  chose headless three.js precisely so the browse-grid thumbnail and the interactive
  viewer would match, and both files said they shared code. They did not: the headless
  page reimplemented the viewer's loader, and the copy had no `MTLLoader` and no
  material-upgrade pass. So every OBJ rendered as untextured white (its `.mtl` was
  never fetched — OBJ references materials by name and the loader does not follow it),
  and every `KHR_materials_unlit` glTF rendered as a flat, unshaded silhouette, because
  the `MeshBasicMaterial` that extension produces ignores the entire lighting rig. The
  thumbnail also captured its frames before the textures had finished decoding, which
  would have kept OBJ untextured even once the `.mtl` loaded.
  The load path — loader choice, `.mtl` resolution, material normalisation — is now one
  module both surfaces import, so the drift cannot recur, and the release-image smoke
  test asserts materials came out normalised and the textured fixtures came out
  textured. Previously it only asserted the poster had non-transparent pixels, which is
  why an untextured catalogue shipped green twice: flat white geometry passes that
  check. Existing thumbnails are regenerated by re-running the preview job for an asset
  (#689). No-spec-impact.

  Correction: re-running the preview job did NOT regenerate anything until #760 landed,
  so this fix — and the two companion fixes below it — were invisible on every existing
  install. See "Recreating a preview now actually re-renders it".

- **Recreating a preview now actually re-renders it.** "Recreate previews" enqueued a
  job, returned a job id, and changed nothing. Every preview worker skips outputs that
  are already in storage — which is what makes an ordinary re-queue nearly free — and
  nothing could tell them not to. So the job ran, skipped everything, and completed
  successfully, and the thumbnail stayed exactly as it was. Three shipped renderer
  fixes (#689, #750, #753) therefore reached no existing asset: the only way to see
  them was to upload the file again under a different hash. The endpoint now forces a
  re-render by default, and `POST /assets/{id}/preview?force=false` selects the old
  gap-filling behaviour deliberately (#760).

  Nothing is deleted first. Each output is overwritten in place by an atomic write, so
  an interrupted rebuild leaves the previous — stale, but present — preview serving
  rather than an asset with no thumbnail at all.

  `aa rebuild-previews --ext glb,fbx,obj` re-renders a whole set, because the situation
  that produces this is "a renderer changed and every asset it ever touched is stale",
  which is not a click-once-per-asset problem. It reports how many of the assets it
  swept already had renders, i.e. how many of those jobs are replacing bytes rather
  than filling a hole. `scripts/preview-backfill.sql`, which could only enqueue
  raster jobs and whose own comment noted that a second run did nothing, is gone.

  Two smaller honesty fixes came with it. `storage_variants` gained `updated_at`: the
  table previously recorded only when a variant was FIRST written, so a successful
  re-render was invisible in the database and "have these bytes changed?" had no
  answer. And a completed job now logs what it wrote and what it skipped — `590 jobs,
  0 failures` used to look identical whether 590 renders happened or none did.
  No-spec-impact.

- **`aa seed` says when the previews it queues will do nothing.** `--reset` truncates
  the content tables but deliberately leaves the content-addressed variant store alone
  (the blobs are on the volume; a database truncate does not erase them, and pretending
  otherwise produced assets with no thumbnails at all). Re-seeding the same dataset
  therefore enqueues a preview job per asset whose output already exists, and every one
  of them skips. That was correct and silent — the seed reported "previews queued: 590"
  either way. It now reports how many of those will skip and points at
  `--force-previews`, which re-renders them (#760). No-spec-impact.

- **363 of 374 3D models were missing their textures, because "a GLB embeds everything"
  was written down as a fact and is not one.** A `.glb` is a binary wrapper around
  ordinary glTF JSON, so it can point at `Textures/foo.png` on disk exactly as a `.gltf`
  does — and almost all of ours do. Four separate places asserted the opposite: the Go
  companion resolver, the seeder that called it, and two dataset scripts, in one of which
  the claim had grown into a justification for how the library was assembled. Because the
  copier shared the belief, the texture folders were never even placed in the dataset, so
  fixing only the code would have found nothing and looked correct. The resolver now reads
  a GLB's JSON and asks, instead of assuming; the duplicated extension list in the seeder
  is gone, so there is one answer to "does this format have companions" rather than two
  that could drift apart. The test that should have caught this was checking an
  eleven-byte text file named `model.glb` — it passed because there was nothing to parse,
  and it kept the assumption alive; those same bytes are now a case that must fail.
  Existing thumbnails still need the preview job re-run to pick the textures up (#750).
  No-spec-impact.

- **The same was true of FBX: 105 of 109 named a texture and none of them found it.** FBX
  keeps its texture filenames inside binary node records, which nothing had ever read, so
  the comment saying FBX "embeds its resources" went unchallenged for the same reason the
  GLB one did. There is now a reader for it, and a matching one in the dataset copier so
  the texture folders actually get staged. Two subtler problems came out with it. Companion
  paths were being stored with the backslashes FBX writes, because the code normalised them
  with a call that does nothing on Linux — so `Textures\barrel.png` was one filename rather
  than a folder and a file, and nothing could match it. And the thumbnail renderer asks for
  textures by bare filename regardless of the folder they live in, so correcting the stored
  path was necessary but not sufficient; the renderer now resolves them explicitly. Proven
  by running the release-image smoke test with and without that step. As with the GLB fix,
  **existing thumbnails do not change until previews are re-rendered** — they are still the
  output of the renderer that shipped before July, which is tracked separately (#753).
  No-spec-impact.

- **Six seed images were showing a fraction of their own artwork.** The Kenney pack's
  Flash-exported sprite sheets carry a stale artboard — `viewBox="0 0 550 400"` on a
  drawing that actually spans 2248x1120 units — and the rasteriser honoured it, so the
  Platformer Pack Remastered background sheet shipped holding **8.8%** of its picture
  and the Physics Assets material sheets held 19.7%. They did not read as broken;
  they read as a legitimately cropped sheet, which is why two prior sweeps went past
  them. Every source that declares a canvas is now measured against what it actually
  draws, and reframed to its real extent when the two disagree. Sources whose canvas
  is correct — 800 of the pool's 806 — render byte-for-byte as before. `aa`'s pool
  builder grew `--rerender` so a rasteriser fix can reach a pool that already exists,
  instead of being skipped as "already on disk" (#685). No-spec-impact.

- **`detect_cropped_renders.mjs` is retired, not retuned.** It flagged 41% of a
  known-good pool, which trains everyone to ignore it — and that is how the above
  survived. Swept against ground truth over 9,504 combinations of its thresholds,
  alpha cutoff, agreeing-edge count and minimum pixel floor, **none** found all six
  genuinely lossy files and the best precision reached anywhere was 0.043. The signal
  is not there to be tuned: edge coverage measures a drawing's silhouette where it
  meets the frame, and that silhouette is identical whether the frame was right or
  cut. `seed/scripts/probe_render_loss.mjs` is the crop gate now — it compares the
  render against the source, covers all 1,031 pool sources rather than the 806 it used
  to, and no longer counts its own measurement boundary as lost artwork (#685).

## [v0.7.0] — 2026-07-28 — Browse correctness, visibility security, and a real seed catalogue

### Security

Four separate leaks, all found in one week and all the same underlying mistake: a
read path that wrote out the "who may see this" rule itself instead of asking the
one component that owns it. Each copy was correct when written, then the shared rule
moved and the copy didn't. None was caught by a test. They are grouped here because
the pattern matters more than any one of them (#665).

The last entry below is a different thing — a permission that was too broad rather
than a rule that drifted — but it is the same data class, so it belongs here.

- **Anyone signed in could read anyone else's private posts.** Adding
  `?visibility=private` to the post list returned other people's private posts —
  title and body — while opening the same post directly correctly refused. No special
  role was needed; an ordinary account was enough. The list and the single-post view
  now run the *same* rule, so they cannot disagree again, and a test enumerates the
  visibility tiers from the database itself so a tier added later is covered without
  anyone remembering (#660).

- **Any signed-in user could read other people's private collections.** The IIIF
  manifest route returned a collection's name, description and full member list with
  no permission check at all for signed-in callers (#661).

- **On a public install, adding a tag filter exposed unpublished work.** Browsing
  anonymously with `?tag=…` returned draft, archived and restricted assets that the
  unfiltered browse correctly hid, because the tag-filtered branch was a separate
  query that never got the visibility rule. Measured on the reference install: 34
  items including 17 drafts, versus 5 after the fix (#657).

- **Related-asset and IIIF manifest routes leaked unpublished assets anonymously**,
  including using a draft asset as a similarity anchor. Also fixed the same drift
  pointing the other way: genuinely public collections were returning "not found" to
  anonymous visitors (#661).

- **Session IP addresses are now a separately granted permission.** Viewing a user's
  sessions in the admin area required only the ordinary "read users" permission, yet
  it showed raw client IP addresses — personal data that the audit log had already
  been gating behind a dedicated permission. The two now match: the session list is
  still visible, the addresses need the additional grant, and a new decision record
  fixes the naming so the next surface carrying personal data doesn't invent a third
  standard (#573, ADR 0072).

### Operator-facing changes

- **The server image is half the size: 3.64 GB → 1.82 GB.** Blender is no longer
  packaged. It was 1.3 GB of the image — roughly a third — plus the ten X/GL
  libraries it loaded at startup, and since the three.js renderer landed (#498)
  nothing in the product invoked it: every 3D format in the reference catalogue
  (`glb`, `obj`, `fbx`, `gltf`) already rendered through the three.js worker, and
  a search of the whole catalogue for the Blender-only formats returned nothing.
  `stl`, `ply` and `dae` moved onto the worker with this change so they keep
  their thumbnails. Formats with no three.js loader (`.blend`, `.usd*`, `.abc`,
  `.x3d`) get no generated thumbnail for now — the file itself still uploads,
  downloads and serves normally — and regain one when the Blender converter
  ships as an optional plugin (#499). Nothing to do on upgrade; no configuration
  changed. (#500, ADR 0069 amended.)

  Two smaller consequences worth knowing: arm64 deployments are unaffected
  because they never had Blender in the first place (its tarball is x64-only) —
  they have had 3D previews since #498. And the `AA`-side escape hatch that
  forced the old renderer is gone; with one renderer there is nothing to switch
  to. Nobody had it set.

### User-facing changes

- **The seeded demo library now has eleven working studios instead of one.**
  site_a shipped 1,007 assets in which **Animation and Characters had none at
  all**, Marketing Art had 3 and Textures had 8, while Environment held **47.3%
  of everything**. Clicking a studio either showed an empty page or showed the
  whole dataset. It now holds **1,946** assets with every team between 116 and
  421, and Environment down to **21.6%** — a studio with a specialism rather
  than a studio plus ten placeholders (#572, closes #562).

  Two levers, because a floor alone would have left Environment at 37%. **55
  records were on the wrong team in the source data and said so in their own
  tags** — 34 minimap icons tagged `ui`, 18 tiling texture plates tagged
  `texture`, 3 voiceover clips tagged `voiceover` — and moving them to UI /
  Textures / Audio is a correctness fix that happens to cap the biggest team.
  The other 895 are new, drawn from the CC0 Kenney bundle the library already
  came from and of which only ~1.3% was in use. Nothing was deleted: posts,
  collections and sibling groups all reference those ids.

  The **floor is 60 per team, and it comes from the product** — `/search`
  returns 25 results a page and the browse rails render 24 tiles, so a team
  whose whole library fits in one response has nothing to scroll and nothing to
  narrow. It reads as a stub even when it is technically non-empty.

- **45 more video references, chosen to look like a game studio's.** Video
  coverage went from 47 clips to **92**. The additions are searched for
  deliberately — arcade cabinets, controllers and keyboards, neon and glitch
  plates, particles, smoke and sparks, pixel-art animation, esports floors —
  rather than generic stock, and each record records the search that found it.
  They land across Reference, VFX, UI, Marketing Art and Animation instead of
  piling into one bucket (#572).

- **Sponza renders instead of failing.** The canonical Khronos test scene was
  the one 3D asset in the instance stuck at `failed`, because its geometry
  buffer and 69 textures were never staged next to it — the copier attached a
  model's siblings only when it copied the model, so a model already present at
  the destination silently skipped its own companions. It now reaches `ready`
  with a turntable (#572, completes #486).

- **Assets with no preview picture now get a designed tile instead of a blank
  one.** Text and code files never get a rendered thumbnail, and a preview can
  also simply have failed — a 3D scene missing its geometry file, a photograph
  too large for the render cap. Both used to land on an anonymous grey landscape
  glyph that said "image missing" whether the asset was a CAD model, a README or
  a JPEG, which read as a broken tile rather than a deliberate one. The tile now
  states the two facts it actually has: the file's format, set as a wordmark, and
  its kind in plain language — `GLTF / 3D model`, `MD / Document`. Where the card
  does not already show the title next to the tile, the title is in it too, since
  a document is mostly its name. The tile composes itself to the space it has, so
  it holds up from a 60px masonry sliver to a full-width feed column. Rare on an
  image-heavy library — 3 of 1007 assets on the reference install — and much less
  rare on a document- or CAD-heavy one (#558).

### Accessibility

- **Form controls you could not see the edge of.** Inputs, selects and
  text areas drew their border in the same colour as a divider rule, which
  measured **1.38:1 in dark and 1.28:1 in light** against the surface behind it.
  On a divider that quietness is deliberate; on a control the border *is* the
  affordance — it is the only thing saying "you can type here" — and WCAG 2.2
  requires 3:1 for it (SC 1.4.11). 251 controls now use the strong border role,
  which was itself raised to clear the bar: measured on the rendered page,
  **1.38 → 3.98 (dark)** and **1.28 → 3.42 (light)**. Divider borders are
  unchanged; they carry no information and the low contrast there is intended.

- **Focus was easy to lose on those same controls.** 122 of them indicated
  focus by darkening that 1px border — a **1.95:1** change between the two
  states, and one that would have become invisible once the resting border was
  strengthened. They now draw the standard 2px focus ring, measured at
  **7.08:1 (dark)** and **3.39:1 (light)** against the page.

- **Secondary colour ramp fixed before anything used it.** Its white text
  measured 4.46:1 on the steel fill, under the 4.5:1 body-text floor. Now 4.85:1.
  No component is wired to this ramp yet, so nothing changes visually — the point
  is that the first one to reach for it does not ship a failure (#594).

### Fixes

- **The demo profile silently shipped 36 fewer assets than the studio profile it
  is a copy of.** `demo` and `dev` are aliases for `studio-a` and `studio-b`, but
  they were written before the dataset upgrade pass ran — so every upgrade since
  #604 landed on the studio profiles and missed its own aliases. A demo re-seed
  would have dropped all 36 added videos and nothing would have reported it. The
  aliases are re-copied after the upgrade, and a test asserts they match (#572).

- **A site could serve fewer posts than its dataset had.** `posts.json` was the
  one file the archive publisher never wrote — it was copied by hand — so site_a
  served 584 posts against a profile holding 859. The publisher now stages it
  (`--posts`), and warns when it is left stale (#572).

- **A missing source cache reported fully-staged assets as missing.** The
  internet-fetched cache is gitignored and usually absent on a machine that
  already has a populated site, so re-publishing reported 58 present-and-correct
  videos as MISSING and exited non-zero. Absence of a *source* is not absence of
  the *asset*; the check now confirms against the manifest's own byte count
  first (#572).

- **A few catalogue tiles showed a tiny graphic marooned in a big empty box.** The
  splat and line-pattern thumbnails rendered their artwork at about 1% of the tile,
  jammed into the top-left corner. Two separate things were wrong. The images the
  instance was serving had been rendered before the earlier canvas fix (#630) and were
  never re-loaded, so the catalogue was still handing out the old broken pictures. And
  the fix itself only went half way: it measures each drawing on a fixed-size search
  frame, so the safety margin it leaves is a fixed distance in the drawing's own
  coordinates — fine for a big drawing, a quarter of the picture for a small one. Small
  vectors came out filling half their frame. The renderer now measures a second time at
  the drawing's own scale, so the frame is tight whatever the size, and the 110 affected
  images were re-rendered and re-loaded. Images that were already correct are unchanged,
  including sprites whose source deliberately declares a padded canvas — those keep
  their padding. A new checker (`seed/scripts/detect_oversized_canvas.mjs`) measures how
  much of a frame the artwork actually fills; the existing one only sees artwork cut off
  at an edge and reads this failure as healthy (#672).

- **Resetting the demo left stale rows pointing at content that no longer existed.**
  `aa seed --reset` empties the content tables with `TRUNCATE ... CASCADE`, and CASCADE
  only follows foreign keys — so any table that names its target by a *kind + id* pair
  (which cannot have a foreign key) kept its rows while the things they referred to were
  deleted. Notifications about vanished posts, scheduled actions queued against deleted
  assets, workflow history for wiped assets, and featured placements for collections that
  were no longer there all survived every reset. Follower edges were worse: nothing linked
  them to the accounts they described, so each reset added a whole dataset's worth on top
  of the last (149 → 298 → 447 measured across three runs) while every earlier edge pointed
  at an account that no longer existed. A reset now finishes by deleting exactly the rows
  whose target is gone — rows that still point at something real, such as an action
  scheduled against the admin account, are left alone. Every such table in the database is
  now classified explicitly, with the storage pin table deliberately exempt, and a test
  fails if a new one is added without a decision (#569).

- **Search was broken for every signed-in user.** Every authenticated query returned
  an internal error and no results. A change months earlier had removed the "featured"
  flag from collections — featuring became a placement rather than a property — but the
  search query still asked for the old column, so the whole search failed rather than
  just the collection portion of it. Search works again; a test now pins the query
  against the real schema so a removed column cannot silently break it a second time
  (#650).

- **Masonry no longer reshuffles while you scroll.** Each time the feed loaded more
  results, the tiles you were already looking at jumped sideways into different columns.
  The layout balanced all columns by height across the entire list, so adding anything to
  the end genuinely changed where earlier items belonged. Tiles are now placed into
  columns as they arrive and stay put — loading more only ever grows one column downward.
  Measured: previously 30 of 36 visible tiles moved on each page load; now none do
  (#651).

- **Missing blur-up placeholders on posts.** Assets inside a post shipped without their
  tiny preview hash, so tiles popped in from blank instead of fading up from a blur, even
  though the data existed server-side (#648).

- **Masonry overlay controls no longer overflow thin tiles.** Giving each tile its true
  aspect ratio made audio waveforms genuinely thin — the narrowest measured 24px tall —
  while the selection checkbox and options menu need 44px each, so they spilled outside
  the tile they belonged to. Masonry tiles now have a floor tall enough to hold them,
  derived from the controls rather than picked; the overlay keeps only those two
  controls; and everything else about the asset moved into a tooltip that follows the
  cursor and sits outside the artwork it describes. The thinnest assets are slightly
  letterboxed as a result, which is the deliberate trade — a tile too small to click is
  worse than one slightly taller than its picture (#652).

- **Cards fetched images larger than the space they were drawn in.** The hint telling the
  browser how much room a picture would occupy advertised the largest size the install
  generates rather than the actual column width, so browsers downloaded oversized files —
  33% too large on a desktop wall, 113% on a phone. The hint now describes the real slot
  (#639).

- **A written post now returns the same shape a read does.** Creating or updating a post
  returned a response missing the preview availability, pixel dimensions and blur-up hash
  that every read path includes. Nothing visibly broke, because the app re-fetches after
  saving — but four such fields had quietly accumulated, and anything trusting the save
  response would have rendered a card with no picture and no placeholder. The same gap
  existed on two asset write paths and is fixed there too (#655).

- **Audio, 3D, video, fonts and ebooks had no blur-up placeholder at all.** The tiny
  preview hash was only ever computed when the uploaded file was itself an image, so
  every asset whose thumbnail is a *rendered* preview — an audio waveform, a 3D
  turntable, a video frame, a page render, a glyph specimen — had none, and its tile
  flashed blank before the picture arrived. Most visible on audio, which is both the
  largest group and the thinnest tile in masonry. Every preview format now computes the
  hash from the picture it just rendered, and a one-time sweep fills it in for assets
  already in the library — 618 of them on the reference install (#645).

- **The IIIF Image API returned 404 for every asset.** Both the image and
  `info.json` endpoints gated on `assets.has_image`, a column nothing in the
  codebase ever writes, so the condition was true for every asset and the whole
  Image API had been dead since it shipped — with no error, because "404" is also
  the correct answer for an asset that genuinely has no image. Image endpoints now
  serve real bytes, gated on whether a configured IIIF variant is actually stored.

  `info.json` had a **second, unrelated cause**, now also fixed: it reports an
  image's pixel dimensions, and nothing ever recorded them. The metadata
  extractor emits width and height, but no field definition existed to receive
  them, so the values were discarded and every `info.json` 404ed. The
  definitions are now seeded and wired to the extractor — on both fresh installs
  and existing ones. **The IIIF Image API is fully functional for the first time
  since it shipped** (#614, #618).

- **Widescreen art was square-cropped on cards.** Every card requested a
  single 320×320 centre-cropped thumbnail, because that was the only size
  guaranteed to exist. A 16:9 video or a wide illustration therefore displayed
  as a square — visibly disagreeing with its own hover preview, which used the
  true aspect ratio. Cards now pick an appropriately-sized image from the sizes
  this install actually generates, so wide art displays wide and large tiles
  stop showing upscaled thumbnails. The grid's contact-sheet view keeps its
  square crop, which is intentional (#502, #589).

- **Masonry now stacks tiles at their real proportions.** Previously every masonry
  tile was a fixed square, so a 16:9 video and a wide audio waveform were letterboxed
  into identical boxes and the view was indistinguishable from the grid. Tiles now
  follow each image's own aspect ratio — the space is reserved from recorded pixel
  dimensions before the image loads, so nothing jumps. The grid keeps its square
  contact-sheet tiles, which is intentional (#640).

- **Scroll position survives closing an asset or post.** Opening a post from deep in
  the feed and closing it returned you to the top, losing everything you had scrolled
  past. Position and loaded pages are now restored on the post, asset, collection and
  profile routes (#584).

- **The viewer's minimize button did nothing when the navbar was hidden.** Minimizing
  now brings the navbar back, with search usable, instead of leaving the viewer
  indistinguishable from its maximized state (#635).

- **Masonry view rendered as a single full-width column.** For five days the masonry
  layout showed one enormous tile per row instead of a multi-column wall — a CSS length
  property was given a percentage, which silently voided the whole declaration and fell
  back to "one column". Masonry now forms columns that track the tile-size control, on
  desktop and phone alike (#637).

- **Cropped artwork in the seeded catalogue.** Some vector-sourced thumbnails were
  missing chunks of their artwork — cut off mid-shape at the edges. The source files
  declare no canvas size, and the renderer was guessing one and clipping anything that
  fell outside it. 110 affected images were re-rendered; the renderer now measures each
  drawing's real extent first (#630).

- **Viewer gap when the navbar auto-hides.** Opening a post after scrolling far
  enough that the navbar had slid away left a navbar-sized gap above the viewer,
  with the feed's tiles bleeding through. The viewer's top edge was glued to a
  measured navbar height that never updated when the navbar hid (a transform,
  which resize observers can't see). It now tracks the navbar's actual state —
  expanding flush to the top of the screen when the navbar hides, and yielding
  the space again when it returns, with a matching animation (#628).

- **Regenerated previews never reached the browser.** Asset byte routes shipped
  `Cache-Control: immutable, max-age=31536000` with an ETag derived from the URL
  path — a validator that cannot change — and answered conditional requests with
  304 without ever consulting the stored bytes. Once a client had cached a
  variant, no sequence of requests could return updated content, so an operator
  who used "Recreate previews" after a renderer fix could never see the result.
  Validators are now derived from the stored bytes, and revalidation is permitted
  (#620).

- **EXIF metadata extraction processed zero assets.** The backfill selected on the
  same never-written `has_image` column, so a run would report success having
  enqueued nothing. It now selects on file format — the formats the metadata
  pipeline actually has an extractor for, EXIF plus camera raw. (Extracted values
  still need field definitions with an extraction source configured before they
  land anywhere; tracked in #618.) (#579)

- **Admin featured-content thumbnails.** The curation list at
  `/admin/content/featured` could not render a thumbnail for *any* subject:
  asset tiles were gated on the same never-written column (fixed with it), and
  collection tiles had no cover resolution at all — the public rail resolved
  covers since #559, but the admin list never received the same treatment.
  Operators now see real covers for both subject kinds, including team-tier
  covers the public rail rightly refuses to anonymous visitors (#619, #625).

- **AI asset hints never identified images.** The AI bridge derived its MIME hint
  from the same dead column, so it was never set. It now derives a real MIME from
  the file extension (`image/png` rather than the `image/*` wildcard it aspired
  to) (#579).

### Operator-facing changes

- **New capability `users.pii.read` — session IP addresses now need it.** The admin
  view of a user's sessions (`/admin/users/{ref}/sessions`, and the "Active sessions"
  panel on the user detail page) returned each session's raw client IP to anyone
  holding `users.read`, while the audit log has required a dedicated
  `system.audit.pii.read` for actor IPs since v0.5.0. Same data class, two different
  bars — so the looser one was raised rather than the stricter one lowered. `users.read`
  still lists the sessions, labels the devices, and revokes them; the address is
  additionally gated on `users.pii.read`, exactly as audit gates actor IPs. `system.admin`
  is unaffected (it satisfies every capability). **Operators who want an existing
  non-admin role to keep seeing session IPs must grant it the new capability** — the
  field is simply absent otherwise, never blank. Documented as a rule for every future
  IP-bearing surface in ADR 0072 (#573).

### API

- **Removed: `asset_has_image` from featured-item payloads.** It reported whether
  a raster thumbnail existed for a tile's cover asset, and it was **always
  false** — the underlying database column had no writer anywhere, in any
  install. Clients should use `preview_available`, which is computed from live
  variant existence and has been the trustworthy signal since it was added. No
  client behaviour changes, because nothing could have usefully depended on a
  field that was universally false. The column itself was dropped in the same
  change (#579).

- **`ladder_available` on asset payloads** — reports whether the *complete*
  configured preview ladder exists for an asset, so clients can build a responsive
  `srcset` instead of assuming a single thumbnail size. Computed against the
  operator's configured rungs rather than a hardcoded list, so an install that
  tunes its ladder is described accurately (#591).

- **`GET /previews`** — the rung keys and dimensions this install generates, so a
  client can build width descriptors without hardcoding defaults. Governed by
  public mode: anonymous on a public install, 401 on a private one (#591).

## [v0.6.0] — 2026-07-23 — Public read surface + demo hardening

### User-facing changes

- **Public user-profile pages.** Every user now has a profile page, reachable by
  username (`/users/by-username/{name}`) or stable ref, showing their display
  name, avatar, and the assets/posts/collections a viewer is allowed to see. It
  reuses the existing visibility rules — anonymous visitors see only public work
  (and only when public mode is on), and an owner can opt out of anonymous
  exposure. This also cleared the last of the dead author/similar-asset links
  (#478).

- **Shared view controls across every asset surface.** The browse view switcher
  (grid / masonry / thumbnail / list) and sort direction now appear on the
  profile and post-by-asset pages too, not just the main browse — one consistent
  control bar everywhere assets are shown (#511).

- **Faster 3D previews, and multi-file models fixed.** Open-format 3D previews
  (glTF/GLB, FBX, OBJ) now render through a headless three.js worker instead of
  Blender — much faster, and **arm64 deployments get 3D previews for the first
  time** (the Blender path was amd64-only). Multi-file glTF (a `.gltf` plus its
  external `.bin`/textures) now renders correctly, where before it failed
  silently (#497/#498/#507/#508, #486). Blender stays as an automatic fallback.

### Fixes

- **Federation-path query bug.** A metadata-adapter query referenced a
  nonexistent column (`owning_team_id` instead of the real `team_id`), so that
  path errored on every call. Pre-existing since ≤v0.5.2 and invisible to
  standard CI (which doesn't run federation); caught by the federation nightly
  and fixed before this release (#538).

- **CI reliability.** A large hardening pass on the test suite — shared-auth
  setup resilience, worker-isolation races, and timeout tuning — so a green run
  genuinely means green, not retry-masked (#485, #481, #505, #527, #535).

## [v0.5.2] — 2026-07-21

A content-visibility capability so read-only viewers (the public demo) can see
their whole catalogue.

- **`content.read.all` capability (#474).** A content-plane-only read cap,
  honored solely in `visibility.CanReadContent` alongside the `system.admin`
  wildcard — it grants asset-byte reads at every sensitivity tier and nothing
  else (no admin surfaces, no writes; it is not a wildcard). This lets a
  read-only role (e.g. the demo viewer) see `team`/`restricted` content that
  would otherwise return blank "Preview unavailable" tiles, without exposing
  any administrative surface. Migration `00014` defines the cap; granting it to
  a role is a deploy-side provisioning step (ADR 0060).

## [v0.5.1] — 2026-07-21

Promoted all of `dev` since v0.5.0 — the foundation work below (audit
retention/export, scheduled actions) plus two demo-surfaced fixes and a
visibility-consolidation batch. A patch version number, a substantial release.

### Operator-facing changes

- **Audit-log retention and export.** The audit log now has a retention
  policy — configurable per event category (a default of 7 years, with
  shorter or longer holds per category), a legal-hold flag that exempts
  individual events from purge, and a nightly enforcement pass. A GDPR
  erasure request anonymises a user across the log — the events are
  kept, the person is replaced by a `deleted-user` placeholder — so the
  trail survives without the personal data. And the whole log can be
  exported as CSV or NDJSON over a date range, streamed so exports of
  millions of rows don't exhaust memory; IP addresses are withheld from
  the export for callers who can't see them in the live view.

- **Scheduled actions.** Operators (and, later, the privacy, commerce and
  audit-retention features) can now schedule a change to run at a future
  time — change an asset's sensitivity, soft-delete, change state, or
  notify — and cancel it before it fires. Each action executes atomically
  with its audit entry, so it either fully happens and is logged or fully
  does not; a failure is recorded rather than half-applied. This is the
  generic engine (ADR 0020); the asset-gating features that use it —
  blur, reveal, timed embargo lift — land in later sprints.

### User-facing changes

- **Shareable, reloadable asset pages.** Assets now have a real
  `/assets/[id]` page, so a link to an asset opens, reloads, and shares
  correctly. Before this, clicking an asset inside a collection
  dead-ended on a "Not found" page — the tile linked to a route that
  never existed (#475). A build-time link-integrity check now guards
  against dead internal links (ADR 0068).

- **3D previews work on published builds again.** Turntable thumbnails
  for 3D models (glTF / OBJ / FBX and more) had silently stopped
  generating on released images — the published image shipped without
  the renderer — so every 3D asset showed no preview (#470). Fixed for
  amd64, with a build-and-render smoke so it can't regress unnoticed.

### Fixes

- Content-visibility hardening: soft-deleted collections no longer
  appear to signed-in non-owners, and the IIIF image path enforces the
  same visibility rule as the browse grid (#451, #460), plus audit and
  admin-gating cleanups (#458, #431).

## [v0.5.0] — 2026-07-20 — Public mode: anonymous browsing

Content is now reachable without an account, on an operator's terms. The
visibility model got a single enforcement point, sensitivity moved to the
content plane, and opening the surface surfaced (and closed) three
pre-existing access holes in the foundation it was built on.

### Operator-facing changes

- **A `public` visibility tier now exists** for collections and posts, and
  anonymous callers have a defined, enforced view of content: published,
  public, ready assets and public collections/posts only. Content
  visibility is decided in exactly one place — the visibility predicate —
  which every read path splices in (ADR 0063).
  Authenticated behaviour is deliberately unchanged. An authenticated
  caller still *sees* assets of every sensitivity in listings — that is
  intended, not a gap: sensitivity gates the bytes, never the rows, so
  restricted material stays listed as a locked item rather than
  vanishing (ADR 0020 via ADR 0064).
- **Asset browse now goes through that same predicate.** The browse query was
  sqlc-generated static SQL, which cannot accept a runtime fragment — it was
  the one read path visibility could not reach. Converted to hand-built SQL and
  gated. The superadmin-only `include_deleted` flag waives the soft-delete
  check **and only that** — publication status, sensitivity and processing
  state still apply, so the flag cannot drift into meaning "skip authorization".

- **Asset sensitivity is now enforced when serving files.** Previously any
  authenticated caller could download any asset's bytes — including `draft`
  and `restricted` material — because the byte-streaming endpoints checked
  only that a caller was signed in. Sensitivity now gates **content**: `team`
  assets require team membership, and `restricted`/`embargo` are limited to
  the owner and system administrators. Listing is deliberately unchanged —
  restricted assets remain visible as locked items rather than vanishing
  (ADR 0064, following ADR 0020). Denials return 404 rather than 403 so a
  response cannot be used to confirm that a restricted asset exists.

- **Two remaining copies of the visibility rule were removed, and a
  latent IIIF gap was found in the process.** Reverse-image search
  carried its own hand-written "anonymous sees public only" filter; it
  now uses the same visibility predicate as every other read path,
  which also correctly hides draft and still-processing assets that the
  old copy let through. The IIIF manifest layer keeps its own
  sensitivity gate — investigation confirmed it is not a duplicate but
  the *only* thing refusing a restricted asset's manifest to an
  anonymous caller, and a misleading code comment that invited its
  removal was corrected.

- **Audit-log IP addresses are now gated behind their own capability.**
  A read-only auditor could previously see the IP of every actor in the
  log — personal data that identifies people and approximates their
  location — because it rode along with the ordinary
  `system.audit.read` view. Seeing *what happened* and seeing *from
  where* are now separate grants: `system.audit.read` returns the log
  without IPs, and a dedicated `system.audit.pii.read` is required to
  see them. The address is withheld at the API, not merely hidden in
  the UI.

- **Access requests can no longer name a capability that doesn't exist.**
  `requested_capability` on an asset-access request was free text stored
  verbatim, in a field that feeds an authorisation decision — so a
  requester could put anything at all in it. It is now constrained to
  the real capability registry by a foreign key, and a request naming an
  unknown capability is rejected with a clear 400 instead of failing
  deeper in. Deleting a capability that still has outstanding requests
  now fails loudly rather than silently discarding the record of who
  asked for what.
  This narrows the field rather than fully securing it: a request can
  still name a *real* capability the requester shouldn't be able to ask
  for. Which capabilities are legitimately requestable is decided with
  the access-grant flow, which remains deliberately unbuilt.

- **A logged-out visitor now has something to look at.** Curated
  content can be featured for a public audience, and the front page
  renders it. Featuring is now a placement rather than a flag on the
  thing featured — the same collection can be featured publicly and
  internally at once, with its own ordering in each, and an individual
  asset can be featured without wrapping it in a collection.
  Two separate featured mechanisms had grown up side by side; there is
  now one. Featuring never widens access: a featured item renders only
  if the viewer could already see it, so publishing the rail does not
  publish the library.

- **Public browsing is now an operator choice, and it is off by
  default.** Anonymous access had no switch: any instance running this
  code served its public content to the internet whether the operator
  wanted that or not, and an existing install would have had it turned
  on by an upgrade. There is now a setting for it, enforced at the API
  rather than by hiding pages — turning it off means anonymous requests
  are refused, not merely unlinked. A fresh install starts private, and
  first-boot, login and SSO keep working with it off.

- **Signing in no longer hid public collections, and logged-out
  visitors could no longer see private ones.** Two visibility defects
  surfaced while opening anonymous access, both now fixed. An
  authenticated user got "not found" on a public collection they did
  not own — signing in *removed* access, and an administrator saw less
  than a logged-out stranger. Separately, the collection **list**
  endpoint applied no visibility rule at all, so an anonymous request
  returned every collection in the system, private ones included, with
  their names. Listing now goes through the same single visibility
  decision as every other read path.

- **A collection's contents are now visible to logged-out visitors —
  and were previously readable by any signed-in account.** Listing what
  is inside a collection applied no visibility check at all: any
  authenticated caller could enumerate the full contents of any
  collection by id, including collections they had no access to, and the
  response carried titles, types and publication status for draft
  material. The endpoint now checks the caller may see the collection,
  and filters the contents themselves — so a public collection shows
  only its public items to an anonymous visitor, while its owner still
  sees everything. Public collection pages render their contents rather
  than appearing empty.

- **Browsing without an account now works.** Listing assets and
  collections, and opening a single asset or collection, no longer
  require a signed-in caller: `GET /assets`, `GET /assets/{id}`,
  `GET /collections` and `GET /collections/{id}` serve anonymous
  requests, with the visibility predicate deciding what comes back —
  published, public, ready content only. Every write path still
  requires authentication.
  **This also closed a pre-existing hole**, which is the more important
  half: the two detail endpoints previously checked only that *some*
  caller was signed in and then fetched by id, so any authenticated
  account could read any asset or collection — including another
  user's private collection — simply by knowing its id. Both now run a
  real visibility check, and a denial returns 404 rather than 403 so a
  response cannot confirm that a hidden item exists.
  One consequence to expect: a public collection's *contents* are not
  yet anonymous, so a logged-out collection page shows its title and an
  empty body until that lands separately.

- **Anonymous visitors can now load public images.** The byte-streaming
  endpoints previously required a signed-in caller before anything else
  ran; they now defer to the same content check, which admits anonymous
  callers to `public`-tier assets and nothing else. `team`, `restricted`
  and `embargo` bytes remain unreachable without an account, across
  every byte-serving path (originals, derivatives, HLS segments and
  archive entries). This is the first surface where an anonymous request
  receives real content rather than metadata — the metadata endpoints
  are still authenticated and land separately.

### Infrastructure / housekeeping

- **The site now rebuilds from a signal that can fail.** When docs
  this repo owns change, the marketing site was rebuilt by firing a
  Cloudflare deploy hook — a bare POST that reports success for having
  been sent, not for a build that worked. Nineteen production deploys
  failed over twenty-four hours behind that signal with nothing to show
  it. The trigger now dispatches to the site repository instead,
  carrying the exact commit that changed so a rapid second push cannot
  cause the wrong content to be built, and a rejected credential fails
  loudly rather than skipping silently.


- `app/schema.sql` refreshed from a cleanly migrated database. The
  committed copy had drifted in **column order** — Postgres physical
  order is creation order, so columns added by later migrations land at
  the tail, and the stale file described an order the migrations never
  produce. That silently changed which Go types sqlc generated. Query
  column lists were realigned with the real schema; pg_dump's
  `\restrict`/`\unrestrict` markers are stripped so the file is
  byte-reproducible.
- Version files corrected to 0.4.0 (they had been left at 0.3.1).

## [v0.4.0] — 2026-07-18

Operator visibility: the async pipeline and the storage layer are now
observable and manageable from the admin surface. No-spec-impact.

### Operator-facing changes

- **Jobs admin.** The whole async pipeline (derivatives, previews, AI
  tagging, federation outbox) runs on the job queue, and until now it could
  only be inspected with `psql`. New surfaces, read-gated on
  `system.jobs.read` so a read-only operator can watch without holding
  `system.admin`: **queue** (jobs by status/type with age and priority),
  **workers** (active workers, lease state, stale-lease flag), **live**
  (status counts), **failed** (with `last_error`), **kinds** (per-type
  concurrency), **schedules** (future-dated work). Requeue, cancel, and
  concurrency edits require `system.admin`; a job that is currently running
  is never touched by either action.
- **Storage admin.** **Usage** (deduplicated bytes on disk, originals vs
  derivatives, breakdowns by content type and backend) and **variants**
  (per-family inventory), read-gated on the new `system.storage.read`.
- **Storage integrity sweeps.** `orphan_scan` reconciles the object store
  against the database in both directions; `checksum_verify` re-hashes
  stored bytes against the content-addressed key. Both run as batched,
  resumable job kinds, so they appear in the jobs queue like any other work,
  and both report into an admin surface. Findings are **advisory** and
  record scan time; no destructive cleanup ships in this release.
- **About reports the real version.** The page previously showed a
  hard-coded placeholder. It now reads a new anonymous `GET /build-info`
  endpoint serving the version baked in at build time. The displayed licence
  was also corrected to AGPL-3.0-only, matching the repository.
- **Help is visible to read-only operators.** Documentation, shortcuts,
  about, release notes, and support are now explicitly public admin tiles
  rather than implicitly superuser-only, and appear identically on desktop
  and mobile (ADR 0061).

### Infrastructure / housekeeping

- Storage backends gained an ordered, cursor-resumable `List` (ADR 0062).
  Filesystem walk order is not lexicographic over the key space, so the fs
  backend prunes and sorts to honour the contract; a shared contract test
  enforces it for every backend.
- Dependabot grouped per ecosystem into minor-and-patch versus majors with a
  lower open-PR limit, so routine bumps stay auto-mergeable and a batch no
  longer starves the self-hosted runners ahead of a release.
- Pre-checkout stale-`.git`-lock sweep on every self-hosted job, fixing
  intermittent checkout failures caused by cancelled mid-fetch runs.

## [v0.3.1] — 2026-07-17

Admin read-cap UI + foundation cleanup. No-spec-impact.

### Operator-facing changes

- **Admin UI for read-cap holders.** The frontend half of v0.3.0's read
  capabilities: the admin menu + route guard now gate **per-tile on the
  capability each surface enforces**, so a read-only role (without
  `system.admin`) sees and can browse the admin sections its caps permit —
  the admin menu lights up on the public demo. Backend still enforces every
  write.

### Infrastructure / housekeeping

- Repo-wide `gofmt` normalization + a `gofmt -l` CI gate.
- `make release` target codifying the release prep (version bump, openapi
  regen, drift check, open the promotion PR) — does not tag or toggle
  protection.
- Dependabot `github-actions` group split (routine bumps auto-merge; majors
  gated); steel secondary token wired into the Alert info tone.
- CHANGELOG + roadmap reconciled to current (they had drifted two releases
  behind).

## [v0.3.0] — 2026-07-17

Derivatives, read-only admin, responsive UI. No-spec-impact.

### Operator-facing changes

- **Media derivatives generated on seed/upload.** `aa seed` (and the
  upload path) now produce `col`/`hires`/`screen` thumbnails plus
  `sprites.jpg` video hover-scrub sheets — the browse grid renders real
  thumbnails instead of 404ing, and videos get a slideshow preview.
- **Read-only admin access.** A role can hold `*.read` admin
  capabilities and browse the admin surface **without** the
  `system.admin` superuser cap — six previously superuser-only surfaces
  (activities, featured, license, metadata-extraction, federation,
  requests) now render read-only, and the admin menu + route guard show
  each section per the capability its handler enforces. Backend enforces
  every write regardless.
- **Responsive + accessible UI.** Browse + navbar are fluid from a 390px
  phone to a 3840px / 32:9 ultrawide — an `auto-fill` grid where size is
  the lever and column count is the outcome (no breakpoint cliffs), an
  Instagram-style single-column `feed` view, hide-on-scroll chrome, and
  WCAG 2.2 AA target sizing on coarse pointers. Desktop layout unchanged.
- **Featured content curation** is seeded, so the admin Featured rail and
  the public collections featured tab both show content on a fresh seed.
- **Operator-bug fixes.** `PATCH /admin/system/site` now merges instead
  of blanking omitted fields (was: updating base_url wiped the site
  name); unroutable file extensions no longer mint guaranteed-terminal
  preview jobs; the nightly `ref` dispatch footgun is closed.

### Infrastructure

- CI/nightly stability arc — per-run compose isolation + resource caps,
  and five stacked shared-daemon/host causes fixed; the federation
  nightly is green for the first time since 2026-06-21. Repo-wide `gofmt`
  normalization + a `gofmt -l` CI gate.

## [v0.2.0] — 2026-07-16 — Admin surface unlock + public demo

Post-v0.1.2 incremental work. No-spec-impact.

### Operator-facing changes

- **Admin tiles unlocked (Tier 1–2).** The admin surface is now fully
  navigable: audit-log viewer (`/admin/audit`), per-user active
  sessions + capability grants/revokes, resource requests, **trash**
  with soft-delete restore across assets/posts/collections, system
  log, and an **API explorer served from the Go binary**
  (`/api/v1/openapi.json`, replacing the external-spec fetch).
- **`AA_DEMO_MODE`.** Env-gated demo mode — a `demo`/`demo` credential
  hint + fill button on the sign-in screen and a read-only banner
  when signed in as the demo user. Off by default; zero footprint on
  real installs.
- **Public read-only demo** at `demo.artist-alley.org` — runs the
  release image behind a write-blocking nginx edge, seeded from the
  Layer-A dataset, and auto-redeploys on each release.

## [v0.1.2] — 2026-07-15

> Reconstructed from the `v0.1.1..v0.1.2` commit range — this release was
> tagged without CHANGELOG or GitHub release notes at the time.

Brand, polish, and dependency hygiene; no wire-format changes.

### User-facing changes

- **Burnt/Steel brand.** Repaletted to the burnt accent + steel secondary,
  wired through components; finalized the chevron mark and the configured
  site-name handling; enlarged the sign-in brand mark; added a `viewBox`
  to the favicon/logo SVGs so the browser-tab favicon renders.
- **API docs are cleaner.** A usable getting-started, clearer error
  documentation, and internal phase codes dropped from the published spec
  (the first pass of the ongoing scrub).
- **Install quickstart fixed** — corrected `AA_MASTER_KEY`, the image path,
  the cosign identity, and pgvector setup.

### Fixes

- **Per-type job concurrency caps** are now applied in the single-process
  worker pool.
- **Saved-search notifications** no longer hot-loop — reschedules are
  grid-aligned.

### Infrastructure / housekeeping

- Supply-chain forks retargeted from `mscrnt/*` to `Artist-Alley-Org`.
- `pdfjs-dist` upgraded to v6; dependency sweep clearing Dependabot alerts.
- Test suite isolated from the shared dev database (#291); CI prunes
  dangling images to stop a runner disk leak.
- Real-world IP scrubbed from published surfaces; ArchivePub stamped
  v1.0-final (spec-only).

## [v0.1.1] — 2026-07-13

> Reconstructed from the `v0.1.0..v0.1.1` commit range — tagged without
> notes at the time.

A patch release restoring media processing and clearing shipped-artifact
vulnerabilities.

### Fixes

- **In-process worker pool never claimed jobs** (nil `Types` + a gate
  guard), so media processing silently stalled after v0.1.0. Fixed (#279)
  — this is the reason v0.1.1 exists.
- **GHCR image owner casing** — the org rename broke edge + release image
  pushes; the owner is now lowercased (#280).

### Infrastructure / housekeeping

- Shipped-artifact vulnerabilities cleared (torch floor raised, `aa-clip`
  bumped, npm sweep) — all open Dependabot alerts closed.

## [v0.1.0] — 2026-07-11 — Encryption arc (Phase 1.22.I)

The full encrypted-federation arc (1.22.I-a through 1.22.I-i) is
shipped + dogfood-validated end-to-end. ArchivePub spec at
**v1.0-rc1** with Appendix A conformance test vectors locked.
Seven-day soak window through **2026-06-22**; v1.0 final ships
as a no-code spec-only commit if soak is clean (otherwise
v1.0-rc2 first).

### Operator-facing changes

- **New** `POST /account/security/rotate-federation-keys` —
  user self-rotation of the X25519 federation keypair. Previous
  key is retained for the configured grace window (default 30
  days) so in-flight envelopes still decrypt.
- **New** `POST /admin/federation/users/{ref}/rotate-keys` —
  operator-initiated rotation for compromised-key recovery.
  `rotated_by_user_ref` records the admin's `user.ref` so the
  audit feed distinguishes recovery from self-rotation.
- **New** `GET /admin/federation/key-health` — aggregate
  observability dashboard data: users without a keypair, remote
  actors missing encryption keys, peers without negotiated
  capabilities, retained keys near expiry. Drill-down rows for
  the first + last categories ride along.
- **Behavior** Federation activities for `restricted`-tier
  assets are now encrypted end-to-end via NaCl-box. Senders
  refuse to dispatch when the recipient peer hasn't negotiated
  the `nacl-box` capability OR the recipient's pubkey isn't
  cached locally.
- **Behavior** Receivers reject plaintext envelopes targeting
  `restricted`-tier assets with `reject_reason=encryption_required`
  + audit `federation.inbox.encryption_required_rejected`.
- **Behavior** Asset sensitivity is set at create time (default
  `public`) and consulted by both sender + receiver gates.
  Changing the tier post-create propagates to in-flight
  emissions automatically (intentional: simpler than copy-at-
  grant semantics; a follow-up phase can layer the alternate
  behavior on top if operator feedback demands).

### Wire-format additions

- `aa:encryptionPublicKey` block in actor profile JSON (v0.3).
- `supported_capabilities` field in peer handshake offer /
  confirm envelopes (v0.4).
- `encryption` block in envelope JSON — per-recipient NaCl-box
  ciphertext + sender/recipient key id+version + nonce (v0.5).
- New reject reasons: `decrypt_failed` (v0.6),
  `encryption_required` (active at v1.0-rc1).

### New conformance test vectors

Appendix A of the spec now lists the 8 active scenarios under
`scripts/dogfood/scenarios/` that any conformant ArchivePub
implementation MUST pass against a peer running the reference:

- `01-like-cross-instance` — wire signature + dispatch
- `05-restricted-asset-roundtrip` — receiver-side defense gate
- `06-wire-dispatch` — outbox dispatcher + sub-1s p99
- `07-encryption-key-distribution` — actor profile + remote-actor cache
- `08-capability-negotiation` — handshake intersection
- `09-outbox-encryption-sender-side` — NaCl-box envelope shape
- `11-refusal-flip` — sensitivity-driven sender refusal
- `12-rotation-lifecycle` — rotation + sweeper + admin observability

Scenarios 02, 03, 04 remain outline scripts pending product
wiring (collection share UI, cascade observability).

### Migrations

| # | Schema change | Phase |
|---|---|---|
| 00007 | `federation_user_keys` table — X25519 keypair storage with `is_current` partial unique + multi-version retention | 1.22.I-b |
| 00008 | `federation_remote_actors.encryption_public_key` columns | 1.22.I-c |
| 00009 | `federation_peers.capabilities` + `capabilities_negotiated_at` | 1.22.I-d |
| 00010 | `federation_outbox.was_encrypted` + sender/recipient key version observability | 1.22.I-e |
| 00011 | `federation_inbox.was_encrypted` + `decrypted_with_key_version` | 1.22.I-f |
| 00012 | `federation_outbox.refused_reason` + `status='refused'` admission | 1.22.I-g |
| 00013 | `federation_user_keys.rotated_at` + `rotated_by_user_ref` + `system_config.federation.user_keys.retained_until_days` | 1.22.I-h |
| 00014 | `assets.sensitivity` (tier vocabulary + partial index on restricted/embargo) | 1.22.I-i |

### Backend admin observability

- 3 new audit events: `federation.user.key_rotated`,
  `federation.user.key_retained_expired`,
  `federation.inbox.encryption_required_rejected`.
- Background `userkeys.Sweeper` goroutine — ticks every hour
  with a boot-time first sweep covering downtime expirations;
  emits one audit per non-zero reap (quiet steady state).
- Receiver-side dispatcher stage-3.5 — gates plaintext envelopes
  against the target object's sensitivity tier via the
  `SensitivityLookup` callback (currently resolves `asset`-kind
  objects; other kinds pass through pending their own
  sensitivity columns).

### Out of scope / deferred

- Per-peer policy overrides ("always encrypt to peer X")
- Cross-instance key revocation broadcasts
- Hardware-token / HSM integration
- Algorithm migration mechanics (X25519 → P256 / PQ)
- `federation_shares.sensitivity` copy-at-grant semantics
  (asset-axis sensitivity is the single source of truth at v1.0-rc1)
