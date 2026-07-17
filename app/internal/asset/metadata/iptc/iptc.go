// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package iptc parses IPTC IIM (Information Interchange Model)
// datasets out of JPEG and TIFF image files. Pure-Go, no
// external deps — IIM is a fixed-shape binary format and the
// JPEG/TIFF carriers are well-known.
//
// Format primer:
//
//   - JPEG: IPTC datasets are wrapped in an "8BIM" image-resource
//     block with resource id 0x0404, which lives inside an APP13
//     segment (marker 0xFF 0xED) whose payload starts with the
//     ASCII tag "Photoshop 3.0\x00".
//
//   - TIFF: IPTC bytes are stored under tag 33723 (0x83BB,
//     "Photoshop"). Same 8BIM container, same 0x0404 lookup. The
//     dispatcher in the iptc package's Extractor reuses the
//     8BIM walker for both carriers.
//
//   - Inside the 0x0404 block: a sequence of records, each:
//     0x1C    record marker (one byte; always 0x1C)
//     rr      record number (1 byte)
//     dd      dataset number (1 byte)
//     len     2-byte big-endian payload length
//     payload `len` bytes
//     Extended records (high bit of the first length byte set)
//     are extremely rare in practice; the parser skips them
//     gracefully rather than crashing.
//
//   - Dataset 1:90 (CodedCharacterSet) declares the text
//     encoding for every subsequent string field. "\x1B%G"
//     means UTF-8; anything else (or absent) is treated as
//     ISO 8859-1 (Latin-1) per the spec.
//
// Reference: IPTC IIM 4.2, §A — "Information Interchange Model
// Subject Reference System".
package iptc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
)

// Datasets in the Application Record (record number 2). Selected
// by what Lightroom / Photoshop / camera firmwares commonly write
// + what artist-alley's catalog actually surfaces; the full IIM
// set is much larger.
const (
	dsCodedCharacterSet uint8 = 90 // record 1; declares encoding

	dsObjectName      uint8 = 5   // 2:5
	dsKeywords        uint8 = 25  // 2:25 (repeatable; we join with ", ")
	dsInstructions    uint8 = 40  // 2:40
	dsByline          uint8 = 80  // 2:80
	dsBylineTitle     uint8 = 85  // 2:85
	dsCity            uint8 = 90  // 2:90
	dsProvinceState   uint8 = 95  // 2:95
	dsCountry         uint8 = 101 // 2:101
	dsHeadline        uint8 = 105 // 2:105
	dsCredit          uint8 = 110 // 2:110
	dsSource          uint8 = 115 // 2:115
	dsCopyrightNotice uint8 = 116 // 2:116
	dsCaptionAbstract uint8 = 120 // 2:120
)

// Result is the typed projection of one IPTC parse. Empty fields
// mean "no value present" — the file may carry IPTC for some
// datasets but not others.
type Result struct {
	Fields map[metadata.CanonicalField]metadata.Value
}

