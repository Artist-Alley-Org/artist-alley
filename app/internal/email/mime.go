package email

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"net/mail"
	"sort"
	"strings"
	"time"
)

// buildMIME assembles the raw RFC 5322 + MIME bytes the SMTP
// DATA command swallows. Two flavours:
//
//   - HTMLBody empty → single text/plain part.
//   - HTMLBody non-empty → multipart/alternative with text/plain
//     and text/html parts, text-first (RFC 2046 § 5.1.4 prefers
//     the last-listed alternative; we list text first so clients
//     that can't parse multipart degrade to text).
//
// Subjects with non-ASCII characters get RFC 2047 word-encoded.
// We do NOT word-encode display names — operators set them once
// in sysconfig and bring their own ASCII.
func buildMIME(msg Message) ([]byte, error) {
	now := time.Now().UTC().Format(time.RFC1123Z)

	headers := map[string]string{
		"From":         msg.From,
		"To":           strings.Join(msg.To, ", "),
		"Subject":      maybeEncodeWord(msg.Subject),
		"Date":         now,
		"MIME-Version": "1.0",
	}
	for k, v := range msg.Headers {
		// Caller-supplied headers win when set, but the
		// sender-managed ones above are not overridable.
		if _, locked := headers[k]; locked {
			continue
		}
		headers[k] = v
	}

	var buf bytes.Buffer
	if msg.HTMLBody == "" {
		headers["Content-Type"] = "text/plain; charset=utf-8"
		headers["Content-Transfer-Encoding"] = "8bit"
		writeHeaders(&buf, headers)
		buf.WriteString("\r\n")
		buf.WriteString(normaliseEOL(msg.TextBody))
		return buf.Bytes(), nil
	}

	boundary, err := mimeBoundary()
	if err != nil {
		return nil, err
	}
	headers["Content-Type"] = `multipart/alternative; boundary="` + boundary + `"`
	writeHeaders(&buf, headers)
	buf.WriteString("\r\n")

	// text/plain part
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	buf.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	buf.WriteString(normaliseEOL(msg.TextBody))
	buf.WriteString("\r\n")

	// text/html part
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	buf.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	buf.WriteString(normaliseEOL(msg.HTMLBody))
	buf.WriteString("\r\n")

	buf.WriteString("--" + boundary + "--\r\n")
	return buf.Bytes(), nil
}

func writeHeaders(buf *bytes.Buffer, headers map[string]string) {
	// Stable order so tests can string-compare bodies + relays
	// don't see different orderings per send.
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		buf.WriteString(k)
		buf.WriteString(": ")
		buf.WriteString(headers[k])
		buf.WriteString("\r\n")
	}
}

// normaliseEOL coerces \n → \r\n. SMTP DATA wants CRLF; many
// callers compose with Go's \n. Idempotent on already-CRLF input.
func normaliseEOL(s string) string {
	// Replace bare \n only when not already preceded by \r.
	if !strings.Contains(s, "\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' && (i == 0 || s[i-1] != '\r') {
			b.WriteString("\r\n")
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// maybeEncodeWord wraps the value in RFC 2047 =?utf-8?b?...?= when
// it carries non-ASCII bytes. ASCII subjects round-trip verbatim.
func maybeEncodeWord(s string) string {
	for _, r := range s {
		if r > 127 {
			return mime.BEncoding.Encode("utf-8", s)
		}
	}
	return s
}

func mimeBoundary() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("email: random boundary: %w", err)
	}
	return "AA_boundary_" + hex.EncodeToString(b[:]), nil
}

// mailParseAddress is the shared address parser the SMTPSender + the
// Capture validator share. Returns the bare address (no display name).
func mailParseAddress(s string) (string, error) {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return "", err
	}
	return a.Address, nil
}
