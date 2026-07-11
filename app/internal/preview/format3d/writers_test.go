// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package format3d

import (
	"bytes"
	"math"
	"testing"
)

// fixtureCube builds a 1×1×1 cube with one animation frame that
// scales every vertex up to 1.5× — exercises both the static
// vertex stream and the morph-target path.
func fixtureCube() *Model {
	verts := []Vertex{
		{Position: [3]float32{0, 0, 0}, Normal: [3]float32{-1, 0, 0}, TexCoord: [2]float32{0, 0}},
		{Position: [3]float32{1, 0, 0}, Normal: [3]float32{1, 0, 0}, TexCoord: [2]float32{1, 0}},
		{Position: [3]float32{1, 1, 0}, Normal: [3]float32{0, 1, 0}, TexCoord: [2]float32{1, 1}},
		{Position: [3]float32{0, 1, 0}, Normal: [3]float32{0, 1, 0}, TexCoord: [2]float32{0, 1}},
		{Position: [3]float32{0, 0, 1}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{0, 0}},
		{Position: [3]float32{1, 0, 1}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{1, 0}},
		{Position: [3]float32{1, 1, 1}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{1, 1}},
		{Position: [3]float32{0, 1, 1}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{0, 1}},
	}
	tris := []Triangle{
		{0, 1, 2}, {0, 2, 3},
		{4, 6, 5}, {4, 7, 6},
		{0, 4, 5}, {0, 5, 1},
		{2, 6, 7}, {2, 7, 3},
		{0, 3, 7}, {0, 7, 4},
		{1, 5, 6}, {1, 6, 2},
	}

	bigger := make([][3]float32, len(verts))
	biggerN := make([][3]float32, len(verts))
	for i, v := range verts {
		bigger[i] = [3]float32{v.Position[0] * 1.5, v.Position[1] * 1.5, v.Position[2] * 1.5}
		biggerN[i] = v.Normal
	}
	return &Model{
		Name:      "cube",
		Vertices:  verts,
		Triangles: tris,
		Material:  Material{Name: "cube_mat"},
		Animations: []Animation{
			{
				Name: "grow", FPS: 10,
				Frames: []AnimationFrame{{Positions: bigger, Normals: biggerN}},
			},
		},
	}
}

// approxEqual checks that the round-tripped vertex matches the
// original within format-quantisation tolerance. MD2/MDL use 0..255
// byte encoding scaled by bbox → max ~0.4% loss on the longest
// axis; MD3 uses 1/64 fixed-point so much tighter.
func approxEqual(a, b [3]float32, eps float32) bool {
	for i := 0; i < 3; i++ {
		d := a[i] - b[i]
		if d < 0 {
			d = -d
		}
		if d > eps {
			return false
		}
	}
	return true
}

func TestMD2_RoundTrip(t *testing.T) {
	src := fixtureCube()
	var buf bytes.Buffer
	if err := EncodeMD2(src, &buf); err != nil {
		t.Fatalf("EncodeMD2: %v", err)
	}
	got, err := DecodeMD2(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeMD2: %v", err)
	}
	if len(got.Triangles) != len(src.Triangles) {
		t.Fatalf("tri count: got %d want %d", len(got.Triangles), len(src.Triangles))
	}
	// MD2's (vertex, ST) expansion permutes the vertex list — the
	// decoder builds unique combos in triangle-visit order, not in
	// our original index order. Check at the triangle level instead.
	// MD2 quantises positions to 0..255 across the per-frame bbox →
	// 1/255 ≈ 0.004 worst-case per-component drift on a 1-unit cube.
	assertTrianglesRoundTrip(t, src, got, 0.01)

	if len(got.Animations) != 1 || len(got.Animations[0].Frames) != 1 {
		t.Fatalf("expected one animation with one frame, got %+v", got.Animations)
	}
}

// assertTrianglesRoundTrip checks that every (a, b, c) position
// triple in the original survives the encode → decode pass. The
// orientation may shift but each tri's corner positions must match
// the source's corresponding tri to within eps.
func assertTrianglesRoundTrip(t *testing.T, src, got *Model, eps float32) {
	t.Helper()
	if len(src.Triangles) != len(got.Triangles) {
		t.Fatalf("triangle count: src=%d got=%d", len(src.Triangles), len(got.Triangles))
	}
	for i, tr := range src.Triangles {
		want := [3][3]float32{
			src.Vertices[tr.A].Position,
			src.Vertices[tr.B].Position,
			src.Vertices[tr.C].Position,
		}
		gt := got.Triangles[i]
		have := [3][3]float32{
			got.Vertices[gt.A].Position,
			got.Vertices[gt.B].Position,
			got.Vertices[gt.C].Position,
		}
		for j := 0; j < 3; j++ {
			if !approxEqual(want[j], have[j], eps) {
				t.Errorf("tri %d corner %d: got %v want %v", i, j, have[j], want[j])
			}
		}
	}
}

func TestMD3_RoundTrip(t *testing.T) {
	src := fixtureCube()
	var buf bytes.Buffer
	if err := EncodeMD3(src, &buf); err != nil {
		t.Fatalf("EncodeMD3: %v", err)
	}
	got, err := DecodeMD3(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeMD3: %v", err)
	}
	// MD3 is 1/64 fixed-point — 1/128 per axis worst case.
	assertTrianglesRoundTrip(t, src, got, 0.02)
}

func TestMDL_RoundTrip(t *testing.T) {
	src := fixtureCube()
	var buf bytes.Buffer
	if err := EncodeMDL(src, &buf); err != nil {
		t.Fatalf("EncodeMDL: %v", err)
	}
	got, err := DecodeMDL(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeMDL: %v", err)
	}
	if len(got.Vertices) == 0 || len(got.Triangles) != len(src.Triangles) {
		t.Fatalf("tri count: got %d want %d", len(got.Triangles), len(src.Triangles))
	}
	if got.Material.DiffuseImageBytes == nil {
		t.Errorf("MDL should embed paletted skin PNG")
	}
}

func TestNearestMD2Normal_AxisAligned(t *testing.T) {
	// The MD2 normal table contains canonical axis vectors —
	// asking for +X / -X / +Y / -Y / +Z / -Z should round-trip
	// back to a normal within 1e-3 of the request.
	cases := [][3]float32{
		{1, 0, 0}, {-1, 0, 0},
		{0, 1, 0}, {0, -1, 0},
		{0, 0, 1}, {0, 0, -1},
	}
	for _, want := range cases {
		idx := nearestMD2Normal(want)
		got := md2Normals[idx]
		// Pick the closest table entry; the axis vectors exist in
		// the table so we should match to 1e-3.
		dot := got[0]*want[0] + got[1]*want[1] + got[2]*want[2]
		if math.Abs(float64(1-dot)) > 1e-3 {
			t.Errorf("nearest for %v returned %v (idx %d, dot %v)", want, got, idx, dot)
		}
	}
}
