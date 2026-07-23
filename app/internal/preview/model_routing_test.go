// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsThreeJSExt(t *testing.T) {
	for _, ext := range []string{"glb", "gltf", "fbx", "obj", ".GLB", "Gltf"} {
		if !isThreeJSExt(ext) {
			t.Errorf("isThreeJSExt(%q) = false, want true", ext)
		}
	}
	for _, ext := range []string{"blend", "mview", "md2", "stl", "png", ""} {
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

	// DisableThreeJS forces the Blender path.
	disabled := &ModelHandler{ThreeJSScript: script, NodePath: "sh", DisableThreeJS: true}
	if disabled.threeJSAvailable() {
		t.Error("DisableThreeJS should force unavailable")
	}

	// Missing script → unavailable (e.g. arm64 image without the worker).
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

	// Missing node binary → unavailable (falls back to Blender).
	noNode := &ModelHandler{ThreeJSScript: script, NodePath: "definitely-not-a-real-binary-xyz"}
	if noNode.threeJSAvailable() {
		t.Error("missing node binary should be unavailable")
	}
}
