# Artist Alley — Demo Studio Seed (Layer A): Attributions

This dataset bundles content from many sources under several licenses.
It is published under **no single aggregate license**: each source's own
terms apply, as set out in **Aggregate license** below. This document is
the canonical attribution list for those sources.

If you redistribute this dataset (in whole or in part), you must
preserve this file and provide attribution to every source whose
content you include.

## Aggregate license

There is **no single aggregate license** any more.

The dataset was once published under **CC-BY-SA 4.0**, chosen because that
was the most restrictive license among the sources (the Tux Racer /
Hedgewars / OpenArena gameplay videos from Wikimedia). A single
aggregate claim is no longer used: the sources carry different terms, and
the 75 National Park Service videos (see below) carry a provisional
public-domain label whose basis is still under review.

**Per-asset `license` + `attribution` in `MANIFEST.json` are
authoritative.** Redistributors must attribute every source and honour
each source's own terms.

> This paragraph lived only in the copy of this file staged on the
> archive share. Correcting an output and not its input is how a fix
> un-fixes itself on the next run (#675) — the repo copy is the source,
> and `populate_archive.py` publishes it.

## Sources

### Game art + audio + UI + sprites + fonts (CC0)

- **[Kenney.nl](https://kenney.nl/)** — Multiple asset packs:
  - retro-fantasy-kit, retro-textures-fantasy, rpg-audio, rpg-urban-pack,
    light-masks, minimap-pack, blocky-characters, ui-pack-adventure,
    ui-pack-rpg-expansion, prototype-textures, development-essentials,
    music-jingles, voiceover-pack, kenney-fonts, animal-pack-remastered,
    pixel-line-platformer, format3d, icons
  - Added by the per-team rebalance (#572), each drawn from the pack's
    own free CC0 download at `kenney.nl/assets/<slug>`:
    animal-pack, animated-characters-protagonists,
    animated-characters-retro, animated-characters-survivors,
    blaster-kit, brick-pack, cube-pets, cursor-pack, fish-pack,
    flag-pack, food-kit, generic-items, holiday-kit, mini-characters,
    monster-builder-pack, new-platformer-pack, particle-pack,
    pattern-pack, pattern-pack-lines, planets, platformer-characters,
    prototype-kit, ranks-pack, robot-pack, shape-characters, skyboxes,
    splat-pack, survival-kit, toon-characters, ui-pack,
    ui-pack-adventure, ui-pack-sci-fi
  - Factory Kit 3.0: one model (`machine.glb`), the kit's `colormap.png`
    texture it references, and the kit's own `License.txt` (CC0), copied
    from the Kenney Game Assets All-in-1 3.6.0 bundle into
    `3d/kenney-allin1/3d-assets-factory-kit/`. Source page:
    https://kenney.nl/assets/factory-kit
  - License: **CC0 1.0** (public domain dedication)
  - All Kenney content is dedicated to the public domain by the author
  - Machine-readable provenance for the #572 packs — page URL, direct
    zip URL and served byte count — is committed at
    [`seed/upgrades/kenney-pack-sources.json`](upgrades/kenney-pack-sources.json),
    and every record additionally names the file inside that zip plus
    its sha256 (`metadata.source_archive`). `python3
    seed/scripts/kenney_pack_sources.py verify-records --records
    seed/upgrades/balance-assets.site_a.json` re-proves the whole set
    against the live downloads.

- **[PixelSpaces.io](https://pixelspaces.io/)** — UI kits + sprite packs
  - License: CC0 / free-pack equivalent (see PixelSpaces site)

- **[UISketch](https://www.kaggle.com/datasets/vinothpandian/uisketch)** — Hand-drawn UI sketches
  - License: **CC0 1.0**

- **[IconKitchen](https://icon.kitchen)** — Generated icon sets
  - License: free for any use (per IconKitchen terms)

### 3D models (CC-BY / CC0)

- **[Khronos glTF Sample Models](https://github.com/KhronosGroup/glTF-Sample-Models)**:
  - DamagedHelmet — © ctxwing, CC-BY 4.0
  - FlightHelmet — © Microsoft, CC0 1.0
  - BoxAnimated — © Cesium, CC0 1.0
  - Avocado — © Microsoft, CC0 1.0
  - BoomBox — © Microsoft, CC0 1.0
  - Duck — © Sony Computer Entertainment Inc, CC-BY-SA 3.0
  - Lantern — © Microsoft, CC0 1.0
  - WaterBottle — © Microsoft, CC0 1.0
  - BrainStem — © Keith Hunter / Smith Micro, CC0 1.0
  - CesiumMan — © Cesium, CC-BY 4.0
  - ToyCar — © 2017 Microsoft, CC0 1.0

### Video (CC-BY / CC-BY-SA / Public Domain)

- **[Blender Foundation](https://www.blender.org/about/foundation/)** open films:
  - **Big Buck Bunny** — © Blender Foundation, peach.blender.org, CC-BY 3.0
  - **Sintel** (trailer) — © Blender Foundation, durian.blender.org, CC-BY 3.0

- **Open-source game gameplay** (via Wikimedia Commons):
  - **Tux Racer** gameplay — CC-BY-SA 4.0 (Wikimedia contributor)
  - **Hedgewars** gameplay — CC-BY-SA 4.0 (Wikimedia contributor)
  - **OpenArena** gameplay — CC-BY-SA 4.0 (Wikimedia contributor)

- **Video game references** (via Wikimedia Commons):
  - **Pong** gameplay loop (1972) — Public Domain
  - **Donkey Kong arcade** — CC-BY 2.0 (Wikimedia contributor)

### Video: U.S. National Park Service B-roll (provisional public domain)

`videos/nps/<park>/<item-id>.mp4`: **75 clips** that replace the earlier
Pexels videos one-to-one, keeping the same asset IDs and posts: 63 from
Grand Canyon National Park, 7 from Death Valley National Park, 4 from
Joshua Tree National Park and 1 from Acadia National Park. Each is the
480p MP4 that the clip's NPS media page serves.

- Each clip's NPS credit is on its record (`attribution`), with its NPS
  collection page, media page, source URL and review flags under
  `metadata`.
- **What the collection pages say.** The Grand Canyon B-roll pages:
  "The video files that may be downloaded from these web pages are
  "public domain" videos", and the videos must not be used to imply NPS
  endorsement. The Joshua Tree index: "All video files in this archive
  are "public domain" clips", with the same endorsement limit and a
  statement that the NPS arrowhead is a protected symbol. The Death
  Valley page: "These silent videos are in the public domain and can be
  used for any purpose", with the request "Please credit "Courtsey of the
  National Park Service."" (spelling as published). The Acadia page:
  "All footage listed below is in the public domain."
- **What every NPS media page also says:** "Multimedia made available by
  NPS should not be presumed to be public domain. ... Contact the host
  park or program to ascertain who owns the material." This notice is
  recorded on every record (`metadata.media_page_notice`) and is not
  resolved here.
- **Status: provisional.** The `license` label records the collection
  pages' statement; it is not legal clearance or release approval. Each
  clip was reviewed from four sampled frames, ffprobe, a full decode and
  its audio level (all 75 are silent); that does not prove a whole clip
  is free of people, logos or other rights issues.
- Do not use these clips to imply endorsement by the National Park
  Service.

### Audio (CC0)

- **[Kenney.nl audio packs](https://kenney.nl/)**:
  - rpg-audio, music-jingles, voiceover-pack — CC0 1.0

### Documents / EPUBs (Public Domain)

- **[Project Gutenberg](https://www.gutenberg.org/)** EPUBs:
  - Frankenstein (Mary Shelley)
  - Pride and Prejudice (Jane Austen)
  - The Adventures of Sherlock Holmes (Arthur Conan Doyle)
  - Alice's Adventures in Wonderland (Lewis Carroll)
  - Moby Dick (Herman Melville)
  - The Time Machine (H.G. Wells)
  - A Tale of Two Cities (Charles Dickens)
  - Around the World in Eighty Days (Jules Verne)
  - The Picture of Dorian Gray (Oscar Wilde)
  - Treasure Island (Robert Louis Stevenson)
  - The Iliad (Homer, trans. Alexander Pope)
  - Walden (Henry David Thoreau)
  - License: **Public Domain** (US-side; check local laws)

- **Vue.js README** — © Vue.js contributors, MIT License (compatible)

### Fonts (Open Font License)

- **[Google Fonts](https://fonts.google.com/)** via SIL Open Font License (OFL):
  - Sono, Wellfleet, Playwrite DE Grund

### Images — astronomy + photography (Public Domain)

- **NASA** content (via Wikimedia Commons), all Public Domain:
  - Hubble Pillars of Creation — NASA, ESA, Hubble Heritage Team (STScI/AURA)
  - Earthrise from Apollo 8 — NASA / William Anders (1968)
  - Pale Blue Dot — NASA / JPL-Caltech / Voyager 1 (1990)
  - Saturn during Equinox — NASA / JPL / Space Science Institute / Cassini
  - Hubble Ultra Deep Field — NASA, ESA, S. Beckwith (STScI), HUDF Team
  - Mars Curiosity Self-Portrait at "Big Sky" — NASA / JPL-Caltech / MSSS (2015)

### Images — fine art (Public Domain / CC0)

- **[Met Museum Open Access](https://www.metmuseum.org/art/collection/open-access)** (CC0 1.0):
  - Aristotle with a Bust of Homer — Rembrandt van Rijn, 1653
  - Washington Crossing the Delaware — Emanuel Leutze, 1851
  - The Annunciation — Hans Memling, 1465–75
  - Self-Portrait with a Straw Hat — Vincent van Gogh, 1887
  - Madame Roulin and Her Baby — Vincent van Gogh, 1888
  - The Abduction of the Sabine Women — Nicolas Poussin, 1633–34

- **[Met Museum Open Access](https://www.metmuseum.org/art/collection/open-access)**
  (CC0 1.0) — the twelve works this dataset labels `mature: true`
  (#1217). They are classical nudes and academic life studies, added
  because the dataset needed content that HONESTLY carries the label:

  | Work | Artist | Date |
  |---|---|---|
  | Venus and Cupid | Lucas Cranach the Elder | ca. 1525–27 |
  | Venus and Cupid | Lorenzo Lotto | 1520s |
  | Venus and the Lute Player | Titian | ca. 1565–70 |
  | Venus and Adonis | Titian | 1550s |
  | Venus and Adonis | Peter Paul Rubens | probably mid-1630s |
  | Mars and Venus United by Love | Paolo Veronese | 1570s |
  | The Toilette of Venus | François Boucher | 1751 |
  | Pygmalion and Galatea | Jean-Léon Gérôme | ca. 1890 |
  | Study of a Female Nude | Henri Lehmann | 1840 |
  | Study of a Nude Man | Gustave Courbet | early 1840s |
  | Study of a Male Nude | Cavaliere d'Arpino | 1568–1640 |
  | Marble Statue Group of the Three Graces | Roman | 2nd century CE |

  Each entry carries its Met object id and object URL in
  `metadata.met_object_id` / `metadata.met_object_url`.

- **Wikimedia Commons** public-domain reproductions:
  - The Great Wave off Kanagawa — Katsushika Hokusai (c. 1831)
  - The Starry Night — Vincent van Gogh (1889), Google Art Project digitisation

### HDR environments (CC0)

- **[Polyhaven](https://polyhaven.com/)** (CC0 1.0):
  - studio_small_03 — Greg Zaal
  - studio_small_09 — Greg Zaal
  - adams_place_bridge — Greg Zaal
  - venice_sunset — Andreas Mischok
  - belfast_sunset — Sergej Majboroda
  - christmas_photo_studio_07 — Sergej Majboroda
  - abandoned_factory_canteen_01 — Sergej Majboroda

### Images — generated in-house (CC0)

- **Aurora R&D — AI-generated (Stable Diffusion 3.5 Large)** (CC0 1.0):
  45 images under `images/aurora-generated/`, four per team across all
  eleven teams (five for Reference), produced with **Stable Diffusion
  3.5 Large** run locally through ComfyUI. No third-party model output
  is redistributed here beyond what the model itself produced from our
  own prompts; the weights are not included in this dataset.
  - Every one carries
    `metadata.acquisition_source: "Generated in-house (Stable Diffusion 3.5 Large via ComfyUI)"`,
    `license: "CC0 1.0"`, and
    `attribution: "Aurora R&D — AI-generated (Stable Diffusion 3.5 Large)"`.
  - "Aurora R&D" is the fictional studio this dataset simulates — see
    **Studio simulation metadata** below. It is not a real company.
  - Carried in the pipeline by
    [`seed/upgrades/generated-assets.site_a.json`](upgrades/generated-assets.site_a.json)
    and the twelve posts in
    [`seed/upgrades/generated-posts.site_a.json`](upgrades/generated-posts.site_a.json).

- **Aurora R&D, authored in-house** (CC0 1.0): two plates under
  `images/aurora-authored/`, produced by `seed/scripts/authored_plates.py`
  (Python stdlib only, no image library) so the corpus can honestly carry
  the two `ai_provenance` states the 45 images above cannot (#1290,
  ADR 0094). Both carry `license: "CC0 1.0"`.
  - `studio-colour-chart.png`, record `530cb8f1-1aa4-ab97-87e8-30ad58ac59fb`,
    11,404 bytes, declares `ai_provenance: none`. A calibration chart: 24
    colour patches, a 21-step greyscale ramp and corner registration
    marks, every pixel placed by arithmetic in the script. No generative
    model is involved at any point, which is what makes `none` a
    statement of fact rather than a disclaimer. Its `attribution` names
    Aurora R&D as authoring it in-house with no generative AI (the exact
    string is on the record), and its
    `metadata.acquisition_source` is
    `"Authored in-house (deterministic plate generator, seed/scripts/authored_plates.py)"`.
  - `reference-mood-board.png`, record `c4542a8f-c9a1-edfa-f2f4-1fea6ab84d96`,
    1,290,128 bytes, declares `ai_provenance: assisted`. One of the
    Stable Diffusion 3.5 Large plates listed above,
    `images/aurora-generated/ref-colour-script.png` (record
    `4d1c5235-32dd-5faa-8a71-3b5f1d3151df`, named as `MOOD_BOARD_SOURCE`
    in the script), is downsampled into a panel and its palette strip is
    sampled from that plate's own pixels; the swatch grid, rules and
    registration marks around it are drawn by the script. Part of the
    work came out of a generative model and part did not, so neither
    `generated` nor `none` would be true of it. Its `attribution` names
    Aurora R&D as authoring it in-house over an AI-generated plate (the
    exact string is on the record), and its `metadata.acquisition_source`
    is
    `"Authored in-house (seed/scripts/authored_plates.py over a Stable Diffusion 3.5 Large plate)"`.
  - "Aurora R&D" is the fictional studio this dataset simulates (see
    **Studio simulation metadata** below). It is not a real company.
  - The repository carries the recipe, not the bytes: the plates are
    built once against the dataset source with
    `python3 seed/scripts/authored_plates.py build` before a publish, and
    the build is deterministic, so a rebuild is byte-identical and the
    sizes above do not churn.
  - Carried in the pipeline by
    [`seed/upgrades/authored-assets.site_a.json`](upgrades/authored-assets.site_a.json)
    and the two posts in
    [`seed/upgrades/authored-posts.site_a.json`](upgrades/authored-posts.site_a.json).

### Reference material (community contributed)

- **The Models Resource** (referenced game rips) — NOT included in this
  Layer A dataset. Those assets live only in site_b (the local-only
  full dataset) and never ship publicly.

## The `ai_provenance` declaration, and why only these 45

`MANIFEST.json` carries an optional `ai_provenance` on an asset. It is
the MAKER'S OWN DECLARATION about how the work was made, and the absence
of the key means **nobody was asked** — not "no AI". Exactly 45 entries
declare `generated`, and the two authored plates above declare `none`
and `assisted`, one each; every other entry omits the key entirely,
which is the honest state for a corpus assembled before the field
existed.

**Why nothing else in the dataset declares it.** It was tried the other
way first, and it was wrong. Two upgrade documents (`ai-declarations.site_a.json`
and its site_b twin) declared `generated` on four **Kenney.nl** works —
rows that carry `attribution: "Kenney (kenney.nl)"` on the same line.
That is a false statement about a named real creator, in a dataset that
is redistributed. Both documents were deleted in #1260 and the four rows
were stripped; `seed/scripts/apply_upgrade.py` now refuses any record
that declares AI without in-house provenance, so the pipeline cannot
make the same claim again. **A declaration must be about work we
actually made** — which is why the 45 images above exist at all.

**If you do not want them**, drop the 45 entries whose
`ai_provenance` is `"generated"` and the twelve posts that carry them
(the eleven `<Team> — AI reference set` posts plus `Backplate study — AI
plate beside a public-domain plate`); nothing else in the dataset
references them, except that the authored mood-board post
(`155b61b8-9c9e-58ac-46fe-fcc79f338555`) pairs `ref-colour-script.png`
with `reference-mood-board.png` and would keep only the latter.

## The `mature` label, and why these twelve

`MANIFEST.json` carries a boolean `mature` on every asset. It is a
CONTENT RATING and it is independent of `sensitivity_tier`, which is a
clearance: a public work can be mature, and a restricted one need not
be. Twelve assets are labelled; every other entry is `false`.

**Why classical fine art with nudity.** A DAM's mature flag exists for
the case a museum or a studio actually meets: work that is entirely
legitimate, entirely public, and that some viewers have not opted into
seeing. Classical nudes and academic life studies are that case exactly,
they are public-domain, and they are content an art team genuinely keeps
as figure reference. Labelling anything else in this dataset would have
been a lie told to make a test fixture exist.

**Why they are `public` tier.** Making them team-tier would have hidden
them behind a clearance and left the rating untested — the two axes have
to be able to disagree for either to be exercised.

**If you do not want them**, drop the twelve entries whose `mature` is
true and the two posts that carry them (`Life drawing reference` and
`Anatomy reference`); nothing else in the dataset references them.

## Studio simulation metadata

The dataset includes synthetic studio metadata generated by
`generate_metadata.py` in the broader artist-alley dataset:
- 30 fictional artist + reviewer names
- 11 teams (Animation, Audio, Characters, Environment, etc.)
- 13 project names (Project Echo, Project Mirror, Project Heroes, etc.)
- Per-asset review state, version, pipeline_stage, ratings

This metadata is original work by the dataset author, released under
CC0 1.0.

## Honor system

If you use this dataset, please:
- Preserve this ATTRIBUTIONS.md file in your distribution
- Provide aggregate attribution to "Artist Alley — Demo Studio Seed"
  in your work
- Honor each individual source's attribution requirement (per the list
  above)
- For commercial use: respect any source-specific commercial-use clauses
  (most Layer A licenses allow commercial use; check Kenney CC0 +
  Khronos CC-BY for the canonical reference)

If you spot a missing or incorrect attribution, please file an issue
at https://github.com/Artist-Alley-Org/artist-alley/issues — we'll fix it
immediately. Attribution accuracy matters.
