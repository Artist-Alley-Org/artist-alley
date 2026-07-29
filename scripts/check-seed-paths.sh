#!/usr/bin/env bash
# scripts/check-seed-paths.sh
#
# Fails if a maintainer's machine leaks into the public repo.
#
# Why this gate exists: seed/ accumulated 51 files documenting one
# workstation's directory layout, a NAS share, and the paths of two
# unrelated private projects — enough for a reader to infer what else
# was on the machine. None of it helped anyone seed their own instance
# (issue #715). It accumulated because nothing was watching, exactly
# like the ADR frontmatter drift that scripts/check-adr-frontmatter.sh
# now guards.
#
# What it checks:
#
#   1. No absolute host path under seed/ — /mnt, /home, /Users, /srv,
#      /media, /opt, /volume1 (Synology), Windows drive letters, UNC
#      shares. Container-side paths (/seed/site, /app, ...) are fine;
#      they are arguments to a mount, not a place on someone's disk.
#      A dataset root differs between every machine that has one, so a
#      hardcoded root is wrong on correctness grounds before it is
#      wrong on privacy grounds.
#
#   2. No mention, anywhere in the tracked tree, of the private things
#      that were being published: the two unrelated project names, the
#      NAS share name, the NAS vendor, and the Kaggle credential file.
#      Nothing that ships may name the credential file — not to warn
#      about it, not to ignore it, not in a comment.
#
# Both checks run over tracked files only (`git ls-files`), because an
# untracked scratch file is not published. Binary files are skipped.
#
# Usage:
#   ./scripts/check-seed-paths.sh          # check the tracked tree
#   ./scripts/check-seed-paths.sh a b      # check specific files
#
# If you need a real filesystem root, take it as a CLI flag or an
# environment variable and document the variable. `seed/README.md` and
# `seed/SEED_INSTRUCTIONS.md` show the shape.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

python3 - "$@" <<'PY'
import re
import subprocess
import sys

SELF = 'scripts/check-seed-paths.sh'

# 1. Absolute host paths, checked under seed/ only.
#
# Anchored on a boundary rather than \b so `//mnt/...` and
# `"/home/...` are caught too — the leaked strings in #715 included
# `images//mnt/d/Projects/...`, which a \b-anchored pattern misses.
HOST_PATH = re.compile(
    r'(?<![A-Za-z0-9_.-])'
    r'(?:'
    r'/(?:mnt|home|Users|srv|media|opt|volume\d|export|net|remotes)/'
    r'|[A-Za-z]:[\\/](?:Users|Projects|Data)[\\/]'
    r'|\\\\[A-Za-z0-9_-]+\\'          # UNC share
    r')'
)

# Deliberately not part of the host-path rule: `/seed/...`, `/app`,
# `/src`, `/datasets` and friends are container-side mount targets that
# say nothing about anyone's disk.

# 2. Private names, checked across the whole tracked tree.
PRIVATE_NAMES = [
    (re.compile(r'snapdex', re.I),
     'names an unrelated private project'),
    (re.compile(r'unraid[_-]management', re.I),
     'names an unrelated private project'),
    (re.compile(r'blackbox[_-]archives', re.I),
     "names the maintainer's NAS share"),
    (re.compile(r'synology', re.I),
     "names the maintainer's NAS vendor"),
    (re.compile(r'kaggle\.txt', re.I),
     'names the Kaggle credential file, which must stay unmentioned'),
]

BINARY_EXT = {
    'png', 'jpg', 'jpeg', 'gif', 'webp', 'ico', 'bmp', 'tif', 'tiff',
    'pdf', 'epub', 'cbz', 'zip', 'gz', 'tgz', 'bz2', 'xz', 'br',
    'mp3', 'ogg', 'wav', 'flac', 'm4a', 'm4b', 'aac',
    'mp4', 'webm', 'mov', 'mkv', 'avi', 'ogv',
    'glb', 'gltf', 'bin', 'fbx', 'obj', 'hdr', 'exr', 'ktx2',
    'ttf', 'otf', 'woff', 'woff2',
    'so', 'dylib', 'dll', 'node', 'wasm', 'a', 'o',
}


def tracked(paths):
    if paths:
        return paths
    out = subprocess.run(['git', 'ls-files', '-z'],
                         capture_output=True, text=True, check=True).stdout
    return [p for p in out.split('\0') if p]


def read_text(path):
    if path.rsplit('.', 1)[-1].lower() in BINARY_EXT:
        return None
    try:
        with open(path, 'rb') as fh:
            blob = fh.read()
    except (FileNotFoundError, IsADirectoryError):
        return None  # deleted in the change under test
    if b'\0' in blob:
        return None
    try:
        return blob.decode('utf-8')
    except UnicodeDecodeError:
        return None


problems = []
checked_paths = 0
checked_names = 0

for path in tracked(sys.argv[1:]):
    if path == SELF:
        continue  # the checker states the patterns it forbids
    text = read_text(path)
    if text is None:
        continue

    in_seed = path == 'seed' or path.startswith('seed/')
    if in_seed:
        checked_paths += 1
    checked_names += 1

    for lineno, line in enumerate(text.splitlines(), 1):
        if in_seed:
            hit = HOST_PATH.search(line)
            if hit:
                problems.append(
                    '%s:%d: absolute host path %r — take the root as a flag '
                    'or env var instead' % (path, lineno, hit.group(0)))
        for pattern, why in PRIVATE_NAMES:
            hit = pattern.search(line)
            if hit:
                problems.append(
                    '%s:%d: %r %s' % (path, lineno, hit.group(0), why))

for p in problems:
    print(p, file=sys.stderr)

if problems:
    print(
        '\ncheck-seed-paths: %d problem(s). These publish a maintainer\'s '
        'machine layout or a private project to a public repo — see issue '
        '#715 and seed/README.md.' % len(problems),
        file=sys.stderr)
    sys.exit(1)

print('check-seed-paths: %d file(s) scanned for host paths under seed/, '
      '%d for private names. Clean.' % (checked_paths, checked_names))
PY
