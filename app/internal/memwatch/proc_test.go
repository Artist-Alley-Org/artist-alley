// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package memwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProc writes a synthetic /proc tree. rssPages is deliberately in
// PAGES, matching what the kernel puts in field 24, so the test would
// catch a unit slip in the reader rather than agreeing with it.
type fakeProcEntry struct {
	pid, ppid int
	comm      string
	state     string
	rssPages  int64
}

func writeFakeProc(t *testing.T, entries []fakeProcEntry) string {
	t.Helper()
	root := t.TempDir()
	for _, e := range entries {
		dir := filepath.Join(root, fmt.Sprint(e.pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Fields after comm: state ppid pgrp session tty tpgid flags
		// minflt cminflt majflt cmajflt utime stime cutime cstime
		// priority nice threads itreal starttime vsize rss
		fields := []string{e.state, fmt.Sprint(e.ppid)}
		for i := 0; i < 19; i++ {
			fields = append(fields, "0")
		}
		fields = append(fields, fmt.Sprint(e.rssPages))
		line := fmt.Sprintf("%d (%s) %s\n", e.pid, e.comm, strings.Join(fields, " "))
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A non-numeric entry, exactly as the real /proc carries
	// (self, sys, meminfo...). The scan must skip it, not fail on it.
	if err := os.MkdirAll(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestScanDescendantsWalksTheWholeSubtree(t *testing.T) {
	// The shape that motivated walking descendants rather than direct
	// children: the app spawns node, node spawns chrome, chrome spawns
	// a crashpad handler. Counting only direct children attributes
	// ~0 bytes to the subtree that actually holds the memory.
	root := writeFakeProc(t, []fakeProcEntry{
		{pid: 1, ppid: 0, comm: "aa", state: "S", rssPages: 100},
		{pid: 10, ppid: 1, comm: "node", state: "S", rssPages: 200},
		{pid: 11, ppid: 10, comm: "chrome", state: "R", rssPages: 1000},
		{pid: 12, ppid: 11, comm: "chrome_crashpad_handler", state: "S", rssPages: 50},
		{pid: 20, ppid: 1, comm: "ffmpeg", state: "R", rssPages: 300},
		// A zombie: the #890 signature. Holds no RSS but must be
		// counted, because an accumulating count is the whole signal.
		{pid: 21, ppid: 1, comm: "ffprobe", state: "Z", rssPages: 0},
		// Not ours — a sibling process that must not be attributed to
		// this container's app.
		{pid: 99, ppid: 0, comm: "postgres", state: "S", rssPages: 9999},
	})

	p := scanDescendants(root, 1, true)
	if !p.Available {
		t.Fatal("plane not available")
	}
	if p.Count != 5 {
		t.Fatalf("descendant count = %d, want 5 (node, chrome, crashpad, ffmpeg, ffprobe)", p.Count)
	}
	if p.Zombies != 1 {
		t.Fatalf("zombies = %d, want 1", p.Zombies)
	}
	wantRSS := (200 + 1000 + 50 + 300) * pageSize
	if p.RSSBytes != wantRSS {
		t.Fatalf("rss = %d, want %d", p.RSSBytes, wantRSS)
	}
	// Largest first, so the line's first entry is always the suspect.
	if p.Top[0].Comm != "chrome" {
		t.Fatalf("top comm = %q, want chrome", p.Top[0].Comm)
	}
	if len(p.All) != 5 {
		t.Fatalf("full table len = %d, want 5", len(p.All))
	}
}

func TestScanDescendantsSkipsFullTableUnlessAsked(t *testing.T) {
	root := writeFakeProc(t, []fakeProcEntry{
		{pid: 1, ppid: 0, comm: "aa", state: "S", rssPages: 1},
		{pid: 2, ppid: 1, comm: "ffmpeg", state: "S", rssPages: 1},
	})
	if p := scanDescendants(root, 1, false); p.All != nil {
		t.Fatalf("ordinary sample retained a per-PID slice: %v", p.All)
	}
}

func TestReadProcStatHandlesCommWithSpacesAndParens(t *testing.T) {
	// "(chrome (deleted))" is a real comm after a binary is replaced
	// under a running process. Splitting the line on whitespace shifts
	// every later field, which silently misreads ppid and rss as
	// whatever landed there — a wrong number, not an error.
	root := writeFakeProc(t, []fakeProcEntry{
		{pid: 7, ppid: 3, comm: "chrome (deleted)", state: "S", rssPages: 42},
	})
	got, ok := readProcStat(root, 7)
	if !ok {
		t.Fatal("parse failed")
	}
	if got.Comm != "chrome (deleted)" {
		t.Fatalf("comm = %q", got.Comm)
	}
	if got.PPID != 3 {
		t.Fatalf("ppid = %d, want 3", got.PPID)
	}
	if got.RSSBytes != 42*pageSize {
		t.Fatalf("rss = %d, want %d", got.RSSBytes, 42*pageSize)
	}
}

func TestRollupCardinalityIsBounded(t *testing.T) {
	// Cardinality is the property being defended: one field per PID
	// would make the sample line unbounded and unaggregatable.
	entries := []fakeProcEntry{{pid: 1, ppid: 0, comm: "aa", state: "S"}}
	for i := 0; i < 20; i++ {
		entries = append(entries, fakeProcEntry{
			pid: 100 + i, ppid: 1,
			comm:     fmt.Sprintf("worker%02d", i),
			state:    "S",
			rssPages: int64(100 - i), // descending, so ordering is checkable
		})
	}
	p := scanDescendants(writeFakeProc(t, entries), 1, false)

	if len(p.Top) != maxCommsReported+1 {
		t.Fatalf("rollup entries = %d, want %d (%d + other)", len(p.Top), maxCommsReported+1, maxCommsReported)
	}
	last := p.Top[len(p.Top)-1]
	if last.Comm != "other" {
		t.Fatalf("last rollup = %q, want other", last.Comm)
	}
	if last.Count != 20-maxCommsReported {
		t.Fatalf("other count = %d, want %d", last.Count, 20-maxCommsReported)
	}
	// The bucketing must not lose bytes: the total is exact whatever
	// falls into "other".
	var sum int64
	for _, r := range p.Top {
		sum += r.RSSBytes
	}
	if sum != p.RSSBytes {
		t.Fatalf("rollup total %d != plane total %d", sum, p.RSSBytes)
	}
}

func TestScanDescendantsMissingProcRoot(t *testing.T) {
	p := scanDescendants(filepath.Join(t.TempDir(), "nope"), 1, false)
	if p.Available {
		t.Fatal("plane reported available with no /proc")
	}
}
