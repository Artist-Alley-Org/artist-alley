// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// aa-abr-extract — read a Photoshop .abr brush pack and emit a
// directory of PNG stamps + a manifest JSON.
//
// This is a dev tool / pipeline component; the production upload
// path (Phase 1.21c) wraps the same `abr.ParseBrushes` call but
// stores the output in object storage instead of the local fs.
// Keeping the CLI around is useful for:
//   - Debugging a brush pack that doesn't import cleanly
//   - Bootstrapping our built-in brush pack (curate stamps from a
//     known-good .abr, vendor the resulting PNGs into the repo)
//   - Smoke-testing the parser after a regen
//
// Usage:
//
//	aa-abr-extract <input.abr> <output-dir>
//
// Output layout:
//
//	<output-dir>/
//	  manifest.json        — { brushes: [{id, file, w, h}, ...] }
//	  stamps/<id>.png      — one grayscale PNG per brush stamp
//
// The grayscale PNGs are the brush "alpha mask" — 255 = solid,
// 0 = transparent. The frontend stamp renderer composes them with
// the user's chosen color at draw time. Identical to how every
// stamp-based brush engine works (Photoshop / Krita / Procreate).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"sort"

	"github.com/mscrnt/artist-alley/app/internal/abr"
)

// manifestEntry is one row in manifest.json. We keep the schema tiny
// for now (id + file + dims) — the upload pipeline + brush-pack DB
// will store the same fields plus storage-backend keys. As the
// renderer grows (spacing / jitter / dynamics from the descriptor
// block) those fields land here too.
type manifestEntry struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type manifest struct {
	Source  string          `json:"source"`
	Count   int             `json:"count"`
	Brushes []manifestEntry `json:"brushes"`
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s <input.abr> <output-dir>\n", os.Args[0])
		os.Exit(2)
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
	}
	inputPath := flag.Arg(0)
	outDir := flag.Arg(1)

	f, err := os.Open(inputPath)
	if err != nil {
		fatal("open input: %v", err)
	}
	defer f.Close()

	brushes, err := abr.ParseBrushes(f)
	if err != nil {
		fatal("parse: %v", err)
	}
	if len(brushes) == 0 {
		fatal("no brushes decoded (file may use an unsupported subversion)")
	}

	stampDir := filepath.Join(outDir, "stamps")
	if err := os.MkdirAll(stampDir, 0o755); err != nil {
		fatal("mkdir output: %v", err)
	}

	entries := make([]manifestEntry, 0, len(brushes))
	for _, b := range brushes {
		file := b.ID + ".png"
		path := filepath.Join(stampDir, file)
		out, err := os.Create(path)
		if err != nil {
			fatal("create %s: %v", path, err)
		}
		if err := png.Encode(out, b.AsImage()); err != nil {
			out.Close()
			fatal("encode %s: %v", path, err)
		}
		out.Close()
		entries = append(entries, manifestEntry{
			ID:     b.ID,
			File:   "stamps/" + file,
			Width:  b.Width,
			Height: b.Height,
		})
	}

	// Stable order in the manifest so re-runs over the same pack
	// produce byte-identical output (useful for diffing / caching).
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	m := manifest{
		Source:  filepath.Base(inputPath),
		Count:   len(entries),
		Brushes: entries,
	}
	mf, err := os.Create(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		fatal("create manifest: %v", err)
	}
	enc := json.NewEncoder(mf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		mf.Close()
		fatal("write manifest: %v", err)
	}
	mf.Close()

	fmt.Printf("extracted %d brushes to %s\n", len(entries), outDir)
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "aa-abr-extract: "+format+"\n", args...)
	os.Exit(1)
}
