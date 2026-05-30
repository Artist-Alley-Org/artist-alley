package format3d

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
)

// MDL (Quake I .mdl) decoder. Format is simpler than MD2/MD3 — one
// mesh, one paletted texture, per-frame vertex animation as a
// single linear stream.
//
// We support the "simple frame" variant. Quake also supported
// "frame groups" (a sub-animation with its own interval array) for
// idle loops; those are flagged by frame type == 1 and we skip
// them gracefully (the static rest pose still works).
const (
	mdlMagic   uint32 = 0x4F504449 // "IDPO"
	mdlVersion uint32 = 6
	mdlNameLen        = 16
)

type mdlHeader struct {
	Magic       uint32
	Version     uint32
	Scale       [3]float32
	Origin      [3]float32
	BoundingRad float32
	EyePosition [3]float32
	NumSkins    int32
	SkinWidth   int32
	SkinHeight  int32
	NumVerts    int32
	NumTris     int32
	NumFrames   int32
	SyncType    int32 // 0 = synchron, 1 = random
	Flags       int32
	Size        float32 // average triangle size
}

// DecodeMDL parses a Quake I .mdl. Texture is decoded from the
// embedded palette into a PNG so the GLB carries a real raster.
func DecodeMDL(r io.Reader) (*Model, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("mdl: read: %w", err)
	}
	headerSize := binary.Size(mdlHeader{})
	if len(data) < headerSize {
		return nil, errors.New("mdl: file too short for header")
	}
	var h mdlHeader
	if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &h); err != nil {
		return nil, fmt.Errorf("mdl: header: %w", err)
	}
	if h.Magic != mdlMagic {
		return nil, fmt.Errorf("mdl: bad magic 0x%08x (want 0x%08x for 'IDPO')", h.Magic, mdlMagic)
	}
	if h.Version != mdlVersion {
		return nil, fmt.Errorf("mdl: unsupported version %d (want %d)", h.Version, mdlVersion)
	}
	if h.NumVerts <= 0 || h.NumTris <= 0 || h.NumFrames <= 0 {
		return nil, fmt.Errorf("mdl: empty model (verts=%d tris=%d frames=%d)", h.NumVerts, h.NumTris, h.NumFrames)
	}

	cursor := headerSize

	// Skin table. Each entry: int32 group (0 = single skin, 1 =
	// group with multiple frames) + width*height palette indices.
	// We grab just the first skin (the most-used one in practice).
	skinSize := int(h.SkinWidth * h.SkinHeight)
	if skinSize <= 0 {
		return nil, errors.New("mdl: skin dimensions invalid")
	}
	var firstSkinIndices []byte
	for si := int32(0); si < h.NumSkins; si++ {
		if cursor+4 > len(data) {
			return nil, errors.New("mdl: skin table truncated")
		}
		group := int32(binary.LittleEndian.Uint32(data[cursor:]))
		cursor += 4
		if group == 0 {
			if cursor+skinSize > len(data) {
				return nil, errors.New("mdl: simple skin truncated")
			}
			if si == 0 {
				firstSkinIndices = data[cursor : cursor+skinSize]
			}
			cursor += skinSize
		} else {
			// Group skin — interval count then sub-skins. We skip
			// it without decoding; first skin we found (if any) wins.
			if cursor+4 > len(data) {
				return nil, errors.New("mdl: group skin header truncated")
			}
			nGroup := int32(binary.LittleEndian.Uint32(data[cursor:]))
			cursor += 4
			if cursor+int(nGroup)*4 > len(data) { // intervals
				return nil, errors.New("mdl: group skin intervals truncated")
			}
			cursor += int(nGroup) * 4
			groupBytes := int(nGroup) * skinSize
			if cursor+groupBytes > len(data) {
				return nil, errors.New("mdl: group skin payload truncated")
			}
			if si == 0 && firstSkinIndices == nil {
				firstSkinIndices = data[cursor : cursor+skinSize]
			}
			cursor += groupBytes
		}
	}

	// ST coords. 12 bytes each: onseam(i32), s(i32), t(i32). Quake
	// used the 'onseam' flag to mark verts on the back-of-model
	// texture seam — those need an extra horizontal half-width
	// offset when used on a back-facing triangle.
	if cursor+int(h.NumVerts)*12 > len(data) {
		return nil, errors.New("mdl: ST table truncated")
	}
	type rawST struct{ OnSeam, S, T int32 }
	stEntries := make([]rawST, h.NumVerts)
	_ = binary.Read(bytes.NewReader(data[cursor:cursor+int(h.NumVerts)*12]), binary.LittleEndian, &stEntries)
	cursor += int(h.NumVerts) * 12

	// Triangles. 16 bytes each: facesfront(i32), vertindex[3](i32).
	if cursor+int(h.NumTris)*16 > len(data) {
		return nil, errors.New("mdl: triangle table truncated")
	}
	type rawTri struct {
		FacesFront int32
		VertIdx    [3]int32
	}
	tris := make([]rawTri, h.NumTris)
	_ = binary.Read(bytes.NewReader(data[cursor:cursor+int(h.NumTris)*16]), binary.LittleEndian, &tris)
	cursor += int(h.NumTris) * 16

	// Frames. Simple frame: type(i32 = 0) + bbox_min(4) + bbox_max(4)
	// + name(16) + verts(numVerts × 4 bytes). Frame groups (type==1)
	// — bail; we skip them.
	type rawFrame struct {
		name string
		xyz  [][3]float32
		nrm  [][3]float32
	}
	frames := make([]rawFrame, 0, h.NumFrames)
	for fi := int32(0); fi < h.NumFrames; fi++ {
		if cursor+4 > len(data) {
			return nil, errors.New("mdl: frame header truncated")
		}
		ftype := int32(binary.LittleEndian.Uint32(data[cursor:]))
		cursor += 4
		if ftype != 0 {
			// Group frame: skip its sub-header + sub-frames.
			if cursor+4 > len(data) {
				return nil, errors.New("mdl: group frame header truncated")
			}
			nSub := int32(binary.LittleEndian.Uint32(data[cursor:]))
			cursor += 4
			cursor += 8 + int(nSub)*4 // group bbox + intervals
			for gi := int32(0); gi < nSub; gi++ {
				cursor += 8 + mdlNameLen + int(h.NumVerts)*4
				if cursor > len(data) {
					return nil, errors.New("mdl: group sub-frame truncated")
				}
			}
			continue
		}
		// Simple frame body: 4 bytes bboxmin + 4 bboxmax + name + verts.
		bodySize := 4 + 4 + mdlNameLen + int(h.NumVerts)*4
		if cursor+bodySize > len(data) {
			return nil, errors.New("mdl: simple frame truncated")
		}
		body := data[cursor : cursor+bodySize]
		cursor += bodySize

		nameBytes := body[8 : 8+mdlNameLen]
		name := strings.TrimRight(string(nameBytes), "\x00 ")
		verts := body[8+mdlNameLen:]

		xyz := make([][3]float32, h.NumVerts)
		nrm := make([][3]float32, h.NumVerts)
		for vi := int32(0); vi < h.NumVerts; vi++ {
			b := verts[vi*4 : vi*4+4]
			xyz[vi] = [3]float32{
				float32(b[0])*h.Scale[0] + h.Origin[0],
				float32(b[1])*h.Scale[1] + h.Origin[1],
				float32(b[2])*h.Scale[2] + h.Origin[2],
			}
			// MDL re-uses the MD2 normal table convention.
			if int(b[3]) < len(md2Normals) {
				nrm[vi] = md2Normals[b[3]]
			}
		}
		frames = append(frames, rawFrame{name: name, xyz: xyz, nrm: nrm})
	}
	if len(frames) == 0 {
		return nil, errors.New("mdl: no simple frames decoded")
	}

	// Expand (vertIdx, onseam, facesfront) into glTF-friendly unique
	// vertices. Quake's seam handling adds 0.5 to S for back-facing
	// triangles that touch onseam verts.
	skinW := float32(h.SkinWidth)
	skinH := float32(h.SkinHeight)
	if skinW == 0 {
		skinW = 1
	}
	if skinH == 0 {
		skinH = 1
	}

	type expKey struct {
		vert    int32
		seamAdj bool // true == use back-of-model UV (S + skinW/2)
	}
	expIdx := make(map[expKey]uint32)
	expand := make([]expKey, 0, len(stEntries))
	rest := frames[0]

	model := &Model{
		Name:      "mdl",
		Triangles: make([]Triangle, 0, len(tris)),
	}
	for _, t := range tris {
		var corners [3]uint32
		for ci := 0; ci < 3; ci++ {
			vi := t.VertIdx[ci]
			seamAdj := t.FacesFront == 0 && stEntries[vi].OnSeam != 0
			k := expKey{vert: vi, seamAdj: seamAdj}
			idx, ok := expIdx[k]
			if !ok {
				idx = uint32(len(expand))
				expIdx[k] = idx
				expand = append(expand, k)
			}
			corners[ci] = idx
		}
		model.Triangles = append(model.Triangles, Triangle{A: corners[0], B: corners[1], C: corners[2]})
	}

	model.Vertices = make([]Vertex, len(expand))
	for i, k := range expand {
		st := stEntries[k.vert]
		s := float32(st.S)
		if k.seamAdj {
			s += skinW / 2
		}
		model.Vertices[i] = Vertex{
			Position: rest.xyz[k.vert],
			Normal:   rest.nrm[k.vert],
			TexCoord: [2]float32{(s + 0.5) / skinW, (float32(st.T) + 0.5) / skinH},
		}
	}

	// Texture — render the paletted skin into a PNG via the Quake 1
	// palette table. This means MDL uploads carry their colour
	// even though the format predates standalone texture files.
	if firstSkinIndices != nil {
		if pngBytes, err := mdlPalettedToPNG(firstSkinIndices, int(h.SkinWidth), int(h.SkinHeight)); err == nil {
			model.Material = Material{
				Name:              "mdl_skin",
				DiffuseImageBytes: pngBytes,
				DiffuseImageMIME:  "image/png",
			}
		}
	}

	// Animation: every frame after rest becomes a morph target.
	if len(frames) > 1 {
		anim := Animation{Name: "default", FPS: 10}
		anim.Frames = make([]AnimationFrame, 0, len(frames)-1)
		for fi := 1; fi < len(frames); fi++ {
			fp := AnimationFrame{
				Positions: make([][3]float32, len(expand)),
				Normals:   make([][3]float32, len(expand)),
			}
			for i, k := range expand {
				fp.Positions[i] = frames[fi].xyz[k.vert]
				fp.Normals[i] = frames[fi].nrm[k.vert]
			}
			anim.Frames = append(anim.Frames, fp)
		}
		model.Animations = []Animation{anim}
	}
	return model, nil
}

