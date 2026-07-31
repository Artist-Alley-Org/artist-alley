#!/usr/bin/env python3
"""Regenerate the EXIF extractor's fixture corpus.

Run from this directory:
    pip install Pillow
    python3 generate.py

Outputs deterministic, version-controlled JPEG fixtures with
documented EXIF state. Re-running the script must produce
byte-identical output (no timestamps, no random seeds).

Why Pillow's native Exif API (not piexif): piexif emits APP1
segments that older dsoprea-go-exif versions trip on (parses
into the JPEG body proper). Pillow 10+'s ``getexif()`` API
writes a leaner, more-compliant EXIF blob that the Go-side
parser handles cleanly.
"""

from pathlib import Path

from PIL import ExifTags, Image, ImageDraw

HERE = Path(__file__).parent

CANVAS = (64, 64)

# Deliberately non-square, and deliberately not a round multiple of the
# square canvas: a pair that can only come from THIS fixture.
LANDSCAPE = (96, 48)


def base_image(color=(70, 130, 180)) -> Image.Image:
    img = Image.new("RGB", CANVAS, color)
    d = ImageDraw.Draw(img)
    # Distinctive top-left corner pixel — orientation tests use
    # this to assert post-rotation pixel position.
    d.rectangle([0, 0, 7, 7], fill=(255, 50, 50))
    return img


def save(name: str, img: Image.Image, exif=None) -> None:
    out = HERE / name
    kwargs = {"format": "JPEG", "quality": 92}
    if exif is not None:
        kwargs["exif"] = exif
    img.save(out, **kwargs)
    print(f"  wrote {out.name} ({out.stat().st_size} bytes)")


def full_exif() -> None:
    img = base_image()
    exif = img.getexif()
    exif[ExifTags.Base.Make] = "Canon"
    exif[ExifTags.Base.Model] = "Canon EOS R5"
    exif[ExifTags.Base.Artist] = "Kenneth (fixture)"
    exif[ExifTags.Base.ImageDescription] = "Test image for AA metadata extractor"
    exif[ExifTags.Base.DateTime] = "2024:03:15 14:30:00"
    exif[ExifTags.Base.Orientation] = 1
    save("with_full_exif.jpg", img, exif)


def orientation(tag_value: int, name: str) -> None:
    img = base_image()
    exif = img.getexif()
    exif[ExifTags.Base.Orientation] = tag_value
    save(name, img, exif)


def orientation_landscape(tag_value: int, name: str) -> None:
    """A fixture whose rotation is actually OBSERVABLE (#765).

    The three orientation_N.jpg fixtures above are 64x64. A square
    image rotated 90 degrees is the same size as a square image not
    rotated, so any test asserting a WIDTH against them passes whether
    the rotation was applied, skipped, or applied twice — which is how
    a pre-rotation pixel_width write survived in the tree unnoticed.

    This one is LANDSCAPE_W x LANDSCAPE_H stored with the tag set, so a
    correct pipeline records the transposed pair and an incorrect one
    records the stored pair, and the two are distinguishable.
    """
    w, h = LANDSCAPE
    img = Image.new("RGB", (w, h), (70, 130, 180))
    d = ImageDraw.Draw(img)
    # Left third a different colour: after a 90-degree rotation this
    # band is horizontal, so a visual check disambiguates 6 from 8.
    d.rectangle([0, 0, w // 3, h], fill=(255, 50, 50))
    exif = img.getexif()
    exif[ExifTags.Base.Orientation] = tag_value
    save(name, img, exif)


def with_gps() -> None:
    """SF Ferry Building: 37.7955°N, 122.3937°W.

    TODO(1.18.A-2 follow-up): Pillow's getexif() GPS sub-IFD
    write-side has a tuple-format quirk that rejects the standard
    EXIF rational format. piexif emits a blob dsoprea won't parse;
    Pillow native fails to encode GPS at all. For now this fixture
    has no GPS data; GPS extraction is covered by the unit-level
    helpers + the production code path will be validated against
    real-camera photos in dogfood. File an issue + revisit with
    either (a) a different fixture-gen tool (exiftool), (b) a
    hand-crafted EXIF blob in Go, or (c) waiting for the dsoprea-
    piexif compat bug to resolve upstream.
    """
    # Placeholder: write a plain image; the with_gps.jpg path
    # exists so the test file finds something, but tests for GPS
    # extraction are currently skipped pending the fixture-gen fix.
    save("with_gps.jpg", base_image(), None)


def no_metadata() -> None:
    save("no_metadata.jpg", base_image(color=(100, 100, 100)))


def unicode_artist() -> None:
    """Japanese kanji artist name — round-trip UTF-8 test.

    EXIF's Artist field is ASCII-typed in the spec, but several
    camera vendors store UTF-8 bytes there anyway. Pillow writes
    the string as-is; dsoprea returns the bytes as-string which
    our extractor decodes as UTF-8.
    """
    img = base_image()
    exif = img.getexif()
    exif[ExifTags.Base.Artist] = "山田太郎"
    save("unicode_artist.jpg", img, exif)


def future_date() -> None:
    """Validator should reject; extractor still pulls the value."""
    img = base_image()
    exif = img.getexif()
    exif[ExifTags.Base.DateTime] = "3000:01:01 00:00:00"
    save("future_date.jpg", img, exif)


def gps_boundary() -> None:
    """TODO: see with_gps note — Pillow's getexif() GPS write-side
    is broken. Placeholder fixture for now; GPS boundary covered
    by the validator unit tests (see validate_test.go)."""
    save("gps_boundary.jpg", base_image(), None)


def malformed_truncated() -> None:
    """Cut a real JPEG in half — extractor returns ErrMalformedFile,
    NEVER panic."""
    import io
    buf = io.BytesIO()
    base_image().save(buf, format="JPEG", quality=92)
    truncated = buf.getvalue()[: len(buf.getvalue()) // 2]
    out = HERE / "malformed_truncated.jpg"
    out.write_bytes(truncated)
    print(f"  wrote {out.name} ({out.stat().st_size} bytes — truncated)")


def main() -> None:
    print("Regenerating EXIF fixture corpus:")
    full_exif()
    orientation(1, "orientation_1.jpg")
    orientation(6, "orientation_6.jpg")
    orientation(8, "orientation_8.jpg")
    orientation_landscape(6, "orientation_6_landscape.jpg")
    with_gps()
    no_metadata()
    unicode_artist()
    future_date()
    gps_boundary()
    malformed_truncated()
    print("Done.")


if __name__ == "__main__":
    main()
