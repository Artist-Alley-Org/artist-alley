---
id: "0082"
title: The Go heap is bounded by the container's own ceiling, and every environment has one
status: accepted
date: 2026-07-31
area: infrastructure
phases: []
supersedes: []
related:
  - "0080"
tags:
  - infra
  - performance
  - ci
  - previews
excerpt: >-
  The Go runtime does not read cgroup limits, so the collector paced from the
  live-heap ratio alone and drove the app past its own 4 GiB ceiling into an OOM
  kill during preview rendering. GOMEMLIMIT is now derived from the cgroup at
  boot rather than hardcoded, and the base stack carries a ceiling too — the
  previous CI-only cap meant the failure was reproducible in CI and invisible
  everywhere else.
---

## Context

CI runs carried 20+ kernel OOM kills against the app container:

```
oom-kill:constraint=CONSTRAINT_MEMCG, task=aa
total-vm:6637132kB, anon-rss:3649884kB
```

The killed task is `aa`, the Go binary itself — 3.65 GB of anonymous RSS against
a `mem_limit: 4g`. The same condition produced the stall signature behind the
random `dev` failures: several requests taking ~8 s and completing within
microseconds of each other, which is a process that was not running and got
released together, not a CPU-starved one.

**The Go runtime does not read cgroup memory limits.** `GOGC` (default 100)
paces the collector purely as a ratio of the live heap: the next collection
targets twice the live set, with no upper bound of any kind. Nothing in the
runtime knows a container ceiling exists. (`golang/go#75164` proposes changing
this; unresolved as of Go 1.26.)

Note this is *not* "the runtime sizes its heap from host RAM" — a tempting
explanation given the 125 GB host against a 4 GB container, but not what
happens. Host memory does not enter GC pacing at all. The defect is the absence
of any ceiling, not a ceiling read from the wrong place.

## Decision

**GOMEMLIMIT is derived from the container's own cgroup at boot**
(`app/internal/memlimit`): read `memory.max` (v2) or `memory.limit_in_bytes`
(v1), apply 90 % of it via `debug.SetMemoryLimit`. An explicit `GOMEMLIMIT` in
the environment always wins; no cgroup ceiling means the runtime is left alone
rather than handed a number invented from host RAM.

A static `ENV GOMEMLIMIT` in the Dockerfile was rejected: the ceiling lives in
compose and differs per environment, so a baked-in value would be correct in
exactly one place and silently wrong everywhere else, with nothing to signal
that the two had drifted apart. Deriving it makes the runtime's ceiling and the
container's ceiling the same fact by construction.

Writing the ~90 lines rather than taking `automemlimit` follows the project's
standing preference for Go-native over an added dependency. The library's real
value is the v1/v2 split and the no-limit sentinel, both of which are covered
here by tests that assert the specific failure each one causes: cgroup v1 spells
"unlimited" as a value near `PAGE_COUNTER_MAX`, and taking it literally would
hand the runtime a ~9 exabyte ceiling while logging that a limit had been
applied.

**The base stack now carries an app ceiling too** (`AA_APP_MEM_LIMIT`, default
`4g`). Previously only the CI resource override capped the app; production
capped nothing. That silent difference was itself a defect — the same unbounded
growth existed on an uncapped host with nothing to stop it, expanding until the
whole machine was under pressure and degrading every other container rather than
the one at fault. Bounding the default path also means the derived-limit
mechanism is exercised by default instead of only under CI's override.

## Consequences

Measured on the 1,946-asset preview-heavy seed, sampling `/healthz` and an
authenticated API endpoint once per second across the whole run:

| | no GOMEMLIMIT | derived GOMEMLIMIT |
|---|---|---|
| peak NextGC target | **4407 MB** | 3151 MB |
| peak RSS | 3782 MB | 3413 MB |
| peak HeapSys | 5122 MB | 4150 MB |
| worst API latency | **4.79 s** | **0.150 s** |
| samples over 1 s | 1 | 0 |

The decisive number is `NextGC`: without a limit the runtime's own target heap
reached **4407 MB against a 4096 MB ceiling** — it was planning to grow past the
container limit, which is precisely the OOM. With the derived limit the target
stays under both the 3865 MB soft limit and the container ceiling.

The heap profile taken at peak (`inuse_space`, captured before any limit was
set) attributes the memory to preview variant generation:

```
986.70MB 71.46%  golang.org/x/image/draw.(*kernelScaler).makeTmpBuf
247.25MB 17.91%  image.NewYCbCr
 99.84MB  7.23%  image.NewRGBA
          ...
1303.84MB 94.43% preview.(*RasterHandler).Handle
1056.59MB 76.52%   └─ preview.(*RasterHandler).writeVariant
```

