#!/usr/bin/env bash
# scripts/check-mdx-hazards.sh
#
# Scans docs/**/*.md paths that get synced into the Astro/MDX docs
# site for prose patterns that MDX parses as JSX and fail the build:
#
#   - bare `{[a-zA-Z_]`  -> parsed as a JSX expression start
#   - bare `<` that is NOT followed by a space, tab or line ending
#     -> parsed as a JSX tag start
#
# Both are ONLY hazards when they appear in prose. Anything inside
# a triple-backtick fenced block or a single-backtick inline code
# span (including multi-line spans) is fine, as is an escaped `\<`
# and the HTML entity `&lt;`. Autolinks like <https://...> are also
# skipped (see the caveat below).
#
# The scanner exits non-zero + prints file:line: <pattern>: <line>
# for each real hazard, so CI can fail the PR + the operator sees
# where to fix.
#
# Scope: only the paths that actually feed the docs site build
# (docs/adr/**/*.md, docs/install/**/*.md, docs/roadmap.md). Other
# docs/*.md files stay unfiltered - they're plain reference files
# that no site build reads.
#
# ── Why the `<` rule is shaped this way ───────────────────────────
#
# The rule used to be `<[a-zA-Z]`, on the assumption that MDX only
# treats `<` as a tag start when a name follows it. That is wrong,
# and it let a real break through: sprint 18b landed the prose
# `bounds A <= B` in ADR 0093 and this scanner reported 0 hazards,
# while the site's `Build verify` (run 33286001571) died with
#
#   [@mdx-js/rollup] Unexpected character `=` (U+003D) before name,
#   expected a character that can start a name ...
#   .../adr/0093-browse-and-search-compose-one-query.mdx:465:31
#
# The rule below is not a guess. It mirrors the one branch in the
# MDX tokenizer that decides whether a `<` opens a tag -
# `startAfter()` in micromark-extension-mdx-jsx/lib/factory-tag.js:
#
#     // Deviate from JSX, which allows arbitrary whitespace.
#     if (markdownLineEndingOrSpace(code)) {
#       return nok(code)
#     }
#
# So, measured against @mdx-js/mdx 3.1.1 (the compiler @astrojs/mdx
# 4.3.x uses):
#
#   `A < B`   compiles - a space after `<` bails out of tag parsing,
#             leaving a literal `<`. Same for a tab and for a `<`
#             at end of line. This is where MDX deliberately parts
#             company with JSX.
#   `A <= B`  FAILS  - `=` cannot start a name.
#   `1<2`     FAILS  - nor can a digit.
#   `I <3`    FAILS, `a <- b` FAILS, `<!-- c -->` FAILS, `a <_b`
#             FAILS, `a <. b` FAILS ... every non-whitespace
#             follower enters tag parsing and then errors unless a
#             complete, well-formed element follows.
#
# Hence: hazard = `<` + anything that is not space/tab/CR/LF. That
# is strictly broader than the old `<[a-zA-Z]`, so everything the
# old rule caught is still caught, and `A < B`-style prose stays
# legal because the parser genuinely accepts it.
#
# Caveat on autolinks: `<https://example.com>` is valid CommonMark
# but is NOT valid MDX - the compiler reads `https:` as a namespaced
# tag name and chokes on the `/`. This scanner still skips them,
# preserving long-standing behaviour; the only autolink in the
# synced corpus lives in docs/install/README.md, which the site
# consumes as a config partial rather than rendering as an MDX page.
# Do not add new autolinks to ADRs.
#
# Two prior dev-side breakages informed the brace half of this gate:
#   - PR #208's {up,down,undo,...} in feedback-loop docs
#     (broke Cloudflare Pages for ~12h until PR #213 fixed it)
#   - the {up,down} sequence flagged again mid-PR #213 review
#
# See §4.20 of docs/v0_1_readiness.md.
#
# Usage:
#   scripts/check-mdx-hazards.sh                 # scan every synced path
#   scripts/check-mdx-hazards.sh FILE...         # scan just these (CI mode)
#   scripts/check-mdx-hazards.sh --unfiltered F  # skip the synced-path
#                                                # filter (test fixtures)

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

# `<` opens a JSX tag unless the next character is markdown
# whitespace. See the header comment for the tokenizer branch this
# mirrors. `\r` is here so a CRLF file's line-final `<` is treated
# the way the parser treats it: as a line ending, not a name.
JSX_SAFE_FOLLOWERS = ' \t\r'


def is_synced_path(path: str) -> bool:
    for g in SYNCED_GLOBS:
        for match in glob.iglob(g, recursive=True):
            if os.path.samefile(match, path):
                return True
    return False


def is_escaped(line: str, idx: int) -> bool:
    """True when the char at idx is preceded by an odd run of backslashes."""
    n = 0
    j = idx - 1
    while j >= 0 and line[j] == '\\':
        n += 1
        j -= 1
    return n % 2 == 1


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
    lines = content.split('\n')
    for lineno, line in enumerate(lines, 1):
        for m in re.finditer(r'\{[a-zA-Z_]', line):
            hz.append((lineno, m.start(), '{-prefix', line.rstrip()))
        for m in re.finditer(r'<', line):
            i = m.start()
            if is_escaped(line, i):
                continue
            nxt = line[i + 1] if i + 1 < len(line) else '\n'
            if nxt in JSX_SAFE_FOLLOWERS or nxt == '\n':
                # `< ` / `<\t` / `<` at end of line: the tokenizer
                # bails out of tag parsing and emits a literal `<`.
                # The one exception is a `<` at true end-of-file,
                # which errors with "unexpected end of file before
                # name", handled below.
                if not (nxt == '\n' and lineno == len(lines) and i + 1 >= len(line)):
                    continue
            hz.append((lineno, i, '<-prefix', line.rstrip()))
    return hz


explicit = []
unfiltered = False
for arg in sys.argv[1:]:
    if not arg:
        continue
    if arg == '--unfiltered':
        unfiltered = True
        continue
    explicit.append(arg)

if explicit:
    # CI-mode: only scan the paths listed on argv (typically the PR's
    # changed files). Filter to synced paths so unrelated docs edits
    # don't trigger the gate on their own — the gate's job is to
    # protect the docs site build, not to police every markdown file.
    # `--unfiltered` bypasses that filter so the scanner's own
    # regression fixtures (which live outside docs/) can be scanned.
    paths = explicit if unfiltered else [p for p in explicit if is_synced_path(p)]
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
print('Wrap braced identifiers ({name}), angle-bracketed identifiers (<name>) and', file=sys.stderr)
print('comparison expressions (A <= B, size < A, 1<2) in inline code:', file=sys.stderr)
print('`{name}`, `<name>`, `A <= B`. Or escape the bracket as \\< / &lt;.', file=sys.stderr)
sys.exit(1)
PY
