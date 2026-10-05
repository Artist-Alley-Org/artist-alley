#!/usr/bin/env python3
"""
Retire a catalogue record that can never materialize, by document.

THE DEFECT THIS EXISTS FOR
--------------------------
Two catalogue records can describe the same bytes owned by the same
user. The app cannot hold both: asset identity is `(owner_user_ref,
file_hash)`, enforced by `idx_assets_owner_hash_unique` (app/schema.sql),
and no `DedupBehavior` value relaxes it. With the pre-check skipped the
INSERT raises 23505 and the fallback hands back the EXISTING asset
(app/internal/assets/handler.go). So one of the two records has no id, no
declaration, no size and no field values on a seeded instance, and a post
naming it silently loses a member: `SeedInsertAsset` ends in a bare
`ON CONFLICT DO NOTHING`, the seeder counts a `deduped` and continues, and
the id never enters the runner's asset map.

The catalogue produces the pair honestly. Its identity is per SOURCE
PATH: a `local` id comes from the CSV `asset_id`, a balance id from the
pack member path. Two different pack members whose bytes are identical
(here, the Blue and Grey copies of one Kenney vector, which are the same
918 bytes) therefore mint two records that materialize to one row.

THREE HASHES, NEVER CONFLATED
-----------------------------
  source_sha256        the pack archive member BEFORE any render (an SVG
                       for `hq`, the shipped file itself for `pack`).
                       Stored as `metadata.source_archive.sha256`. It
                       proves two records share an INPUT.
  materialized_sha256  the PRODUCED file the pipeline ships: the rendered
                       PNG, or the copied pack file. It proves two records
                       MATERIALIZE to one row.
  assets.file_hash     the uploaded produced file, which is the app's
                       live uniqueness key.

`source_archive.sha256` is NOT the app, content or storage hash, and a
staged PNG is never compared against an SVG member hash. The invariant
that matters is `(owner, produced_byte_sha256)`;
`(owner, source_archive.sha256, render.px)` is a cheap repository-only
PROXY of it, necessary but not sufficient, and this module never
describes the proxy as complete.

TWO KINDS OF EVIDENCE, DISCRIMINATED BY `evidence.kind` (#1319)
---------------------------------------------------------------
The hashes above describe a record whose bytes we PRODUCED. `local` has
no such bytes: the source dataset the profiles were built against has
been permanently retired, and of 696 site_a and 552 site_b `local`
records, ZERO carry `metadata.media_url` and ZERO carry
`metadata.source_archive`. A `local` retirement could only satisfy the
produced-source schema by FABRICATING a source hash, a member, a render
size and a rasteriser it never had. So the schema says which claim it is
making:

  produced_source    `hq` and `pack`. Every requirement above, unchanged:
                     source and materialized hashes that must DIFFER, a
                     member, a render size, a tool, and both
                     `retired_record` cross-checks. Layer B re-derives the
                     bytes from an external source.

  preserved_archive  `local`. A stable `retired_sha256` and
                     `survivor_sha256` which must be EQUAL. That equality
                     is the documented byte collapse. No member, no render
                     size, no tool, no source-archive claim, because there
                     is none to make. Layer B authenticates against a
                     FROZEN PRE-OPERATION SNAPSHOT attested by an external
                     manifest, never against the live or staged tree.

⛔ `preserved_archive` IS THE WEAKER CLAIM AND IS NEVER DESCRIBED AS
ANYTHING ELSE. It exists only because the owner ruled that the archive is
now the maintained dataset for `local`. Its integrity is the snapshot
manifest recomputation and nothing else; the archive agreeing with itself
is not evidence, because a stale copy agrees with itself perfectly.

⛔ `kind` IS REQUIRED AND HAS NO DEFAULT. An unlabelled entry is refused.
A default would have to choose, and choosing `produced_source` would hand
the stronger authority to an entry nobody labelled.

TWO VALIDATION LAYERS, NAMED APART
----------------------------------
  Layer A  repository-local. It runs where there is no pack, no pool and
           no dataset source: the assembler calls `apply_upgrade.py` with
           no source roots, and the required guard suite runs on a runner
           that has none.
             A1 structural, document-internal, before the document
                influences any pass. `parse_collapse_document`.
             A2 state, at the collapse stage, on the MERGED profile and
                posts. `evaluate` / `apply_collapse`.
  Layer B  publish-time SOURCE AUTHENTICATION, in `populate_archive.py`,
           using the source roots it already takes. Everything in Layer A
           re-checked, plus the two produced files located under the root
           their `source_root` names, hashed, required equal to EACH
           OTHER within one build and equal to `materialized_sha256`, plus
           the destination record compared key for key against
           `retired_record`.

⛔ LAYER A NEVER AUTHORISES A PUBLISH. It is structural and stateful, not
evidential: it proves the document describes this profile and that the
profile is in one of the two legal states. Only Layer B has seen bytes.
A report line that says otherwise is the defect this separation exists
to prevent, which is why the two layers have different names here and in
every message they print.

⚠️ WHY THE RECORDED ABSOLUTE HASH IS RE-DERIVED RATHER THAN TRUSTED.
Produced PNGs are byte-reproducible only within one sharp build
(`kenney_hq.py` documents 24 site_a files differing in exactly 8 pHYs
bytes across versions). The load-bearing claim is equality between the
two produced files IN ONE BUILD. The absolute value is recorded so a
drift is visible, and Layer B re-measures it: a mismatch REFUSES and
names re-measurement. An IDAT-only comparison may appear in a refusal
message as a diagnostic. It is never an acceptance path.

EXACTLY TWO STATES PER OBJECT, AND A THIRD IS A HARD FAILURE
------------------------------------------------------------
Per entry, the asset and each post are judged independently:

  ASSET  Pending  `retired_id` present AND structurally equal, key for
                  key and nested included, to the document's verbatim
                  `retired_record`.
         Applied  `retired_id` absent.
         Invalid  present but differing in any key. This fails BEFORE any
                  deletion, so a stale document can never delete newly
                  regenerated or changed data.
         `survivor_id` must be present in both states.

  POST   Pending  the post exists under `post_id` with membership
                  EXACTLY `old_members`, order included.
         Applied  the post exists under `new_post_id` where present else
                  `post_id`, with membership EXACTLY `new_members`, order
                  included.
         Invalid  any other membership or id state, the post being absent
                  included.

An asset may be Applied while its post is Pending, or the reverse.

⛔ WHERE STATE IS EVALUATED MATTERS. On a regenerated profile the retired
record and the affected post both ENTER through the upgrade documents
(`balance-assets`, `balance-posts`), so before those merges they are
legitimately absent and are neither Pending nor Applied. State is
therefore evaluated at the collapse stage, on the merged in-memory
profile and posts, which is the state that would otherwise be written.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable, Sequence

# `<root>/profiles/<stem>.assets.json` -> `<root>/upgrades/asset-collapse.<stem>.json`,
# mirroring how `manifest_guard.migration_document_path` locates the
# post-id migration document from the posts profile alone. Repository
# state, found from a path the callers already have.
COLLAPSE_DOCUMENT_PREFIX = "asset-collapse."
ASSETS_PROFILE_SUFFIX = ".assets.json"

PENDING = "pending"
APPLIED = "applied"

# Roots a retirement can be authenticated for at all. A pre-staged root
# ("site", "torrent_import", "internet") has no reproducible source AND no
# preserved-snapshot standing, so a retirement on one of those could never
# reach Layer B and would be a Layer-A-only claim wearing a publish tick.
# Refusing it in A1 is the honest shape.
AUTHENTICABLE_ROOTS = frozenset({"local", "hq", "pack"})

# ⛔ TWO KINDS OF EVIDENCE, AND THEY ARE NOT INTERCHANGEABLE (#1319).
#
#   produced_source    the strong claim. Bytes are re-derived from a
#                      source nobody in this pipeline controls: the Kenney
#                      pack for `hq`, the attested `source_archive` member
#                      for `pack`. Two produced files are located, hashed,
#                      and required equal to each other in one build.
#
#   preserved_archive  the WEAKER claim, accepted only because the owner
#                      ruled the archive is now the maintained dataset for
#                      `local`. There is no source left to re-derive from:
#                      of 696 site_a and 552 site_b `local` records, ZERO
#                      carry `metadata.media_url` or
#                      `metadata.source_archive`. Authority is a FROZEN
#                      PRE-OPERATION SNAPSHOT, attested by an external
#                      manifest that is recomputed immediately before use.
#
# ⛔ THE KIND IS REQUIRED AND THERE IS NO DEFAULT. A missing `kind` refuses.
# A default branch would have to pick one, and picking `produced_source`
# would hand the stronger authority to an unlabelled entry, which is
# precisely the permissive arm that makes a guard worse than none.
KIND_PRODUCED = "produced_source"
KIND_PRESERVED = "preserved_archive"
EVIDENCE_KINDS = (KIND_PRODUCED, KIND_PRESERVED)

# Which roots each kind may claim. The boundary is per ROOT and it is
# decided: `hq` and `pack` remain source-backed because they are still
# externally reproducible, and the published trees already disagree with
# the profiles on `file_size_bytes` for 1 (site_a) and 392 (site_b)
# records, every one of them `hq`. Letting a `preserved_archive` entry
# name `hq` would be a way to claim the weaker authority exactly where the
# stronger one is available. Kept in step with
# `manifest_guard.SOURCE_BACKED_ROOTS` and
# `preserved_archive.PRESERVED_ROOTS`.
PRODUCED_SOURCE_ROOTS = frozenset({"hq", "pack"})
PRESERVED_ARCHIVE_ROOTS = frozenset({"local"})

_UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
_SHA1_RE = re.compile(r"^[0-9a-f]{40}$")

_ENTRY_KEYS = ("retired_id", "survivor_id", "owner_username", "source_root",
               "retired_file_path", "evidence", "retired_record",
               "acknowledged_losses", "post_substitutions")

_POOL_KEYS = ("pack", "render_px", "rasteriser", "sharp", "node")

# The evidence keys each kind owns. ⛔ THEY ARE DISJOINT ON PURPOSE. A
# preserved entry carrying `materialized_sha256` is REFUSED rather than
# ignored: an ignored field looks like it was honoured, and an operator
# reading the document would believe a produced hash had been checked when
# nothing had. The same applies in reverse.
_PRODUCED_EVIDENCE_KEYS = ("source_sha256", "member", "render_px",
                           "materialized_sha256", "materialized_tool")
_PRESERVED_EVIDENCE_KEYS = ("retired_sha256", "survivor_sha256")


class CollapseError(ValueError):
    """The document cannot be used, or the data is in a third state.

    Raised, never returned, for the same reason `MigrationError` is: an
    unusable document must never quietly become "no collapses". That
    reading would let a stale document authorise nothing while the run
    still reports success, which is the failure shape the whole of
    ADR 0097 is about.
    """


# ---------------------------------------------------------------------------
# Document model
# ---------------------------------------------------------------------------

@dataclass(frozen=True)
class PostSubstitution:
    """One post whose membership the retirement rewrites.

    `new_post_id` is absent for a kind whose id does not derive from
    membership. `asset_group` derives its id from the source group_id
    (`stable_uuid("post", "group", gid)`), so a member swap does not move
    it; the kinds that DO derive from membership move through the
    existing chained post-id migration document instead, and this field
    is how a substitution says which case it is.
    """
    post_id: str
    old_members: tuple[str, ...]
    new_members: tuple[str, ...]
    new_post_id: str | None = None
    curation_pipeline_members: dict[str, str] | None = None

    @property
    def applied_id(self) -> str:
        return self.new_post_id or self.post_id


@dataclass(frozen=True)
class CollapseEntry:
    """One retirement, discriminated by `kind`.

    The produced-source fields and the preserved-archive fields are
    mutually exclusive and A1 has already refused any entry that mixes
    them, so a reader never has to work out which set is meaningful: ask
    `kind`.
    """
    retired_id: str
    survivor_id: str
    owner_username: str
    source_root: str
    retired_file_path: str
    kind: str
    retired_record: dict
    acknowledged_losses: tuple[dict, ...]
    # produced_source only
    source_sha256: str | None = None
    source_member: str | None = None
    render_px: int | None = None
    materialized_sha256: str | None = None
    materialized_tool: str | None = None
    # preserved_archive only. Both name FILES, and A1 requires them EQUAL:
    # that equality IS the documented byte collapse. ⚠️ This is the
    # opposite of the produced-source rule, which requires
    # `source_sha256 != materialized_sha256` because those two describe
    # DIFFERENT bytes (a pack member before a render, and the render). The
    # two rules live on different fields and must never be merged.
    retired_sha256: str | None = None
    survivor_sha256: str | None = None
    post_substitutions: tuple[PostSubstitution, ...] = ()

    @property
    def retired_source_path(self) -> str:
        return str(self.retired_record.get("source_path") or "")

    @property
    def is_preserved(self) -> bool:
        return self.kind == KIND_PRESERVED

    @property
    def retired_bytes_sha256(self) -> str:
        """The hash the file at `retired_file_path` must have before it is
        removed, whichever kind established it.

        For `produced_source` that is `materialized_sha256`: both produced
        files hash to it, the retired one included. For
        `preserved_archive` it is `retired_sha256`.
        """
        return (self.retired_sha256 if self.is_preserved
                else self.materialized_sha256)


@dataclass(frozen=True)
class CollapseDocument:
    profile: str
    pool_of_record: dict
    entries: tuple[CollapseEntry, ...] = ()
    path: Path | None = None

    @property
    def has_produced_source(self) -> bool:
        """True when at least one entry claims produced-source evidence.

        That presence is what makes `pool_of_record` load-bearing: a
        produced hash is only reproducible against the toolchain that
        produced it. A preserved-only document produces nothing, so a pool
        on one describes nothing and is refused as meaningless.
        """
        return any(e.kind == KIND_PRODUCED for e in self.entries)

    @property
    def is_preserved_only(self) -> bool:
        return bool(self.entries) and not self.has_produced_source

    @property
    def retired_ids(self) -> frozenset[str]:
        return frozenset(e.retired_id for e in self.entries)

    @property
    def survivor_ids(self) -> frozenset[str]:
        return frozenset(e.survivor_id for e in self.entries)

    @property
    def by_retired_id(self) -> dict[str, CollapseEntry]:
        return {e.retired_id: e for e in self.entries}

    @property
    def moved_post_ids(self) -> frozenset[str]:
        """Post ids a substitution MOVES to a different id.

        Only these. A substitution that keeps the post's id leaves
        nothing to exclude anywhere, and a set that quietly included them
        would suppress a legitimate merge.
        """
        return frozenset(s.post_id for e in self.entries
                         for s in e.post_substitutions if s.new_post_id)

    @property
    def retired_paths(self) -> tuple[str, ...]:
        return tuple(e.retired_file_path for e in self.entries)


EMPTY = CollapseDocument(profile="", pool_of_record={}, entries=())


# ---------------------------------------------------------------------------
# Losses: ENUMERATED, never inferred
# ---------------------------------------------------------------------------

def recompute_losses(retired_record: dict, survivor_record: dict) -> list[dict]:
    """Everything the retired record holds that does not survive.

    ⛔ THE GUARD MUST NOT DIFF THE RETIRED RECORD AGAINST THE SURVIVOR AND
    CALL THE RESULT ACCEPTABLE. `acknowledged_losses` is the enumerated
    list a human signed; this function recomputes it so the two can be
    compared for EQUALITY. A recomputation that disagrees with the
    document means the data moved under the document, and that is a
    refusal, not a re-derivation.

    The rule, matching what the collapse stage actually does: identical
    value is a no-op, conflicting value means the survivor wins and the
    retired value is lost, a key only the retired record has is dropped
    and enumerated, a key only the survivor has is kept and is not a
    loss.

    Leaf paths are dotted and compared at every depth, not just at the
    two levels `manifest_guard.NESTED_KEYS` descends: a loss buried three
    levels down is still a loss. A subtree the survivor does not hold as
    a dict is recorded whole, so the enumeration is always finite and
    deterministic.
    """
    out: list[dict] = []

    def walk(a: Any, b: Any, path: str, b_present: bool) -> None:
        if isinstance(a, dict) and isinstance(b, dict) and b_present:
            for k, v in a.items():
                sub = f"{path}.{k}" if path else k
                walk(v, b.get(k), sub, k in b)
            return
        if not b_present:
            out.append({"path": path, "retired_value": a})
        elif a != b:
            out.append({"path": path, "retired_value": a, "survivor_value": b})

    walk(retired_record, survivor_record, "", True)
    out.sort(key=lambda x: x["path"])
    return out


def losses_equal(recorded: Sequence[dict], recomputed: Sequence[dict]) -> bool:
    """Structural equality of the two enumerations, order included.

    `recompute_losses` sorts by path and A1 requires the recorded list to
    be sorted by path too, so an equal set cannot fail this over
    ordering, and an unequal one cannot pass it by being re-sorted.
    """
    return list(recorded) == list(recomputed)


# ---------------------------------------------------------------------------
# Layer A1: structural, document-internal
# ---------------------------------------------------------------------------

def collapse_document_path(profile_path: Path) -> Path | None:
    """Where the committed document for an assets profile lives, or None
    when the file is not named `<stem>.assets.json`.

    Used by the publish guard and the site verifier, which know the
    profile path and nothing about an upgrades directory.
    """
    name = profile_path.name
    if not name.endswith(ASSETS_PROFILE_SUFFIX):
        return None
    stem = name[:-len(ASSETS_PROFILE_SUFFIX)]
    if not stem:
        return None
    return (profile_path.resolve().parent.parent / "upgrades"
            / f"{COLLAPSE_DOCUMENT_PREFIX}{stem}.json")


def collapse_document_in(upgrades: Path, profile_path: Path) -> Path | None:
    """The same document, located inside an explicit upgrades directory.

    `apply_upgrade.py` already takes `--upgrades` and loads every other
    document from it, including the fixtures a test lays out in a temp
    tree. Deriving the location two ways would be drift waiting to
    happen, so the stem rule lives in one place and only the directory
    differs.
    """
    name = profile_path.name
    if not name.endswith(ASSETS_PROFILE_SUFFIX):
        return None
    stem = name[:-len(ASSETS_PROFILE_SUFFIX)]
    if not stem:
        return None
    return upgrades / f"{COLLAPSE_DOCUMENT_PREFIX}{stem}.json"


def _require(cond: bool, source: str, msg: str) -> None:
    if not cond:
        raise CollapseError(f"{source}: {msg}")


def _uuidish(v: Any) -> bool:
    return isinstance(v, str) and bool(_UUID_RE.match(v))


def _positive_int(v: Any) -> bool:
    return isinstance(v, int) and not isinstance(v, bool) and v > 0


def _nested(rec: Any, *keys: str) -> Any:
    cur = rec
    for k in keys:
        if not isinstance(cur, dict):
            return None
        cur = cur.get(k)
    return cur


def _parse_substitution(raw: Any, i: int, j: int, source: str,
                        entry_retired: str, entry_survivor: str) -> PostSubstitution:
    where = f"collapse[{i}].post_substitutions[{j}]"
    _require(isinstance(raw, dict), source, f"{where} is not an object")
    pid = raw.get("post_id")
    _require(_uuidish(pid), source, f"{where}.post_id is not a well formed id: {pid!r}")
    old = raw.get("old_members")
    new = raw.get("new_members")
    for name, members in (("old_members", old), ("new_members", new)):
        _require(isinstance(members, list) and members and
                 all(_uuidish(m) for m in members),
                 source, f"{where}.{name} must be a non-empty list of well formed ids")
        _require(len(set(members)) == len(members), source,
                 f"{where}.{name} names the same asset twice")
    _require(list(old) != list(new), source,
             f"{where} records a substitution that changes nothing; Pending and "
             f"Applied would be the same state and neither could be told from the "
             f"other")
    _require(entry_retired in old, source,
             f"{where}.old_members does not contain the retired id {entry_retired}")
    _require(entry_retired not in new, source,
             f"{where}.new_members still contains the retired id {entry_retired}")
    _require(entry_survivor in new, source,
             f"{where}.new_members does not contain the survivor {entry_survivor}")
    new_pid = raw.get("new_post_id")
    if new_pid is not None:
        _require(_uuidish(new_pid), source,
                 f"{where}.new_post_id is not a well formed id: {new_pid!r}")
        _require(new_pid != pid, source,
                 f"{where}.new_post_id equals post_id; omit it when the kind does "
                 f"not derive its id from membership")
    cur = raw.get("curation_pipeline_members")
    if cur is not None:
        _require(isinstance(cur, dict) and set(cur) == {"old", "new"}, source,
                 f"{where}.curation_pipeline_members must hold exactly `old` and `new`")
        for k in ("old", "new"):
            _require(isinstance(cur[k], str) and bool(_SHA1_RE.match(cur[k])), source,
                     f"{where}.curation_pipeline_members.{k} is not a sha1 digest")
        _require(cur["old"] != cur["new"], source,
                 f"{where}.curation_pipeline_members records the same digest twice")
    unknown = sorted(set(raw) - {"post_id", "old_members", "new_members",
                                 "new_post_id", "curation_pipeline_members", "_why"})
    _require(not unknown, source, f"{where} holds unknown key(s) {unknown}")
    return PostSubstitution(
        post_id=pid,
        old_members=tuple(old),
        new_members=tuple(new),
        new_post_id=new_pid,
        curation_pipeline_members=dict(cur) if cur else None,
    )


def _parse_entry(raw: Any, i: int, source: str) -> CollapseEntry:
    where = f"collapse[{i}]"
    _require(isinstance(raw, dict), source, f"{where} is not an object")
    missing = [k for k in _ENTRY_KEYS if k not in raw]
    _require(not missing, source, f"{where} is missing key(s) {missing}")

    rid, sid = raw["retired_id"], raw["survivor_id"]
    _require(_uuidish(rid), source, f"{where}.retired_id is not well formed: {rid!r}")
    _require(_uuidish(sid), source, f"{where}.survivor_id is not well formed: {sid!r}")
    _require(rid != sid, source, f"{where} retires {rid} onto itself")

    owner = raw["owner_username"]
    _require(isinstance(owner, str) and owner, source,
             f"{where}.owner_username is empty; identity is per OWNER and a "
             f"retirement that does not name one cannot be checked")
    root = raw["source_root"]
    _require(root in AUTHENTICABLE_ROOTS, source,
             f"{where}.source_root={root!r} has no reproducible source, so its "
             f"produced bytes could never be re-derived at publish. Authenticable "
             f"roots: {sorted(AUTHENTICABLE_ROOTS)}")
    path = raw["retired_file_path"]
    _require(isinstance(path, str) and path, source,
             f"{where}.retired_file_path is empty")

    ev = raw["evidence"]
    _require(isinstance(ev, dict), source, f"{where}.evidence is not an object")
    kind = ev.get("kind")
    _require(kind in EVIDENCE_KINDS, source,
             f"{where}.evidence.kind is {kind!r}; it must be one of "
             f"{list(EVIDENCE_KINDS)}. There is NO default: the two kinds make "
             f"different claims, and an unlabelled entry granted the stronger one "
             f"would be authority nobody wrote down.")

    # ⛔ THE WRONG KIND'S FIELDS ARE REFUSED, NEVER IGNORED. A preserved
    # entry carrying `materialized_sha256` would look, to anyone reading
    # the document, like a produced hash that had been checked. Nothing
    # checks it, so the field must not be allowed to sit there.
    own = (_PRODUCED_EVIDENCE_KEYS if kind == KIND_PRODUCED
           else _PRESERVED_EVIDENCE_KEYS)
    other = (_PRESERVED_EVIDENCE_KEYS if kind == KIND_PRODUCED
             else _PRODUCED_EVIDENCE_KEYS)
    trespass = sorted(k for k in other if k in ev)
    _require(not trespass, source,
             f"{where}.evidence declares kind={kind!r} but carries {trespass}, "
             f"which belong to the other kind. Nothing validates them here, so "
             f"they would be authority this entry does not have.")
    unknown_ev = sorted(set(ev) - set(own) - {"kind", "_why"})
    _require(not unknown_ev, source,
             f"{where}.evidence holds unknown key(s) {unknown_ev} for "
             f"kind={kind!r}")

    src_sha = mat_sha = member = tool = None
    px = None
    ret_sha = sur_sha = None
    if kind == KIND_PRODUCED:
        _require(root in PRODUCED_SOURCE_ROOTS, source,
                 f"{where}.source_root={root!r} is not source-backed, so its bytes "
                 f"cannot be re-derived from an external source. "
                 f"{KIND_PRODUCED} roots: {sorted(PRODUCED_SOURCE_ROOTS)}")
        src_sha = ev.get("source_sha256")
        mat_sha = ev.get("materialized_sha256")
        _require(isinstance(src_sha, str) and bool(_SHA256_RE.match(src_sha)), source,
                 f"{where}.evidence.source_sha256 is not a sha256: {src_sha!r}")
        _require(isinstance(mat_sha, str) and bool(_SHA256_RE.match(mat_sha)), source,
                 f"{where}.evidence.materialized_sha256 is not a sha256: {mat_sha!r}")
        _require(src_sha != mat_sha, source,
                 f"{where}.evidence records one value as both the source hash and "
                 f"the produced hash; they describe different bytes and conflating "
                 f"them is the mistake this document exists to prevent")
        member = ev.get("member")
        _require(isinstance(member, str) and member, source,
                 f"{where}.evidence.member is empty; a source hash with no member "
                 f"names no bytes")
        px = ev.get("render_px")
        _require(_positive_int(px), source,
                 f"{where}.evidence.render_px must be a positive int, got {px!r}")
        tool = ev.get("materialized_tool")
        _require(isinstance(tool, str) and tool, source,
                 f"{where}.evidence.materialized_tool is empty; a produced hash is "
                 f"only reproducible against the tool that produced it")
    else:
        _require(root in PRESERVED_ARCHIVE_ROOTS, source,
                 f"{where}.source_root={root!r} is not archive-authoritative, so "
                 f"the weaker {KIND_PRESERVED} claim does not apply to it: its "
                 f"bytes are still externally reproducible and must be "
                 f"authenticated as {KIND_PRODUCED}. {KIND_PRESERVED} roots: "
                 f"{sorted(PRESERVED_ARCHIVE_ROOTS)}")
        ret_sha = ev.get("retired_sha256")
        sur_sha = ev.get("survivor_sha256")
        for name, v in (("retired_sha256", ret_sha), ("survivor_sha256", sur_sha)):
            _require(isinstance(v, str) and bool(_SHA256_RE.match(v)), source,
                     f"{where}.evidence.{name} is not a sha256: {v!r}")
        # ⛔ EQUAL, AND THE OPPOSITE OF THE PRODUCED-SOURCE RULE ABOVE. There
        # these two values had to DIFFER, because a pack member before a
        # render is not the render. Here the whole claim is that the retired
        # file and the survivor file are the SAME BYTES, which is what makes
        # them one row in the app and what makes the retirement a collapse
        # rather than a deletion. Different fields, opposite rules, never
        # merged into one.
        _require(ret_sha == sur_sha, source,
                 f"{where}.evidence records retired_sha256 {ret_sha[:12]}… and "
                 f"survivor_sha256 {sur_sha[:12]}…, which are DIFFERENT bytes. A "
                 f"{KIND_PRESERVED} retirement claims the two files collapse to one "
                 f"row; two different files are two records, not a collision.")

    rec = raw["retired_record"]
    _require(isinstance(rec, dict) and rec, source,
             f"{where}.retired_record must be the record verbatim")
    _require(rec.get("id") == rid, source,
             f"{where}.retired_record.id is {rec.get('id')!r}, not the retired id")
    _require(rec.get("owner_username") == owner, source,
             f"{where}.retired_record.owner_username is "
             f"{rec.get('owner_username')!r}, not {owner!r}")
    _require(rec.get("source_root") == root, source,
             f"{where}.retired_record.source_root is {rec.get('source_root')!r}, "
             f"not {root!r}")
    _require(rec.get("file_path") == path, source,
             f"{where}.retired_record.file_path is {rec.get('file_path')!r}, not "
             f"the recorded retired_file_path {path!r}")
    _require(isinstance(rec.get("source_path"), str) and rec.get("source_path"), source,
             f"{where}.retired_record has no source_path, so its produced file "
             f"cannot be located under any source root")
    # Where a source hash is CLAIMED, the committed record must say the
    # same thing. This is the cheap half of the proxy invariant, and it
    # is checked here so a document cannot quietly describe a render the
    # record never declared.
    #
    # ⛔ THESE TWO CROSS-CHECKS ARE PRODUCED-SOURCE ONLY, AND A PRESERVED
    # ENTRY MUST NOT FAKE THEM. Measured on the committed profiles: of 696
    # site_a and 552 site_b `local` records, ZERO carry
    # `metadata.source_archive` at all. Demanding one would make a `local`
    # retirement expressible only by INVENTING provenance for it, which is
    # the whole reason the schema is discriminated.
    if kind == KIND_PRODUCED:
        _require(_nested(rec, "metadata", "source_archive", "sha256") == src_sha,
                 source,
                 f"{where}.retired_record.metadata.source_archive.sha256 is "
                 f"{_nested(rec, 'metadata', 'source_archive', 'sha256')!r}, which "
                 f"disagrees with evidence.source_sha256")
        _require(_nested(rec, "metadata", "render", "px") == px, source,
                 f"{where}.retired_record.metadata.render.px is "
                 f"{_nested(rec, 'metadata', 'render', 'px')!r}, which disagrees "
                 f"with evidence.render_px")

    losses = raw["acknowledged_losses"]
    _require(isinstance(losses, list), source,
             f"{where}.acknowledged_losses must be a list")
    seen_paths: set[str] = set()
    last = ""
    for k, loss in enumerate(losses):
        _require(isinstance(loss, dict), source,
                 f"{where}.acknowledged_losses[{k}] is not an object")
        _require(set(loss) <= {"path", "retired_value", "survivor_value"}, source,
                 f"{where}.acknowledged_losses[{k}] holds unknown key(s) "
                 f"{sorted(set(loss) - {'path', 'retired_value', 'survivor_value'})}")
        lp = loss.get("path")
        _require(isinstance(lp, str) and lp, source,
                 f"{where}.acknowledged_losses[{k}].path is empty")
        _require("retired_value" in loss, source,
                 f"{where}.acknowledged_losses[{k}] records no retired_value")
        _require(lp not in seen_paths, source,
                 f"{where}.acknowledged_losses names {lp} twice")
        _require(lp >= last, source,
                 f"{where}.acknowledged_losses is not sorted by path ({lp} follows "
                 f"{last}); the recomputation is sorted, so an unsorted list could "
                 f"only ever be compared by re-sorting it, and a comparison that "
                 f"normalises its input is not an equality check")
        seen_paths.add(lp)
        last = lp

    subs = raw["post_substitutions"]
    _require(isinstance(subs, list), source,
             f"{where}.post_substitutions must be a list")
    parsed = tuple(_parse_substitution(s, i, j, source, rid, sid)
                   for j, s in enumerate(subs))
    seen_posts: set[str] = set()
    for s in parsed:
        _require(s.post_id not in seen_posts, source,
                 f"{where} substitutes post {s.post_id} twice")
        seen_posts.add(s.post_id)

    unknown = sorted(set(raw) - set(_ENTRY_KEYS) - {"_why"})
    _require(not unknown, source, f"{where} holds unknown key(s) {unknown}")

    return CollapseEntry(
        retired_id=rid, survivor_id=sid, owner_username=owner, source_root=root,
        retired_file_path=path, kind=kind,
        retired_record=rec, acknowledged_losses=tuple(losses),
        source_sha256=src_sha, source_member=member, render_px=px,
        materialized_sha256=mat_sha, materialized_tool=tool,
        retired_sha256=ret_sha, survivor_sha256=sur_sha,
        post_substitutions=parsed,
    )


def parse_collapse_document(data: Any, *, source: str,
                            profile_name: str | None = None) -> CollapseDocument:
    """LAYER A1. Validate a parsed document one entry at a time, fail closed.

    Every check here is a refusal and none is overridable. It runs BEFORE
    the document influences any pass, so a malformed document can never
    reach the stage that deletes a record.

    It deliberately does NOT look at the profile: A1 is document
    internal, and the survivor record it would need is not in the
    document. The check that `acknowledged_losses` matches a
    recomputation therefore lives in A2, with the same strictness, where
    the merged profile is in hand (see `evaluate`).
    """
    _require(isinstance(data, dict), source,
             f"expected an object with a `collapse` list, got {type(data).__name__}")
    profile = data.get("profile")
    _require(isinstance(profile, str) and profile, source,
             "missing `profile` (the assets file it describes)")
    if profile_name is not None:
        _require(profile == profile_name, source,
                 f"describes profile {profile!r}, but the assets file being "
                 f"upgraded is {profile_name!r}; refusing to apply one profile's "
                 f"retirements to another")
    entries_raw = data.get("collapse")
    _require(isinstance(entries_raw, list), source,
             f"`collapse` must be a list, got {type(entries_raw).__name__}")
    entries = tuple(_parse_entry(e, i, source) for i, e in enumerate(entries_raw))

    # ⛔ `pool_of_record` IS REQUIRED BY THE PRESENCE OF A PRODUCED-SOURCE
    # ENTRY, NOT BY THE DOCUMENT EXISTING. It records the toolchain a
    # produced hash is only reproducible against. A preserved-only document
    # produces nothing: the bytes were not rendered by us, they were
    # PRESERVED, so a pool on one names a toolchain that had no part in
    # the claim, and a field that describes nothing is refused rather than
    # accepted as harmless. A MIXED document takes the stricter arm: one
    # produced-source entry makes the pool load-bearing for that entry,
    # and every entry is still validated only under its own kind.
    has_produced = any(e.kind == KIND_PRODUCED for e in entries)
    preserved_only = bool(entries) and not has_produced
    pool = data.get("pool_of_record")
    if preserved_only:
        _require("pool_of_record" not in data, source,
                 f"this document holds only {KIND_PRESERVED} entries, which record "
                 f"no produced bytes, so `pool_of_record` describes nothing. "
                 f"Refusing it rather than ignoring it: a pool that is never used "
                 f"reads as a toolchain claim that was checked.")
        pool = {}
    elif has_produced or "pool_of_record" in data:
        _require(isinstance(pool, dict), source,
                 "missing `pool_of_record` (what produced the bytes: pack, "
                 "render_px, rasteriser, and the LOCKED sharp and node versions "
                 "actually used)")
        for k in _POOL_KEYS:
            _require(pool.get(k) not in (None, "", {}), source,
                     f"pool_of_record.{k} is empty; a produced hash is only "
                     f"reproducible against the toolchain that produced it")
        _require(_positive_int(pool.get("render_px")), source,
                 f"pool_of_record.render_px must be a positive int, got "
                 f"{pool.get('render_px')!r}")
    else:
        pool = {}

    retired: set[str] = set()
    survivors: set[str] = set()
    for e in entries:
        _require(e.retired_id not in retired, source,
                 f"{e.retired_id} is retired twice")
        retired.add(e.retired_id)
        survivors.add(e.survivor_id)
    both = sorted(retired & survivors)
    _require(not both, source,
             f"{len(both)} id(s) are recorded as both a retired id and a survivor "
             f"({both[:5]}); a record cannot be kept and removed by one document")

    # ⛔ ONE POST, ONE SUBSTITUTION, ACROSS THE WHOLE DOCUMENT. Two
    # entries substituting the same post would have to be CHAINED: the
    # second's `old_members` would describe the state the first leaves
    # behind, which is not the state the stage evaluates (every object is
    # judged before any is applied, so a stale first entry cannot delete
    # on the strength of a second). Rather than evaluate a chain the
    # stage cannot honour, the document says it plainly: a post whose
    # membership loses several retired records is expressed as ONE
    # substitution on ONE entry, naming the final membership, and the
    # other entries carry no substitution for it.
    seen_post: dict[str, str] = {}
    for e in entries:
        for sub in e.post_substitutions:
            prev = seen_post.get(sub.post_id)
            _require(prev is None, source,
                     f"post {sub.post_id} is substituted by two entries "
                     f"({prev} and {e.retired_id}); a chained substitution cannot "
                     f"be authenticated, because every object is judged before "
                     f"any is applied. Express it as ONE substitution naming the "
                     f"final membership.")
            seen_post[sub.post_id] = e.retired_id

    unknown = sorted(set(data) - {"_why", "profile", "pool_of_record", "collapse"})
    _require(not unknown, source, f"holds unknown top level key(s) {unknown}")

    return CollapseDocument(profile=profile, pool_of_record=pool, entries=entries)


def load_collapse_document(path: Path, *,
                           profile_name: str | None = None) -> CollapseDocument:
    """Read and validate a committed document. Raises CollapseError."""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as e:
        raise CollapseError(f"{path}: unreadable ({e})") from e
    doc = parse_collapse_document(data, source=str(path), profile_name=profile_name)
    return CollapseDocument(profile=doc.profile, pool_of_record=doc.pool_of_record,
                            entries=doc.entries, path=path)


# ---------------------------------------------------------------------------
# Layer A2: state, on the merged profile and posts
# ---------------------------------------------------------------------------

@dataclass
class EntryState:
    entry: CollapseEntry
    asset_state: str
    post_states: dict[str, str] = field(default_factory=dict)


@dataclass
class CollapseResult:
    states: list[EntryState] = field(default_factory=list)
    records_removed: int = 0
    posts_rewritten: int = 0
    posts_renamed: int = 0

    @property
    def pending_assets(self) -> int:
        return sum(1 for s in self.states if s.asset_state == PENDING)

    @property
    def pending_posts(self) -> int:
        return sum(1 for s in self.states
                   for st in s.post_states.values() if st == PENDING)

    @property
    def pending(self) -> int:
        """Objects this run would still have to change.

        Used as a `--check` drift term, so it has to count OBJECTS rather
        than entries: an entry whose asset is Applied and whose post is
        Pending is drift, and an entry count would read it as settled.
        """
        return self.pending_assets + self.pending_posts


def _members(post: dict) -> tuple[str, ...]:
    return tuple(post.get("asset_ids") or ())


def evaluate(profile: list[dict], posts: list[dict],
             doc: CollapseDocument) -> CollapseResult:
    """LAYER A2. Judge every object, changing nothing.

    ⛔ EVERY entry is judged before ANY is applied, and a third state
    raises. That ordering is the whole safety property: a stale document
    whose second entry no longer matches must not be allowed to delete
    the record named by its first.
    """
    by_id = {a.get("id"): a for a in profile}
    posts_by_id: dict[str, dict] = {}
    for p in posts:
        # Last wins, which is what a reader of the written file gets.
        # `dedupe_posts` has already run at the point the stage is
        # called, so a duplicate here is not something this pass hides.
        posts_by_id[p.get("id")] = p

    violations: list[str] = []
    states: list[EntryState] = []
    source = str(doc.path or "collapse document")

    for e in doc.entries:
        survivor = by_id.get(e.survivor_id)
        if survivor is None:
            violations.append(
                f"{e.retired_id}: the survivor {e.survivor_id} is not in the "
                f"profile. A retirement with no survivor is a deletion, and this "
                f"document cannot authorise one.")
            continue

        # The losses are recomputed here, not in A1, because A1 cannot
        # see the survivor. Same strictness: an inequality is a refusal.
        recomputed = recompute_losses(e.retired_record, survivor)
        if not losses_equal(e.acknowledged_losses, recomputed):
            violations.append(
                f"{e.retired_id}: acknowledged_losses ({len(e.acknowledged_losses)} "
                f"entry(ies)) disagrees with what retiring it onto {e.survivor_id} "
                f"would actually lose ({len(recomputed)} entry(ies)). The losses "
                f"are ENUMERATED, never inferred, so a disagreement means the data "
                f"moved under the document. First difference: "
                f"{_first_loss_difference(e.acknowledged_losses, recomputed)}")
            continue

        retired = by_id.get(e.retired_id)
        if retired is None:
            asset_state = APPLIED
        elif retired == e.retired_record:
            asset_state = PENDING
        else:
            violations.append(
                f"{e.retired_id}: the record in the profile is not the "
                f"retired_record this document authorises. Refusing BEFORE any "
                f"deletion: a stale document must never remove data that has "
                f"changed since it was written. First difference: "
                f"{_first_record_difference(e.retired_record, retired)}")
            continue

        st = EntryState(entry=e, asset_state=asset_state)
        for sub in e.post_substitutions:
            here = posts_by_id.get(sub.post_id)
            there = posts_by_id.get(sub.applied_id)
            is_pending = here is not None and _members(here) == sub.old_members
            is_applied = (there is not None and _members(there) == sub.new_members
                          and (sub.applied_id == sub.post_id
                               or sub.post_id not in posts_by_id))
            if is_pending and not is_applied:
                st.post_states[sub.post_id] = PENDING
            elif is_applied and not is_pending:
                st.post_states[sub.post_id] = APPLIED
            else:
                violations.append(
                    f"{sub.post_id}: post membership is neither exactly the "
                    f"documented old_members nor exactly the new_members "
                    f"(order included). Found "
                    f"{_found_members(posts_by_id, sub)}. A third state is a hard "
                    f"failure, never a normalisation.")
        states.append(st)

    if violations:
        raise CollapseError(
            f"{source}: {len(violations)} object(s) are in a state this document "
            f"does not describe:\n  - " + "\n  - ".join(violations))
    return CollapseResult(states=states)


def _found_members(posts_by_id: dict[str, dict], sub: PostSubstitution) -> str:
    bits = []
    for label, pid in (("post_id", sub.post_id), ("new_post_id", sub.applied_id)):
        if pid is None or (label == "new_post_id" and sub.new_post_id is None):
            continue
        p = posts_by_id.get(pid)
        bits.append(f"{label}={pid} " + ("ABSENT" if p is None
                                         else f"members={list(_members(p))}"))
    return "; ".join(bits)


def _first_loss_difference(recorded: Sequence[dict], recomputed: Sequence[dict]) -> str:
    for i in range(max(len(recorded), len(recomputed))):
        a = recorded[i] if i < len(recorded) else None
        b = recomputed[i] if i < len(recomputed) else None
        if a != b:
            return f"recorded[{i}]={json.dumps(a, sort_keys=True)[:160]} vs " \
                   f"recomputed[{i}]={json.dumps(b, sort_keys=True)[:160]}"
    return "none"


def _first_record_difference(want: dict, got: dict) -> str:
    diff = recompute_losses(want, got)
    if diff:
        d = diff[0]
        return f".{d['path']} document={json.dumps(d.get('retired_value'))[:80]} " \
               f"profile={json.dumps(d.get('survivor_value'))[:80] if 'survivor_value' in d else 'ABSENT'}"
    extra = sorted(set(got) - set(want))
    return f"the profile record carries extra key(s) {extra}" if extra else "none"


def apply_collapse(profile: list[dict], posts: list[dict],
                   doc: CollapseDocument) -> CollapseResult:
    """Evaluate, then convert every Pending object to Applied.

    Applied is an idempotent no-op and a third state has already raised,
    so a second run over the same data changes nothing and a stale
    document changes nothing at all.
    """
    result = evaluate(profile, posts, doc)
    if not result.states:
        return result

    retire_now = {s.entry.retired_id for s in result.states
                  if s.asset_state == PENDING}
    if retire_now:
        before = len(profile)
        profile[:] = [a for a in profile if a.get("id") not in retire_now]
        result.records_removed = before - len(profile)

    posts_by_id = {p.get("id"): p for p in posts}
    for s in result.states:
        for sub in s.entry.post_substitutions:
            if s.post_states.get(sub.post_id) != PENDING:
                continue
            post = posts_by_id[sub.post_id]
            post["asset_ids"] = list(sub.new_members)
            result.posts_rewritten += 1
            if sub.new_post_id:
                post["id"] = sub.new_post_id
                result.posts_renamed += 1
    return result


# ---------------------------------------------------------------------------
# The partial proxy, used by the audit and by the guard suite
# ---------------------------------------------------------------------------

def proxy_key(record: dict) -> tuple | None:
    """`(owner_username, metadata.source_archive.sha256, metadata.render.px)`.

    ⚠️ A PARTIAL PROXY for the real `(owner, produced_byte_sha256)`
    invariant, and it is documented that way everywhere it is used. It is
    NECESSARY but not SUFFICIENT: two records sharing it definitely share
    an input and a render size, which is how the one live collision was
    found, but two records can still produce identical bytes from
    different inputs and this key will not say so. It is cheap, it needs
    no pack and no pool, and it runs in a repository with neither. The
    sufficient check is Layer B, which hashes the produced files.

    Returns None for a record that declares no source archive hash, which
    is most of the corpus.
    """
    meta = record.get("metadata") or {}
    sha = (meta.get("source_archive") or {}).get("sha256")
    if not sha:
        return None
    return (record.get("owner_username"), sha, (meta.get("render") or {}).get("px"))


def proxy_collisions(profile: Iterable[dict]) -> dict[tuple, list[str]]:
    """Groups of 2 or more records sharing `proxy_key`."""
    groups: dict[tuple, list[str]] = {}
    for rec in profile:
        key = proxy_key(rec)
        if key is None:
            continue
        groups.setdefault(key, []).append(str(rec.get("id")))
    return {k: v for k, v in groups.items() if len(v) > 1}
