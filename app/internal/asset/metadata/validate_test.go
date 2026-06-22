// Validator unit tests. Pure functions — no DB / network.

package metadata

import (
	"strings"
	"testing"
	"time"
)

func TestValidateText_HappyPath(t *testing.T) {
	v := Value{Kind: ValueKindText, Text: "Canon EOS 5D Mark IV"}
	got, err := ValidateValue(v, 0)
	if err != nil {
		t.Fatalf("happy path rejected: %v", err)
	}
	if got.Text != v.Text {
		t.Errorf("text mutated: %q vs %q", got.Text, v.Text)
	}
}

func TestValidateText_InvalidUTF8_Rejected(t *testing.T) {
	v := Value{Kind: ValueKindText, Text: string([]byte{0xff, 0xfe, 0xfd})}
	if _, err := ValidateValue(v, 0); err == nil {
		t.Errorf("invalid UTF-8 should be rejected")
	}
}

func TestValidateText_ControlChar_Rejected(t *testing.T) {
	v := Value{Kind: ValueKindText, Text: "Canon\x00EOS"}
	if _, err := ValidateValue(v, 0); err == nil {
		t.Errorf("embedded null should be rejected")
	}
}

func TestValidateText_TabAndNewlinePermitted(t *testing.T) {
	v := Value{Kind: ValueKindText, Text: "line1\nline2\tcol2"}
	if _, err := ValidateValue(v, 0); err != nil {
		t.Errorf("tab/newline rejected, should be allowed: %v", err)
	}
}

func TestValidateText_TrailingWhitespaceTrimmed(t *testing.T) {
	v := Value{Kind: ValueKindText, Text: "Canon EOS \x00\x00 "}
	got, err := ValidateValue(v, 0)
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if got.Text != "Canon EOS" {
		t.Errorf("trim failed: got %q want %q", got.Text, "Canon EOS")
	}
}

func TestValidateText_EmptyAfterTrim_Rejected(t *testing.T) {
	v := Value{Kind: ValueKindText, Text: "   \x00\x00   "}
	if _, err := ValidateValue(v, 0); err == nil {
		t.Errorf("post-trim empty should be rejected")
	}
}

func TestValidateText_LengthCapEnforced(t *testing.T) {
	v := Value{Kind: ValueKindText, Text: strings.Repeat("a", MaxTextLength+1)}
	if _, err := ValidateValue(v, 0); err == nil {
		t.Errorf("over-cap text should be rejected")
	}
}

func TestValidateText_LengthCustomCap(t *testing.T) {
	v := Value{Kind: ValueKindText, Text: strings.Repeat("a", 100)}
	if _, err := ValidateValue(v, 50); err == nil {
		t.Errorf("text over custom cap should be rejected")
	}
	if _, err := ValidateValue(v, 200); err != nil {
		t.Errorf("text under custom cap rejected: %v", err)
	}
}

func TestValidateText_UnicodeKanjiRoundTrips(t *testing.T) {
	v := Value{Kind: ValueKindText, Text: "撮影者: 山田太郎"}
	got, err := ValidateValue(v, 0)
	if err != nil {
		t.Fatalf("kanji rejected: %v", err)
	}
	if got.Text != v.Text {
		t.Errorf("kanji mutated: %q vs %q", got.Text, v.Text)
	}
}

func TestValidateTime_ZeroRejected(t *testing.T) {
	v := Value{Kind: ValueKindTime, Time: time.Time{}}
	if _, err := ValidateValue(v, 0); err == nil {
		t.Errorf("zero time should be rejected")
	}
}

func TestValidateTime_BeforeFirstPhotograph_Rejected(t *testing.T) {
	v := Value{Kind: ValueKindTime, Time: time.Date(1820, 5, 1, 0, 0, 0, 0, time.UTC)}
	if _, err := ValidateValue(v, 0); err == nil {
		t.Errorf("1820 datetime should be rejected (predates photography)")
	}
}

func TestValidateTime_FarFuture_Rejected(t *testing.T) {
	v := Value{Kind: ValueKindTime, Time: time.Now().Add(7 * 24 * time.Hour)}
	if _, err := ValidateValue(v, 0); err == nil {
		t.Errorf("week-in-future datetime should be rejected")
	}
}

