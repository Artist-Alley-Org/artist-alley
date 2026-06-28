package pdf_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/asset/metadata/pdf"
)

// minimalPDF builds a one-page PDF with a populated /Info dictionary
// and returns the raw bytes. xref offsets are computed as we write
// each object so the file is genuinely well-formed (vs the "trick
// pdfcpu's relaxed mode" version, which would silently mask real
// bugs in the extractor).
//
// PDF version 1.4 chosen for max compatibility — pdfcpu accepts
// every version we care about; 1.4 keeps the structure boring
// enough that a future reader can audit the byte layout by hand.
func minimalPDF(title, author, subject, keywords, creator, producer string) []byte {
	var buf bytes.Buffer

	// Track object byte offsets for the xref table. Object 0 is the
	// canonical free entry; real objects are 1..N.
	offsets := []int{0}
	addObj := func(body string) {
		offsets = append(offsets, buf.Len())
		buf.WriteString(body)
	}

	// PDF header + binary marker (per ISO 32000-1 §7.5.2 — the four
	// high-bit bytes make grep + file(1) reliably detect binary PDFs).
	buf.WriteString("%PDF-1.4\n")
	buf.WriteString("%\xe2\xe3\xcf\xd3\n")

	addObj("1 0 obj\n<</Type/Catalog/Pages 2 0 R>>\nendobj\n")
	addObj("2 0 obj\n<</Type/Pages/Count 1/Kids[3 0 R]>>\nendobj\n")
	addObj("3 0 obj\n<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Resources<<>>>>\nendobj\n")
	info := fmt.Sprintf("4 0 obj\n<</Title(%s)/Author(%s)/Subject(%s)/Keywords(%s)/Creator(%s)/Producer(%s)>>\nendobj\n",
		title, author, subject, keywords, creator, producer)
	addObj(info)

	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(offsets))
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets[1:] {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<</Size %d/Root 1 0 R/Info 4 0 R>>\nstartxref\n%d\n%%%%EOF\n",
		len(offsets), xrefPos)

	return buf.Bytes()
}

func TestExtractor_Supports(t *testing.T) {
	e := pdf.New()
	cases := []struct {
		mime string
		want bool
	}{
		{"application/pdf", true},
		{"application/x-pdf", true},
		{"APPLICATION/PDF", true}, // case-insensitive
		{"image/jpeg", false},
		{"text/plain", false},
		{"", false},
	}
	for _, c := range cases {
		if got := e.Supports(c.mime); got != c.want {
			t.Errorf("Supports(%q) = %v, want %v", c.mime, got, c.want)
		}
	}
}

func TestExtract_RealisticPDF(t *testing.T) {
	src := minimalPDF(
		"Annual Report 2026",
		"Ada Lovelace",
		"Quarterly financial overview",
		"finance, q4, projections",
		"LibreOffice 7.6",
		"libreoffice-pdf-export 7.6",
	)

	res, err := pdf.New().Extract(context.Background(), bytes.NewReader(src), "application/pdf")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if res.PageCount != 1 {
		t.Errorf("PageCount = %d, want 1", res.PageCount)
	}
	checks := []struct {
		field metadata.CanonicalField
		want  string
	}{
		{metadata.FieldPDFTitle, "Annual Report 2026"},
		{metadata.FieldPDFAuthor, "Ada Lovelace"},
		{metadata.FieldPDFSubject, "Quarterly financial overview"},
		// pdfcpu alphabetises the tokens internally on parse; we
		// reflect that here rather than fight the library.
		{metadata.FieldPDFKeywords, "finance, projections, q4"},
		{metadata.FieldPDFCreator, "LibreOffice 7.6"},
		{metadata.FieldPDFProducer, "libreoffice-pdf-export 7.6"},
	}
	for _, c := range checks {
		v, ok := res.Fields[c.field]
		if !ok {
			t.Errorf("missing field %s", c.field)
			continue
		}
		if v.Kind != metadata.ValueKindText {
			t.Errorf("%s: kind = %v, want text", c.field, v.Kind)
		}
		if v.Text != c.want {
			t.Errorf("%s = %q, want %q", c.field, v.Text, c.want)
		}
	}
}

func TestExtract_NoInfoDict_StillCarriesPageCount(t *testing.T) {
	// PDF with no /Info — only catalog + pages. Page count alone is
	// enough to NOT be ErrNoMetadata since assets.page_count is a
	// real value worth stamping.
	var buf bytes.Buffer
	offsets := []int{0}
	addObj := func(body string) {
		offsets = append(offsets, buf.Len())
		buf.WriteString(body)
	}
	buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	addObj("1 0 obj\n<</Type/Catalog/Pages 2 0 R>>\nendobj\n")
	addObj("2 0 obj\n<</Type/Pages/Count 2/Kids[3 0 R 4 0 R]>>\nendobj\n")
	addObj("3 0 obj\n<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Resources<<>>>>\nendobj\n")
	addObj("4 0 obj\n<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Resources<<>>>>\nendobj\n")
	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, off := range offsets[1:] {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xrefPos)

	res, err := pdf.New().Extract(context.Background(), bytes.NewReader(buf.Bytes()), "application/pdf")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.PageCount != 2 {
		t.Errorf("PageCount = %d, want 2", res.PageCount)
	}
	if len(res.Fields) != 0 {
		t.Errorf("Fields = %v, want empty", res.Fields)
	}
}

func TestExtract_UnsupportedMIME(t *testing.T) {
	res, err := pdf.New().Extract(context.Background(), bytes.NewReader([]byte("not a pdf")), "image/jpeg")
	if !errors.Is(err, metadata.ErrUnsupportedFormat) {
		t.Errorf("err = %v, want ErrUnsupportedFormat", err)
	}
	if len(res.Fields) > 0 {
		t.Errorf("Fields populated on unsupported MIME: %v", res.Fields)
	}
}

func TestExtract_MalformedSource(t *testing.T) {
	res, err := pdf.New().Extract(context.Background(), bytes.NewReader([]byte("this is not a pdf, just garbage")), "application/pdf")
	if !errors.Is(err, metadata.ErrMalformedFile) {
		t.Errorf("err = %v, want ErrMalformedFile", err)
	}
	// Format is echoed so the failure-row carries useful context.
	if res.Format != "application/pdf" {
		t.Errorf("Format = %q, want application/pdf", res.Format)
	}
}

func TestExtract_KeywordsFiltering(t *testing.T) {
	// PDFs with sloppy keyword strings (extra whitespace, empty
	// tokens between commas) shouldn't produce ", ," noise in the
	// joined output.
	src := minimalPDF("T", "A", "S", "  foo ,  , bar  ", "C", "P")
	res, err := pdf.New().Extract(context.Background(), bytes.NewReader(src), "application/pdf")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Note: pdfcpu's keyword parser splits on commas, which is the
	// PDF spec's loose convention. After our filterEmpty pass we
	// expect tokens trimmed + empties dropped.
	got := res.Fields[metadata.FieldPDFKeywords].Text
	if strings.Contains(got, ", ,") {
		t.Errorf("Keywords carry empty token: %q", got)
	}
}
