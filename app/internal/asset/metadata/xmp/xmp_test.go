package xmp_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/xmp"
)

// minimalXMPPacket builds an XMP packet covering the
// representative shapes the parser must handle: simple text,
// rdf:Alt (language-default), rdf:Bag (unordered list),
// rdf:Seq (ordered list), attribute-shorthand rating + label.
const minimalXMPPacket = `<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="Test 1.0">
  <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
    <rdf:Description
        xmlns:dc="http://purl.org/dc/elements/1.1/"
        xmlns:xmp="http://ns.adobe.com/xap/1.0/"
        xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/"
        xmlns:lr="http://ns.adobe.com/lightroom/1.0/"
        xmp:Rating="4"
        xmp:Label="Red">
      <dc:title>
        <rdf:Alt>
          <rdf:li xml:lang="x-default">Untitled Work</rdf:li>
          <rdf:li xml:lang="ja">無題</rdf:li>
        </rdf:Alt>
      </dc:title>
      <dc:description>
        <rdf:Alt>
          <rdf:li xml:lang="x-default">A long description.</rdf:li>
        </rdf:Alt>
      </dc:description>
      <dc:creator>
        <rdf:Seq>
          <rdf:li>Ada Lovelace</rdf:li>
          <rdf:li>Grace Hopper</rdf:li>
        </rdf:Seq>
      </dc:creator>
      <dc:subject>
        <rdf:Bag>
          <rdf:li>nature</rdf:li>
          <rdf:li>river</rdf:li>
        </rdf:Bag>
      </dc:subject>
      <photoshop:Headline>Short headline</photoshop:Headline>
      <lr:hierarchicalSubject>
        <rdf:Bag>
          <rdf:li>People|Family|Kids</rdf:li>
          <rdf:li>Places|Iceland</rdf:li>
        </rdf:Bag>
      </lr:hierarchicalSubject>
    </rdf:Description>
  </rdf:RDF>
</x:xmpmeta>`

func TestParseXMPPacket_AllRepresentativeShapes(t *testing.T) {
	res, err := xmp.ParseXMPPacket([]byte(minimalXMPPacket))
	if err != nil {
		t.Fatalf("ParseXMPPacket: %v", err)
	}
	cases := []struct {
		field metadata.CanonicalField
		want  string
	}{
		{metadata.FieldXMPTitle, "Untitled Work"},
		{metadata.FieldXMPDescription, "A long description."},
		{metadata.FieldXMPPhotoshopHeadline, "Short headline"},
		{metadata.FieldXMPLabel, "Red"},
	}
	for _, c := range cases {
		if got := res.Fields[c.field].Text; got != c.want {
			t.Errorf("%s = %q, want %q", c.field, got, c.want)
		}
	}
	if got := res.Fields[metadata.FieldXMPCreator].Text; !strings.Contains(got, "Ada") || !strings.Contains(got, "Grace") {
		t.Errorf("Creator (rdf:Seq) lost names: %q", got)
	}
	if got := res.Fields[metadata.FieldXMPSubjects].Text; !strings.Contains(got, "nature") || !strings.Contains(got, "river") {
		t.Errorf("Subject (rdf:Bag) lost tags: %q", got)
	}
	if got := res.Fields[metadata.FieldXMPHierarchicalTags].Text; !strings.Contains(got, "People|Family") || !strings.Contains(got, "Iceland") {
		t.Errorf("Hierarchical tags missing entries: %q", got)
	}
	if v := res.Fields[metadata.FieldXMPRating]; v.Kind != metadata.ValueKindNum || v.Num != 4.0 {
		t.Errorf("Rating = %+v, want Num=4.0", v)
	}
}

