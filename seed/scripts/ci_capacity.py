#!/usr/bin/env python3
"""
Disk-capacity phase model for the hosted UI workflows (#1514, ADR 0100).

    ci_capacity.py sites     <profile> --pins <ci-datasets.v1.json>
    ci_capacity.py preflight <profile> --pins <...> --free N
    ci_capacity.py prepare   <profile> --pins <...> --dataset-free N --docker-free N [--same-fs]

The workflows run in this order, so disk use peaks per PHASE, not as one sum of everything:

  build   the image(s) are built: Docker holds B_peak (base images, build cache, final images).
  prune   this job's own build cache is removed (`docker builder prune -af`; nothing later builds, and
          compose-cleanup prunes it at the end anyway). Docker keeps B_kept (the images).
  fetch   the archive parts D and the extracted tree E coexist; the fetch tool then deletes D.
  seed    E stays (the seed reads it) while the seed copies its selected closure S into the storage
          volume and the render queue writes previews (budgeted at S again); postgres + logs take R.
  late    E is released after the seed; Playwright's browser + OS deps take W.

  data phases = {fetch: D + E,  seed: E + 2S + R,  late: 2S + R + W}
  preflight (before the build, predicted):  max(B_peak + W, B_kept + W + max(data phases)) + margin
  prepare   (after the build, MEASURED free): max(data phases) + margin
  margin    = max(1 GiB, 10% of the peak it is added to)   (first-run variance, checkout drift, logs)

W is counted in every phase because the Playwright browser cache may be restored at job start.
D and E come from each site's `capacity`; B_peak, B_kept, S and R per profile, and W, from
`capacity_model`, which records where each measured number came from. Both checks exit 1 with the
numbers when the measured free space is short. Neither deletes anything.

When the dataset directory and Docker's data root are different filesystems, `prepare` checks each pool
for its own share: the dataset pool for D + E, the Docker pool for 2S + R + W.
"""

from __future__ import annotations

import argparse
import json
import sys

GIB = 1 << 30
MIN_MARGIN = GIB
MARGIN_FRACTION = 0.10


class CapacityError(Exception):
    """The pins file's capacity model is malformed."""


def _int(d: dict, key: str, where: str) -> int:
    v = d.get(key) if isinstance(d, dict) else None
    if type(v) is not int or v < 0:
        raise CapacityError(f"{where}.{key} must be a non-negative integer")
    return v


def load_profile(pins: dict, profile: str) -> dict:
    model = pins.get("capacity_model")
    if not isinstance(model, dict):
        raise CapacityError("pins file has no capacity_model")
    profiles = model.get("profiles", {})
    if profile not in profiles:
        raise CapacityError(f"unknown profile {profile!r} (have {sorted(profiles)})")
    p = profiles[profile]
    sites = p.get("sites")
    if not isinstance(sites, list) or not sites or len(set(sites)) != len(sites):
        raise CapacityError(f"profiles.{profile}.sites must be a non-empty list without repeats")
    d = e = 0
    for s in sites:
        if s not in pins.get("sites", {}):
            raise CapacityError(f"profiles.{profile} names unknown site {s!r}")
        cap = pins["sites"][s].get("capacity", {})
        d += _int(cap, "download_bytes", f"sites.{s}.capacity")
        e += _int(cap, "extracted_bytes", f"sites.{s}.capacity")
    m = {
        "profile": profile, "sites": sites, "download": d, "extracted": e,
        "build_peak": _int(p, "build_peak_bytes", f"profiles.{profile}"),
        "build_kept": _int(p, "build_kept_bytes", f"profiles.{profile}"),
        "seed": _int(p, "seed_bytes", f"profiles.{profile}"),
        "runtime": _int(p, "runtime_bytes", f"profiles.{profile}"),
        "late": _int(model, "late_bytes", "capacity_model"),
    }
    if m["build_kept"] > m["build_peak"]:
        raise CapacityError(f"profiles.{profile}: build_kept_bytes exceeds build_peak_bytes")
    return m


