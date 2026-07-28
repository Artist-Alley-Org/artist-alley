// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsThreeJSExt(t *testing.T) {
	// Every entry here must also appear in loadModel() in
	// scripts/threejs/render.html, and scripts/threejs/smoke.mjs must
	// render a fixture for it — this map is only a claim; the smoke is
	// the proof.
	for _, ext := range []string{
		"glb", "gltf", "fbx", "obj", "stl", "ply", "dae", ".GLB", "Gltf",
	} {
		if !isThreeJSExt(ext) {
			t.Errorf("isThreeJSExt(%q) = false, want true", ext)
		}
	}
	// mview / md2 convert to .glb before the render decision is taken, so
	// they are deliberately absent here (see Handle). blend/usd/abc have no
	// stock three.js loader and get no turntable until the Blender
	// converter ships as a plugin (#499).
	for _, ext := range []string{"blend", "mview", "md2", "usdz", "abc", "png", ""} {
		if isThreeJSExt(ext) {
			t.Errorf("isThreeJSExt(%q) = true, want false", ext)
		}
	}
}

func TestThreeJSAvailable(t *testing.T) {
	// A fake worker layout: <dir>/worker.mjs + <dir>/node_modules/three.
	dir := t.TempDir()
	script := filepath.Join(dir, "worker.mjs")
	if err := os.WriteFile(script, []byte("// noop"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "three"), 0o755); err != nil {
		t.Fatal(err)
	}

	// `sh` stands in for `node` so LookPath succeeds deterministically in CI.
	base := &ModelHandler{ThreeJSScript: script, NodePath: "sh"}
	if !base.threeJSAvailable() {
		t.Fatal("expected available with script + node_modules + node-on-PATH")
	}

	// (The DisableThreeJS escape hatch was removed in #500. It forced the
	// Blender path; with Blender out of the image it could only mean
	// "generate no 3D previews at all", and nothing ever set it — no env
	// var, no sysconfig key, only this test.)

	// Missing script → unavailable. Since #500 that is a deployment fault
	// rather than a fallback trigger: Handle turns it into a retryable job
	// error instead of marking the asset ready with no preview.
	noScript := &ModelHandler{ThreeJSScript: filepath.Join(dir, "absent.mjs"), NodePath: "sh"}
	if noScript.threeJSAvailable() {
		t.Error("missing script should be unavailable")
	}

	// Missing node_modules → unavailable (deps never installed).
	bareDir := t.TempDir()
	bareScript := filepath.Join(bareDir, "worker.mjs")
	_ = os.WriteFile(bareScript, []byte("// noop"), 0o644)
	noMods := &ModelHandler{ThreeJSScript: bareScript, NodePath: "sh"}
	if noMods.threeJSAvailable() {
		t.Error("missing node_modules should be unavailable")
	}

	// Missing node binary → unavailable.
	noNode := &ModelHandler{ThreeJSScript: script, NodePath: "definitely-not-a-real-binary-xyz"}
	if noNode.threeJSAvailable() {
		t.Error("missing node binary should be unavailable")
	}
}
