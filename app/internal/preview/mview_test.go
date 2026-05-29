package preview

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// buildEntry synthesises one entry in the Marmoset Archive shape so
// we can round-trip without a real .mview file. Tracks the same byte
// layout the parser expects.
func buildEntry(name, typ string, flags uint32, data []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(name)
	buf.WriteByte(0)
	buf.WriteString(typ)
	buf.WriteByte(0)
	_ = binary.Write(&buf, binary.LittleEndian, flags)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(data))) // uncompressed == compressed (flag=0)
	buf.Write(data)
	return buf.Bytes()
}

func TestExtractMviewThumbnail_FoundFirst(t *testing.T) {
	jpegBytes := []byte{0xff, 0xd8, 0xff, 0xe0, 0x42, 0x41, 0x52} // not a real JPEG, parser doesn't care
	archive := buildEntry("thumbnail.jpg", "image/jpeg", 0, jpegBytes)
	got, err := ExtractMviewThumbnail(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("expected thumbnail, got error: %v", err)
	}
	if !bytes.Equal(got, jpegBytes) {
		t.Fatalf("thumb mismatch: got %x want %x", got, jpegBytes)
	}
}

func TestExtractMviewThumbnail_SkipsPriorEntries(t *testing.T) {
	jpegBytes := []byte("the-real-thumbnail-bytes")
	// Archive has a chunk we don't want, THEN the thumbnail.
	archive := bytes.Join([][]byte{
		buildEntry("metadata.json", "application/json", 0, []byte(`{"name":"foo"}`)),
		buildEntry("preview.png", "image/png", 0, []byte("decoy")),
		buildEntry("thumbnail.jpg", "image/jpeg", 0, jpegBytes),
	}, nil)
	got, err := ExtractMviewThumbnail(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("expected thumbnail, got error: %v", err)
	}
	if !bytes.Equal(got, jpegBytes) {
		t.Fatalf("thumb mismatch: got %q want %q", got, jpegBytes)
	}
}

func TestExtractMviewThumbnail_CompressedTruncatedStream(t *testing.T) {
	// Compressed flag set, but the input has 1 byte while
	// uncompressed_size says 100. The decoder produces just the
	// literal first byte and then hits the length-mismatch guard.
	var buf bytes.Buffer
	buf.WriteString("thumbnail.jpg")
	buf.WriteByte(0)
	buf.WriteString("image/jpeg")
	buf.WriteByte(0)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(mviewFlagCompressed))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(100))
	buf.WriteByte(0x42)
	_, err := ExtractMviewThumbnail(bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, ErrMviewDecompress) {
		t.Fatalf("expected ErrMviewDecompress on length mismatch, got %v", err)
	}
}

func TestDecompressLZW_LiteralByteStream(t *testing.T) {
	// "Smoke test": a stream where every code is a literal byte (no
	// dictionary lookups). With 12-bit codes packed two-per-three-
	// bytes, encoding the sequence [0x41 0x42 0x43] is:
	//   - first byte of output = first byte of input = 0x41
	//   - then r=1 reads code at packedIdx=1 → (input[2]<<4)|(input[1]>>4)
	//   - r=2 reads code at packedIdx=3 → ((input[3]&15)<<8)|input[2]
	// Building a stream that decompresses to [0x41,0x42,0x43] requires
	// encoding codes 0x042 and 0x043 in that packed shape:
	//   r=1: code 0x042 → (m<<4)|(n>>4)=0x042 → m=0x04, n=0x20
	//   r=2: code 0x043 → ((m&15)<<8)|n=0x043 → m=0x00, n=0x43
	// Encoder layout (bytes 0..4): 0x41, 0x20, 0x04, 0x43, 0x00
	in := []byte{0x41, 0x20, 0x04, 0x43, 0x00}
	out, err := decompressLZW(in, 3)
	if err != nil {
		t.Fatalf("decompress failed: %v", err)
	}
	want := []byte{0x41, 0x42, 0x43}
	if !bytes.Equal(out, want) {
		t.Fatalf("decompress mismatch: got %x want %x", out, want)
	}
}

func TestExtractMviewThumbnail_NotFound(t *testing.T) {
	archive := buildEntry("metadata.json", "application/json", 0, []byte(`{}`))
	_, err := ExtractMviewThumbnail(bytes.NewReader(archive))
	if err == nil {
		t.Fatal("expected error when thumbnail entry is absent")
	}
	// EOF is the expected signal — we walked the whole archive without finding it.
}

func TestExtractMviewThumbnail_StringLengthGuard(t *testing.T) {
	// Construct an entry whose name has no null terminator within the cap.
	huge := bytes.Repeat([]byte{'a'}, mviewMaxStringLen+1)
	_, err := ExtractMviewThumbnail(bytes.NewReader(huge))
	if err == nil || !strings.Contains(err.Error(), "cstring") {
		t.Fatalf("expected cstring-length error, got %v", err)
	}
}

func TestExtractMviewThumbnail_EntrySizeGuard(t *testing.T) {
	// Craft a header that claims 2GB of data — should refuse before
	// allocating.
	var buf bytes.Buffer
	buf.WriteString("thumbnail.jpg")
	buf.WriteByte(0)
	buf.WriteString("image/jpeg")
	buf.WriteByte(0)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(2_000_000_000))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(2_000_000_000))
	_, err := ExtractMviewThumbnail(bytes.NewReader(buf.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "size cap") {
		t.Fatalf("expected size cap error, got %v", err)
	}
}
