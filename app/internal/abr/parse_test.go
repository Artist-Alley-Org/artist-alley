package abr

import (
	"bytes"
	"image"
	"strings"
	"testing"
)

// encodeRow assembles a row's PackBits payload + the 2-byte BE row
// length entry used by the per-row index. Returned as (rowIdx, body).
// The encoder mirrors the decoder we test so a hand-written test
// can't drift from format quirks (e.g. literal run length = n+1).
func encodeRow(opcodes ...any) (rowLen []byte, body []byte) {
	var buf bytes.Buffer
	for _, op := range opcodes {
		switch v := op.(type) {
		case literalRun:
			if len(v) == 0 || len(v) > 128 {
				panic("literal run must be 1..128 bytes")
			}
			buf.WriteByte(byte(len(v) - 1))
			buf.Write(v)
		case repeatRun:
			if v.count < 2 || v.count > 128 {
				panic("repeat count must be 2..128")
			}
			buf.WriteByte(byte(-int8(v.count - 1)))
			buf.WriteByte(v.b)
		case nopByte:
			buf.WriteByte(0x80)
		default:
			panic("unknown opcode type")
		}
	}
	body = buf.Bytes()
	rowLen = []byte{byte(len(body) >> 8), byte(len(body) & 0xff)}
	return
}

type literalRun []byte
type repeatRun struct {
	count int
	b     byte
}
type nopByte struct{}

// buildPackBitsPayload glues the per-row length table to the
// concatenated row bodies so it matches the on-disk layout the
// decoder expects.
func buildPackBitsPayload(t *testing.T, rows ...[]any) []byte {
	t.Helper()
	var lens, bodies bytes.Buffer
	for _, row := range rows {
		l, b := encodeRow(row...)
		lens.Write(l)
		bodies.Write(b)
	}
	return append(lens.Bytes(), bodies.Bytes()...)
}

