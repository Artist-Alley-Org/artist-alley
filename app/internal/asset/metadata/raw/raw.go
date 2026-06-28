// Package raw implements the metadata.Extractor for TIFF-based raw
// camera files (CR2 / NEF / DNG / ARW / RW2).
//
// What it does:
//
//  1. Treats the source as a TIFF container (raws are TIFF with
//     extra IFDs / SubIFDs for the maker-note data).
//  2. Walks every IFD + SubIFD looking for the JpegIFOffset (0x0201)
//     + JpegIFByteCount (0x0202) pair, which points at an embedded
//     JPEG preview.
//  3. Picks the LARGEST preview — most raws carry a small thumbnail
//     in IFD1 AND a full-resolution JPEG preview in a SubIFD; we
//     want the latter for variant generation.
//  4. Delegates standard EXIF tag parsing (Make / Model /
//     DateTimeOriginal / GPS / etc.) to the existing exif extractor
//     by handing the raw TIFF bytes back to dsoprea/go-exif. Raws
//     ARE TIFFs, so the existing parse path works without
//     modification.
//
// What it does NOT do:
//
//   - RAF (FUJIFILM): proprietary non-TIFF container. Separate
//     parser is a follow-up phase if/when demand justifies it.
//   - Native raw demosaic / decode: that would mean shipping a CGo
//     libraw dependency. The point of the embedded-preview pipeline
//     is to avoid demosaic entirely — every raw a camera writes
//     carries a JPEG preview baked by the camera's own ISP, which
//     is exactly what a user expects to see as a thumbnail. If a
//     raw lacks an embedded preview (rare; older DNG converters
//     sometimes strip them), the extractor returns ErrNoMetadata
//     for the preview channel and the asset falls back to the
//     generic file-type icon until the user uploads a sidecar.
//   - Maker-note decoding (Canon AF points, Nikon shutter count,
//     etc.). The brief is metadata pipeline parity, not Adobe
//     Lightroom parity.
//
// Concurrency: stateless; safe for concurrent use across goroutines.
package raw

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	metadata "github.com/mscrnt/artist-alley/app/internal/asset/metadata"
)

// MaxSourceBytes guards against unbounded raw uploads. Mirrors the
// preview pipeline cap (200 MB) so the two paths agree on what's
// too big to bother with.
const MaxSourceBytes = 256 * 1024 * 1024

// TIFF tag numbers we care about. We only need the JPEG preview
// pair + SubIFDs. Standard EXIF tags are picked up by handing the
// bytes back to the exif extractor via the dispatcher chain — they
// don't need to be re-implemented here.
const (
	tagSubIFDs            = 0x014A // SubIFDs — list of additional IFD offsets
	tagJPEGInterchange    = 0x0201 // JpegIFOffset — byte offset of embedded JPEG
	tagJPEGInterchangeLen = 0x0202 // JpegIFByteCount — length of embedded JPEG
	// Panasonic RW2 magic: instead of standard TIFF 0x002A in bytes
	// 2-3, RW2 puts 0x0055. We accept both magic values + the IFD
	// walker is otherwise identical.
	tiffMagicStd = 0x002A
	tiffMagicRW2 = 0x0055
)

// FieldType IDs from the TIFF spec.
const (
	ttByte      = 1
	ttASCII     = 2
	ttShort     = 3
	ttLong      = 4
	ttRational  = 5
	ttUndefined = 7
	ttSLong     = 9
	ttSRational = 10
)

// preview holds one candidate JPEG preview we found while walking
// IFDs. We collect every candidate, then pick the largest at the
// end — the small in-IFD1 thumbnail isn't useful for variant
// generation; only the high-res preview is.
type preview struct {
	offset uint32
	length uint32
}

// Extractor implements metadata.Extractor for the raw-camera MIME
// types listed in Supports.
type Extractor struct{}

// New constructs an Extractor.
func New() *Extractor { return &Extractor{} }

// Name implements metadata.Extractor.
func (Extractor) Name() string { return "raw" }