// ParseIPTCBlob parses the inner IPTC payload (i.e. the bytes
// of the 8BIM resource id=0x0404 block, NOT the surrounding
// Photoshop image-resource container). Returns a populated
// Result with one entry per recognised dataset.
func ParseIPTCBlob(payload []byte) (Result, error) {
	out := Result{Fields: map[metadata.CanonicalField]metadata.Value{}}
	if len(payload) == 0 {
		return out, ErrNoIPTC
	}

	// First pass: find dataset 1:90 to determine encoding.
	utf8 := false
	walkDatasets(payload, func(rec, ds uint8, data []byte) {
		if rec == 1 && ds == dsCodedCharacterSet {
			if bytes.HasPrefix(data, []byte("\x1b%G")) {
				utf8 = true
			}
		}
	})

	// Keywords (2:25) is repeatable; accumulate all instances.
	var keywords []string

	walkDatasets(payload, func(rec, ds uint8, data []byte) {
		if rec != 2 {
			return
		}
		s := decodeString(data, utf8)
		if s == "" {
			return
		}
		switch ds {
		case dsKeywords:
			keywords = append(keywords, s)
		case dsObjectName:
			setText(out.Fields, metadata.FieldIPTCObjectName, s)
		case dsInstructions:
			setText(out.Fields, metadata.FieldIPTCInstructions, s)
		case dsByline:
			setText(out.Fields, metadata.FieldIPTCByline, s)
		case dsBylineTitle:
			setText(out.Fields, metadata.FieldIPTCBylineTitle, s)
		case dsCity:
			setText(out.Fields, metadata.FieldIPTCCity, s)
		case dsProvinceState:
			setText(out.Fields, metadata.FieldIPTCState, s)
		case dsCountry:
			setText(out.Fields, metadata.FieldIPTCCountry, s)
		case dsHeadline:
			setText(out.Fields, metadata.FieldIPTCHeadline, s)
		case dsCredit:
			setText(out.Fields, metadata.FieldIPTCCredit, s)
		case dsSource:
			setText(out.Fields, metadata.FieldIPTCSource, s)
		case dsCopyrightNotice:
			setText(out.Fields, metadata.FieldIPTCCopyright, s)
		case dsCaptionAbstract:
			setText(out.Fields, metadata.FieldIPTCCaption, s)
		}
	})

	if len(keywords) > 0 {
		setText(out.Fields, metadata.FieldIPTCKeywords, strings.Join(keywords, ", "))
	}

	if len(out.Fields) == 0 {
		return out, ErrNoIPTC
	}
	return out, nil
}

// walkDatasets iterates the (record, dataset, payload) tuples in
// an IPTC IIM stream. Tolerant of trailing garbage + extended
// records (skipped); the only fatal stop is running out of bytes
// mid-header.
func walkDatasets(b []byte, fn func(record, dataset uint8, payload []byte)) {
	for i := 0; i+5 <= len(b); {
		if b[i] != 0x1C {
			// Records are always 0x1C-prefixed; skip slack.
			i++
			continue
		}
		rec := b[i+1]
		ds := b[i+2]
		// Length: 2-byte big-endian when the high bit of byte[3]
		// is clear; otherwise the low 15 bits of bytes[3..4] are
		// the count of bytes that follow with the actual length
		// (extended dataset). The extended form is rare enough
		// that we skip gracefully rather than implement it.
		if b[i+3]&0x80 != 0 {
			extCntLen := int(binary.BigEndian.Uint16(b[i+3:i+5]) & 0x7FFF)
			i += 5 + extCntLen
			continue
		}
		length := int(binary.BigEndian.Uint16(b[i+3 : i+5]))
		payloadStart := i + 5
		payloadEnd := payloadStart + length
		if payloadEnd > len(b) {
			// Truncated record — bail rather than panic.
			return
		}
		fn(rec, ds, b[payloadStart:payloadEnd])
		i = payloadEnd
	}
}

