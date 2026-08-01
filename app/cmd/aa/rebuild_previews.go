// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/db"
	aahttp "github.com/mscrnt/artist-alley/app/internal/http"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/logging"
	"github.com/mscrnt/artist-alley/app/internal/preview"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// runRebuildPreviews is `aa rebuild-previews` (#760): re-render the
// previews for a whole set of assets, not one at a time.
//
// This is the missing half of the fix. The per-asset control now really
// re-renders, but the situation that produced the bug report is "a
// renderer changed, so every asset it ever touched is stale" — 590 3D
// models in the dev catalogue, whose card thumbnails were rendered by a
// Blender build that has since been removed from the image. There was
// no procedure for that other than clicking 590 times.
//
// Enqueue-only: it inserts jobs at PriorityBackfil and exits. The
// running app's worker pool drains them, so a rebuild cannot preempt
// interactive work and does not need this process to stay alive.
func runRebuildPreviews(args []string) error {
	fs := flag.NewFlagSet("rebuild-previews", flag.ContinueOnError)
	extCSV := fs.String("ext", "",
		"comma-separated extensions to rebuild (e.g. glb,fbx,obj); empty = every previewable extension")
	limit := fs.Int("limit", 0,
		fmt.Sprintf("cap the number of assets swept (0 = %d)", preview.RebuildDefaultLimit))
	force := fs.Bool("force", true,
		"re-render outputs that already exist. false = fill gaps only, which is what "+
			"an ordinary re-queue already does")
	dryRun := fs.Bool("dry-run", false,
		"report what would be enqueued and enqueue nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.Setup(cfg.LogLevel, cfg.LogFormat)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The storage backend is opened only so the report can say how many
	// of these assets already have a rendered thumbnail. A backend that
	// won't open is not fatal — it costs the staleness column, not the
	// rebuild.
	var storageSvc *storage.Service
	if backend, bErr := aahttp.BuildStorageBackend(cfg); bErr != nil {
		fmt.Fprintf(os.Stderr,
			"WARNING: storage backend unavailable (%v); the report cannot say how many "+
				"assets already have renders.\n", bErr)
	} else {
		storageSvc = storage.NewService(backend, pool)
	}

	var exts []string
	if strings.TrimSpace(*extCSV) != "" {
		for _, e := range strings.Split(*extCSV, ",") {
			if e = strings.TrimSpace(e); e != "" {
				exts = append(exts, e)
			}
		}
	}

	// Enqueue-only Service, same as the seeder's: a nil Registry is
	// fine because Enqueue never consults it.
	rep, err := preview.Rebuild(ctx, pool, jobs.NewService(pool, logger, nil), preview.RebuildOptions{
		Extensions: exts,
		Limit:      *limit,
		Force:      *force,
		DryRun:     *dryRun,
		Storage:    storageSvc,
	})
	if err != nil {
		return err
	}

	verb := "enqueued"
	if *dryRun {
		verb = "would enqueue"
	}
	// Assets and jobs are no longer the same number: a video plans two
	// (the cheap poster job plus the full ladder, #818). Print both
	// rather than one under the other's name.
	fmt.Printf("%s %d preview job(s) across %d asset(s), from %d matching row(s); force=%v\n",
		verb, rep.Jobs(), rep.Enqueued, rep.Matched, *force)
	if storageSvc != nil {
		if *force {
			fmt.Printf("  %d of them already have a rendered `col` variant — those renders "+
				"will be REPLACED\n", rep.Stale)
		} else {
			// Deliberately not "and change nothing" any more. Since #827
			// a skip reconciles: it writes back any storage_variants row
			// that went missing while the bytes survived, and stamps a
			// thumbhash the ladder never got to. On a healthy install
			// that is a no-op; on one restored from a backup it is the
			// whole repair.
			fmt.Printf("  %d of them already have a rendered `col` variant — with force=false "+
				"those jobs will not re-render, but they WILL reconcile any missing "+
				"storage_variants row and thumbhash (#827)\n", rep.Stale)
		}
	}
	for _, ext := range rep.ExtensionsSorted() {
		fmt.Printf("  %-6s %d\n", ext, rep.PerExt[ext])
	}
	if len(rep.PerJobType) > 1 {
		fmt.Println("  by job type:")
		for _, t := range rep.JobTypesSorted() {
			fmt.Printf("    %-22s %d\n", t, rep.PerJobType[t])
		}
	}
	if rep.Failed > 0 {
		fmt.Printf("  %d asset(s) failed to enqueue\n", rep.Failed)
	}
	if rep.Matched == 0 {
		fmt.Println("NOTE: nothing matched. Check --ext against the asset's actual file_extension.")
	}
	return nil
}
