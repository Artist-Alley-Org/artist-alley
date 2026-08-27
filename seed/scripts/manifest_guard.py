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
    CORRUPTED_MEASUREMENT
                    both non-empty and different, the key is a
                    MEASUREMENT of the bytes, and the destination is the
                    only thing that measured them. LOSS. See below.

MEASUREMENTS ARE NOT EDITS (#1312)
----------------------------------
ADR 0097 splits authority in two, and the guard used to implement only
half of it:

    the profile is authoritative for CONTENT
    the produced artifact is authoritative for MEASUREMENTS

A title, a licence, a field value are content: the profile decides, and a
disagreement is an edit. `file_size_bytes` and `metadata.sha256` are not
opinions — they describe bytes, and the side that actually weighed the
bytes is right. A profile carrying a stale number publishes over a
destination carrying a true one, the guard says "would change: 1 value",
and nobody looks. Sprint 14 shipped 86 wrongly-"corrected" byte counts
that way.

⛔⛔ BUT THE VERDICT IS PER RECORD CLASS, NEVER PER FIELD NAME.
`source_root` says where a record's bytes come from, and that decides
which side did the weighing:

  MEASURABLE_ROOTS ("site", "torrent_import", "internet")
      The bytes are staged AT the destination. There is no reproducible
      source to re-derive them from, so the destination IS the artifact
      and a source that disagrees is stale. CORRUPTED_MEASUREMENT, LOSS.

  SOURCE_BACKED_ROOTS ("local", "hq", "pack")
      The bytes are copied from a source the profile is built against —
      a kenney-hq pool render, a local file. The SHARE can lag that
      source, so a disagreement means "the published copy is old", not
      "the record is wrong". CHANGED_VALUE, reported, never refused.

      Measured 2026-08-27 on a freshly built pool: all 656 of site_b's
      `hq` records match the PROFILE, and the share matches on only 264.
      Adopting the destination's number for the other 392 would have
      corrupted a correct profile — which is what makes this per-class
      rather than per-field.

⛔ AND ONE FIELD CHANGES CLASS WITH THE RECORD.
`metadata.sha256` is a measurement on an `hq` or `site` record and
IDENTITY on an `internet` one: `sanitize_and_assemble.py:1828` mints the
asset id as `stable_uuid("asset", "internet", sha256)` and derives three
timestamps from it. It is the hash of the DOWNLOAD, not of the shipped
cut, so it is not describing the destination's bytes at all. Treating it
as a measurement there would refuse the edit that legitimately moves an
id. A single rule over the field NAME gets one of the two wrong.

⭐ THE VERDICT IS PER FIELD, NOT PER RECORD. A record may carry a
corrupted measurement AND a legitimate edit at once; the measurement is
refused and the edit still passes.

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
CHANGED_VALUE = "CHANGED_VALUE"
CORRUPTED_MEASUREMENT = "CORRUPTED_MEASUREMENT"

# Roots whose bytes are staged at the destination with no reproducible
# source to re-derive them from. Kept in step with
# `measure_staged.MEASURABLE_ROOTS`, which is the tool that produces the
# corrections this guard refuses to publish over.
MEASURABLE_ROOTS = frozenset({"site", "torrent_import", "internet"})

# Roots copied from a source the profile is built against. The published
# copy may lag it, so the SOURCE is authoritative and a disagreement is a
# stale publish rather than a wrong record.
SOURCE_BACKED_ROOTS = frozenset({"local", "hq", "pack"})

# Keys that describe bytes rather than state an opinion about them.
# `metadata.origin_bytes` is what `metadata.media_url` serves; it too is
# measured, not chosen.
MEASUREMENT_KEYS = frozenset({"file_size_bytes", "metadata.sha256",
                              "metadata.origin_bytes"})

# ⛔ `metadata.sha256` on an `internet` record is the hash of the
# DOWNLOAD, and the asset id is derived from it
# (`sanitize_and_assemble.py:1828`). It describes neither the shipped
# bytes nor an opinion about them: it is identity, and identity edits are
# the profile's to make.
IDENTITY_NOT_MEASUREMENT = frozenset({("internet", "metadata.sha256")})


def _roots_of(src: dict, dst: dict) -> set[str]:
    """The record's class, as either side declares it.

    Both sides are consulted deliberately. `source_root` is content, so
    the source may legitimately be correcting it — but a guard that
    guessed wrong here would either refuse a real edit or wave through a
    real corruption. Taking the union means a disagreement can only ever
    ADD a refusal, never hide one, and a refusal is the recoverable
    mistake.
    """
    return {str(r.get("source_root") or "local") for r in (src, dst)}


def classify_change(key: str, src: dict, dst: dict) -> str:
    """CHANGED_VALUE or CORRUPTED_MEASUREMENT for one differing key.

    ⛔ Per record CLASS, not per field name — see the module docstring.
    A key that is not a measurement at all is an edit whatever the root
    says, which is why the cheap test comes first.
    """
    if key not in MEASUREMENT_KEYS:
        return CHANGED_VALUE
    roots = _roots_of(src, dst)
    if any((root, key) in IDENTITY_NOT_MEASUREMENT for root in roots):
        return CHANGED_VALUE
    if roots & MEASURABLE_ROOTS:
        return CORRUPTED_MEASUREMENT
    return CHANGED_VALUE


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
        return len({x.record_id for x in self.losses
                    if x.kind not in (MISSING_RECORD, CORRUPTED_MEASUREMENT)})

    @property
    def stripped(self) -> list[Loss]:
        """Losses that BLANK something: a dropped key or an emptied value."""
        return [x for x in self.losses
                if x.kind not in (MISSING_RECORD, CORRUPTED_MEASUREMENT)]

    @property
    def corruptions(self) -> list[Loss]:
        """Losses that OVERWRITE a true measurement with a stale one."""
        return [x for x in self.losses if x.kind == CORRUPTED_MEASUREMENT]


def _record_change(rid: str, key: str, dval: Any, src: dict, dst: dict,
                   out: Comparison) -> None:
    """File one both-sides-non-empty disagreement as an edit or a loss.

    ⭐ Called per KEY, so a record carrying a corrupted measurement AND a
    legitimate edit has each judged on its own: the measurement lands in
    `losses` and refuses the run, the edit lands in `changes` and passes.
    """
    kind = classify_change(key, src, dst)
    if kind == CORRUPTED_MEASUREMENT:
        out.losses.append(Loss(CORRUPTED_MEASUREMENT, rid, key, dval))
    else:
        out.changes.append(Loss(CHANGED_VALUE, rid, key, dval))


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
                    _record_change(rid, f"{key}.{k2}", d2, src, dst, out)
            continue
        if is_empty(sval) and not is_empty(dval):
            out.losses.append(Loss(EMPTIED_VALUE, rid, key, dval))
        elif sval != dval:
            _record_change(rid, key, dval, src, dst, out)


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
    stripped = cmp.stripped
    if cmp.records_lost or stripped:
        lines.append(f"    ⛔ WOULD LOSE: {cmp.records_lost} record(s) deleted, "
                     f"{cmp.records_degraded} record(s) stripped of "
                     f"{len(stripped)} value(s)")
        shown = [x for x in cmp.losses if x.kind != CORRUPTED_MEASUREMENT]
        for x in shown[:sample]:
            lines.append(f"       {x}")
        if len(shown) > sample:
            lines.append(f"       … and {len(shown) - sample} more")
    corrupted = cmp.corruptions
    if corrupted:
        # Named separately because the remedy is different: a stripped
        # value is carried back into the profile, a stale measurement is
        # RE-MEASURED. Reporting them as one number invites the wrong fix.
        lines.append(f"    ⛔ WOULD OVERWRITE A MEASUREMENT with a stale one: "
                     f"{len(corrupted)} value(s) across "
                     f"{len({x.record_id for x in corrupted})} record(s)")
        for x in corrupted[:sample]:
            lines.append(f"       {x} (destination measured {x.dest_value!r})")
        if len(corrupted) > sample:
            lines.append(f"       … and {len(corrupted) - sample} more")
        lines.append("       Fix: python3 seed/scripts/measure_staged.py emit "
                     "--profile <profile> --site <site> --out "
                     "seed/upgrades/staged-measurements.<site>.json")
    if cmp.added:
        lines.append(f"    would add: {len(cmp.added)} record(s)")
    if cmp.changes:
        lines.append(f"    would change: {len(cmp.changes)} value(s) "
                     "(both sides non-empty — an edit, not a loss)")
    if cmp.ok:
        lines.append("    ✅ nothing at the destination would be lost")
    return "\n".join(lines)
