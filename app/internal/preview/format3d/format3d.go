// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package format3d is an in-process Go importer for legacy 3D mesh
// formats no stock three.js loader reads — the long tail of
// game-engine assets users upload (Quake / Half-Life model formats
// today, more later).
//
// Each format gets its own file (md2.go, md3.go, mdl.go, …). They
// all parse into the shared Model intermediate, which GLB.Write
// emits as a glTF 2.0 binary the existing preview.model worker
// renders like any other .glb.
//
// Design constraints:
//
//   - Pure Go, no cgo, no subprocess. Same ownership story as the
//     mviewer/go port: we never bundle a binary, never link a C
//     library, and the licence story stays clean for downstream
//     redistribution.
//
//   - One file per format, self-contained parser. Avoids leaky
//     cross-format coupling so a future port (LWO, MS3D, …) can be
//     added or removed without touching its siblings.
//
//   - The intermediate Model is intentionally lossy compared to the
//     source format's full structure. It captures only what glTF can
//     express + what a turntable render actually needs: triangle
//     meshes, per-frame vertex animations (morph targets), and a
//     single diffuse texture. Format-specific features (lighting,
//     LOD chains, hardware-specific quirks) drop on the floor.
//
// API shape:
//
//	model, err := md2.Decode(reader)        // format → *format3d.Model
//	err := format3d.WriteGLB(model, writer) // *Model → glTF binary stream
//
// The model worker reaches for this package when the upload's
// extension has no three.js loader; the conversion happens before the
// render decision is taken, so the result routes to the worker as a
// .glb. See app/internal/preview/model.go.
package format3d

// Vertex is one position + texture-coord pair in the rest pose. For
// animated formats the per-frame positions live in Animation.Frames
// (morph targets) instead of mutating Vertex.Position over time —
// glTF's morph-target shape maps cleanly to this layout.
type Vertex struct {
	Position [3]float32
	Normal   [3]float32
	TexCoord [2]float32
}

// Triangle is three vertex indices into Model.Vertices, CCW-wound.
type Triangle struct {
	A, B, C uint32
}

// Animation is a per-frame morph-target series. Each Frame has the
// same length as Model.Vertices — entry i in the frame is the
// position vertex i should be at when that frame is active. glTF
// emits these as KHR_animation_pointer-free linear morph targets so
// any viewer renders them correctly.
type Animation struct {
	Name   string
	FPS    float32
	Frames []AnimationFrame
}

type AnimationFrame struct {
	Positions [][3]float32
	Normals   [][3]float32
}

// Material is the single-texture rest pose for the model. format3d
// doesn't try to model multi-pass effects — the only thing every
// legacy format we touch agrees on is "one diffuse texture wrapped
// onto the mesh."
type Material struct {
	Name string
	// DiffuseImageBytes is the texture's image bytes (PNG, JPEG, or
	// the format's native raster), ready to feed to a glTF image
	// embed. Empty when the source doesn't carry a texture (we ship
	// the mesh with a fallback grey PBR material in that case).
	DiffuseImageBytes []byte
	DiffuseImageMIME  string
}

// Model is the format-agnostic intermediate the per-format decoders
// produce and the glTF writer consumes.
type Model struct {
	Name       string
	Vertices   []Vertex
	Triangles  []Triangle
	Material   Material
	Animations []Animation
}
