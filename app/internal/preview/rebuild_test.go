// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// ---------------------------------------------------------------------------
// #760 — the bulk path. `aa rebuild-previews` is what an operator reaches
// for after a renderer changes, so the property that matters is that the
// jobs it inserts CARRY force. A sweep that enqueues 590 ordinary jobs
// looks identical from the queue page and does nothing at all.
// ---------------------------------------------------------------------------

func TestRebuild_EnqueuesForcedJobsForMatchingExtensions(t *testing.T) {
	rig := newPreviewTestRig(t)
	ctx := t.Context()

	glbID, _ := rig.seedPreviewAsset(t, "glb", []byte("glb bytes"))
	pngID, _ := rig.seedPreviewAsset(t, "png", []byte("png bytes"))
	dropJobsFor(t, rig, glbID.String(), pngID.String())

	svc := jobs.NewService(rig.pool, rig.logger, nil)
	rep, err := Rebuild(ctx, rig.pool, svc, RebuildOptions{
		Extensions: []string{"GLB"}, // case + dot normalisation is the caller's convenience
		Force:      true,
		Storage:    rig.storage,
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rep.PerExt["glb"] == 0 {
		t.Fatalf("no glb jobs enqueued: %+v", rep)
	}
	if rep.PerJobType[string(jobs.TypePreview3D)] != rep.PerExt["glb"] {
		t.Errorf("glb did not route to preview.3d: %+v", rep)
	}

	forced, other := forcedPayloadCounts(t, rig, glbID)
	if forced == 0 {
		t.Error("the swept asset got no forced job — a rebuild that enqueues ordinary " +
			"jobs is the no-op this command exists to replace (#760)")
	}
	if other != 0 {
		t.Errorf("%d non-forced job(s) enqueued for the swept asset", other)
	}

	// The extension filter is a filter, not a suggestion: a png in the
	// same catalogue must be untouched.
	if f, o := forcedPayloadCounts(t, rig, pngID); f+o != 0 {
		t.Errorf("--ext glb swept a png asset too (%d forced, %d plain)", f, o)
	}
}

func TestRebuild_DryRunEnqueuesNothing(t *testing.T) {
	rig := newPreviewTestRig(t)
	ctx := t.Context()

	assetID, _ := rig.seedPreviewAsset(t, "glb", []byte("glb bytes"))
	dropJobsFor(t, rig, assetID.String())

	svc := jobs.NewService(rig.pool, rig.logger, nil)
	rep, err := Rebuild(ctx, rig.pool, svc, RebuildOptions{
		Extensions: []string{"glb"}, Force: true, DryRun: true, Storage: rig.storage,
	})
	if err != nil {
		t.Fatalf("Rebuild dry-run: %v", err)
	}
	if rep.Enqueued == 0 {
		t.Error("dry-run reported nothing; it is supposed to report what it WOULD do")
	}
	if f, o := forcedPayloadCounts(t, rig, assetID); f+o != 0 {
		t.Errorf("dry-run inserted %d forced + %d plain job(s)", f, o)
	}
}

// An extension no handler renders must be refused, not swept silently to
// zero rows: "0 assets matched" and "that format has no renderer" are
// different answers and the operator needs the second one.
func TestRebuild_RejectsUnrenderableExtension(t *testing.T) {
	rig := newPreviewTestRig(t)
	svc := jobs.NewService(rig.pool, rig.logger, nil)
	if _, err := Rebuild(t.Context(), rig.pool, svc, RebuildOptions{
		Extensions: []string{"xyzzy"}, Force: true,
	}); err == nil {
		t.Error("Rebuild accepted an extension no handler can render")
	}
}

// dropJobsFor removes any preview jobs naming these assets, now and at
// test end. The suite shares one database, and a job row outlives the
// asset it names (jobs has no FK to assets), so a leftover from another
// test would be counted as this one's.
func dropJobsFor(t *testing.T, rig *previewTestRig, ids ...string) {
	t.Helper()
	del := func() {
		for _, id := range ids {
			_, _ = rig.pool.Exec(context.Background(),
				`DELETE FROM jobs WHERE type LIKE 'preview.%' AND payload->>'asset_id' = $1`, id)
		}
	}
	del()
	t.Cleanup(del)
}

// forcedPayloadCounts returns how many queued preview jobs for this asset
// carry force=true, and how many don't.
func forcedPayloadCounts(t *testing.T, rig *previewTestRig, assetID interface{ String() string }) (int, int) {
	t.Helper()
	rows, err := rig.pool.Query(context.Background(),
		`SELECT payload FROM jobs WHERE type LIKE 'preview.%' AND payload->>'asset_id' = $1`,
		assetID.String())
	if err != nil {
		t.Fatalf("query jobs: %v", err)
	}
	defer rows.Close()
	var forced, plain int
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		var p struct {
			Force bool `json:"force"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if p.Force {
			forced++
		} else {
			plain++
		}
	}
	return forced, plain
}
