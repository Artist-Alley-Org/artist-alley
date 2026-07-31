// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package memlimit derives GOMEMLIMIT from the container's own cgroup
// memory ceiling and hands it to the Go runtime at boot.
//
// # Why this exists
//
// The Go runtime does NOT read cgroup memory limits. GOGC (default 100)
// paces the collector purely as a RATIO of the live heap: the next GC
// targets 2x the live set, with no upper bound of any kind. A process
// whose live set is 1.8 GB will therefore grow to ~3.6 GB of heap before
// the collector runs — and if the cgroup ceiling is 4 GB, the kernel OOM
// killer wins the race. Nothing in the runtime knows the ceiling exists.
// (golang/go#75164 proposes making the runtime cgroup-aware; unresolved
// as of Go 1.26.)
//
// GOMEMLIMIT is the knob that supplies the missing ceiling. It is a SOFT
// limit: as the heap approaches it the collector runs more often, and
// total GC CPU is capped at 50% so a live set that genuinely does not fit
// exceeds the limit rather than thrashing forever. A single oversized
// allocation is always granted, so setting it cannot turn an allocation
// into a panic.
//
// # Why derive it instead of baking it in
//
// The ceiling lives in compose and differs per environment (4g under the
// CI resource override, unset in the base/production stack). A static
// ENV GOMEMLIMIT in the Dockerfile would be correct for exactly one
// environment and silently wrong in every other, with no signal that the
// two had drifted apart. Reading the cgroup means the runtime's ceiling
// is the container's ceiling by construction — they cannot disagree.
//
// When no cgroup limit is set, we leave the runtime alone rather than
// inventing a number from host RAM.
package memlimit

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	// cgroup v2 unified hierarchy. Docker with a cgroup namespace
	// (the default) mounts the container's own cgroup at this root,
	// so the container reads its own limit here.
	v2Path = "/sys/fs/cgroup/memory.max"
	// cgroup v1 memory controller.
	v1Path = "/sys/fs/cgroup/memory/memory.limit_in_bytes"

	// DefaultRatio leaves headroom between the Go heap ceiling and the
	// cgroup ceiling for everything GOMEMLIMIT does NOT cover: thread
	// stacks, the binary's own text/data, and any memory the runtime
	// has not yet returned to the OS. 90% matches the de-facto default
	// used by the ecosystem (automemlimit).
	DefaultRatio = 0.9

	// unlimitedThreshold — cgroup v1 spells "no limit" as a sentinel
	// near PAGE_COUNTER_MAX rather than a word, and the exact value
	// varies with page size and kernel version. Anything at or above
	// this (1 PiB) is a sentinel, not a real ceiling anybody set.
	unlimitedThreshold int64 = 1 << 50
)

// ErrNoLimit reports that the process is not under a cgroup memory
// ceiling — either no controller is present or it is explicitly
// unlimited. Callers should leave the runtime's default in place.
var ErrNoLimit = errors.New("memlimit: no cgroup memory limit set")

// Detect returns the process's cgroup memory ceiling in bytes, plus the
// path it was read from. It returns ErrNoLimit when the process is
// unconstrained, which is a normal condition and not a failure.
func Detect() (limit int64, source string, err error) {
	// v2 first: on a unified-hierarchy host the v1 path does not exist,
	// and on a hybrid host v2 is the authoritative one for the limit.
	if n, err := readV2(v2Path); err == nil {
		return n, v2Path, nil
	} else if errors.Is(err, ErrNoLimit) {
		return 0, v2Path, ErrNoLimit
	}
	if n, err := readV1(v1Path); err == nil {
		return n, v1Path, nil
	} else if errors.Is(err, ErrNoLimit) {
		return 0, v1Path, ErrNoLimit
	}
	return 0, "", ErrNoLimit
}

// readV2 parses a cgroup v2 memory.max file. The literal "max" means
// unconstrained.
func readV2(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, ErrNoLimit
	}
	return parseLimit(s)
}

// readV1 parses a cgroup v1 memory.limit_in_bytes file. "No limit" is a
// sentinel close to int64 max rather than a word.
func readV1(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseLimit(strings.TrimSpace(string(b)))
}

// parseLimit converts a raw cgroup byte count, mapping non-positive and
// sentinel-large values onto ErrNoLimit.
func parseLimit(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memlimit: parse %q: %w", s, err)
	}
	if n <= 0 || n >= unlimitedThreshold {
		return 0, ErrNoLimit
	}
	return n, nil
}

// Result describes what Apply did, for the caller to log.
type Result struct {
	// Applied is false when the runtime was deliberately left alone.
	Applied bool
	// CgroupLimit is the ceiling read from the cgroup, 0 if none.
	CgroupLimit int64
	// Limit is the value handed to debug.SetMemoryLimit.
	Limit int64
	// Ratio actually used.
	Ratio float64
	// Source is the cgroup file read, or a reason string when the
	// runtime was left alone.
	Source string
}

// Apply derives a soft heap ceiling from the cgroup limit and installs it
// via debug.SetMemoryLimit. It is a no-op — reporting Applied false — in
// three cases, each deliberate:
//
//   - GOMEMLIMIT is already set in the environment. The operator (or the
//     runtime, at startup) has spoken; we do not second-guess an explicit
//     value with a derived one.
//   - ratio <= 0. The documented off switch, used to profile the
//     unbounded behaviour on purpose.
//   - No cgroup ceiling exists. We do not synthesise a limit from host
//     RAM; an unconstrained process keeps the runtime default.
func Apply(ratio float64) Result {
	if v := strings.TrimSpace(os.Getenv("GOMEMLIMIT")); v != "" {
		return Result{Source: "GOMEMLIMIT set in environment; runtime value left as-is"}
	}
	if ratio <= 0 {
		return Result{Ratio: ratio, Source: "ratio <= 0; disabled by configuration"}
	}
	if ratio > 1 {
		// Above the cgroup ceiling the limit cannot help — the kernel
		// kills the process before the collector reacts. Clamp instead
		// of refusing so a fat-fingered value degrades to "safe".
		ratio = 1
	}

	cgLimit, source, err := Detect()
	if err != nil {
		return Result{Ratio: ratio, Source: "no cgroup memory limit; runtime default left in place"}
	}

	limit := int64(math.Floor(float64(cgLimit) * ratio))
	if limit <= 0 {
		return Result{CgroupLimit: cgLimit, Ratio: ratio, Source: "derived limit <= 0; runtime default left in place"}
	}
	debug.SetMemoryLimit(limit)
	return Result{
		Applied:     true,
		CgroupLimit: cgLimit,
		Limit:       limit,
		Ratio:       ratio,
		Source:      source,
	}
}
