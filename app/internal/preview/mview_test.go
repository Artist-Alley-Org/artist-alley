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

func TestExtractMviewThumbnail_CompressedEntryRejected(t *testing.T) {
	// Compressed flag set — we don't have an LZ77 decompressor yet.
	archive := buildEntry("thumbnail.jpg", "image/jpeg", mviewFlagCompressed, []byte{0x42})
	_, err := ExtractMviewThumbnail(bytes.NewReader(archive))
	if !errors.Is(err, ErrMviewEntryCompressed) {
		t.Fatalf("expected ErrMviewEntryCompressed, got %v", err)
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
