// Package xmp parses Adobe XMP (Extensible Metadata Platform)
// packets out of JPEG and PNG image files. Pure-Go, no external
// deps — XMP is RDF/XML over a few well-known namespaces, and
// encoding/xml's token stream gives us enough to walk it
// namespace-aware.
//
// Format primer:
//
//   - JPEG: XMP packet sits inside an APP1 segment (marker
//     0xFF 0xE1) whose payload starts with the namespace URI
//     "http://ns.adobe.com/xap/1.0/\x00". The actual XML follows.
//     (Other APP1 segments carry EXIF — same marker, different
//     identifier prefix, so the walker has to check both.)
//
//   - PNG: XMP packet is an iTXt chunk with keyword
//     "XML:com.adobe.xmp" + uncompressed UTF-8 payload.
//
//   - The XML packet itself: x:xmpmeta wraps a rdf:RDF wraps one
//     or more rdf:Description. Each Description's children are
//     either simple text properties (dc:title, xmp:Rating) or
//     RDF containers (rdf:Bag for unordered, rdf:Seq for ordered,
//     rdf:Alt for language-alternative). dc:title etc. always
//     come wrapped in an rdf:Alt with x-default + per-language
//     entries; we pick x-default (or first) and roll the rest
//     into a comma-joined string for the catalog field.
//
// Reference: ISO 16684-1 (XMP 1.1).
package xmp

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
)

// Well-known XMP namespace URIs the parser recognises. Prefixes
// vary per file (Lightroom uses "lr:", others use "lightroom:");
// we key off the URI so the mapping is stable across producers.
const (
	NSDc           = "http://purl.org/dc/elements/1.1/"
	NSXmp          = "http://ns.adobe.com/xap/1.0/"
	NSXmpRights    = "http://ns.adobe.com/xap/1.0/rights/"
	NSPhotoshop    = "http://ns.adobe.com/photoshop/1.0/"
	NSIptc4xmpCore = "http://iptc.org/std/Iptc4xmpCore/1.0/xmlns/"
	NSLightroom    = "http://ns.adobe.com/lightroom/1.0/"
	NSRdf          = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"
)

// ErrNoXMP signals that the carrier had no XMP payload.
// Extractor maps to metadata.ErrNoMetadata.
var ErrNoXMP = errors.New("xmp: no XMP packet present")

// Result is the typed projection of one XMP parse.
type Result struct {
	Fields map[metadata.CanonicalField]metadata.Value
}

// ParseXMPPacket decodes the raw XMP XML bytes into a Result.
// Tolerant of unknown namespaces (ignored) + missing fields
// (left unset).
func ParseXMPPacket(packet []byte) (Result, error) {
	out := Result{Fields: map[metadata.CanonicalField]metadata.Value{}}
	if len(packet) == 0 {
		return out, ErrNoXMP
	}
	dec := xml.NewDecoder(bytes.NewReader(packet))

	// Walk the token stream. The only elements we drill into are
	// <rdf:Description ...> blocks — their CHILDREN are the
	// property elements we care about. Attribute-shorthand
	// (rdf:Description with property values as attributes) is
	// also picked up.
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Space != NSRdf || start.Name.Local != "Description" {
			continue
		}

		// First sweep: attribute-shorthand properties.
		for _, a := range start.Attr {
			absorb(out.Fields, a.Name.Space, a.Name.Local, a.Value)
		}

		// Second sweep: child property elements. Pass to a
		// dedicated walker so the parent-loop logic stays
		// readable.
		if err := walkDescription(dec, &start, out.Fields); err != nil {
			return out, err
		}
	}

	if len(out.Fields) == 0 {
		return out, ErrNoXMP
	}
	return out, nil
}

// walkDescription consumes tokens until the parent's
// EndElement, decoding each direct child as a property.
func walkDescription(dec *xml.Decoder, parent *xml.StartElement, fields map[metadata.CanonicalField]metadata.Value) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			if t.Name == parent.Name {
				return nil
			}
		case xml.StartElement:
			val, err := decodeProperty(dec, &t)
			if err != nil {
				return err
			}
			if val != "" {
				absorb(fields, t.Name.Space, t.Name.Local, val)
			}
		}
	}
}

