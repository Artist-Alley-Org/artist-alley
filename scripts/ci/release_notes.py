#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Kenneth Blossom
"""
Check that a release has its reviewed release-notes file.

The GitHub Release body for tag vX.Y.Z is the file docs/release-notes/vX.Y.Z.md: public prose,
reviewed by a person in the release-prep PR, never an auto-generated list of PR titles. release.yml
publishes that file with `gh release create --notes-file` and never generates notes, so a release
without a valid file must fail before the Release page exists. This script is that gate, run in
two places:

  CI, on every PR and push (ci.yml, job `release-notes`):
      python3 -I scripts/ci/release_notes.py --package-json web/package.json
  The version in web/package.json is the version the next tag will carry (RELEASING.md), so a
  version bump without its notes file fails before anything is tagged.

  release.yml, before `gh release create`:
      python3 -I scripts/ci/release_notes.py --tag "$TAG" --package-json web/package.json
  Here the tag must also agree with web/package.json at the tagged commit: a stable tag vX.Y.Z
  must equal the package version, and a prerelease tag vX.Y.Z-suffix (an -rc or -beta) may carry
  either that exact version or its base X.Y.Z. Either way the notes file is the one for the full
  tag, so a prerelease needs its own docs/release-notes/vX.Y.Z-suffix.md.

What it checks, and nothing more: the exact file for the exact tag exists, is UTF-8 text, is not
blank, and starts with a Markdown heading that names that exact tag. That catches a missing file,
an empty one, and a previous release's file copied without updating. It deliberately does not
judge the prose: whether the notes read well is the reviewer's job, and a style heuristic here
would only pretend to replace that review.

Prints the notes path on success. Exits 1 with a reason on failure, 2 on bad arguments.
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

NOTES_DIR = Path("docs/release-notes")
TAG_RE = re.compile(r"^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$")
HEADING_RE = re.compile(r"^#{1,6} \S")


class NotesError(Exception):
    """A release-notes check failed; the message says why."""


def notes_path(tag: str, root: Path) -> Path:
    """The one file whose content becomes the Release body for this exact tag."""
    if not TAG_RE.match(tag):
        raise NotesError(f"{tag!r} is not a release tag of the form vX.Y.Z or vX.Y.Z-suffix")
    return root / NOTES_DIR / f"{tag}.md"


def tag_from_package_json(path: Path) -> str:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as e:
        raise NotesError(f"cannot read the version from {path}: {e}") from e
    version = data.get("version") if isinstance(data, dict) else None
    if not isinstance(version, str) or not version:
        raise NotesError(f"{path} has no version string")
    return f"v{version}"


def tag_matches_package(tag: str, version: str) -> bool:
    """A stable tag equals the package version; a prerelease tag equals it or extends its base.

    v0.11.2 needs 0.11.2. v0.11.2-rc.1 accepts 0.11.2-rc.1 or the base 0.11.2, but not 0.11.3,
    0.12.0 or 0.11.2-rc.2.
    """
    tagged = tag[1:] if tag.startswith("v") else tag
    if tagged == version:
        return True
    base, sep, _suffix = tagged.partition("-")
    return bool(sep) and base == version


def names_tag(line: str, tag: str) -> bool:
    """True when the heading line contains the tag as a whole token (v0.11.1, not v0.11.10)."""
    return re.search(rf"(?<![0-9A-Za-z.-]){re.escape(tag)}(?![0-9A-Za-z-]|\.[0-9A-Za-z])", line) is not None


def check_notes(tag: str, root: Path) -> Path:
    """Validate docs/release-notes/<tag>.md under root. Returns its path or raises NotesError."""
    path = notes_path(tag, root)
    if not path.is_file():
        raise NotesError(
            f"{path.relative_to(root)} does not exist. Every release needs reviewed release notes "
            f"for its exact tag; see RELEASING.md, 'Release notes'."
        )
    try:
        text = path.read_text(encoding="utf-8")
    except UnicodeDecodeError as e:
        raise NotesError(f"{path.relative_to(root)} is not UTF-8 text: {e}") from e
    lines = [ln for ln in text.splitlines() if ln.strip()]
    if not lines:
        raise NotesError(f"{path.relative_to(root)} is blank")
    first = lines[0]
    if not HEADING_RE.match(first):
        raise NotesError(
            f"{path.relative_to(root)} must start with a Markdown heading naming {tag}; "
            f"its first line is {first[:80]!r}"
        )
    if not names_tag(first, tag):
        raise NotesError(
            f"{path.relative_to(root)} starts with {first[:80]!r}, which does not name {tag}; "
            f"was a previous release's notes copied without updating?"
        )
    if len(lines) < 2:
        raise NotesError(f"{path.relative_to(root)} has a heading but no notes")
    return path


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--tag", help="the exact release tag, e.g. v0.11.1")
    p.add_argument("--package-json", type=Path, help="web/package.json; its version names the tag")
    p.add_argument("--root", type=Path, default=Path("."), help="repository root (default: .)")
    args = p.parse_args(argv)
    if not args.tag and not args.package_json:
        p.error("give --tag, --package-json, or both")
    try:
        tag = args.tag
        if tag is not None:
            notes_path(tag, args.root)  # reject a malformed tag before comparing versions
        if args.package_json:
            pkg_tag = tag_from_package_json(args.root / args.package_json)
            if tag and not tag_matches_package(tag, pkg_tag[1:]):
                raise NotesError(
                    f"tag {tag} does not match {args.package_json} version {pkg_tag[1:]}: a stable "
                    f"tag must equal it, and a prerelease tag must be that version or that version "
                    f"plus a -suffix. Bump the version with `make release` before tagging"
                )
            tag = tag or pkg_tag
        path = check_notes(tag, args.root)
    except NotesError as e:
        print(f"release-notes: FAIL: {e}", file=sys.stderr)
        return 1
    print(f"release-notes: ok: {path.relative_to(args.root)} for {tag}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
