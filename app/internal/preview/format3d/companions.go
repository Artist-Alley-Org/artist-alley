// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package format3d

// Companion discovery for multi-file 3D assets (#486).
//
// A .gltf (JSON) declares its geometry buffer + textures as external
// URIs (buffers[].uri, images[].uri). An .obj points at one or more
// .mtl material libraries (`mtllib`), and each .mtl points at texture
// maps (`map_Kd`, `bump`, ...). None of that is embedded — the loader
// (the headless three.js worker, or the interactive viewer's GLTFLoader) resolves
// those references relative to the model file's directory at load time.
//
// So a multi-file model only renders if its siblings are registered as
// companions. This file extracts the declared references so the ingest
// / seed path can register them automatically instead of relying on the
// uploader to attach each one by hand.
//
// A .glb is NOT automatically self-contained (#750). It is a binary
// container wrapping the same glTF JSON document, so its buffers[].uri /
// images[].uri can name external files exactly as a .gltf's can — an
// exporter chooses whether to embed. Only parsing tells you which: 363
// of the 374 GLBs in the seed catalogue reference external textures.
// So GLB is parsed, and "self-contained" is a result, not an assumption.
//
// FBX was the same shape of question and is now answered the same way
// (#753, see fbx.go): a Video node either embeds its bytes in a Content
// property or names a file in RelativeFilename / FileName. 127 of the
// seed catalogue's 131 FBX name a file, 126 name one that could be a
// sibling, and 0 embed. Reading it needs a node walk over both container
// encodings, so it lives in its own file.
//
// Nothing here assumes a format is self-contained. STL, PLY and DAE
// return nil because no reader exists for their references yet, which is
// a gap in this file and not a property of those formats — DAE in
// particular declares images in <library_images>.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ParseGLTFCompanions extracts the external resource paths a glTF (JSON)
// document declares — buffers[].uri and images[].uri — as clean,
// forward-slash relative paths in document order, de-duplicated.
//
// Embedded `data:` URIs and any absolute or remote (scheme://, leading
// '/') reference are skipped: those need no companion. Percent-encoded
// URIs (the glTF spec stores URIs URL-encoded) are decoded so the
// returned path matches the sibling file on disk.
func ParseGLTFCompanions(gltfJSON []byte) ([]string, error) {
	// Minimal shape: we only care about the URIs, not the full document.
	// Unmarshalling into a narrow struct avoids pulling the whole model
	// (and avoids any loader trying to fetch buffers eagerly).
	var doc struct {
		Buffers []struct {
			URI string `json:"uri"`
		} `json:"buffers"`
		Images []struct {
			URI string `json:"uri"`
		} `json:"images"`
	}
	if err := json.Unmarshal(gltfJSON, &doc); err != nil {
		return nil, fmt.Errorf("parse gltf: %w", err)
	}

	seen := make(map[string]struct{})
	var out []string
	add := func(uri string) {
		p, ok := cleanCompanionURI(uri)
		if !ok {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, b := range doc.Buffers {
		add(b.URI)
	}
	for _, im := range doc.Images {
		add(im.URI)
	}
	return out, nil
}

// ParseGLBCompanions extracts the external resource paths a GLB (binary
// glTF) declares. A GLB wraps a glTF JSON document in a chunked binary
// container, so this pulls the JSON chunk out and hands it to
// ParseGLTFCompanions — same document, same rules. A GLB whose buffers
// and images are all embedded yields none, which is the answer for 11 of
// the 374 GLBs in the seed catalogue and an assumption for none of them.
func ParseGLBCompanions(r io.Reader) ([]string, error) {
	gltfJSON, err := ReadGLBJSONChunk(r)
	if err != nil {
		return nil, err
	}
	return ParseGLTFCompanions(gltfJSON)
}

// ParseOBJMaterialLibs returns the .mtl filenames an OBJ references via
// `mtllib` directives. A single directive may list several libraries:
//
//	mtllib a.mtl b.mtl
func ParseOBJMaterialLibs(obj []byte) []string {
	return scanDirectiveArgs(obj, map[string]bool{"mtllib": true})
}

// mtlMapDirectives is the set of MTL statements whose (last) argument is
// a texture filename. Covers the common maps plus the PBR extension
// keywords three.js's MTLLoader understands.
var mtlMapDirectives = map[string]bool{
	"map_ka": true, "map_kd": true, "map_ks": true, "map_ke": true,
	"map_ns": true, "map_d": true, "map_bump": true, "bump": true,
	"disp": true, "decal": true, "refl": true, "norm": true,
	"map_pr": true, "map_pm": true, "map_ps": true, "map_ke_": true,
}

// ParseMTLTextures returns the texture map filenames an MTL references.
// The filename is taken as the LAST whitespace-delimited token on the
// line so leading option flags (`-bm 0.2`, `-o u v w`, `-s ...`) that
// precede the path are ignored.
func ParseMTLTextures(mtl []byte) []string {
	seen := make(map[string]struct{})
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(mtl))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if !mtlMapDirectives[strings.ToLower(fields[0])] {
			continue
		}
		// The path is the last token; option flags precede it.
		p, ok := cleanCompanionURI(fields[len(fields)-1])
		if !ok {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// ResolveCompanions walks the on-disk companions of a model file and
// returns their paths RELATIVE to the model's directory (forward-slash),
// deterministically ordered. glTF and GLB resolve their buffer + image
// URIs (a GLB may reference external URIs — parse to find out, #750);
// FBX resolves its Video/Texture filenames unless the media is embedded
// (#753); OBJ resolves mtllib .mtl files and, recursively, the textures
// each .mtl declares. Every other extension returns nil because nothing
// here can read its references yet.
//
// `found` lists companions that exist on disk; `missing` lists declared
// references whose file is absent (so the caller can log the gap without
// failing). A subdirectory reference (e.g. `textures/foo.png`) is
// preserved in the returned relative path.
func ResolveCompanions(mainPath string) (found []string, missing []string, err error) {
	dir := filepath.Dir(mainPath)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(mainPath), "."))

	// declared holds every relative companion path we should look for,
	// in discovery order; exists filters against the filesystem.
	var declared []string
	switch ext {
	case "gltf":
		raw, rerr := os.ReadFile(mainPath)
		if rerr != nil {
			return nil, nil, fmt.Errorf("read gltf: %w", rerr)
		}
		declared, err = ParseGLTFCompanions(raw)
		if err != nil {
			return nil, nil, err
		}
	case "glb":
		// Streamed, not ReadFile: only the leading header + JSON chunk is
		// needed, and the BIN chunk after it is the bulk of the file.
		f, oErr := os.Open(mainPath)
		if oErr != nil {
			return nil, nil, fmt.Errorf("open glb: %w", oErr)
		}
		declared, err = ParseGLBCompanions(f)
		f.Close()
		if err != nil {
			// A malformed container resolves to no companions rather than
			// killing the caller's run; the error carries why, and the
			// seed/ingest caller logs it per the soft-fail contract.
			return nil, nil, err
		}
	case "fbx":
		// Streamed for the same reason GLB is: the references sit in small
		// Video/Texture records and the geometry that dwarfs them is
		// skipped, not buffered.
		f, oErr := os.Open(mainPath)
		if oErr != nil {
			return nil, nil, fmt.Errorf("open fbx: %w", oErr)
		}
		declared, err = ParseFBXCompanions(f)
		f.Close()
		if err != nil {
			// Soft-fail contract, as for glb: an unreadable container
			// resolves to no companions and the error says why, rather than
			// silently claiming the model has none.
			return nil, nil, err
		}
	case "obj":
		raw, rerr := os.ReadFile(mainPath)
		if rerr != nil {
			return nil, nil, fmt.Errorf("read obj: %w", rerr)
		}
		// mtllib files, then each .mtl's textures (paths are relative to
		// the .mtl, which for our seed layout is the model dir).
		seen := make(map[string]struct{})
		addDeclared := func(rel string) {
			if _, dup := seen[rel]; dup {
				return
			}
			seen[rel] = struct{}{}
			declared = append(declared, rel)
		}
		for _, mtlRel := range ParseOBJMaterialLibs(raw) {
			addDeclared(mtlRel)
			mtlAbs := filepath.Join(dir, filepath.FromSlash(mtlRel))
			mtlBytes, mErr := os.ReadFile(mtlAbs)
			if mErr != nil {
				continue // missing .mtl surfaces below via the exists check
			}
			mtlDir := path.Dir(mtlRel) // texture paths are relative to the .mtl
			for _, tex := range ParseMTLTextures(mtlBytes) {
				rel := tex
				if mtlDir != "." && mtlDir != "" {
					rel = path.Join(mtlDir, tex)
				}
				addDeclared(rel)
			}
		}
	default:
		// No reader for this format's references (STL, PLY, DAE, ...).
		// Not the same claim as "it has none" — see the file header.
		return nil, nil, nil
	}

	for _, rel := range declared {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if _, statErr := os.Stat(abs); statErr != nil {
			missing = append(missing, rel)
			continue
		}
		found = append(found, rel)
	}
	return found, missing, nil
}

// cleanCompanionURI normalises a declared URI into a safe relative
// companion path, or reports ok=false when the reference needs no
// companion (embedded data: URI, remote scheme, absolute path) or would
// escape the model directory (`..`, leading '/').
func cleanCompanionURI(uri string) (string, bool) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return "", false
	}
	// Embedded payloads and remote resources are self-resolving.
	lower := strings.ToLower(uri)
	if strings.HasPrefix(lower, "data:") ||
		strings.Contains(uri, "://") {
		return "", false
	}
	// glTF URIs are percent-encoded; decode to match the on-disk name.
	if dec, derr := url.PathUnescape(uri); derr == nil {
		uri = dec
	}
	// Backslash → slash, explicitly (#753). This used to be
	// filepath.ToSlash, which is a NO-OP on Linux: it swaps
	// os.PathSeparator for '/', and on Linux the separator already IS
	// '/'. So `Textures\barrel.png` — what every FBX and many
	// Windows-authored MTLs write — would survive as ONE path segment
	// containing a backslash, and both consumers of the stored path split
	// on '/' only:
	//
	//   * preview.stageCompanions joins the stored path onto the render
	//     workdir, producing a file literally NAMED "Textures\barrel.png"
	//     in the workdir root rather than a Textures/ directory;
	//   * companionLoadingManager compares the request against the stored
	//     path, and its basename of `textures\barrel.png` is the whole
	//     string, so it can never equal the `barrel.png` a loader asks
	//     for.
	//
	// Note which side the backslash is on. #753 predicted the loader would
	// REQUEST a backslash URL; measured, it does not — three.js's
	// FBXLoader does `images[id].split('\\').pop()` and asks for the bare
	// basename. The break is on the stored-companion side, which is why
	// the fix belongs here.
	//
	// Fixing it HERE rather than at the two consumption points is a
	// deliberate choice, and it is safe because it changes nothing that
	// already works: this function is shared with glTF/GLB/OBJ, and
	// measured over the whole seed catalogue not one .mtl, .gltf or GLB
	// JSON chunk writes a backslash in a URI (0 of 14,596 references
	// across 11,078 files), nor does any asset_companions row carry one.
	// So there is no stored data to migrate — only FBX changes behaviour.
	// It is still the right place for the other formats: a
	// Windows-authored MTL may legitimately write
	// `map_Kd Textures\foo.png`, and one canonical stored form beats two
	// consumers each remembering to normalise.
	//
	// The stored companion path is POSIX-relative regardless of what wrote
	// the model.
	uri = strings.ReplaceAll(uri, `\`, "/")
	if strings.HasPrefix(uri, "/") {
		return "", false
	}
	// A Windows drive-absolute path names a file on the exporter's
	// machine, not a sibling of the model. Four of the catalogue's FBX
	// carry one in FileName (`C:\Users\...\tex.png`), and after the
	// conversion above it would otherwise pass as an ordinary relative
	// path.
	if len(uri) >= 2 && uri[1] == ':' && isASCIILetter(uri[0]) {
		return "", false
	}
	cleaned := path.Clean(uri)
	if cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", false
	}
	return cleaned, true
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// scanDirectiveArgs collects every whitespace-delimited argument of the
// named line directives (case-insensitive), cleaned to safe relative
// paths and de-duplicated in first-seen order.
func scanDirectiveArgs(data []byte, directives map[string]bool) []string {
	seen := make(map[string]struct{})
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !directives[strings.ToLower(fields[0])] {
			continue
		}
		for _, arg := range fields[1:] {
			p, ok := cleanCompanionURI(arg)
			if !ok {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}