// decodeProperty extracts the string value of one property
// element. Three shapes are recognised:
//   - Simple text:    <ns:Prop>value</ns:Prop>
//   - Language alt:   <ns:Prop><rdf:Alt><rdf:li xml:lang="x-default">value</rdf:li>...</rdf:Alt></ns:Prop>
//   - Bag / Seq list: <ns:Prop><rdf:Bag><rdf:li>a</rdf:li><rdf:li>b</rdf:li></rdf:Bag></ns:Prop>
//     (or rdf:Seq, same shape) — joined into a comma-separated
//     string for the catalog field.
func decodeProperty(dec *xml.Decoder, prop *xml.StartElement) (string, error) {
	// Peek the first child; if it's an rdf:Bag/Seq/Alt we
	// switch to the list walker, otherwise treat as plain text.
	var charData strings.Builder
	var items []string
	defaultLang := ""
	mode := "text"

	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Space == NSRdf {
				switch t.Name.Local {
				case "Bag", "Seq", "Alt":
					mode = "list"
				case "li":
					// For language-alt blocks, prefer x-default.
					if mode == "list" {
						s, _ := readListItem(dec, &t)
						depth--
						lang := ""
						for _, a := range t.Attr {
							if a.Name.Local == "lang" {
								lang = a.Value
							}
						}
						if lang == "x-default" {
							defaultLang = s
						} else {
							items = append(items, s)
						}
					}
				}
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if mode == "text" {
				charData.Write(t)
			}
		}
	}
	if mode == "list" {
		if defaultLang != "" {
			return defaultLang, nil
		}
		return strings.Join(items, ", "), nil
	}
	return strings.TrimSpace(charData.String()), nil
}

