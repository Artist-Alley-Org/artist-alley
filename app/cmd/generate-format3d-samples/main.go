// Generate procedural .md2/.md3/.mdl samples for the artist-alley
// dataset. Run:
//
//   go run ./scripts/generate-format3d-samples \
//       --out /mnt/d/Projects/unraid_management/artist-alley_dataset/format3d
//
// Each shape gets emitted as all three formats so the gallery
// gets variety across the engine pipelines we now own natively.
// Output files are 100% synthetic — no derived id Software /
// OpenArena content, so the dataset stays license-clean.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/preview/format3d"
)

type shape struct {
	name string
	fn   func() *format3d.Model
}

func main() {
	out := flag.String("out", "format3d-samples", "output directory")
	flag.Parse()

	shapes := []shape{
		{"cube", makeCube},
		{"tetrahedron", makeTetrahedron},
		{"octahedron", makeOctahedron},
		{"torus", makeTorus},
		{"wavy-plane", makeWavyPlane},
		{"breathing-sphere", makeBreathingSphere},
		{"spinning-pyramid", makeSpinningPyramid},
	}

	formats := []struct {
		ext     string
		encode  func(*format3d.Model, *os.File) error
	}{
		{"md2", func(m *format3d.Model, f *os.File) error { return format3d.EncodeMD2(m, f) }},
		{"md3", func(m *format3d.Model, f *os.File) error { return format3d.EncodeMD3(m, f) }},
		{"mdl", func(m *format3d.Model, f *os.File) error { return format3d.EncodeMDL(m, f) }},
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		die(err)
	}

	for _, s := range shapes {
		m := s.fn()
		m.Name = s.name
		for _, fmt_ := range formats {
			path := filepath.Join(*out, s.name+"."+fmt_.ext)
			f, err := os.Create(path)
			if err != nil {
				die(err)
			}
			if err := fmt_.encode(m, f); err != nil {
				_ = f.Close()
				_ = os.Remove(path)
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", path, err)
				continue
			}
			info, _ := f.Stat()
			_ = f.Close()
			fmt.Printf("%-32s  %s  %d bytes\n", path, strings.ToUpper(fmt_.ext), info.Size())
		}
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// ---------- shape factories ---------------------------------------------

func makeCube() *format3d.Model {
	v := []format3d.Vertex{
		{Position: [3]float32{-0.5, -0.5, -0.5}, Normal: [3]float32{0, 0, -1}, TexCoord: [2]float32{0, 0}},
		{Position: [3]float32{0.5, -0.5, -0.5}, Normal: [3]float32{0, 0, -1}, TexCoord: [2]float32{1, 0}},
		{Position: [3]float32{0.5, 0.5, -0.5}, Normal: [3]float32{0, 0, -1}, TexCoord: [2]float32{1, 1}},
		{Position: [3]float32{-0.5, 0.5, -0.5}, Normal: [3]float32{0, 0, -1}, TexCoord: [2]float32{0, 1}},
		{Position: [3]float32{-0.5, -0.5, 0.5}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{0, 0}},
		{Position: [3]float32{0.5, -0.5, 0.5}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{1, 0}},
		{Position: [3]float32{0.5, 0.5, 0.5}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{1, 1}},
		{Position: [3]float32{-0.5, 0.5, 0.5}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{0, 1}},
	}
	t := []format3d.Triangle{
		{0, 2, 1}, {0, 3, 2}, // -Z
		{4, 5, 6}, {4, 6, 7}, // +Z
		{0, 1, 5}, {0, 5, 4}, // -Y
		{2, 3, 7}, {2, 7, 6}, // +Y
		{0, 4, 7}, {0, 7, 3}, // -X
		{1, 2, 6}, {1, 6, 5}, // +X
	}
	return &format3d.Model{Vertices: v, Triangles: t}
}

func makeTetrahedron() *format3d.Model {
	v := []format3d.Vertex{
		{Position: [3]float32{0, 1, 0}, Normal: [3]float32{0, 1, 0}, TexCoord: [2]float32{0.5, 1}},
		{Position: [3]float32{-1, -0.5, 1}, Normal: [3]float32{-1, -0.3, 0.8}, TexCoord: [2]float32{0, 0}},
		{Position: [3]float32{1, -0.5, 1}, Normal: [3]float32{1, -0.3, 0.8}, TexCoord: [2]float32{1, 0}},
		{Position: [3]float32{0, -0.5, -1}, Normal: [3]float32{0, -0.3, -1}, TexCoord: [2]float32{0.5, 0.5}},
	}
	t := []format3d.Triangle{
		{0, 2, 1}, {0, 3, 2}, {0, 1, 3}, {1, 2, 3},
	}
	return &format3d.Model{Vertices: v, Triangles: t}
}

func makeOctahedron() *format3d.Model {
	v := []format3d.Vertex{
		{Position: [3]float32{0, 1, 0}, Normal: [3]float32{0, 1, 0}, TexCoord: [2]float32{0.5, 1}},
		{Position: [3]float32{0, -1, 0}, Normal: [3]float32{0, -1, 0}, TexCoord: [2]float32{0.5, 0}},
		{Position: [3]float32{1, 0, 0}, Normal: [3]float32{1, 0, 0}, TexCoord: [2]float32{1, 0.5}},
		{Position: [3]float32{-1, 0, 0}, Normal: [3]float32{-1, 0, 0}, TexCoord: [2]float32{0, 0.5}},
		{Position: [3]float32{0, 0, 1}, Normal: [3]float32{0, 0, 1}, TexCoord: [2]float32{0.75, 0.5}},
		{Position: [3]float32{0, 0, -1}, Normal: [3]float32{0, 0, -1}, TexCoord: [2]float32{0.25, 0.5}},
	}
	t := []format3d.Triangle{
		{0, 4, 2}, {0, 2, 5}, {0, 5, 3}, {0, 3, 4},
		{1, 2, 4}, {1, 5, 2}, {1, 3, 5}, {1, 4, 3},
	}
	return &format3d.Model{Vertices: v, Triangles: t}
}

func makeTorus() *format3d.Model {
	const (
		major, minor = 1.5, 0.5
		nMajor       = 32
		nMinor       = 16
		nFrames      = 24
	)
	verts, tris := torusGeom(major, minor, nMajor, nMinor)
	model := &format3d.Model{Vertices: verts, Triangles: tris}

	// Animation: spin the torus's UV-V offset over time to simulate
	// a "rolling" surface. Easiest to express as a per-frame vertex
	// twist that rotates major-direction.
	frames := make([]format3d.AnimationFrame, 0, nFrames)
	for fi := 0; fi < nFrames; fi++ {
		theta := float64(fi) / float64(nFrames) * 2 * math.Pi
		positions := make([][3]float32, len(verts))
		normals := make([][3]float32, len(verts))
		for i, v := range verts {
			// Rotate around Z by theta.
			cx, cy := math.Cos(theta)*float64(v.Position[0])-math.Sin(theta)*float64(v.Position[1]),
				math.Sin(theta)*float64(v.Position[0])+math.Cos(theta)*float64(v.Position[1])
			positions[i] = [3]float32{float32(cx), float32(cy), v.Position[2]}
			normals[i] = v.Normal
		}
		frames = append(frames, format3d.AnimationFrame{Positions: positions, Normals: normals})
	}
	model.Animations = []format3d.Animation{{Name: "spin", FPS: 12, Frames: frames}}
	return model
}

func torusGeom(major, minor float64, nMajor, nMinor int) ([]format3d.Vertex, []format3d.Triangle) {
	verts := make([]format3d.Vertex, 0, nMajor*nMinor)
	for i := 0; i < nMajor; i++ {
		th := float64(i) / float64(nMajor) * 2 * math.Pi
		ct, st := math.Cos(th), math.Sin(th)
		for j := 0; j < nMinor; j++ {
			ph := float64(j) / float64(nMinor) * 2 * math.Pi
			cp, sp := math.Cos(ph), math.Sin(ph)
			r := major + minor*cp
			pos := [3]float32{float32(r * ct), float32(r * st), float32(minor * sp)}
			// normal = direction from ring centre.
			rc := [3]float32{float32(major * ct), float32(major * st), 0}
			n := [3]float32{pos[0] - rc[0], pos[1] - rc[1], pos[2] - rc[2]}
			n = normalize(n)
			verts = append(verts, format3d.Vertex{
				Position: pos, Normal: n,
				TexCoord: [2]float32{float32(i) / float32(nMajor), float32(j) / float32(nMinor)},
			})
		}
	}
	tris := make([]format3d.Triangle, 0, nMajor*nMinor*2)
	for i := 0; i < nMajor; i++ {
		i2 := (i + 1) % nMajor
		for j := 0; j < nMinor; j++ {
			j2 := (j + 1) % nMinor
			a := uint32(i*nMinor + j)
			b := uint32(i2*nMinor + j)
			c := uint32(i2*nMinor + j2)
			d := uint32(i*nMinor + j2)
			tris = append(tris, format3d.Triangle{A: a, B: b, C: c}, format3d.Triangle{A: a, B: c, C: d})
		}
	}
	return verts, tris
}

func makeWavyPlane() *format3d.Model {
	const (
		size = 16
		span = 4.0
	)
	verts := make([]format3d.Vertex, 0, size*size)
	for j := 0; j < size; j++ {
		for i := 0; i < size; i++ {
			x := -span/2 + span*float64(i)/float64(size-1)
			y := -span/2 + span*float64(j)/float64(size-1)
			verts = append(verts, format3d.Vertex{
				Position: [3]float32{float32(x), float32(y), 0},
				Normal:   [3]float32{0, 0, 1},
				TexCoord: [2]float32{float32(i) / float32(size-1), float32(j) / float32(size-1)},
			})
		}
	}
	tris := make([]format3d.Triangle, 0, (size-1)*(size-1)*2)
	for j := 0; j < size-1; j++ {
		for i := 0; i < size-1; i++ {
			a := uint32(j*size + i)
			b := uint32(j*size + i + 1)
			c := uint32((j+1)*size + i + 1)
			d := uint32((j+1)*size + i)
			tris = append(tris, format3d.Triangle{A: a, B: b, C: c}, format3d.Triangle{A: a, B: c, C: d})
		}
	}

	frames := make([]format3d.AnimationFrame, 0, 32)
	for fi := 0; fi < 32; fi++ {
		t := float64(fi) / 32 * 2 * math.Pi
		positions := make([][3]float32, len(verts))
		normals := make([][3]float32, len(verts))
		for k, v := range verts {
			x, y := float64(v.Position[0]), float64(v.Position[1])
			z := 0.25 * math.Sin(2*x+t) * math.Cos(2*y+t)
			positions[k] = [3]float32{v.Position[0], v.Position[1], float32(z)}
			normals[k] = [3]float32{0, 0, 1}
		}
		frames = append(frames, format3d.AnimationFrame{Positions: positions, Normals: normals})
	}
	return &format3d.Model{
		Vertices: verts, Triangles: tris,
		Animations: []format3d.Animation{{Name: "ripple", FPS: 16, Frames: frames}},
	}
}

func makeBreathingSphere() *format3d.Model {
	verts, tris := sphereGeom(1.0, 20, 16)
	frames := make([]format3d.AnimationFrame, 0, 24)
	for fi := 0; fi < 24; fi++ {
		t := float64(fi) / 24 * 2 * math.Pi
		scale := 1.0 + 0.15*math.Sin(t)
		positions := make([][3]float32, len(verts))
		normals := make([][3]float32, len(verts))
		for i, v := range verts {
			positions[i] = [3]float32{
				v.Position[0] * float32(scale),
				v.Position[1] * float32(scale),
				v.Position[2] * float32(scale),
			}
			normals[i] = v.Normal
		}
		frames = append(frames, format3d.AnimationFrame{Positions: positions, Normals: normals})
	}
	return &format3d.Model{
		Vertices: verts, Triangles: tris,
		Animations: []format3d.Animation{{Name: "breathe", FPS: 10, Frames: frames}},
	}
}

func makeSpinningPyramid() *format3d.Model {
	base := []format3d.Vertex{
		{Position: [3]float32{0, 1.5, 0}, Normal: [3]float32{0, 1, 0}, TexCoord: [2]float32{0.5, 1}},
		{Position: [3]float32{-1, -0.5, -1}, Normal: [3]float32{-0.6, -0.3, -0.6}, TexCoord: [2]float32{0, 0}},
		{Position: [3]float32{1, -0.5, -1}, Normal: [3]float32{0.6, -0.3, -0.6}, TexCoord: [2]float32{1, 0}},
		{Position: [3]float32{1, -0.5, 1}, Normal: [3]float32{0.6, -0.3, 0.6}, TexCoord: [2]float32{1, 1}},
		{Position: [3]float32{-1, -0.5, 1}, Normal: [3]float32{-0.6, -0.3, 0.6}, TexCoord: [2]float32{0, 1}},
	}
	tris := []format3d.Triangle{
		{0, 2, 1}, {0, 3, 2}, {0, 4, 3}, {0, 1, 4},
		{1, 2, 3}, {1, 3, 4},
	}
	frames := make([]format3d.AnimationFrame, 0, 16)
	for fi := 0; fi < 16; fi++ {
		theta := float64(fi) / 16 * 2 * math.Pi
		positions := make([][3]float32, len(base))
		normals := make([][3]float32, len(base))
		for i, v := range base {
			cx := math.Cos(theta)*float64(v.Position[0]) - math.Sin(theta)*float64(v.Position[2])
			cz := math.Sin(theta)*float64(v.Position[0]) + math.Cos(theta)*float64(v.Position[2])
			positions[i] = [3]float32{float32(cx), v.Position[1], float32(cz)}
			normals[i] = v.Normal
		}
		frames = append(frames, format3d.AnimationFrame{Positions: positions, Normals: normals})
	}
	return &format3d.Model{
		Vertices: base, Triangles: tris,
		Animations: []format3d.Animation{{Name: "spin", FPS: 12, Frames: frames}},
	}
}

func sphereGeom(radius float64, slices, stacks int) ([]format3d.Vertex, []format3d.Triangle) {
	verts := make([]format3d.Vertex, 0, (slices+1)*(stacks+1))
	for j := 0; j <= stacks; j++ {
		ph := math.Pi*float64(j)/float64(stacks) - math.Pi/2
		cp, sp := math.Cos(ph), math.Sin(ph)
		for i := 0; i <= slices; i++ {
			th := 2 * math.Pi * float64(i) / float64(slices)
			ct, st := math.Cos(th), math.Sin(th)
			pos := [3]float32{
				float32(radius * cp * ct),
				float32(radius * cp * st),
				float32(radius * sp),
			}
			n := normalize(pos)
			verts = append(verts, format3d.Vertex{
				Position: pos, Normal: n,
				TexCoord: [2]float32{float32(i) / float32(slices), float32(j) / float32(stacks)},
			})
		}
	}
	tris := make([]format3d.Triangle, 0, slices*stacks*2)
	for j := 0; j < stacks; j++ {
		for i := 0; i < slices; i++ {
			a := uint32(j*(slices+1) + i)
			b := a + 1
			c := uint32((j+1)*(slices+1) + i + 1)
			d := uint32((j+1)*(slices+1) + i)
			tris = append(tris, format3d.Triangle{A: a, B: b, C: c}, format3d.Triangle{A: a, B: c, C: d})
		}
	}
	return verts, tris
}

func normalize(v [3]float32) [3]float32 {
	l := float32(math.Sqrt(float64(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])))
	if l == 0 {
		return v
	}
	return [3]float32{v[0] / l, v[1] / l, v[2] / l}
}
