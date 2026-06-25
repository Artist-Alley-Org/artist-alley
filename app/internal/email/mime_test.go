package email

import (
	"strings"
	"testing"
)

func TestBuildMIME_PlainTextOnly(t *testing.T) {
	msg := Message{
		From: "ops@example.com", To: []string{"a@b.c"},
		Subject: "hello", TextBody: "line1\nline2",
	}
	body, err := buildMIME(msg)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "Content-Type: text/plain; charset=utf-8\r\n") {
		t.Errorf("missing text/plain content-type:\n%s", got)
	}
	if !strings.Contains(got, "Subject: hello\r\n") {
		t.Errorf("missing subject:\n%s", got)
	}
	if !strings.Contains(got, "line1\r\nline2") {
		t.Errorf("LF should be normalised to CRLF:\n%s", got)
	}
	if strings.Contains(got, "multipart/") {
		t.Errorf("plain-text message should NOT be multipart")
	}
}

func TestBuildMIME_MultipartAlternativeWhenHTMLPresent(t *testing.T) {
	msg := Message{
		From: "ops@example.com", To: []string{"a@b.c"},
		Subject: "hello", TextBody: "plain", HTMLBody: "<p>html</p>",
	}
	body, err := buildMIME(msg)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, `multipart/alternative; boundary="AA_boundary_`) {
		t.Errorf("expected multipart/alternative with AA_boundary_ prefix:\n%s", got)
	}
	if !strings.Contains(got, "Content-Type: text/plain") {
		t.Errorf("missing text/plain part:\n%s", got)
	}
	if !strings.Contains(got, "Content-Type: text/html") {
		t.Errorf("missing text/html part:\n%s", got)
	}
	// Plain MUST come before HTML — RFC 2046 § 5.1.4 prefers
	// last-listed alternative, so text-first protects terminal
	// clients from getting HTML when they can't render it.
	plainIdx := strings.Index(got, "text/plain")
	htmlIdx := strings.Index(got, "text/html")
	if plainIdx < 0 || htmlIdx < 0 || plainIdx > htmlIdx {
		t.Errorf("plain part should come before html part (plain@%d, html@%d)", plainIdx, htmlIdx)
	}
}

func TestBuildMIME_NonASCIISubjectIsBase64Encoded(t *testing.T) {
	msg := Message{
		From: "ops@example.com", To: []string{"a@b.c"},
		Subject: "こんにちは", TextBody: "hi",
	}
	body, err := buildMIME(msg)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "=?utf-8?b?") {
		t.Errorf("non-ASCII subject should be RFC 2047 b-word-encoded:\n%s", got)
	}
	if strings.Contains(got, "Subject: こんにちは") {
		t.Errorf("raw non-ASCII subject leaked into the header line")
	}
}

func TestBuildMIME_CallerHeadersDoNotOverrideSenderManaged(t *testing.T) {
	msg := Message{
		From: "ops@example.com", To: []string{"a@b.c"},
		Subject: "real", TextBody: "y",
		Headers: map[string]string{
			"Subject":          "MALICIOUS",
			"List-Unsubscribe": "<https://example.com/unsub>",
		},
	}
	body, err := buildMIME(msg)
	if err != nil {
		t.Fatalf("buildMIME: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "Subject: real\r\n") {
		t.Errorf("sender-managed Subject was overridden by caller header:\n%s", got)
	}
	if !strings.Contains(got, "List-Unsubscribe: <https://example.com/unsub>") {
		t.Errorf("caller-supplied custom header missing:\n%s", got)
	}
}

func TestNormaliseEOL_IdempotentOnCRLF(t *testing.T) {
	in := "a\r\nb\r\nc"
	if got := normaliseEOL(in); got != in {
		t.Errorf("normaliseEOL(%q) = %q, want unchanged", in, got)
	}
}

func TestMailParseAddress_ExtractsBareAddr(t *testing.T) {
	got, err := mailParseAddress("Display Name <user@example.com>")
	if err != nil {
		t.Fatalf("mailParseAddress: %v", err)
	}
	if got != "user@example.com" {
		t.Errorf("got %q, want user@example.com", got)
	}
}
