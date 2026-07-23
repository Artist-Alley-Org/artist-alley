// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package format3d

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestResolveCompanions_SelfContained(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "model.glb"), "glTF binary")
	found, missing, err := ResolveCompanions(filepath.Join(dir, "model.glb"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found != nil || missing != nil {
		t.Fatalf("GLB should declare no companions: found=%v missing=%v", found, missing)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
