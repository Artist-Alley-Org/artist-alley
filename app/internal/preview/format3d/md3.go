// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package format3d

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// MD3 (Quake III Arena) format. Unlike MD2, MD3 splits a character
// into multiple meshes ("surfaces") with their own materials. Quake
// III used this to swap individual body parts (head/upper/lower).
//
// Our intermediate Model is single-mesh-only, so we concatenate
// every surface into one merged mesh. Acceptable for the turntable
// thumbnail case; if a real authoring use shows up we'd promote
// Model.Vertices/Triangles to []MeshGroup.
const (
	md3Magic       uint32 = 0x33504449 // "IDP3"
	md3Version     uint32 = 15
	md3NameLen            = 64
	md3SurfaceMagic uint32 = 0x37534449 // "IDS7"
	md3XyzScale           = 1.0 / 64.0  // i16 → world units
)

// md3Header sits at offset 0.
type md3Header struct {
	Magic          uint32
	Version        uint32
	Name           [md3NameLen]byte
	Flags          int32
	NumFrames      int32
	NumTags        int32
	NumSurfaces    int32
	NumSkins       int32 // unused in MD3 spec; always 0
	OffsetFrames   int32
	OffsetTags     int32
	OffsetSurfaces int32
	OffsetEnd      int32
}

// md3Frame is the per-frame metadata block; the actual vertex data
// lives inside each surface (one surface = its own per-frame vertex
// stream).
type md3Frame struct {
	MinBounds [3]float32
	MaxBounds [3]float32
	LocalOrig [3]float32
	Radius    float32
	Name      [16]byte
}

// md3SurfaceHeader prefixes each surface block.
type md3SurfaceHeader struct {
	Magic           uint32
	Name            [md3NameLen]byte
	Flags           int32
	NumFrames       int32
	NumShaders      int32
	NumVerts        int32
	NumTriangles    int32
	OffsetTriangles int32
	OffsetShaders   int32
	OffsetST        int32
	OffsetXYZNormal int32
	OffsetEnd       int32
}

