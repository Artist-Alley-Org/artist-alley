#!/usr/bin/env python3
"""
The one punctuation rule every seed title goes through (#1319).

Owner ruling, 2026-09-25/26: an asset title holds no comma and no em
dash, and every separator the pipeline generates in any title is an
ASCII hyphen. So a comma or em dash becomes " - " when it touches
whitespace on either side, and a bare "-" when it touches none:

    "Sintel \u2014 full film"            -> "Sintel - full film"
    "Sintel , full film"               -> "Sintel - full film"
    "Sono Variablefont Mono,wght"      -> "Sono Variablefont Mono-wght"

WHY A MODULE OF ITS OWN. Six writers store an asset title and they live
in four scripts: `sanitize_and_assemble` (local, torrent, internet),
`pexels_gameplay`, `studio_balance` and `apply_upgrade`.
`sanitize_and_assemble` already imports `apply_upgrade`, so the rule
cannot live in either of those without an import cycle, and a second
copy is exactly how two writers come to disagree. This module imports
nothing from the seed tree, so every writer can take the rule from here.

WHAT IT IS NOT. It is not the post-title rule. A comma an author wrote
in a post title is legal (`Notes, sketches and studies`), so the post
path only ever calls this with `separators=EM_DASH`
(`sanitize_and_assemble.clean_dashes`), which cannot touch a comma.
Descriptions, tags and other free text never go through it.
"""

from __future__ import annotations

EM_DASH = "\u2014"

# The characters an asset title may not hold. `apply_upgrade.merge_added`
# refuses a newly merged record carrying one, rather than normalising it:
# an upgrade document is historical evidence and is not rewritten.
TITLE_SEPARATORS = "," + EM_DASH


def normalize_title(text: str, separators: str = TITLE_SEPARATORS) -> str:
    """Replace every separator in `text` with an ASCII hyphen.

    Each comma and each em dash is its own separator and is judged on its
    own: one that touches whitespace on either side becomes " - " and takes
    the whitespace it touches with it, so no double space can form; one
    that touches none becomes "-". So "a,,b" is two separators and reads
    "a--b". Idempotent: the output holds none of `separators`, so a second
    pass finds nothing to do.
    """
    out = ""
    i, n = 0, len(text)
    while i < n:
        ch = text[i]
        if ch not in separators:
            out += ch
            i += 1
            continue
        spaced = ((i > 0 and text[i - 1].isspace())
                  or (i + 1 < n and text[i + 1].isspace()))
        if not spaced:
            out += "-"
            i += 1
            continue
        # The whitespace before it is already in `out` (copied verbatim, or
        # the trailing space of a spaced separator just before it); the
        # whitespace after it is skipped. Either way it becomes one space.
        out = out.rstrip() + " - "
        i += 1
        while i < n and text[i].isspace():
            i += 1
    return out


def has_title_separator(text: str) -> bool:
    """True when `text` holds a character an asset title may not."""
    return any(ch in text for ch in TITLE_SEPARATORS)
