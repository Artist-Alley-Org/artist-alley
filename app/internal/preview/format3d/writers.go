package format3d

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// MD2 / MD3 / MDL writers — emit minimal-but-valid files our own
// Decode* funcs round-trip cleanly. Used for:
//   1. round-trip tests in this package (write → decode → compare)
//   2. scripts/generate-format3d-samples to produce demo content for
//      the artist-alley dataset seed
//
// Not aimed at id-tools-faithful output. We omit niceties like
// MD2 GL commands, MDL group skins, MD3 tags. The decoders ignore
// those too, so leaving them out keeps both sides honest.

// ---------- MD2 ----------------------------------------------------------

// EncodeMD2 writes a Model as a Quake II .md2 stream.
//
// Limitations:
//   - 1 skin slot; skin name = Material.Name (truncated to 64 bytes)
//   - No GL command list (NumGLCmds = 0)
//   - Frame 0 = the rest pose vertices; each Animation Frame becomes
//     a subsequent MD2 frame, with names derived from anim.Name +
//     index
//   - Vertex positions get quantised to MD2's 0..255 + scale/translate
//     encoding. Lossy by spec (~0.4% per-axis on the model's bbox).
func EncodeMD2(m *Model, w io.Writer) error {
	if m == nil || len(m.Vertices) == 0 || len(m.Triangles) == 0 {
		return errors.New("md2: empty model")
	}
	if len(m.Vertices) > 65535 || len(m.Triangles) > 65535 {
		return fmt.Errorf("md2: too many verts (%d) or tris (%d) for u16 indexing",
			len(m.Vertices), len(m.Triangles))
	}

	// MD2 spec wants a separate ST table indexed independently from
	// vertex indices. We've already flattened both into "one vertex
	// per unique (pos, uv)" — so st_index == vert_index always. ST
	// table just mirrors the vertex array.
	numXYZ := uint32(len(m.Vertices))
	numST := uint32(len(m.Vertices))
	numTris := uint32(len(m.Triangles))

	// Sweep through all frames (rest + each animation) to find one
	// global bbox; MD2 stores per-frame scale/translate so the same
	// vertex's quantised bytes change per-frame, but using a global
	// bbox keeps the writer trivial and gives reasonable precision.
	allFrames := collectAllFrames(m)
	numFrames := uint32(len(allFrames))

	skinName := truncateName(m.Material.Name, 63)
	if skinName == "" {
		skinName = "default"
	}
	skinBytes := zeroPad([]byte(skinName), 64)

	frameStride := uint32(6*4 + md2FrameNameLen + numXYZ*4)
	hdrSize := uint32(binary.Size(md2Header{}))

	offSkins := hdrSize
	offST := offSkins + 64 // 1 skin × 64 bytes
	offTris := offST + numST*4
	offFrames := offTris + numTris*12
	offEnd := offFrames + numFrames*frameStride

	h := md2Header{
		Magic: md2Magic, Version: md2Version,
		SkinWidth: 256, SkinHeight: 256,
		FrameSize:    frameStride,
		NumSkins:     1,
		NumXYZ:       numXYZ,
		NumST:        numST,
		NumTris:      numTris,
		NumGLCmds:    0,
		NumFrames:    numFrames,
		OffsetSkins:  offSkins,
		OffsetST:     offST,
		OffsetTris:   offTris,
		OffsetFrames: offFrames,
		OffsetGLCmds: offFrames + numFrames*frameStride,
		OffsetEnd:    offEnd,
	}

	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, h); err != nil {
		return fmt.Errorf("md2: header write: %w", err)
	}
	buf.Write(skinBytes)

	// ST table — quantise each UV to the 256×256 skin space.
	for _, v := range m.Vertices {
		s := int16(clamp(v.TexCoord[0]*256, 0, 65535))
		t := int16(clamp(v.TexCoord[1]*256, 0, 65535))
		_ = binary.Write(&buf, binary.LittleEndian, s)
		_ = binary.Write(&buf, binary.LittleEndian, t)
	}

	// Triangle table — ST indices equal vertex indices since we
	// emitted one ST per vertex.
	for _, tr := range m.Triangles {
		_ = binary.Write(&buf, binary.LittleEndian, [3]uint16{uint16(tr.A), uint16(tr.B), uint16(tr.C)})
		_ = binary.Write(&buf, binary.LittleEndian, [3]uint16{uint16(tr.A), uint16(tr.B), uint16(tr.C)})
	}

	// Frame table.
	for fi, frame := range allFrames {
		minP, maxP := boundsOf(frame.positions)
		scale := [3]float32{
			(maxP[0] - minP[0]) / 255,
			(maxP[1] - minP[1]) / 255,
			(maxP[2] - minP[2]) / 255,
		}
		for i := 0; i < 3; i++ {
			if scale[i] == 0 {
				scale[i] = 1
			}
		}
		translate := minP
		_ = binary.Write(&buf, binary.LittleEndian, scale)
		_ = binary.Write(&buf, binary.LittleEndian, translate)
		buf.Write(zeroPad([]byte(framenameFor(m, fi)), md2FrameNameLen))
		for vi, p := range frame.positions {
			x := byte(clamp((p[0]-translate[0])/scale[0], 0, 255))
			y := byte(clamp((p[1]-translate[1])/scale[1], 0, 255))
			z := byte(clamp((p[2]-translate[2])/scale[2], 0, 255))
			n := nearestMD2Normal(frame.normals[vi])
			buf.Write([]byte{x, y, z, n})
		}
	}

	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("md2: stream write: %w", err)
	}
	return nil
}