def margin(peak: int) -> int:
    return max(MIN_MARGIN, int(peak * MARGIN_FRACTION))


def data_phases(m: dict) -> dict:
    grow = 2 * m["seed"] + m["runtime"]
    return {"fetch": m["download"] + m["extracted"],
            "seed": m["extracted"] + grow,
            "late": grow + m["late"]}


def prepare_need(m: dict) -> int:
    """Free bytes needed after the build + prune, on one filesystem."""
    peak = max(data_phases(m).values())
    return peak + margin(peak)


def prepare_need_split(m: dict) -> tuple[int, int]:
    """(dataset pool, docker pool) after the build + prune, on two filesystems."""
    ds = m["download"] + m["extracted"]
    dk = 2 * m["seed"] + m["runtime"] + m["late"]
    return ds + margin(ds), dk + margin(dk)


def plan_phases(m: dict) -> dict:
    """Whole-job peak per phase on ONE filesystem, for the preflight before the build."""
    out = {"build": m["build_peak"] + m["late"]}
    for k, v in data_phases(m).items():
        out[k] = m["build_kept"] + m["late"] + v if k != "late" else m["build_kept"] + v
    return out


def preflight_need(m: dict) -> int:
    peak = max(plan_phases(m).values())
    return peak + margin(peak)


def gib(n: int) -> str:
    return f"{n / GIB:.2f} GiB ({n:,} bytes)"


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Disk-capacity phase model for the hosted UI workflows.")
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("sites", "preflight", "prepare"):
        sp = sub.add_parser(name)
        sp.add_argument("profile")
        sp.add_argument("--pins", required=True)
        if name == "preflight":
            sp.add_argument("--free", type=int, required=True)
        if name == "prepare":
            sp.add_argument("--dataset-free", type=int, required=True)
            sp.add_argument("--docker-free", type=int, required=True)
            sp.add_argument("--same-fs", action="store_true")
    a = ap.parse_args(argv)
    try:
        with open(a.pins, encoding="utf-8") as fh:
            m = load_profile(json.load(fh), a.profile)
    except (OSError, ValueError, CapacityError) as e:
        print(f"ci_capacity: FAILED: {e}", file=sys.stderr)
        return 2
    if a.cmd == "sites":
        print(" ".join(m["sites"]))
        return 0
    print(f"ci_capacity: {a.profile}: sites {', '.join(m['sites'])}")
    for k in ("download", "extracted", "build_peak", "build_kept", "seed", "runtime", "late"):
        print(f"ci_capacity:   {k:10s} {gib(m[k])}")
    if a.cmd == "preflight":
        for k, v in plan_phases(m).items():
            print(f"ci_capacity:   phase {k:6s} holds {gib(v)}")
        need = preflight_need(m)
        print(f"ci_capacity: whole job, one filesystem: need {gib(need)}, measured free {gib(a.free)}")
        ok = a.free >= need
    elif a.same_fs:
        for k, v in data_phases(m).items():
            print(f"ci_capacity:   phase {k:6s} adds {gib(v)}")
        need = prepare_need(m)
        print(f"ci_capacity: after build (one filesystem): need {gib(need)}, measured free {gib(a.dataset_free)}")
        ok = a.dataset_free >= need
    else:
        ds, dk = prepare_need_split(m)
        print(f"ci_capacity: after build, dataset pool: need {gib(ds)}, measured free {gib(a.dataset_free)}")
        print(f"ci_capacity: after build, docker pool:  need {gib(dk)}, measured free {gib(a.docker_free)}")
        ok = a.dataset_free >= ds and a.docker_free >= dk
    if not ok:
        print("ci_capacity: FAILED: insufficient disk for this workflow's phases (see above). Nothing was "
              "deleted to make room. A smaller pinned PR dataset edition is the planned remedy (ADR 0100).",
              file=sys.stderr)
        return 1
    print("ci_capacity: OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