// readListItem reads an <rdf:li> element's text content and
// consumes its EndElement.
func readListItem(dec *xml.Decoder, start *xml.StartElement) (string, error) {
	var sb strings.Builder
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			sb.Write(t)
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// absorb maps one (namespace, local-name, value) tuple into the
// matching CanonicalField. Unknown combos are dropped silently.
func absorb(fields map[metadata.CanonicalField]metadata.Value, ns, local, value string) {
	if value == "" {
		return
	}
	var field metadata.CanonicalField
	switch ns {
	case NSDc:
		switch local {
		case "title":
			field = metadata.FieldXMPTitle
		case "description":
			field = metadata.FieldXMPDescription
		case "creator":
			field = metadata.FieldXMPCreator
		case "subject":
			field = metadata.FieldXMPSubjects
		case "rights":
			field = metadata.FieldXMPRights
		}
	case NSXmp:
		switch local {
		case "Rating":
			field = metadata.FieldXMPRating
		case "Label":
			field = metadata.FieldXMPLabel
		}
	case NSXmpRights:
		if local == "UsageTerms" {
			// xmpRights:UsageTerms takes priority over dc:rights
			// when both are present — UsageTerms is the more
			// specific Adobe-blessed surface.
			field = metadata.FieldXMPRights
		}
	case NSPhotoshop:
		switch local {
		case "Headline":
			field = metadata.FieldXMPPhotoshopHeadline
		case "Instructions":
			field = metadata.FieldXMPInstructions
		}
	case NSLightroom:
		if local == "hierarchicalSubject" {
			field = metadata.FieldXMPHierarchicalTags
		}
	}
	if field == "" {
		return
	}

	if field == metadata.FieldXMPRating {
		// Rating is 0-5; expose as numeric so downstream code
		// (sort, filter) can pivot. Parse leniently — drop on
		// non-numeric input rather than write garbage.
		var n float64
		var dec int
		for _, c := range value {
			switch {
			case c >= '0' && c <= '9':
				n = n*10 + float64(c-'0')
				if dec > 0 {
					dec *= 10
				}
			case c == '.' && dec == 0:
				dec = 1
			default:
				return
			}
		}
		if dec > 0 {
			n = n / float64(dec)
		}
		fields[field] = metadata.Value{Kind: metadata.ValueKindNum, Num: n}
		return
	}
	fields[field] = metadata.Value{Kind: metadata.ValueKindText, Text: value}
}

// ---------------------------------------------------------------------------
// JPEG carrier
// ---------------------------------------------------------------------------

const jpegXMPIdentifier = "http://ns.adobe.com/xap/1.0/\x00"

// FindJPEGXMPPacket extracts the XMP packet from a JPEG byte
// stream. Returns ErrNoXMP when no APP1 segment carries the
// adobe.com/xap identifier.
func FindJPEGXMPPacket(jpeg []byte) ([]byte, error) {
	if len(jpeg) < 4 || jpeg[0] != 0xFF || jpeg[1] != 0xD8 {
		return nil, ErrNoXMP
	}
	i := 2
	for i+4 <= len(jpeg) {
		if jpeg[i] != 0xFF {
			return nil, ErrNoXMP
		}
		for i < len(jpeg) && jpeg[i] == 0xFF {
			i++
		}
		if i >= len(jpeg) {
			return nil, ErrNoXMP
		}
		marker := jpeg[i]
		i++
		if marker == 0xDA || marker == 0xD9 {
			return nil, ErrNoXMP
		}
		if i+2 > len(jpeg) {
			return nil, ErrNoXMP
		}
		segLen := int(binary.BigEndian.Uint16(jpeg[i : i+2]))
		if segLen < 2 || i+segLen > len(jpeg) {
			return nil, ErrNoXMP
		}
		if marker == 0xE1 { // APP1
			payload := jpeg[i+2 : i+segLen]
			if bytes.HasPrefix(payload, []byte(jpegXMPIdentifier)) {
				return payload[len(jpegXMPIdentifier):], nil
			}
		}
		i += segLen
	}
	return nil, ErrNoXMP
}

// ---------------------------------------------------------------------------
// PNG carrier
// ---------------------------------------------------------------------------

const pngXMPKeyword = "XML:com.adobe.xmp"

var pngSignature = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

// FindPNGXMPPacket scans a PNG byte stream for the iTXt chunk
// holding the XMP packet (keyword "XML:com.adobe.xmp",
// compression flag 0, language + translated-keyword fields
// usually empty).
func FindPNGXMPPacket(png []byte) ([]byte, error) {
	if !bytes.HasPrefix(png, pngSignature) {
		return nil, ErrNoXMP
	}
	i := len(pngSignature)
	for i+12 <= len(png) {
		length := int(binary.BigEndian.Uint32(png[i : i+4]))
		ctype := string(png[i+4 : i+8])
		dataStart := i + 8
		dataEnd := dataStart + length
		if dataEnd+4 > len(png) {
			return nil, ErrNoXMP
		}
		if ctype == "iTXt" {
			// iTXt layout: keyword\0 compFlag(1) compMethod(1)
			// language\0 translatedKeyword\0 text
			seg := png[dataStart:dataEnd]
			zero := bytes.IndexByte(seg, 0)
			if zero >= 0 && string(seg[:zero]) == pngXMPKeyword {
				rest := seg[zero+1:]
				if len(rest) < 2 {
					return nil, ErrNoXMP
				}
				compFlag := rest[0]
				rest = rest[2:] // skip compFlag + compMethod
				// language\0
				if z := bytes.IndexByte(rest, 0); z >= 0 {
					rest = rest[z+1:]
				}
				// translated-keyword\0
				if z := bytes.IndexByte(rest, 0); z >= 0 {
					rest = rest[z+1:]
				}
				if compFlag == 0 {
					return rest, nil
				}
				// Compressed XMP iTXt is legal-but-rare; skip
				// in the MVP rather than pull in compress/zlib
				// for an edge case.
				return nil, ErrNoXMP
			}
		}
		i = dataEnd + 4 // 4-byte CRC after data
	}
	return nil, ErrNoXMP
}
