#!/usr/bin/env bash
# scripts/check-mdx-hazards.sh
#
# Scans docs/**/*.md paths that get synced into the Astro/MDX docs
# site for prose patterns that MDX parses as JSX and fail the build:
#
#   - bare `{[a-zA-Z_]`   → parsed as JSX expression start
#   - bare `<[a-zA-Z]`    → parsed as JSX tag start
#
# Both are ONLY hazards when they appear in prose. Anything inside
# a triple-backtick fenced block or a single-backtick inline code
# span (including multi-line spans) is fine. Autolinks like
# <https://…> are also fine.
#
# The scanner exits non-zero + prints file:line: <pattern>: <line>
# for each real hazard, so CI can fail the PR + the operator sees
# where to fix.
#
# Scope: only the paths that actually feed the docs site build
# (docs/adr/**/*.md, docs/install/**/*.md, docs/roadmap.md). Other
# docs/*.md files stay unfiltered — they're plain reference files
# that no site build reads.
#
# Two prior dev-side breakages informed this gate:
#   - PR #208's {up,down,undo,…} in feedback-loop docs
#     (broke Cloudflare Pages for ~12h until PR #213 fixed it)
#   - the {up,down} sequence flagged again mid-PR #213 review
#
# See §4.20 of docs/v0_1_readiness.md.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Optional: a set of file paths to scan explicitly (used by CI to
# scan just the changed files in a PR). When empty, scan all synced
# paths.
FILES_ARG=("$@")

python3 - "${FILES_ARG[@]}" <<'PY'
import glob
import os
import re
import sys

SYNCED_GLOBS = [
    'docs/adr/**/*.md',
    'docs/install/**/*.md',
    'docs/roadmap.md',
]

def is_synced_path(path: str) -> bool:
    for g in SYNCED_GLOBS:
        for match in glob.iglob(g, recursive=True):
            if os.path.samefile(match, path):
                return True
    return False

def scan(path: str):
    """Return a list of (lineno, col, kind, snippet) hazards."""
    with open(path) as f:
        content = f.read()
    # Strip fenced code blocks first (multi-line, may nest inline).
    content = re.sub(r'```.*?```', lambda m: '\n' * m.group(0).count('\n'), content, flags=re.DOTALL)
    # Strip inline code spans (single-backtick, may span lines).
    content = re.sub(r'`[^`]+`', lambda m: ' ' * len(m.group(0)) if '\n' not in m.group(0) else '\n' * m.group(0).count('\n'), content, flags=re.DOTALL)
    # Strip autolinks — <http(s)://…> is valid markdown syntax, not JSX.
    content = re.sub(r'<https?://[^>\s]+>', '', content)

    hz = []
    for lineno, line in enumerate(content.split('\n'), 1):
        for m in re.finditer(r'\{[a-zA-Z_]', line):
            hz.append((lineno, m.start(), '{-prefix', line.rstrip()))
        for m in re.finditer(r'<[a-zA-Z]', line):
            hz.append((lineno, m.start(), '<-prefix', line.rstrip()))
    return hz


explicit = [p for p in sys.argv[1:] if p]
if explicit:
    # CI-mode: only scan the paths listed on argv (typically the PR's
    # changed files). Filter to synced paths so unrelated docs edits
    # don't trigger the gate on their own — the gate's job is to
    # protect the docs site build, not to police every markdown file.
    paths = [p for p in explicit if is_synced_path(p)]
else:
    seen = set()
    paths = []
    for g in SYNCED_GLOBS:
        for match in sorted(glob.iglob(g, recursive=True)):
            if match not in seen:
                seen.add(match)
                paths.append(match)

total_hazards = 0
for path in paths:
    try:
        hz = scan(path)
    except (FileNotFoundError, IsADirectoryError):
        continue
    for lineno, col, kind, snippet in hz:
        print(f'{path}:{lineno}:{col}: {kind}: {snippet[:160]}', file=sys.stderr)
        total_hazards += 1

if total_hazards == 0:
    print(f'mdx-hazard-check: 0 hazards in {len(paths)} synced-to-Astro path(s).')
    sys.exit(0)

print(f'\nmdx-hazard-check: {total_hazards} hazard(s) found across {len(paths)} synced path(s).', file=sys.stderr)
print('Wrap braced identifiers ({name}) or angle-bracketed identifiers (<name>) in inline code (`{name}` / `<name>`).', file=sys.stderr)
sys.exit(1)
PY
