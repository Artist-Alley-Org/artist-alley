// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package format3d

// FBX external-reference reading (#753).
//
// An FBX is a tree of typed node records. The media a model uses lives
// under Objects as `Video` nodes (the image clip) and `Texture` nodes
// (how it is applied), and each names its file in `RelativeFilename` /
// `FileName` string properties. A Video may instead carry the bytes
// inline in a `Content` property — that is the "embedded media" export
// option — in which case there is no sibling file to register.
//
// So, exactly as for GLB (#750), whether an FBX is self-contained is a
// per-file fact you only learn by reading it. Measured over the seed
// catalogue's 131 FBX (109 in site_a, 22 in site_b): 127 carry a
// Video/Texture node naming a file, 0 embed a Content payload, and 126
// yield a usable relative companion path. The 127th is site_b's
// clyde.fbx, whose only reference is the authoring machine's
// `C:\Users\OMISTAJA\Downloads\clyde_diffuse.png` — a real external
// reference, but not to a sibling of the model, so it correctly resolves
// to nothing.
//
// Two container encodings are read:
//
//   - Binary. A 27-byte header ("Kaydara FBX Binary  \0", 0x1A 0x00,
//     uint32 version) then a list of node records. Each record is
//     [endOffset, numProperties, propertyListLen, nameLen, name,
//     properties..., nested records..., null record]. The three numbers
//     are uint32 below version 7500 and uint64 from 7500 on — the
//     catalogue has both (8 files at 7400, 123 at 7700), so a parser
//     that only knows one width reads half the corpus as garbage.
//   - ASCII. The same tree as indented `Name: args {` blocks. Every file
//     in the catalogue is binary, so the ASCII side is covered by
//     fixtures written to the documented layout rather than by the
//     corpus — stated as an assumption, not a guarantee (ADR 0068).
//
// Deliberately NOT a filename-shaped byte regex. A regex is what
// produced the evidence in #753 and it cannot tell a texture reference
// from any other string that looks like a path — the Creator string, a
// take name, a bone named "hand.L.png" all match. The node walk knows
// *which* property it is reading.
//
// Property payloads are skipped rather than decoded unless the node is
// one of the handful we read, so a 900 KB rigged character costs a walk
// over its record headers and no array decompression.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

const (
	// fbxBinaryMagic is the 21-byte sentinel a binary FBX opens with —
	// "Kaydara FBX Binary  " plus its NUL terminator.
	fbxBinaryMagic = "Kaydara FBX Binary  \x00"

	// fbxHeaderLen is magic(21) + 0x1A 0x00 (2) + uint32 version (4).
	fbxHeaderLen = 27

	// fbxWideVersion is the version from which record offsets, property
	// counts and property-list lengths widen from uint32 to uint64.
	fbxWideVersion = 7500

	// Bounds. A malformed or hostile container must fail, not exhaust
	// memory: every one of these is a "no real FBX comes near this"
	// ceiling, not a format limit.
	fbxMaxDepth   = 32
	fbxMaxNodes   = 1 << 21
	fbxMaxProps   = 1 << 20
	fbxMaxStrLen  = 64 << 10
	fbxMaxSkip    = 1 << 40
	fbxMaxASCIILn = 8 << 20
)

// fbxMedia is one Video or Texture node's reference to an image: the
// relative path it prefers, the (often absolute) authoring-time path,
// and whether the bytes were embedded in the file instead.
type fbxMedia struct {
	kind     string // "video" | "texture"
	rel      string // RelativeFilename
	abs      string // FileName / Filename
	embedded bool   // Content property carried bytes
}

// ParseFBXCompanions extracts the external media paths an FBX declares,
// as clean forward-slash relative paths in discovery order,
// de-duplicated.
//
// Per node the RELATIVE filename wins and the absolute one is only a
// fallback, which is what three.js's FBXLoader does too
// (`videoNode.RelativeFilename || videoNode.Filename`). Matching the
// loader matters: registering a companion the loader will never ask for
// is as useless as registering none. Absolute authoring paths
// (`C:\Users\...\tex.png`, which four of the catalogue's FBX carry) are
// rejected by cleanCompanionURI — they name a file on the exporter's
// machine, not a sibling of the model.
//
// A Video whose Content property holds bytes is embedded: it yields no
// companion, and neither does any Texture naming the same file, since
// the loader takes those bytes over the path.
//
// A container that cannot be read returns an error and no paths. It
// never returns "no companions" for bytes it failed to understand —
// that conflation is the bug #750 fixed for GLB.
func ParseFBXCompanions(r io.Reader) ([]string, error) {
	br := bufio.NewReaderSize(r, 64<<10)

	head, err := br.Peek(len(fbxBinaryMagic))
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, fmt.Errorf("format3d: read fbx header: %w", err)
	}
	if len(head) == len(fbxBinaryMagic) && string(head) == fbxBinaryMagic {
		return parseFBXBinary(br)
	}
	return parseFBXASCII(br)
}

