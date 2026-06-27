package iptc_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/iptc"
)

// dataset builds one IPTC IIM dataset record.
func dataset(rec, ds uint8, payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x1C)
	buf.WriteByte(rec)
	buf.WriteByte(ds)
	var lenBytes [2]byte
	binary.BigEndian.PutUint16(lenBytes[:], uint16(len(payload)))
	buf.Write(lenBytes[:])
	buf.Write(payload)
	return buf.Bytes()
}

// app13With8BIM wraps an IPTC blob in the Photoshop 3.0 / 8BIM /
// 0x0404 envelope that JPEG APP13 segments carry.
func app13With8BIM(iptcBlob []byte) []byte {
	var resBuf bytes.Buffer
	resBuf.WriteString("8BIM")
	// Resource id 0x0404 = IPTC IIM.
	resBuf.WriteByte(0x04)
	resBuf.WriteByte(0x04)
	// Pascal-string name: zero-length, padded to even total.
	resBuf.WriteByte(0x00)
	resBuf.WriteByte(0x00)
	// 4-byte data length (big-endian).
	var lenBytes [4]byte
	binary.BigEndian.PutUint32(lenBytes[:], uint32(len(iptcBlob)))
	resBuf.Write(lenBytes[:])
	resBuf.Write(iptcBlob)
	if len(iptcBlob)%2 != 0 {
		resBuf.WriteByte(0x00) // even-pad
	}
	return resBuf.Bytes()
}

// jpegWithIPTC builds a minimal JPEG byte stream containing an
// APP13 segment with the given IPTC blob. SOI + APP13 + EOI is
// enough — the JPEG carrier walker only needs the markers, not
// a decodable image stream.
func jpegWithIPTC(iptcBlob []byte) []byte {
	app13Body := append([]byte("Photoshop 3.0\x00"), app13With8BIM(iptcBlob)...)
	var jpeg bytes.Buffer
	jpeg.Write([]byte{0xFF, 0xD8}) // SOI
	jpeg.Write([]byte{0xFF, 0xED}) // APP13 marker
	var segLen [2]byte
	binary.BigEndian.PutUint16(segLen[:], uint16(len(app13Body)+2)) // length includes itself
	jpeg.Write(segLen[:])
	jpeg.Write(app13Body)
	jpeg.Write([]byte{0xFF, 0xD9}) // EOI
	return jpeg.Bytes()
}

func TestParseIPTCBlob_Latin1Default(t *testing.T) {
	// No 1:90 charset → bytes default to Latin-1. Test with a
	// pure-ASCII payload (round-trips identically under either).
	blob := bytes.Join([][]byte{
		dataset(2, 80, []byte("Ada Lovelace")),       // by-line
		dataset(2, 120, []byte("A test caption.")),   // caption
		dataset(2, 25, []byte("nature")),             // keyword #1
		dataset(2, 25, []byte("river")),              // keyword #2
	}, nil)

	res, err := iptc.ParseIPTCBlob(blob)
	if err != nil {
		t.Fatalf("ParseIPTCBlob: %v", err)
	}
	if got := res.Fields[metadata.FieldIPTCByline].Text; got != "Ada Lovelace" {
		t.Errorf("By-line = %q, want Ada Lovelace", got)
	}
	if got := res.Fields[metadata.FieldIPTCCaption].Text; got != "A test caption." {
		t.Errorf("Caption = %q, want 'A test caption.'", got)
	}
	if got := res.Fields[metadata.FieldIPTCKeywords].Text; got != "nature, river" {
		t.Errorf("Keywords = %q, want 'nature, river'", got)
	}
}

func TestParseIPTCBlob_UTF8WhenDeclared(t *testing.T) {
	// 1:90 = ESC %G declares UTF-8; subsequent strings stay byte-
	// for-byte intact instead of Latin-1-lifted.
	blob := bytes.Join([][]byte{
		dataset(1, 90, []byte("\x1B%G")),
		dataset(2, 80, []byte("田中太郎")),
		dataset(2, 105, []byte("こんにちは")),
	}, nil)

	res, err := iptc.ParseIPTCBlob(blob)
	if err != nil {
		t.Fatalf("ParseIPTCBlob: %v", err)
	}
	if got := res.Fields[metadata.FieldIPTCByline].Text; got != "田中太郎" {
		t.Errorf("By-line UTF-8 round-trip = %q, want 田中太郎", got)
	}
	if got := res.Fields[metadata.FieldIPTCHeadline].Text; got != "こんにちは" {
		t.Errorf("Headline UTF-8 round-trip = %q, want こんにちは", got)
	}
}

