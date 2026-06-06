package federation_test

import (
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

func TestCanonicalizeSortsObjectKeys(t *testing.T) {
	in := []byte(`{"z":1,"a":2,"m":3}`)
	out, err := federation.Canonicalize(in)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	got := string(out)
	if got != `{"a":2,"m":3,"z":1}` {
		t.Errorf("key sort drift: %q", got)
	}
}

func TestCanonicalizeStripsWhitespace(t *testing.T) {
	in := []byte("{\n  \"a\": 1,\n  \"b\": 2\n}")
	out, _ := federation.Canonicalize(in)
	got := string(out)
	if strings.ContainsAny(got, "\n\t ") {
		t.Errorf("expected no whitespace, got %q", got)
	}
}

func TestCanonicalizeRejectsBadJSON(t *testing.T) {
	_, err := federation.Canonicalize([]byte("{not json}"))
	if err == nil {
		t.Error("malformed JSON should fail canonicalize")
	}
}

func TestCanonicalizeValueRoundTrip(t *testing.T) {
	// Encoded via stdlib JSON then canonicalized. The canonicalized
	// form is deterministic regardless of the intermediate
	// serializer's quirks (the JCS layer normalizes).
	v := map[string]any{"z": 1, "a": 2, "list": []any{3, 1, 2}}
	out, err := federation.CanonicalizeValue(v)
	if err != nil {
		t.Fatalf("canonicalize value: %v", err)
	}
	got := string(out)
	if got != `{"a":2,"list":[3,1,2],"z":1}` {
		t.Errorf("canonicalize value drift: %q", got)
	}
}

func TestCanonicalizeValueOnUnencodable(t *testing.T) {
	// A function isn't JSON-encodable; the marshal step must
	// surface the error rather than producing garbage bytes.
	_, err := federation.CanonicalizeValue(map[string]any{"f": func() {}})
	if err == nil {
		t.Error("encoding a function value should fail")
	}
}

func TestCanonicalizeDeterministic(t *testing.T) {
	// Two different lexical orderings of the same logical object
	// produce identical canonical bytes — the property the
	// envelope signature depends on.
	a, _ := federation.Canonicalize([]byte(`{"a":1,"b":2}`))
	b, _ := federation.Canonicalize([]byte(`{"b":2,"a":1}`))
	if string(a) != string(b) {
		t.Errorf("canonicalize not deterministic across key order: %q vs %q", a, b)
	}
}