// ---------------------------------------------------------------- binary

// fbxScanner walks a binary FBX forward-only. Records address each other
// by ABSOLUTE file offset, so the walk tracks its own position and skips
// (rather than seeks) to the next record — which keeps the reader an
// io.Reader and never holds the file in memory.
type fbxScanner struct {
	r     *bufio.Reader
	pos   uint64
	wide  bool
	nodes int
}

func parseFBXBinary(br *bufio.Reader) ([]string, error) {
	head := make([]byte, fbxHeaderLen)
	if _, err := io.ReadFull(br, head); err != nil {
		return nil, fmt.Errorf("format3d: read fbx header: %w", err)
	}
	version := binary.LittleEndian.Uint32(head[23:27])

	s := &fbxScanner{r: br, pos: fbxHeaderLen, wide: version >= fbxWideVersion}
	var media []*fbxMedia
	// The top-level record list is bounded only by the file itself; it
	// ends at its own null record (or, for a truncated file, at EOF).
	if err := s.walk(^uint64(0), 0, nil, &media); err != nil {
		return nil, fmt.Errorf("format3d: fbx v%d: %w", version, err)
	}
	return fbxCompanionPaths(media), nil
}

// walk reads the record list occupying [s.pos, end), collecting media
// nodes. `cur` is the enclosing Video/Texture node, if any, so a
// RelativeFilename record knows which one it belongs to.
func (s *fbxScanner) walk(end uint64, depth int, cur *fbxMedia, acc *[]*fbxMedia) error {
	if depth > fbxMaxDepth {
		return fmt.Errorf("record nesting deeper than %d", fbxMaxDepth)
	}
	hdrLen := uint64(13) // 3×uint32 + nameLen byte
	if s.wide {
		hdrLen = 25 // 3×uint64 + nameLen byte
	}

	for s.pos+hdrLen <= end {
		endOffset, err := s.number()
		if err != nil {
			// EOF with nothing read at the TOP level is a file that ended
			// after its last record without the trailing null record. EOF
			// anywhere else is a truncated file, which is an error: a
			// container we could not finish reading must never come back
			// as "declares no companions".
			if depth == 0 && errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("truncated record header at offset %d: %w", s.pos, err)
		}
		numProps, err := s.number()
		if err != nil {
			return fmt.Errorf("truncated record header at offset %d: %w", s.pos, err)
		}
		propLen, err := s.number()
		if err != nil {
			return fmt.Errorf("truncated record header at offset %d: %w", s.pos, err)
		}
		nameLenB, err := s.readN(1)
		if err != nil {
			return fmt.Errorf("truncated record header at offset %d: %w", s.pos, err)
		}
		nameLen := uint64(nameLenB[0])

		// The all-zero record terminates a list.
		if endOffset == 0 && numProps == 0 && propLen == 0 && nameLen == 0 {
			return nil
		}
		if endOffset <= s.pos || endOffset > end {
			return fmt.Errorf("record end offset %d out of range at %d", endOffset, s.pos)
		}
		if s.nodes++; s.nodes > fbxMaxNodes {
			return fmt.Errorf("more than %d records", fbxMaxNodes)
		}

		nameB, err := s.readN(int(nameLen))
		if err != nil {
			return fmt.Errorf("truncated record name at offset %d: %w", s.pos, err)
		}
		name := strings.ToLower(string(nameB))

		propsEnd := s.pos + propLen
		if propsEnd > endOffset {
			return fmt.Errorf("property list of %q overruns its record", name)
		}
		// Only the few nodes that can hold a media path are decoded; every
		// other property list — vertex arrays, curves, connections — is
		// skipped whole.
		if cur != nil && numProps > 0 && numProps <= fbxMaxProps && fbxIsMediaField(name) {
			if err := s.readMediaProp(name, cur); err != nil {
				return err
			}
		}
		if s.pos > propsEnd {
			return fmt.Errorf("property list of %q read past its end", name)
		}
		if err := s.skip(propsEnd - s.pos); err != nil {
			return fmt.Errorf("truncated property list of %q: %w", name, err)
		}

		child := cur
		if name == "video" || name == "texture" {
			child = &fbxMedia{kind: name}
			*acc = append(*acc, child)
		}
		if s.pos < endOffset {
			if err := s.walk(endOffset, depth+1, child, acc); err != nil {
				return err
			}
		}
		if s.pos > endOffset {
			return fmt.Errorf("record %q read past its end", name)
		}
		if err := s.skip(endOffset - s.pos); err != nil {
			return fmt.Errorf("truncated record %q: %w", name, err)
		}
	}
	return nil
}

