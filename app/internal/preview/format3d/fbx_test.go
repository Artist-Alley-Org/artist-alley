// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package format3d

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testdata/kenney-barrels.fbx is a REAL export, not a synthesised
// container: Kenney's "Retro Fantasy Kit" barrels.fbx, CC0, written by
// FBX SDK 2020.3.2 as binary version 7700. It is in the repo because
// ADR 0068's amendment is specific — a fixture has to be able to
// exercise what it claims to cover, and #750's predecessor test proved
// how far a plausible-looking hand-built fixture gets you (eleven bytes
// of text asserting "declares no companions", passing for two releases
// while the bug shipped).
//
// Its two Video/Texture pairs name Textures\barrel.png and
// Textures\planks.png with Windows separators, so it also covers the
// canonicalisation half of #753.
const fbxRealFixture = "testdata/kenney-barrels.fbx"

func TestParseFBXCompanions_RealBinaryExport(t *testing.T) {
	f, err := os.Open(fbxRealFixture)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	got, err := ParseFBXCompanions(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Asserting the exact SET, not "found something": a filename-shaped
	// byte regex over this file also "finds something", which is why one
	// is not the resolver.
	want := []string{"Textures/barrel.png", "Textures/planks.png"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolveCompanions_FBXRealExport(t *testing.T) {
	dir := t.TempDir()
	blob, err := os.ReadFile(fbxRealFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	model := filepath.Join(dir, "barrels.fbx")
	if err := os.WriteFile(model, blob, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The reference is `Textures\barrel.png`. If the stored path kept the
	// backslash it would be a single segment, this MkdirAll+file layout
	// would not satisfy it, and `found` would be empty — so this asserts
	// the canonicalisation as much as the parse.
	if err := os.MkdirAll(filepath.Join(dir, "Textures"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(dir, "Textures", "barrel.png"), "png")
	// planks.png deliberately absent — it must surface as missing, not
	// vanish and not fail the resolve.

	found, missing, err := ResolveCompanions(model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(found, []string{"Textures/barrel.png"}) {
		t.Fatalf("found = %v", found)
	}
	if !reflect.DeepEqual(missing, []string{"Textures/planks.png"}) {
		t.Fatalf("missing = %v", missing)
	}
}

// Both container widths, for both directions. The catalogue holds 123
// files at version 7700 (64-bit record offsets) and 8 at 7400 (32-bit),
// so a parser that knows one width reads the other as garbage.
func TestParseFBXCompanions_BinaryBothWidthsBothDirections(t *testing.T) {
	for _, version := range []uint32{7400, 7700} {
		t.Run(versionName(version), func(t *testing.T) {
			// External: the same shape the real export has.
			external := buildFBXBinary(t, version, fbxSceneNodes(nil))
			got, err := ParseFBXCompanions(bytes.NewReader(external))
			if err != nil {
				t.Fatalf("external: %v", err)
			}
			if want := []string{"Textures/barrel.png"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("external: got %v, want %v", got, want)
			}

			// Embedded: identical tree PLUS a Content property holding a
			// real PNG. That is the "embed media" export option, and the
			// loader uses those bytes over the path — so the honest answer
			// is no companions, even though a Texture node still names the
			// file. The two fixtures differ ONLY by that node, so this
			// cannot pass for the reason #750's did (bytes the parser
			// rejected outright): the sibling case above proves the parser
			// reads this exact container and finds the reference.
			embedded := buildFBXBinary(t, version, fbxSceneNodes(onePixelPNG(t)))
			got, err = ParseFBXCompanions(bytes.NewReader(embedded))
			if err != nil {
				t.Fatalf("embedded: %v", err)
			}
			if got != nil {
				t.Fatalf("embedded media should declare no companions, got %v", got)
			}
		})
	}
}

func TestParseFBXCompanions_PrefersRelativeOverAbsolute(t *testing.T) {
	// Four of the catalogue's FBX carry an authoring-machine absolute path
	// in FileName next to a usable RelativeFilename. three.js's FBXLoader
	// resolves `RelativeFilename || Filename`, so registering the absolute
	// one would register a companion the loader never asks for.
	both := buildFBXBinary(t, 7700, []fbxTestNode{{
		name: "Objects",
		kids: []fbxTestNode{{
			name:  "Texture",
			props: []fbxTestProp{strProp("tex\x00\x01Texture")},
			kids: []fbxTestNode{
				{name: "FileName", props: []fbxTestProp{strProp(`C:\Users\Someone\Desktop\ChTX_mario_01_cl.png`)}},
				{name: "RelativeFilename", props: []fbxTestProp{strProp("ChTX_mario_01_cl.png")}},
			},
		}},
	}})
	got, err := ParseFBXCompanions(bytes.NewReader(both))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"ChTX_mario_01_cl.png"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// An absolute path with no relative alternative is not a sibling of
	// the model and resolves to nothing at all.
	absOnly := buildFBXBinary(t, 7700, []fbxTestNode{{
		name: "Objects",
		kids: []fbxTestNode{{
			name:  "Video",
			props: []fbxTestProp{strProp("clip\x00\x01Video")},
			kids: []fbxTestNode{
				{name: "Filename", props: []fbxTestProp{strProp(`C:\Users\OMISTAJA\Downloads\clyde_diffuse.png`)}},
			},
		}},
	}})
	got, err = ParseFBXCompanions(bytes.NewReader(absOnly))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("absolute-only reference should declare no companions, got %v", got)
	}
}

// A filename-shaped string outside a Video/Texture node is not a texture
// reference. This is the property a byte-level regex cannot have, and the
// reason #753 says a regex is evidence and not a resolver.
func TestParseFBXCompanions_IgnoresFilenamesOutsideMediaNodes(t *testing.T) {
	blob := buildFBXBinary(t, 7700, []fbxTestNode{
		{name: "Creator", props: []fbxTestProp{strProp("Blender (stable FBX IO) - 4.2.0 - render.png")}},
		{name: "Objects", kids: []fbxTestNode{
			{
				name:  "Model",
				props: []fbxTestProp{strProp("hand.L.png\x00\x01Model"), strProp("LimbNode")},
				kids: []fbxTestNode{
					// A Model's FileName is not a media reference.
					{name: "Properties70", kids: []fbxTestNode{
						{name: "P", props: []fbxTestProp{strProp("RelPath"), strProp("KString"), strProp("decoy.tga")}},
					}},
				},
			},
			{name: "Geometry", props: []fbxTestProp{strProp("mesh.png\x00\x01Geometry"), strProp("Mesh")}},
		}},
	})
	got, err := ParseFBXCompanions(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("no Video/Texture node exists, so nothing is declared; got %v", got)
	}
}

func TestParseFBXCompanions_ASCII(t *testing.T) {
	// The catalogue has no ASCII FBX, so this is written to the documented
	// ASCII layout — the same tree the binary fixtures carry, in the text
	// encoding the FBX SDK emits for "FBX ASCII".
	external := `; FBX 7.3.0 project file
; ----------------------------------------------------

FBXHeaderExtension:  {
	FBXHeaderVersion: 1003
	FBXVersion: 7300
	Creator: "FBX SDK/FBX Plugins version 2020.3.2"
}
Objects:  {
	Video: 105553116266432, "Video::barrel", "Clip" {
		Type: "Clip"
		Filename: "Textures\barrel.png"
		RelativeFilename: "Textures\barrel.png"
	}
	Texture: 105553116266433, "Texture::barrel", "" {
		Type: "TextureVideoClip"
		FileName: "Textures\barrel.png"
		RelativeFilename: "Textures\barrel.png"
	}
	Model: 105553116266434, "Model::planks.png", "Mesh" {
		Properties70:  {
			P: "RelPath", "KString", "XRefUrl", "", "decoy.tga"
		}
	}
}
`
	got, err := ParseFBXCompanions(strings.NewReader(external))
	if err != nil {
		t.Fatalf("external: %v", err)
	}
	if want := []string{"Textures/barrel.png"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("external: got %v, want %v", got, want)
	}

	embedded := strings.Replace(external,
		`		Type: "Clip"`,
		"\t\tType: \"Clip\"\n\t\tContent: \"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8DwQACfsD/Wq9lPwAAAAASUVORK5CYII=\"",
		1)
	got, err = ParseFBXCompanions(strings.NewReader(embedded))
	if err != nil {
		t.Fatalf("embedded: %v", err)
	}
	if got != nil {
		t.Fatalf("ASCII embedded media should declare no companions, got %v", got)
	}
}

// Bytes that are not a readable FBX produce an error and no companions.
// "Unreadable" must never collapse into "declares none".
func TestResolveCompanions_FBXMalformed(t *testing.T) {
	truncated := buildFBXBinary(t, 7700, fbxSceneNodes(nil))
	truncated = truncated[:len(truncated)/2]

	badOffset := buildFBXBinary(t, 7700, fbxSceneNodes(nil))
	// Point the first record's endOffset past the end of the file.
	binary.LittleEndian.PutUint64(badOffset[fbxHeaderLen:], 1<<62)

	cases := map[string][]byte{
		// The #750 shape: a plausible text file wearing the extension.
		"plain text":     []byte("this is not an fbx, it is a note to self\n"),
		"empty":          {},
		"magic only":     []byte(fbxBinaryMagic),
		"truncated tree": truncated,
		"bad end offset": badOffset,
	}
	dir := t.TempDir()
	for name, blob := range cases {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".fbx")
		if err := os.WriteFile(p, blob, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		found, missing, err := ResolveCompanions(p)
		if err == nil {
			t.Fatalf("%s: expected an error, got found=%v", name, found)
		}
		if found != nil || missing != nil {
			t.Fatalf("%s: found=%v missing=%v, want none", name, found, missing)
		}
	}
}

// cleanCompanionURI's separator handling, which used to be
// filepath.ToSlash — a no-op on Linux, so every backslash survived into
// the stored companion path as part of one segment (#753).
func TestCleanCompanionURI_Separators(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`Textures\barrel.png`, "Textures/barrel.png", true},
		{`Textures\sub\deep.png`, "Textures/sub/deep.png", true},
		{`Textures/already-posix.png`, "Textures/already-posix.png", true},
		{`C:\Users\Someone\tex.png`, "", false},
		{`c:/Users/Someone/tex.png`, "", false},
		{`\\server\share\tex.png`, "", false},
		{`..\..\etc\passwd`, "", false},
		{`\abs.png`, "", false},
	}
	for _, tc := range cases {
		got, ok := cleanCompanionURI(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("cleanCompanionURI(%q) = %q,%v; want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// --------------------------------------------------------------- helpers

func versionName(v uint32) string {
	if v >= fbxWideVersion {
		return "v" + itoa(v) + "-64bit-offsets"
	}
	return "v" + itoa(v) + "-32bit-offsets"
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// fbxTestNode / fbxTestProp are the writer side of the record format: the
// fixtures below are assembled as a tree and serialised with real
// endOffset arithmetic, so the parser walks them exactly as it walks an
// FBX SDK export.
type fbxTestNode struct {
	name  string
	props []fbxTestProp
	kids  []fbxTestNode
}

type fbxTestProp struct {
	code byte
	data []byte
}

func strProp(s string) fbxTestProp { return fbxTestProp{code: 'S', data: []byte(s)} }
func rawProp(b []byte) fbxTestProp { return fbxTestProp{code: 'R', data: b} }

// fbxSceneNodes is the tree a one-texture Kenney-style export has: a
// header extension, then a Video clip and the Texture that applies it,
// both naming the same Windows-separated relative path. `embedded`, when
// non-empty, becomes the Video's Content property — the difference
// between "references a sibling" and "carries the image".
func fbxSceneNodes(embedded []byte) []fbxTestNode {
	video := fbxTestNode{
		name:  "Video",
		props: []fbxTestProp{strProp("barrel\x00\x01Video"), strProp("Clip")},
		kids: []fbxTestNode{
			{name: "Type", props: []fbxTestProp{strProp("Clip")}},
			{name: "Filename", props: []fbxTestProp{strProp(`Textures\barrel.png`)}},
			{name: "RelativeFilename", props: []fbxTestProp{strProp(`Textures\barrel.png`)}},
		},
	}
	if len(embedded) > 0 {
		video.kids = append(video.kids,
			fbxTestNode{name: "Content", props: []fbxTestProp{rawProp(embedded)}})
	}
	return []fbxTestNode{
		{name: "FBXHeaderExtension", kids: []fbxTestNode{
			{name: "Creator", props: []fbxTestProp{strProp("artist-alley format3d test")}},
		}},
		{name: "Objects", kids: []fbxTestNode{
			video,
			{
				name:  "Texture",
				props: []fbxTestProp{strProp("barrel\x00\x01Texture"), strProp("")},
				kids: []fbxTestNode{
					{name: "Type", props: []fbxTestProp{strProp("TextureVideoClip")}},
					{name: "FileName", props: []fbxTestProp{strProp(`Textures\barrel.png`)}},
					{name: "RelativeFilename", props: []fbxTestProp{strProp(`Textures\barrel.png`)}},
				},
			},
		}},
	}
}

// buildFBXBinary serialises roots as a binary FBX of the given version:
// 27-byte header, the record list, then the top-level null record.
func buildFBXBinary(t *testing.T, version uint32, roots []fbxTestNode) []byte {
	t.Helper()
	wide := version >= fbxWideVersion
	var buf bytes.Buffer
	buf.WriteString(fbxBinaryMagic)
	buf.Write([]byte{0x1A, 0x00})
	var v [4]byte
	binary.LittleEndian.PutUint32(v[:], version)
	buf.Write(v[:])

	off := uint64(fbxHeaderLen)
	for _, r := range roots {
		b := encodeFBXTestNode(r, off, wide)
		buf.Write(b)
		off += uint64(len(b))
	}
	buf.Write(make([]byte, fbxNullRecordLen(wide)))
	return buf.Bytes()
}

func fbxNullRecordLen(wide bool) int {
	if wide {
		return 25
	}
	return 13
}

func encodeFBXTestNode(n fbxTestNode, base uint64, wide bool) []byte {
	hdrLen := uint64(fbxNullRecordLen(wide))
	props := encodeFBXTestProps(n.props)
	kidsStart := base + hdrLen + uint64(len(n.name)) + uint64(len(props))

	var kids bytes.Buffer
	if len(n.kids) > 0 {
		off := kidsStart
		for _, k := range n.kids {
			b := encodeFBXTestNode(k, off, wide)
			kids.Write(b)
			off += uint64(len(b))
		}
		kids.Write(make([]byte, fbxNullRecordLen(wide)))
	}

	var out bytes.Buffer
	putFBXNum(&out, kidsStart+uint64(kids.Len()), wide) // endOffset
	putFBXNum(&out, uint64(len(n.props)), wide)
	putFBXNum(&out, uint64(len(props)), wide)
	out.WriteByte(byte(len(n.name)))
	out.WriteString(n.name)
	out.Write(props)
	out.Write(kids.Bytes())
	return out.Bytes()
}

func encodeFBXTestProps(props []fbxTestProp) []byte {
	var out bytes.Buffer
	for _, p := range props {
		out.WriteByte(p.code)
		var l [4]byte
		binary.LittleEndian.PutUint32(l[:], uint32(len(p.data)))
		out.Write(l[:])
		out.Write(p.data)
	}
	return out.Bytes()
}

func putFBXNum(w *bytes.Buffer, v uint64, wide bool) {
	if wide {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], v)
		w.Write(b[:])
		return
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	w.Write(b[:])
}

// onePixelPNG is real encoded PNG, not a placeholder string: the
// embedded-media direction is only meaningful if the Content property
// holds what an exporter would actually put there.
func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0x8b, G: 0x5a, B: 0x2b, A: 0xff})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}