// ---------- MDL ----------------------------------------------------------

// EncodeMDL writes a Quake I .mdl. Always emits a 4×4 single-skin
// (Quake's smallest legal value), all indices facing front (no
// onseam adjustments). Frame and palette decisions follow the
// decoder's expectations.
func EncodeMDL(m *Model, w io.Writer) error {
	if m == nil || len(m.Vertices) == 0 || len(m.Triangles) == 0 {
		return errors.New("mdl: empty model")
	}
	if len(m.Vertices) > 32767 || len(m.Triangles) > 32767 {
		return errors.New("mdl: too many verts/tris for i32 fields (we use i32 but Quake clamps low)")
	}
	allFrames := collectAllFrames(m)
	numFrames := int32(len(allFrames))

	minP, maxP := boundsOf(unionFramesXYZ(allFrames))
	scale := [3]float32{
		(maxP[0] - minP[0]) / 255,
		(maxP[1] - minP[1]) / 255,
		(maxP[2] - minP[2]) / 255,
	}
	for i := 0; i < 3; i++ {
		if scale[i] == 0 {
			scale[i] = 1
		}
	}
	origin := minP

	h := mdlHeader{
		Magic: mdlMagic, Version: mdlVersion,
		Scale: scale, Origin: origin,
		BoundingRad: 1,
		EyePosition: [3]float32{},
		NumSkins:    1,
		SkinWidth:   4, SkinHeight: 4,
		NumVerts:  int32(len(m.Vertices)),
		NumTris:   int32(len(m.Triangles)),
		NumFrames: numFrames,
		SyncType:  0,
		Flags:     0,
		Size:      1,
	}

	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, h); err != nil {
		return fmt.Errorf("mdl: header write: %w", err)
	}

	// One simple skin — palette index 15 (the bright neutral grey
	// at the top of Quake's palette ramp), 4×4 = 16 bytes.
	_ = binary.Write(&buf, binary.LittleEndian, int32(0)) // group=0
	for i := 0; i < 16; i++ {
		buf.WriteByte(15)
	}

	// ST table — onseam=0, S/T in skin coords.
	for _, v := range m.Vertices {
		s := int32(clamp(v.TexCoord[0]*4-0.5, 0, 4))
		t := int32(clamp(v.TexCoord[1]*4-0.5, 0, 4))
		_ = binary.Write(&buf, binary.LittleEndian, int32(0))
		_ = binary.Write(&buf, binary.LittleEndian, s)
		_ = binary.Write(&buf, binary.LittleEndian, t)
	}

	// Triangles — facesfront=1, three indices.
	for _, tr := range m.Triangles {
		_ = binary.Write(&buf, binary.LittleEndian, int32(1))
		_ = binary.Write(&buf, binary.LittleEndian, [3]int32{int32(tr.A), int32(tr.B), int32(tr.C)})
	}

	// Frames — all simple (type=0).
	for fi, frame := range allFrames {
		_ = binary.Write(&buf, binary.LittleEndian, int32(0)) // frame type
		// bbox min / max as packed verts (placeholder zero is fine
		// for thumbnails; the renderer ignores these).
		buf.Write([]byte{0, 0, 0, 0})
		buf.Write([]byte{255, 255, 255, 0})
		buf.Write(zeroPad([]byte(framenameFor(m, fi)), mdlNameLen))
		for vi, p := range frame.positions {
			x := byte(clamp((p[0]-origin[0])/scale[0], 0, 255))
			y := byte(clamp((p[1]-origin[1])/scale[1], 0, 255))
			z := byte(clamp((p[2]-origin[2])/scale[2], 0, 255))
			n := nearestMD2Normal(frame.normals[vi])
			buf.Write([]byte{x, y, z, n})
		}
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("mdl: stream write: %w", err)
	}
	return nil
}