func TestParseXMPPacket_EmptyOrNoDescriptions_ErrNoXMP(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"/></x:xmpmeta>`),
		// rdf:Description with no known-namespace properties.
		[]byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:nonsense="x://x"><nonsense:Foo>bar</nonsense:Foo></rdf:Description></rdf:RDF></x:xmpmeta>`),
	}
	for i, b := range cases {
		if _, err := xmp.ParseXMPPacket(b); !errors.Is(err, xmp.ErrNoXMP) {
			t.Errorf("case %d: expected ErrNoXMP; got %v", i, err)
		}
	}
}

func TestParseXMPPacket_MalformedXML_PropagatesError(t *testing.T) {
	if _, err := xmp.ParseXMPPacket([]byte("not-xml")); err == nil {
		t.Errorf("malformed XML should error")
	}
}

func TestFindJPEGXMPPacket_HappyPath(t *testing.T) {
	jpeg := wrapInJPEG_APP1(xmpAPP1Body([]byte(minimalXMPPacket)))
	pkt, err := xmp.FindJPEGXMPPacket(jpeg)
	if err != nil {
		t.Fatalf("FindJPEGXMPPacket: %v", err)
	}
	if !bytes.Equal(pkt, []byte(minimalXMPPacket)) {
		t.Errorf("packet mismatch (len got=%d want=%d)", len(pkt), len(minimalXMPPacket))
	}
}

func TestFindJPEGXMPPacket_NoAPP1(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	if _, err := xmp.FindJPEGXMPPacket(jpeg); !errors.Is(err, xmp.ErrNoXMP) {
		t.Errorf("expected ErrNoXMP; got %v", err)
	}
}

func TestFindPNGXMPPacket_HappyPath(t *testing.T) {
	png := buildPNGWithXMP([]byte(minimalXMPPacket))
	pkt, err := xmp.FindPNGXMPPacket(png)
	if err != nil {
		t.Fatalf("FindPNGXMPPacket: %v", err)
	}
	if !bytes.Equal(pkt, []byte(minimalXMPPacket)) {
		t.Errorf("packet mismatch (len got=%d want=%d)", len(pkt), len(minimalXMPPacket))
	}
}

func TestFindPNGXMPPacket_NotAPNG(t *testing.T) {
	if _, err := xmp.FindPNGXMPPacket([]byte("not-png")); !errors.Is(err, xmp.ErrNoXMP) {
		t.Errorf("expected ErrNoXMP; got %v", err)
	}
}

func TestExtractor_JPEGCarrier(t *testing.T) {
	jpeg := wrapInJPEG_APP1(xmpAPP1Body([]byte(minimalXMPPacket)))
	res, err := xmp.New().Extract(context.Background(), bytes.NewReader(jpeg), "image/jpeg")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := res.Fields[metadata.FieldXMPTitle].Text; got != "Untitled Work" {
		t.Errorf("Title = %q", got)
	}
}

func TestExtractor_PNGCarrier(t *testing.T) {
	png := buildPNGWithXMP([]byte(minimalXMPPacket))
	res, err := xmp.New().Extract(context.Background(), bytes.NewReader(png), "image/png")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := res.Fields[metadata.FieldXMPTitle].Text; got != "Untitled Work" {
		t.Errorf("Title = %q", got)
	}
}

func TestExtractor_NoXMP_ErrNoMetadata(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	if _, err := xmp.New().Extract(context.Background(), bytes.NewReader(jpeg), "image/jpeg"); !errors.Is(err, metadata.ErrNoMetadata) {
		t.Errorf("expected ErrNoMetadata; got %v", err)
	}
}

func TestExtractor_SupportsOnlyJPEGAndPNG(t *testing.T) {
	e := xmp.New()
	for _, mime := range []string{"image/jpeg", "image/jpg", "image/png"} {
		if !e.Supports(mime) {
			t.Errorf("Supports(%q) false; want true", mime)
		}
	}
	for _, mime := range []string{"image/tiff", "image/webp", "application/pdf"} {
		if e.Supports(mime) {
			t.Errorf("Supports(%q) true; want false (carrier walker MVP)", mime)
		}
	}
}

// --- carrier-fixture helpers ---

func xmpAPP1Body(packet []byte) []byte {
	body := append([]byte("http://ns.adobe.com/xap/1.0/\x00"), packet...)
	return body
}

func wrapInJPEG_APP1(body []byte) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xD8})       // SOI
	buf.Write([]byte{0xFF, 0xE1})       // APP1
	var lenBytes [2]byte
	binary.BigEndian.PutUint16(lenBytes[:], uint16(len(body)+2))
	buf.Write(lenBytes[:])
	buf.Write(body)
	buf.Write([]byte{0xFF, 0xD9}) // EOI
	return buf.Bytes()
}

func buildPNGWithXMP(packet []byte) []byte {
	var buf bytes.Buffer
	// PNG signature.
	buf.Write([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	// IHDR — minimal valid (1x1, 8-bit, greyscale).
	writeChunk(&buf, "IHDR", []byte{
		0, 0, 0, 1, 0, 0, 0, 1,
		8, 0, 0, 0, 0,
	})
	// iTXt chunk:  keyword\0 compFlag(0) compMethod(0) language\0 translatedKeyword\0 text.
	var itxt bytes.Buffer
	itxt.WriteString("XML:com.adobe.xmp")
	itxt.WriteByte(0)
	itxt.WriteByte(0) // compFlag uncompressed
	itxt.WriteByte(0) // compMethod
	itxt.WriteByte(0) // language\0 (empty)
	itxt.WriteByte(0) // translated-keyword\0 (empty)
	itxt.Write(packet)
	writeChunk(&buf, "iTXt", itxt.Bytes())
	// IEND.
	writeChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func writeChunk(w *bytes.Buffer, kind string, data []byte) {
	var lenBytes [4]byte
	binary.BigEndian.PutUint32(lenBytes[:], uint32(len(data)))
	w.Write(lenBytes[:])
	w.WriteString(kind)
	w.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(kind))
	crc.Write(data)
	var crcBytes [4]byte
	binary.BigEndian.PutUint32(crcBytes[:], crc.Sum32())
	w.Write(crcBytes[:])
}