func TestParseIPTCBlob_TrimsTrailingNulls(t *testing.T) {
	// Camera firmware often null-pads fixed-length string fields.
	blob := dataset(2, 80, []byte("Ada\x00\x00\x00"))
	res, _ := iptc.ParseIPTCBlob(blob)
	if got := res.Fields[metadata.FieldIPTCByline].Text; got != "Ada" {
		t.Errorf("trim should drop trailing nulls; got %q", got)
	}
}

func TestParseIPTCBlob_EmptyReturnsErrNoIPTC(t *testing.T) {
	if _, err := iptc.ParseIPTCBlob(nil); !errors.Is(err, iptc.ErrNoIPTC) {
		t.Errorf("nil payload should ErrNoIPTC, got %v", err)
	}
	// Blob with only the charset marker (no app-record datasets)
	// also has no actionable values.
	blob := dataset(1, 90, []byte("\x1B%G"))
	if _, err := iptc.ParseIPTCBlob(blob); !errors.Is(err, iptc.ErrNoIPTC) {
		t.Errorf("charset-only blob should ErrNoIPTC, got %v", err)
	}
}

func TestParseIPTCBlob_TruncatedRecord_StopsCleanly(t *testing.T) {
	// Build a valid record then chop the payload. Walker must
	// return what it has + not panic.
	blob := dataset(2, 80, []byte("Ada Lovelace"))
	truncated := blob[:len(blob)-3]
	// Should not panic. May or may not return a result, but
	// must not blow up.
	_, _ = iptc.ParseIPTCBlob(truncated)
}

func TestFindJPEGIPTCBlob_HappyPath(t *testing.T) {
	want := bytes.Join([][]byte{
		dataset(2, 80, []byte("Photographer Name")),
		dataset(2, 120, []byte("Long caption text.")),
	}, nil)
	jpeg := jpegWithIPTC(want)

	got, err := iptc.FindJPEGIPTCBlob(jpeg)
	if err != nil {
		t.Fatalf("FindJPEGIPTCBlob: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("blob mismatch:\n got: %x\nwant: %x", got, want)
	}
}

func TestFindJPEGIPTCBlob_NoAPP13(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xD9} // SOI + EOI, no segments
	if _, err := iptc.FindJPEGIPTCBlob(jpeg); !errors.Is(err, iptc.ErrNoIPTC) {
		t.Errorf("expected ErrNoIPTC; got %v", err)
	}
}

func TestFindJPEGIPTCBlob_NotAJPEG(t *testing.T) {
	if _, err := iptc.FindJPEGIPTCBlob([]byte("PNG-like prefix")); !errors.Is(err, iptc.ErrNoIPTC) {
		t.Errorf("expected ErrNoIPTC on non-JPEG; got %v", err)
	}
}

func TestExtractor_PicksUpFieldsFromJPEGCarrier(t *testing.T) {
	blob := bytes.Join([][]byte{
		dataset(2, 80, []byte("Test Photographer")),
		dataset(2, 25, []byte("alpha")),
		dataset(2, 25, []byte("beta")),
	}, nil)
	jpeg := jpegWithIPTC(blob)

	res, err := iptc.New().Extract(context.Background(), bytes.NewReader(jpeg), "image/jpeg")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := res.Fields[metadata.FieldIPTCByline].Text; got != "Test Photographer" {
		t.Errorf("By-line = %q", got)
	}
	if got := res.Fields[metadata.FieldIPTCKeywords].Text; !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
		t.Errorf("Keywords = %q, want both alpha + beta", got)
	}
}

func TestExtractor_NoIPTCReturnsErrNoMetadata(t *testing.T) {
	// Bare JPEG with no APP13 → extractor reports no_metadata.
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	_, err := iptc.New().Extract(context.Background(), bytes.NewReader(jpeg), "image/jpeg")
	if !errors.Is(err, metadata.ErrNoMetadata) {
		t.Errorf("expected ErrNoMetadata; got %v", err)
	}
}

func TestExtractor_SupportsJPEGOnly(t *testing.T) {
	e := iptc.New()
	for _, mime := range []string{"image/jpeg", "image/jpg"} {
		if !e.Supports(mime) {
			t.Errorf("Supports(%q) = false, want true", mime)
		}
	}
	for _, mime := range []string{"image/png", "image/tiff", "image/webp", "application/pdf"} {
		if e.Supports(mime) {
			t.Errorf("Supports(%q) = true, want false (carrier walker is JPEG-only for MVP)", mime)
		}
	}
}
