package metadata

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
)

// Extractor pulls technical metadata out of an image file's bytes.
// One implementation per file-format family; the dispatcher in
// [ExtractorRegistry] picks the right one based on MIME type.
//
// Extractors MUST NOT panic — wrap risky third-party library
// calls in defer/recover and return [ErrLibraryPanic] instead.
// The job handler relies on this contract for failure isolation.
//
// Implementations are stateless + safe for concurrent use across
// many goroutines.
type Extractor interface {
	// Name returns the stable identifier recorded in
	// extraction_failure rows (e.g. "exif", "icc").
	Name() string

	// Supports reports whether this extractor can read the given
	// MIME type. The dispatcher walks registered extractors in
	// registration order; the first Supports==true wins.
	Supports(mimeType string) bool

	// Extract reads the source bytes and returns the structured
	// Result. Returns a typed error from the sentinel set so the
	// caller can classify.
	Extract(ctx context.Context, r io.Reader, mimeType string) (Result, error)
}

// Result is the typed projection of everything one extraction
// pass produced. Field values are stored as typed Values rather
// than raw strings so the applier can route them to the right
// field-value column (text / num / date / options / ref).
//
// Empty fields are NORMAL — most images don't carry GPS, fewer
// carry contact info, fewer still carry licence URLs. An empty
// Result is not an error.
type Result struct {
	// Format is the canonical name of the detected container
	// (e.g. "image/jpeg", "image/png"). Echoed for the audit row.
	Format string

	// Fields maps canonical extraction-field keys (see
	// [CanonicalField]) to their extracted Value. Validator runs
	// each Value before the applier writes it.
	Fields map[CanonicalField]Value

	// ICCProfile is the source's colour profile bytes if present.
	// Variant pipeline writes these byte-for-byte into every
	// derived variant so print colour stays accurate.
	ICCProfile []byte

	// Orientation is the EXIF orientation tag (1-8, or 0 if
	// absent). Variant pipeline reads this to rotate the variant
	// at encode time; source bytes are never modified.
	Orientation int
}

// CanonicalField is the stable string the extractor + applier +
// validator all agree on. Operators map these to field-definition
// IDs via field_definition.extraction_source + .extraction_canonical.
type CanonicalField string

