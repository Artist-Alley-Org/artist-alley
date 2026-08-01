// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// ---------------------------------------------------------------------------
// Bulk re-render (#760).
//
// The per-asset force path answers "this thumbnail is wrong, fix it".
// This answers the case that actually happens: a renderer changed, and
// every asset it ever touched is now stale. Three merged fixes (#689,
// #750, #753) were invisible on every install because there was no way
// to say that — a click-through of 590 assets is not a procedure.
//
// It lives in a package rather than in cmd/aa so the admin API can grow
// a button onto the same code path later without reimplementing it.
// ---------------------------------------------------------------------------

// RebuildOptions selects which assets get a fresh preview job.
type RebuildOptions struct {
	// Extensions restricts the sweep, lowercase and dotless
	// ("glb", "fbx"). Empty means every extension some handler can
	// render.
	Extensions []string

	// Limit caps the number of assets swept. 0 means the built-in
	// ceiling (RebuildDefaultLimit) — an unbounded bulk enqueue is
	// never what an operator meant to type.
	Limit int

	// Force re-renders outputs that already exist. Almost always true:
	// a rebuild of assets that have no variants is what the ordinary
	// re-queue already did. False is offered for the gap-filling sweep.
	Force bool

	// DryRun reports what would be enqueued and enqueues nothing.
	DryRun bool

	// Storage, when set, lets the report say how many of the swept
	// assets ALREADY have a rendered card thumbnail — i.e. how many of
	// these jobs are replacing bytes rather than filling a hole. That
	// number is the whole point of the report: without it, "590 jobs
	// queued, 590 done" is the same output whether 590 renders happened
	// or none did.
	Storage *storage.Service
}

// RebuildDefaultLimit is the ceiling applied when Limit is 0. Large
// enough for a full dev catalogue, small enough that a typo does not
// queue a hundred thousand renders.
const RebuildDefaultLimit = 5000

// RebuildReport is the honest tally. Every field exists because the
// alternative — one "queued: 590" line — is what made a fully skipped
// run indistinguishable from a fully rendered one.
type RebuildReport struct {
	// Matched is how many assets the extension filter selected.
	Matched int
	// Enqueued is how many preview jobs were actually inserted.
	Enqueued int
	// Stale is how many of those already had a `col` variant on the
	// storage volume, i.e. how many renders this run is REPLACING
	// rather than creating. With Force false, this is instead the
	// number that will skip.
	Stale int
	// Failed is per-asset enqueue failures (a queue hiccup); the sweep
	// continues past them.
	Failed int
	// PerExt breaks Enqueued down by extension, so a report can say
	// which formats were actually touched instead of one total.
	PerExt map[string]int
	// PerJobType breaks it down by handler, which is what the job
	// queue will show.
	PerJobType map[string]int
}

// ExtensionsSorted returns the extensions present in PerExt, sorted, so
// callers can print a stable report.
func (r RebuildReport) ExtensionsSorted() []string {
	out := make([]string, 0, len(r.PerExt))
	for e := range r.PerExt {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// JobTypesSorted returns the job types present in PerJobType, sorted.
func (r RebuildReport) JobTypesSorted() []string {
	out := make([]string, 0, len(r.PerJobType))
	for t := range r.PerJobType {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Jobs is the number of QUEUE ROWS the sweep created, as opposed to
// Enqueued, which counts assets. The two diverged when video started
// planning two jobs per asset (#818) and an operator sizing a rebuild
// needs the first number, not the second.
func (r RebuildReport) Jobs() int {
	n := 0
	for _, c := range r.PerJobType {
		n += c
	}
	return n
}

// Rebuild enqueues a preview job per matching asset.
//
// It does NOT delete anything. Each handler overwrites its outputs in
// place with an atomic backend write, so an interrupted rebuild leaves
// the previous — stale, but present — preview serving. Deleting first
// and rendering second would trade a stale thumbnail for a missing one
// on any crash, which is the worse failure for a catalogue this size.
func Rebuild(
	ctx context.Context,
	pool *pgxpool.Pool,
	jobsSvc *jobs.Service,
	opts RebuildOptions,
) (RebuildReport, error) {
	rep := RebuildReport{PerExt: map[string]int{}, PerJobType: map[string]int{}}

	exts := make([]string, 0, len(opts.Extensions))
	for _, e := range opts.Extensions {
		n := dispatch.Normalize(e)
		if n == "" {
			continue
		}
		// Refuse an extension nothing can render rather than sweeping
		// zero rows and reporting success — a silent no-op is the class
		// of bug this whole change is about.
		if !dispatch.CanPreview(&n) {
			return rep, fmt.Errorf("preview: no handler renders %q", e)
		}
		exts = append(exts, n)
	}
	if len(exts) == 0 {
		exts = dispatch.PreviewableExts()
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = RebuildDefaultLimit
	}

	rows, err := assets.New(pool).ListAssetsForBackfill(ctx, assets.ListAssetsForBackfillParams{
		Extensions: exts,
		RowLimit:   int32(limit),
	})
	if err != nil {
		return rep, fmt.Errorf("preview: list assets for rebuild: %w", err)
	}
	rep.Matched = len(rows)

	for _, row := range rows {
		if row.FileHash == nil || *row.FileHash == "" || row.FileExtension == nil {
			continue
		}
		ext := dispatch.Normalize(*row.FileExtension)
		steps := dispatch.PlanForExt(&ext, jobs.PriorityBackfil)
		if opts.Storage != nil && variantOnBackend(ctx, opts.Storage, *row.FileHash, "col") {
			rep.Stale++
		}
		if opts.DryRun {
			rep.Enqueued++
			rep.PerExt[ext]++
			for _, step := range steps {
				rep.PerJobType[string(step.Type)]++
			}
			continue
		}
		// Enqueued counts ASSETS, PerJobType counts JOBS. They stopped
		// agreeing when video started planning two (#818), and the two
		// questions an operator asks a rebuild report — "how much of my
		// library did this sweep" and "how much work did it make" — want
		// different answers.
		payload := dispatch.NewPayload(uuid.UUID(row.ID.Bytes), *row.FileHash, &ext, opts.Force)
		enqueued := 0
		for _, step := range steps {
			priority := step.Priority
			if _, err := jobsSvc.Enqueue(ctx, step.Type, payload,
				jobs.EnqueueOpts{Priority: &priority},
			); err != nil {
				continue
			}
			enqueued++
			rep.PerJobType[string(step.Type)]++
		}
		if enqueued == 0 {
			rep.Failed++
			continue
		}
		rep.Enqueued++
		rep.PerExt[ext]++
	}
	return rep, nil
}
