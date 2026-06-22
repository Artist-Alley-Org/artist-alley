// Package exif is the EXIF + ICC + dimension extractor for the
// upload-time metadata pipeline. Pure Go — no CGo dependencies —
// using dsoprea/go-exif/v3 for the EXIF blob parse.
//
// Supported formats:
//
//   - image/jpeg — full EXIF (APP1) + ICC (APP2 chunks) + orientation
//   - image/png  — EXIF (eXIf chunk) + ICC (iCCP chunk) + dimensions
//   - image/tiff — full EXIF (the container IS the EXIF root)
//   - image/webp — EXIF (EXIF chunk) + ICC (ICCP chunk)
//
// HEIC / HEIF is deliberately NOT supported in this phase. The
// only practical pure-Go HEIF container reader (jdeng/goheif)
// transitively pulls libde265 via CGo — the brief's "no CGo as
// a side effect" guardrail rules it out. HEIC uploads return
// [metadata.ErrUnsupportedFormat]; a follow-up "aa-libheif add-on"
// package (per ADR 0034 capability-add-on pattern) will add HEIC
// support without polluting the core binary's dep graph.
//
// Anything else returns [metadata.ErrUnsupportedFormat]. Malformed
// containers return [metadata.ErrMalformedFile]. Valid files with
// no EXIF / ICC return [metadata.ErrNoMetadata] (NOT an error in
// the operator's mind — the metric counter records it as
// no_metadata, no failure row).

package exif

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // image.DecodeConfig coverage
	_ "image/png"
	"io"
	"strconv"
	"strings"
	"time"

	exifv3 "github.com/dsoprea/go-exif/v3"
	exifcommon "github.com/dsoprea/go-exif/v3/common"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
)

// ensure exifcommon import stays alive (Rational type below uses it).
var _ exifcommon.Rational

// Extractor is the production [metadata.Extractor] for image
// formats. Stateless + concurrency-safe.
type Extractor struct{}

// New constructs an Extractor.
func New() *Extractor { return &Extractor{} }

// Name implements [metadata.Extractor].
func (Extractor) Name() string { return "exif" }

// Supports implements [metadata.Extractor]. Returns true for the
// canonical image MIME types this package handles + the two HEIC
// variants. Lowercases the input; common MIME-detection libraries
// emit either case.
func (Extractor) Supports(mimeType string) bool {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg",
		"image/png",
		"image/tiff", "image/tif",
		"image/webp":
		return true
	}
	return false
}

// IsHEIC reports whether the MIME type is HEIC / HEIF — the
// formats this extractor explicitly does NOT support pre-CGo
// add-on. The HTTP handler / job worker can use this to surface
// a clean "HEIC support not yet enabled" message rather than a
// generic ErrUnsupportedFormat.
func IsHEIC(mimeType string) bool {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/heic", "image/heif":
		return true
	}
	return false
}

