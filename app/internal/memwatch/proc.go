// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package memwatch

import (
	"bytes"
	"os"
	"sort"
	"strconv"
)

// maxCommsReported bounds the cardinality of the per-command rollup in a
// sample line. A render storm can have dozens of children alive at once
// and their PIDs churn constantly; one log field per PID would make the
// sample line unbounded in width and unaggregatable across time. Eight
// distinct command names plus an "other" bucket has covered every child
// the preview pipeline spawns (ffmpeg, ffprobe, gs, pdftoppm, unar,
// magick, node, chrome, chrome_crashpad_handler) with room to spare,
// and the total RSS figure is exact regardless of how many fall into
// "other".
const maxCommsReported = 8

// procInfo is one process read out of /proc.
type procInfo struct {
	PID  int
	PPID int
	// Comm is the kernel's short command name (comm, max 15 chars).
	// It is what we aggregate by: it is stable per binary, unlike a
	// full argv that carries per-job paths and would explode the
	// cardinality we are deliberately bounding.
	Comm string
	// State is the single-letter /proc state: R, S, D, Z, T...
	State byte
	// RSSBytes is resident set size. Read from /proc/<pid>/stat field
	// 24 (pages) rather than a second open of /proc/<pid>/statm, so a
	// scan of N processes costs N file reads and not 2N.
	RSSBytes int64
}

// commRollup is per-command aggregated RSS, the bounded-cardinality
// unit a sample line reports.
type commRollup struct {
	Comm     string
	Count    int
	RSSBytes int64
}

// childPlane is the third plane of a sample: everything the app spawned
// that the Go runtime cannot see but the cgroup charges anyway.
//
// "Children" here means all DESCENDANTS, not just direct children. The
// preview pipeline's heaviest child is a node worker that itself forks
// headless chromium, which forks a crashpad handler; counting only
// direct children would attribute ~0 bytes to exactly the subtree that
// motivated this instrumentation (#890).
type childPlane struct {
	// Available is false when /proc could not be read at all. A zero
	// count with Available true is a real "no children", which is a
	// different fact from "we could not look".
	Available bool
	Count     int
	// Zombies counts descendants in state Z. They hold no RSS, so
	// they never explain memory growth — but an accumulating count is
	// the direct signature of a missing wait4 (#890), and it costs
	// one integer to carry it in every sample.
	Zombies int
	// RSSBytes is the SUM of every descendant's resident set size, and
	// it deliberately double-counts shared pages. A measured storm
	// showed 22 chromium processes summing to 2.2 GB inside a cgroup
	// charged 1.9 GB in total: they are forks of one zygote and share
	// most of their mapped pages, which the kernel charges once and
	// RSS reports per process.
	//
	// So this figure is a RANKING signal, not an accounting one — it
	// answers "which subtree is growing" and never "how much of the
	// ceiling is theirs". cg_anon minus the Go runtime's retained
	// memory is the accounting answer, and both appear in the same
	// line so the two can never be confused for one another.
	RSSBytes int64
	// Top is the per-command rollup, largest RSS first, capped at
	// maxCommsReported entries with the remainder folded into "other".
	Top []commRollup
	// All is the complete unaggregated list, populated only for the
	// threshold capture's full-table log line. Nil for ordinary
	// samples so a sample never retains a slice of every PID.
	All []procInfo
}

// procScanner walks /proc. It is a type rather than a function so the
// per-sample scratch — the stat read buffer, the pid maps, the path
// builder — is allocated once and reused. This runs every interval
// forever inside the process it is measuring; a sampler that allocated
// a megabyte per sample would be adding to the number it reports.
type procScanner struct {
	buf   []byte
	byPID map[int]procInfo
	kids  map[int][]int
	path  []byte
	queue []int
	seen  map[int]bool
}

func newProcScanner() *procScanner {
	return &procScanner{
		// 512 B holds every /proc/<pid>/stat line comfortably; the
		// reader tolerates a truncated read by failing the parse, and
		// a failed parse is a skipped process, not a broken sample.
		buf:   make([]byte, 512),
		byPID: make(map[int]procInfo, 256),
		kids:  make(map[int][]int, 256),
		path:  make([]byte, 0, 64),
		seen:  make(map[int]bool, 64),
	}
}

// scanDescendants is the allocating convenience form, used by tests and
// by one-shot callers.
func scanDescendants(procRoot string, self int, wantAll bool) childPlane {
	return newProcScanner().scan(procRoot, self, wantAll)
}

