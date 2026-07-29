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

> This paragraph lived only in the copy of this file staged alongside
> the published data. Correcting an output and not its input is how a
> fix un-fixes itself on the next run (#675) — **this file is the
> source**, and the publishing step copies it. Edit it here.

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
  - Every record from these packs carries machine-readable provenance
    in `MANIFEST.json` under `metadata.source_archive`: the pack page
    (where the CC0 dedication lives), the direct zip URL, the path of
    the file inside that zip, and that member's sha256. That is enough
    to re-derive any asset here from its original download without any
    tooling of ours.

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
- Included in the published dataset (owner decision 2026-07-25). Any
  wording elsewhere restricting these clips to the local-only set is
  stale — a #607 regression, corrected in #675.

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

## Studio simulation metadata

The dataset includes synthetic studio metadata — no real studio, no
real person:
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