func TestValidateTime_Recent_Accepted(t *testing.T) {
	v := Value{Kind: ValueKindTime, Time: time.Now().Add(-365 * 24 * time.Hour)}
	if _, err := ValidateValue(v, 0); err != nil {
		t.Errorf("year-ago datetime rejected: %v", err)
	}
}

func TestValidateTime_24hToleranceAccepted(t *testing.T) {
	// Camera with timezone-mismatched clock could be up to ~24h
	// ahead of UTC. Accept these as the tolerance per validator.
	v := Value{Kind: ValueKindTime, Time: time.Now().Add(12 * time.Hour)}
	if _, err := ValidateValue(v, 0); err != nil {
		t.Errorf("12h-future datetime rejected (should be within 24h tolerance): %v", err)
	}
}

func TestValidateGPS_HappyPath(t *testing.T) {
	v := Value{Kind: ValueKindGPS, GPS: GPSCoord{Latitude: 37.7749, Longitude: -122.4194}}
	if _, err := ValidateValue(v, 0); err != nil {
		t.Errorf("SF GPS rejected: %v", err)
	}
}

func TestValidateGPS_LatitudeOutOfRange_Rejected(t *testing.T) {
	for _, lat := range []float64{-91, 91, 200, -200} {
		t.Run("", func(t *testing.T) {
			v := Value{Kind: ValueKindGPS, GPS: GPSCoord{Latitude: lat, Longitude: 0.001}}
			if _, err := ValidateValue(v, 0); err == nil {
				t.Errorf("lat %v should be rejected", lat)
			}
		})
	}
}

func TestValidateGPS_LongitudeOutOfRange_Rejected(t *testing.T) {
	for _, lon := range []float64{-181, 181, 360, -360} {
		t.Run("", func(t *testing.T) {
			v := Value{Kind: ValueKindGPS, GPS: GPSCoord{Latitude: 0.001, Longitude: lon}}
			if _, err := ValidateValue(v, 0); err == nil {
				t.Errorf("lon %v should be rejected", lon)
			}
		})
	}
}

func TestValidateGPS_NullIsland_Rejected(t *testing.T) {
	v := Value{Kind: ValueKindGPS, GPS: GPSCoord{Latitude: 0, Longitude: 0}}
	if _, err := ValidateValue(v, 0); err == nil {
		t.Errorf("(0,0) should be rejected as no-fix sentinel")
	}
}

func TestValidateGPS_BoundaryAccepted(t *testing.T) {
	for _, c := range []GPSCoord{
		{Latitude: 90, Longitude: 180},
		{Latitude: -90, Longitude: -180},
		{Latitude: 89.999, Longitude: 179.999},
	} {
		t.Run("", func(t *testing.T) {
			v := Value{Kind: ValueKindGPS, GPS: c}
			if _, err := ValidateValue(v, 0); err != nil {
				t.Errorf("boundary %v rejected: %v", c, err)
			}
		})
	}
}

func TestNormalizeCameraMakeModel(t *testing.T) {
	cases := []struct {
		make_, model, want string
	}{
		{"Canon", "Canon EOS 5D", "Canon EOS 5D"},                     // dup-prefix strip
		{"Sony", "ILCE-7M3", "Sony ILCE-7M3"},                         // clean concat
		{"SONY", "Sony FE 24-70mm", "SONY FE 24-70mm"},                // case-insensitive strip
		{"", "EOS 5D", "EOS 5D"},                                      // empty make
		{"Canon", "", "Canon"},                                        // empty model
		{"NIKON CORPORATION", "NIKON CORPORATION D850", "NIKON CORPORATION D850"},
		{"Canon", "Canon", "Canon"},                                   // dup whole-string passes through cleanly (make==model edge case)
	}
	for _, c := range cases {
		t.Run(c.make_+"/"+c.model, func(t *testing.T) {
			got := NormalizeCameraMakeModel(c.make_, c.model)
			if got != c.want {
				t.Errorf("NormalizeCameraMakeModel(%q, %q) = %q, want %q", c.make_, c.model, got, c.want)
			}
		})
	}
}

func TestValidateValue_UnknownKind_Errors(t *testing.T) {
	v := Value{Kind: ValueKind(99)}
	if _, err := ValidateValue(v, 0); err == nil {
		t.Errorf("unknown ValueKind should error")
	}
}
