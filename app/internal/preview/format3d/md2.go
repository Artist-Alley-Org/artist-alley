// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package format3d

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MD2 magic ("IDP2") + supported version. id Software shipped only
// version 8; any other value means we're looking at a different
// format that happens to share the IDP* family namespace.
const (
	md2Magic              uint32 = 0x32504449 // "IDP2"
	md2Version            uint32 = 8
	md2FrameNameLen              = 16
	md2VertexNormalsCount        = 162
)

// md2Header is the 17-uint32 file header. Field order + names
// follow the historic id Software documentation (modelheader_t).
type md2Header struct {
	Magic        uint32
	Version      uint32
	SkinWidth    uint32
	SkinHeight   uint32
	FrameSize    uint32
	NumSkins     uint32
	NumXYZ       uint32
	NumST        uint32
	NumTris      uint32
	NumGLCmds    uint32
	NumFrames    uint32
	OffsetSkins  uint32
	OffsetST     uint32
	OffsetTris   uint32
	OffsetFrames uint32
	OffsetGLCmds uint32
	OffsetEnd    uint32
}

// DecodeMD2 parses a Quake II .md2 stream into a Model.
//
//   - All frames become one Animation called "default" so the
//     turntable renderer can play the model's full cycle without
//     needing per-clip metadata (MD2 doesn't carry clip names; the
//     idle / run / death ranges are conventionally hard-coded in
//     the game engine).
//   - The rest pose used for the static mesh + texture mapping is
//     the model's first frame. Subsequent frames become morph
//     targets keyed at 1/FPS each.
//   - Skin filenames in the file are *not* dereferenced — MD2
//     ships the model without the texture, so the caller would need
//     to supply it as a companion. Material.Name preserves the
//     first skin's filename so callers can re-attach later.
func DecodeMD2(r io.Reader) (*Model, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("md2: read: %w", err)
	}
	if len(data) < int(unsafeSize(md2Header{})) {
		return nil, errors.New("md2: file too short for header")
	}

	var h md2Header
	if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &h); err != nil {
		return nil, fmt.Errorf("md2: header: %w", err)
	}
	if h.Magic != md2Magic {
		return nil, fmt.Errorf("md2: bad magic 0x%08x (want 0x%08x for 'IDP2')", h.Magic, md2Magic)
	}
	if h.Version != md2Version {
		return nil, fmt.Errorf("md2: unsupported version %d (want %d)", h.Version, md2Version)
	}
	if h.NumXYZ == 0 || h.NumTris == 0 || h.NumFrames == 0 {
		return nil, fmt.Errorf("md2: empty model (verts=%d tris=%d frames=%d)", h.NumXYZ, h.NumTris, h.NumFrames)
	}

	// Texture coords. Stored as i16 in [0, SkinWidth) × [0, SkinHeight).
	// glTF wants [0, 1] floats with the V-axis pointing down — Quake's
	// origin is top-left already, so divide and we're done.
	if int(h.OffsetST)+int(h.NumST)*4 > len(data) {
		return nil, errors.New("md2: ST table truncated")
	}
	stRaw := data[h.OffsetST : h.OffsetST+h.NumST*4]
	type rawST struct{ S, T int16 }
	stEntries := make([]rawST, h.NumST)
	_ = binary.Read(bytes.NewReader(stRaw), binary.LittleEndian, &stEntries)

	skinW := float32(h.SkinWidth)
	skinH := float32(h.SkinHeight)
	if skinW == 0 {
		skinW = 1
	}
	if skinH == 0 {
		skinH = 1
	}

	// Triangles. Each is (3 × uint16 vertex index) + (3 × uint16 ST
	// index). Vertex + ST indexing is decoupled — two triangles
	// sharing a position may have different texture coords, which
	// breaks the "one vertex = one (pos, uv) pair" assumption glTF
	// wants. We resolve by expanding into unique (pos, uv) combos
	// during frame decode.
	if int(h.OffsetTris)+int(h.NumTris)*12 > len(data) {
		return nil, errors.New("md2: triangle table truncated")
	}
	triRaw := data[h.OffsetTris : h.OffsetTris+h.NumTris*12]
	type rawTri struct {
		VertIdx [3]uint16
		STIdx   [3]uint16
	}
	tris := make([]rawTri, h.NumTris)
	_ = binary.Read(bytes.NewReader(triRaw), binary.LittleEndian, &tris)

	// Decode every frame in one pass. The rest pose is frame 0;
	// frames 1..N are morph targets. Each frame: scale[3], translate[3],
	// name[16], then h.NumXYZ × 4 bytes (xyz + normal_index).
	frameStride := int(h.FrameSize)
	if frameStride < 6*4+md2FrameNameLen+int(h.NumXYZ)*4 {
		return nil, fmt.Errorf("md2: frame stride %d too small for %d verts", frameStride, h.NumXYZ)
	}
	if int(h.OffsetFrames)+int(h.NumFrames)*frameStride > len(data) {
		return nil, errors.New("md2: frame table truncated")
	}

	type framePose struct {
		name    string
		xyz     [][3]float32 // length == h.NumXYZ
		normals [][3]float32
	}
	poses := make([]framePose, h.NumFrames)
	for fi := uint32(0); fi < h.NumFrames; fi++ {
		off := int(h.OffsetFrames) + int(fi)*frameStride
		fr := bytes.NewReader(data[off : off+frameStride])
		var scale, translate [3]float32
		_ = binary.Read(fr, binary.LittleEndian, &scale)
		_ = binary.Read(fr, binary.LittleEndian, &translate)
		nameBytes := make([]byte, md2FrameNameLen)
		_, _ = io.ReadFull(fr, nameBytes)
		name := strings.TrimRight(string(nameBytes), "\x00 ")

		xyz := make([][3]float32, h.NumXYZ)
		norms := make([][3]float32, h.NumXYZ)
		for vi := uint32(0); vi < h.NumXYZ; vi++ {
			var v [4]uint8
			_, _ = io.ReadFull(fr, v[:])
			xyz[vi] = [3]float32{
				float32(v[0])*scale[0] + translate[0],
				float32(v[1])*scale[1] + translate[1],
				float32(v[2])*scale[2] + translate[2],
			}
			if int(v[3]) < len(md2Normals) {
				norms[vi] = md2Normals[v[3]]
			}
		}
		poses[fi] = framePose{name: name, xyz: xyz, normals: norms}
	}

	// Expand (vertIdx, stIdx) into the glTF-friendly unique-vertex
	// representation. Track the original xyz index alongside so we
	// can write the per-frame morph deltas for the same expanded
	// table.
	type expanded struct {
		xyzIdx uint32
		stIdx  uint16
	}
	uniqueKey := func(xyz uint32, st uint16) uint64 {
		return uint64(xyz)<<32 | uint64(st)
	}
	keyToExpandedIdx := make(map[uint64]uint32, h.NumXYZ)
	expanded3 := make([]expanded, 0, h.NumXYZ)
	model := &Model{
		Name:      "md2",
		Triangles: make([]Triangle, 0, h.NumTris),
	}
	for _, t := range tris {
		var corners [3]uint32
		for ci := 0; ci < 3; ci++ {
			key := uniqueKey(uint32(t.VertIdx[ci]), t.STIdx[ci])
			idx, ok := keyToExpandedIdx[key]
			if !ok {
				idx = uint32(len(expanded3))
				keyToExpandedIdx[key] = idx
				expanded3 = append(expanded3, expanded{
					xyzIdx: uint32(t.VertIdx[ci]),
					stIdx:  t.STIdx[ci],
				})
			}
			corners[ci] = idx
		}
		model.Triangles = append(model.Triangles, Triangle{A: corners[0], B: corners[1], C: corners[2]})
	}

	// Build the rest pose vertex list from the expanded table.
	rest := poses[0]
	model.Vertices = make([]Vertex, len(expanded3))
	for i, ex := range expanded3 {
		st := stEntries[ex.stIdx]
		model.Vertices[i] = Vertex{
			Position: rest.xyz[ex.xyzIdx],
			Normal:   rest.normals[ex.xyzIdx],
			TexCoord: [2]float32{float32(st.S) / skinW, float32(st.T) / skinH},
		}
	}

	// Skin name (rarely used at runtime but useful as a hint when
	// the upload's missing the texture companion).
	if h.NumSkins > 0 && int(h.OffsetSkins)+64 <= len(data) {
		name := strings.TrimRight(string(data[h.OffsetSkins:h.OffsetSkins+64]), "\x00 ")
		model.Material.Name = name
	}

	// Animation: one entry covering every non-rest frame as morph
	// targets. MD2's canonical FPS is 10 (Quake II ran the in-game
	// model interpolation at 10 frames/sec source rate).
	if h.NumFrames > 1 {
		anim := Animation{Name: "default", FPS: 10}
		anim.Frames = make([]AnimationFrame, 0, h.NumFrames-1)
		for fi := uint32(1); fi < h.NumFrames; fi++ {
			f := AnimationFrame{
				Positions: make([][3]float32, len(expanded3)),
				Normals:   make([][3]float32, len(expanded3)),
			}
			for i, ex := range expanded3 {
				f.Positions[i] = poses[fi].xyz[ex.xyzIdx]
				f.Normals[i] = poses[fi].normals[ex.xyzIdx]
			}
			anim.Frames = append(anim.Frames, f)
		}
		model.Animations = []Animation{anim}
	}
	return model, nil
}