// mdlPalettedToPNG converts an indexed-color skin to PNG using the
// Quake 1 palette. The palette is the canonical 256 × RGB table id
// Software shipped in gfx/palette.lmp; we ship it inline so MDL
// decoding doesn't need a runtime asset.
func mdlPalettedToPNG(indices []byte, w, h int) ([]byte, error) {
	if w*h != len(indices) {
		return nil, fmt.Errorf("mdl: skin size %dx%d does not match %d bytes", w, h, len(indices))
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i, idx := range indices {
		r, g, b := quake1Palette[idx*3+0], quake1Palette[idx*3+1], quake1Palette[idx*3+2]
		img.SetRGBA(i%w, i/w, color.RGBA{R: r, G: g, B: b, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// quake1Palette is id Software's original Quake 1 256-colour palette
// (gfx/palette.lmp), 768 bytes = 256 × (R, G, B). Public domain via
// the GPL'd Quake source release.
var quake1Palette = [768]byte{
	0, 0, 0, 15, 15, 15, 31, 31, 31, 47, 47, 47, 63, 63, 63, 75, 75, 75, 91, 91, 91, 107, 107, 107,
	123, 123, 123, 139, 139, 139, 155, 155, 155, 171, 171, 171, 187, 187, 187, 203, 203, 203, 219, 219, 219, 235, 235, 235,
	15, 11, 7, 23, 15, 11, 31, 23, 11, 39, 27, 15, 47, 35, 19, 55, 43, 23, 63, 47, 23, 75, 55, 27,
	83, 59, 27, 91, 67, 31, 99, 75, 31, 107, 83, 31, 115, 87, 31, 123, 95, 35, 131, 103, 35, 143, 111, 35,
	11, 11, 15, 19, 19, 27, 27, 27, 39, 39, 39, 51, 47, 47, 63, 55, 55, 75, 63, 63, 87, 71, 71, 103,
	79, 79, 115, 91, 91, 127, 99, 99, 139, 107, 107, 151, 115, 115, 163, 123, 123, 175, 131, 131, 187, 139, 139, 203,
	0, 0, 0, 7, 7, 0, 11, 11, 0, 19, 19, 0, 27, 27, 0, 35, 35, 0, 43, 43, 7, 47, 47, 7,
	55, 55, 7, 63, 63, 7, 71, 71, 7, 75, 75, 11, 83, 83, 11, 91, 91, 11, 99, 99, 11, 107, 107, 15,
	7, 0, 0, 15, 0, 0, 23, 0, 0, 31, 0, 0, 39, 0, 0, 47, 0, 0, 55, 0, 0, 63, 0, 0,
	71, 0, 0, 79, 0, 0, 87, 0, 0, 95, 0, 0, 103, 0, 0, 111, 0, 0, 119, 0, 0, 127, 0, 0,
	19, 19, 0, 27, 27, 0, 35, 35, 0, 47, 43, 0, 55, 47, 0, 67, 55, 0, 75, 59, 7, 87, 67, 7,
	95, 71, 7, 107, 75, 11, 119, 83, 15, 131, 87, 19, 139, 91, 19, 151, 95, 27, 163, 99, 31, 175, 103, 35,
	35, 19, 7, 47, 23, 11, 59, 31, 15, 75, 35, 19, 87, 43, 23, 99, 47, 31, 115, 55, 35, 127, 59, 43,
	143, 67, 51, 159, 79, 51, 175, 99, 47, 191, 119, 47, 207, 143, 43, 223, 171, 39, 239, 203, 31, 255, 243, 27,
	11, 7, 0, 27, 19, 0, 43, 35, 15, 55, 43, 19, 71, 51, 27, 83, 55, 35, 99, 63, 43, 111, 71, 51,
	127, 83, 63, 139, 95, 71, 155, 107, 83, 167, 123, 95, 183, 135, 107, 195, 147, 123, 211, 163, 139, 227, 179, 151,
	171, 139, 163, 159, 127, 151, 147, 115, 135, 139, 103, 123, 127, 91, 111, 119, 83, 99, 107, 75, 87, 95, 63, 75,
	87, 55, 67, 75, 47, 55, 67, 39, 47, 55, 31, 35, 43, 23, 27, 35, 19, 19, 23, 11, 11, 15, 7, 7,
	187, 115, 159, 175, 107, 143, 163, 95, 131, 151, 87, 119, 139, 79, 107, 127, 75, 95, 115, 67, 83, 107, 59, 75,
	95, 51, 63, 83, 43, 55, 71, 35, 43, 59, 31, 35, 47, 23, 27, 35, 19, 19, 23, 11, 11, 15, 7, 7,
	219, 195, 187, 203, 179, 167, 191, 163, 155, 175, 151, 139, 163, 135, 123, 151, 123, 111, 135, 111, 95, 123, 99, 83,
	107, 87, 71, 95, 75, 59, 83, 63, 51, 67, 51, 39, 55, 43, 31, 39, 31, 23, 27, 19, 15, 15, 11, 7,
	111, 131, 123, 103, 123, 111, 95, 115, 103, 87, 107, 95, 79, 99, 87, 71, 91, 79, 63, 83, 71, 55, 75, 63,
	47, 67, 55, 43, 59, 47, 35, 51, 39, 31, 43, 31, 23, 35, 23, 15, 27, 19, 11, 19, 11, 7, 11, 7,
	255, 243, 27, 239, 223, 23, 219, 203, 19, 203, 183, 15, 187, 167, 15, 171, 151, 11, 155, 131, 7, 139, 115, 7,
	123, 99, 7, 107, 83, 0, 91, 71, 0, 75, 55, 0, 59, 43, 0, 43, 31, 0, 27, 15, 0, 11, 7, 0,
	0, 0, 255, 11, 11, 239, 19, 19, 223, 27, 27, 207, 35, 35, 191, 43, 43, 175, 47, 47, 159, 47, 47, 143,
	47, 47, 127, 47, 47, 111, 47, 47, 95, 43, 43, 79, 35, 35, 63, 27, 27, 47, 19, 19, 31, 11, 11, 15,
	43, 0, 0, 59, 0, 0, 75, 7, 0, 95, 7, 0, 111, 15, 0, 127, 23, 7, 147, 31, 7, 163, 39, 11,
	183, 51, 15, 195, 75, 27, 207, 99, 43, 219, 127, 59, 227, 151, 79, 231, 171, 95, 239, 191, 119, 247, 211, 139,
	167, 123, 59, 183, 155, 55, 199, 195, 55, 231, 227, 87, 127, 191, 255, 171, 231, 255, 215, 255, 255, 103, 0, 0,
	139, 0, 0, 179, 0, 0, 215, 0, 0, 255, 0, 0, 255, 243, 147, 255, 247, 199, 255, 255, 255, 159, 91, 83,
}
