#!/usr/bin/env python3
"""
Refuse to publish a profile that is POORER than the site it overwrites.

THE BUG THIS GUARDS AGAINST (#1275)
-----------------------------------
`populate_archive.py` copies the profile straight over the site's
`MANIFEST.json`, and regenerates the per-site `metadata.csv` from the
profile's path map. The per-site files are OUTPUTS, so whatever the
profile says wins — including when the profile says LESS.

That is not hypothetical. Measured on 2026-08-26, the committed
`studio-a.assets.json` was behind `site_a/MANIFEST.json` by:

    * 1 asset absent entirely (`0407bb0c…`, "The Great Wave — rotated
      scan"), which a run would have DELETED from the published dataset;
    * 10,145 `field_values` keys across 1,947 records — 100 of them
      losing their field values completely;
    * 1,947 `mature` flags, 160 `file_size_bytes` and 10 `metadata.sha256`
      values, the last two of which are the ones that match the bytes
      actually on disk.

Nothing compared the two before writing, and the loss lands in a
PUBLISHED dataset. `apply_upgrade.py` documents the same shape from the
other direction: a per-site file edited by hand is undone by the next
run, so the profile is the thing that has to be correct — and this
module is what refuses to proceed when it is not.

WHAT COUNTS AS "THE DESTINATION IS AHEAD"
-----------------------------------------
A missing key, an emptied value and a DIFFERENT value are three cases,
not one, and only the first two are losses:

    MISSING_RECORD  an id the destination has and the source does not.
                    Publishing deletes it. LOSS.
    MISSING_KEY     a key the destination's record has and the source's
                    does not. Publishing drops it. LOSS.
    EMPTIED_VALUE   both have the key; the destination's is non-empty and
                    the source's is empty (None, "", [], {}).
                    Publishing blanks it. LOSS.
    CHANGED_VALUE   both non-empty and different. NOT a loss — this is
                    what an edit looks like, and the profile is the
                    source of truth for edits. Reported, never refused.

⛔ Absence of the whole comparison is not permission. A destination with
no MANIFEST.json yet is a first publish and compares clean; a
destination whose MANIFEST.json cannot be PARSED is refused, because
"unreadable" must not be quietly treated as "empty".

DUPLICATE IDS
-------------
A source holding two records under one id is refused before any
comparison. The destination file cannot represent both, the seeder reads
whichever it encounters last, and the two disagree — `studio-a.posts.json`
carries eight such ids where one says "Props sprint roundup — 8 drops"
and its twin says "— 10 drops". Publishing that is not a loss so much as
a coin toss, and a coin toss over published data is worse.
"""

from __future__ import annotations

import json
from collections import Counter
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

# Keys whose values are dicts worth descending into. The losses this
# guard exists to catch live one level down (`field_values`), so a
# whole-value comparison would report "changed" and let them through.
NESTED_KEYS = ("field_values", "metadata")

MISSING_RECORD = "MISSING_RECORD"
MISSING_KEY = "MISSING_KEY"
EMPTIED_VALUE = "EMPTIED_VALUE"


def is_empty(v: Any) -> bool:
    """Empty for the purpose of "did publishing blank this".

    `False` and `0` are values, not emptiness — `mature: false` is a
    declaration and losing it is a loss.
    """
    return v is None or v == "" or v == [] or v == {}


@dataclass
class Loss:
    kind: str
    record_id: str
    key: str = ""
    dest_value: Any = None

    def __str__(self) -> str:
        if self.kind == MISSING_RECORD:
            return f"{MISSING_RECORD} {self.record_id}"
        return f"{self.kind} {self.record_id} .{self.key}"


@dataclass
class Comparison:
    label: str
    n_source: int = 0
    n_dest: int = 0
    losses: list[Loss] = field(default_factory=list)
    changes: list[Loss] = field(default_factory=list)
    added: list[str] = field(default_factory=list)
    duplicates: dict[str, int] = field(default_factory=dict)

    @property
    def ok(self) -> bool:
        return not self.losses and not self.duplicates

    @property
    def records_lost(self) -> int:
        return sum(1 for x in self.losses if x.kind == MISSING_RECORD)

    @property
    def records_degraded(self) -> int:
        return len({x.record_id for x in self.losses if x.kind != MISSING_RECORD})