// DecodeMD3 parses a Quake III .md3 stream into a Model. All
// surfaces merge into the single output mesh. Per-frame morph
// targets cover every frame after the rest pose.
func DecodeMD3(r io.Reader) (*Model, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("md3: read: %w", err)
	}
	headerSize := binary.Size(md3Header{})
	if len(data) < headerSize {
		return nil, errors.New("md3: file too short for header")
	}

	var h md3Header
	if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &h); err != nil {
		return nil, fmt.Errorf("md3: header: %w", err)
	}
	if h.Magic != md3Magic {
		return nil, fmt.Errorf("md3: bad magic 0x%08x (want 0x%08x for 'IDP3')", h.Magic, md3Magic)
	}
	if h.Version != md3Version {
		return nil, fmt.Errorf("md3: unsupported version %d (want %d)", h.Version, md3Version)
	}
	if h.NumSurfaces <= 0 || h.NumFrames <= 0 {
		return nil, fmt.Errorf("md3: empty model (surfaces=%d frames=%d)", h.NumSurfaces, h.NumFrames)
	}

	model := &Model{
		Name: strings.TrimRight(string(h.Name[:]), "\x00 "),
	}

	// Walk surfaces in order. Each surface's offsets are relative to
	// its own header position, not the file. We advance a cursor by
	// surface.OffsetEnd between surfaces.
	surfacePos := int(h.OffsetSurfaces)
	type accumulatedFrame struct {
		positions [][3]float32
		normals   [][3]float32
	}
	frames := make([]accumulatedFrame, h.NumFrames)

	for si := int32(0); si < h.NumSurfaces; si++ {
		if surfacePos < 0 || surfacePos >= len(data) {
			return nil, fmt.Errorf("md3: surface %d offset %d out of file (size %d)", si, surfacePos, len(data))
		}
		var sh md3SurfaceHeader
		if err := binary.Read(bytes.NewReader(data[surfacePos:]), binary.LittleEndian, &sh); err != nil {
			return nil, fmt.Errorf("md3: surface %d header: %w", si, err)
		}
		if sh.Magic != md3SurfaceMagic {
			return nil, fmt.Errorf("md3: surface %d bad magic 0x%08x", si, sh.Magic)
		}
		if sh.NumFrames != h.NumFrames {
			return nil, fmt.Errorf("md3: surface %d frame mismatch (have %d, scene has %d)",
				si, sh.NumFrames, h.NumFrames)
		}

		surfaceBase := surfacePos
		baseVertex := uint32(len(model.Vertices))

		// Triangles — three int32 vertex indices each, local to this
		// surface. We offset by baseVertex when merging.
		triOff := surfaceBase + int(sh.OffsetTriangles)
		triEnd := triOff + int(sh.NumTriangles)*12
		if triEnd > len(data) {
			return nil, fmt.Errorf("md3: surface %d triangle table truncated", si)
		}
		triReader := bytes.NewReader(data[triOff:triEnd])
		for ti := int32(0); ti < sh.NumTriangles; ti++ {
			var ind [3]int32
			_ = binary.Read(triReader, binary.LittleEndian, &ind)
			model.Triangles = append(model.Triangles, Triangle{
				A: baseVertex + uint32(ind[0]),
				B: baseVertex + uint32(ind[1]),
				C: baseVertex + uint32(ind[2]),
			})
		}

		// ST table — two float32 per vertex.
		stOff := surfaceBase + int(sh.OffsetST)
		stEnd := stOff + int(sh.NumVerts)*8
		if stEnd > len(data) {
			return nil, fmt.Errorf("md3: surface %d ST table truncated", si)
		}
		uvs := make([][2]float32, sh.NumVerts)
		_ = binary.Read(bytes.NewReader(data[stOff:stEnd]), binary.LittleEndian, &uvs)

		// XYZ + normal stream — per-frame, 8 bytes per vertex.
		//   xyz: 3 × int16 (multiply by 1/64 for world units)
		//   normal: encoded lat/lon — 1 byte each — into a unit vec
		xyzStride := int(sh.NumVerts) * 8
		for fi := int32(0); fi < h.NumFrames; fi++ {
			off := surfaceBase + int(sh.OffsetXYZNormal) + int(fi)*xyzStride
			if off+xyzStride > len(data) {
				return nil, fmt.Errorf("md3: surface %d frame %d truncated", si, fi)
			}
			frameVerts := data[off : off+xyzStride]
			if frames[fi].positions == nil {
				frames[fi].positions = make([][3]float32, 0)
				frames[fi].normals = make([][3]float32, 0)
			}
			for vi := int32(0); vi < sh.NumVerts; vi++ {
				v := frameVerts[vi*8 : (vi+1)*8]
				x := int16(binary.LittleEndian.Uint16(v[0:]))
				y := int16(binary.LittleEndian.Uint16(v[2:]))
				z := int16(binary.LittleEndian.Uint16(v[4:]))
				pos := [3]float32{
					float32(x) * md3XyzScale,
					float32(y) * md3XyzScale,
					float32(z) * md3XyzScale,
				}
				n := md3UnpackNormal(v[6], v[7])
				frames[fi].positions = append(frames[fi].positions, pos)
				frames[fi].normals = append(frames[fi].normals, n)

				if fi == 0 {
					model.Vertices = append(model.Vertices, Vertex{
						Position: pos,
						Normal:   n,
						TexCoord: uvs[vi],
					})
				}
			}
		}

		// First surface's first shader becomes the model's material
		// name. MD3 ships the shader name (e.g. "models/players/sarge/sarge.tga")
		// not the bytes — caller would need a companion to get the
		// actual texture.
		if si == 0 && sh.NumShaders > 0 && model.Material.Name == "" {
			shaderOff := surfaceBase + int(sh.OffsetShaders)
			if shaderOff+md3NameLen <= len(data) {
				name := strings.TrimRight(string(data[shaderOff:shaderOff+md3NameLen]), "\x00 ")
				model.Material.Name = name
			}
		}

		surfacePos += int(sh.OffsetEnd)
	}

	// Build per-frame morph targets out of accumulated positions.
	// Frame 0 is the rest pose so it doesn't become a morph entry.
	if h.NumFrames > 1 {
		anim := Animation{Name: "default", FPS: 15} // Quake 3 default model FPS
		anim.Frames = make([]AnimationFrame, 0, h.NumFrames-1)
		for fi := int32(1); fi < h.NumFrames; fi++ {
			anim.Frames = append(anim.Frames, AnimationFrame{
				Positions: frames[fi].positions,
				Normals:   frames[fi].normals,
			})
		}
		model.Animations = []Animation{anim}
	}
	return model, nil
}

// md3UnpackNormal recovers a unit vector from MD3's two-byte
// spherical coords. lat / lng are unsigned bytes; the runtime
// shader maps them to radians via:
//
//	lat = byte0 × 2π / 255
//	lng = byte1 × 2π / 255
//	x = cos(lng) × sin(lat)
//	y = sin(lng) × sin(lat)
//	z = cos(lat)
func md3UnpackNormal(b0, b1 byte) [3]float32 {
	lat := float64(b0) * (2 * math.Pi / 255)
	lng := float64(b1) * (2 * math.Pi / 255)
	sl := math.Sin(lat)
	return [3]float32{
		float32(math.Cos(lng) * sl),
		float32(math.Sin(lng) * sl),
		float32(math.Cos(lat)),
	}
}