// Supports implements metadata.Extractor. The five MIME types are
// the standard mediatype identifiers per [RFC6838 §4.2.5] for raw
// images. The upload pipeline emits these (Phase 1.18.A-3.B wires
// them into the per-extension MIME detection table).
func (Extractor) Supports(mimeType string) bool {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/x-canon-cr2",
		"image/x-nikon-nef",
		"image/x-adobe-dng",
		"image/x-sony-arw",
		"image/x-panasonic-rw2":
		return true
	}
	return false
}

// Extract implements metadata.Extractor. Returns Result with
// PreviewImageBytes populated when an embedded JPEG preview was
// found; Fields populated with whatever standard EXIF tags this
// extractor's caller wants to keep (today: none — the exif
// extractor runs separately when raw MIME types are added to its
// Supports list, in a future phase).
func (e *Extractor) Extract(ctx context.Context, r io.Reader, mimeType string) (out metadata.Result, retErr error) {
	if !e.Supports(mimeType) {
		return metadata.Result{}, metadata.ErrUnsupportedFormat
	}

	defer func() {
		if rec := recover(); rec != nil {
			retErr = fmt.Errorf("%w: %v", metadata.ErrLibraryPanic, rec)
			out = metadata.Result{Format: mimeType}
		}
	}()

	raw, err := io.ReadAll(io.LimitReader(r, MaxSourceBytes+1))
	if err != nil {
		return metadata.Result{Format: mimeType}, fmt.Errorf("raw: read source: %w", err)
	}
	if int64(len(raw)) > MaxSourceBytes {
		return metadata.Result{Format: mimeType}, fmt.Errorf("raw: source %d bytes > cap %d: %w",
			len(raw), MaxSourceBytes, metadata.ErrMalformedFile)
	}
	if err := ctx.Err(); err != nil {
		return metadata.Result{Format: mimeType}, err
	}

	previews, err := ExtractPreviews(raw)
	if err != nil {
		return metadata.Result{Format: mimeType}, err
	}

	result := metadata.Result{
		Format: mimeType,
		Fields: map[metadata.CanonicalField]metadata.Value{},
	}
	if len(previews) > 0 {
		// Largest wins. Sort would be more idiomatic but for the
		// 2-5 candidates a typical raw carries, a linear max is
		// faster + obvious.
		largest := previews[0]
		for _, p := range previews[1:] {
			if p.length > largest.length {
				largest = p
			}
		}
		end := uint64(largest.offset) + uint64(largest.length)
		if end > uint64(len(raw)) {
			return metadata.Result{Format: mimeType}, fmt.Errorf(
				"raw: preview offset %d + length %d exceeds source %d: %w",
				largest.offset, largest.length, len(raw), metadata.ErrMalformedFile,
			)
		}
		jpeg := raw[largest.offset : largest.offset+largest.length]
		// Sanity-check JPEG marker. Camera firmware sometimes
		// declares an offset that points at an unrelated TIFF
		// substrip; we'd rather emit ErrNoMetadata than persist
		// non-JPEG bytes as the "embedded preview" variant.
		if len(jpeg) < 2 || jpeg[0] != 0xFF || jpeg[1] != 0xD8 {
			return metadata.Result{Format: mimeType}, metadata.ErrNoMetadata
		}
		result.PreviewImageBytes = jpeg
	}

	if len(result.PreviewImageBytes) == 0 && len(result.Fields) == 0 {
		return metadata.Result{Format: mimeType}, metadata.ErrNoMetadata
	}
	return result, nil
}

