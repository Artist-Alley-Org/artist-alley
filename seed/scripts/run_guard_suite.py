#!/usr/bin/env python3
"""CI entry point for the seed guard suite (#1300).

    python3 seed/scripts/run_guard_suite.py

`test_dataset_upgrade.py` is a stdlib `unittest` module and can be run
directly — that stays the developer path and is what its own docstring
documents. This wrapper exists for one reason: **a suite that runs but
collects nothing reports success**, and that is the failure mode #1300
was filed about. `python3 test_dataset_upgrade.py` exits 0 on a suite of
zero tests, and a CI step that only checks the exit code cannot tell a
150-test pass from an empty one.

So this runner adds three assertions the bare `unittest.main()` cannot:

  1. **Every seed module imports.** The test module imports ten of them
     at top level, so any one of them failing to import takes the whole
     suite with it. Importing them here first, one at a time, turns
     "the suite vanished" into "kenney_hq.py does not import, here is
     the traceback" — the same failure, named.
  2. **The collected count is above a floor.** A renamed file, a broken
     `TestLoader` pattern or a deleted class drops the count silently.
     `MIN_TESTS` is the floor the gate refuses to go below.
  3. **The count is printed and published.** The number lands in the job
     log and in `$GITHUB_STEP_SUMMARY`, so a drop is visible on the run
     page without opening the log.

⛔ Do not wire this behind `|| true` or `continue-on-error`. A gate that
cannot fail is the defect wearing a green tick.
"""

from __future__ import annotations

import importlib
import os
import sys
import traceback
import unittest
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent

# The modules `test_dataset_upgrade` imports at top level. Kept here as
# an explicit list rather than derived from the test file's imports: the
# point is to fail with the offending module's NAME before the suite is
# loaded, which means knowing the names up front.
SEED_MODULES = [
    "apply_upgrade",
    "audit_uncatalogued",
    "authored_plates",
    "kenney_hq",
    "kenney_pack_sources",
    "manifest_guard",
    "pexels_gameplay",
    "populate_archive",
    "resolve_media_urls",
    "studio_balance",
]

TEST_MODULE = "test_dataset_upgrade"

# Floor, not a target. The suite held 150 tests when this gate landed.
# Raise it when a batch of tests lands; never lower it to make a red
# run go green — a dropped test is the thing this number exists to
# catch.
MIN_TESTS = 150


def _summary(line: str) -> None:
    """Append a line to the GitHub step summary when running in CI."""
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not path:
        return
    try:
        with open(path, "a", encoding="utf-8") as fh:
            fh.write(line + "\n")
    except OSError:
        # A summary we cannot write is not a reason to fail the gate.
        pass


def main() -> int:
    sys.path.insert(0, str(SCRIPTS))

    # ── 1. every seed module imports ──────────────────────────────────
    broken: list[str] = []
    for name in SEED_MODULES:
        try:
            importlib.import_module(name)
        except Exception:  # noqa: BLE001 — any import failure counts
            broken.append(name)
            print(f"::error title=Seed module failed to import::{name}.py",
                  file=sys.stderr)
            traceback.print_exc()
    if broken:
        print(f"\nFAILED: {len(broken)} seed module(s) did not import: "
              f"{', '.join(broken)}", file=sys.stderr)
        _summary(f"❌ **Seed guard suite** — {len(broken)} module(s) failed "
                 f"to import: `{'`, `'.join(broken)}`")
        return 1

    # ── 2. the suite loads and collects more than the floor ───────────
    # Imported directly rather than through TestLoader.loadTestsFromName:
    # the loader turns an ImportError into a synthetic failing test,
    # which fails the run but reports the traceback as test output. A
    # direct import raises here, with the real traceback, before any
    # counting happens.
    try:
        module = importlib.import_module(TEST_MODULE)
    except Exception:  # noqa: BLE001
        print(f"::error title=Guard suite failed to load::{TEST_MODULE}.py",
              file=sys.stderr)
        traceback.print_exc()
        _summary(f"❌ **Seed guard suite** — `{TEST_MODULE}.py` failed to load")
        return 1

    loader = unittest.TestLoader()
    suite = loader.loadTestsFromModule(module)
    collected = suite.countTestCases()

    if loader.errors:
        for err in loader.errors:
            print(err, file=sys.stderr)
        print(f"\nFAILED: the loader reported {len(loader.errors)} error(s)",
              file=sys.stderr)
        _summary("❌ **Seed guard suite** — the test loader reported errors")
        return 1

    print(f"collected {collected} tests from {TEST_MODULE}.py "
          f"(floor: {MIN_TESTS})", file=sys.stderr)
    if collected < MIN_TESTS:
        print(f"::error title=Guard suite shrank::collected {collected} "
              f"tests, floor is {MIN_TESTS}", file=sys.stderr)
        print(f"\nFAILED: collected {collected} tests, which is below the "
              f"floor of {MIN_TESTS}. Either tests were removed — say so and "
              f"lower the floor deliberately — or collection is broken.",
              file=sys.stderr)
        _summary(f"❌ **Seed guard suite** — collected {collected} tests, "
                 f"below the floor of {MIN_TESTS}")
        return 1

    # ── 3. run it ─────────────────────────────────────────────────────
    result = unittest.TextTestRunner(verbosity=2, stream=sys.stderr).run(suite)

    ran = result.testsRun
    failed = len(result.failures) + len(result.errors)
    print(f"\nseed guard suite: ran {ran} tests, {failed} failed, "
          f"{len(result.skipped)} skipped", file=sys.stderr)

    if ran < MIN_TESTS:
        # Collection said one number and execution another — a suite
        # that stops early is not a suite that passed.
        print(f"::error title=Guard suite ran fewer tests than it "
              f"collected::ran {ran}, collected {collected}", file=sys.stderr)
        _summary(f"❌ **Seed guard suite** — ran {ran} of {collected} "
                 f"collected tests")
        return 1

    if not result.wasSuccessful():
        _summary(f"❌ **Seed guard suite** — {failed} of {ran} tests failed")
        return 1

    _summary(f"✅ **Seed guard suite** — {ran} tests passed "
             f"({len(SEED_MODULES)} seed modules imported clean)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
