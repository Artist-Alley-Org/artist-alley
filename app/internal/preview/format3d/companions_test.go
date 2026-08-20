// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package format3d

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseGLTFCompanions(t *testing.T) {
	tests := []struct {
		name string
		json string
		want []string
	}{
		{
			name: "buffer + images in order, deduped",
			json: `{
				"buffers":[{"uri":"model.bin"},{"uri":"model.bin"}],
				"images":[{"uri":"base.png"},{"uri":"normal.png"}]
			}`,
			want: []string{"model.bin", "base.png", "normal.png"},
		},
		{
			name: "skips embedded data URIs and self-contained GLB shape",
			json: `{"buffers":[{"uri":"data:application/octet-stream;base64,AAAA"}]}`,
			want: nil,
		},
		{
			name: "skips remote references",
			json: `{"images":[{"uri":"https://cdn.example.com/tex.png"}]}`,
			want: nil,
		},
		{
			name: "percent-decodes URIs to match disk names",
			json: `{"images":[{"uri":"Skin%20Base%20Color.png"}]}`,
			want: []string{"Skin Base Color.png"},
		},
		{
			name: "rejects path traversal",
			json: `{"buffers":[{"uri":"../../etc/passwd"}],"images":[{"uri":"/abs.png"}]}`,
			want: nil,
		},
		{
			name: "preserves subdirectories",
			json: `{"images":[{"uri":"textures/wood_diffuse.png"}]}`,
			want: []string{"textures/wood_diffuse.png"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseGLTFCompanions([]byte(tc.json))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseGLTFCompanions_BadJSON(t *testing.T) {
	if _, err := ParseGLTFCompanions([]byte("not json")); err == nil {
		t.Fatal("expected error for malformed glTF JSON")
	}
}

func TestParseOBJMaterialLibs(t *testing.T) {
	obj := "# comment\n" +
		"mtllib model.mtl extra.mtl\n" +
		"v 0 0 0\n" +
		"mtllib model.mtl\n" // duplicate collapses
	got := ParseOBJMaterialLibs([]byte(obj))
	want := []string{"model.mtl", "extra.mtl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseMTLTextures(t *testing.T) {
	mtl := "newmtl body\n" +
		"map_Kd body_diffuse.png\n" +
		"map_Bump -bm 0.2 body_normal.png\n" + // option flags before the path
		"bump body_normal.png\n" + // duplicate collapses
		"map_Ks textures/spec.png\n" +
		"# map_Kd commented.png\n"
	got := ParseMTLTextures([]byte(mtl))
	want := []string{"body_diffuse.png", "body_normal.png", "textures/spec.png"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolveCompanions_GLTF(t *testing.T) {
	dir := t.TempDir()
	gltf := `{
		"buffers":[{"uri":"model.bin"}],
		"images":[{"uri":"base.png"},{"uri":"missing.png"}]
	}`
	writeFile(t, filepath.Join(dir, "model.gltf"), gltf)
	writeFile(t, filepath.Join(dir, "model.bin"), "binary")
	writeFile(t, filepath.Join(dir, "base.png"), "png")
	// missing.png intentionally absent

	found, missing, err := ResolveCompanions(filepath.Join(dir, "model.gltf"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(found, []string{"model.bin", "base.png"}) {
		t.Fatalf("found = %v", found)
	}
	if !reflect.DeepEqual(missing, []string{"missing.png"}) {
		t.Fatalf("missing = %v", missing)
	}
}

func TestResolveCompanions_OBJChainsToTextures(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "m.obj"), "mtllib m.mtl\nv 0 0 0\n")
	writeFile(t, filepath.Join(dir, "m.mtl"), "newmtl a\nmap_Kd diffuse.png\n")
	writeFile(t, filepath.Join(dir, "diffuse.png"), "png")

	found, _, err := ResolveCompanions(filepath.Join(dir, "m.obj"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(found, []string{"m.mtl", "diffuse.png"}) {
		t.Fatalf("found = %v", found)
	}
}

// TestResolveCompanions_GLBExternalURIs is the direction the old
// `GLB is self-contained` assumption denied existed (#750): a GLB whose
// images[] name sibling files, exactly as the seed catalogue's 363
// Kenney GLBs do.
func TestResolveCompanions_GLBExternalURIs(t *testing.T) {
	dir := t.TempDir()
	doc := `{
		"asset":{"version":"2.0"},
		"buffers":[{"byteLength":4}],
		"images":[
			{"uri":"Textures/planks.png"},
			{"uri":"Textures/cobblestone.png"},
			{"uri":"Textures/absent.png"}
		]
	}`
	writeGLB(t, filepath.Join(dir, "structure-wall.glb"), doc, []byte("bin\x00"))
	if err := os.MkdirAll(filepath.Join(dir, "Textures"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "Textures", "planks.png"), "png")
	writeFile(t, filepath.Join(dir, "Textures", "cobblestone.png"), "png")
	// Textures/absent.png intentionally not written.

	found, missing, err := ResolveCompanions(filepath.Join(dir, "structure-wall.glb"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"Textures/planks.png", "Textures/cobblestone.png"}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("found = %v, want %v", found, want)
	}
	if !reflect.DeepEqual(missing, []string{"Textures/absent.png"}) {
		t.Fatalf("missing = %v", missing)
	}
}

// TestResolveCompanions_GLBEmbedded is the other direction, and the
// reason the fix is a parse rather than a new blanket rule: a GLB that
// really does carry everything — GLB-chunk buffer, data: URI image —
// must still resolve to nothing.
func TestResolveCompanions_GLBEmbedded(t *testing.T) {
	dir := t.TempDir()
	doc := `{
		"asset":{"version":"2.0"},
		"buffers":[{"byteLength":4}],
		"images":[{"uri":"data:image/png;base64,iVBORw0KGgo="},{"bufferView":0}]
	}`
	writeGLB(t, filepath.Join(dir, "embedded.glb"), doc, []byte("bin\x00"))
	// A sibling that exists but is NOT declared must not be picked up.
	writeFile(t, filepath.Join(dir, "stray.png"), "png")

	found, missing, err := ResolveCompanions(filepath.Join(dir, "embedded.glb"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found != nil || missing != nil {
		t.Fatalf("embedded GLB should declare no companions: found=%v missing=%v", found, missing)
	}
}

// TestResolveCompanions_GLBFromWriteGLB runs the check against bytes this
// package itself produced. WriteGLB embeds everything, so the honest
// answer is no companions — proven on a real container rather than a
// hand-built one.
func TestResolveCompanions_GLBFromWriteGLB(t *testing.T) {
	dir := t.TempDir()
	blob, err := WriteGLBBytes(fixtureCube())
	if err != nil {
		t.Fatalf("WriteGLBBytes: %v", err)
	}
	path := filepath.Join(dir, "cube.glb")
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	found, missing, err := ResolveCompanions(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found != nil || missing != nil {
		t.Fatalf("WriteGLB output should be self-contained: found=%v missing=%v", found, missing)
	}
}

// A file that isn't a GLB but claims the extension resolves to no
// companions and reports why. The seed/ingest caller logs that and keeps
// going — the soft-fail contract — rather than failing the run.
func TestResolveCompanions_GLBMalformed(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"bad-magic.glb": "not a glb at all, just text",
		// Right magic, truncated before the chunk header — the shape the
		// old test fixture accidentally had.
		"truncated.glb": "glTF binary",
	} {
		writeFile(t, filepath.Join(dir, name), content)
		found, missing, err := ResolveCompanions(filepath.Join(dir, name))
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if found != nil || missing != nil {
			t.Fatalf("%s: found=%v missing=%v, want none", name, found, missing)
		}
	}
}

// Extensions with no reference reader resolve to nil. This pins current
// behaviour; it is NOT a claim that these formats embed everything. DAE
// in particular declares its images in <library_images> and is not read
// yet — see the resolver's header comment. FBX used to be in this list
// and is now parsed (#753, fbx_test.go).
func TestResolveCompanions_NoReaderForFormat(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"model.stl", "model.ply", "model.dae", "notes.txt"} {
		writeFile(t, filepath.Join(dir, name), "whatever")
		found, missing, err := ResolveCompanions(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if found != nil || missing != nil {
			t.Fatalf("%s: found=%v missing=%v, want none", name, found, missing)
		}
	}
}

func TestReadGLBJSONChunk(t *testing.T) {
	doc := `{"asset":{"version":"2.0"},"images":[{"uri":"a.png"}]}`
	var buf bytes.Buffer
	appendGLB(t, &buf, doc, []byte("binchunk"))

	got, err := ReadGLBJSONChunk(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != doc {
		t.Fatalf("got %q, want %q", got, doc)
	}

	// The BIN chunk must be left unread: the reader stops after JSON.
	if buf.Len() == 0 {
		t.Fatal("BIN chunk was consumed; the reader should stop after the JSON chunk")
	}
}

func TestReadGLBJSONChunk_Rejects(t *testing.T) {
	cases := map[string][]byte{
		"empty":      {},
		"short":      []byte("glTF"),
		"bad magic":  bytes.Repeat([]byte{0x01}, 32),
		"no json":    glbWithChunkType(t, 0x004E4942), // BIN first
		"short json": append(glbHeaderBytes(t, 20, 999), 'x'),
	}
	for name, blob := range cases {
		if _, err := ReadGLBJSONChunk(bytes.NewReader(blob)); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

// writeGLB assembles a spec-shaped GLB — 12-byte header, JSON chunk,
// BIN chunk — around `doc` and writes it to path. Padding is the
// spec's: spaces after JSON, zeros after BIN.
func writeGLB(t *testing.T, path, doc string, bin []byte) {
	t.Helper()
	var buf bytes.Buffer
	appendGLB(t, &buf, doc, bin)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendGLB(t *testing.T, w *bytes.Buffer, doc string, bin []byte) {
	t.Helper()
	jsonPad := make([]byte, (4-len(doc)%4)%4)
	for i := range jsonPad {
		jsonPad[i] = ' '
	}
	binPad := make([]byte, (4-len(bin)%4)%4)
	jsonLen := len(doc) + len(jsonPad)
	binLen := len(bin) + len(binPad)
	total := 12 + 8 + jsonLen + 8 + binLen

	put32 := func(v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		w.Write(b[:])
	}
	put32(0x46546C67) // 'glTF'
	put32(2)
	put32(uint32(total))
	put32(uint32(jsonLen))
	put32(0x4E4F534A) // 'JSON'
	w.WriteString(doc)
	w.Write(jsonPad)
	put32(uint32(binLen))
	put32(0x004E4942) // 'BIN\0'
	w.Write(bin)
	w.Write(binPad)
}

// glbHeaderBytes builds just the 12-byte header plus a chunk header
// claiming `chunkLen` bytes of JSON, so callers can truncate after it.
func glbHeaderBytes(t *testing.T, total, chunkLen uint32) []byte {
	t.Helper()
	b := make([]byte, 0, 20)
	for _, v := range []uint32{0x46546C67, 2, total, chunkLen, 0x4E4F534A} {
		var x [4]byte
		binary.LittleEndian.PutUint32(x[:], v)
		b = append(b, x[:]...)
	}
	return b
}

func glbWithChunkType(t *testing.T, chunkType uint32) []byte {
	t.Helper()
	b := make([]byte, 0, 24)
	for _, v := range []uint32{0x46546C67, 2, 24, 4, chunkType} {
		var x [4]byte
		binary.LittleEndian.PutUint32(x[:], v)
		b = append(b, x[:]...)
	}
	return append(b, 0, 0, 0, 0)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// #754 — the declaration half, separated
// ---------------------------------------------------------------------------

// TestCompanionSupportFor is the extension TABLE, asserted directly.
//
// It exists because three copies of this list used to be spelled out
// across the tree and they had already drifted: seed's
// applyAssetCompanions pre-filtered on `case "gltf", "obj"` and cost 363
// seeded GLBs their companion rows (#750). Now there is one table and
// this is the test that guards its contents.
func TestCompanionSupportFor(t *testing.T) {
	for _, c := range []struct {
		ext  string
		want CompanionSupport
	}{
		{"gltf", CompanionComplete},
		{"glb", CompanionComplete},
		{".GLB", CompanionComplete}, // leading dot + case are both tolerated
		{"fbx", CompanionComplete},
		{"obj", CompanionFirstLevel},
		// Not a claim that these are self-contained — a claim that we
		// cannot read their references. DAE in particular declares
		// images in <library_images>.
		{"dae", CompanionUnsupported},
		{"stl", CompanionUnsupported},
		{"ply", CompanionUnsupported},
		{"png", CompanionUnsupported},
		{"", CompanionUnsupported},
	} {
		if got := CompanionSupportFor(c.ext); got != c.want {
			t.Errorf("CompanionSupportFor(%q) = %v, want %v", c.ext, got, c.want)
		}
	}
}

// TestDeclaredCompanions_ReadsFromBytesAlone is the property the upload
// path needs: no filesystem, no sibling directory, just the model's own
// bytes. An uploaded asset lives in content-addressed storage under a
// hash and has neither.
func TestDeclaredCompanions_ReadsFromBytesAlone(t *testing.T) {
	doc := `{
		"asset":{"version":"2.0"},
		"buffers":[{"uri":"geometry.bin","byteLength":4}],
		"images":[{"uri":"Textures/planks.png"},{"uri":"data:image/png;base64,iVBORw0KGgo="}]
	}`

	t.Run("glb", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "wall.glb")
		writeGLB(t, p, doc, []byte("bin\x00"))
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got, support, err := DeclaredCompanions("glb", bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if support != CompanionComplete {
			t.Errorf("support = %v, want CompanionComplete", support)
		}
		want := []string{"geometry.bin", "Textures/planks.png"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("declared = %v, want %v (the data: URI needs no companion)", got, want)
		}
	})

	t.Run("gltf", func(t *testing.T) {
		got, support, err := DeclaredCompanions(".GLTF", strings.NewReader(doc))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if support != CompanionComplete {
			t.Errorf("support = %v, want CompanionComplete", support)
		}
		if !reflect.DeepEqual(got, []string{"geometry.bin", "Textures/planks.png"}) {
			t.Errorf("declared = %v", got)
		}
	})
}

// TestDeclaredCompanions_OBJIsFirstLevelOnly.
//
// An .obj names .mtl libraries and each .mtl names its textures. The
// second level needs the .mtl's CONTENT, which at ingest has not been
// uploaded. Reporting the first level is correct; reporting it WITHOUT
// saying so would under-report silently, which recreates the original
// bug one level down.
func TestDeclaredCompanions_OBJIsFirstLevelOnly(t *testing.T) {
	obj := "mtllib barrel.mtl extra.mtl\nv 0 0 0\n"
	got, support, err := DeclaredCompanions("obj", strings.NewReader(obj))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if support != CompanionFirstLevel {
		t.Fatalf("support = %v, want CompanionFirstLevel — a caller that thinks this "+
			"list is complete will tell the artist a lie by omission", support)
	}
	if !reflect.DeepEqual(got, []string{"barrel.mtl", "extra.mtl"}) {
		t.Errorf("declared = %v", got)
	}
}

// TestDeclaredCompanions_UnsupportedIsNotEmpty.
//
// ⚠️ The one answer this function must never give: a confident empty
// list for a format nobody could read. `unsupported` and "declares
// nothing" are different sentences and the support value is how the
// caller tells them apart.
func TestDeclaredCompanions_UnsupportedIsNotEmpty(t *testing.T) {
	got, support, err := DeclaredCompanions("dae", strings.NewReader("<COLLADA/>"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if support != CompanionUnsupported {
		t.Fatalf("support = %v, want CompanionUnsupported", support)
	}
	if got != nil {
		t.Errorf("declared = %v, want nil", got)
	}
}

// TestDeclaredCompanions_MalformedErrorsRatherThanReportingNone.
//
// The soft-fail contract belongs to the CALLER. A container we could not
// finish reading must never come back as "declares no companions" — that
// is the silent-success failure the endpoint above it exists to remove.
func TestDeclaredCompanions_MalformedErrorsRatherThanReportingNone(t *testing.T) {
	got, _, err := DeclaredCompanions("glb", strings.NewReader("not a glb at all"))
	if err == nil {
		t.Fatalf("malformed GLB parsed without error, declared = %v", got)
	}
	if got != nil {
		t.Errorf("declared = %v, want nil alongside the error", got)
	}
}

// TestResolveCompanions_StillResolvesTheOBJSecondLevel.
//
// The refactor moved the extension table out of ResolveCompanions and
// left it the filesystem half. The OBJ second level — each .mtl's own
// textures, read from the .mtl beside the model — is the part that
// genuinely needs a directory, and it must survive the move.
func TestResolveCompanions_StillResolvesTheOBJSecondLevel(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "barrel.obj"), "mtllib barrel.mtl\nv 0 0 0\n")
	writeFile(t, filepath.Join(dir, "barrel.mtl"), "newmtl m\nmap_Kd wood.png\n")
	writeFile(t, filepath.Join(dir, "wood.png"), "png")

	found, missing, err := ResolveCompanions(filepath.Join(dir, "barrel.obj"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"barrel.mtl", "wood.png"}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("found = %v, want %v — the .mtl recursion did not survive the split", found, want)
	}
	if missing != nil {
		t.Errorf("missing = %v, want nil", missing)
	}
}
