// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package memwatch

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

// cgroupPlane is the second plane of a sample: what the kernel accounts
// against the container, which is the ONLY number the OOM killer reads.
//
// The Go runtime's view and this view can disagree by gigabytes — every
// byte held by a spawned ffmpeg/chromium child, every cgo buffer, every
// page of page cache charged to us. Reporting only the runtime's view
// would produce a sampler that says "heap is fine" while the container
// is being killed, which is the failure mode this package exists to
// prevent.
type cgroupPlane struct {
	// Available is false on a host with no cgroup memory controller
	// visible (bare-metal dev runs, macOS, some CI sandboxes). That is
	// a normal condition, not an error: the sampler keeps running and
	// reports the two planes it can see.
	Available bool
	// Version is 2 or 1.
	Version int
	// CurrentBytes is total charged memory (anon + file + kernel).
	CurrentBytes int64
	// PeakBytes is the high-water mark since the cgroup was created.
	// cgroup v2 exposes this as memory.peak only on Linux 5.19+; 0
	// means "this kernel does not offer it", not "no memory used".
	PeakBytes int64
	// AnonBytes is anonymous memory — the figure the kernel's OOM
	// report prints as anon-rss and therefore the one that must be
	// compared against past kill evidence.
	AnonBytes int64
	// FileBytes is page cache charged to the cgroup. It is reclaimable
	// under pressure, so it inflates CurrentBytes without being a
	// cause of death; carrying it separately stops it from being
	// mistaken for growth.
	FileBytes int64
	// LimitBytes is the ceiling, 0 when unlimited.
	LimitBytes int64
}

// UsedFraction is CurrentBytes/LimitBytes, or 0 when there is no limit
// to be a fraction of. This is what the threshold capture triggers on:
// deliberately the TOTAL charge rather than anon alone, because that is
// what the kernel compares against memory.max.
func (c cgroupPlane) UsedFraction() float64 {
	if !c.Available || c.LimitBytes <= 0 {
		return 0
	}
	return float64(c.CurrentBytes) / float64(c.LimitBytes)
}

// cgroup v2 unified-hierarchy paths. Docker with a cgroup namespace
// (the default) mounts the CONTAINER's own cgroup at this root, so
// these read the container's numbers and not the host's.
const (
	v2Root = "/sys/fs/cgroup"
	v1Root = "/sys/fs/cgroup/memory"
)

// readCgroup reads whichever hierarchy is present. root is the mount
// point, overridable so tests can drive it from a fixture directory.
func readCgroup(root string) cgroupPlane {
	if p, ok := readCgroupV2(root); ok {
		return p
	}
	if p, ok := readCgroupV1(root + "/memory"); ok {
		return p
	}
	return cgroupPlane{}
}

func readCgroupV2(dir string) (cgroupPlane, bool) {
	cur, err := readUint(dir + "/memory.current")
	if err != nil {
		return cgroupPlane{}, false
	}
	p := cgroupPlane{Available: true, Version: 2, CurrentBytes: cur}
	// memory.peak landed in Linux 5.19. Absence is expected on older
	// kernels and must not sink the whole plane.
	if peak, err := readUint(dir + "/memory.peak"); err == nil {
		p.PeakBytes = peak
	}
	if lim, err := readLimit(dir + "/memory.max"); err == nil {
		p.LimitBytes = lim
	}
	stat := readKeyedFile(dir + "/memory.stat")
	p.AnonBytes = stat["anon"]
	p.FileBytes = stat["file"]
	return p, true
}

func readCgroupV1(dir string) (cgroupPlane, bool) {
	cur, err := readUint(dir + "/memory.usage_in_bytes")
	if err != nil {
		return cgroupPlane{}, false
	}
	p := cgroupPlane{Available: true, Version: 1, CurrentBytes: cur}
	if peak, err := readUint(dir + "/memory.max_usage_in_bytes"); err == nil {
		p.PeakBytes = peak
	}
	if lim, err := readLimit(dir + "/memory.limit_in_bytes"); err == nil {
		p.LimitBytes = lim
	}
	stat := readKeyedFile(dir + "/memory.stat")
	// v1 names the same quantities differently.
	p.AnonBytes = stat["rss"]
	p.FileBytes = stat["cache"]
	return p, true
}

// unlimitedThreshold — cgroup v1 spells "no limit" as a sentinel near
// PAGE_COUNTER_MAX whose exact value varies with page size and kernel
// version. Anything at or above 1 PiB is a sentinel, not a ceiling
// anybody set. (Same rule as internal/memlimit, kept local so this
// package reads /sys without depending on that one.)
const unlimitedThreshold int64 = 1 << 50

var errNoLimit = errors.New("memwatch: no cgroup memory limit")

func readLimit(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, errNoLimit
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if n <= 0 || n >= unlimitedThreshold {
		return 0, errNoLimit
	}
	return n, nil
}

func readUint(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// readKeyedFile parses the "key value" line format used by memory.stat.
// A missing or malformed file yields an empty map rather than an error:
// anon/file are enrichment, and losing them must not cost us
// memory.current.
func readKeyedFile(path string) map[string]int64 {
	out := map[string]int64{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		out[k] = n
	}
	return out
}
