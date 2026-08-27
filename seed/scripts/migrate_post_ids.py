#!/usr/bin/env python3
"""Move post ids onto the identity-derived key (#1293, ADR 0098).

    # what would move, and nothing else                 (the default)
    python3 seed/scripts/migrate_post_ids.py

    # the permanent invariant: every committed id already derives
    python3 seed/scripts/migrate_post_ids.py --check

    # do it, and write the mapping document beside the profiles
    python3 seed/scripts/migrate_post_ids.py --write

    # read-only reconcile against a published site
    python3 seed/scripts/migrate_post_ids.py \
        --reconcile /path/to/site_a/posts.json --site site_a

WHY THIS EXISTS
---------------
Three post kinds keyed their id on the sample's ANCHOR — the most-recent
member — which is a value that ACCOMPANIES the post rather than one that
identifies it. A team's genuinely most-recent asset lands in many
samples, so several roundups with different membership derived one id.

⛔ That is a disappearance, not a duplicate: `aa seed` keys on the stable
id, so of n colliding rows exactly one is ever seeded.

`sanitize_and_assemble.py` now derives those ids from MEMBERSHIP. This
tool carries the committed profiles across to the same rule. It calls
`roundup_post_id` / `sprint_post_id` / `showreel_post_id` from the
assembler rather than reimplementing them — a migration that spells the
key out a second time is a second definition of identity, and the two
drift.

⛔ DRY RUN BY DEFAULT. `--write` is the only thing that touches a file,
and it prints every id it is about to move first. The published archive
is never opened for writing by this tool at all: `--reconcile` reads.

WHAT IT DOES NOT DO
-------------------
It does not re-derive posts. Membership, titles, timestamps and
everything else are left exactly as committed — only `id` moves, and
only for the three kinds whose key was wrong. Nothing else in the
profile can change as a result of running this.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections import Counter
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import sanitize_and_assemble as sa  # noqa: E402

PROFILES = Path(__file__).resolve().parents[1] / "profiles"
UPGRADES = Path(__file__).resolve().parents[1] / "upgrades"

DEFAULT_PROFILES = ("studio-a.posts.json", "studio-b.posts.json",
                    "dataset.posts.json")

MIGRATED_KINDS = ("team_roundup", "project_sprint", "cinematics_showreel")


def detect_format(text: str, default: int = 1) -> tuple[int, str]:
    """The indent width and file terminator the file already uses.

    ⚠️ Three writers, three formats. `apply_upgrade.py` and
    `studio_balance.py` write the studio profiles with `indent=1` and a
    trailing newline; `sanitize_and_assemble.write_json` uses `indent=2`
    and NO trailing newline, which is what `dataset.posts.json` carries.
    Writing either one back at the wrong width rewrites EVERY line, and
    52,000 changed lines is a diff nobody can review for the 68 that
    matter. Migrating an id is not an excuse to reformat a file.
    """
    indent = default
    for line in text.split("\n", 2)[1:2]:
        stripped = line.lstrip(" ")
        if stripped:
            indent = len(line) - len(stripped)
    return indent, ("\n" if text.endswith("\n") else "")


class Unresolvable(ValueError):
    """A row the new key cannot be computed for. Always fatal — a
    migration that skips the rows it does not understand leaves the
    profile in two id schemes at once."""


def derived_id(post: dict) -> str:
    """The id this post's own content derives under the new key.

    `label` and `reel_label` are recovered from the title. That is a
    PARSE, not the naming heuristic ADR 0095 rejects: the title was
    formatted by the assembler from a closed vocabulary, the match is
    structural and exact, and anything that does not match raises rather
    than guessing. Both label lists live in `sanitize_and_assemble` and
    are checked against, so a label that stopped existing is caught here
    instead of producing a plausible wrong id.

    ⛔ THE PARSE IS THE FORMATTER'S INVERSE AND LIVES BESIDE IT.
    `sa.sprint_label_from_title` / `sa.reel_label_from_title` are the
    exact inverses of `sa.title_project_sprint` / `sa.title_showreel`,
    so a title change cannot leave the two halves disagreeing quietly.
    #1306 retired the em dash these used to split on and this function
    went red the same minute, which is the behaviour worth keeping: the
    recovered label is an INPUT to the derived id, so a parse that
    silently half-matched would move ids rather than fail.
    """
    kind = post["post_kind"]
    asset_ids = post["asset_ids"]

    if kind == "team_roundup":
        # title: title_team_roundup(team_name)
        return sa.roundup_post_id(post["team_name"], asset_ids)

    if kind == "project_sprint":
        # title: title_project_sprint(project, label)
        project = post["collection_name"]
        label = sa.sprint_label_from_title(project, post["title"])
        if label is None:
            raise Unresolvable(
                f"{post['id']}: title {post['title']!r} does not begin with "
                f"collection_name {project!r}, so the sprint label cannot be "
                f"recovered")
        if label not in sa.SPRINT_LABELS:
            raise Unresolvable(
                f"{post['id']}: recovered sprint label {label!r} is not one of "
                f"{list(sa.SPRINT_LABELS)}")
        return sa.sprint_post_id(project, label, asset_ids)

    if kind == "cinematics_showreel":
        # title: title_showreel(reel_label)
        label = sa.reel_label_from_title(post["title"])
        if label is None:
            raise Unresolvable(
                f"{post['id']}: title {post['title']!r} is not a showreel title")
        if label not in sa.REEL_LABELS:
            raise Unresolvable(
                f"{post['id']}: recovered reel label {label!r} is not one of "
                f"{list(sa.REEL_LABELS)}")
        return sa.showreel_post_id(post["studio"], label, asset_ids)

    raise Unresolvable(f"{post['id']}: {kind} is not a migrated kind")


def plan(posts: list[dict]) -> list[dict]:
    """Every id that moves, and why. Raises Unresolvable on any row of a
    migrated kind that cannot be keyed."""
    moves = []
    for post in posts:
        if post["post_kind"] not in MIGRATED_KINDS:
            continue
        new = derived_id(post)
        if new == post["id"]:
            continue
        moves.append({
            "old_id": post["id"],
            "new_id": new,
            "post_kind": post["post_kind"],
            "title": post["title"],
            "members": len(post["asset_ids"]),
        })
    return moves


def check_safe(posts: list[dict], moves: list[dict]) -> list[str]:
    """Reasons the plan must not be applied. Empty list means safe."""
    problems: list[str] = []
    by_old = {m["old_id"]: m for m in moves}

    # The set of ids AFTER the move, computed the way the file will look.
    after: list[str] = []
    for post in posts:
        m = by_old.get(post["id"])
        # A colliding old id maps every one of its rows to the same
        # entry, which is precisely the bug — each row still gets its own
        # NEW id, so recompute per row rather than trusting the index.
        if m and post["post_kind"] in MIGRATED_KINDS:
            after.append(derived_id(post))
        else:
            after.append(post["id"])

    dupes = [i for i, n in Counter(after).items() if n > 1]
    if dupes:
        problems.append(
            f"{len(dupes)} id(s) would still collide after the move: "
            f"{dupes[:5]}")

    moved_new = {m["new_id"] for m in moves}
    untouched = {p["id"] for p in posts if p["id"] not in by_old}
    clash = moved_new & untouched
    if clash:
        problems.append(
            f"{len(clash)} new id(s) collide with a post that is NOT moving: "
            f"{sorted(clash)[:5]}")

    if len(after) != len(posts):
        problems.append("the plan changes the number of rows")

    return problems


def apply(posts: list[dict], moves: list[dict]) -> int:
    """Rewrite ids in place. Returns how many rows changed."""
    by_old = {m["old_id"] for m in moves}
    changed = 0
    for post in posts:
        if post["post_kind"] in MIGRATED_KINDS and post["id"] in by_old:
            post["id"] = derived_id(post)
            changed += 1
    return changed


def reconcile(moves: list[dict], published: Path) -> int:
    """Read-only: does the published site account for every moved id?

    ⛔ Opens `published` for reading and nothing else. The archive share
    is real, shared and not backed up (ADR 0097: it is an OUTPUT).
    """
    rows = json.loads(published.read_text(encoding="utf-8"))
    live = {p["id"] for p in rows}
    old = {m["old_id"] for m in moves}
    new = {m["new_id"] for m in moves}

    print(f"\nreconcile against {published} ({len(rows):,} rows)",
          file=sys.stderr)
    known = old & live
    print(f"  moved ids the published site already holds : {len(known)} / {len(old)}",
          file=sys.stderr)
    unknown = sorted(old - live)
    if unknown:
        print(f"  ⚠ moved ids the published site does NOT hold : {len(unknown)}",
              file=sys.stderr)
        for i in unknown[:10]:
            print(f"      {i}", file=sys.stderr)
    already = new & live
    if already:
        print(f"  ⛔ NEW ids ALREADY present on the published site: {len(already)}",
              file=sys.stderr)
        return 1
    print("  the publish guard (ADR 0097) will see these as content the source "
          "no longer has and REFUSE the next publish. That is the guard "
          "working; the mapping document beside the profiles is what a "
          "reconciled publish reads.", file=sys.stderr)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--profile", type=Path, action="append", default=[],
                    help="posts.json to migrate (repeatable; defaults to the "
                         "committed studio-a / studio-b / dataset profiles)")
    ap.add_argument("--write", action="store_true",
                    help="actually rewrite the profiles (default is a dry run)")
    ap.add_argument("--check", action="store_true",
                    help="exit non-zero if any id would move; writes nothing")
    ap.add_argument("--reconcile", type=Path,
                    help="a published posts.json to compare against, READ ONLY")
    ap.add_argument("--doc", type=Path,
                    help="where to write the old->new mapping document "
                         f"(default: {UPGRADES}/post-id-migration.<profile>.json)")
    args = ap.parse_args()

    if args.write and args.check:
        ap.error("--write and --check are opposites")

    profiles = args.profile or [PROFILES / n for n in DEFAULT_PROFILES]

    total_moves = 0
    rc = 0
    for path in profiles:
        if not path.is_file():
            print(f"error: {path} not found", file=sys.stderr)
            return 2
        text = path.read_text(encoding="utf-8")
        posts = json.loads(text)
        indent, eof = detect_format(text)
        try:
            moves = plan(posts)
        except Unresolvable as exc:
            print(f"error: {path.name}: {exc}", file=sys.stderr)
            print("       Nothing was written. A row of a migrated kind whose "
                  "key cannot be recovered means the profile and the assembler "
                  "disagree about what these posts are; fix that before "
                  "migrating.", file=sys.stderr)
            return 2

        kinds = Counter(m["post_kind"] for m in moves)
        print(f"\n{path.name}: {len(posts):,} posts, {len(moves)} id(s) move "
              f"({', '.join(f'{k}={n}' for k, n in sorted(kinds.items())) or 'none'})",
              file=sys.stderr)

        problems = check_safe(posts, moves)
        if problems:
            for p in problems:
                print(f"  ⛔ {p}", file=sys.stderr)
            print("  refusing to write", file=sys.stderr)
            return 2

        # ⛔ Every id that moves is printed BEFORE anything is written.
        for m in sorted(moves, key=lambda m: (m["post_kind"], m["title"])):
            print(f"  {m['old_id']} -> {m['new_id']}  "
                  f"{m['post_kind']:<20} {m['members']:>2} members  {m['title']}",
                  file=sys.stderr)

        total_moves += len(moves)

        if args.reconcile:
            rc |= reconcile(moves, args.reconcile)

        if not args.write:
            continue

        doc_path = args.doc or (UPGRADES /
                                f"post-id-migration.{path.stem.split('.')[0]}.json")
        doc = {
            "_why": [
                "POST IDS MOVED ONTO THE IDENTITY-DERIVED KEY (#1293, ADR 0098).",
                "team_roundup, project_sprint and cinematics_showreel keyed their "
                "id on the sample's ANCHOR — the most-recent member — which does "
                "not identify the post. Different roundups derived one id, and "
                "`aa seed` keys on the stable id, so all but one of a colliding "
                "set could never be seeded.",
                "The new key is the post's MEMBERSHIP. This document is the "
                "old->new mapping, so a publish that finds the destination "
                "holding ids the source no longer has can tell a migration from "
                "a loss (ADR 0097).",
                "Generated by seed/scripts/migrate_post_ids.py. Nothing but `id` "
                "changed.",
            ],
            "profile": path.name,
            "moves": sorted(moves, key=lambda m: m["old_id"]),
        }
        doc_path.parent.mkdir(parents=True, exist_ok=True)
        doc_path.write_text(json.dumps(doc, indent=2, ensure_ascii=False) + "\n",
                            encoding="utf-8")

        changed = apply(posts, moves)
        path.write_text(json.dumps(posts, indent=indent, ensure_ascii=False) + eof,
                        encoding="utf-8")
        print(f"  wrote {path} ({changed} ids moved)", file=sys.stderr)
        print(f"  wrote {doc_path}", file=sys.stderr)

    if args.check and total_moves:
        print(f"\nFAILED: {total_moves} post id(s) do not derive from the "
              f"content of the post they name. Run without --check to see the "
              f"plan, then with --write to apply it.", file=sys.stderr)
        return 1

    if args.check:
        print("\nOK: every post id derives from the post it names.",
              file=sys.stderr)
    elif not args.write:
        print(f"\n(dry run — {total_moves} id(s) would move; nothing written. "
              f"Pass --write to apply.)", file=sys.stderr)

    return rc


if __name__ == "__main__":
    sys.exit(main())