// decodeString turns raw IPTC bytes into a UTF-8 Go string.
// Trims trailing nulls/whitespace (firmware artifacts). When
// utf8=false the source is treated as Latin-1; each byte
// becomes its Unicode code point.
func decodeString(raw []byte, utf8 bool) string {
	raw = bytes.TrimRight(raw, "\x00 \t\r\n")
	if len(raw) == 0 {
		return ""
	}
	if utf8 {
		return string(raw)
	}
	// Latin-1 → UTF-8 by code-point lift.
	var sb strings.Builder
	sb.Grow(len(raw))
	for _, c := range raw {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

func setText(m map[metadata.CanonicalField]metadata.Value, f metadata.CanonicalField, s string) {
	m[f] = metadata.Value{Kind: metadata.ValueKindText, Text: s}
}

// ErrNoIPTC signals that the carrier had no IPTC payload. The
// extractor maps this to metadata.ErrNoMetadata so the job
// handler records no_metadata (not a failure).
var ErrNoIPTC = errors.New("iptc: no IPTC datasets present")

// ---------------------------------------------------------------------------
// JPEG carrier
// ---------------------------------------------------------------------------

// FindJPEGIPTCBlob extracts the IPTC payload from a JPEG byte
// stream. Returns ErrNoIPTC when no APP13 segment carries the
// "Photoshop 3.0" tag, or when the 8BIM walker can't find a
// 0x0404 resource block. Defensive against truncated /
// malformed APP13 segments — bails to ErrNoIPTC rather than
// panicking.
func FindJPEGIPTCBlob(jpeg []byte) ([]byte, error) {
	app13, err := findJPEGAPP13(jpeg)
	if err != nil {
		return nil, err
	}
	return findIPTCIn8BIM(app13)
}

// findJPEGAPP13 walks the JPEG marker stream + returns the
// APP13 (FF ED) segment payload whose first bytes are the
// "Photoshop 3.0\x00" identifier. The IPTC chunk sits inside.
func findJPEGAPP13(jpeg []byte) ([]byte, error) {
	if len(jpeg) < 4 || jpeg[0] != 0xFF || jpeg[1] != 0xD8 {
		return nil, ErrNoIPTC // not a JPEG → no IPTC for us
	}
	i := 2
	for i+4 <= len(jpeg) {
		if jpeg[i] != 0xFF {
			return nil, ErrNoIPTC
		}
		// Skip fill bytes (rare but legal).
		for i < len(jpeg) && jpeg[i] == 0xFF {
			i++
		}
		if i >= len(jpeg) {
			return nil, ErrNoIPTC
		}
		marker := jpeg[i]
		i++
		// SOS marker (0xDA) is followed by the entropy-coded
		// stream; APP segments (if present) live before it.
		if marker == 0xDA || marker == 0xD9 {
			return nil, ErrNoIPTC
		}
		if i+2 > len(jpeg) {
			return nil, ErrNoIPTC
		}
		segLen := int(binary.BigEndian.Uint16(jpeg[i : i+2]))
		if segLen < 2 || i+segLen > len(jpeg) {
			return nil, ErrNoIPTC
		}
		if marker == 0xED { // APP13
			payload := jpeg[i+2 : i+segLen]
			if bytes.HasPrefix(payload, []byte("Photoshop 3.0\x00")) {
				return payload[14:], nil
			}
		}
		i += segLen
	}
	return nil, ErrNoIPTC
}

// findIPTCIn8BIM walks the Photoshop 8BIM image-resource blocks
// looking for resource id 0x0404 (the IPTC IIM container).
func findIPTCIn8BIM(b []byte) ([]byte, error) {
	for i := 0; i+12 <= len(b); {
		if !bytes.HasPrefix(b[i:], []byte("8BIM")) {
			return nil, ErrNoIPTC
		}
		resID := binary.BigEndian.Uint16(b[i+4 : i+6])
		// Pascal-string name (padded to even total length
		// including the length byte). Skip past it.
		nameLen := int(b[i+6])
		nameTotal := 1 + nameLen
		if nameTotal%2 != 0 {
			nameTotal++
		}
		nameEnd := i + 6 + nameTotal
		if nameEnd+4 > len(b) {
			return nil, ErrNoIPTC
		}
		dataLen := int(binary.BigEndian.Uint32(b[nameEnd : nameEnd+4]))
		dataStart := nameEnd + 4
		dataEnd := dataStart + dataLen
		if dataEnd > len(b) {
			return nil, ErrNoIPTC
		}
		if resID == 0x0404 {
			return b[dataStart:dataEnd], nil
		}
		// Data segment is also padded to even.
		nextI := dataEnd
		if dataLen%2 != 0 {
			nextI++
		}
		i = nextI
	}
	return nil, ErrNoIPTC
}
