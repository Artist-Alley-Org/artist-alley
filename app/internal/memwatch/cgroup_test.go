// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package memwatch

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadCgroupV2(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"memory.current": "5600000000\n",
		"memory.peak":    "5900000000\n",
		"memory.max":     "10737418240\n",
		"memory.stat":    "anon 5390000000\nfile 180000000\nkernel_stack 4096\n",
	})

	p := readCgroup(root)
	if !p.Available || p.Version != 2 {
		t.Fatalf("plane = %+v, want available v2", p)
	}
	if p.CurrentBytes != 5600000000 || p.PeakBytes != 5900000000 {
		t.Fatalf("current/peak = %d/%d", p.CurrentBytes, p.PeakBytes)
	}
	// anon is the figure the kernel's OOM report prints; keeping it
	// separate from `file` is what stops reclaimable page cache being
	// mistaken for growth.
	if p.AnonBytes != 5390000000 || p.FileBytes != 180000000 {
		t.Fatalf("anon/file = %d/%d", p.AnonBytes, p.FileBytes)
	}
	if got := p.UsedFraction(); got < 0.52 || got > 0.53 {
		t.Fatalf("used fraction = %v, want ~0.521", got)
	}
}

func TestReadCgroupV2NoPeakOrLimit(t *testing.T) {
	// memory.peak needs Linux 5.19+, and an uncapped container spells
	// its limit "max". Neither may sink the plane: losing memory.current
	// is what would blind the sampler.
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"memory.current": "12345\n",
		"memory.max":     "max\n",
	})
	p := readCgroup(root)
	if !p.Available {
		t.Fatal("plane unavailable")
	}
	if p.PeakBytes != 0 || p.LimitBytes != 0 {
		t.Fatalf("peak/limit = %d/%d, want 0/0", p.PeakBytes, p.LimitBytes)
	}
	if p.UsedFraction() != 0 {
		t.Fatalf("used fraction = %v with no limit, want 0", p.UsedFraction())
	}
}

func TestReadCgroupV1Fallback(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, filepath.Join(root, "memory"), map[string]string{
		"memory.usage_in_bytes":     "2000\n",
		"memory.max_usage_in_bytes": "3000\n",
		"memory.limit_in_bytes":     "8000\n",
		"memory.stat":               "rss 1500\ncache 400\n",
	})
	p := readCgroup(root)
	if !p.Available || p.Version != 1 {
		t.Fatalf("plane = %+v, want available v1", p)
	}
	if p.CurrentBytes != 2000 || p.PeakBytes != 3000 || p.LimitBytes != 8000 {
		t.Fatalf("plane = %+v", p)
	}
	if p.AnonBytes != 1500 || p.FileBytes != 400 {
		t.Fatalf("anon/file = %d/%d", p.AnonBytes, p.FileBytes)
	}
}

func TestReadCgroupV1UnlimitedSentinel(t *testing.T) {
	// cgroup v1 spells "no limit" as a near-int64-max sentinel rather
	// than a word. Read literally it becomes a ceiling of 8 exabytes,
	// which makes every usage fraction ~0 and the threshold capture
	// unreachable — a guard that cannot fire.
	root := t.TempDir()
	writeFiles(t, filepath.Join(root, "memory"), map[string]string{
		"memory.usage_in_bytes": "2000",
		"memory.limit_in_bytes": "9223372036854771712",
	})
	p := readCgroup(root)
	if p.LimitBytes != 0 {
		t.Fatalf("limit = %d, want 0 for the unlimited sentinel", p.LimitBytes)
	}
}

func TestReadCgroupAbsent(t *testing.T) {
	p := readCgroup(filepath.Join(t.TempDir(), "nothing-here"))
	if p.Available {
		t.Fatal("reported available with no cgroup files")
	}
	if p.UsedFraction() != 0 {
		t.Fatal("non-zero fraction from an unavailable plane")
	}
}
