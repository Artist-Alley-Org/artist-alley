// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package format3d

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Synthesise a smallest-possible MS3D file: header + 3 verts +
// 1 triangle + 0 groups + 0 materials. Used to confirm the
// decoder hits the happy path without needing a real Milkshape
// export bundled in the repo.
func synthMS3D(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("MS3D000000")
	_ = binary.Write(&b, binary.LittleEndian, uint32(4)) // version

	// 3 vertices.
	_ = binary.Write(&b, binary.LittleEndian, uint16(3))
	verts := [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}}
	for _, v := range verts {
		b.WriteByte(0)                                            // flags
		_ = binary.Write(&b, binary.LittleEndian, v[0])
		_ = binary.Write(&b, binary.LittleEndian, v[1])
		_ = binary.Write(&b, binary.LittleEndian, v[2])
		b.WriteByte(0xFF) // boneId = -1 (no bone)
		b.WriteByte(0)    // refCount
	}

	// 1 triangle.
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(0)) // flags
	_ = binary.Write(&b, binary.LittleEndian, uint16(0))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))
	for i := 0; i < 3; i++ {
		_ = binary.Write(&b, binary.LittleEndian, float32(0))
		_ = binary.Write(&b, binary.LittleEndian, float32(0))
		_ = binary.Write(&b, binary.LittleEndian, float32(1))
	}
	for i := 0; i < 3; i++ {
		_ = binary.Write(&b, binary.LittleEndian, float32(float64(i)*0.5))
	}
	for i := 0; i < 3; i++ {
		_ = binary.Write(&b, binary.LittleEndian, float32(float64(i)*0.5))
	}
	b.WriteByte(0) // smoothing
	b.WriteByte(0) // group

	// 0 groups, 0 materials.
	_ = binary.Write(&b, binary.LittleEndian, uint16(0))
	_ = binary.Write(&b, binary.LittleEndian, uint16(0))
	return b.Bytes()
}

func TestDecodeMS3D_HappyPath(t *testing.T) {
	data := synthMS3D(t)
	m, err := DecodeMS3D(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeMS3D: %v", err)
	}
	if len(m.Triangles) != 1 {
		t.Fatalf("triangle count: got %d want 1", len(m.Triangles))
	}
	// Expanded vertices should be 3 distinct (vert, uv) pairs since
	// each corner of our single tri has unique UV.
	if len(m.Vertices) != 3 {
		t.Fatalf("vertex count: got %d want 3 (one per unique (vert, uv))", len(m.Vertices))
	}
}

func TestDecodeMS3D_BadSignature(t *testing.T) {
	garbage := make([]byte, 14)
	if _, err := DecodeMS3D(bytes.NewReader(garbage)); err == nil {
		t.Fatal("expected error on zeroed header")
	}
}

func TestDecodeMS3D_UnsupportedVersion(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("MS3D000000")
	_ = binary.Write(&b, binary.LittleEndian, uint32(99))
	if _, err := DecodeMS3D(bytes.NewReader(b.Bytes())); err == nil {
		t.Fatal("expected error on unsupported version")
	}
}