const (
	FieldCaptureDateTime    CanonicalField = "capture_datetime"
	FieldCameraMake         CanonicalField = "camera_make"
	FieldCameraModel        CanonicalField = "camera_model"
	FieldCameraMakeAndModel CanonicalField = "camera_make_model"
	FieldLensModel          CanonicalField = "lens_model"
	FieldGPSLatitude        CanonicalField = "gps_latitude"
	FieldGPSLongitude       CanonicalField = "gps_longitude"
	FieldGPSCoordinates     CanonicalField = "gps_coordinates"
	FieldExposureTime       CanonicalField = "exposure_time"
	FieldFNumber            CanonicalField = "f_number"
	FieldISO                CanonicalField = "iso"
	FieldFocalLength        CanonicalField = "focal_length"
	FieldArtist             CanonicalField = "artist"
	FieldCopyright          CanonicalField = "copyright"
	FieldImageDescription   CanonicalField = "image_description"
	FieldOrientation        CanonicalField = "orientation"
	FieldPixelWidth         CanonicalField = "pixel_width"
	FieldPixelHeight        CanonicalField = "pixel_height"

	// --- Phase 1.18.A-3: IPTC IIM (legacy news-photo metadata) ---
	// IPTC fields embedded in JPEG/TIFF Photoshop image-resource
	// segments. Extracted by the iptc package. Where IPTC and EXIF
	// disagree on the same semantic field (e.g. capture date, artist)
	// the per-field extraction config picks which wins; both write
	// with their own SetBy provenance.
	FieldIPTCKeywords     CanonicalField = "iptc_keywords"      // dataset 2:25 — comma-joined free-tag set
	FieldIPTCByline       CanonicalField = "iptc_byline"        // 2:80 — primary photographer
	FieldIPTCBylineTitle  CanonicalField = "iptc_byline_title"  // 2:85
	FieldIPTCCaption      CanonicalField = "iptc_caption"       // 2:120 — long-form abstract
	FieldIPTCHeadline     CanonicalField = "iptc_headline"      // 2:105 — short summary
	FieldIPTCCredit       CanonicalField = "iptc_credit"        // 2:110 — agency / publication
	FieldIPTCSource       CanonicalField = "iptc_source"        // 2:115 — original supplier
	FieldIPTCCity         CanonicalField = "iptc_city"          // 2:90
	FieldIPTCState        CanonicalField = "iptc_state"         // 2:95 — province / state
	FieldIPTCCountry      CanonicalField = "iptc_country"       // 2:101
	FieldIPTCObjectName   CanonicalField = "iptc_object_name"   // 2:5  — title-of-work
	FieldIPTCInstructions CanonicalField = "iptc_instructions"  // 2:40 — editor's notes
	FieldIPTCCopyright    CanonicalField = "iptc_copyright"     // 2:116 — distinct from EXIF copyright

	// --- Phase 1.18.A-3: XMP (Adobe XML metadata packet) ---
	// XMP namespaces we pull values from: dc (Dublin Core),
	// photoshop, xmpRights, Iptc4xmpCore, lr (Lightroom hierarchy).
	// Operators map these to their preferred field-definitions via
	// the existing extraction_config picker.
	FieldXMPTitle             CanonicalField = "xmp_title"              // dc:title (rdf:Alt — default lang wins)
	FieldXMPDescription       CanonicalField = "xmp_description"        // dc:description
	FieldXMPCreator           CanonicalField = "xmp_creator"            // dc:creator (rdf:Seq — joined)
	FieldXMPSubjects          CanonicalField = "xmp_subjects"           // dc:subject (rdf:Bag — keyword set)
	FieldXMPRights            CanonicalField = "xmp_rights"             // dc:rights / xmpRights:UsageTerms
	FieldXMPHierarchicalTags  CanonicalField = "xmp_hierarchical_tags"  // lr:hierarchicalSubject ("People|Family|...")
	FieldXMPRating            CanonicalField = "xmp_rating"             // xmp:Rating (0-5)
	FieldXMPLabel             CanonicalField = "xmp_label"              // xmp:Label ("Red"/"Yellow"/etc.)
	FieldXMPPhotoshopHeadline CanonicalField = "xmp_photoshop_headline" // photoshop:Headline
	FieldXMPInstructions      CanonicalField = "xmp_instructions"       // photoshop:Instructions
)

// Value is the typed extracted value. Exactly ONE of the fields
// is populated; the Kind discriminator tells the applier which.
type Value struct {
	Kind ValueKind

	// One of the following is populated based on Kind:
	Text   string
	Num    float64
	Time   time.Time
	GPS    GPSCoord // when Kind == ValueKindGPS
}

// ValueKind tags the union shape of [Value].
type ValueKind int

const (
	ValueKindText ValueKind = iota
	ValueKindNum
	ValueKindTime
	ValueKindGPS
)

// GPSCoord is a normalised lat/lon pair in decimal degrees.
// Validators reject coordinates outside the planet.
type GPSCoord struct {
	Latitude  float64
	Longitude float64
}

// ---------------------------------------------------------------------------
// Extraction config (operator-owned)
// ---------------------------------------------------------------------------

// FieldExtractionConfig describes how the operator wants ONE
// field-definition populated from extraction. Stored in
// field_definition.extraction_source + .extraction_mode columns.
type FieldExtractionConfig struct {
	FieldID  uuid.UUID

	// Source names the canonical extraction field this
	// field-definition receives values from. Empty = no
	// extraction (the field stays operator-managed).
	Source CanonicalField

	// Mode controls write behaviour:
	//   - "skip_if_set": only write when the asset's field is empty
	//   - "replace":     overwrite any existing value
	//   - "append":      for multi-value fields, add to the set
	//   - "prepend":     for ordered text, prepend with separator
	//
	// Default (when row exists with empty mode) is "skip_if_set"
	// — the conservative choice that never clobbers operator work.
	Mode ExtractionMode
}

