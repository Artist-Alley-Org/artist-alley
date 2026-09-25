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

  MEASURABLE_ROOTS ("site", "torrent_import", "internet", "local")
      The bytes are staged AT the destination. There is no reproducible
      source to re-derive them from, so the destination IS the artifact
      and a source that disagrees is stale. CORRUPTED_MEASUREMENT, LOSS.

      ⛔ `local` MOVED HERE IN #1319, and it is a change of AUTHORITY.
      Its source dataset has been permanently retired and the published
      archive is now the maintained copy; 0 of 696 site_a and 0 of 552
      site_b `local` records carry a media_url or a source_archive, so
      there is nothing left to re-derive one from.

  SOURCE_BACKED_ROOTS ("hq", "pack")
      The bytes are copied from a source the profile is built against —
      a kenney-hq pool render, an attested pack member. The SHARE can lag
      that source, so a disagreement means "the published copy is old",
      not "the record is wrong". CHANGED_VALUE, reported, never refused.

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

MIGRATED IDS ARE NOT DELETED RECORDS (#1319)
---------------------------------------------
Two migrations (#1293, #1310, ADR 0098) moved 511 post ids onto values
derived from the post's own content. The published wall still holds the
OLD ids, so a plain comparison reads every one of them as a record the
profile deleted: 175 MISSING_RECORD on site_a and 336 on site_b, none of
them a loss. The pipeline wrote a reconciliation document for exactly
this reader, `seed/upgrades/post-id-migration.<stem>.json`, and until
this section nothing read it.

    MIGRATED_RECORD a destination id that is a recorded `old_id` whose
                    `new_id` is present in the source. NOT a loss. The
                    record is compared against its new self with the
                    identity key excluded, so a value carried across
                    the move is still guarded: MISSING_KEY and
                    EMPTIED_VALUE across a move refuse exactly as they
                    do on an unmoved record, and CHANGED_VALUE stays
                    report-only.

The evidence is the committed document and nothing else. A move is
never inferred from a title, a member set or a resemblance; an id the
document does not record stays MISSING_RECORD. And the document is
validated one-to-one before any comparison, fail-closed: unparseable,
a missing or malformed move, a `profile` that does not name the file
being guarded, one old id recorded twice, two old ids landing on one
new id, an id on both sides (an uncomposed chain), a new id the source
does not hold, or an old id the source still holds. Every one of those
refuses, none is overridable, and the message names the document and
the ids. A document that is absent is not an error: it is simply no
evidence, and the plain comparison runs.

A SOURCE-AUTHENTICATED RETIREMENT IS NOT A DELETED RECORD (#1319)
-----------------------------------------------------------------
Two catalogue records can describe the same produced bytes owned by one
user. The app's identity is `(owner_user_ref, file_hash)` and no
`DedupBehavior` value relaxes it, so one of the two can never exist: it
gets no row, no field values, and the post naming it silently loses a
member. `asset_collapse.py` retires the loser by document, naming one
survivor and enumerating every value the retirement costs.

    COLLAPSED_RECORD  a destination id a validated collapse document
                      retires onto a survivor the source holds. NOT a
                      loss. Its own report line, never folded into
                      losses, additions, changes or MIGRATED_RECORD.

⛔ IT IS PRODUCED ONLY UNDER LAYER B, AND THIS MODULE CANNOT PRODUCE IT
ALONE. `compare` accepts a mapping of retirements the CALLER has already
source-authenticated: `populate_archive.py` locates both produced files
under the source root the record names, hashes them, and requires them
equal to each other within one build and equal to the document's
`materialized_sha256`. The comparison here adds the last check, which is
the only one that needs the destination: the record standing at the
retired id must equal the document's verbatim `retired_record` on every
key, nested included. A stale or altered predecessor is MISSING_RECORD,
because a document that describes a record the destination no longer
holds is not evidence about the record it does hold.

A document that is absent is not an error and not permission: it is
simply no evidence, and every destination-only id stays MISSING_RECORD.

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
from typing import Any, Iterable, Mapping

# Keys whose values are dicts worth descending into. The losses this
# guard exists to catch live one level down (`field_values`), so a
# whole-value comparison would report "changed" and let them through.
NESTED_KEYS = ("field_values", "metadata")

MISSING_RECORD = "MISSING_RECORD"
MISSING_KEY = "MISSING_KEY"
EMPTIED_VALUE = "EMPTIED_VALUE"
CHANGED_VALUE = "CHANGED_VALUE"
CORRUPTED_MEASUREMENT = "CORRUPTED_MEASUREMENT"
MIGRATED_RECORD = "MIGRATED_RECORD"
COLLAPSED_RECORD = "COLLAPSED_RECORD"

# The committed reconciliation documents live beside the profiles:
#   <root>/profiles/<stem>.posts.json  ->  <root>/upgrades/post-id-migration.<stem>.json
# Repository-owned state, located from the posts file alone, so the
# existing `--posts <profile>` invocation needs no new argument.
MIGRATION_DOCUMENT_PREFIX = "post-id-migration."
POSTS_PROFILE_SUFFIX = ".posts.json"

# Roots whose bytes are staged at the destination with no reproducible
# source to re-derive them from. Kept in step with
# `measure_staged.MEASURABLE_ROOTS`, which is the tool that produces the
# corrections this guard refuses to publish over.
#
# ⛔ `local` JOINED THIS SET (#1319), AND THAT IS A CHANGE OF AUTHORITY, NOT
# A TIGHTENING. The source dataset `local` was copied from
# (`/mnt/d/Projects/unraid_management/artist-alley_dataset`) has been
# permanently retired and no longer exists, and the maintained datasets are
# the published trees under `/mnt/blackbox_archives/datasets/artist_alley`.
# Measured on the committed profiles: of 696 site_a and 552 site_b `local`
# records, ZERO carry `metadata.media_url` and ZERO carry
# `metadata.source_archive`, so there is nothing left to re-derive a
# `local` byte count FROM. A disagreement there is therefore a corrupted
# measurement and a refusal, not "the share is stale".
MEASURABLE_ROOTS = frozenset({"site", "torrent_import", "internet", "local"})

# Roots copied from a source the profile is built against. The published
# copy may lag it, so the SOURCE is authoritative and a disagreement is a
# stale publish rather than a wrong record.
#
# ⛔ `hq` AND `pack` STAY HERE, AND THE REASON IS MEASURED, NOT PREFERRED.
# `hq` rebuilds from the Kenney pack through the committed
# `seed/upgrades/kenney-hq-pool.json`; `pack` copies or extracts members
# verified against `metadata.source_archive.sha256`, which 378 of 378
# site_a `pack` records carry. And the published trees ALREADY disagree
# with the profiles on `file_size_bytes` for 1 record (site_a) and 392
# (site_b). Every single one of them is `hq`, because the share holds an
# older pool build. Reclassifying `hq` would turn all 393 into
# CORRUPTED_MEASUREMENT and refuse every publish. Kept in step with
# `asset_collapse.PRODUCED_SOURCE_ROOTS`.
SOURCE_BACKED_ROOTS = frozenset({"hq", "pack"})

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
    # (old_id, new_id) for every destination record the migration
    # document accounts for. Never a loss, never folded into `added`
    # or `changes`; `migration_source` names the document for the report.
    migrated: list[tuple[str, str]] = field(default_factory=list)
    migration_source: str = ""
    # (retired_id, survivor_id) for every destination record a
    # source-authenticated collapse document accounts for. Never a loss,
    # never folded into `added`, `changes` or `migrated`;
    # `collapse_source` names the document for the report.
    collapsed: list[tuple[str, str]] = field(default_factory=list)
    collapse_source: str = ""

    @property
    def ok(self) -> bool:
        return not self.losses and not self.duplicates

    @property
    def records_migrated(self) -> int:
        return len(self.migrated)

    @property
    def records_collapsed(self) -> int:
        return len(self.collapsed)

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


def _compare_record(rid: str, src: dict, dst: dict, out: Comparison,
                    skip: frozenset[str] = frozenset()) -> None:
    """Judge every key of the destination record against the source's.

    `skip` holds the keys that are NOT compared. It carries exactly one
    thing today: the identity key of a record whose id the migration
    document moved, because the id transition is the migration itself
    and reporting it as CHANGED_VALUE would say a move is an edit.
    Nothing else is ever excluded; a value carried across a move is
    guarded by the same rules as a value on an unmoved record.
    """
    for key, dval in dst.items():
        if key in skip:
            continue
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


def _same_record(want: Mapping[str, Any], got: Mapping[str, Any]) -> bool:
    """Whole-record equality, nested included, in both directions.

    Deliberately not `_compare_record`: that one judges a source against
    a destination and forgives a CHANGED_VALUE, which is exactly the
    tolerance a retirement must not have. Here the question is whether
    the destination holds the very record the document enumerated the
    losses of, and anything short of identical means it does not.
    """
    return dict(want) == dict(got)


def compare(source: Iterable[dict], dest: Iterable[dict] | None,
            label: str, id_key: str = "id",
            migration: "Migration | Mapping[str, str] | None" = None,
            collapses: "Mapping[str, Mapping[str, Any]] | None" = None,
            collapse_source: str = "") -> Comparison:
    """Compare what publishing `source` would do to `dest`.

    `dest` is None when the destination file does not exist yet — a
    first publish, which cannot lose anything.

    `migration` is the validated old_id -> new_id mapping for THIS
    source (a `Migration` from `load_migration_document`, or a plain
    mapping). It is checked against the source before any record is
    judged, and a mapping the source contradicts raises MigrationError
    rather than comparing: a document that names a new id the source
    does not hold is not evidence of anything. Absent, the comparison
    is the plain one and every destination-only id is MISSING_RECORD.

    `collapses` maps a retired id to `{"survivor_id", "retired_record"}`
    for every retirement the CALLER has already source-authenticated
    (Layer B: both produced files located under the root the record
    names, hashed, equal to each other in one build and equal to the
    document's `materialized_sha256`). This function adds the one check
    that needs the destination, and it is strict: the destination record
    must equal `retired_record` on every key, nested included. A stale or
    altered predecessor falls through to MISSING_RECORD.

    ⛔ Passing a mapping here is an ASSERTION that the bytes were checked.
    Nothing in this module can make that check, so nothing in this module
    may manufacture the mapping.
    """
    src_list = list(source)
    out = Comparison(label=label, n_source=len(src_list))

    counts = Counter(r.get(id_key) for r in src_list)
    out.duplicates = {k: n for k, n in counts.items() if n > 1 and k is not None}

    moves: dict[str, str] = {}
    if migration is not None:
        if isinstance(migration, Migration):
            moves = migration.moves
            out.migration_source = str(migration.path or "migration document")
        else:
            moves = dict(migration)
            out.migration_source = "migration document"
        validate_migration_against_source(
            moves, (r.get(id_key) for r in src_list), source=out.migration_source)

    if dest is None:
        out.added = [str(r.get(id_key)) for r in src_list]
        return out

    dst_list = list(dest)
    out.n_dest = len(dst_list)
    # Last-wins on the destination side too: that is what a reader of the
    # file gets, so it is what publishing would replace.
    src_by_id = {r.get(id_key): r for r in src_list}
    dst_by_id = {r.get(id_key): r for r in dst_list}

    # The identity key is excluded ONLY on a moved record, and only
    # because the move is what the document records. Every other key
    # of that record is judged exactly as on an unmoved one.
    across_move = frozenset({id_key})
    retirements = dict(collapses or {})
    if retirements:
        out.collapse_source = collapse_source or "collapse document"
    for rid, drec in dst_by_id.items():
        srec = src_by_id.get(rid)
        if srec is None:
            retired = retirements.get(rid)
            if retired is not None:
                # ⛔ The destination's record must be the one the document
                # authorises, key for key. A document that describes a
                # predecessor the destination no longer holds says nothing
                # about the record it DOES hold, and retiring on that
                # basis would delete data nobody enumerated.
                if _same_record(retired.get("retired_record") or {}, drec):
                    out.collapsed.append((str(rid), str(retired.get("survivor_id"))))
                    continue
                out.losses.append(Loss(MISSING_RECORD, str(rid)))
                continue
            new_id = moves.get(rid)
            if new_id is None:
                out.losses.append(Loss(MISSING_RECORD, str(rid)))
                continue
            # validate_migration_against_source proved new_id is in the
            # source, so this lookup cannot miss.
            out.migrated.append((str(rid), str(new_id)))
            _compare_record(str(rid), src_by_id[new_id], drec, out, skip=across_move)
            continue
        _compare_record(str(rid), srec, drec, out)

    # A migration's new id is the old record under a new name, not an
    # addition: 863 = 686 shared + 175 migrated + 2 new on site_a.
    landed = {new for _, new in out.migrated}
    out.added = [str(r) for r in src_by_id if r not in dst_by_id and r not in landed]
    return out


class MigrationError(ValueError):
    """The migration document cannot be used as evidence.

    Raised, never returned, for the same reason an unreadable
    destination raises: "unusable" must not quietly become "no
    migrations", because that would turn every moved id back into a
    deleted record and refuse a correct publish, or (with a document
    that was silently half-read) wave a real loss through.
    """


@dataclass(frozen=True)
class Migration:
    """A validated one-to-one old_id -> new_id mapping for one profile."""
    moves: dict[str, str]
    profile: str = ""
    path: Path | None = None


def migration_document_path(posts_path: Path) -> Path | None:
    """Where the committed document for a posts profile lives, or None
    when the file is not named `<stem>.posts.json`.

    `<root>/profiles/<stem>.posts.json` -> `<root>/upgrades/post-id-migration.<stem>.json`,
    which is where `migrate_post_ids.py` writes it. Derived from the
    posts path alone so the existing `--posts` argument is sufficient,
    and so a test can lay a fixture out the same way in a temp tree.
    """
    name = posts_path.name
    if not name.endswith(POSTS_PROFILE_SUFFIX):
        return None
    stem = name[:-len(POSTS_PROFILE_SUFFIX)]
    if not stem:
        return None
    return posts_path.resolve().parent.parent / "upgrades" / f"{MIGRATION_DOCUMENT_PREFIX}{stem}.json"


def parse_migration_document(data: Any, *, source: str,
                             profile_name: str | None = None) -> Migration:
    """Validate a parsed document one-to-one, fail-closed.

    Every check here is a refusal and none is overridable. The message
    names the document (`source`) and the offending ids, so the fix is
    to correct the committed document, never to skip it.
    """
    if not isinstance(data, dict):
        raise MigrationError(f"{source}: expected an object with a `moves` list, "
                             f"got {type(data).__name__}")
    profile = data.get("profile")
    if not isinstance(profile, str) or not profile:
        raise MigrationError(f"{source}: missing `profile` (the posts file it describes)")
    if profile_name is not None and profile != profile_name:
        raise MigrationError(
            f"{source}: describes profile {profile!r}, but the posts file being "
            f"guarded is {profile_name!r}; refusing to apply one profile's moves "
            f"to another")
    moves = data.get("moves")
    if not isinstance(moves, list):
        raise MigrationError(f"{source}: `moves` must be a list, got "
                             f"{type(moves).__name__}")
    mapping: dict[str, str] = {}
    targets: dict[str, str] = {}
    for i, m in enumerate(moves):
        if not isinstance(m, dict):
            raise MigrationError(f"{source}: moves[{i}] is not an object")
        old, new = m.get("old_id"), m.get("new_id")
        if not isinstance(old, str) or not old or not isinstance(new, str) or not new:
            raise MigrationError(f"{source}: moves[{i}] lacks a non-empty "
                                 f"old_id/new_id (old_id={old!r}, new_id={new!r})")
        if old == new:
            raise MigrationError(f"{source}: {old} is recorded as moving to itself")
        if old in mapping:
            if mapping[old] == new:
                raise MigrationError(f"{source}: old id {old} is recorded twice")
            raise MigrationError(
                f"{source}: old id {old} is recorded with two different targets, "
                f"{mapping[old]} and {new}")
        if new in targets:
            raise MigrationError(
                f"{source}: two old ids ({targets[new]} and {old}) both move to "
                f"{new}; a many-to-one move cannot be authenticated")
        mapping[old] = new
        targets[new] = old
    both = sorted(set(mapping) & set(targets))
    if both:
        raise MigrationError(
            f"{source}: {len(both)} id(s) appear as both old_id and new_id "
            f"(an uncomposed chain; migrate_post_ids.accumulate_moves composes "
            f"them): {both[:5]}")
    return Migration(moves=mapping, profile=profile)


def load_migration_document(path: Path, *, profile_name: str | None = None) -> Migration:
    """Read and validate a committed document. Raises MigrationError."""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        raise MigrationError(f"{path}: unreadable ({e})") from e
    m = parse_migration_document(data, source=str(path), profile_name=profile_name)
    return Migration(moves=m.moves, profile=m.profile, path=path)


def validate_migration_against_source(moves: "Mapping[str, str]",
                                      source_ids: Iterable[Any],
                                      source: str = "migration document") -> None:
    """The document must agree with the profile it describes.

    A new id the source does not hold means the document describes a
    profile this is not; an old id the source still holds means the
    migration was never applied to it. Either way the document is not
    evidence for THIS publish, and using half of it would be guessing.
    """
    ids = set(source_ids)
    absent = sorted(new for new in moves.values() if new not in ids)
    if absent:
        raise MigrationError(
            f"{source}: {len(absent)} mapped new id(s) are absent from the "
            f"source, so the document does not describe it: {absent[:5]}")
    stale = sorted(old for old in moves if old in ids)
    if stale:
        raise MigrationError(
            f"{source}: {len(stale)} old id(s) are still present in the source; "
            f"the migration it records was not applied: {stale[:5]}")


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
    if cmp.migrated:
        # Its own line, never folded into lose/add/change: a moved id is
        # the same record under the name the pipeline gave it.
        lines.append(f"    migrated: {len(cmp.migrated)} record(s) carried to a new "
                     f"id per {cmp.migration_source or 'the migration document'}")
    if cmp.collapsed:
        # ⛔ Its own line, never folded into lose/add/change/migrate. A
        # retirement is the only case where a destination record
        # legitimately has no successor under its own id, and it is
        # SOURCE-AUTHENTICATED: the produced bytes were re-derived from
        # the source roots for this run. Reporting it as anything else
        # would either read as a loss the operator must override, or
        # disappear into a number nobody inspects.
        lines.append(f"    collapsed: {len(cmp.collapsed)} record(s) retired onto a "
                     f"named survivor per {cmp.collapse_source or 'the collapse document'}")
        for rid, sid in cmp.collapsed[:sample]:
            lines.append(f"       {COLLAPSED_RECORD} {rid} -> {sid}")
        if len(cmp.collapsed) > sample:
            lines.append(f"       … and {len(cmp.collapsed) - sample} more")
    if cmp.added:
        lines.append(f"    would add: {len(cmp.added)} record(s)")
    if cmp.changes:
        lines.append(f"    would change: {len(cmp.changes)} value(s) "
                     "(both sides non-empty — an edit, not a loss)")
    if cmp.ok:
        lines.append("    ✅ nothing at the destination would be lost")
    return "\n".join(lines)
