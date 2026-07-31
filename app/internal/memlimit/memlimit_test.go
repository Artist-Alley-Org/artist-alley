// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package memlimit

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "limit")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return p
}

func TestReadV2_RealLimit(t *testing.T) {
	// The exact shape CI's `mem_limit: 4g` produces.
	got, err := readV2(writeTemp(t, "4294967296\n"))
	if err != nil {
		t.Fatalf("readV2: %v", err)
	}
	if want := int64(4 * 1024 * 1024 * 1024); got != want {
		t.Errorf("limit = %d, want %d", got, want)
	}
}

func TestReadV2_MaxMeansUnlimited(t *testing.T) {
	// cgroup v2 spells "no limit" as the literal word `max`. Parsing
	// this as a number would fail; treating a parse failure as "no
	// limit" would be right by accident, so assert the explicit path.
	if _, err := readV2(writeTemp(t, "max\n")); !errors.Is(err, ErrNoLimit) {
		t.Errorf("err = %v, want ErrNoLimit", err)
	}
}

func TestReadV1_SentinelMeansUnlimited(t *testing.T) {
	// cgroup v1 has no `max` keyword — an unconstrained cgroup reports
	// a sentinel near PAGE_COUNTER_MAX. Taking that literally would
	// hand the runtime a ~9 exabyte ceiling, i.e. silently no ceiling
	// at all while logging that one was applied.
	for _, s := range []string{
		"9223372036854771712", // PAGE_COUNTER_MAX, 4 KiB pages
		"9223372036854775807", // int64 max
	} {
		if _, err := readV1(writeTemp(t, s+"\n")); !errors.Is(err, ErrNoLimit) {
			t.Errorf("readV1(%s) err = %v, want ErrNoLimit", s, err)
		}
	}
}

func TestReadV1_RealLimit(t *testing.T) {
	got, err := readV1(writeTemp(t, "2147483648\n"))
	if err != nil {
		t.Fatalf("readV1: %v", err)
	}
	if want := int64(2 * 1024 * 1024 * 1024); got != want {
		t.Errorf("limit = %d, want %d", got, want)
	}
}

func TestRead_MissingFileIsNotALimit(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := readV2(missing); err == nil {
		t.Error("readV2 on missing file: want error")
	}
	if _, err := readV1(missing); err == nil {
		t.Error("readV1 on missing file: want error")
	}
}

func TestParseLimit_Garbage(t *testing.T) {
	if _, err := parseLimit("not-a-number"); err == nil {
		t.Error("want parse error")
	}
	if _, err := parseLimit("0"); !errors.Is(err, ErrNoLimit) {
		t.Error("zero should read as no limit")
	}
	if _, err := parseLimit("-1"); !errors.Is(err, ErrNoLimit) {
		t.Error("negative should read as no limit")
	}
}

// restoreMemLimit puts the runtime back where the test found it, so an
// Apply test cannot leak a ceiling into the rest of the package's tests.
func restoreMemLimit(t *testing.T) {
	t.Helper()
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
}

func TestApply_RespectsExplicitGOMEMLIMIT(t *testing.T) {
	restoreMemLimit(t)
	t.Setenv("GOMEMLIMIT", "512MiB")
	res := Apply(DefaultRatio)
	if res.Applied {
		t.Error("Apply overrode an operator-set GOMEMLIMIT; it must defer")
	}
}

func TestApply_RatioZeroDisables(t *testing.T) {
	restoreMemLimit(t)
	t.Setenv("GOMEMLIMIT", "")
	res := Apply(0)
	if res.Applied {
		t.Error("ratio 0 must disable; this is the off switch used to profile unbounded")
	}
}

func TestApply_NoCgroupLeavesRuntimeAlone(t *testing.T) {
	restoreMemLimit(t)
	t.Setenv("GOMEMLIMIT", "")
	before := debug.SetMemoryLimit(-1)

	// This asserts behaviour only when the test host is genuinely
	// unconstrained; under a memory-capped container Apply SHOULD
	// apply, and asserting otherwise would fail for the right reason
	// in the wrong environment.
	if _, _, err := Detect(); !errors.Is(err, ErrNoLimit) {
		t.Skip("test host is under a cgroup memory limit; nothing to assert")
	}
	res := Apply(DefaultRatio)
	if res.Applied {
		t.Error("no cgroup limit must leave the runtime default in place")
	}
	if after := debug.SetMemoryLimit(-1); after != before {
		t.Errorf("memory limit changed %d -> %d", before, after)
	}
}

func TestApply_RatioAboveOneIsClamped(t *testing.T) {
	restoreMemLimit(t)
	t.Setenv("GOMEMLIMIT", "")
	if _, _, err := Detect(); err != nil {
		t.Skip("no cgroup limit here; clamping is only observable when one exists")
	}
	res := Apply(4.0)
	if !res.Applied {
		t.Fatal("want applied")
	}
	if res.Ratio != 1 {
		t.Errorf("ratio = %v, want clamped to 1", res.Ratio)
	}
	if res.Limit > res.CgroupLimit {
		t.Errorf("limit %d exceeds cgroup ceiling %d", res.Limit, res.CgroupLimit)
	}
}

func TestApply_DerivedLimitUnderCeiling(t *testing.T) {
	restoreMemLimit(t)
	t.Setenv("GOMEMLIMIT", "")
	if _, _, err := Detect(); err != nil {
		t.Skip("no cgroup limit on this host")
	}
	res := Apply(DefaultRatio)
	if !res.Applied {
		t.Fatal("want applied")
	}
	// The headroom is the whole point: GOMEMLIMIT covers the Go heap,
	// not thread stacks or the binary itself.
	if res.Limit >= res.CgroupLimit {
		t.Errorf("limit %d leaves no headroom under ceiling %d", res.Limit, res.CgroupLimit)
	}
	want := int64(math.Floor(float64(res.CgroupLimit) * DefaultRatio))
	if res.Limit != want {
		t.Errorf("limit = %d, want %d", res.Limit, want)
	}
	if got := debug.SetMemoryLimit(-1); got != res.Limit {
		t.Errorf("runtime limit = %d, want %d — Apply reported a value it did not install", got, res.Limit)
	}
}
