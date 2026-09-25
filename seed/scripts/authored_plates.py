#!/usr/bin/env python3
"""
Produce the two studio plates that let the corpus declare `assisted` and
`none` (#1290).

WHY THE CORPUS NEEDED NEW BYTES RATHER THAN NEW LABELS
------------------------------------------------------
`ai_provenance` has four states and the seeded dataset only ever declared
one. Measured on the coding stack at `85e96542`: `generated` 69 live,
`assisted` 18 — every one soft-deleted — and `none` 12, likewise. So two
of the four states had never been rendered for a human being, and the one
a wrong rendering damages most is `none`, which must never be turned into
a "no AI" claim.

The obvious fix is to write the labels onto records that already exist.
⛔ It is forbidden, and not on a technicality:

  * ADR 0094 — writing `none` onto an undeclared row asserts a disclosure
    on the maker's behalf, "a fabricated disclosure on the one topic where
    a false disclaimer is the worst available error".
  * #1260 — the corpus already shipped four records declaring `generated`
    on Kenney.nl works. `attribution: "Kenney (kenney.nl)"` sat on the
    same row. That was a false statement about a named real creator in a
    dataset that is published, and it is exactly what re-labelling makes.

Every asset in this corpus is either a third party's work (Kenney, Pexels,
Google Fonts, Wikimedia, the Met, NASA, …) or one of the 45 in-house
Stable Diffusion plates from #1260 — and all 45 are already `generated`,
with `acquisition_source: "Generated in-house (Stable Diffusion 3.5 Large
via ComfyUI)"` on the same record. Re-declaring one of those `assisted`
would contradict its own provenance field. There was nothing in the
corpus that could honestly carry either label, so the honest answer is to
MAKE something that can.

WHAT THIS PRODUCES, AND WHY EACH LABEL IS TRUE OF IT
----------------------------------------------------
  studio-colour-chart.png   `none`
      A calibration chart: colour patches, a greyscale ramp and
      registration marks, every pixel placed by the arithmetic below.
      No generative model is involved at any point, which is what makes
      `none` a statement of fact rather than a disclaimer.

  reference-mood-board.png  `assisted`
      A mood board. One AI-generated plate from the #1260 set is
      downsampled into a panel; the palette strip beside it is sampled
      from that plate's own pixels; the swatch blocks, rules and
      registration marks around it are drawn here. So part of the work
      came out of a generative model and part did not, which is precisely
      the middle state — neither `generated` nor `none` would be true.

⚠️ THE REPO CARRIES THE RECIPE, NOT THE BYTES. Same as `kenney_hq.py
build`: the dataset lives on the archive share and the pipeline is
reproducible from the repo.

⛔ THE INPUT IS AN ATTESTED SNAPSHOT AND THE OUTPUT IS EXTERNAL (#1319).
The old recipe read and wrote `$DATASET_SRC`, and that source dataset has
been permanently retired: the maintained copy is the published archive.
Two rules follow, and neither is a convenience:

  * `--generated-source` is the `images/aurora-generated` directory of a
    FROZEN, MANIFEST-ATTESTED snapshot, not the live site. The mood board
    samples those pixels, so they are an INPUT to a hash the profile
    records; reading them from a tree that can change under the run makes
    the output unreproducible and the claim unverifiable.
  * `--out` is a scratch directory OUTSIDE every site tree. The plates are
    then installed deliberately, as a separate step, and the install is
    checked against the sizes and hashes the profile already records.
    ⛔ `populate_archive.py` NEVER SYNTHESIZES THEM: a publish that can
    manufacture the bytes it is about to attest has no independent
    evidence left.

    # attest the snapshot first (bytes only; mtimes prove nothing)
    python3 seed/scripts/preserved_archive.py snapshot-manifest \\
        --snapshot $FROZEN/site_a --out $EVIDENCE/site_a.snapshot-manifest.json

    python3 seed/scripts/authored_plates.py build \\
        --generated-source $FROZEN/site_a/images/aurora-generated \\
        --out              $SCRATCH/aurora-authored

Measured 2026-09-24 against the archive's own `images/aurora-generated`,
the two plates reproduce byte-exactly at the sizes and hashes the
committed `studio-a.assets.json` already records:

    studio-colour-chart.png     11,404 B  sha256 1495db50a28a55ba…
    reference-mood-board.png 1,290,128 B  sha256 6fa8e7e7790b76fc…

Deterministic: the same inputs produce byte-identical outputs, so a
rebuild does not churn `file_size_bytes` in the profile.

Stdlib only — zlib and struct. `sharp` is a build dependency of
`kenney_hq.py` and is not installed everywhere; a plate generator that
needed it would be a plate generator nobody could run.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import struct
import sys
import zlib
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import preserved_archive as pa  # noqa: E402

# The plate whose pixels the mood board samples. Named rather than
# discovered, so the output cannot change because a directory listing
# came back in a different order.
MOOD_BOARD_SOURCE = "ref-colour-script.png"

W, H = 1344, 768          # matches the #1260 plates, so the wall is even
PAD = 48


# --------------------------------------------------------------------------
# PNG, 8-bit RGB, non-interlaced. That is what the #1260 plates are
# (colour type 2, depth 8), so the reader only has to handle that case —
# and the writer only ever emits it.
# --------------------------------------------------------------------------

def png_read_rgb(path: Path) -> tuple[int, int, bytearray]:
    raw = path.read_bytes()
    if raw[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError(f"{path}: not a PNG")
    pos, idat, w, h = 8, bytearray(), 0, 0
    while pos < len(raw):
        (length,) = struct.unpack(">I", raw[pos:pos + 4])
        ctype = raw[pos + 4:pos + 8]
        body = raw[pos + 8:pos + 8 + length]
        if ctype == b"IHDR":
            w, h, depth, colour, _, _, interlace = struct.unpack(">IIBBBBB", body)
            if (depth, colour, interlace) != (8, 2, 0):
                raise ValueError(
                    f"{path}: expected 8-bit RGB non-interlaced, got depth={depth} "
                    f"colour={colour} interlace={interlace}")
        elif ctype == b"IDAT":
            idat += body
        elif ctype == b"IEND":
            break
        pos += 12 + length

    data = zlib.decompress(bytes(idat))
    stride = w * 3
    out = bytearray(w * h * 3)
    prev = bytearray(stride)
    src = 0
    for y in range(h):
        filt = data[src]
        src += 1
        line = bytearray(data[src:src + stride])
        src += stride
        if filt == 1:                                   # Sub
            for i in range(3, stride):
                line[i] = (line[i] + line[i - 3]) & 0xFF
        elif filt == 2:                                 # Up
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 0xFF
        elif filt == 3:                                 # Average
            for i in range(stride):
                a = line[i - 3] if i >= 3 else 0
                line[i] = (line[i] + ((a + prev[i]) >> 1)) & 0xFF
        elif filt == 4:                                 # Paeth
            for i in range(stride):
                a = line[i - 3] if i >= 3 else 0
                b = prev[i]
                c = prev[i - 3] if i >= 3 else 0
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                pred = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
                line[i] = (line[i] + pred) & 0xFF
        elif filt != 0:
            raise ValueError(f"{path}: unknown row filter {filt}")
        out[y * stride:(y + 1) * stride] = line
        prev = line
    return w, h, out


def png_write_rgb(path: Path, w: int, h: int, pix: bytearray) -> int:
    stride = w * 3
    raw = bytearray()
    for y in range(h):
        raw.append(0)                                   # filter: None
        raw += pix[y * stride:(y + 1) * stride]
    comp = zlib.compress(bytes(raw), 9)

    def chunk(tag: bytes, body: bytes) -> bytes:
        return (struct.pack(">I", len(body)) + tag + body
                + struct.pack(">I", zlib.crc32(tag + body) & 0xFFFFFFFF))

    blob = (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", comp)
            + chunk(b"IEND", b""))
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(blob)
    return len(blob)


# --------------------------------------------------------------------------
# Drawing
# --------------------------------------------------------------------------

def canvas(w: int, h: int, rgb: tuple[int, int, int]) -> bytearray:
    return bytearray(bytes(rgb) * (w * h))


def rect(pix: bytearray, w: int, x0: int, y0: int, rw: int, rh: int,
         rgb: tuple[int, int, int]) -> None:
    row = bytes(rgb) * rw
    for y in range(y0, y0 + rh):
        off = (y * w + x0) * 3
        pix[off:off + rw * 3] = row


def frame(pix: bytearray, w: int, x0: int, y0: int, rw: int, rh: int,
          rgb: tuple[int, int, int], t: int = 3) -> None:
    rect(pix, w, x0, y0, rw, t, rgb)
    rect(pix, w, x0, y0 + rh - t, rw, t, rgb)
    rect(pix, w, x0, y0, t, rh, rgb)
    rect(pix, w, x0 + rw - t, y0, t, rh, rgb)


def registration(pix: bytearray, w: int, h: int,
                 rgb: tuple[int, int, int]) -> None:
    """Corner marks. A calibration plate without them looks like wallpaper."""
    arm, t = 34, 3
    for cx, cy in ((PAD // 2, PAD // 2), (w - PAD // 2, PAD // 2),
                   (PAD // 2, h - PAD // 2), (w - PAD // 2, h - PAD // 2)):
        rect(pix, w, cx - arm // 2, cy - t // 2, arm, t, rgb)
        rect(pix, w, cx - t // 2, cy - arm // 2, t, arm, rgb)


def downsample(src: bytearray, sw: int, sh: int,
               dw: int, dh: int) -> bytearray:
    """Nearest neighbour. Deterministic and dependency-free; a mood-board
    panel is not the place to need a resampling kernel."""
    out = bytearray(dw * dh * 3)
    for y in range(dh):
        sy = y * sh // dh
        for x in range(dw):
            sx = x * sw // dw
            s = (sy * sw + sx) * 3
            d = (y * dw + x) * 3
            out[d:d + 3] = src[s:s + 3]
    return out


def blit(dst: bytearray, dw: int, src: bytearray, sw: int, sh: int,
         x0: int, y0: int) -> None:
    for y in range(sh):
        d = ((y0 + y) * dw + x0) * 3
        s = y * sw * 3
        dst[d:d + sw * 3] = src[s:s + sw * 3]


def sample_palette(pix: bytearray, w: int, h: int, n: int) -> list[tuple[int, int, int]]:
    """Average colour of `n` equal vertical bands.

    Averaging rather than point-sampling on purpose: a single pixel from a
    painted plate is as likely to be a highlight as the colour a person
    would call the band's own.
    """
    out = []
    for i in range(n):
        x0, x1 = i * w // n, (i + 1) * w // n
        r = g = b = count = 0
        for y in range(0, h, 4):                       # every 4th row is plenty
            base = y * w * 3
            for x in range(x0, x1, 4):
                o = base + x * 3
                r += pix[o]
                g += pix[o + 1]
                b += pix[o + 2]
                count += 1
        out.append((r // count, g // count, b // count))
    return out


# --------------------------------------------------------------------------
# The two plates
# --------------------------------------------------------------------------

# A calibration chart's patches are the point of it, so they are named
# constants rather than a generated ramp — these are the primaries,
# secondaries and skin/foliage/sky references a studio chart carries.
CHART_PATCHES = [
    (0xBE, 0x6F, 0x5A), (0xD9, 0xA5, 0x8B), (0x6B, 0x84, 0xA8), (0x5F, 0x6F, 0x42),
    (0x86, 0x83, 0xB0), (0x6F, 0xC0, 0xB4), (0xD6, 0x7F, 0x2C), (0x50, 0x59, 0xA6),
    (0xC1, 0x5B, 0x66), (0x5C, 0x3D, 0x69), (0xA4, 0xC2, 0x3F), (0xE0, 0xA3, 0x22),
    (0x38, 0x3C, 0x96), (0x51, 0x9E, 0x50), (0xAF, 0x36, 0x3C), (0xED, 0xC8, 0x1E),
    (0xB5, 0x51, 0x9C), (0x00, 0x86, 0xA8), (0xF0, 0xF0, 0xEB), (0xC1, 0xC2, 0xC0),
    (0x91, 0x93, 0x92), (0x63, 0x65, 0x65), (0x3A, 0x3C, 0x3D), (0x18, 0x1A, 0x1B),
]


def build_colour_chart(out: Path) -> int:
    pix = canvas(W, H, (0x1B, 0x1D, 0x20))
    cols, rows = 6, 4
    gw = W - 2 * PAD
    ramp_h = 96
    grid_h = H - 2 * PAD - ramp_h - PAD // 2
    cw, ch = gw // cols, grid_h // rows
    for i, rgb in enumerate(CHART_PATCHES):
        x = PAD + (i % cols) * cw
        y = PAD + (i // cols) * ch
        rect(pix, W, x + 4, y + 4, cw - 8, ch - 8, rgb)
        frame(pix, W, x + 4, y + 4, cw - 8, ch - 8, (0x0E, 0x0F, 0x11), 2)

    # 21-step greyscale ramp: the half of a chart that makes it a
    # measuring instrument rather than a swatch card.
    ry = PAD + grid_h + PAD // 2
    steps = 21
    sw = gw // steps
    for i in range(steps):
        v = round(i * 255 / (steps - 1))
        rect(pix, W, PAD + i * sw, ry, sw, ramp_h, (v, v, v))
    frame(pix, W, PAD, ry, sw * steps, ramp_h, (0x0E, 0x0F, 0x11), 2)
    registration(pix, W, H, (0xE8, 0xE8, 0xE4))
    return png_write_rgb(out, W, H, pix)


def build_mood_board(out: Path, generated_source: Path) -> int:
    src_path = generated_source / MOOD_BOARD_SOURCE
    if not src_path.is_file():
        raise SystemExit(
            f"error: {src_path} not found.\n"
            "  The mood board samples one of the #1260 Stable Diffusion plates —\n"
            "  that is what makes its `assisted` declaration true. Point\n"
            "  --generated-source at the images/aurora-generated directory of a\n"
            "  FROZEN, manifest-attested snapshot (#1319), not at the live site,\n"
            "  whose bytes can change under the run.")
    sw, sh, spix = png_read_rgb(src_path)

    pix = canvas(W, H, (0x16, 0x18, 0x1A))
    panel_w = (W - 2 * PAD) * 55 // 100
    panel_h = H - 2 * PAD
    thumb = downsample(spix, sw, sh, panel_w, panel_h)
    blit(pix, W, thumb, panel_w, panel_h, PAD, PAD)
    frame(pix, W, PAD, PAD, panel_w, panel_h, (0xE8, 0xE8, 0xE4), 3)

    # The palette strip is sampled from the plate above it, so the AI
    # contribution reaches the authored half of the sheet as colour rather
    # than as decoration.
    right_x = PAD + panel_w + PAD // 2
    right_w = W - right_x - PAD
    palette = sample_palette(spix, sw, sh, 6)
    strip_h = 120
    bw = right_w // len(palette)
    for i, rgb in enumerate(palette):
        rect(pix, W, right_x + i * bw, PAD, bw, strip_h, rgb)
    frame(pix, W, right_x, PAD, bw * len(palette), strip_h, (0xE8, 0xE8, 0xE4), 2)

    # Authored swatch blocks: fixed studio greys, drawn here, nothing
    # sampled. The half of the sheet that is not the model's.
    grid_y = PAD + strip_h + PAD // 2
    grid_h = H - grid_y - PAD
    for r in range(3):
        for c in range(3):
            v = 0x22 + (r * 3 + c) * 0x18
            bwid, bhei = right_w // 3, grid_h // 3
            rect(pix, W, right_x + c * bwid + 3, grid_y + r * bhei + 3,
                 bwid - 6, bhei - 6, (v, v, v + 6))
    frame(pix, W, right_x, grid_y, (right_w // 3) * 3, (grid_h // 3) * 3,
          (0xE8, 0xE8, 0xE4), 2)
    registration(pix, W, H, (0xE8, 0xE8, 0xE4))
    return png_write_rgb(out, W, H, pix)


PLATES = ("studio-colour-chart.png", "reference-mood-board.png")

# Where an installed plate lives inside a site, relative to the site root.
# The profile records carry this prefix, so the expectation is read FROM the
# profile rather than restated here; this is only how the directory is found.
INSTALL_DIR = "images/aurora-authored"


def expected_plates(profile_path: Path) -> dict[str, tuple[int, str]]:
    """filename -> (file_size_bytes, sha256) for the authored-plate records
    the committed profile already carries.

    ⛔ THE EXPECTATION IS THE PROFILE'S, NOT THIS MODULE'S. A constant here
    would be a second opinion that could drift from the one the publish
    guard and the seeder actually use, and the pair would then agree with
    each other while disagreeing with the dataset.
    """
    prof = json.loads(profile_path.read_text(encoding="utf-8"))
    out: dict[str, tuple[int, str]] = {}
    for rec in prof:
        fp = str(rec.get("file_path") or "")
        if not fp.startswith(INSTALL_DIR + "/"):
            continue
        sha = (rec.get("metadata") or {}).get("sha256")
        size = rec.get("file_size_bytes")
        if not sha or not isinstance(size, int):
            raise SystemExit(
                f"error: {profile_path}: the authored-plate record {rec.get('id')} "
                f"({fp}) declares no sha256 and/or no file_size_bytes, so an "
                f"install cannot be checked against it. An unattested plate is "
                f"not installable.")
        out[Path(fp).name] = (size, sha)
    return out


def verify_install(directory: Path, want: dict[str, tuple[int, str]]) -> list[str]:
    """Every way an installed plate set can fail to be the documented one.

    EXACTLY the expected files, each at its declared size and hash. ⛔ An
    UNEXPECTED file refuses too: the install hands a directory to the site
    and then the Kaggle uploader hands the site to the world, so a third
    output nobody enumerated would be published as studio work.
    """
    refusals: list[str] = []
    if not directory.is_dir():
        return [f"{directory} is not a directory"]
    present = {p.name for p in sorted(directory.iterdir()) if p.is_file()}
    for extra in sorted(present - set(want)):
        refusals.append(f"{extra} is not one of the {len(want)} documented plates "
                        f"{sorted(want)}; an output nobody enumerated would be "
                        f"published as studio work")
    for name, (size, sha) in sorted(want.items()):
        f = directory / name
        if not f.is_file():
            refusals.append(f"{name} is absent")
            continue
        got = f.stat().st_size
        if got != size:
            refusals.append(f"{name} is {got:,} B, the profile declares {size:,} B")
            continue
        digest = hashlib.sha256(f.read_bytes()).hexdigest()
        if digest != sha:
            refusals.append(f"{name} is {got:,} B as declared but hashes "
                            f"{digest[:12]}… where the profile declares "
                            f"{sha[:12]}…. Same length, different pixels.")
    return refusals


def _cmd_verify(args) -> int:
    """Check a built or installed plate set against the committed profile.

    ⛔ THIS IS THE GATE THAT RUNS BEFORE A PUBLISH, NOT DURING ONE.
    `populate_archive.py` never synthesizes the plates: a publish that can
    manufacture the bytes it is about to attest has no independent evidence
    left. So the bytes are built externally, checked here, installed, and
    only then published.
    """
    want = expected_plates(args.profile)
    if not want:
        print(f"error: {args.profile} carries no {INSTALL_DIR}/ records, so there "
              f"is nothing to verify an install against.", file=sys.stderr)
        return 2
    directory = args.dir
    if args.site is not None:
        directory = args.site / INSTALL_DIR
    refusals = verify_install(directory, want)
    if refusals:
        print(f"⛔ {directory} is not the documented plate set "
              f"({len(refusals)} refusal(s)):", file=sys.stderr)
        for r in refusals:
            print(f"  - {r}", file=sys.stderr)
        return 1
    for name, (size, sha) in sorted(want.items()):
        print(f"  {name:28s} {size:>9,} B  {sha[:12]}…", file=sys.stderr)
    print(f"{len(want)} plate(s) match the profile exactly", file=sys.stderr)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("command", choices=("build", "verify"))
    ap.add_argument("--generated-source", type=Path, default=None,
                    help="the images/aurora-generated directory of a FROZEN, "
                         "manifest-attested snapshot (#1260, #1319). The mood "
                         "board samples one of those plates, which is what makes "
                         "its `assisted` declaration true, so it must not be read "
                         "from a tree that can change under the run.")
    ap.add_argument("--out", type=Path, default=None,
                    help="build: scratch destination directory OUTSIDE every site "
                         "tree, e.g. $SCRATCH/aurora-authored. The plates are "
                         "installed into a site as a separate, checked step; "
                         "nothing synthesizes them during a publish.")
    ap.add_argument("--snapshot", type=Path, default=None,
                    help="build: the FROZEN pre-operation snapshot root. "
                         "--generated-source must be inside it, which is what "
                         "makes the sampled pixels attested.")
    ap.add_argument("--live-site", type=Path, default=None,
                    help="build: the live published site (same path as --staging "
                         "for a direct publish)")
    ap.add_argument("--staging", type=Path, default=None,
                    help="build: the tree a publish writes")
    ap.add_argument("--evidence", type=Path, action="append", default=[],
                    help="build: an evidence path the scratch output must stay "
                         "outside of. Repeatable.")
    ap.add_argument("--dir", type=Path, default=None,
                    help="verify: the directory holding the built or installed "
                         "plates")
    ap.add_argument("--site", type=Path, default=None,
                    help=f"verify: a site root, whose {INSTALL_DIR}/ is checked")
    ap.add_argument("--profile", type=Path, default=None,
                    help="verify: the assets profile whose authored-plate records "
                         "state the expected sizes and hashes")
    args = ap.parse_args()

    if args.command == "verify":
        if args.profile is None or (args.dir is None) == (args.site is None):
            print("error: verify needs --profile and exactly one of --dir / --site",
                  file=sys.stderr)
            return 2
        return _cmd_verify(args)
    missing = [n for n, v in (("--out", args.out), ("--snapshot", args.snapshot),
                              ("--live-site", args.live_site),
                              ("--staging", args.staging)) if v is None]
    if missing:
        print(f"error: build needs {', '.join(missing)}.\n"
              "  The plates are built OUTSIDE every site tree and installed as a "
              "separate checked step, so the build has to know where those trees "
              "are in order to prove its output is not inside one. ⛔ An "
              "unprovable boundary must not read as a satisfied one; for a direct "
              "publish pass the same path for --live-site and --staging.",
              file=sys.stderr)
        return 2

    # ⛔ ALIAS REFUSAL. Writing the plates into the directory they sample
    # would make the next build's input depend on the last build's output,
    # and the recorded hashes would then describe a tree that produced
    # itself.
    if args.generated_source is None:
        print("error: build needs --generated-source", file=sys.stderr)
        return 2
    if not pa.refuse_aliasing({"--generated-source": args.generated_source,
                               "--out": args.out}):
        return 2
    # ⛔ AND THE SCRATCH OUTPUT SITS OUTSIDE ALL THREE OPERATION TREES AND
    # OUTSIDE THE EVIDENCE. A directory inside live or staging would be
    # published and pruned as if it were content; one inside the snapshot would
    # be attested as if it were part of what the snapshot froze.
    if not pa.refuse_operation(
            live=args.live_site, staging=args.staging, snapshot=args.snapshot,
            evidence={f"--evidence[{i}]": e for i, e in enumerate(args.evidence)},
            scratch={"--out": args.out}):
        return 2
    # The sampled pixels are an INPUT to a hash the profile records, so they
    # must come from the attested tree rather than from wherever the operator
    # happened to point.
    gen, snap = args.generated_source.resolve(), args.snapshot.resolve()
    if not (gen == snap or gen.is_relative_to(snap)):
        print(f"error: --generated-source {gen} is not inside --snapshot {snap}.\n"
              "  The mood board samples those pixels, so they are an input to a "
              "hash the profile records. Read from a tree that can change under "
              "the run and the output is unreproducible and the claim "
              "unverifiable.", file=sys.stderr)
        return 2
    args.out.mkdir(parents=True, exist_ok=True)

    n = build_colour_chart(args.out / PLATES[0])
    print(f"{PLATES[0]:28s} {n:>9,} B  ai_provenance=none", file=sys.stderr)
    n = build_mood_board(args.out / PLATES[1], args.generated_source)
    print(f"{PLATES[1]:28s} {n:>9,} B  ai_provenance=assisted", file=sys.stderr)
    print(f"\nwrote 2 plate(s) to {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