// readMediaProp reads the FIRST property of a media field node, which is
// where the value lives, and skips the rest.
func (s *fbxScanner) readMediaProp(name string, cur *fbxMedia) error {
	p, err := s.readProp()
	if err != nil {
		return err
	}
	switch name {
	case "relativefilename":
		if p.code == 'S' {
			cur.rel = p.str
		}
	case "filename":
		if p.code == 'S' {
			cur.abs = p.str
		}
	case "content":
		// Bytes here mean the image travels inside the FBX. Only the
		// length matters; the payload is skipped like any other blob.
		if p.length > 0 {
			cur.embedded = true
		}
	}
	return nil
}

// fbxProp is as much of a property as companion discovery needs: its
// type code, its string payload when it is a string, and the declared
// byte length of a string/raw payload.
type fbxProp struct {
	code   byte
	str    string
	length uint64
}

var fbxArrayElemSize = map[byte]uint64{'f': 4, 'd': 8, 'l': 8, 'i': 4, 'b': 1}

func (s *fbxScanner) readProp() (fbxProp, error) {
	b, err := s.readN(1)
	if err != nil {
		return fbxProp{}, err
	}
	p := fbxProp{code: b[0]}
	switch p.code {
	case 'C':
		return p, s.skip(1)
	case 'Y':
		return p, s.skip(2)
	case 'I', 'F':
		return p, s.skip(4)
	case 'D', 'L':
		return p, s.skip(8)
	case 'f', 'd', 'l', 'i', 'b':
		hdr, err := s.readN(12)
		if err != nil {
			return p, err
		}
		arrayLen := uint64(binary.LittleEndian.Uint32(hdr[0:4]))
		encoding := binary.LittleEndian.Uint32(hdr[4:8])
		compLen := uint64(binary.LittleEndian.Uint32(hdr[8:12]))
		if encoding != 0 {
			return p, s.skip(compLen)
		}
		return p, s.skip(arrayLen * fbxArrayElemSize[p.code])
	case 'S', 'R':
		lb, err := s.readN(4)
		if err != nil {
			return p, err
		}
		p.length = uint64(binary.LittleEndian.Uint32(lb))
		if p.code == 'S' && p.length <= fbxMaxStrLen {
			data, err := s.readN(int(p.length))
			if err != nil {
				return p, err
			}
			p.str = string(data)
			return p, nil
		}
		return p, s.skip(p.length)
	default:
		return p, fmt.Errorf("unknown property type %#02x at offset %d", p.code, s.pos-1)
	}
}

func (s *fbxScanner) number() (uint64, error) {
	if s.wide {
		b, err := s.readN(8)
		if err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint64(b), nil
	}
	b, err := s.readN(4)
	if err != nil {
		return 0, err
	}
	return uint64(binary.LittleEndian.Uint32(b)), nil
}

func (s *fbxScanner) readN(n int) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.r, buf); err != nil {
		return nil, err
	}
	s.pos += uint64(n)
	return buf, nil
}

func (s *fbxScanner) skip(n uint64) error {
	if n == 0 {
		return nil
	}
	if n > fbxMaxSkip {
		return fmt.Errorf("declared span of %d bytes at offset %d", n, s.pos)
	}
	got, err := io.CopyN(io.Discard, s.r, int64(n))
	s.pos += uint64(got)
	return err
}

func fbxIsMediaField(name string) bool {
	switch name {
	case "relativefilename", "filename", "content":
		return true
	}
	return false
}

// ----------------------------------------------------------------- ASCII

// fbxBlock is one open `Name: … {` block during the ASCII walk. media is
// non-nil only for Video/Texture blocks, so a leaf line knows whether it
// is naming a texture or is just another property of some other node.
type fbxBlock struct {
	name  string
	media *fbxMedia
}

