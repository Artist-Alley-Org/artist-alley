package metadata

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ---------------------------------------------------------------------------
// Synthesized ICC fixtures — full JPEG/PNG/WebP containers with
// known ICC profile bytes inside. Avoids dragging in real
// 6-15 KB ICC profiles from external sources; the round-trip
// test cares about byte fidelity, not profile correctness.
// ---------------------------------------------------------------------------

// fakeICCProfile is a deterministic byte blob we splice into
// containers + assert comes back byte-identical from
// ExtractICCProfile. Real ICC profiles are 1-15 KB; this 256-byte
// stub exercises the chunk-copy code paths without bloating
// fixtures.
var fakeICCProfile = func() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}()

// ---------------------------------------------------------------------------
// JPEG ICC tests
// ---------------------------------------------------------------------------

// buildJPEGWithICC stitches a minimal JPEG container with one
// APP2 ICC chunk (single-chunk profile, seq=1, total=1). The
// JPEG itself isn't decodable (no actual image data) — the
// ICC extractor doesn't care.
func buildJPEGWithICC(icc []byte) []byte {
	var buf bytes.Buffer
	// SOI
	buf.Write([]byte{0xFF, 0xD8})
	// APP2 with ICC payload
	payload := append([]byte(jpegPrefix), 0x01, 0x01) // seq=1, total=1
	payload = append(payload, icc...)
	segLen := len(payload) + 2 // +2 for the length field itself
	buf.Write([]byte{0xFF, 0xE2})
	_ = binary.Write(&buf, binary.BigEndian, uint16(segLen))
	buf.Write(payload)
	// EOI
	buf.Write([]byte{0xFF, 0xD9})
	return buf.Bytes()
}

func TestExtractICC_JPEG_SingleChunk_RoundTripByteIdentical(t *testing.T) {
	jpg := buildJPEGWithICC(fakeICCProfile)
	got, err := ExtractICCProfile(jpg, "image/jpeg")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !bytes.Equal(got, fakeICCProfile) {
		t.Errorf("ICC bytes differ — round-trip broken")
	}
}

func TestExtractICC_JPEG_NoICC_ReturnsNilNoError(t *testing.T) {
	// SOI + EOI only — valid empty JPEG.
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	got, err := ExtractICCProfile(jpg, "image/jpeg")
	if err != nil {
		t.Errorf("got err %v, want nil", err)
	}
	if got != nil {
		t.Errorf("got %d bytes, want nil", len(got))
	}
}

func TestExtractICC_JPEG_NotAJPEG_ReturnsErrMalformed(t *testing.T) {
	_, err := ExtractICCProfile([]byte("not a jpeg"), "image/jpeg")
	if err == nil {
		t.Errorf("missing SOI should be rejected")
	}
}

func TestExtractICC_JPEG_MultiChunk_ConcatenatedInOrder(t *testing.T) {
	// Build a 3-chunk JPEG. Each chunk carries 1/3 of the profile.
	chunkSize := len(fakeICCProfile) / 3
	chunks := [][]byte{
		fakeICCProfile[:chunkSize],
		fakeICCProfile[chunkSize : chunkSize*2],
		fakeICCProfile[chunkSize*2:],
	}
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xD8}) // SOI
	for i, c := range chunks {
		payload := append([]byte(jpegPrefix), byte(i+1), 0x03)
		payload = append(payload, c...)
		buf.Write([]byte{0xFF, 0xE2})
		_ = binary.Write(&buf, binary.BigEndian, uint16(len(payload)+2))
		buf.Write(payload)
	}
	buf.Write([]byte{0xFF, 0xD9}) // EOI
	got, err := ExtractICCProfile(buf.Bytes(), "image/jpeg")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !bytes.Equal(got, fakeICCProfile) {
		t.Errorf("multi-chunk concat broken")
	}
}

// ---------------------------------------------------------------------------
// PNG ICC tests
// ---------------------------------------------------------------------------

