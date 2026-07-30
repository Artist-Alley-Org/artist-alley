// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package format3d

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/qmuntal/gltf"
	"github.com/qmuntal/gltf/modeler"
)

// GLB container constants (glTF 2.0 §4.4, little-endian).
//
// These duplicate qmuntal/gltf's own unexported glbHeaderMagic /
// glbChunkJSON: the library knows the container but keeps the constants
// private and only exposes them behind a full Document decode, which
// eagerly resolves buffers. ReadGLBJSONChunk needs the JSON chunk and
// nothing else, so it reads the container directly.
const (
	glbMagic     uint32 = 0x46546C67 // "glTF"
	glbChunkJSON uint32 = 0x4E4F534A // "JSON"

	// glbMaxJSONChunk caps how much a declared chunk length may make us
	// allocate. A malformed or hostile header can claim 4 GiB; the JSON
	// chunk of a real model is orders of magnitude under this.
	glbMaxJSONChunk uint32 = 64 << 20 // 64 MiB
)

// ReadGLBJSONChunk returns the JSON chunk of a GLB (binary glTF)
// stream — the same glTF document a .gltf file holds as plain text.
//
// Layout read: a 12-byte header (magic "glTF", version, total length)
// followed by length-prefixed chunks, the first of which the spec
// requires to be the JSON chunk. Only that chunk is read, so the BIN
// chunk (nearly all of the file) is never pulled into memory.
func ReadGLBJSONChunk(r io.Reader) ([]byte, error) {
	var header struct {
		Magic   uint32
		Version uint32
		Length  uint32
	}
	if err := binary.Read(r, binary.LittleEndian, &header); err != nil {
		return nil, fmt.Errorf("format3d: read glb header: %w", err)
	}
	if header.Magic != glbMagic {
		return nil, fmt.Errorf("format3d: not a glb (magic %#08x)", header.Magic)
	}

	var chunk struct {
		Length uint32
		Type   uint32
	}
	if err := binary.Read(r, binary.LittleEndian, &chunk); err != nil {
		return nil, fmt.Errorf("format3d: read glb chunk header: %w", err)
	}
	if chunk.Type != glbChunkJSON {
		return nil, fmt.Errorf("format3d: first glb chunk is %#08x, want JSON", chunk.Type)
	}
	if chunk.Length > glbMaxJSONChunk {
		return nil, fmt.Errorf("format3d: glb json chunk too large (%d bytes)", chunk.Length)
	}

	buf := make([]byte, chunk.Length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("format3d: read glb json chunk: %w", err)
	}
	// The chunk is padded to a 4-byte boundary with trailing spaces,
	// which encoding/json tolerates; trim anyway so callers that hash or
	// compare the document see exactly the JSON.
	return bytes.TrimRight(buf, " \x00"), nil
}

