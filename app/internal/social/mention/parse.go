package mention

import (
	"regexp"
	"strings"
)

// Mention is a single parsed @-reference. InstanceHost is the
// federation seam: "" means a local username; a populated host (e.g.
// "peer.example.com") is a federated actor resolved via WebFinger in a
// later phase. v0.1.0 only ever produces InstanceHost == "".
type Mention struct {
	Username     string
	InstanceHost string
}

// mentionRe matches @username where username is 1..32 chars from the
// registration charset [a-zA-Z0-9_-] (see auth.registerUsernamePattern:
// ^[a-zA-Z0-9_-]{3,32}$). Hyphens are valid in usernames, so a
// \w-only class would truncate "@mary-jane" to "@mary" and never
// resolve the real user — the class must match what registration
// allows. The greedy match takes the longest candidate; the resolver
// drops it if no such user exists.
//
// The preceding boundary is enforced in code (not the regex) so we can
// reject emails like "foo@bar" — an @ must be preceded by start-of-
// string or a non-username char, never by a username char.
var mentionRe = regexp.MustCompile(`@([a-zA-Z0-9_-]{1,32})`)

// Regions we blank out before scanning, so a @user inside them never
// registers. Order matters: fenced blocks first (they can contain
// backticks + link syntax), then inline code, then links.
var (
	fencedCodeRe = regexp.MustCompile("(?s)```.*?```")
	inlineCodeRe = regexp.MustCompile("`[^`\n]*`")
	// Markdown link: [text](url) — the url often carries @ (mailto:,
	// query params). Blank the whole construct.
	mdLinkRe = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)`)
	// Bare URL up to the next whitespace. Covers http(s) + mailto so a
	// pasted "mailto:foo@bar.com" or "https://x/@handle" isn't a hit.
	bareURLRe = regexp.MustCompile(`(?i)\b(?:https?://|mailto:)\S+`)
)

// ParseMentions extracts unique local @username mentions from text,
// excluding any that fall inside code fences, inline code, or links.
// Results are de-duplicated case-insensitively (the first-seen casing
// is preserved) and returned in first-seen order for stable output.
//
// Pure function — no I/O. InstanceHost is always "" in v0.1.0.
func ParseMentions(text string) []Mention {
	if text == "" || !strings.Contains(text, "@") {
		return nil
	}

	// Blank out excluded regions with spaces of equal length so byte
	// offsets stay aligned (not that we use them, but it keeps the
	// masked text the same shape and avoids gluing adjacent words).
	masked := text
	for _, re := range []*regexp.Regexp{fencedCodeRe, inlineCodeRe, mdLinkRe, bareURLRe} {
		masked = re.ReplaceAllStringFunc(masked, func(m string) string {
			return strings.Repeat(" ", len(m))
		})
	}

	var out []Mention
	seen := make(map[string]struct{})
	for _, loc := range mentionRe.FindAllStringSubmatchIndex(masked, -1) {
		atPos := loc[0] // index of '@'
		// Boundary check: the char before '@' must not be a word char
		// (rejects emails "foo@bar", "a@b"). Start-of-string is fine.
		if atPos > 0 {
			prev := masked[atPos-1]
			if isWordByte(prev) {
				continue
			}
		}
		username := masked[loc[2]:loc[3]]
		key := strings.ToLower(username)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Mention{Username: username, InstanceHost: ""})
	}
	return out
}

// isWordByte reports whether b is a \w byte (ASCII letter, digit, or
// underscore). Matches the character class the mention regex uses.
func isWordByte(b byte) bool {
	return b == '_' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}