func TestDecompressPackBits_Literal(t *testing.T) {
	// One row of three literal bytes.
	payload := buildPackBitsPayload(t, []any{
		literalRun{0x10, 0x20, 0x30},
	})
	got, err := decompressPackBits(payload, 3, 1)
	if err != nil {
		t.Fatalf("decompressPackBits: %v", err)
	}
	want := []byte{0x10, 0x20, 0x30}
	if !bytes.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDecompressPackBits_Repeat(t *testing.T) {
	// Row: five copies of 0xFF.
	payload := buildPackBitsPayload(t, []any{
		repeatRun{count: 5, b: 0xFF},
	})
	got, err := decompressPackBits(payload, 5, 1)
	if err != nil {
		t.Fatalf("decompressPackBits: %v", err)
	}
	want := bytes.Repeat([]byte{0xFF}, 5)
	if !bytes.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDecompressPackBits_MixedAndMultiRow(t *testing.T) {
	// Row 0: literal {1,2} then repeat {9 x3}.
	// Row 1: repeat {0xAA x2} then literal {3,4,5}.
	// 0x80 no-op embedded in row 0 to confirm it's silently consumed.
	payload := buildPackBitsPayload(t,
		[]any{
			literalRun{0x01, 0x02},
			nopByte{},
			repeatRun{count: 3, b: 0x09},
		},
		[]any{
			repeatRun{count: 2, b: 0xAA},
			literalRun{0x03, 0x04, 0x05},
		},
	)
	got, err := decompressPackBits(payload, 5, 2)
	if err != nil {
		t.Fatalf("decompressPackBits: %v", err)
	}
	want := []byte{
		0x01, 0x02, 0x09, 0x09, 0x09, // row 0
		0xAA, 0xAA, 0x03, 0x04, 0x05, // row 1
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDecompressPackBits_RejectsMissingRowTable(t *testing.T) {
	_, err := decompressPackBits([]byte{0x00}, 5, 3) // need 6 bytes of table
	if err == nil {
		t.Fatal("expected error for missing row table")
	}
	if !strings.Contains(err.Error(), "row-length table") {
		t.Errorf("unexpected error: %v", err)
	}
}

// A row that decodes to fewer bytes than the declared width is the
// classic "encoder bug" failure mode — the decoder must catch it
// rather than silently emit a short row that the brush renderer
// would then misalign.
func TestDecompressPackBits_RejectsRowSizeMismatch(t *testing.T) {
	payload := buildPackBitsPayload(t, []any{
		literalRun{0x01, 0x02},
	})
	_, err := decompressPackBits(payload, 5, 1) // expect 5 bytes per row, got 2
	if err == nil {
		t.Fatal("expected size-mismatch error")
	}
	if !strings.Contains(err.Error(), "expected 5") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Row length in the index claims more bytes than the payload
// contains — corruption that must surface as a parse error, not
// an out-of-bounds slice panic.
func TestDecompressPackBits_RejectsRowOverrun(t *testing.T) {
	// Row-length table says row 0 is 100 bytes; payload only has 1.
	payload := append([]byte{0x00, 0x64}, 0x01)
	_, err := decompressPackBits(payload, 1, 1)
	if err == nil {
		t.Fatal("expected row-overrun error")
	}
	if !strings.Contains(err.Error(), "overruns") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Literal opcode declares 5 bytes but the row body only has 3.
// We hand-build the bytes (not via encodeRow) so we can construct an
// invalid stream the encoder would refuse to produce.
func TestDecompressPackBits_RejectsLiteralOverflow(t *testing.T) {
	// Row 0 length = 3 bytes: opcode 0x04 (literal of 5), then 2 data bytes.
	payload := []byte{
		0x00, 0x03, // row 0 length = 3
		0x04, 0x11, 0x22, // opcode says 5 literal bytes; only 2 follow
	}
	_, err := decompressPackBits(payload, 5, 1)
	if err == nil {
		t.Fatal("expected literal-overflow error")
	}
	if !strings.Contains(err.Error(), "literal run overflows") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Repeat opcode with no following payload byte must error rather
// than read past the row boundary.
func TestDecompressPackBits_RejectsRepeatWithoutPayload(t *testing.T) {
	// Row length = 1 byte: opcode 0xFF (repeat 2 of next byte) with
	// no next byte present in the row.
	payload := []byte{
		0x00, 0x01, // row 0 length = 1
		0xFF,
	}
	_, err := decompressPackBits(payload, 2, 1)
	if err == nil {
		t.Fatal("expected repeat-without-payload error")
	}
	if !strings.Contains(err.Error(), "repeat-run") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBrush_AsImage(t *testing.T) {
	b := Brush{
		Width:  3,
		Height: 2,
		Alpha:  []byte{1, 2, 3, 4, 5, 6},
	}
	img := b.AsImage()
	if img.Bounds() != image.Rect(0, 0, 3, 2) {
		t.Errorf("bounds = %v", img.Bounds())
	}
	if img.Stride != 3 {
		t.Errorf("stride = %d", img.Stride)
	}
	// Shared backing array — mutating the image should mutate Alpha.
	img.Pix[0] = 99
	if b.Alpha[0] != 99 {
		t.Errorf("AsImage didn't share Alpha (got %d)", b.Alpha[0])
	}
}

func TestParseBrushes_EmptyInput(t *testing.T) {
	// Zero-length input can't carry a valid header — the parser must
	// surface a parse error, not panic.
	_, err := ParseBrushes(bytes.NewReader(nil))
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "abr:") {
		t.Errorf("error %q missing abr: prefix", err.Error())
	}
}

func TestParseBrushes_GarbageInput(t *testing.T) {
	// Garbage that's long enough to clear a few header reads but
	// can't form a valid section table. Must reject cleanly.
	_, err := ParseBrushes(bytes.NewReader(bytes.Repeat([]byte{0xAB}, 256)))
	if err == nil {
		t.Fatal("expected error for garbage input")
	}
}

func TestParseBrushes_ReadErrorPropagates(t *testing.T) {
	// io.ReadAll surfaces reader errors verbatim — confirm we wrap
	// them with the abr: prefix so logs are scoped to the parser.
	r := &errReader{}
	_, err := ParseBrushes(r)
	if err == nil {
		t.Fatal("expected error from failing reader")
	}
	if !strings.Contains(err.Error(), "abr: read input") {
		t.Errorf("error %q missing abr: read prefix", err.Error())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errSentinel{} }

type errSentinel struct{}

func (errSentinel) Error() string { return "synthetic read failure" }