// ---------- MD3 ----------------------------------------------------------

// EncodeMD3 writes a Quake III .md3 — one surface, no tags, no
// shaders bound. Vertex positions go through the 1/64 fixed-point
// encoding the decoder reverses.
func EncodeMD3(m *Model, w io.Writer) error {
	if m == nil || len(m.Vertices) == 0 || len(m.Triangles) == 0 {
		return errors.New("md3: empty model")
	}
	if len(m.Vertices) > 32767 {
		return errors.New("md3: too many verts for i16 position encoding")
	}
	allFrames := collectAllFrames(m)
	numFrames := int32(len(allFrames))
	numVerts := int32(len(m.Vertices))
	numTris := int32(len(m.Triangles))

	headerSize := int32(binary.Size(md3Header{}))
	frameSize := int32(binary.Size(md3Frame{})) // 56 bytes
	surfaceHeaderSize := int32(binary.Size(md3SurfaceHeader{}))

	// Layout: header → frames → tags(0) → surface{header → tris → ST
	// → xyz/normal stream}.
	offFrames := headerSize
	offTags := offFrames + numFrames*frameSize
	offSurfaces := offTags

	// Surface internal offsets (relative to surface start).
	surfTriOff := surfaceHeaderSize
	surfShaderOff := surfTriOff + numTris*12
	surfSTOff := surfShaderOff // we ship 0 shaders → bytes coincide
	surfXYZOff := surfSTOff + numVerts*8
	surfEndOff := surfXYZOff + numFrames*numVerts*8

	totalEnd := offSurfaces + surfEndOff

	var buf bytes.Buffer

	hdr := md3Header{
		Magic: md3Magic, Version: md3Version,
		Flags:          0,
		NumFrames:      numFrames,
		NumTags:        0,
		NumSurfaces:    1,
		NumSkins:       0,
		OffsetFrames:   offFrames,
		OffsetTags:     offTags,
		OffsetSurfaces: offSurfaces,
		OffsetEnd:      totalEnd,
	}
	copy(hdr.Name[:], truncateName(m.Name, 63))
	_ = binary.Write(&buf, binary.LittleEndian, hdr)

	// Frame metadata. Bounds are decorative for thumbnails; we set
	// them but the decoder only uses xyz from the surface stream.
	for _, frame := range allFrames {
		minP, maxP := boundsOf(frame.positions)
		fr := md3Frame{
			MinBounds: minP, MaxBounds: maxP, LocalOrig: [3]float32{}, Radius: 1,
		}
		_ = binary.Write(&buf, binary.LittleEndian, fr)
	}

	// (no tags)

	// Surface.
	sh := md3SurfaceHeader{
		Magic:          md3SurfaceMagic,
		Flags:          0,
		NumFrames:      numFrames,
		NumShaders:     0,
		NumVerts:       numVerts,
		NumTriangles:   numTris,
		OffsetTriangles: surfTriOff,
		OffsetShaders:  surfShaderOff,
		OffsetST:       surfSTOff,
		OffsetXYZNormal: surfXYZOff,
		OffsetEnd:      surfEndOff,
	}
	copy(sh.Name[:], truncateName(m.Name+"_surf0", 63))
	_ = binary.Write(&buf, binary.LittleEndian, sh)

	for _, tr := range m.Triangles {
		_ = binary.Write(&buf, binary.LittleEndian, [3]int32{int32(tr.A), int32(tr.B), int32(tr.C)})
	}
	for _, v := range m.Vertices {
		_ = binary.Write(&buf, binary.LittleEndian, v.TexCoord)
	}
	for _, frame := range allFrames {
		for vi, p := range frame.positions {
			x := int16(clamp(p[0]/md3XyzScale, -32767, 32767))
			y := int16(clamp(p[1]/md3XyzScale, -32767, 32767))
			z := int16(clamp(p[2]/md3XyzScale, -32767, 32767))
			lat, lng := md3PackNormal(frame.normals[vi])
			_ = binary.Write(&buf, binary.LittleEndian, x)
			_ = binary.Write(&buf, binary.LittleEndian, y)
			_ = binary.Write(&buf, binary.LittleEndian, z)
			buf.WriteByte(lat)
			buf.WriteByte(lng)
		}
	}

	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("md3: stream write: %w", err)
	}
	return nil
}

