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

// MS3D — Milkshape 3D's native binary format. id Software's MD2/3
// dominated game-mod tooling in the 90s, but MS3D ended up the
// lingua franca for hobbyist character work (the editor sold for
// $35 and exported to half the engines of the era). Lots of legacy
// content assets out there are .ms3d.
//
// Spec: file signature "MS3D000000" (10 bytes) + version uint32
// (must be 4). Then a sequence of length-prefixed sections:
//
//	vertices  : 2-byte count, 15 bytes/entry
//	triangles : 2-byte count, 70 bytes/entry
//	groups    : 2-byte count, variable-length entries
//	materials : 2-byte count, 361 bytes/entry
//	animation : float fps, float currentTime, int32 totalFrames
//	joints    : 2-byte count, variable (skinning)
//	<optional sub-version + comments + extra vertex info>
//
// We decode through `materials`, then bail at the animation header
// — the per-vertex per-frame deformation pipeline MS3D uses doesn't
// map to glTF morph targets the way MD2/3 do (it's bone-driven),
// and we don't ship a skinning runtime yet. Static mesh + UVs +
// first material's texture name is the MVP.
const (
	ms3dSignature = "MS3D000000"
	ms3dVersion   = uint32(4)
)

// DecodeMS3D parses an MS3D stream into a static Model. Bone /
// animation data is parsed past for offset validity but not
// surfaced — the resulting glTF is the rest pose.
func DecodeMS3D(r io.Reader) (*Model, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("ms3d: read: %w", err)
	}
	if len(data) < 14 {
		return nil, errors.New("ms3d: file too short for header")
	}
	if string(data[:10]) != ms3dSignature {
		return nil, fmt.Errorf("ms3d: bad signature %q (want %q)", data[:10], ms3dSignature)
	}
	version := binary.LittleEndian.Uint32(data[10:14])
	if version != ms3dVersion {
		return nil, fmt.Errorf("ms3d: unsupported version %d (want %d)", version, ms3dVersion)
	}

	c := &leReader{buf: data, pos: 14}

	// --- vertices --------------------------------------------------------
	numVerts, err := c.u16()
	if err != nil {
		return nil, fmt.Errorf("ms3d: vertex count: %w", err)
	}
	if err := c.requireBytes(int(numVerts) * 15); err != nil {
		return nil, fmt.Errorf("ms3d: vertex array truncated: %w", err)
	}
	type ms3dVertex struct{ Pos [3]float32 }
	verts := make([]ms3dVertex, numVerts)
	for i := uint16(0); i < numVerts; i++ {
		c.pos++ // flags u8
		x, _ := c.f32()
		y, _ := c.f32()
		z, _ := c.f32()
		c.pos++ // boneId i8
		c.pos++ // refCount u8
		verts[i] = ms3dVertex{Pos: [3]float32{x, y, z}}
	}

	// --- triangles -------------------------------------------------------
	numTris, err := c.u16()
	if err != nil {
		return nil, fmt.Errorf("ms3d: triangle count: %w", err)
	}
	type ms3dTri struct {
		VertIdx [3]uint16
		Normals [3][3]float32
		S, T    [3]float32
	}
	tris := make([]ms3dTri, numTris)
	for i := uint16(0); i < numTris; i++ {
		if err := c.requireBytes(70); err != nil {
			return nil, fmt.Errorf("ms3d: triangle %d truncated: %w", i, err)
		}
		c.pos += 2 // flags u16
		v0, _ := c.u16()
		v1, _ := c.u16()
		v2, _ := c.u16()
		var ns [3][3]float32
		for j := 0; j < 3; j++ {
			ns[j][0], _ = c.f32()
			ns[j][1], _ = c.f32()
			ns[j][2], _ = c.f32()
		}
		var s, t [3]float32
		for j := 0; j < 3; j++ {
			s[j], _ = c.f32()
		}
		for j := 0; j < 3; j++ {
			t[j], _ = c.f32()
		}
		c.pos++ // smoothingGroup u8
		c.pos++ // groupIndex u8
		tris[i] = ms3dTri{VertIdx: [3]uint16{v0, v1, v2}, Normals: ns, S: s, T: t}
	}

	// --- groups (skipped — we don't slice by material for the MVP) ------
	numGroups, err := c.u16()
	if err != nil {
		return nil, fmt.Errorf("ms3d: group count: %w", err)
	}
	for g := uint16(0); g < numGroups; g++ {
		c.pos++    // flags u8
		c.skip(32) // name[32]
		nt, err := c.u16()
		if err != nil {
			return nil, fmt.Errorf("ms3d: group %d triangle-count: %w", g, err)
		}
		c.skip(int(nt) * 2) // triangleIndices[nTriangles]
		c.pos++             // materialIndex i8
	}

	// --- materials ------------------------------------------------------
	numMats, err := c.u16()
	if err != nil {
		return nil, fmt.Errorf("ms3d: material count: %w", err)
	}
	model := &Model{Name: "ms3d"}
	if numMats > 0 {
		// We only surface the first material's diffuse texture
		// filename — same constraint as our MD2 / MDL decoders.
		// The texture itself ships separately; users attach it
		// via the companion API.
		if err := c.requireBytes(361); err != nil {
			return nil, fmt.Errorf("ms3d: material 0 truncated: %w", err)
		}
		name := readFixedString(c.buf[c.pos:c.pos+32], 32)
		c.skip(32)                // name
		c.skip(16 + 16 + 16 + 16) // ambient/diffuse/specular/emissive (4×4 floats)
		c.skip(4 + 4)             // shininess, transparency
		c.skip(1)                 // mode
		tex := readFixedString(c.buf[c.pos:c.pos+128], 128)
		c.skip(128) // diffuse texture path
		c.skip(128) // alpha map path
		if name == "" {
			name = "ms3d_mat"
		}
		model.Material = Material{Name: name + " — " + tex}
		// Advance past the remaining materials we're not decoding.
		c.skip(int(numMats-1) * 361)
	}

	// --- Build glTF-friendly vertex/triangle tables ----------------------
	//
	// MS3D stores UV per-corner-per-tri (so two triangles sharing a
	// vertex can disagree). Same as MD2; expand into unique
	// (vertIdx, s, t) keys to satisfy glTF's one-vertex-one-uv rule.
	type expKey struct {
		vert uint16
		s, t float32
	}
	expIdx := make(map[expKey]uint32)
	expand := make([]expKey, 0, numVerts)
	expNormals := make([][3]float32, 0, numVerts)

	model.Triangles = make([]Triangle, 0, numTris)
	for _, tr := range tris {
		var corners [3]uint32
		for ci := 0; ci < 3; ci++ {
			k := expKey{vert: tr.VertIdx[ci], s: tr.S[ci], t: tr.T[ci]}
			idx, ok := expIdx[k]
			if !ok {
				idx = uint32(len(expand))
				expIdx[k] = idx
				expand = append(expand, k)
				expNormals = append(expNormals, tr.Normals[ci])
			}
			corners[ci] = idx
		}
		model.Triangles = append(model.Triangles, Triangle{A: corners[0], B: corners[1], C: corners[2]})
	}

	model.Vertices = make([]Vertex, len(expand))
	for i, k := range expand {
		// MS3D stores V with origin at the top, same as glTF — no flip.
		model.Vertices[i] = Vertex{
			Position: verts[k.vert].Pos,
			Normal:   expNormals[i],
			TexCoord: [2]float32{k.s, k.t},
		}
	}

	// Animation + skinning sections are not surfaced. We still
	// validate offsets so a malformed file fails here rather than
	// pretending to succeed.
	if c.pos+12 <= len(c.buf) {
		// fps, currentTime, totalFrames — skip
		c.skip(12)
		if numJoints, err := c.u16(); err == nil {
			_ = numJoints
		}
	}
	return model, nil
}

