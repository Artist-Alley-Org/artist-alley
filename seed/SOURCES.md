# Expanded source catalogue for seed data

What we've already pulled vs what's still on the table. The "next" column
flags items worth adding to `fetch_gaps.py` once we have an HTTP path
that doesn't go through archive.org (which currently 503s us for direct
downloads).

## Sources actively used by fetch_gaps.py

| Source | Items pulled | Licenses | Status |
|---|---|---|---|
| Blender Foundation (download.blender.org) | BBB, Sintel trailer | CC-BY 3.0 | ✓ direct URLs work |
| Khronos glTF Sample Models (GitHub raw) | 13 PBR / animation models | CC-BY / CC0 | ✓ stable |
| Project Gutenberg | 12 EPUBs (classic novels) | Public Domain | ✓ stable |
| NASA via Wikimedia | Pillars / Earthrise / Pale Blue / Hubble Deep / Saturn / Mars / Apollo / etc. | Public Domain | ✓ Wikimedia API resolves filenames |
| Polyhaven (dl.polyhaven.org) | 6 HDR environments | CC0 | ✓ stable |
| Met Museum Open Access (collectionapi.metmuseum.org) | 6 classical artworks | CC0 | ✓ API works well |
| Open-source game gameplay (Wikimedia) | 2 Tux Racer clips | CC-BY-SA | ✓ Wikimedia video files |
| Wikimedia VG references | Pong gameplay GIF, Donkey Kong arcade | PD / CC-BY | ✓ partial (5 of 7 transient failures) |

**Persistent failure:** archive.org direct downloads. Both `archive.org`
and `www.archive.org` paths return HTTP 503 for direct file GETs from
our IP, even with User-Agent + Referer + redirect-follow. This blocks
LibriVox audiobooks (their host is archive.org), Internet Archive's
public-domain comic library, Steamboat Willie, Gertie the Dinosaur,
Cosmos Laundromat, and Caminandes.

## Recommended next sources (HTTP-fetchable)

These can extend `fetch_gaps.py` directly when we want more coverage.
Listed roughly in order of effort-to-add vs payoff.

### High value, low effort

1. **NASA Image and Video Library API** (`images-api.nasa.gov`)
   Public domain. Programmatic search + download. Hosts videos NASA
   actually distributes (Apollo footage, ISS, rover footage, mission
   highlights) on their own CDN — bypasses archive.org. This is the
   right source for NASA video content.

2. **Smithsonian Open Access** (`api.si.edu/openaccess/api/v1.0/`)
   ~4.5 million CC0 items across art, science, history, photography.
   Searchable. Many artworks + historical photographs ideal for the
   "reference library" feel.

3. **Wikipedia / Wikimedia Featured Pictures**
   API endpoint:
   `https://commons.wikimedia.org/w/api.php?action=query&list=categorymembers&cmtitle=Category:Featured_pictures_on_Wikimedia_Commons`
   ~10,000 high-quality CC-BY / PD photographs hand-curated by
   Wikimedia. Great for the photo category.

4. **Standard Ebooks** (`standardebooks.org`)
   High-quality public-domain EPUBs (better-formatted than Project
   Gutenberg). Stable URLs. ~700 books and growing.

5. **Musopen** (`musopen.org`)
   Public-domain classical music recordings (Bach / Beethoven /
   Chopin / etc.). Has bulk-download collections. Layer A audio
   coverage — replaces the LibriVox gap.

6. **Free Music Archive** (`freemusicarchive.org`)
   CC-licensed music across genres. API + bulk downloads. Mix of
   CC-BY, CC-BY-SA, CC0.

7. **NASA Audio Collection** (`science.nasa.gov`)
   Apollo communications, ISS sounds, planetary data sonifications.
   Public domain. Direct download URLs on the NASA domain.

8. **Library of Congress** (`loc.gov/api/`)
   Public domain historical photographs, audio (78s), maps. API
   exists but is rate-limited.

9. **Europeana** (`api.europeana.eu`)
   European cultural heritage — many CC0/PD items. Free API key.

### Medium effort, larger gains

10. **Kaggle datasets** (`kaggle.com/datasets`)
    Mix of CC0 / CC-BY / open-license datasets. Many useful:
    - "Free Spoken Digit Dataset" (audio samples, CC-BY-SA)
    - "Common Voice" (voice recordings, CC0)
    - "Open Images Dataset" (~9M CC-licensed images, subset accessible)
    - "Game Sprite Datasets" (varies — check per-dataset license)
    - "Painter by Numbers" (~100K artwork images, mostly PD)

    Setup: requires kaggle.com account + `~/.kaggle/kaggle.json` API
    token. CLI: `pip install kaggle && kaggle datasets download
    <user>/<dataset>`. Add a per-dataset entry to fetch_gaps.py
    pointing at the Kaggle CDN URL or shell out to `kaggle`.

11. **Hugging Face Hub datasets** (`huggingface.co/datasets`)
    Many CC0 / public-domain image collections. Some video clip
    datasets (action recognition). Has its own `datasets` Python
    library or direct file downloads from the Hub.