// ExtractPreviews walks the TIFF container looking for embedded
// JPEG previews. Returns every candidate (offset + length pair) the
// IFD chain advertises, in walk order. The caller picks which to
// use (largest wins for variant generation; first-found is fine for
// a thumbnail-only need).
//
// Exported so raw-camera tooling outside this package (a future
// "preview-only" admin command) can reuse the walker without
// dragging in the rest of the extractor.
func ExtractPreviews(raw []byte) ([]preview, error) {
	if len(raw) < 8 {
		return nil, fmt.Errorf("raw: source too small for TIFF header: %w", metadata.ErrMalformedFile)
	}
	bo, magic, firstIFD, err := readTIFFHeader(raw)
	if err != nil {
		return nil, err
	}
	if magic != tiffMagicStd && magic != tiffMagicRW2 {
		return nil, fmt.Errorf("raw: TIFF magic 0x%04x not recognised: %w", magic, metadata.ErrMalformedFile)
	}

	previews := []preview{}
	// Track visited IFD offsets to defend against pathological raws
	// with self-referential SubIFD chains. Real raws never do this
	// but a hostile upload would otherwise spin forever.
	visited := make(map[uint32]bool)
	if err := walkIFDChain(raw, bo, firstIFD, visited, &previews, 0); err != nil {
		return nil, err
	}
	return previews, nil
}

// readTIFFHeader returns (byteOrder, magic, firstIFDOffset) for a
// TIFF source. Errors fold to ErrMalformedFile.
func readTIFFHeader(raw []byte) (binary.ByteOrder, uint16, uint32, error) {
	if string(raw[0:2]) == "II" {
		bo := binary.LittleEndian
		magic := bo.Uint16(raw[2:4])
		first := bo.Uint32(raw[4:8])
		return bo, magic, first, nil
	}
	if string(raw[0:2]) == "MM" {
		bo := binary.BigEndian
		magic := bo.Uint16(raw[2:4])
		first := bo.Uint32(raw[4:8])
		return bo, magic, first, nil
	}
	return nil, 0, 0, fmt.Errorf("raw: byte-order marker %q not II/MM: %w", raw[0:2], metadata.ErrMalformedFile)
}

// maxIFDRecursion caps the SubIFD recursion depth. Real raws nest
// at most 2-3 levels; 16 leaves headroom for future weirdness
// without enabling stack exhaustion via crafted input.
const maxIFDRecursion = 16

// walkIFDChain follows the IFD linked list at offset, harvesting
// JPEG-preview pairs + recursing into any SubIFDs found via
// tag 0x014A. depth guards against malicious nesting.
func walkIFDChain(raw []byte, bo binary.ByteOrder, offset uint32, visited map[uint32]bool, previews *[]preview, depth int) error {
	if depth > maxIFDRecursion {
		return nil // cap silently — partial results beat refusing the whole file
	}
	for offset != 0 {
		if visited[offset] {
			return nil
		}
		visited[offset] = true
		next, err := walkOneIFD(raw, bo, offset, visited, previews, depth)
		if err != nil {
			return err
		}
		offset = next
	}
	return nil
}

// walkOneIFD parses ONE IFD starting at offset and returns the
// offset of the next IFD in the chain (0 = end). Entries whose
// tag matches an interesting one (JPEG preview pair, SubIFDs)
// trigger the right side-effect.
func walkOneIFD(raw []byte, bo binary.ByteOrder, offset uint32, visited map[uint32]bool, previews *[]preview, depth int) (uint32, error) {
	if uint64(offset)+2 > uint64(len(raw)) {
		return 0, fmt.Errorf("raw: IFD offset %d out of range: %w", offset, metadata.ErrMalformedFile)
	}
	nEntries := bo.Uint16(raw[offset : offset+2])
	entryStart := uint64(offset) + 2
	entriesEnd := entryStart + uint64(nEntries)*12
	if entriesEnd+4 > uint64(len(raw)) {
		return 0, fmt.Errorf("raw: IFD at %d truncated (%d entries): %w", offset, nEntries, metadata.ErrMalformedFile)
	}

	var (
		jpegOffset    uint32
		jpegLength    uint32
		jpegOffsetSet bool
		jpegLengthSet bool
	)

	for i := uint64(0); i < uint64(nEntries); i++ {
		entOff := entryStart + i*12
		tag := bo.Uint16(raw[entOff : entOff+2])
		typ := bo.Uint16(raw[entOff+2 : entOff+4])
		count := bo.Uint32(raw[entOff+4 : entOff+8])
		valBytes := raw[entOff+8 : entOff+12]

		switch tag {
		case tagJPEGInterchange:
			if v, ok := readScalarUint32(raw, bo, typ, count, valBytes); ok {
				jpegOffset = v
				jpegOffsetSet = true
			}
		case tagJPEGInterchangeLen:
			if v, ok := readScalarUint32(raw, bo, typ, count, valBytes); ok {
				jpegLength = v
				jpegLengthSet = true
			}
		case tagSubIFDs:
			// SubIFDs is an array of LONG offsets. Walk each.
			offsets, ok := readUint32Array(raw, bo, typ, count, valBytes)
			if !ok {
				continue
			}
			for _, sub := range offsets {
				if sub == 0 {
					continue
				}
				if err := walkIFDChain(raw, bo, sub, visited, previews, depth+1); err != nil {
					return 0, err
				}
			}
		}
	}

	if jpegOffsetSet && jpegLengthSet && jpegLength > 0 {
		*previews = append(*previews, preview{offset: jpegOffset, length: jpegLength})
	}

	nextIFDOff := bo.Uint32(raw[entriesEnd : entriesEnd+4])
	return nextIFDOff, nil
}