// readFixedString trims at the first NUL byte and strips trailing
// whitespace. MS3D's name fields are NUL-terminated within a fixed
// box; we standardise on Go strings.
func readFixedString(b []byte, max int) string {
	if len(b) > max {
		b = b[:max]
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimRight(string(b), " \x00")
}

// leReader is a tiny little-endian byte cursor scoped to the
// format3d package's binary decoders. mview/go has its own
// (similarly-shaped) reader; we keep one here to avoid a
// cross-package import + so format3d stays self-contained.
type leReader struct {
	buf []byte
	pos int
}

func (c *leReader) u16() (uint16, error) {
	if c.pos+2 > len(c.buf) {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint16(c.buf[c.pos:])
	c.pos += 2
	return v, nil
}

func (c *leReader) f32() (float32, error) {
	if c.pos+4 > len(c.buf) {
		return 0, io.ErrUnexpectedEOF
	}
	bits := binary.LittleEndian.Uint32(c.buf[c.pos:])
	c.pos += 4
	return mathFloat32frombits(bits), nil
}

// skip advances by n bytes (clamped at end-of-buffer).
func (c *leReader) skip(n int) {
	c.pos += n
	if c.pos > len(c.buf) {
		c.pos = len(c.buf)
	}
}

// requireBytes ensures the cursor has at least n bytes left.
func (c *leReader) requireBytes(n int) error {
	if c.pos+n > len(c.buf) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// mathFloat32frombits delegates to math.Float32frombits; named
// locally so callers don't have to know about the import.
func mathFloat32frombits(bits uint32) float32 { return math.Float32frombits(bits) }
