// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// FirstSurvivingPhotograph is the lower datetime bound the
// validator enforces — Niépce's "View from the Window at Le Gras"
// (1826). Any EXIF capture-datetime earlier than this is corrupt /
// out-of-range and gets rejected, no question.
var FirstSurvivingPhotograph = time.Date(1826, 1, 1, 0, 0, 0, 0, time.UTC)

// MaxTextLength is the per-text-field fallback cap when the
// field-definition doesn't supply its own. Chosen to fit a
// reasonable EXIF/IPTC string + leave headroom for unicode
// expansion under VARCHAR storage assumptions.
const MaxTextLength = 4096

// ValidateValue runs the value through the universal validator
// rules from decision 8 in the brief. Returns the (possibly
// normalized) value + an error if rejected; on rejection the
// caller writes an extraction_failure row + skips the apply.
//
// Validation is type-driven by Value.Kind — text rules don't run
// against numeric values, etc. The maxLength parameter lets the
// caller pass the field-definition's per-field cap; pass 0 to
// use [MaxTextLength].
func ValidateValue(v Value, maxLength int) (Value, error) {
	switch v.Kind {
	case ValueKindText:
		return validateText(v, maxLength)
	case ValueKindNum:
		return v, nil // numeric range checks happen per-field, not universally
	case ValueKindTime:
		return validateTime(v)
	case ValueKindGPS:
		return validateGPS(v)
	}
	return v, fmt.Errorf("metadata.validate: unknown value kind %d", v.Kind)
}

// ValidateGPSLat reports whether lat is in the planet's lat range.
func ValidateGPSLat(lat float64) bool { return lat >= -90 && lat <= 90 }

// ValidateGPSLon reports whether lon is in the planet's lon range.
func ValidateGPSLon(lon float64) bool { return lon >= -180 && lon <= 180 }

func validateText(v Value, maxLength int) (Value, error) {
	if !utf8.ValidString(v.Text) {
		return v, fmt.Errorf("metadata.validate: text is not valid UTF-8")
	}
	// Trim FIRST so trailing nulls / spaces (Canon + Sony firmware
	// artifacts) don't trip the control-character check below.
	// Embedded control chars still get rejected on the second pass.
	v.Text = strings.TrimRight(v.Text, " \t\r\n\x00")
	if v.Text == "" {
		return v, fmt.Errorf("metadata.validate: text is empty after trim")
	}
	cap := maxLength
	if cap <= 0 {
		cap = MaxTextLength
	}
	if len(v.Text) > cap {
		return v, fmt.Errorf("metadata.validate: text length %d exceeds cap %d", len(v.Text), cap)
	}
	// Reject control characters except tab and newline embedded
	// MID-string. EXIF / IPTC strings occasionally carry null
	// markers from corrupted firmware; storing them risks
	// downstream rendering / search-index corruption.
	for _, r := range v.Text {
		if r == '\t' || r == '\n' {
			continue
		}
		if unicode.IsControl(r) {
			return v, fmt.Errorf("metadata.validate: text contains control character 0x%02x", r)
		}
	}
	return v, nil
}

func validateTime(v Value) (Value, error) {
	if v.Time.IsZero() {
		return v, fmt.Errorf("metadata.validate: datetime is zero value")
	}
	if v.Time.Before(FirstSurvivingPhotograph) {
		return v, fmt.Errorf("metadata.validate: datetime %v predates first photograph (1826)", v.Time)
	}
	// 24-hour future tolerance — covers timezone-mismatched
	// camera clocks (camera set to local time, server UTC) while
	// still rejecting "year 2099 buffer-overflow" garbage.
	upper := time.Now().UTC().Add(24 * time.Hour)
	if v.Time.After(upper) {
		return v, fmt.Errorf("metadata.validate: datetime %v is in the future (beyond 24h tolerance)", v.Time)
	}
	return v, nil
}

func validateGPS(v Value) (Value, error) {
	if !ValidateGPSLat(v.GPS.Latitude) {
		return v, fmt.Errorf("metadata.validate: latitude %v outside [-90, 90]", v.GPS.Latitude)
	}
	if !ValidateGPSLon(v.GPS.Longitude) {
		return v, fmt.Errorf("metadata.validate: longitude %v outside [-180, 180]", v.GPS.Longitude)
	}
	// Reject the "null island" (0, 0) coordinate — historically
	// the GPS receiver's "no fix" output. Real photos at exactly
	// the equator+meridian intersection don't exist outside test
	// fixtures; if one does the operator can mint a custom field
	// and write it manually.
	if v.GPS.Latitude == 0 && v.GPS.Longitude == 0 {
		return v, fmt.Errorf("metadata.validate: GPS (0, 0) is the 'no fix' sentinel, not a real location")
	}
	return v, nil
}

// NormalizeCameraMakeModel collapses redundant make+model
// strings ("Canon Canon EOS 5D" → "Canon EOS 5D") that some
// firmware emits. Idempotent: a clean string passes through
// unchanged.
func NormalizeCameraMakeModel(make_, model string) string {
	make_ = strings.TrimSpace(make_)
	model = strings.TrimSpace(model)
	if make_ == "" {
		return model
	}
	if model == "" {
		return make_
	}
	// Same make + model strings (firmware bug emitting the make
	// in both slots) collapse to the make alone.
	if strings.EqualFold(make_, model) {
		return make_
	}
	// Strip duplicate make prefix from model. Case-insensitive
	// because Sony writes "Sony" + "ILCE-7M3" but some lenses
	// emit "SONY" + "Sony FE 24-70mm".
	if strings.HasPrefix(strings.ToLower(model), strings.ToLower(make_)+" ") {
		model = model[len(make_)+1:]
	}
	return make_ + " " + model
}