// parseFBXASCII reads the text form of the same tree. It is a
// brace-depth walk rather than a line grep for exactly the reason the
// binary side is a node walk: `FileName` means something only inside a
// Video or Texture block.
func parseFBXASCII(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), fbxMaxASCIILn)

	var stack []fbxBlock
	var media []*fbxMedia
	// An ASCII FBX always declares itself. Without this a plain text file
	// named .fbx would parse to "declares no companions", which is the
	// false-negative #750 was about.
	sawFBXMarker := false

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		if !sawFBXMarker &&
			(strings.Contains(line, "FBXHeaderExtension") || strings.Contains(line, "FBXVersion")) {
			sawFBXMarker = true
		}

		for strings.HasPrefix(line, "}") {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			line = strings.TrimSpace(line[1:])
		}
		if line == "" {
			continue
		}

		key := ""
		colon := strings.IndexByte(line, ':')
		if colon > 0 {
			k := strings.TrimSpace(line[:colon])
			if !strings.ContainsAny(k, "\"{}") {
				key = strings.ToLower(k)
			}
		}

		if strings.HasSuffix(line, "{") {
			blk := fbxBlock{name: key}
			if key == "video" || key == "texture" {
				blk.media = &fbxMedia{kind: key}
				media = append(media, blk.media)
			}
			if len(stack) >= fbxMaxDepth {
				return nil, fmt.Errorf("format3d: fbx ascii nesting deeper than %d", fbxMaxDepth)
			}
			stack = append(stack, blk)
			continue
		}

		if key != "" && len(stack) > 0 {
			if cur := stack[len(stack)-1].media; cur != nil {
				value := fbxQuoted(line)
				switch key {
				case "relativefilename":
					cur.rel = value
				case "filename":
					cur.abs = value
				case "content":
					// `Content: ,` with the base64 payload on following lines
					// is still embedded media; only an explicit empty string
					// is not.
					rest := strings.Trim(strings.TrimSpace(line[colon+1:]), ", \t")
					if rest != "" && rest != `""` {
						cur.embedded = true
					}
				}
			}
		}

		for strings.HasSuffix(line, "}") {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			line = strings.TrimSpace(strings.TrimSuffix(line, "}"))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("format3d: read fbx ascii: %w", err)
	}
	if !sawFBXMarker {
		return nil, errors.New("format3d: not an fbx (no binary magic, no FBXHeaderExtension)")
	}
	return fbxCompanionPaths(media), nil
}

// fbxQuoted returns the text between the first and last double quote on
// a line, which is how ASCII FBX writes a string value.
func fbxQuoted(line string) string {
	first := strings.IndexByte(line, '"')
	last := strings.LastIndexByte(line, '"')
	if first < 0 || last <= first {
		return ""
	}
	return line[first+1 : last]
}

// ----------------------------------------------------------------- shared

// fbxCompanionPaths turns the collected media nodes into the companion
// list: relative-first per node, embedded media excluded, cleaned and
// de-duplicated in discovery order.
func fbxCompanionPaths(media []*fbxMedia) []string {
	// Only the Video carries the Content, so an embedded image would
	// still register a companion via the Texture node that applies it.
	// The embedded names are therefore collected first and excluded
	// everywhere.
	//
	// Pairing a Texture to its Video by FILENAME is a heuristic, and
	// deliberately so: the authoritative link is a Connections record
	// (`C: "OO", videoID, textureID`), and walking Connections means
	// holding the whole object graph to resolve IDs the walk has already
	// streamed past. Exporters write the same path into both nodes — the
	// Kenney fixture does, and three.js reads the Video's path for
	// exactly this reason — so the filename is a faithful key in
	// practice. Where it is not, the cost is one staged file the loader
	// never asks for, never a missing texture: this only ever REMOVES
	// candidates, and only when some Video in the same file embedded an
	// image of that name.
	embedded := make(map[string]struct{})
	for _, m := range media {
		if !m.embedded {
			continue
		}
		for _, raw := range []string{m.rel, m.abs} {
			if raw == "" {
				continue
			}
			embedded[strings.ToLower(fbxBase(raw))] = struct{}{}
		}
	}

	seen := make(map[string]struct{})
	var out []string
	for _, m := range media {
		if m.embedded {
			continue
		}
		p, ok := cleanCompanionURI(m.rel)
		if !ok {
			p, ok = cleanCompanionURI(m.abs)
		}
		if !ok {
			continue
		}
		if _, isEmbedded := embedded[strings.ToLower(path.Base(p))]; isEmbedded {
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

// fbxBase is the filename part of a path written with either separator —
// FBX stores Windows paths whatever platform wrote them.
func fbxBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