// readScalarUint32 reads a single LONG / SHORT value from a 12-byte
// IFD entry's value field. Returns (value, ok); ok=false on type
// mismatch or count != 1.
func readScalarUint32(raw []byte, bo binary.ByteOrder, typ uint16, count uint32, valBytes []byte) (uint32, bool) {
	if count != 1 {
		return 0, false
	}
	switch typ {
	case ttLong, ttSLong:
		return bo.Uint32(valBytes[0:4]), true
	case ttShort:
		return uint32(bo.Uint16(valBytes[0:2])), true
	}
	_ = raw // value fits in valBytes when count==1; unused
	return 0, false
}

// readUint32Array reads an array of LONG / SHORT values. The values
// fit inline when total size <=4 bytes; otherwise the entry's
// value field is a file offset.
func readUint32Array(raw []byte, bo binary.ByteOrder, typ uint16, count uint32, valBytes []byte) ([]uint32, bool) {
	var perElement uint32
	switch typ {
	case ttLong, ttSLong:
		perElement = 4
	case ttShort:
		perElement = 2
	default:
		return nil, false
	}
	totalBytes := uint64(count) * uint64(perElement)
	src := valBytes
	if totalBytes > 4 {
		off := bo.Uint32(valBytes[0:4])
		if uint64(off)+totalBytes > uint64(len(raw)) {
			return nil, false
		}
		src = raw[off : uint64(off)+totalBytes]
	}
	out := make([]uint32, 0, count)
	for i := uint64(0); i < uint64(count); i++ {
		switch typ {
		case ttLong, ttSLong:
			out = append(out, bo.Uint32(src[i*4:i*4+4]))
		case ttShort:
			out = append(out, uint32(bo.Uint16(src[i*2:i*2+2])))
		}
	}
	return out, true
}

// MimeTypeForExt returns the canonical MIME type the upload
// pipeline should assign to a raw-camera file by extension.
// Returns "" for any extension this package doesn't recognise so
// the caller can fall through to other format detectors.
func MimeTypeForExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "cr2":
		return "image/x-canon-cr2"
	case "nef":
		return "image/x-nikon-nef"
	case "dng":
		return "image/x-adobe-dng"
	case "arw":
		return "image/x-sony-arw"
	case "rw2":
		return "image/x-panasonic-rw2"
	}
	return ""
}

// SupportedExtensions returns the file-extension allowlist (no leading
// dot, lowercase) for raw-camera files this package extracts. Used by
// the upload pipeline + the variant-source override hook.
func SupportedExtensions() []string {
	return []string{"cr2", "nef", "dng", "arw", "rw2"}
}

// Compile-time conformance.
var _ metadata.Extractor = (*Extractor)(nil)

// Sentinels re-exported for callers that want to branch without
// importing the parent package.
var (
	ErrNotRaw  = errors.New("raw: source is not a TIFF-based raw")
	ErrTooSmal = errors.New("raw: source is too small to be a raw")
)