def _compare_record(rid: str, src: dict, dst: dict, out: Comparison) -> None:
    for key, dval in dst.items():
        if key not in src:
            if not is_empty(dval):
                out.losses.append(Loss(MISSING_KEY, rid, key, dval))
            continue
        sval = src[key]
        if key in NESTED_KEYS and isinstance(dval, dict) and isinstance(sval, dict):
            for k2, d2 in dval.items():
                if k2 not in sval:
                    if not is_empty(d2):
                        out.losses.append(Loss(MISSING_KEY, rid, f"{key}.{k2}", d2))
                elif is_empty(sval[k2]) and not is_empty(d2):
                    out.losses.append(Loss(EMPTIED_VALUE, rid, f"{key}.{k2}", d2))
                elif sval[k2] != d2:
                    out.changes.append(Loss("CHANGED_VALUE", rid, f"{key}.{k2}", d2))
            continue
        if is_empty(sval) and not is_empty(dval):
            out.losses.append(Loss(EMPTIED_VALUE, rid, key, dval))
        elif sval != dval:
            out.changes.append(Loss("CHANGED_VALUE", rid, key, dval))


def compare(source: Iterable[dict], dest: Iterable[dict] | None,
            label: str, id_key: str = "id") -> Comparison:
    """Compare what publishing `source` would do to `dest`.

    `dest` is None when the destination file does not exist yet — a
    first publish, which cannot lose anything.
    """
    src_list = list(source)
    out = Comparison(label=label, n_source=len(src_list))

    counts = Counter(r.get(id_key) for r in src_list)
    out.duplicates = {k: n for k, n in counts.items() if n > 1 and k is not None}

    if dest is None:
        out.added = [str(r.get(id_key)) for r in src_list]
        return out

    dst_list = list(dest)
    out.n_dest = len(dst_list)
    # Last-wins on the destination side too: that is what a reader of the
    # file gets, so it is what publishing would replace.
    src_by_id = {r.get(id_key): r for r in src_list}
    dst_by_id = {r.get(id_key): r for r in dst_list}

    for rid, drec in dst_by_id.items():
        srec = src_by_id.get(rid)
        if srec is None:
            out.losses.append(Loss(MISSING_RECORD, str(rid)))
            continue
        _compare_record(str(rid), srec, drec, out)

    out.added = [str(r) for r in src_by_id if r not in dst_by_id]
    return out


def load_json_list(path: Path) -> list[dict] | None:
    """Read a manifest-shaped JSON list, or None if it is not there.

    ⛔ A parse failure RAISES. "Unreadable" is not "empty": treating a
    truncated MANIFEST.json as an empty destination would make the guard
    wave through exactly the run that most needs stopping.
    """
    if not path.is_file():
        return None
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        # Re-raised with the path attached. The bare decoder error is
        # "Expecting value: line 1 column 13", which names neither the
        # file nor what was being attempted.
        raise ValueError(f"{path}: unreadable ({e})") from e
    if not isinstance(data, list):
        raise ValueError(f"{path}: expected a list of records, got {type(data).__name__}")
    return data


def format_report(cmp: Comparison, sample: int = 12) -> str:
    lines = [f"  {cmp.label}: source {cmp.n_source:,} record(s), "
             f"destination {cmp.n_dest:,}"]
    if cmp.duplicates:
        extra = sum(n - 1 for n in cmp.duplicates.values())
        lines.append(f"    ⛔ DUPLICATE IDS in the source: {len(cmp.duplicates)} id(s), "
                     f"{extra} extra record(s)")
        for rid, n in list(cmp.duplicates.items())[:sample]:
            lines.append(f"       {rid} ×{n}")
        if len(cmp.duplicates) > sample:
            lines.append(f"       … and {len(cmp.duplicates) - sample} more")
    if cmp.losses:
        lines.append(f"    ⛔ WOULD LOSE: {cmp.records_lost} record(s) deleted, "
                     f"{cmp.records_degraded} record(s) stripped of "
                     f"{len(cmp.losses) - cmp.records_lost} value(s)")
        for x in cmp.losses[:sample]:
            lines.append(f"       {x}")
        if len(cmp.losses) > sample:
            lines.append(f"       … and {len(cmp.losses) - sample} more")
    if cmp.added:
        lines.append(f"    would add: {len(cmp.added)} record(s)")
    if cmp.changes:
        lines.append(f"    would change: {len(cmp.changes)} value(s) "
                     "(both sides non-empty — an edit, not a loss)")
    if cmp.ok:
        lines.append("    ✅ nothing at the destination would be lost")
    return "\n".join(lines)