12. **Open Game Art** (`opengameart.org`)
    CC0 / CC-BY game art (sprites, models, music, sound effects).
    Has RSS feeds + browseable collections. The sprite stuff overlaps
    with Kenney; the audio + music is good complement.

13. **Pixabay / Pexels / Unsplash**
    All have free-tier APIs returning CC0-equivalent (their own
    permissive license). Good for "stock photo" feel. Less curated
    than Wikimedia featured pictures but trivially large.

14. **YouTube Audio Library exports** (manual)
    YouTube provides a library of CC-BY / public-domain audio for
    creator use. No API — you'd download manually.

### Game-specific sources (for the game-studio demo feel)

15. **Itch.io free game pack archives**
    Some indie devs release CC0 game packs. itch.io has an API for
    listing free assets but each pack is a separate download.

16. **Quaternius** (`quaternius.com`) — CC0 3D models
    Stylized character + environment packs. Direct download per pack.

17. **Sketchfab Public Domain collection**
    `sketchfab.com/feed?features=downloadable&licenses=cc0`. Has
    an API; CC0 models for download.

18. **Blender Open Movies Project** (`studio.blender.org/films/`)
    Beyond what we have (BBB / Sintel), there's Tears of Steel /
    Cosmos / Spring / Coffee Run / Charge / Agent 327 / Sprite Fright
    — but the direct-download URLs are at archive.org for most. The
    Blender Studio also has Sprite Fright pre-production assets and
    Snow assets on their CDN that bypass archive.org.

## Direct .torrent file links (confirmed working)

These are stable URLs that return the .torrent file directly (HTTP 302
redirect to the actual .torrent payload). Drop the URL in your torrent
client; it'll fetch the metadata + start downloading from Internet
Archive's seedbox. Most have hundreds of seeders.

### Blender Foundation open films (CC-BY 3.0/4.0)

| Film | .torrent URL | What it includes |
|---|---|---|
| **Sintel** | `https://archive.org/download/Sintel/Sintel_archive.torrent` | Full film + production files + reference renders |
| **Big Buck Bunny** | `https://archive.org/download/BigBuckBunny_124/BigBuckBunny_124_archive.torrent` | Full film + behind-scenes assets |
| **Tears of Steel** | `https://archive.org/download/tears_of_steel/tears_of_steel_archive.torrent` | Full film + VFX assets |
| **Caminandes 2: Gran Dillama** | `https://archive.org/download/Caminandes2GranDillama/Caminandes2GranDillama_archive.torrent` | Full film |

Pattern for other Internet Archive items: `https://archive.org/download/<identifier>/<identifier>_archive.torrent` — works for ~80% of items (the rest 503 us; LibriVox + Cosmos Laundromat in particular currently block).

To find more: search archive.org for a topic, click any item, scroll to "DOWNLOAD OPTIONS" → there's always a "TORRENT" link if available.

### Specific items worth grabbing for the seed dataset

These all expand the Layer A pool (public-safe content that could ship in site_a):

| Content | .torrent URL (probe first; some may 503) | Approx unpacked size |
|---|---|---|
| Sintel film | `archive.org/download/Sintel/Sintel_archive.torrent` ✓ | ~5 GB |
| BBB film | `archive.org/download/BigBuckBunny_124/BigBuckBunny_124_archive.torrent` ✓ | ~3 GB |
| Tears of Steel | `archive.org/download/tears_of_steel/tears_of_steel_archive.torrent` ✓ | ~5 GB |
| Caminandes 2 | `archive.org/download/Caminandes2GranDillama/Caminandes2GranDillama_archive.torrent` ✓ | ~500 MB |
| Steamboat Willie 1928 (PD) | `archive.org/download/SteamboatWillie_201803/SteamboatWillie_201803_archive.torrent` (probe) | ~70 MB |
| Gertie the Dinosaur 1914 (PD) | `archive.org/download/Gertie_the_Dinosaur/Gertie_the_Dinosaur_archive.torrent` (probe) | ~30 MB |
| LibriVox catalog (per book) | `archive.org/download/<book_librivox>/<book_librivox>_archive.torrent` (most 503 today) | varies |

### How to integrate downloaded torrents

Same as before:
1. Extract to a directory, e.g., `$DATASET_SRC/torrents/<set>/`
2. Add pattern to `SHARED_PACK_PATTERNS` in `sanitize_and_assemble.py` if Layer A
3. Add a TRIM entry if the set is big
4. Re-run sanitize_and_assemble.py + populate_archive.py

## Torrents (other recommended — search to find current URLs)

These are large bulk packages that don't fit a per-file fetch pattern
but would dramatically expand the seed if you grab them via your usual
torrent client and drop them at `$DATASET_SRC/` (or another input path we point sanitize_and_assemble
at). Sizes are approximate.

### Audio / Audiobooks

