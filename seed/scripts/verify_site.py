#!/usr/bin/env python3
"""
Verify a published site against the profiles it was built from. Read-only.

    python3 seed/scripts/verify_site.py check \\
        --profile seed/profiles/studio-a.assets.json \\
        --posts   seed/profiles/studio-a.posts.json \\
        --site    $DATASETS/site_a \\
        --expect  <expectations.json> \\
        --baseline <baseline.json>

    python3 seed/scripts/verify_site.py baseline --site $DATASETS/site_a \\
        --out <baseline.json>            # BEFORE staging, never under --site

WHY A VERIFIER AND NOT A LOOK (#1319)
-------------------------------------
`populate_archive.py` publishes by copying the profile over the site's
MANIFEST.json and posts.json and by copying bytes into place. The guard
in front of it (`manifest_guard.py`, ADR 0097) refuses a publish that
would lose content, and that is the whole of what it proves: that the
run was allowed to start. Nothing afterwards says the run finished, that
every file the profile names landed at the size the profile records,
that the site now carries every id the profile carries and no other, or
that the files nobody asked the run to touch are still the bytes they
were. Every one of those has failed silently before (#572: posts.json
copied by hand, 584 posts served against a profile of 859; #604: a run
skipping 916 assets and exiting 0).

So this prints a verdict per invariant and exits non-zero if any one
of them failed. It reads the site and nothing else; it never writes
under `--site`, and the `baseline` subcommand refuses an `--out` inside
it.

THREE CLASSES OF ASSERTION, KEPT DISTINCT IN THE OUTPUT
-------------------------------------------------------
  profile-derived   Facts the profile pair alone determines: row and
                    distinct-id counts of MANIFEST.json and posts.json,
                    the existence and recorded size of every file the
                    profile names, and the migration-aware publish guard
                    run profile-versus-site (0 losses, 0 corrupted
                    measurements, 0 duplicate source ids). No argument
                    beyond the three paths is needed.

  site-specific     Facts about ONE site that the profile cannot know,
                    SUPPLIED by the operator in `--expect`: exact counts
                    and the ids that must appear exactly once. Nothing in
                    this module hard-codes what the live archive holds
                    today; a verifier that carried "861" as a constant
                    would be right until the next publish and silently
                    wrong after it. Without `--expect` the class reports
                    "not supplied".

  preservation      Before/after: every `*.bak`, `dataset-metadata.json`,
                    `kenney-hq-replacements.json` and `groups.csv` present
                    at the site is byte-equal to an explicit baseline
                    (`--baseline`, recorded before staging, or
                    `--reference`, a directory holding the pre-publish
                    copies), and the site's ATTRIBUTIONS.md equals the
                    repository copy. `metadata.csv` is preservation-owned
                    too but is NOT exact-bytes, because a retirement
                    legitimately removes its row: it is checked against the
                    expected-transform document (`--csv-transform`, written
                    before the publish by `preserved_archive.py`), which
                    requires exactly the documented removals and every
                    retained row byte-identical and in order.
                    Without a baseline the class reports "not compared".
                    It never reports "pass" for a comparison it did not
                    make: a green tick over an unmade check is the shape
                    of every silent failure above.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import asset_collapse as ac  # noqa: E402
import manifest_guard as mg  # noqa: E402
import preserved_archive as pa  # noqa: E402

SCRIPTS = Path(__file__).resolve().parent
REPO_ATTRIBUTIONS = SCRIPTS.parent / "ATTRIBUTIONS.md"

# The files a publish must leave alone. `*.bak` is matched at any depth;
# the named files live at the site root.
#
# ⛔ `groups.csv` JOINED THIS SET (#1319). It needs no transformation: its
# `asset_count` column is an ORIGINAL-DATASET fact that already disagrees
# with the shipped subset. Measured on site_b, `grp-00219` states 8 and
# ships 3, `grp-00215` states 8 and ships 2, and 262 of 1,047 rows disagree
# in all. And no group loses its last shipped member to a retirement (0 of
# 1,047 ship nothing). So the correct rule is the strictest one: the bytes
# do not change. ⛔ DO NOT REINTERPRET OR REWRITE `asset_count`. It
# describes the dataset the rows came from, not the cut this site ships,
# and "correcting" it would overwrite a fact with a derivation.
#
# ⚠️ `metadata.csv` IS DELIBERATELY NOT HERE. A retirement legitimately
# removes its row, so an exact-bytes rule would refuse the one change that
# is correct. It is governed by the expected-transform document instead
# (`preserved_archive.py`), which is narrower than a waiver in both
# directions: exactly the documented rows leave and every other row
# survives byte-identically, in order.
PRESERVED_NAMES = ("dataset-metadata.json", "kenney-hq-replacements.json",
                   pa.GROUPS_NAME)
PRESERVED_GLOB = "*.bak"
ATTRIBUTIONS_NAME = "ATTRIBUTIONS.md"

PROFILE_DERIVED = "profile-derived"
SITE_SPECIFIC = "site-specific"
PRESERVATION = "preservation"

PASS = "pass"
FAIL = "fail"
INFO = "info"
NOT_COMPARED = "not compared"

# Keys `--expect` may carry. Anything else is refused: a misspelt key
# would otherwise be an expectation that is never checked.
EXPECTATION_KEYS = frozenset({
    "manifest_rows", "manifest_ids", "posts_rows", "posts_ids",
    "migrations", "once", "require_attributions",
    # #1319, the retirement keys.
    #   collapses             how many retirements the committed
    #                         asset-collapse document for this profile
    #                         authorises. A count the operator states, so
    #                         a document that grew an entry nobody
    #                         expected fails here.
    #   retired_paths_absent  true when every documented
    #                         `retired_file_path` must be gone from the
    #                         site. This is the Kaggle-tree rule: a
    #                         retired file left in staging is uploaded.
    #   same_owner_same_bytes how many groups of staged files share one
    #                         owner AND identical bytes.
    #
    # ⚠️ `same_owner_same_bytes` IS A STAGED-STATE DIAGNOSTIC, NOT SOURCE
    # AUTHORITY. It reads the DESTINATION, which ADR 0097 makes an output
    # and never an input: it says what the published tree currently
    # holds, and says nothing about whether two `hq`, `pack` or `local`
    # records would produce identical bytes from their sources. Only the
    # publish guard can answer that, by hashing the produced files under
    # the source roots. A number here is a place to look, not a verdict
    # on the catalogue.
    "collapses", "retired_paths_absent", "same_owner_same_bytes",
})

SAMPLE = 8


@dataclass
class Verdict:
    klass: str
    name: str
    status: str
    detail: str = ""

    def __str__(self) -> str:
        mark = {PASS: "PASS", FAIL: "FAIL", INFO: "info", NOT_COMPARED: "NOT COMPARED"}[self.status]
        line = f"[{self.klass}] {mark:<12} {self.name}"
        if self.detail:
            line += f": {self.detail}"
        return line


@dataclass
class Report:
    verdicts: list[Verdict] = field(default_factory=list)

    def add(self, klass: str, name: str, status: str, detail: str = "") -> Verdict:
        v = Verdict(klass, name, status, detail)
        self.verdicts.append(v)
        return v

    def check(self, klass: str, name: str, ok: bool, detail: str = "") -> Verdict:
        return self.add(klass, name, PASS if ok else FAIL, detail)

    @property
    def failed(self) -> list[Verdict]:
        return [v for v in self.verdicts if v.status == FAIL]

    @property
    def not_compared(self) -> list[Verdict]:
        return [v for v in self.verdicts if v.status == NOT_COMPARED]

    @property
    def ok(self) -> bool:
        return not self.failed

    def summary(self) -> str:
        passed = sum(1 for v in self.verdicts if v.status == PASS)
        head = "VERIFIED" if self.ok else "FAILED"
        parts = [f"{len(self.failed)} failed", f"{passed} passed"]
        if self.not_compared:
            parts.append(f"{len(self.not_compared)} not compared: "
                         + ", ".join(v.name for v in self.not_compared))
        return f"RESULT: {head} ({'; '.join(parts)})"


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()



def staged_same_owner_same_bytes(manifest: list[dict], site: Path) -> dict[tuple, list[str]]:
    """Groups of staged files sharing one owner AND identical bytes.

    ⚠️ A DIAGNOSTIC ABOUT THE DESTINATION, AND NOTHING MORE. ADR 0097
    makes the published tree an OUTPUT, so a reading taken from it can
    describe what is currently staged and can never authorise a change to
    the catalogue. It is here because the app's live uniqueness key is
    `(owner_user_ref, file_hash)` over exactly these bytes, so a group of
    two or more is a place a seeded instance will silently drop a row.
    Whether the SOURCE would produce that collision again is a question
    only the publish guard can answer, by hashing the produced files
    under the source roots.

    ⭐ Only files whose records agree on owner AND on `file_size_bytes`
    are hashed. Identical bytes imply an identical length, so the
    partition loses nothing and turns a whole-tree hash into a handful.
    Cross-owner identical bytes are legal (they are the CAS dedup
    fixture) and are never grouped.
    """
    buckets: dict[tuple, list[dict]] = {}
    for rec in manifest:
        rel = rec.get("file_path")
        if not rel:
            continue
        p = site / rel
        if not p.is_file():
            continue
        buckets.setdefault((rec.get("owner_username"), p.stat().st_size), []).append(rec)
    groups: dict[tuple, list[str]] = {}
    for (owner, size), recs in buckets.items():
        if len(recs) < 2:
            continue
        for rec in recs:
            digest = sha256_file(site / rec["file_path"])
            groups.setdefault((owner, digest), []).append(str(rec.get("id")))
    return {k: sorted(v) for k, v in groups.items() if len(v) > 1}


def _sample(items: list, n: int = SAMPLE) -> str:
    shown = ", ".join(str(x) for x in items[:n])
    if len(items) > n:
        shown += f", ... and {len(items) - n} more"
    return shown


def preserved_files(root: Path) -> dict[str, Path]:
    """Every preserved file present under `root`, keyed by site-relative path."""
    out: dict[str, Path] = {}
    if not root.is_dir():
        return out
    for name in PRESERVED_NAMES:
        p = root / name
        if p.is_file():
            out[name] = p
    for p in sorted(root.rglob(PRESERVED_GLOB)):
        if p.is_file():
            out[p.relative_to(root).as_posix()] = p
    return out


def record_baseline(site: Path) -> dict[str, Any]:
    """The preservation baseline: site-relative path -> sha256, recorded
    BEFORE a publish so the verifier can prove afterwards that none of
    these files changed. Includes ATTRIBUTIONS.md when present, so a
    baseline also pins the copy the site shipped with."""
    files = {rel: sha256_file(p) for rel, p in preserved_files(site).items()}
    attr = site / ATTRIBUTIONS_NAME
    if attr.is_file():
        files[ATTRIBUTIONS_NAME] = sha256_file(attr)
    return {"site": str(site), "files": files}


def load_expectations(path: Path) -> dict[str, Any]:
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError(f"{path}: expectations must be an object")
    unknown = sorted(set(data) - EXPECTATION_KEYS)
    if unknown:
        raise ValueError(f"{path}: unknown expectation key(s) {unknown}; "
                         f"known: {sorted(EXPECTATION_KEYS)}")
    return data


def _load_list(path: Path, what: str) -> list[dict]:
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, list):
        raise ValueError(f"{path}: {what} must be a list of records")
    return data


def _ids(rows: list[dict]) -> list:
    return [r.get("id") for r in rows]


def _loss_kinds(cmp: mg.Comparison) -> str:
    kinds: dict[str, int] = {}
    for x in cmp.losses:
        kinds[x.kind] = kinds.get(x.kind, 0) + 1
    return ", ".join(f"{k} {n}" for k, n in sorted(kinds.items()))


# --------------------------------------------------------------------------
# profile-derived
# --------------------------------------------------------------------------

def _check_counts(rep: Report, label: str, source: list[dict], site_rows: list[dict]) -> None:
    src_ids, dst_ids = _ids(source), _ids(site_rows)
    rep.check(PROFILE_DERIVED, f"{label} rows", len(site_rows) == len(source),
              f"site {len(site_rows)}, profile {len(source)}")
    rep.check(PROFILE_DERIVED, f"{label} distinct ids",
              len(set(dst_ids)) == len(set(src_ids)),
              f"site {len(set(dst_ids))}, profile {len(set(src_ids))}")


def _check_files(rep: Report, profile: list[dict], site: Path) -> None:
    missing: list[str] = []
    wrong: list[str] = []
    measured = 0
    for a in profile:
        rel = a.get("file_path")
        if not rel:
            continue
        p = site / rel
        if not p.is_file():
            missing.append(rel)
            continue
        size = a.get("file_size_bytes")
        if isinstance(size, int) and not isinstance(size, bool) and size > 0:
            measured += 1
            actual = p.stat().st_size
            if actual != size:
                wrong.append(f"{rel} (profile {size}, on disk {actual})")
    rep.check(PROFILE_DERIVED, "profile files present at the site", not missing,
              f"{len(profile)} record(s); missing {len(missing)}"
              + (f": {_sample(missing)}" if missing else ""))
    rep.check(PROFILE_DERIVED, "recorded file_size_bytes match the bytes on disk",
              not wrong,
              f"{measured} recorded size(s); mismatched {len(wrong)}"
              + (f": {_sample(wrong)}" if wrong else ""))


def _check_guard(rep: Report, label: str, cmp: mg.Comparison) -> None:
    rep.check(PROFILE_DERIVED, f"{label} guard: no loss", not cmp.losses,
              f"losses {len(cmp.losses)}" + (f" ({_loss_kinds(cmp)})" if cmp.losses else "")
              + f"; changes {len(cmp.changes)}; added {len(cmp.added)}")
    rep.check(PROFILE_DERIVED, f"{label} guard: no corrupted measurement",
              not cmp.corruptions, f"{len(cmp.corruptions)}")
    rep.check(PROFILE_DERIVED, f"{label} guard: no duplicate source ids",
              not cmp.duplicates, f"{len(cmp.duplicates)}")


def verify(profile_path: Path, posts_path: Path, site: Path, *,
           migration_path: Path | None = None,
           collapse_path: Path | None = None,
           expectations: dict[str, Any] | None = None,
           baseline: dict[str, Any] | None = None,
           reference: Path | None = None,
           csv_transform: Path | None = None,
           attributions: Path = REPO_ATTRIBUTIONS) -> Report:
    """Every verdict, in order; see the module docstring for the classes."""
    rep = Report()

    profile = _load_list(profile_path, "profile")
    posts = _load_list(posts_path, "posts")

    # -- profile-derived ---------------------------------------------------
    site_manifest = mg.load_json_list(site / "MANIFEST.json")
    site_posts = mg.load_json_list(site / "posts.json")
    rep.check(PROFILE_DERIVED, "MANIFEST.json present at the site", site_manifest is not None)
    rep.check(PROFILE_DERIVED, "posts.json present at the site", site_posts is not None)
    site_manifest = site_manifest or []
    site_posts = site_posts or []

    _check_counts(rep, "MANIFEST.json", profile, site_manifest)
    _check_counts(rep, "posts.json", posts, site_posts)
    _check_files(rep, profile, site)

    manifest_cmp = mg.compare(profile, site_manifest, "MANIFEST.json")
    _check_guard(rep, "MANIFEST.json", manifest_cmp)

    migration: mg.Migration | None = None
    doc_path = migration_path if migration_path is not None else mg.migration_document_path(posts_path)
    if doc_path is not None and doc_path.is_file():
        try:
            migration = mg.load_migration_document(doc_path, profile_name=posts_path.name)
            rep.add(PROFILE_DERIVED, "migration document", INFO,
                    f"{doc_path} ({len(migration.moves)} recorded move(s))")
        except mg.MigrationError as e:
            rep.add(PROFILE_DERIVED, "migration document", FAIL, str(e))
    elif migration_path is not None:
        rep.add(PROFILE_DERIVED, "migration document", FAIL, f"{doc_path}: not a file")
    else:
        rep.add(PROFILE_DERIVED, "migration document", INFO,
                f"none at {doc_path}; every destination-only id counts as a loss")

    posts_cmp: mg.Comparison | None
    try:
        posts_cmp = mg.compare(posts, site_posts, "posts.json", migration=migration)
    except mg.MigrationError as e:
        posts_cmp = None
        rep.add(PROFILE_DERIVED, "posts.json guard", FAIL, str(e))
    if posts_cmp is not None:
        _check_guard(rep, "posts.json", posts_cmp)
        rep.add(PROFILE_DERIVED, "posts.json guard: migrated records", INFO,
                f"{posts_cmp.records_migrated}")

    # #1319. Repository-local: the document is read and validated here,
    # and that is ALL this proves. A retirement is source-authenticated
    # at publish, by hashing the produced files under the source roots;
    # a verifier that reads only the site could never do it, and a green
    # tick here must not be mistaken for one.
    collapse: ac.CollapseDocument = ac.EMPTY
    collapse_present: ac.CollapseDocument | None = None
    collapse_raw: bytes | None = None
    # ⛔ A DOCUMENT THAT EXISTS AND CANNOT BE VALIDATED IS NOT AN ABSENT ONE.
    # Without this flag the binding verdict below would say "no collapse
    # document exists", which is a different and much softer claim than "the
    # document is unusable" and is exactly the reading this codebase refuses
    # everywhere else.
    collapse_unusable = False
    cdoc = collapse_path if collapse_path is not None else ac.collapse_document_path(profile_path)
    if cdoc is not None and cdoc.is_file():
        try:
            collapse_raw = cdoc.read_bytes()
            collapse = ac.load_collapse_document(cdoc, profile_name=profile_path.name)
            collapse_present = collapse
            rep.add(PROFILE_DERIVED, "collapse document", INFO,
                    f"{cdoc} ({len(collapse.entries)} documented retirement(s); "
                    f"structural only, produced bytes are authenticated at publish)")
        except ac.CollapseError as e:
            rep.add(PROFILE_DERIVED, "collapse document", FAIL, str(e))
            collapse_unusable = True
    elif collapse_path is not None:
        rep.add(PROFILE_DERIVED, "collapse document", FAIL, f"{cdoc}: not a file")
    else:
        rep.add(PROFILE_DERIVED, "collapse document", INFO,
                f"none at {cdoc}; no record is documented as retired")

    # -- site-specific -----------------------------------------------------
    if expectations is None:
        rep.add(SITE_SPECIFIC, "expectations", NOT_COMPARED, "no --expect supplied")
    else:
        for key, have in (("manifest_rows", len(site_manifest)),
                          ("manifest_ids", len(set(_ids(site_manifest)))),
                          ("posts_rows", len(site_posts)),
                          ("posts_ids", len(set(_ids(site_posts))))):
            if key in expectations:
                want = expectations[key]
                rep.check(SITE_SPECIFIC, key, have == want, f"site {have}, expected {want}")
        if "migrations" in expectations:
            want = expectations["migrations"]
            have = posts_cmp.records_migrated if posts_cmp is not None else None
            rep.check(SITE_SPECIFIC, "migrations", have == want,
                      f"site {have}, expected {want}")
        if "collapses" in expectations:
            want = expectations["collapses"]
            rep.check(SITE_SPECIFIC, "collapses", len(collapse.entries) == want,
                      f"document {len(collapse.entries)}, expected {want}")
        if expectations.get("retired_paths_absent"):
            still = [rel for rel in collapse.retired_paths if (site / rel).is_file()]
            rep.check(SITE_SPECIFIC, "retired paths absent from the site", not still,
                      f"{len(collapse.retired_paths)} documented path(s); "
                      f"{len(still)} still present"
                      + (f": {_sample(still)}" if still else ""))
        if "same_owner_same_bytes" in expectations:
            want = expectations["same_owner_same_bytes"]
            groups = staged_same_owner_same_bytes(site_manifest, site)
            rep.check(SITE_SPECIFIC, "same_owner_same_bytes", len(groups) == want,
                      f"site {len(groups)}, expected {want} (STAGED-STATE "
                      f"DIAGNOSTIC: it reads the destination and is not source "
                      f"authority for hq, pack or local records)"
                      + (f"; {_sample([f'{k[0]}/{k[1][:12]}:{v}' for k, v in sorted(groups.items())])}"
                         if groups else ""))
        for rid in expectations.get("once", ()):
            n = sum(1 for r in site_posts if r.get("id") == rid)
            rep.check(SITE_SPECIFIC, f"post {rid} appears exactly once", n == 1,
                      f"{n} row(s)")

    # -- preservation ------------------------------------------------------
    present = preserved_files(site)
    if baseline is not None and reference is not None:
        rep.add(PRESERVATION, "baseline", FAIL, "give --baseline or --reference, not both")
    elif baseline is not None:
        files = baseline.get("files")
        if not isinstance(files, dict):
            rep.add(PRESERVATION, "baseline", FAIL, "baseline has no `files` object")
        else:
            _check_against(rep, site, {k: v for k, v in files.items() if k != ATTRIBUTIONS_NAME},
                           present, "baseline")
    elif reference is not None:
        ref_files = {rel: sha256_file(p) for rel, p in preserved_files(reference).items()}
        _check_against(rep, site, ref_files, present, f"reference {reference}")
    else:
        rep.add(PRESERVATION, "preserved files", NOT_COMPARED,
                f"{len(present)} preserved file(s) present; no --baseline or --reference")

    # `metadata.csv` is preservation-owned but NOT exact-baseline: a
    # retirement legitimately removes its row. The expected-transform
    # document says which rows those are, and every other row must survive
    # byte-identically and in order. ⛔ NOT COMPARED is reported as NOT
    # COMPARED: a green tick over an unmade check is the shape of every
    # silent failure this module exists for, and #1319 measured what that
    # looks like: the regeneration wrote a HEADER-ONLY metadata.csv and
    # nothing noticed, because nothing was looking.
    site_csv = site / pa.CSV_NAME
    if csv_transform is not None:
        try:
            doc = pa.load_csv_transform(csv_transform)
            if not site_csv.is_file():
                rep.add(PRESERVATION, f"{pa.CSV_NAME} is the documented transform",
                        FAIL, f"absent at {site_csv}")
            else:
                refusals = pa.verify_csv_transform(doc, site_csv.read_bytes())
                rep.check(PRESERVATION,
                          f"{pa.CSV_NAME} is the documented transform",
                          not refusals,
                          (f"{doc['expected']['data_rows']} retained row(s), "
                           f"{len(doc['removals'])} documented removal(s)")
                          if not refusals else "; ".join(refusals[:3]))
                # ⛔ AND THE VERIFIER CANNOT BLESS A TRANSFORM WHOSE AUTHORITY
                # IS NOT THIS DOCUMENT'S. The removal recomputation needs the
                # PRE-operation CSV and the site holds the post-operation one,
                # so what is proved here is the identity half: the profile, the
                # document digest, its entry count and its retirement set. A
                # document that has moved since the transform was emitted fails
                # here, which is what stops a stale transform being verified
                # green after the fact.
                bound = (["the collapse document for this profile exists but "
                           "cannot be validated, so nothing authorises this "
                           "transform. Unusable is not absent."]
                         if collapse_unusable else pa.binding_refusals(
                             doc, profile_name=profile_path.name,
                             collapse_doc=collapse_present,
                             collapse_raw=collapse_raw))
                rep.check(PRESERVATION,
                          f"{pa.CSV_NAME} transform is authorised by the "
                          f"collapse document",
                          not bound,
                          "; ".join(bound[:3]) if bound else
                          (f"{doc['profile']}, "
                           + (f"document {doc[pa.COLLAPSE_BINDING_KEY]['sha256'][:12]}… "
                              f"({doc[pa.COLLAPSE_BINDING_KEY]['entries']} entry(ies))"
                              if doc.get(pa.COLLAPSE_BINDING_KEY)
                              else "no collapse document, so no removal is authorised")))
        except pa.PreservedError as e:
            rep.add(PRESERVATION, f"{pa.CSV_NAME} is the documented transform",
                    FAIL, str(e))
    elif site_csv.is_file():
        rep.add(PRESERVATION, f"{pa.CSV_NAME} is the documented transform",
                NOT_COMPARED,
                f"{site_csv} present; no --csv-transform. A preserved publish "
                f"changes it only through that document (preserved_archive.py "
                f"csv-transform)")

    site_attr = site / ATTRIBUTIONS_NAME
    require_attr = bool((expectations or {}).get("require_attributions", False))
    if site_attr.is_file():
        if attributions.is_file():
            same = sha256_file(site_attr) == sha256_file(attributions)
            rep.check(PRESERVATION, "ATTRIBUTIONS.md equals the repository copy", same,
                      f"site {site_attr}, repository {attributions}")
        else:
            rep.add(PRESERVATION, "ATTRIBUTIONS.md equals the repository copy", FAIL,
                    f"repository copy not found at {attributions}")
    elif require_attr:
        rep.add(PRESERVATION, "ATTRIBUTIONS.md present at the site", FAIL, "absent")
    else:
        rep.add(PRESERVATION, "ATTRIBUTIONS.md equals the repository copy", NOT_COMPARED,
                "absent at the site (set require_attributions in --expect to demand it)")
    return rep


def _check_against(rep: Report, site: Path, expected: dict[str, str],
                   present: dict[str, Path], source: str) -> None:
    """Every file the baseline/reference records must still be at the
    site, byte-for-byte. Files the site holds that the baseline never
    recorded are reported, not failed: a publish may legitimately add a
    new `.bak`, and preservation is about what was already there."""
    gone: list[str] = []
    changed: list[str] = []
    same = 0
    for rel, want in sorted(expected.items()):
        p = site / rel
        if not p.is_file():
            gone.append(rel)
        elif sha256_file(p) != want:
            changed.append(rel)
        else:
            same += 1
    rep.check(PRESERVATION, f"preserved files byte-equal to {source}", not gone and not changed,
              f"{same} unchanged; {len(changed)} changed"
              + (f": {_sample(changed)}" if changed else "")
              + f"; {len(gone)} missing" + (f": {_sample(gone)}" if gone else ""))
    extra = sorted(set(present) - set(expected))
    if extra:
        rep.add(PRESERVATION, "preserved files not in the baseline", INFO, _sample(extra))


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------

def _cmd_check(args: argparse.Namespace) -> int:
    try:
        expectations = load_expectations(args.expect) if args.expect else None
        baseline = None
        if args.baseline:
            baseline = json.loads(args.baseline.read_text(encoding="utf-8"))
            if not isinstance(baseline, dict):
                raise ValueError(f"{args.baseline}: baseline must be an object")
        rep = verify(args.profile, args.posts, args.site,
                     migration_path=args.migration_document,
                     collapse_path=args.collapse_document,
                     expectations=expectations, baseline=baseline,
                     reference=args.reference, csv_transform=args.csv_transform,
                     attributions=args.attributions)
    except (OSError, ValueError) as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    for v in rep.verdicts:
        print(v)
    print(rep.summary())
    return 0 if rep.ok else 1


def _cmd_baseline(args: argparse.Namespace) -> int:
    site = args.site.resolve()
    out = args.out.resolve()
    if out == site or site in out.parents:
        print(f"error: --out {out} is under --site {site}; the baseline must not be "
              "written into the site it describes", file=sys.stderr)
        return 2
    if not site.is_dir():
        print(f"error: --site {site} is not a directory", file=sys.stderr)
        return 2
    doc = record_baseline(site)
    out.write_text(json.dumps(doc, indent=1, sort_keys=True) + "\n", encoding="utf-8")
    print(f"recorded {len(doc['files'])} file(s) from {site} into {out}", file=sys.stderr)
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    chk = sub.add_parser("check", help="verify a site against a profile pair (read-only)")
    chk.add_argument("--profile", required=True, type=Path, help="assets profile (studio-a.assets.json)")
    chk.add_argument("--posts", required=True, type=Path, help="posts profile (studio-a.posts.json)")
    chk.add_argument("--site", required=True, type=Path, help="published site directory")
    chk.add_argument("--migration-document", type=Path, default=None,
                     help="override the post-id migration document location "
                          "(default: seed/upgrades/post-id-migration.<stem>.json "
                          "beside the profiles)")
    chk.add_argument("--collapse-document", type=Path, default=None,
                     help="override the asset-collapse document location "
                          "(default: seed/upgrades/asset-collapse.<stem>.json "
                          "beside the profiles)")
    chk.add_argument("--expect", type=Path, default=None,
                     help="site-specific expectations JSON: manifest_rows, manifest_ids, "
                          "posts_rows, posts_ids, migrations, once (list of ids), "
                          "require_attributions, collapses, retired_paths_absent, "
                          "same_owner_same_bytes")
    chk.add_argument("--baseline", type=Path, default=None,
                     help="preservation baseline written by `baseline` before staging")
    chk.add_argument("--reference", type=Path, default=None,
                     help="directory holding pre-publish copies of the preserved files")
    chk.add_argument("--csv-transform", type=Path, default=None,
                     help="metadata.csv expected-transform document, written "
                          "BEFORE the publish by `preserved_archive.py "
                          "csv-transform`. Without it the metadata.csv verdict "
                          "reports NOT COMPARED rather than passing.")
    chk.add_argument("--attributions", type=Path, default=REPO_ATTRIBUTIONS,
                     help="repository ATTRIBUTIONS.md the site copy must equal")
    chk.set_defaults(fn=_cmd_check)

    bl = sub.add_parser("baseline", help="record the preservation baseline (sha256 per file)")
    bl.add_argument("--site", required=True, type=Path)
    bl.add_argument("--out", required=True, type=Path, help="where to write it; never under --site")
    bl.set_defaults(fn=_cmd_baseline)

    args = ap.parse_args(argv)
    return args.fn(args)


if __name__ == "__main__":
    sys.exit(main())