// WriteGLB serialises a Model as a glTF 2.0 binary stream. Layout:
//
//   - One mesh, one primitive (TRIANGLES) sharing positions /
//     normals / UVs / indices accessors.
//   - One material — either PBR-with-texture (Material.DiffuseImageBytes
//     non-empty) or PBR-grey-fallback.
//   - One node referencing the mesh.
//   - Per Animation: one glTF Animation entry. Frame N becomes morph
//     target N on the primitive; the animation's sampler keys each
//     target's weight to 1.0 at its own time slot, 0.0 elsewhere.
//     That's the glTF-blessed way to play a per-frame vertex
//     animation on a runtime that doesn't know about Quake-style
//     interpolated keyframes natively.
//
// Out of scope (for now): skinning, multiple meshes per model,
// per-frame texture changes. The first two are doable when we hit a
// format that needs them.
func WriteGLB(m *Model, w io.Writer) error {
	if m == nil || len(m.Vertices) == 0 || len(m.Triangles) == 0 {
		return fmt.Errorf("format3d: empty model")
	}

	doc := gltf.NewDocument()
	doc.Asset.Generator = "artist-alley format3d"

	// Vertex attribute accessors — written once, shared across the
	// primitive + every morph target's POSITION delta.
	positions := make([][3]float32, len(m.Vertices))
	normals := make([][3]float32, len(m.Vertices))
	uvs := make([][2]float32, len(m.Vertices))
	for i, v := range m.Vertices {
		positions[i] = v.Position
		normals[i] = v.Normal
		uvs[i] = v.TexCoord
	}
	posAcc := modeler.WritePosition(doc, positions)
	normAcc := modeler.WriteNormal(doc, normals)
	uvAcc := modeler.WriteTextureCoord(doc, uvs)

	indices := make([]uint32, 0, len(m.Triangles)*3)
	for _, t := range m.Triangles {
		indices = append(indices, t.A, t.B, t.C)
	}
	idxAcc := modeler.WriteIndices(doc, indices)

	// Material — PBR base color texture if we have one, otherwise a
	// neutral mid-grey shader so the model isn't pink-missing-shader.
	matIdx := writeMaterial(doc, &m.Material)

	// Morph targets: each animation frame becomes one target whose
	// POSITION attribute holds the *delta* from the rest pose. The
	// rest pose itself is the primitive's base POSITION, so a
	// stationary vertex's delta is (0,0,0) every frame.
	prim := &gltf.Primitive{
		Mode:    gltf.PrimitiveTriangles,
		Indices: gltf.Index(idxAcc),
		Attributes: gltf.PrimitiveAttributes{
			gltf.POSITION:   posAcc,
			gltf.NORMAL:     normAcc,
			gltf.TEXCOORD_0: uvAcc,
		},
		Material: gltf.Index(matIdx),
	}

	if totalFrames := totalAnimationFrames(m.Animations); totalFrames > 0 {
		targets := make([]gltf.PrimitiveAttributes, 0, totalFrames)
		for _, anim := range m.Animations {
			for _, frame := range anim.Frames {
				targets = append(targets, frameToMorphTarget(doc, m, frame))
			}
		}
		prim.Targets = targets
	}

	doc.Meshes = append(doc.Meshes, &gltf.Mesh{
		Name:       m.Name,
		Primitives: []*gltf.Primitive{prim},
	})

	doc.Nodes = append(doc.Nodes, &gltf.Node{
		Name: m.Name,
		Mesh: gltf.Index(0),
	})
	doc.Scenes[0].Nodes = []int{0}

	if err := writeAnimations(doc, m.Animations); err != nil {
		return fmt.Errorf("format3d: write animations: %w", err)
	}

	if err := gltf.NewEncoder(w).Encode(doc); err != nil {
		return fmt.Errorf("format3d: encode glb: %w", err)
	}
	return nil
}