`makeTmpBuf` is `x/image/draw`'s kernel-scaler scratch buffer, sized
`4 × dstWidth × srcHeight` float64s — for a 6780×7071 source scaled to 2048 wide
that is a single ~460 MB allocation, and up to eight preview workers run
concurrently (`workerPoolSize` = NumCPU/2, capped at 8).

**Live set versus GC runway: predominantly runway, over a real but sub-ceiling
live set.** The scratch buffers are genuinely live during their own resize, so
eight concurrent workers do hold on the order of 1–2 GB legitimately; heap drops
back to ~50 MB between bursts, so the remainder was collectable garbage the
runtime simply had no reason to collect. Because the genuine live set sits well
under the ceiling, the limit has room to work and is a sufficient fix for the
OOM — this is not the pathological case where the live set alone exceeds the
limit.

Reducing the live half is a real but separate improvement (bounding per-resize
scratch, or capping raster-worker concurrency against a memory budget rather
than CPU count). It is not required to stop the kill and is not attempted here.

pprof itself is opt-in on a **separate listener** (`AA_PPROF_ADDR`, off by
default, never mounted on the application router) because a heap profile carries
live object contents — tokens, file bytes, DB rows. See `app/internal/debugsrv`.

## Amendment 2026-08-04 — the ceiling was too low, and the reserve was sized for a pure-Go process (#887)

The mechanism above works exactly as designed and is not changed. What changed
is both numbers it is parameterised by.

**The symptom.** Eleven `CONSTRAINT_MEMCG` OOM kills in ~16 hours on the CI/demo
host, every one `task=aa`, `anon-rss` 5.05–5.69 GB against the CI override's
`mem_limit: 6g`. Roughly one kill every 90 minutes, each surfacing downstream as
an unrelated-looking flake — connection errors, half-written data, timeouts.

**The measurement.** A local replay of the CI-profile seed (147 assets) plus its
preview render storm, sampling the cgroup and `runtime.MemStats` together every
three seconds. At the peak sample:

| | bytes | |
|---|---|---|
| cgroup `anon` | 5.39 GB | the number the kernel kills on |
| `memory.current` | 5.56 GB | against a 6.44 GB ceiling |
| Go footprint (`Sys − HeapReleased`) | 4.38 GB | what `GOMEMLIMIT` bounds |
| `HeapAlloc` / `HeapInuse` | 3.31 GB | **live**, not garbage |
| non-Go anonymous RSS | 1.00 GB | the balance |
| derived `GOMEMLIMIT` | 5.80 GB | the runtime sat at **76 %** of it |

**What that rules out.** The tempting story — that the heap dutifully respects
its ceiling while off-heap allocation pushes RSS past the container limit — is
not what happens. Off-heap is real but is a fifth of the peak, and the Go
runtime never approached its own limit, so GC pacing was never the binding
constraint. Lowering the ratio alone would not have prevented a single one of
these kills: 3.31 GB of the peak is *live* heap held by concurrently running
render jobs, and no amount of collection shrinks a live set. It would only have
spent GC CPU during the exact window where the render queue is already the
bottleneck.

**Where the non-Go gigabyte comes from.** This process is not a pure Go process.
Preview rendering shells out to `ffmpeg`, `ffprobe`, `ghostscript`, `pdftoppm`,
`unar`, ImageMagick `convert`, and a `node` + headless-chromium three.js worker.
Every one of those runs inside the app container's own cgroup, so its resident
memory is charged against the same ceiling while being completely invisible to
`GOMEMLIMIT`. The ecosystem's 90 % default assumes a reserve covering thread
stacks and unreturned pages — tens of megabytes. Ours has to cover a gigabyte of
child processes, and 10 % of a 6g ceiling (600 MB) never did.

**Decision.**

- `AA_APP_MEM_LIMIT` default `4g` → **`8g`** on the base stack, `6g` → **`10g`**
  under the CI resource override. The measured peak does not fit a 4g ceiling at
  all, so every preview-rendering instance on the default was living on the
  kernel's mercy. `mem_limit` is a ceiling and not a reservation, so unused
  headroom costs nothing.
- `DefaultRatio` `0.9` → **`0.8`**, sized from the 1.0 GB of measured non-Go RSS
  rather than from the ecosystem default. Shrink it further only against a new
  measurement.
- `AA_GOMEMLIMIT_RATIO` is now **forwarded into the container** by
  `docker-compose.yml`. It was documented as tunable but never appeared in the
  app service's `environment:` block, so setting it had no effect on any
  containerised deploy — the Go process simply never saw it.

**What is not decided here.** The 3.31 GB live heap is the real ceiling-setter
and nothing above reduces it. Bounding per-render scratch, or pacing preview
workers against a memory budget rather than CPU count — the improvement the
original Consequences section already deferred — remains open, now with a
measurement attached to it. Memory instrumentation to catch the next drift is
#888.
