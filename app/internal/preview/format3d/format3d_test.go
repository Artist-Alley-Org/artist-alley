package format3d

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/qmuntal/gltf"
)

// --- MD2 synthetic round-trip --------------------------------------------

func TestDecodeMD2_SyntheticTriangle(t *testing.T) {
	// Smallest valid MD2: 1 frame, 3 verts, 1 triangle, 1 skin.
	// Layout has to match md2Header field-for-field, then ST table,
	// then triangles, then frames. Skins are first by spec but we
	// place skin AFTER header at offset = sizeof(header) to keep
	// the math straightforward.
	const (
		skinW, skinH = 64, 64
		numXYZ       = 3
		numST        = 3
		numTris      = 1
		numFrames    = 1
		numSkins     = 1
	)
	hdrSize := uint32(binary.Size(md2Header{}))
	skinSize := uint32(64)
	stSize := uint32(numST * 4)
	triSize := uint32(numTris * 12)
	frameSize := uint32(6*4 + md2FrameNameLen + numXYZ*4)

	offSkins := hdrSize
	offST := offSkins + skinSize
	offTris := offST + stSize
	offFrames := offTris + triSize
	offEnd := offFrames + frameSize*numFrames

	h := md2Header{
		Magic: md2Magic, Version: md2Version,
		SkinWidth: skinW, SkinHeight: skinH,
		FrameSize: frameSize,
		NumSkins:  numSkins, NumXYZ: numXYZ, NumST: numST,
		NumTris: numTris, NumFrames: numFrames,
		OffsetSkins: offSkins, OffsetST: offST, OffsetTris: offTris,
		OffsetFrames: offFrames, OffsetEnd: offEnd,
	}

	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, h)

	// Skin (64-byte name padded with NULs).
	skinName := append([]byte("synthetic.pcx"), make([]byte, 64-len("synthetic.pcx"))...)
	buf.Write(skinName)

	// ST entries — (0,0), (skinW,0), (0,skinH).
	for _, st := range [][2]int16{{0, 0}, {skinW, 0}, {0, skinH}} {
		_ = binary.Write(&buf, binary.LittleEndian, st[0])
		_ = binary.Write(&buf, binary.LittleEndian, st[1])
	}

	// Triangle — refers to verts 0,1,2 and ST 0,1,2.
	_ = binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, 2}) // vert
	_ = binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, 2}) // ST

	// Frame: scale (1,1,1), translate (0,0,0), name "frame00",
	// then 3 verts at (0,0,0), (255,0,0), (0,255,0) with normal 0.
	_ = binary.Write(&buf, binary.LittleEndian, [3]float32{1, 1, 1})
	_ = binary.Write(&buf, binary.LittleEndian, [3]float32{0, 0, 0})
	name := append([]byte("frame00"), make([]byte, md2FrameNameLen-7)...)
	buf.Write(name)
	for _, v := range [][4]uint8{
		{0, 0, 0, 0},
		{255, 0, 0, 0},
		{0, 255, 0, 0},
	} {
		buf.Write(v[:])
	}

	model, err := DecodeMD2(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeMD2: %v", err)
	}
	if len(model.Vertices) != 3 {
		t.Errorf("vertices: got %d want 3", len(model.Vertices))
	}
	if len(model.Triangles) != 1 {
		t.Errorf("triangles: got %d want 1", len(model.Triangles))
	}
	if model.Material.Name != "synthetic.pcx" {
		t.Errorf("skin name: got %q", model.Material.Name)
	}
}

func TestDecodeMD2_BadMagic(t *testing.T) {
	garbage := make([]byte, binary.Size(md2Header{}))
	if _, err := DecodeMD2(bytes.NewReader(garbage)); err == nil {
		t.Fatal("expected error on zeroed header")
	}
}

// --- GLB writer ----------------------------------------------------------

func TestWriteGLB_RoundTrip(t *testing.T) {
	m := &Model{
		Name: "T",
		Vertices: []Vertex{
			{Position: [3]float32{0, 0, 0}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{0, 0}},
			{Position: [3]float32{1, 0, 0}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{1, 0}},
			{Position: [3]float32{0, 1, 0}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{0, 1}},
		},
		Triangles: []Triangle{{A: 0, B: 1, C: 2}},
		Material:  Material{Name: "grey"},
	}
	glb, err := WriteGLBBytes(m)
	if err != nil {
		t.Fatalf("WriteGLBBytes: %v", err)
	}
	if string(glb[:4]) != "glTF" {
		t.Fatalf("output not a GLB (magic %q)", glb[:4])
	}
	var doc gltf.Document
	if err := gltf.NewDecoder(bytes.NewReader(glb)).Decode(&doc); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if len(doc.Meshes) != 1 {
		t.Errorf("expected one mesh, got %d", len(doc.Meshes))
	}
	if len(doc.Materials) != 1 {
		t.Errorf("expected one material, got %d", len(doc.Materials))
	}
}

func TestWriteGLB_RejectsEmpty(t *testing.T) {
	if _, err := WriteGLBBytes(&Model{}); err == nil {
		t.Fatal("expected error for empty model")
	}
}

// --- MD3 + MDL: header-level sanity (we don't synthesise full
// files; their bring-up tests come when we wire in real samples).

func TestDecodeMD3_BadMagic(t *testing.T) {
	garbage := make([]byte, binary.Size(md3Header{}))
	if _, err := DecodeMD3(bytes.NewReader(garbage)); err == nil {
		t.Fatal("expected error on zeroed header")
	}
}

func TestDecodeMDL_BadMagic(t *testing.T) {
	garbage := make([]byte, binary.Size(mdlHeader{}))
	if _, err := DecodeMDL(bytes.NewReader(garbage)); err == nil {
		t.Fatal("expected error on zeroed header")
	}
}
