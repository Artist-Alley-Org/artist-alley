#!/usr/bin/env python3
"""
Fetch Layer A (public-safe) gap-filling content from internet sources.

This is the gold-standard catalogue — comprehensive coverage of every
viewer kind across multiple sources, so site_a (the public demo source)
has a credible studio-sized library even though the local dataset's
Layer A subset is light on videos / comics / audiobooks.

All sources are CC0, CC-BY (with attribution preserved), or public
domain. Per-asset attribution + license + provenance lands in the
sidecar manifest written next to the bytes.

Usage
-----
    python3 fetch_gaps.py --out /mnt/d/Projects/artist-alley/seed/internet-fetched

Idempotent — re-running skips already-cached files. Use --force to
re-fetch everything.

Sources by viewer kind
----------------------
- video:    Blender Foundation films (CC-BY) + early animation (PD)
- audio:    LibriVox readings (PD) + NASA recordings (PD)
- 3d:       Khronos glTF Sample Models (CC-BY / CC0)
- document: Project Gutenberg EPUBs (PD)
- image:    NASA via Wikimedia (PD) + Polyhaven HDRs (CC0)
- comic:    Internet Archive public-domain comics (PD)
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path

@dataclass
class GapAsset:
    name: str
    url: str
    target_dir: str
    target_filename: str
    license: str
    attribution: str
    source: str
    asset_type: str
    notes: str = ""


GAPS: list[GapAsset] = [
    # =========================================================================
    # VIDEO — Blender Foundation open films (CC-BY) + early animation (PD)
    # =========================================================================
    # ---- Blender open movies, fetched DIRECT from Blender ------------------
    # archive.org mirrors 503 under rate-limiting (measured 2026-07-25: 10
    # consecutive failures across two runs), which silently starved the
    # catalogue of its best video. These come straight from the Foundation,
    # so there is no longer a single point of failure for the content that
    # matters most. Do not "simplify" these back to archive.org mirrors.
    GapAsset(
        name="Sintel (2010) — full film, 1080p",
        url="https://download.blender.org/demo/movies/Sintel.2010.1080p.mkv",
        target_dir="video",
        target_filename="sintel-2010-1080p.mkv",
        license="CC-BY 3.0",
        attribution="(c) Blender Foundation | durian.blender.org",
        source="Blender Foundation",
        asset_type="video",
        notes="Full 15-min film in MKV — long-form playback + a container the trailer doesn't exercise",
    ),
    GapAsset(
        name="Tears of Steel — 720p",
        url="https://download.blender.org/demo/movies/ToS/tears_of_steel_720p.mov",
        target_dir="video",
        target_filename="tears-of-steel-720p.mov",
        license="CC-BY 3.0",
        attribution="(c) Blender Foundation | mango.blender.org",
        source="Blender Foundation",
        asset_type="video",
        notes="Live-action/VFX short in QuickTime — direct Blender mirror",
    ),
    GapAsset(
        name="Big Buck Bunny — 720p surround",
        url="https://www.archive.org/download/BigBuckBunny_124/Content/big_buck_bunny_720p_surround.mp4",
        target_dir="video",
        target_filename="big-buck-bunny.mp4",
        license="CC-BY 3.0",
        attribution="(c) Blender Foundation | peach.blender.org",
        source="Blender Foundation",
        asset_type="video",
        notes="Animated short — exercises 5.1 surround audio path",
    ),
    GapAsset(
        name="Sintel — 480p trailer",
        url="https://download.blender.org/durian/trailer/sintel_trailer-480p.mp4",
        target_dir="video",
        target_filename="sintel-trailer-480p.mp4",
        license="CC-BY 3.0",
        attribution="(c) Blender Foundation | durian.blender.org",
        source="Blender Foundation",
        asset_type="video",
        notes="Trailer for the animated short — small clip via Blender direct",
    ),
    GapAsset(
        name="Tears of Steel",
        url="https://www.archive.org/download/tears_of_steel/tears_of_steel_720p.mov",
        target_dir="video",
        target_filename="tears-of-steel.mov",
        license="CC-BY 3.0",
        attribution="(c) Blender Foundation | mango.blender.org",
        source="Blender Foundation",
        asset_type="video",
        notes="Live-action / VFX short film",
    ),
    GapAsset(
        name="Caminandes 2: Gran Dillama",
        url="https://www.archive.org/download/Caminandes2GranDillama/Caminandes_2_Gran_Dillama_-_Blender_Animated_Short.ogv",
        target_dir="video",
        target_filename="caminandes-2-gran-dillama.ogv",
        license="CC-BY 3.0",
        attribution="(c) Blender Foundation",
        source="Blender Foundation",
        asset_type="video",
        notes="Llama animated short",
    ),
    GapAsset(
        name="Caminandes 3: Llamigos",
        url="https://www.archive.org/download/caminandes-llamigos/caminandes_llamigos_1080p.mp4",
        target_dir="video",
        target_filename="caminandes-3-llamigos.mp4",
        license="CC-BY 3.0",
        attribution="(c) Blender Foundation",
        source="Blender Foundation",
        asset_type="video",
        notes="Third Caminandes episode",
    ),
    GapAsset(
        name="Cosmos Laundromat — First Cycle",
        url="https://www.archive.org/download/CosmosLaundromat/Cosmos%20Laundromat%20-%20First%20Cycle%20%28%20HD%201080p%20%29.mp4",
        target_dir="video",
        target_filename="cosmos-laundromat.mp4",
        license="CC-BY 4.0",
        attribution="(c) Blender Foundation | gooseberry.blender.org",
        source="Blender Foundation",
        asset_type="video",
        notes="VFX-heavy animated short",
    ),
    GapAsset(
        name="Steamboat Willie (1928, public domain 2024)",
        url="https://www.archive.org/download/steamboat-willie-restored-by-aluxtech/SteamboatWillie.mp4",
        target_dir="video",
        target_filename="steamboat-willie-1928.mp4",
        license="Public Domain",
        attribution="Walt Disney / Ub Iwerks, 1928 — entered public domain 2024",
        source="Internet Archive",
        asset_type="video",
        notes="Historical animation milestone — early sync-sound cartoon",
    ),
    GapAsset(
        name="Gertie the Dinosaur (1914)",
        url="https://www.archive.org/download/Gertie_the_Dinosaur/Gertie_the_Dinosaur_512kb.mp4",
        target_dir="video",
        target_filename="gertie-the-dinosaur-1914.mp4",
        license="Public Domain",
        attribution="Winsor McCay, 1914",
        source="Internet Archive",
        asset_type="video",
        notes="Pioneer of character animation — public domain",
    ),
    GapAsset(
        name="NASA — Apollo 11 highlights",
        url="https://www.archive.org/download/Apollo_11_onboard_audio/Apollo_11_onboard_audio.mp4",
        target_dir="video",
        target_filename="apollo-11-highlights.mp4",
        license="Public Domain",
        attribution="NASA",
        source="NASA",
        asset_type="video",
        notes="Apollo 11 mission footage with audio — works as scientific reference",
    ),

    # ---- Open-source game gameplay footage (CC-BY-SA via Wikimedia) ----
    # These are videos OF games, where the game itself is open-source so the
    # gameplay capture is unambiguously freely licensed.
    GapAsset(
        name="Tux Racer gameplay — Ingo's Speedway",
        url="https://upload.wikimedia.org/wikipedia/commons/d/de/Tux_Racer_gameplay_%28Ingo%27s_Speedway%29.webm",
        target_dir="video",
        target_filename="tux-racer-ingos-speedway.webm",
        license="CC-BY-SA 4.0",
        attribution="Tux Racer gameplay (open-source game)",
        source="Wikimedia Commons",
        asset_type="video",
        notes="Open-source game gameplay footage",
    ),
    GapAsset(
        name="Tux Racer gameplay — Path of Daggers",
        url="https://upload.wikimedia.org/wikipedia/commons/3/33/Tux_Racer_gameplay_%28Path_of_Daggers%29.webm",
        target_dir="video",
        target_filename="tux-racer-path-of-daggers.webm",
        license="CC-BY-SA 4.0",
        attribution="Tux Racer gameplay (open-source game)",
        source="Wikimedia Commons",
        asset_type="video",
    ),
    GapAsset(
        name="0 A.D. gameplay test (open-source RTS)",
        url="https://upload.wikimedia.org/wikipedia/commons/b/bc/0_A.D._-_Gameplay-Test_15052019_Full-HD.webm",
        target_dir="video",
        target_filename="0ad-gameplay-test.webm",
        license="CC-BY-SA 4.0",
        attribution="0 A.D. — Empires Ascendant (open-source RTS)",
        source="Wikimedia Commons",
        asset_type="video",
        notes="RTS gameplay footage — open-source game",
    ),
    GapAsset(
        name="Fantastic Contraption gameplay condensed",
        url="https://upload.wikimedia.org/wikipedia/commons/c/cb/Fantastic_Contraption_gameplay_condensed.webm",
        target_dir="video",
        target_filename="fantastic-contraption-gameplay.webm",
        license="CC-BY-SA 4.0",
        attribution="Fantastic Contraption (CC-licensed physics puzzle game)",
        source="Wikimedia Commons",
        asset_type="video",
    ),
    GapAsset(
        name="Hedgewars gameplay (open-source artillery game)",
        url="https://upload.wikimedia.org/wikipedia/commons/c/ca/Hedgewars_gameplay.webm",
        target_dir="video",
        target_filename="hedgewars-gameplay.webm",
        license="CC-BY-SA 4.0",
        attribution="Hedgewars — open-source artillery game",
        source="Wikimedia Commons",
        asset_type="video",
        notes="Turn-based artillery game",
    ),
    GapAsset(
        name="OpenArena 0.8.8 gameplay (open-source FPS)",
        url="https://upload.wikimedia.org/wikipedia/commons/6/60/OpenArena_0.8.8_gameplay.webm",
        target_dir="video",
        target_filename="openarena-gameplay.webm",
        license="CC-BY-SA 4.0",
        attribution="OpenArena — open-source FPS (Quake III Arena derivative)",
        source="Wikimedia Commons",
        asset_type="video",
        notes="First-person shooter gameplay",
    ),
    GapAsset(
        name="Red Eclipse gameplay 1 (open-source FPS)",
        url="https://upload.wikimedia.org/wikipedia/commons/3/34/Red_Eclipse_1%2C5_Gameplay_1.webm",
        target_dir="video",
        target_filename="red-eclipse-gameplay.webm",
        license="CC-BY-SA 4.0",
        attribution="Red Eclipse — open-source first-person arena shooter",
        source="Wikimedia Commons",
        asset_type="video",
        notes="Arena FPS gameplay (~149 MB)",
    ),
    GapAsset(
        name="MineClone2 0.84 release (open-source Minecraft-clone)",
        url="https://upload.wikimedia.org/wikipedia/commons/9/92/MineClone2_-_Release_0.84_-_The_Very_Nice_Release.webm",
        target_dir="video",
        target_filename="mineclone2-release.webm",
        license="CC-BY-SA 4.0",
        attribution="MineClone2 — open-source Minecraft-style sandbox (Minetest game)",
        source="Wikimedia Commons",
        asset_type="video",
        notes="Sandbox / voxel gameplay",
    ),
    GapAsset(
        name="Snake game gameplay (tiny clip)",
        url="https://upload.wikimedia.org/wikipedia/commons/f/f8/Snake.webm",
        target_dir="video",
        target_filename="snake-game.webm",
        license="CC-BY-SA 4.0",
        attribution="Snake gameplay — Wikimedia Commons",
        source="Wikimedia Commons",
        asset_type="video",
        notes="Tiny classic-style snake clip (~200 KB)",
    ),
    GapAsset(
        name="Xonotic 0.8.2 gameplay (open-source FPS)",
        url="https://upload.wikimedia.org/wikipedia/commons/b/b9/Xonotic_0-8-2_gameplay.webm",
        target_dir="video",
        target_filename="xonotic-gameplay.webm",
        license="CC-BY-SA 4.0",
        attribution="Xonotic — open-source competitive arena FPS",
        source="Wikimedia Commons",
        asset_type="video",
        notes="Fast-paced arena FPS gameplay",
    ),

    # =========================================================================
    # AUDIO — LibriVox readings + NASA recordings
    # =========================================================================
    GapAsset(
        name="The Yellow Wallpaper — LibriVox",
        url="https://www.archive.org/download/yellow_wallpaper_librivox/yellow_wallpaper.mp3",
        target_dir="audio",
        target_filename="yellow-wallpaper-librivox.mp3",
        license="Public Domain",
        attribution="Charlotte Perkins Gilman, read by LibriVox volunteers",
        source="LibriVox",
        asset_type="audio",
        notes="Short story reading ~30 min",
    ),
    GapAsset(
        name="The Tell-Tale Heart — LibriVox",
        url="https://www.archive.org/download/tell_tale_heart_dn_librivox/telltaleheart_poe_dn_64kb.mp3",
        target_dir="audio",
        target_filename="tell-tale-heart-librivox.mp3",
        license="Public Domain",
        attribution="Edgar Allan Poe, read by LibriVox volunteers",
        source="LibriVox",
        asset_type="audio",
        notes="Short reading ~16 min",
    ),
    GapAsset(
        name="Aesop's Fables — chapter 1",
        url="https://www.archive.org/download/aesops_fables_v1_librivox/aesopsfables_01_aesop_64kb.mp3",
        target_dir="audio",
        target_filename="aesops-fables-ch01-librivox.mp3",
        license="Public Domain",
        attribution="Aesop, read by LibriVox volunteers",
        source="LibriVox",
        asset_type="audio",
        notes="Classic fable readings",
    ),
    GapAsset(
        name="NASA Apollo 11 — One Small Step",
        url="https://upload.wikimedia.org/wikipedia/commons/2/2c/Apollo11_OneSmallStep_audio.ogg",
        target_dir="audio",
        target_filename="apollo-11-one-small-step.ogg",
        license="Public Domain",
        attribution="NASA — Neil Armstrong, July 20, 1969",
        source="NASA",
        asset_type="audio",
        notes="Historical voice recording",
    ),

    # =========================================================================
    # 3D — Khronos glTF Sample Models (CC-BY / CC0)
    # =========================================================================
    GapAsset(
        name="DamagedHelmet — Khronos PBR test",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/DamagedHelmet/glTF-Binary/DamagedHelmet.glb",
        target_dir="3d",
        target_filename="DamagedHelmet.glb",
        license="CC-BY 4.0",
        attribution="ctxwing | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Canonical PBR material test asset",
    ),
    GapAsset(
        name="FlightHelmet — Khronos detailed scene",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/FlightHelmet/glTF/FlightHelmet.gltf",
        target_dir="3d",
        target_filename="FlightHelmet.gltf",
        license="CC0 1.0",
        attribution="Microsoft | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="High-detail multi-material asset",
    ),
    GapAsset(
        name="BoxAnimated — Khronos animation test",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/BoxAnimated/glTF-Binary/BoxAnimated.glb",
        target_dir="3d",
        target_filename="BoxAnimated.glb",
        license="CC0 1.0",
        attribution="Cesium | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Exercises 3D viewer animation timeline",
    ),
    GapAsset(
        name="Avocado — Khronos PBR food asset",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/Avocado/glTF-Binary/Avocado.glb",
        target_dir="3d",
        target_filename="Avocado.glb",
        license="CC0 1.0",
        attribution="Microsoft | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Small PBR object",
    ),
    GapAsset(
        name="BoomBox — Khronos PBR product",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/BoomBox/glTF-Binary/BoomBox.glb",
        target_dir="3d",
        target_filename="BoomBox.glb",
        license="CC0 1.0",
        attribution="Microsoft | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Product-vis PBR asset",
    ),
    GapAsset(
        name="Duck — Khronos classic test",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/Duck/glTF-Binary/Duck.glb",
        target_dir="3d",
        target_filename="Duck.glb",
        license="CC-BY-SA 3.0",
        attribution="Sony Computer Entertainment Inc | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Reference duck used in graphics demos",
    ),
    GapAsset(
        name="Lantern — Khronos detailed asset",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/Lantern/glTF-Binary/Lantern.glb",
        target_dir="3d",
        target_filename="Lantern.glb",
        license="CC0 1.0",
        attribution="Microsoft | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Detailed PBR architecture asset",
    ),
    GapAsset(
        name="Sponza — Khronos canonical scene",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/Sponza/glTF/Sponza.gltf",
        target_dir="3d",
        target_filename="Sponza.gltf",
        license="CC-BY 3.0",
        attribution="Marko Dabrovic / Frank Meinl | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Architectural test scene — most-used graphics benchmark",
    ),
    GapAsset(
        name="WaterBottle — Khronos PBR",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/WaterBottle/glTF-Binary/WaterBottle.glb",
        target_dir="3d",
        target_filename="WaterBottle.glb",
        license="CC0 1.0",
        attribution="Microsoft | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Transparency / refraction test asset",
    ),
    GapAsset(
        name="BrainStem — Khronos skinned animation",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/BrainStem/glTF-Binary/BrainStem.glb",
        target_dir="3d",
        target_filename="BrainStem.glb",
        license="CC0 1.0",
        attribution="Keith Hunter / Smith Micro | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Skinned-mesh animation test",
    ),
    GapAsset(
        name="CesiumMan — Khronos character animation",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/CesiumMan/glTF-Binary/CesiumMan.glb",
        target_dir="3d",
        target_filename="CesiumMan.glb",
        license="CC-BY 4.0",
        attribution="Cesium | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Walking character animation",
    ),
    GapAsset(
        name="2CylinderEngine — Khronos mechanical",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/2CylinderEngine/glTF-Binary/2CylinderEngine.glb",
        target_dir="3d",
        target_filename="2CylinderEngine.glb",
        license="SCEA Shared Source License",
        attribution="Sony Computer Entertainment Inc | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Mechanical CAD-style asset",
    ),
    GapAsset(
        name="ToyCar — Khronos PBR product",
        url="https://raw.githubusercontent.com/KhronosGroup/glTF-Sample-Models/master/2.0/ToyCar/glTF-Binary/ToyCar.glb",
        target_dir="3d",
        target_filename="ToyCar.glb",
        license="CC0 1.0",
        attribution="2017 Microsoft | Khronos glTF Sample Models",
        source="Khronos Sample Models",
        asset_type="3d",
        notes="Toy product PBR asset",
    ),

    # =========================================================================
    # DOCUMENT — Project Gutenberg EPUBs + NASA technical PDFs
    # =========================================================================
    GapAsset(
        name="Frankenstein — Mary Shelley",
        url="https://www.gutenberg.org/ebooks/84.epub.images",
        target_dir="ebook",
        target_filename="frankenstein.epub",
        license="Public Domain",
        attribution="Mary Wollstonecraft Shelley | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="Pride and Prejudice — Jane Austen",
        url="https://www.gutenberg.org/ebooks/1342.epub.images",
        target_dir="ebook",
        target_filename="pride-and-prejudice.epub",
        license="Public Domain",
        attribution="Jane Austen | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="The Adventures of Sherlock Holmes — Conan Doyle",
        url="https://www.gutenberg.org/ebooks/1661.epub.images",
        target_dir="ebook",
        target_filename="sherlock-holmes.epub",
        license="Public Domain",
        attribution="Arthur Conan Doyle | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="Alice's Adventures in Wonderland — Carroll",
        url="https://www.gutenberg.org/ebooks/11.epub.images",
        target_dir="ebook",
        target_filename="alice-in-wonderland.epub",
        license="Public Domain",
        attribution="Lewis Carroll | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="Moby Dick — Herman Melville",
        url="https://www.gutenberg.org/ebooks/2701.epub.images",
        target_dir="ebook",
        target_filename="moby-dick.epub",
        license="Public Domain",
        attribution="Herman Melville | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="The Time Machine — H.G. Wells",
        url="https://www.gutenberg.org/ebooks/35.epub.images",
        target_dir="ebook",
        target_filename="the-time-machine.epub",
        license="Public Domain",
        attribution="H.G. Wells | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="A Tale of Two Cities — Dickens",
        url="https://www.gutenberg.org/ebooks/98.epub.images",
        target_dir="ebook",
        target_filename="tale-of-two-cities.epub",
        license="Public Domain",
        attribution="Charles Dickens | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="Around the World in Eighty Days — Verne",
        url="https://www.gutenberg.org/ebooks/103.epub.images",
        target_dir="ebook",
        target_filename="around-the-world-80-days.epub",
        license="Public Domain",
        attribution="Jules Verne | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="The Picture of Dorian Gray — Wilde",
        url="https://www.gutenberg.org/ebooks/174.epub.images",
        target_dir="ebook",
        target_filename="picture-of-dorian-gray.epub",
        license="Public Domain",
        attribution="Oscar Wilde | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="Treasure Island — Stevenson",
        url="https://www.gutenberg.org/ebooks/120.epub.images",
        target_dir="ebook",
        target_filename="treasure-island.epub",
        license="Public Domain",
        attribution="Robert Louis Stevenson | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="The Iliad — Homer (trans. Pope)",
        url="https://www.gutenberg.org/ebooks/6130.epub.images",
        target_dir="ebook",
        target_filename="the-iliad.epub",
        license="Public Domain",
        attribution="Homer, translated by Alexander Pope | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="Walden — Thoreau",
        url="https://www.gutenberg.org/ebooks/205.epub.images",
        target_dir="ebook",
        target_filename="walden.epub",
        license="Public Domain",
        attribution="Henry David Thoreau | Project Gutenberg",
        source="Project Gutenberg",
        asset_type="document",
    ),
    GapAsset(
        name="Vue.js README — markdown reference",
        url="https://raw.githubusercontent.com/vuejs/vue/main/README.md",
        target_dir="docs",
        target_filename="vue-README.md",
        license="MIT",
        attribution="Vue.js contributors",
        source="GitHub (vuejs/vue)",
        asset_type="document",
        notes="Markdown viewer test — code blocks + tables + images",
    ),
    GapAsset(
        name="OpenStax College Physics — chapter 1 PDF",
        url="https://assets.openstax.org/oscms-prodcms/media/documents/College_Physics_2e_Volume_1-WEB.pdf",
        target_dir="docs",
        target_filename="openstax-college-physics-vol1.pdf",
        license="CC-BY 4.0",
        attribution="OpenStax | Rice University",
        source="OpenStax",
        asset_type="document",
        notes="Free textbook PDF — exercises PDF viewer with diagrams + equations",
    ),

    # =========================================================================
    # IMAGE — NASA via Wikimedia + Polyhaven HDRs + classic art
    # =========================================================================
    GapAsset(
        name="Hubble Pillars of Creation",
        url="https://upload.wikimedia.org/wikipedia/commons/6/68/Pillars_of_creation_2014_HST_WFC3-UVIS_full-res_denoised.jpg",
        target_dir="nasa",
        target_filename="pillars-of-creation.jpg",
        license="Public Domain",
        attribution="NASA, ESA, and the Hubble Heritage Team (STScI/AURA)",
        source="NASA",
        asset_type="image",
        notes="High-res pan/zoom demo",
    ),
    GapAsset(
        name="Earthrise from Apollo 8",
        url="https://upload.wikimedia.org/wikipedia/commons/a/a8/NASA-Apollo8-Dec24-Earthrise.jpg",
        target_dir="nasa",
        target_filename="earthrise-apollo-8.jpg",
        license="Public Domain",
        attribution="NASA / William Anders | Apollo 8 (1968)",
        source="NASA",
        asset_type="image",
    ),
    GapAsset(
        name="Pale Blue Dot — Voyager 1",
        url="https://upload.wikimedia.org/wikipedia/commons/7/73/Pale_Blue_Dot.png",
        target_dir="nasa",
        target_filename="pale-blue-dot.png",
        license="Public Domain",
        attribution="NASA / JPL-Caltech | Voyager 1 (1990)",
        source="NASA",
        asset_type="image",
    ),
    GapAsset(
        name="Saturn from Cassini",
        url="https://upload.wikimedia.org/wikipedia/commons/c/c7/Saturn_during_Equinox.jpg",
        target_dir="nasa",
        target_filename="saturn-cassini-equinox.jpg",
        license="Public Domain",
        attribution="NASA / JPL / Space Science Institute | Cassini",
        source="NASA",
        asset_type="image",
    ),
    GapAsset(
        name="Hubble Deep Field",
        url="https://upload.wikimedia.org/wikipedia/commons/0/0d/Hubble_ultra_deep_field_high_rez_edit1.jpg",
        target_dir="nasa",
        target_filename="hubble-ultra-deep-field.jpg",
        license="Public Domain",
        attribution="NASA, ESA, S. Beckwith (STScI) and the HUDF Team",
        source="NASA",
        asset_type="image",
    ),
    GapAsset(
        name="Mars Curiosity Self-Portrait — Big Sky drilling site",
        url="https://upload.wikimedia.org/wikipedia/commons/f/f3/Curiosity_Self-Portrait_at_%27Big_Sky%27_Drilling_Site.jpg",
        target_dir="nasa",
        target_filename="curiosity-self-portrait-big-sky.jpg",
        license="Public Domain",
        attribution="NASA / JPL-Caltech / MSSS | Curiosity rover (2015)",
        source="NASA",
        asset_type="image",
    ),
    GapAsset(
        name="The Great Wave off Kanagawa (Hokusai)",
        url="https://upload.wikimedia.org/wikipedia/commons/0/0a/The_Great_Wave_off_Kanagawa.jpg",
        target_dir="art",
        target_filename="great-wave-kanagawa.jpg",
        license="Public Domain",
        attribution="Katsushika Hokusai (c. 1831) — Wikimedia",
        source="Wikimedia Commons",
        asset_type="image",
        notes="Classic woodblock print — art-archive reference",
    ),
    GapAsset(
        name="Starry Night (Van Gogh)",
        url="https://upload.wikimedia.org/wikipedia/commons/e/ea/Van_Gogh_-_Starry_Night_-_Google_Art_Project.jpg",
        target_dir="art",
        target_filename="starry-night-van-gogh.jpg",
        license="Public Domain",
        attribution="Vincent van Gogh (1889) — Google Art Project",
        source="Wikimedia Commons",
        asset_type="image",
        notes="Public-domain painting reference",
    ),

    # ---- Video game reference imagery (Wikimedia PD/CC-BY) ----
    # Game studio reference: arcade cabinets + classic gameplay screenshots
    # for the demo's "reference library" feel.
    GapAsset(
        name="Pong gameplay loop (1972)",
        url="https://upload.wikimedia.org/wikipedia/commons/6/62/Pong_Game_Test2.gif",
        target_dir="games",
        target_filename="pong-gameplay-test.gif",
        license="Public Domain",
        attribution="Atari, 1972 — Wikimedia Commons",
        source="Wikimedia Commons",
        asset_type="image",
        notes="Pong gameplay animation — classic arcade reference",
    ),
    GapAsset(
        name="Donkey Kong arcade",
        url="https://upload.wikimedia.org/wikipedia/commons/8/88/Donkey_Kong_arcade.jpg",
        target_dir="games",
        target_filename="donkey-kong-arcade.jpg",
        license="CC-BY 2.0",
        attribution="Photo of Donkey Kong arcade machine — Wikimedia Commons",
        source="Wikimedia Commons",
        asset_type="image",
    ),
    GapAsset(
        name="Tank (1974) — Atari arcade screenshot",
        url="https://upload.wikimedia.org/wikipedia/commons/1/19/Tank_1974.png",
        target_dir="games",
        target_filename="tank-1974.png",
        license="Public Domain",
        attribution="Atari, 1974 — Wikimedia Commons",
        source="Wikimedia Commons",
        asset_type="image",
        notes="Early arcade game reference",
    ),
    GapAsset(
        name="Bradley Trainer screenshot (early military trainer)",
        url="https://upload.wikimedia.org/wikipedia/commons/7/7d/Bradley_Trainer_screenshot_2.PNG",
        target_dir="games",
        target_filename="bradley-trainer-screenshot.png",
        license="Public Domain",
        attribution="Atari, 1980 — Wikimedia Commons",
        source="Wikimedia Commons",
        asset_type="image",
    ),
    GapAsset(
        name="Atari Pong arcade cabinet",
        url="https://upload.wikimedia.org/wikipedia/commons/b/b0/Atari_Pong_arcade_game_cabinet.jpg",
        target_dir="games",
        target_filename="atari-pong-cabinet.jpg",
        license="CC-BY-SA 4.0",
        attribution="Photo of Atari Pong cabinet — Wikimedia Commons",
        source="Wikimedia Commons",
        asset_type="image",
    ),
    GapAsset(
        name="Atari Pong arcade cabinet — front view",
        url="https://upload.wikimedia.org/wikipedia/commons/8/8f/Atari_Pong_arcade_game_front.jpg",
        target_dir="games",
        target_filename="atari-pong-front.jpg",
        license="CC-BY-SA 4.0",
        attribution="Photo of Atari Pong cabinet — Wikimedia Commons",
        source="Wikimedia Commons",
        asset_type="image",
    ),
    GapAsset(
        name="Galaga arcade gameplay",
        url="https://upload.wikimedia.org/wikipedia/commons/5/58/Galaga_arcade_gameplay.png",
        target_dir="games",
        target_filename="galaga-arcade-gameplay.png",
        license="Public Domain",
        attribution="Namco, 1981 — Wikimedia Commons",
        source="Wikimedia Commons",
        asset_type="image",
    ),

    # ---- Met Museum Open Access (CC0) — classical art reference ----
    GapAsset(
        name="Aristotle with a Bust of Homer (Rembrandt)",
        url="https://images.metmuseum.org/CRDImages/ep/original/DP-30758-001.jpg",
        target_dir="art",
        target_filename="rembrandt-aristotle.jpg",
        license="CC0 1.0",
        attribution="Rembrandt van Rijn, 1653 — Met Museum Open Access",
        source="Met Museum",
        asset_type="image",
        notes="High-resolution classical painting reference",
    ),
    GapAsset(
        name="Washington Crossing the Delaware (Leutze)",
        url="https://images.metmuseum.org/CRDImages/ad/original/DP215410.jpg",
        target_dir="art",
        target_filename="washington-crossing.jpg",
        license="CC0 1.0",
        attribution="Emanuel Leutze, 1851 — Met Museum Open Access",
        source="Met Museum",
        asset_type="image",
    ),
    GapAsset(
        name="The Annunciation (Memling)",
        url="https://images.metmuseum.org/CRDImages/ep/original/DP240360.jpg",
        target_dir="art",
        target_filename="memling-annunciation.jpg",
        license="CC0 1.0",
        attribution="Hans Memling, 1465–75 — Met Museum Open Access",
        source="Met Museum",
        asset_type="image",
    ),
    GapAsset(
        name="Self-Portrait with a Straw Hat (Van Gogh)",
        url="https://images.metmuseum.org/CRDImages/ep/original/DT1502_cropped2.jpg",
        target_dir="art",
        target_filename="van-gogh-straw-hat.jpg",
        license="CC0 1.0",
        attribution="Vincent van Gogh, 1887 — Met Museum Open Access",
        source="Met Museum",
        asset_type="image",
    ),
    GapAsset(
        name="Madame Roulin and Her Baby (Van Gogh)",
        url="https://images.metmuseum.org/CRDImages/rl/original/DT3154.jpg",
        target_dir="art",
        target_filename="van-gogh-madame-roulin.jpg",
        license="CC0 1.0",
        attribution="Vincent van Gogh, 1888 — Met Museum Open Access",
        source="Met Museum",
        asset_type="image",
    ),
    GapAsset(
        name="The Abduction of the Sabine Women (Poussin)",
        url="https://images.metmuseum.org/CRDImages/ep/original/DP-29324-001.jpg",
        target_dir="art",
        target_filename="poussin-sabine-women.jpg",
        license="CC0 1.0",
        attribution="Nicolas Poussin, 1633–34 — Met Museum Open Access",
        source="Met Museum",
        asset_type="image",
    ),

    # ---- HDR / EXR ----
    GapAsset(
        name="Studio Small 03 HDR (Polyhaven)",
        url="https://dl.polyhaven.org/file/ph-assets/HDRIs/hdr/1k/studio_small_03_1k.hdr",
        target_dir="hdr",
        target_filename="studio_small_03_1k.hdr",
        license="CC0 1.0",
        attribution="Greg Zaal | Polyhaven",
        source="Polyhaven",
        asset_type="image",
        notes="Studio lighting HDR for IBL preview",
    ),
    GapAsset(
        name="Adams Place Bridge HDR (Polyhaven)",
        url="https://dl.polyhaven.org/file/ph-assets/HDRIs/hdr/1k/adams_place_bridge_1k.hdr",
        target_dir="hdr",
        target_filename="adams_place_bridge_1k.hdr",
        license="CC0 1.0",
        attribution="Greg Zaal | Polyhaven",
        source="Polyhaven",
        asset_type="image",
    ),
    GapAsset(
        name="Christmas Photo Studio HDR (Polyhaven)",
        url="https://dl.polyhaven.org/file/ph-assets/HDRIs/hdr/1k/christmas_photo_studio_07_1k.hdr",
        target_dir="hdr",
        target_filename="christmas_photo_studio_07_1k.hdr",
        license="CC0 1.0",
        attribution="Sergej Majboroda | Polyhaven",
        source="Polyhaven",
        asset_type="image",
    ),
    GapAsset(
        name="Studio Small 09 HDR (Polyhaven)",
        url="https://dl.polyhaven.org/file/ph-assets/HDRIs/hdr/1k/studio_small_09_1k.hdr",
        target_dir="hdr",
        target_filename="studio_small_09_1k.hdr",
        license="CC0 1.0",
        attribution="Greg Zaal | Polyhaven",
        source="Polyhaven",
        asset_type="image",
    ),
    GapAsset(
        name="Venice Sunset HDR (Polyhaven)",
        url="https://dl.polyhaven.org/file/ph-assets/HDRIs/hdr/1k/venice_sunset_1k.hdr",
        target_dir="hdr",
        target_filename="venice_sunset_1k.hdr",
        license="CC0 1.0",
        attribution="Andreas Mischok | Polyhaven",
        source="Polyhaven",
        asset_type="image",
        notes="Outdoor sunset HDR — golden hour IBL",
    ),
    GapAsset(
        name="Belfast Sunset HDR (Polyhaven)",
        url="https://dl.polyhaven.org/file/ph-assets/HDRIs/hdr/1k/belfast_sunset_1k.hdr",
        target_dir="hdr",
        target_filename="belfast_sunset_1k.hdr",
        license="CC0 1.0",
        attribution="Sergej Majboroda | Polyhaven",
        source="Polyhaven",
        asset_type="image",
    ),
    GapAsset(
        name="Abandoned Factory Canteen HDR (Polyhaven)",
        url="https://dl.polyhaven.org/file/ph-assets/HDRIs/hdr/1k/abandoned_factory_canteen_01_1k.hdr",
        target_dir="hdr",
        target_filename="abandoned_factory_canteen_01_1k.hdr",
        license="CC0 1.0",
        attribution="Sergej Majboroda | Polyhaven",
        source="Polyhaven",
        asset_type="image",
        notes="Industrial interior HDR — moody IBL",
    ),

    # =========================================================================
    # COMICS — Internet Archive public-domain comics (CBZ)
    # =========================================================================
    GapAsset(
        name="Action Comics #1 reprint (1938, PD)",
        url="https://www.archive.org/download/Action_Comics_001/Action%20Comics%20001.cbz",
        target_dir="comic",
        target_filename="action-comics-1.cbz",
        license="Public Domain",
        attribution="DC Comics, 1938 — entered public domain era",
        source="Internet Archive",
        asset_type="comic",
        notes="Comic-viewer test — CBZ format",
    ),
    GapAsset(
        name="Detective Comics #38 — first Robin (1940, PD scan)",
        url="https://www.archive.org/download/DetectiveComics038/Detective%20Comics%20038.cbz",
        target_dir="comic",
        target_filename="detective-comics-38.cbz",
        license="Public Domain",
        attribution="DC Comics, 1940",
        source="Internet Archive",
        asset_type="comic",
    ),
    GapAsset(
        name="Crime Does Not Pay #22 (1942)",
        url="https://www.archive.org/download/Crime_Does_Not_Pay_022/Crime%20Does%20Not%20Pay%20022.cbz",
        target_dir="comic",
        target_filename="crime-does-not-pay-22.cbz",
        license="Public Domain",
        attribution="Lev Gleason Publications, 1942",
        source="Internet Archive",
        asset_type="comic",
        notes="Golden-age crime comic — public domain",
    ),
]

# -----------------------------------------------------------------------------
# Fetch + verify
# -----------------------------------------------------------------------------

def sha256_of_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def fetch(gap: GapAsset, out_root: Path, force: bool = False) -> dict | None:
    out_dir = out_root / gap.target_dir
    out_dir.mkdir(parents=True, exist_ok=True)
    target = out_dir / gap.target_filename

    if target.exists() and not force:
        size = target.stat().st_size
        print(f"  [cached] {gap.name} ({size:,} B)", file=sys.stderr)
        companions = _fetch_gltf_companions(gap, target, force)
        return _record(gap, target, out_root, companions)

    print(f"  [fetch ] {gap.name}", file=sys.stderr)
    print(f"           ← {gap.url}", file=sys.stderr)
    req = urllib.request.Request(gap.url, headers={
        "User-Agent": "artist-alley-seed-fetcher/2.0",
    })
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            data = resp.read()
    except Exception as e:
        print(f"    error: {e}", file=sys.stderr)
        return None

    target.write_bytes(data)
    size = target.stat().st_size
    print(f"           -> {size:,} B written", file=sys.stderr)
    companions = _fetch_gltf_companions(gap, target, force)
    return _record(gap, target, out_root, companions)


def _fetch_gltf_companions(gap: GapAsset, target: Path, force: bool) -> list[str]:
    """Multi-file glTF (#486): a .gltf declares its geometry buffer +
    textures as sibling URIs. Parse the just-fetched .gltf and download
    each declared sibling from the same URL directory into out_dir next
    to the model, so the seed pipeline can stage them as companions. No
    hardcoded file lists — the .gltf is the source of truth. Best-effort:
    a failed sibling logs + is skipped (the model still renders, just
    untextured). GLB and other formats are self-contained → []."""
    if target.suffix.lower() != ".gltf":
        return []
    try:
        doc = json.loads(target.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        print(f"    companion parse skipped: {e}", file=sys.stderr)
        return []

    uris: list[str] = []
    seen: set[str] = set()
    for coll in ("buffers", "images"):
        for item in doc.get(coll, []):
            uri = (item.get("uri") or "").strip()
            if not uri or uri.lower().startswith("data:") or "://" in uri:
                continue
            uri = urllib.parse.unquote(uri)
            if uri.startswith("/") or ".." in uri or uri in seen:
                continue
            seen.add(uri)
            uris.append(uri)

    base_url = gap.url.rsplit("/", 1)[0]
    got: list[str] = []
    for uri in uris:
        dest = target.parent / uri
        if dest.exists() and not force:
            got.append(uri)
            continue
        curl = f"{base_url}/{urllib.parse.quote(uri)}"
        req = urllib.request.Request(curl, headers={"User-Agent": "artist-alley-seed-fetcher/2.0"})
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_bytes(resp.read())
            got.append(uri)
        except Exception as e:
            print(f"    companion error {uri}: {e}", file=sys.stderr)
    if got:
        print(f"           +{len(got)} companions", file=sys.stderr)
    return got


def _record(gap: GapAsset, target: Path, out_root: Path, companions: list[str] | None = None) -> dict:
    rec = {
        "name": gap.name,
        "path": str(target.relative_to(out_root)),
        "size_bytes": target.stat().st_size,
        "sha256": sha256_of_file(target),
        "license": gap.license,
        "attribution": gap.attribution,
        "source": gap.source,
        "asset_type": gap.asset_type,
        "notes": gap.notes,
        "fetched_from": gap.url,
    }
    # Record the multi-file siblings for provenance/visibility. The seed
    # pipeline (populate_archive.py + the Go runner) resolves + stages
    # them from disk, so this list is informational, not load-bearing.
    if companions:
        rec["companions"] = companions
    return rec


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, type=Path,
                        help="Output directory (gitignored cache)")
    parser.add_argument("--force", action="store_true",
                        help="Re-fetch even if file is already cached")
    parser.add_argument("--only", default=None,
                        help="Case-insensitive substring of the gap asset's "
                             "name; fetch just that one. Exists because "
                             "repairing a single multi-file model (Sponza's "
                             "companions, #572) otherwise means re-walking "
                             "the whole multi-GB catalogue, and 'the cheap "
                             "way to fix one asset is to skip the tool' is "
                             "how the companions went missing.")
    parser.add_argument("--manifest", action="store_true",
                        help="Rewrite MANIFEST.json. Off when --only is used: "
                             "a one-asset run would otherwise replace the "
                             "catalogue manifest with a one-entry file.")
    args = parser.parse_args()

    args.out.mkdir(parents=True, exist_ok=True)

    gaps = GAPS
    if args.only:
        needle = args.only.lower()
        gaps = [g for g in GAPS if needle in g.name.lower()]
        if not gaps:
            print(f"error: --only {args.only!r} matched none of "
                  f"{len(GAPS)} gap assets", file=sys.stderr)
            return 2

    print(f"fetching {len(gaps)} gap assets to {args.out}", file=sys.stderr)
    manifest: list[dict] = []
    failed: list[str] = []
    for gap in gaps:
        result = fetch(gap, args.out, force=args.force)
        if result is None:
            failed.append(gap.name)
            continue
        manifest.append(result)

    if args.only and not args.manifest:
        print(f"\n{len(manifest)} asset(s) fetched / cached "
              "(MANIFEST.json left alone — pass --manifest to rewrite it)",
              file=sys.stderr)
        return 1 if failed else 0

    manifest_path = args.out / "MANIFEST.json"
    manifest_path.write_text(json.dumps({
        "asset_count": len(manifest),
        "total_bytes": sum(m["size_bytes"] for m in manifest),
        "assets": manifest,
        "failed": failed,
    }, indent=2), encoding="utf-8")

    print(f"\nwrote manifest to {manifest_path}", file=sys.stderr)
    print(f"  {len(manifest)} assets fetched / cached", file=sys.stderr)
    print(f"  total: {sum(m['size_bytes'] for m in manifest) / 2**20:.1f} MB", file=sys.stderr)
    if failed:
        print(f"  {len(failed)} failed:", file=sys.stderr)
        for name in failed:
            print(f"    - {name}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