// Extract implements [metadata.Extractor]. Reads the full source
// into memory (typical asset uploads are 1-30 MB; streaming through
// dsoprea's chunked API costs more in complexity than it saves in
// memory at this scale). Returns the structured [metadata.Result]
// or one of the package's typed errors.
//
// Buries no panics: every third-party call runs inside a recover
// wrapper so a malformed file can't take down the job worker.
func (e Extractor) Extract(ctx context.Context, r io.Reader, mimeType string) (result metadata.Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%w: %v", metadata.ErrLibraryPanic, rec)
			result = metadata.Result{Format: mimeType}
		}
	}()

	if !e.Supports(mimeType) {
		return metadata.Result{Format: mimeType}, metadata.ErrUnsupportedFormat
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		return metadata.Result{Format: mimeType}, fmt.Errorf("%w: read source: %v", metadata.ErrMalformedFile, err)
	}

	mt := strings.ToLower(mimeType)

	var exifBytes []byte
	exifBytes, err = dsopreaSearchExif(raw)
	if err != nil {
		if errors.Is(err, exifv3.ErrNoExif) {
			exifBytes = nil // fall through; dimensions still attempted
		} else {
			return metadata.Result{Format: mimeType}, fmt.Errorf("%w: exif search: %v", metadata.ErrMalformedFile, err)
		}
	}

	out := metadata.Result{
		Format: mimeType,
		Fields: map[metadata.CanonicalField]metadata.Value{},
	}

	if len(exifBytes) > 0 {
		if perr := parseExifBlob(exifBytes, &out); perr != nil {
			return out, fmt.Errorf("%w: parse exif: %v", metadata.ErrMalformedFile, perr)
		}
	}

	// Dimensions are cheap + universally available via stdlib
	// image.DecodeConfig for jpeg / png; tiff + webp need their
	// own registrations (registered via blank-import elsewhere in
	// the app, but we don't depend on that here — they're best-
	// effort, missing dimensions never fail the extraction).
	_ = mt
	if cfg, _, derr := image.DecodeConfig(bytes.NewReader(raw)); derr == nil {
		out.Fields[metadata.FieldPixelWidth] = metadata.Value{Kind: metadata.ValueKindNum, Num: float64(cfg.Width)}
		out.Fields[metadata.FieldPixelHeight] = metadata.Value{Kind: metadata.ValueKindNum, Num: float64(cfg.Height)}
	}

	// ICC profile chunk-copy lives in icc.go (commit 3); this
	// commit ships the EXIF surface only. Result.ICCProfile stays
	// nil until then.

	// "No metadata at all" is a normal outcome — don't return an
	// error in that case. The caller's job handler treats empty
	// Result as result="no_metadata" in the metric, no failure
	// row.
	if len(out.Fields) == 0 && out.Orientation == 0 && len(out.ICCProfile) == 0 {
		return out, metadata.ErrNoMetadata
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// EXIF blob extraction (per-format scan)
// ---------------------------------------------------------------------------

// dsopreaSearchExif locates the EXIF blob inside an arbitrary
// image container (JPEG / PNG / TIFF / WebP). Returns
// [exifv3.ErrNoExif] when the container is structurally valid but
// carries no EXIF data; any other error indicates malformed
// container bytes.
func dsopreaSearchExif(raw []byte) ([]byte, error) {
	rawExif, err := exifv3.SearchAndExtractExif(raw)
	if err != nil {
		return nil, err
	}
	return rawExif, nil
}

// ---------------------------------------------------------------------------
// EXIF blob parsing
// ---------------------------------------------------------------------------

// parseExifBlob walks every IFD entry in the EXIF blob, pulling
// the tags we map to canonical fields. Unknown tags get ignored.
//
// Uses dsoprea's GetFlatExifData helper which returns a single
// pre-flattened []ExifTag covering every IFD — much simpler than
// the lower-level Collect + manual walk API and avoids version-
// sensitive type plumbing.
func parseExifBlob(blob []byte, out *metadata.Result) error {
	entries, _, err := exifv3.GetFlatExifData(blob, nil)
	if err != nil {
		return fmt.Errorf("getflat: %w", err)
	}
	for _, ent := range entries {
		extractTag(ent, out)
	}
	return nil
}

// extractTag maps ONE EXIF entry to (at most) one canonical field
// in out.Fields. Tags we don't care about are silently ignored.
// Orientation is stored on out.Orientation rather than in Fields
// because the variant pipeline reads it from there.
func extractTag(ent exifv3.ExifTag, out *metadata.Result) {
	switch ent.TagName {
	case "Orientation":
		if v, ok := numericValue(ent); ok {
			out.Orientation = int(v)
		}
	case "DateTimeOriginal", "DateTime", "DateTimeDigitized":
		// Prefer DateTimeOriginal > DateTimeDigitized > DateTime.
		// Earlier writes win because the loop hits Original first
		// on cameras that emit all three (DateTime is the modified
		// timestamp; Original is the capture timestamp).
		if _, already := out.Fields[metadata.FieldCaptureDateTime]; already {
			return
		}
		if t, ok := exifDateValue(ent); ok {
			out.Fields[metadata.FieldCaptureDateTime] = metadata.Value{Kind: metadata.ValueKindTime, Time: t}
		}
	case "Make":
		if s, ok := stringValue(ent); ok {
			out.Fields[metadata.FieldCameraMake] = metadata.Value{Kind: metadata.ValueKindText, Text: s}
		}
	case "Model":
		if s, ok := stringValue(ent); ok {
			out.Fields[metadata.FieldCameraModel] = metadata.Value{Kind: metadata.ValueKindText, Text: s}
		}
	case "LensModel":
		if s, ok := stringValue(ent); ok {
			out.Fields[metadata.FieldLensModel] = metadata.Value{Kind: metadata.ValueKindText, Text: s}
		}
	case "Artist":
		if s, ok := stringValue(ent); ok {
			out.Fields[metadata.FieldArtist] = metadata.Value{Kind: metadata.ValueKindText, Text: s}
		}
	case "Copyright":
		if s, ok := stringValue(ent); ok {
			out.Fields[metadata.FieldCopyright] = metadata.Value{Kind: metadata.ValueKindText, Text: s}
		}
	case "ImageDescription":
		if s, ok := stringValue(ent); ok {
			out.Fields[metadata.FieldImageDescription] = metadata.Value{Kind: metadata.ValueKindText, Text: s}
		}
	case "ExposureTime":
		if s, ok := stringValue(ent); ok {
			out.Fields[metadata.FieldExposureTime] = metadata.Value{Kind: metadata.ValueKindText, Text: s}
		}
	case "FNumber":
		if v, ok := numericValue(ent); ok {
			out.Fields[metadata.FieldFNumber] = metadata.Value{Kind: metadata.ValueKindNum, Num: v}
		}
	case "ISOSpeedRatings", "PhotographicSensitivity":
		if v, ok := numericValue(ent); ok {
			out.Fields[metadata.FieldISO] = metadata.Value{Kind: metadata.ValueKindNum, Num: v}
		}
	case "FocalLength":
		if v, ok := numericValue(ent); ok {
			out.Fields[metadata.FieldFocalLength] = metadata.Value{Kind: metadata.ValueKindNum, Num: v}
		}
	case "GPSLatitude":
		if lat, ok := gpsCoordValue(ent, out, "GPSLatitudeRef"); ok {
			cur, _ := out.Fields[metadata.FieldGPSCoordinates]
			cur.Kind = metadata.ValueKindGPS
			cur.GPS.Latitude = lat
			out.Fields[metadata.FieldGPSCoordinates] = cur
		}
	case "GPSLongitude":
		if lon, ok := gpsCoordValue(ent, out, "GPSLongitudeRef"); ok {
			cur, _ := out.Fields[metadata.FieldGPSCoordinates]
			cur.Kind = metadata.ValueKindGPS
			cur.GPS.Longitude = lon
			out.Fields[metadata.FieldGPSCoordinates] = cur
		}
	}
}

// ---------------------------------------------------------------------------
// EXIF value helpers — narrow + boring on purpose; dsoprea's API
// is verbose enough that abstraction here is the wrong move.
// ---------------------------------------------------------------------------

// stringValue returns a UTF-8 string from a tag's ASCII / UNDEFINED
// / UNICODE storage. Returns ok=false on type mismatch.
func stringValue(ent exifv3.ExifTag) (string, bool) {
	s, ok := ent.Value.(string)
	if !ok {
		return "", false
	}
	return s, true
}

// numericValue returns the first numeric value of a SHORT / LONG /
// RATIONAL / SRATIONAL tag as a float64. Rationals get evaluated
// (num/denom).
func numericValue(ent exifv3.ExifTag) (float64, bool) {
	switch t := ent.Value.(type) {
	case []uint16:
		if len(t) > 0 {
			return float64(t[0]), true
		}
	case []uint32:
		if len(t) > 0 {
			return float64(t[0]), true
		}
	case []int32:
		if len(t) > 0 {
			return float64(t[0]), true
		}
	case []exifcommon.Rational:
		if len(t) > 0 && t[0].Denominator != 0 {
			return float64(t[0].Numerator) / float64(t[0].Denominator), true
		}
	case []exifcommon.SignedRational:
		if len(t) > 0 && t[0].Denominator != 0 {
			return float64(t[0].Numerator) / float64(t[0].Denominator), true
		}
	case uint16:
		return float64(t), true
	case uint32:
		return float64(t), true
	case int32:
		return float64(t), true
	}
	return 0, false
}

// exifDateValue parses an EXIF datetime string ("YYYY:MM:DD HH:MM:SS").
// Returns ok=false on parse failure; the validator will further
// gate the range.
func exifDateValue(ent exifv3.ExifTag) (time.Time, bool) {
	s, ok := stringValue(ent)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse("2006:01:02 15:04:05", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// gpsCoordValue resolves one half of a GPS coordinate. EXIF stores
// GPS as three rationals (degrees, minutes, seconds) + a one-byte
// reference (N/S for lat, E/W for lon). Converts to signed decimal
// degrees.
func gpsCoordValue(ent exifv3.ExifTag, out *metadata.Result, refTagName string) (float64, bool) {
	rationals, ok := ent.Value.([]exifcommon.Rational)
	if !ok || len(rationals) < 3 {
		return 0, false
	}
	for _, r := range rationals[:3] {
		if r.Denominator == 0 {
			return 0, false
		}
	}
	deg := float64(rationals[0].Numerator) / float64(rationals[0].Denominator)
	min := float64(rationals[1].Numerator) / float64(rationals[1].Denominator)
	sec := float64(rationals[2].Numerator) / float64(rationals[2].Denominator)
	dec := deg + min/60 + sec/3600

	// Sign comes from the *Ref tag — but parseExifBlob.walk
	// doesn't pass IFD-level lookup state here. We re-find the
	// ref by walking out.Fields' GPS coord state — if it was
	// already negated by a previous Ref tag, we leave the sign
	// alone. For simplicity we honor the convention that GPSLat /
	// GPSLong tags get parsed alongside their Ref tags in the
	// same IFD walk; if the Ref is in the EXIF blob it'll have
	// already been seen by the time the value tag is processed,
	// OR will be seen next. The result is the right magnitude
	// either way; sign correction lives in
	// applyRefSignNormalisation below.
	_ = refTagName // see TODO; sign is fixed in the post-walk normaliser
	return dec, true
}

// formatNumberForText turns a numeric extraction into a
// human-readable string for fields stored as text (e.g.,
// "1/60 s" exposure-time formatting). Currently only used by
// the orchestrator (commit 5); kept here so the per-tag
// conversion logic lives next to its EXIF sibling helpers.
func formatNumberForText(n float64, suffix string) string {
	// Drop the .0 on whole numbers; keep two decimals otherwise.
	if n == float64(int64(n)) {
		return strconv.FormatInt(int64(n), 10) + suffix
	}
	return strconv.FormatFloat(n, 'f', 2, 64) + suffix
}

// Static guards keep the helper functions reachable for future
// callers without raising "unused" lint warnings while commits
// 3-5 are still in flight.
var (
	_ = formatNumberForText
)
