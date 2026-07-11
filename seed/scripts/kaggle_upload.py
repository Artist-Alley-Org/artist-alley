#!/usr/bin/env python3
"""
Upload site_a (Layer A only — public-safe) to Kaggle as a public dataset.

This makes the artist-alley demo seed downloadable by anyone via the
Kaggle API or browser. Useful for the Phase 1.48 public demo's
provisioner workflow.

Usage
-----
    python3 kaggle_upload.py \\
        --src /mnt/blackbox_archives/datasets/artist_alley/site_a \\
        --handle mscrnt/artist-alley-demo-seed \\
        --version-notes "Initial release"

Requires kagglehub >= 0.4.1 + KAGGLE_API_TOKEN env var (or
~/.kaggle/access_token file). Token format KGAT_...

Dataset metadata
----------------
The script writes a `dataset-metadata.json` into <src> on first run.
Edit it to update title/subtitle/keywords/license. Subsequent runs
create a new VERSION of the dataset (not a new dataset entirely).

License
-------
The aggregate dataset is published under CC-BY-SA 4.0 (the most
restrictive license among the included Layer A sources — Tux Racer
gameplay is CC-BY-SA, the rest are more permissive). This means
redistributors must:
  - Provide attribution (per the ATTRIBUTIONS.md shipped with the
    dataset)
  - Share derivatives under CC-BY-SA 4.0

CC0 / Public Domain content is compatible with CC-BY-SA.
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path


# Kaggle dataset-metadata.json template. See:
# https://github.com/Kaggle/kaggle-api/wiki/Dataset-Metadata
METADATA_TEMPLATE = {
    "title": "Artist Alley — Demo Studio Seed (Layer A)",
    "subtitle": "Open-source game studio archive seed — CC0/CC-BY/PD content only",
    "description": (
        "A studio-shaped digital asset archive composed entirely of "
        "open-source, public-domain, and Creative Commons content. "
        "Sized to seed a small game studio's DAM with realistic format "
        "coverage: images, audio, 3D models, videos, documents, fonts, "
        "and game-asset references.\n\n"
        "This is the 'site_a' tier of the artist-alley seed dataset — "
        "Layer A only, no IP-referenced or personal content. Safe to "
        "redistribute publicly under CC-BY-SA 4.0.\n\n"
        "See ATTRIBUTIONS.md inside this dataset for the full source "
        "and license breakdown."
    ),
    "id": "mscrnt/artist-alley-demo-seed",
    "id_no": None,
    "licenses": [{"name": "CC-BY-SA-4.0"}],
    "keywords": [
        "digital asset management",
        "studio archive",
        "seed dataset",
        "open source",
        "creative commons",
        "public domain",
        "game development",
        "3d models",
        "audio",
        "video",
        "fonts",
    ],
    "collaborators": [],
    "data": [],
}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--src", required=True, type=Path,
                        help="Source directory (e.g., the populated site_a)")
    parser.add_argument("--handle", default="mscrnt/artist-alley-demo-seed",
                        help="Kaggle dataset handle <user>/<dataset>")
    parser.add_argument("--version-notes", default="Initial release",
                        help="Version notes for this upload")
    parser.add_argument("--write-metadata-only", action="store_true",
                        help="Just write dataset-metadata.json; don't upload")
    parser.add_argument("--public", action="store_true", default=True,
                        help="Make dataset public (default true)")
    args = parser.parse_args()

    if not args.src.is_dir():
        print(f"error: --src not a directory: {args.src}", file=sys.stderr)
        return 2

    # Write dataset-metadata.json into the upload directory
    meta_path = args.src / "dataset-metadata.json"
    meta = dict(METADATA_TEMPLATE)
    meta["id"] = args.handle
    meta_path.write_text(json.dumps(meta, indent=2), encoding="utf-8")
    print(f"wrote {meta_path}", file=sys.stderr)

    if args.write_metadata_only:
        print("(--write-metadata-only set; skipping upload)", file=sys.stderr)
        return 0

    # Use kagglehub for the upload
    try:
        import kagglehub
    except ImportError:
        print("error: kagglehub not installed (pip install --user kagglehub)",
              file=sys.stderr)
        return 2

    print(f"uploading to Kaggle: {args.handle}", file=sys.stderr)
    print(f"  source: {args.src}", file=sys.stderr)
    print(f"  version notes: {args.version_notes}", file=sys.stderr)

    try:
        # First-time upload: dataset_upload creates the dataset.
        # Subsequent uploads to the same handle create new versions.
        result = kagglehub.dataset_upload(
            args.handle,
            str(args.src),
            version_notes=args.version_notes,
        )
        print(f"\nuploaded: {result}", file=sys.stderr)
        print(f"URL: https://www.kaggle.com/datasets/{args.handle}", file=sys.stderr)
        return 0
    except Exception as e:
        print(f"upload failed: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