| Torrent | Source | Approx size | License | Why |
|---|---|---|---|---|
| LibriVox catalog (full or by genre) | archive.org torrents (search "librivox" on the Internet Archive's torrent index) | 100-500 GB if all; pick 10-20 GB of varied genres | Public Domain | **The big LibriVox gap. Pick 30-50 readings; we sample chapter samples in fetch.** |
| Musopen full classical archive | musopen.org or archive.org | ~50 GB | Public Domain | Classical music coverage. |
| Free Music Archive bulk dumps | archive.org or via FMA's bulk packs | ~10-50 GB by genre | CC-BY / CC0 | Modern CC music. |

### Video

| Torrent | Source | Approx size | License | Why |
|---|---|---|---|---|
| Blender Open Movies bundle | Blender Cloud occasionally publishes ISO/torrent bundles | ~5-15 GB | CC-BY | Sintel / Tears of Steel / Cosmos / etc. as a single bundle bypasses archive.org's 503s. |
| Internet Archive's "Sci-Fi Horror Trailers" PD pack | archive.org | ~5 GB | Public Domain | Lots of short trailer-format videos. |
| Prelinger Archives sample | archive.org | varies | Public Domain | Industrial / educational PD films. |
| Steamboat Willie (1928) and other early animation | archive.org | <500 MB | Public Domain (2024+) | Historical animation reference. |

### Comics

| Torrent | Source | Approx size | License | Why |
|---|---|---|---|---|
| Digital Comic Museum bulk | digitalcomicmuseum.com or archive.org mirror | ~20-100 GB | Public Domain | Golden-age PD comics in CBZ format. The site has bulk torrents per publisher. **This is the comic gap solver.** |
| Comic Book Plus archives | comicbookplus.com (occasionally has bulk packs) | varies | Public Domain | Same era, different curation. |

### Books

| Torrent | Source | Approx size | License | Why |
|---|---|---|---|---|
| Project Gutenberg complete DVD (latest year) | gutenberg.org/wiki/Gutenberg:The_CD_and_DVD_Project | ~10 GB | Public Domain | Full PG catalog — way more than the 12 EPUBs we fetch. |
| Standard Ebooks catalog | standardebooks.org has a Bittorrent option | ~5 GB | Public Domain | Better-formatted than PG. |

### 3D / game assets

| Torrent | Source | Approx size | License | Why |
|---|---|---|---|---|
| Open Game Art "best of" packs | community torrents | ~5-20 GB | CC0 / CC-BY | Sprites + audio + 3D models bundled. |
| Kenney's full archive (already mostly used) | kenney.nl publishes occasional bundles | ~10 GB | CC0 | We already have the major packs locally; this would just round out. |
| Sketchfab CC0 / cultural-heritage bulk | not officially distributed, but scrapeable | varies | CC0 / CC-BY | High-quality 3D scans. |

### Images / art

| Torrent | Source | Approx size | License | Why |
|---|---|---|---|---|
| Wikimedia Commons category dumps (PD images by category) | dumps.wikimedia.org distributes; also via torrent | ~50 GB for a featured-pictures sub-dump | CC0 / PD / CC-BY | Bulk image variety. |
| Met Museum Open Access bulk | github.com/metmuseum/openaccess has CSV; full images via their CDN (no official torrent but they encourage bulk) | ~500 GB for all 470K objects; we want maybe 5 GB | CC0 | Way more art than the 6 we fetch. |
| Smithsonian Open Access bulk | si.edu (no torrent; only API) | n/a | CC0 | Large but API-only. |

## How to integrate downloaded torrents

If you grab one of the above:

1. Extract to a known directory, e.g.,
   `$DATASET_SRC/torrents/<set>/`
2. Add path patterns to `sanitize_and_assemble.py`'s SHARED_PACK_PATTERNS
   (Layer A torrents) or `STUDIO_B_PROJECTS` (Layer B), depending on
   whether the content is publicly redistributable.
3. Add a per-pack TRIM entry so a 50 GB torrent doesn't blow the
   ~10 GB cap.
4. Re-run sanitize_and_assemble.py → populate_archive.py.

For audiobooks specifically: drop them under a new top-level dir like
`librivox/` and add `librivox/` to SHARED_PACK_PATTERNS (Layer A). The
script's asset_type mapping will turn them into `audio` automatically
(based on `.mp3` extension via the source CSV's `kind` column —
**caveat:** you'd need to extend the CSV-generator-side `kind` heuristic,
OR have sanitize_and_assemble.py infer asset_type from extension when
`kind` is missing).

## Priority recommendation

If you can grab two torrents this week, I'd take:

1. **Digital Comic Museum's PD pack** (~5-20 GB) → solves the comic gap
   entirely. Drop it at `dcm/` and we can have 20-50 PD CBZs across both
   sites with no IP concerns.

2. **A 10-20 GB LibriVox selection** (specific titles, not the full
   500 GB catalog) → solves the audiobook + audio gap. Curate genres
   that fit the studio-archive vibe: short stories, science fiction,
   classical literature.

Everything else can be incremental — the demo seed is already at
1 GB / 963 files on site_a with good format diversity.