// WriteGLBBytes is the convenience wrapper for callers that already
// have the model in memory and want the GLB as a byte slice.
func WriteGLBBytes(m *Model) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteGLB(m, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeMaterial(doc *gltf.Document, mat *Material) int {
	pbr := &gltf.PBRMetallicRoughness{
		BaseColorFactor: &[4]float64{0.8, 0.8, 0.8, 1.0},
		MetallicFactor:  gltf.Float(0),
		RoughnessFactor: gltf.Float(1),
	}
	if len(mat.DiffuseImageBytes) > 0 {
		mime := mat.DiffuseImageMIME
		if mime == "" {
			mime = "image/png"
		}
		imgIdx, err := modeler.WriteImage(doc, mat.Name, mime, bytes.NewReader(mat.DiffuseImageBytes))
		if err == nil {
			doc.Textures = append(doc.Textures, &gltf.Texture{
				Name:   mat.Name,
				Source: gltf.Index(imgIdx),
			})
			pbr.BaseColorTexture = &gltf.TextureInfo{Index: len(doc.Textures) - 1}
			pbr.BaseColorFactor = &[4]float64{1, 1, 1, 1}
		}
	}
	doc.Materials = append(doc.Materials, &gltf.Material{
		Name:                 mat.Name,
		PBRMetallicRoughness: pbr,
	})
	return len(doc.Materials) - 1
}

func totalAnimationFrames(anims []Animation) int {
	n := 0
	for _, a := range anims {
		n += len(a.Frames)
	}
	return n
}

// frameToMorphTarget writes the per-frame delta POSITION + NORMAL
// accessors and returns a PrimitiveAttributes block referencing them.
func frameToMorphTarget(doc *gltf.Document, m *Model, frame AnimationFrame) gltf.PrimitiveAttributes {
	deltas := make([][3]float32, len(m.Vertices))
	for i := range deltas {
		if i < len(frame.Positions) {
			deltas[i] = [3]float32{
				frame.Positions[i][0] - m.Vertices[i].Position[0],
				frame.Positions[i][1] - m.Vertices[i].Position[1],
				frame.Positions[i][2] - m.Vertices[i].Position[2],
			}
		}
	}
	attrs := gltf.PrimitiveAttributes{
		gltf.POSITION: modeler.WritePosition(doc, deltas),
	}
	if len(frame.Normals) == len(m.Vertices) {
		normDelta := make([][3]float32, len(m.Vertices))
		for i := range normDelta {
			normDelta[i] = [3]float32{
				frame.Normals[i][0] - m.Vertices[i].Normal[0],
				frame.Normals[i][1] - m.Vertices[i].Normal[1],
				frame.Normals[i][2] - m.Vertices[i].Normal[2],
			}
		}
		attrs[gltf.NORMAL] = modeler.WriteNormal(doc, normDelta)
	}
	return attrs
}

// writeAnimations emits one gltf.Animation per Model.Animation. Each
// animation drives the mesh's morph-target weights through a SCALAR
// time accessor + a per-target weight matrix accessor that turns one
// target on per frame.
func writeAnimations(doc *gltf.Document, anims []Animation) error {
	if len(anims) == 0 {
		return nil
	}
	totalTargets := totalAnimationFrames(anims)
	if totalTargets == 0 {
		return nil
	}

	targetOffset := 0
	for _, anim := range anims {
		if len(anim.Frames) == 0 {
			continue
		}
		fps := anim.FPS
		if fps <= 0 {
			fps = 10
		}
		times := make([]float32, len(anim.Frames))
		for i := range times {
			times[i] = float32(i) / fps
		}
		timeAcc := modeler.WriteAccessor(doc, gltf.TargetNone, times)
		if timeAcc >= 0 && timeAcc < len(doc.Accessors) {
			doc.Accessors[timeAcc].Min = []float64{float64(times[0])}
			doc.Accessors[timeAcc].Max = []float64{float64(times[len(times)-1])}
		}

		// Weight matrix: len(frames) keyframes × totalTargets weights
		// per keyframe. Frame i drives target (targetOffset+i) to 1.0
		// and every other target to 0.0.
		weights := make([]float32, len(anim.Frames)*totalTargets)
		for i := range anim.Frames {
			weights[i*totalTargets+(targetOffset+i)] = 1.0
		}
		weightAcc := modeler.WriteAccessor(doc, gltf.TargetNone, weights)

		sampler := &gltf.AnimationSampler{
			Input:         timeAcc,
			Output:        weightAcc,
			Interpolation: gltf.InterpolationLinear,
		}
		zero := 0
		channel := &gltf.AnimationChannel{
			Sampler: 0,
			Target: gltf.AnimationChannelTarget{
				Node: &zero,
				Path: gltf.TRSWeights,
			},
		}
		doc.Animations = append(doc.Animations, &gltf.Animation{
			Name:     anim.Name,
			Samplers: []*gltf.AnimationSampler{sampler},
			Channels: []*gltf.AnimationChannel{channel},
		})
		targetOffset += len(anim.Frames)
	}
	return nil
}