// ExtractionMode is the enum stored in
// field_definition.extraction_mode.
type ExtractionMode string

const (
	ExtractionModeSkipIfSet ExtractionMode = "skip_if_set"
	ExtractionModeReplace   ExtractionMode = "replace"
	ExtractionModeAppend    ExtractionMode = "append"
	ExtractionModePrepend   ExtractionMode = "prepend"
)

// ValidMode reports whether m is one of the four supported values.
func (m ExtractionMode) ValidMode() bool {
	switch m {
	case ExtractionModeSkipIfSet, ExtractionModeReplace,
		ExtractionModeAppend, ExtractionModePrepend:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Error sentinels
// ---------------------------------------------------------------------------

// ErrUnsupportedFormat is returned by [Extractor.Extract] when
// the implementation doesn't speak the source's MIME type, and
// by the dispatcher when no registered extractor claims the
// format. Job handler records an extraction_failure row with
// error_kind="unsupported_format" and moves on — asset stays
// uploaded.
var ErrUnsupportedFormat = errors.New("metadata: extractor does not support this format")

// ErrMalformedFile is returned when the file's container shape
// is invalid (truncated JPEG, broken PNG CRC, bad TIFF IFD chain).
// extraction_failure error_kind="malformed_file"; asset stays
// uploaded so the user can re-upload.
var ErrMalformedFile = errors.New("metadata: source file is malformed")

// ErrNoMetadata is returned when the file is valid but contains
// no extractable metadata (a no-EXIF JPEG, a stock-photo PNG with
// no iCCP). Recorded as result="no_metadata" in the metrics
// counter; NO extraction_failure row — this is the normal case
// for many uploads.
var ErrNoMetadata = errors.New("metadata: source contains no extractable metadata")

// ErrLibraryPanic wraps a recovered panic from a third-party
// library. The job handler's defer/recover converts panic→error;
// failure_record error_kind="library_panic".
var ErrLibraryPanic = errors.New("metadata: extraction library panicked")

// ---------------------------------------------------------------------------
// Applier + asset-side handles
// ---------------------------------------------------------------------------

// AssetRef is the minimal projection of the asset row the applier
// needs. Kept narrow so tests can construct without spinning up
// the full asset handler.
type AssetRef struct {
	ID            uuid.UUID
	OwnerUserRef  *int64
	OwningTeamID  *uuid.UUID
	FileHash      string
	MimeType      string
}

// Applier writes extracted values into the field-value system.
// Production impl reads each field's current value, checks
// equal-value (no-op convergence), and routes to the right
// field-value column via the existing field-value handler.
type Applier interface {
	// Apply runs one extraction Result against one asset's
	// fields. Returns a summary of what changed; the caller logs
	// it as ONE audit row (per [decision 12] in the brief).
	Apply(ctx context.Context, asset AssetRef, result Result) (ApplySummary, error)
}

// ApplySummary describes the outcome of one Apply call. Stored
// as the audit-row payload; surfaced to the operator at
// /admin/extraction-failures alongside the failure rows.
type ApplySummary struct {
	FieldsSet            []CanonicalField
	FieldsSkippedNoChange []CanonicalField // equal-value short-circuit fired
	FieldsSkippedMode    []CanonicalField  // skip_if_set hit a populated field
	FieldsSkippedValid   []CanonicalField  // validator rejected
	FailureRows          []FailureRecord
}

// FailureRecord is one validator-rejected (or extraction-failed)
// field. Persisted to extraction_failure table.
type FailureRecord struct {
	Format    string
	ErrorKind string // "unsupported_format" | "malformed_file" | "library_panic" | "validation"
	Message   string
	Field     CanonicalField // empty for whole-file errors
	RawValue  any            // operator-displayable; JSONB column
}