// scan walks procRoot (normally "/proc"), builds the parent map, and
// rolls up every descendant of self.
//
// Processes are read in one pass and the tree is walked afterwards.
// Reading is inherently racy — a PID can exit between the readdir and
// the read of its stat file — and every such miss is skipped silently.
// A memory sampler that failed because a child exited underneath it
// would be a sampler that fails hardest exactly when there is the most
// to see.
func (ps *procScanner) scan(procRoot string, self int, wantAll bool) childPlane {
	d, err := os.Open(procRoot)
	if err != nil {
		return childPlane{}
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return childPlane{}
	}

	byPID, kids := ps.byPID, ps.kids
	clear(byPID)
	clear(kids)
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue // non-numeric /proc entry: self, sys, meminfo...
		}
		info, ok := ps.readStat(procRoot, pid)
		if !ok {
			continue
		}
		byPID[pid] = info
		kids[info.PPID] = append(kids[info.PPID], pid)
	}

	plane := childPlane{Available: true}
	byComm := make(map[string]*commRollup)

	// Breadth-first from self. `seen` guards against a cycle, which
	// cannot happen in a sane process tree but can be synthesised by a
	// PID-reuse race mid-scan; without it that race is an infinite
	// loop inside the sampler.
	seen := ps.seen
	clear(seen)
	seen[self] = true
	queue := append(ps.queue[:0], kids[self]...)
	for head := 0; head < len(queue); head++ {
		pid := queue[head]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		info, ok := byPID[pid]
		if !ok {
			continue
		}
		queue = append(queue, kids[pid]...)

		plane.Count++
		if info.State == 'Z' {
			plane.Zombies++
		}
		plane.RSSBytes += info.RSSBytes
		if wantAll {
			plane.All = append(plane.All, info)
		}
		r, ok := byComm[info.Comm]
		if !ok {
			r = &commRollup{Comm: info.Comm}
			byComm[info.Comm] = r
		}
		r.Count++
		r.RSSBytes += info.RSSBytes
	}
	ps.queue = queue[:0]

	rollups := make([]commRollup, 0, len(byComm))
	for _, r := range byComm {
		rollups = append(rollups, *r)
	}
	sort.Slice(rollups, func(i, j int) bool {
		if rollups[i].RSSBytes != rollups[j].RSSBytes {
			return rollups[i].RSSBytes > rollups[j].RSSBytes
		}
		return rollups[i].Comm < rollups[j].Comm
	})
	if len(rollups) > maxCommsReported {
		other := commRollup{Comm: "other"}
		for _, r := range rollups[maxCommsReported:] {
			other.Count += r.Count
			other.RSSBytes += r.RSSBytes
		}
		rollups = append(rollups[:maxCommsReported:maxCommsReported], other)
	}
	plane.Top = rollups

	if wantAll {
		sort.Slice(plane.All, func(i, j int) bool {
			if plane.All[i].RSSBytes != plane.All[j].RSSBytes {
				return plane.All[i].RSSBytes > plane.All[j].RSSBytes
			}
			return plane.All[i].PID < plane.All[j].PID
		})
	}
	return plane
}

// pageSize is the resident-page size used to convert /proc/<pid>/stat's
// rss field into bytes. Linux reports that field in pages on every
// architecture we run on; os.Getpagesize is the same value.
var pageSize = int64(os.Getpagesize())

// readStat reads and parses /proc/<pid>/stat into the scanner's reused
// buffer. os.ReadFile would allocate a fresh slice per process per
// sample; /proc files report size 0, so ReadFile cannot even size its
// buffer and grows it by doubling.
func (ps *procScanner) readStat(procRoot string, pid int) (procInfo, bool) {
	ps.path = append(ps.path[:0], procRoot...)
	ps.path = append(ps.path, '/')
	ps.path = strconv.AppendInt(ps.path, int64(pid), 10)
	ps.path = append(ps.path, "/stat"...)

	f, err := os.Open(string(ps.path))
	if err != nil {
		return procInfo{}, false // exited between readdir and open
	}
	n, err := f.Read(ps.buf)
	_ = f.Close()
	if err != nil && n == 0 {
		return procInfo{}, false
	}
	return parseProcStat(pid, ps.buf[:n])
}

// readProcStat is the allocating one-shot form, used by tests.
func readProcStat(procRoot string, pid int) (procInfo, bool) {
	b, err := os.ReadFile(procRoot + "/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procInfo{}, false
	}
	return parseProcStat(pid, b)
}

// parseProcStat splits one /proc/<pid>/stat line.
//
// The comm field is the reason this is hand-parsed rather than
// Fields()-split: it is the executable's basename wrapped in
// parentheses and it may itself contain spaces AND parentheses
// ("(chrome (deleted))"). Splitting on whitespace shifts every
// subsequent field by an unpredictable amount, which silently
// misreads ppid and rss as whatever happened to land there. Slicing on
// the LAST ')' is the documented way to do this.
func parseProcStat(pid int, b []byte) (procInfo, bool) {
	open := bytes.IndexByte(b, '(')
	close := bytes.LastIndexByte(b, ')')
	if open < 0 || close < 0 || close < open {
		return procInfo{}, false
	}
	// string(...) here is the ONE deliberate copy: comm outlives the
	// scanner's reused buffer, so it cannot alias it. Everything else
	// is parsed in place.
	comm := string(b[open+1 : close])

	rest := b[close+1:]
	// Field indices after comm: 0=state 1=ppid ... 21=rss (in pages).
	// strings.Fields would allocate a ~40-entry slice per process per
	// sample to reach three of them; walk instead.
	const (
		fState = 0
		fPPID  = 1
		fRSS   = 21
	)
	var state byte
	var ppid int
	var rssPages int64
	seen := -1
	for i := 0; i < len(rest); {
		for i < len(rest) && (rest[i] == ' ' || rest[i] == '\n') {
			i++
		}
		start := i
		for i < len(rest) && rest[i] != ' ' && rest[i] != '\n' {
			i++
		}
		if start == i {
			break
		}
		seen++
		switch seen {
		case fState:
			state = rest[start]
		case fPPID:
			n, err := strconv.Atoi(string(rest[start:i]))
			if err != nil {
				return procInfo{}, false
			}
			ppid = n
		case fRSS:
			n, err := strconv.ParseInt(string(rest[start:i]), 10, 64)
			if err != nil {
				return procInfo{}, false
			}
			rssPages = n
		}
		if seen >= fRSS {
			break
		}
	}
	if seen < fRSS {
		return procInfo{}, false
	}
	return procInfo{
		PID:      pid,
		PPID:     ppid,
		Comm:     comm,
		State:    state,
		RSSBytes: rssPages * pageSize,
	}, true
}