// unsafeSize approximates binary.Size without using reflect at the
// call site. The md2Header is fixed at 17 uint32 = 68 bytes; using
// binary.Size lets us avoid hardcoding magic numbers but cost a
// reflect call per parse. Cheap enough.
func unsafeSize(v any) int {
	return binary.Size(v)
}

// md2Normals is the precomputed normal table id Software bakes into
// Quake II. The MD2 vertex format stores a single-byte index into
// this 162-entry sphere of unit normals, so the decoder has to ship
// the lookup table verbatim. Values lifted from anorms.h in the
// public Quake II SDK.
var md2Normals = [md2VertexNormalsCount][3]float32{
	{-0.525731, 0.000000, 0.850651}, {-0.442863, 0.238856, 0.864188},
	{-0.295242, 0.000000, 0.955423}, {-0.309017, 0.500000, 0.809017},
	{-0.162460, 0.262866, 0.951056}, {0.000000, 0.000000, 1.000000},
	{0.000000, 0.850651, 0.525731}, {-0.147621, 0.716567, 0.681718},
	{0.147621, 0.716567, 0.681718}, {0.000000, 0.525731, 0.850651},
	{0.309017, 0.500000, 0.809017}, {0.525731, 0.000000, 0.850651},
	{0.295242, 0.000000, 0.955423}, {0.442863, 0.238856, 0.864188},
	{0.162460, 0.262866, 0.951056}, {-0.681718, 0.147621, 0.716567},
	{-0.809017, 0.309017, 0.500000}, {-0.587785, 0.425325, 0.688191},
	{-0.850651, 0.525731, 0.000000}, {-0.864188, 0.442863, 0.238856},
	{-0.716567, 0.681718, 0.147621}, {-0.688191, 0.587785, 0.425325},
	{-0.500000, 0.809017, 0.309017}, {-0.238856, 0.864188, 0.442863},
	{-0.425325, 0.688191, 0.587785}, {-0.716567, 0.681718, -0.147621},
	{-0.500000, 0.809017, -0.309017}, {-0.525731, 0.850651, 0.000000},
	{0.000000, 0.850651, -0.525731}, {-0.238856, 0.864188, -0.442863},
	{0.000000, 0.955423, -0.295242}, {-0.262866, 0.951056, -0.162460},
	{0.000000, 1.000000, 0.000000}, {0.000000, 0.955423, 0.295242},
	{-0.262866, 0.951056, 0.162460}, {0.238856, 0.864188, 0.442863},
	{0.262866, 0.951056, 0.162460}, {0.500000, 0.809017, 0.309017},
	{0.238856, 0.864188, -0.442863}, {0.262866, 0.951056, -0.162460},
	{0.500000, 0.809017, -0.309017}, {0.850651, 0.525731, 0.000000},
	{0.716567, 0.681718, 0.147621}, {0.716567, 0.681718, -0.147621},
	{0.525731, 0.850651, 0.000000}, {0.425325, 0.688191, 0.587785},
	{0.864188, 0.442863, 0.238856}, {0.688191, 0.587785, 0.425325},
	{0.809017, 0.309017, 0.500000}, {0.681718, 0.147621, 0.716567},
	{0.587785, 0.425325, 0.688191}, {0.955423, 0.295242, 0.000000},
	{1.000000, 0.000000, 0.000000}, {0.951056, 0.162460, 0.262866},
	{0.850651, -0.525731, 0.000000}, {0.955423, -0.295242, 0.000000},
	{0.864188, -0.442863, 0.238856}, {0.951056, -0.162460, 0.262866},
	{0.809017, -0.309017, 0.500000}, {0.681718, -0.147621, 0.716567},
	{0.850651, 0.000000, 0.525731}, {0.864188, 0.442863, -0.238856},
	{0.809017, 0.309017, -0.500000}, {0.951056, 0.162460, -0.262866},
	{0.525731, 0.000000, -0.850651}, {0.681718, 0.147621, -0.716567},
	{0.681718, -0.147621, -0.716567}, {0.850651, 0.000000, -0.525731},
	{0.809017, -0.309017, -0.500000}, {0.864188, -0.442863, -0.238856},
	{0.951056, -0.162460, -0.262866}, {0.147621, 0.716567, -0.681718},
	{0.309017, 0.500000, -0.809017}, {0.425325, 0.688191, -0.587785},
	{0.442863, 0.238856, -0.864188}, {0.587785, 0.425325, -0.688191},
	{0.688191, 0.587785, -0.425325}, {-0.147621, 0.716567, -0.681718},
	{-0.309017, 0.500000, -0.809017}, {0.000000, 0.525731, -0.850651},
	{-0.525731, 0.000000, -0.850651}, {-0.442863, 0.238856, -0.864188},
	{-0.295242, 0.000000, -0.955423}, {-0.162460, 0.262866, -0.951056},
	{0.000000, 0.000000, -1.000000}, {0.295242, 0.000000, -0.955423},
	{0.162460, 0.262866, -0.951056}, {-0.442863, -0.238856, -0.864188},
	{-0.309017, -0.500000, -0.809017}, {-0.162460, -0.262866, -0.951056},
	{0.000000, -0.850651, -0.525731}, {-0.147621, -0.716567, -0.681718},
	{0.147621, -0.716567, -0.681718}, {0.000000, -0.525731, -0.850651},
	{0.309017, -0.500000, -0.809017}, {0.442863, -0.238856, -0.864188},
	{0.162460, -0.262866, -0.951056}, {0.238856, -0.864188, -0.442863},
	{0.500000, -0.809017, -0.309017}, {0.425325, -0.688191, -0.587785},
	{0.716567, -0.681718, -0.147621}, {0.688191, -0.587785, -0.425325},
	{0.587785, -0.425325, -0.688191}, {0.000000, -0.955423, -0.295242},
	{0.000000, -1.000000, 0.000000}, {0.262866, -0.951056, -0.162460},
	{0.000000, -0.850651, 0.525731}, {0.000000, -0.955423, 0.295242},
	{0.238856, -0.864188, 0.442863}, {0.262866, -0.951056, 0.162460},
	{0.500000, -0.809017, 0.309017}, {0.716567, -0.681718, 0.147621},
	{0.525731, -0.850651, 0.000000}, {-0.238856, -0.864188, -0.442863},
	{-0.500000, -0.809017, -0.309017}, {-0.262866, -0.951056, -0.162460},
	{-0.850651, -0.525731, 0.000000}, {-0.716567, -0.681718, -0.147621},
	{-0.716567, -0.681718, 0.147621}, {-0.525731, -0.850651, 0.000000},
	{-0.500000, -0.809017, 0.309017}, {-0.238856, -0.864188, 0.442863},
	{-0.262866, -0.951056, 0.162460}, {-0.864188, -0.442863, 0.238856},
	{-0.809017, -0.309017, 0.500000}, {-0.688191, -0.587785, 0.425325},
	{-0.681718, -0.147621, 0.716567}, {-0.442863, -0.238856, 0.864188},
	{-0.587785, -0.425325, 0.688191}, {-0.309017, -0.500000, 0.809017},
	{-0.147621, -0.716567, 0.681718}, {-0.425325, -0.688191, 0.587785},
	{-0.162460, -0.262866, 0.951056}, {0.442863, -0.238856, 0.864188},
	{0.162460, -0.262866, 0.951056}, {0.309017, -0.500000, 0.809017},
	{0.147621, -0.716567, 0.681718}, {0.000000, -0.525731, 0.850651},
	{0.425325, -0.688191, 0.587785}, {0.587785, -0.425325, 0.688191},
	{0.688191, -0.587785, 0.425325}, {-0.955423, 0.295242, 0.000000},
	{-0.951056, 0.162460, 0.262866}, {-1.000000, 0.000000, 0.000000},
	{-0.850651, 0.000000, 0.525731}, {-0.955423, -0.295242, 0.000000},
	{-0.951056, -0.162460, 0.262866}, {-0.864188, 0.442863, -0.238856},
	{-0.951056, 0.162460, -0.262866}, {-0.809017, 0.309017, -0.500000},
	{-0.864188, -0.442863, -0.238856}, {-0.951056, -0.162460, -0.262866},
	{-0.809017, -0.309017, -0.500000}, {-0.681718, 0.147621, -0.716567},
	{-0.681718, -0.147621, -0.716567}, {-0.850651, 0.000000, -0.525731},
	{-0.688191, 0.587785, -0.425325}, {-0.587785, 0.425325, -0.688191},
	{-0.425325, 0.688191, -0.587785}, {-0.425325, -0.688191, -0.587785},
	{-0.587785, -0.425325, -0.688191}, {-0.688191, -0.587785, -0.425325},
}