// ---------- helpers ------------------------------------------------------

type framePose struct {
	positions [][3]float32
	normals   [][3]float32
}

// collectAllFrames returns the rest pose followed by every
// animation frame, in playback order. Padding fills missing normals
// with the rest-pose normals so the writer always has full vec
// triples.
func collectAllFrames(m *Model) []framePose {
	out := make([]framePose, 0, 1+totalAnimationFrames(m.Animations))
	rest := framePose{
		positions: make([][3]float32, len(m.Vertices)),
		normals:   make([][3]float32, len(m.Vertices)),
	}
	for i, v := range m.Vertices {
		rest.positions[i] = v.Position
		rest.normals[i] = v.Normal
	}
	out = append(out, rest)
	for _, a := range m.Animations {
		for _, f := range a.Frames {
			pose := framePose{positions: f.Positions}
			if len(f.Normals) == len(m.Vertices) {
				pose.normals = f.Normals
			} else {
				pose.normals = rest.normals
			}
			if len(pose.positions) < len(m.Vertices) {
				padded := make([][3]float32, len(m.Vertices))
				copy(padded, pose.positions)
				for i := len(pose.positions); i < len(m.Vertices); i++ {
					padded[i] = m.Vertices[i].Position
				}
				pose.positions = padded
			}
			out = append(out, pose)
		}
	}
	return out
}

func unionFramesXYZ(frames []framePose) [][3]float32 {
	out := make([][3]float32, 0, len(frames)*8)
	for _, f := range frames {
		out = append(out, f.positions...)
	}
	return out
}

func boundsOf(pts [][3]float32) (mn, mx [3]float32) {
	if len(pts) == 0 {
		return
	}
	mn, mx = pts[0], pts[0]
	for _, p := range pts[1:] {
		for i := 0; i < 3; i++ {
			if p[i] < mn[i] {
				mn[i] = p[i]
			}
			if p[i] > mx[i] {
				mx[i] = p[i]
			}
		}
	}
	return
}

func clamp(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func zeroPad(b []byte, size int) []byte {
	if len(b) >= size {
		return b[:size]
	}
	out := make([]byte, size)
	copy(out, b)
	return out
}

func truncateName(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

func framenameFor(m *Model, idx int) string {
	if idx == 0 {
		return "rest"
	}
	if len(m.Animations) == 0 || idx-1 >= len(m.Animations[0].Frames) {
		return fmt.Sprintf("frame%03d", idx)
	}
	return fmt.Sprintf("%s_%03d", m.Animations[0].Name, idx-1)
}

// nearestMD2Normal finds the closest precomputed normal table entry
// for an arbitrary unit vector. Brute O(162) — fine for one-time
// encoding, not for inner loops.
func nearestMD2Normal(n [3]float32) byte {
	best := byte(0)
	bestDot := float32(-2)
	for i, tab := range md2Normals {
		d := tab[0]*n[0] + tab[1]*n[1] + tab[2]*n[2]
		if d > bestDot {
			bestDot = d
			best = byte(i)
		}
	}
	return best
}

// md3PackNormal is the inverse of md3UnpackNormal — encodes a unit
// vector as the MD3 (lat, lng) byte pair.
func md3PackNormal(n [3]float32) (lat, lng byte) {
	// lat = acos(z), lng = atan2(y, x). Both wrap into [0, 2π].
	// We let unpackNormal's inverse drive the bit math here.
	zClamped := clamp(n[2], -1, 1)
	latF := acosApprox(zClamped)
	lngF := atan2Approx(n[1], n[0])
	lat = byte(int(latF*(255/(2*piApprox))) & 0xff)
	lng = byte(int(lngF*(255/(2*piApprox))) & 0xff)
	return
}

func acosApprox(x float32) float32 { return float32(math.Acos(float64(x))) }

// atan2Approx maps the math.Atan2 result into [0, 2π) so the byte
// quantisation below is a straight modulo, not a signed wraparound.
func atan2Approx(y, x float32) float32 {
	return float32(math.Atan2(float64(y), float64(x)) + 2*piApprox)
}

const piApprox = math.Pi