// buildPNGWithICC stitches a minimal PNG container with one iCCP
// chunk holding the given bytes verbatim. (We don't zlib-compress
// here because the chunk-copy semantics return whatever's in the
// chunk payload; the extractor is bytes-in-bytes-out.)
func buildPNGWithICC(payload []byte) []byte {
	var buf bytes.Buffer
	buf.Write(pngSignature)
	// IHDR with minimal valid content (1x1 grey).
	ihdr := []byte{
		0, 0, 0, 1, // width 1
		0, 0, 0, 1, // height 1
		8,    // bit depth
		0,    // colour type (grey)
		0, 0, 0, // compression, filter, interlace
	}
	writeChunk(&buf, "IHDR", ihdr)
	writeChunk(&buf, "iCCP", payload)
	writeChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func writeChunk(buf *bytes.Buffer, kind string, data []byte) {
	_ = binary.Write(buf, binary.BigEndian, uint32(len(data)))
	buf.WriteString(kind)
	buf.Write(data)
	// CRC32 — placeholder; the extractor doesn't validate it.
	buf.Write([]byte{0, 0, 0, 0})
}

func TestExtractICC_PNG_iCCP_RoundTripByteIdentical(t *testing.T) {
	payload := append([]byte("test-profile\x00\x00"), fakeICCProfile...)
	png := buildPNGWithICC(payload)
	got, err := ExtractICCProfile(png, "image/png")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("PNG ICC bytes differ — round-trip broken")
	}
}

func TestExtractICC_PNG_NoICCP_ReturnsNil(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(pngSignature)
	writeChunk(&buf, "IHDR", []byte{0, 0, 0, 1, 0, 0, 0, 1, 8, 0, 0, 0, 0})
	writeChunk(&buf, "IDAT", []byte{0, 0, 0, 0})
	writeChunk(&buf, "IEND", nil)
	got, err := ExtractICCProfile(buf.Bytes(), "image/png")
	if err != nil {
		t.Errorf("got err %v, want nil", err)
	}
	if got != nil {
		t.Errorf("got %d bytes, want nil", len(got))
	}
}

func TestExtractICC_PNG_NotAPNG_ReturnsErrMalformed(t *testing.T) {
	_, err := ExtractICCProfile([]byte("not a png"), "image/png")
	if err == nil {
		t.Errorf("missing PNG signature should be rejected")
	}
}

// ---------------------------------------------------------------------------
// WebP ICC tests
// ---------------------------------------------------------------------------

// buildWebPWithICC stitches a minimal RIFF/WEBP container with an
// ICCP chunk + a VP8X chunk (the container marker).
func buildWebPWithICC(icc []byte) []byte {
	var buf bytes.Buffer
	// We'll build the body first, then prepend the RIFF header
	// with the right total length.
	var body bytes.Buffer
	body.WriteString("WEBP")
	// VP8X chunk (required for extended container with ICCP).
	body.WriteString("VP8X")
	_ = binary.Write(&body, binary.LittleEndian, uint32(10))
	body.Write(make([]byte, 10))
	// ICCP chunk.
	body.WriteString("ICCP")
	_ = binary.Write(&body, binary.LittleEndian, uint32(len(icc)))
	body.Write(icc)
	if len(icc)%2 == 1 {
		body.WriteByte(0) // RIFF word-align pad
	}

	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(body.Len()))
	buf.Write(body.Bytes())
	return buf.Bytes()
}

func TestExtractICC_WebP_ICCP_RoundTripByteIdentical(t *testing.T) {
	webp := buildWebPWithICC(fakeICCProfile)
	got, err := ExtractICCProfile(webp, "image/webp")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !bytes.Equal(got, fakeICCProfile) {
		t.Errorf("WebP ICC bytes differ — round-trip broken")
	}
}

func TestExtractICC_WebP_OddLengthICC_HandlesPad(t *testing.T) {
	odd := append([]byte{}, fakeICCProfile...)
	odd = append(odd, 0xAA) // length 257 — odd
	webp := buildWebPWithICC(odd)
	got, err := ExtractICCProfile(webp, "image/webp")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !bytes.Equal(got, odd) {
		t.Errorf("odd-length WebP ICC broken")
	}
}

func TestExtractICC_WebP_NotAWebP_ReturnsErrMalformed(t *testing.T) {
	_, err := ExtractICCProfile([]byte("not a webp at all"), "image/webp")
	if err == nil {
		t.Errorf("missing RIFF/WEBP signature should be rejected")
	}
}

// ---------------------------------------------------------------------------
// Format dispatch
// ---------------------------------------------------------------------------

func TestExtractICC_UnsupportedFormat_ReturnsNilNoError(t *testing.T) {
	got, err := ExtractICCProfile([]byte("data"), "image/tiff")
	if err != nil {
		t.Errorf("TIFF should return nil/nil (no extractable profile, not an error): %v", err)
	}
	if got != nil {
		t.Errorf("TIFF should return nil bytes")
	}
}
