# Artist Alley — Demo Studio Seed (Layer A): Attributions

This dataset bundles content from many sources, every one of them
open-source, public-domain, or Creative Commons. This document is the
canonical attribution list — required by the CC-BY-SA 4.0 aggregate
license under which the dataset is published.

If you redistribute this dataset (in whole or in part), you must
preserve this file and provide attribution to every source whose
content you include.

## Aggregate license

There is **no single aggregate license** any more.

The dataset was published under **CC-BY-SA 4.0**, chosen because that
was the most restrictive license among the sources (the Tux Racer /
Hedgewars / OpenArena gameplay videos from Wikimedia). The Pexels
videos added in #605 and extended in #572 are covered by the **Pexels
License**, which is free-to-use but is not a Creative Commons license
and carries its own prohibitions (see below). A CC-BY-SA claim over the
whole set would therefore be false.

**Per-asset `license` + `attribution` in `MANIFEST.json` are
authoritative.** Everything except the Pexels videos remains CC-BY-SA
4.0 compatible; redistributors must attribute every source and honour
the Pexels terms for the `videos/internet/pexels-*` files.

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
  - Sponza — © Marko Dabrovic / Frank Meinl, CC-BY 3.0
  - WaterBottle — © Microsoft, CC0 1.0
  - BrainStem — © Keith Hunter / Smith Micro, CC0 1.0
  - CesiumMan — © Cesium, CC-BY 4.0
  - 2CylinderEngine — © Sony Computer Entertainment Inc, SCEA Shared Source License
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

### Video — Pexels (Pexels License, NOT CC0 / public domain)

`videos/internet/pexels-*` — **75 clips**: 30 added in #605, plus 45
game-adjacent clips added in #572 from these searches — arcade machine,
video game controller, neon lights abstract, particles floating, smoke
effect black background, glitch effect screen, pixel art animation,
keyboard gaming rgb, abstract motion background loop, sparks fire slow
motion, esports tournament, retro gaming console.

- Each clip's videographer is credited per-asset in `MANIFEST.json`
  (`attribution`), and `metadata.fetched_from` is the Pexels page the
  licence and the credit live on.
- **Permitted:** free use and modification; attribution not required
  (given anyway).
- **Prohibited:** selling unaltered copies; redistributing on other
  stock-photo or wallpaper platforms; use in trade marks; implying
  endorsement by the depicted people or by Pexels.
- **Approved for both site_a and site_b** (owner decision 2026-07-25).
  Any "site_b only" wording elsewhere is stale — it was a #607
  regression, corrected in #675, and is asserted against in
  `test_dataset_upgrade.py`.

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

### Reference material (community contributed)

- **The Models Resource** (referenced game rips) — NOT included in this
  Layer A dataset. Those assets live only in site_b (the local-only
  full dataset) and never ship publicly.

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
