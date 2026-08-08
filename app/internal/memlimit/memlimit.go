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
// The ceiling lives in compose and differs per environment (8g on the
// base stack, 10g under the CI resource override, whatever an operator
// sets via AA_APP_MEM_LIMIT in production). A static
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
	// cgroup ceiling for everything GOMEMLIMIT does NOT cover.
	//
	// 80%, not the ecosystem's usual 90% (automemlimit), because this
	// process is not a pure Go process. Preview rendering shells out to
	// ffmpeg, ghostscript, pdftoppm, unar, ImageMagick and a
	// node+headless-chromium three.js worker; every one of those runs in
	// the app container's OWN cgroup, so their resident memory is
	// charged against the same ceiling while being invisible to
	// GOMEMLIMIT. A CI-profile render storm measured 1.0 GB of non-Go
	// anonymous RSS at peak (#887) — more than the 10% a 6g ceiling
	// would have reserved. 20% is sized from that measurement.
	//
	// This is a REserve, not a throttle: at the measured peak the Go
	// runtime sat at 76% of its derived limit, so the ratio has never
	// been what bounds the heap. Shrink it further only against a new
	// measurement of non-Go RSS, never on taste.
	DefaultRatio = 0.8

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

// Kind classifies which of the mutually exclusive paths produced the
// runtime's effective GOMEMLIMIT. The string is logged at boot, so an
// operator reading a log — or a failed CI run six weeks later — can
// answer "is the derivation taking effect?" without inspecting the
// container. That question went unanswerable through the whole #886 /
// #887 investigation, which is why it is now a machine-readable field
// rather than prose inside Source.
const (
	// KindEnv — an explicit GOMEMLIMIT in the environment won. The
	// derivation deliberately stood down.
	KindEnv = "explicit_env"
	// KindDerived — the ceiling was read from the cgroup and the ratio
	// applied. The intended production path.
	KindDerived = "derived_from_cgroup"
	// KindDisabled — AA_GOMEMLIMIT_RATIO <= 0 switched the derivation
	// off on purpose.
	KindDisabled = "disabled_by_ratio"
	// KindNoCgroupLimit — the process is not under a cgroup memory
	// ceiling (bare metal, dev host, unconstrained container). Normal,
	// not a failure — but it means nothing bounds the heap.
	KindNoCgroupLimit = "no_cgroup_limit"
	// KindInvalid — a cgroup limit was found but the derived value was
	// not usable. Should not happen; logged rather than swallowed.
	KindInvalid = "derived_invalid"
)

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
	// Kind is one of the Kind* constants above.
	Kind string
	// Source is the cgroup file read, or a reason string when the
	// runtime was left alone.
	Source string
	// Effective is the runtime's GOMEMLIMIT as the RUNTIME reports it
	// after Apply returns — read back rather than assumed, because
	// "what we intended to set" and "what is in force" are exactly the
	// two things #888 needed distinguished. math.MaxInt64 means no
	// limit is in force.
	Effective int64
}

// Effective returns the runtime's current GOMEMLIMIT without changing
// it. A negative argument to debug.SetMemoryLimit is documented as a
// pure read.
func Effective() int64 { return debug.SetMemoryLimit(-1) }

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
		return Result{
			Kind:      KindEnv,
			Source:    "GOMEMLIMIT set in environment; runtime value left as-is",
			Effective: Effective(),
		}
	}
	if ratio <= 0 {
		return Result{
			Ratio:     ratio,
			Kind:      KindDisabled,
			Source:    "ratio <= 0; disabled by configuration",
			Effective: Effective(),
		}
	}
	if ratio > 1 {
		// Above the cgroup ceiling the limit cannot help — the kernel
		// kills the process before the collector reacts. Clamp instead
		// of refusing so a fat-fingered value degrades to "safe".
		ratio = 1
	}

	cgLimit, source, err := Detect()
	if err != nil {
		return Result{
			Ratio:     ratio,
			Kind:      KindNoCgroupLimit,
			Source:    "no cgroup memory limit; runtime default left in place",
			Effective: Effective(),
		}
	}

	limit := int64(math.Floor(float64(cgLimit) * ratio))
	if limit <= 0 {
		return Result{
			CgroupLimit: cgLimit,
			Ratio:       ratio,
			Kind:        KindInvalid,
			Source:      "derived limit <= 0; runtime default left in place",
			Effective:   Effective(),
		}
	}
	debug.SetMemoryLimit(limit)
	return Result{
		Applied:     true,
		CgroupLimit: cgLimit,
		Limit:       limit,
		Ratio:       ratio,
		Kind:        KindDerived,
		Source:      source,
		Effective:   Effective(),
	}
}
