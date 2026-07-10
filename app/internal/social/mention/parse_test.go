package mention

import (
	"reflect"
	"testing"
)

func usernames(ms []Mention) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Username)
	}
	return out
}

func TestParseMentions_ValidUsernames(t *testing.T) {
	got := usernames(ParseMentions("hey @alice and @bob_42, look at this"))
	want := []string{"alice", "bob_42"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseMentions_ExcludesCodeFences(t *testing.T) {
	// Fenced block + inline code both carry an @; neither is a mention.
	text := "ping @real\n```\nsudo -u @notme run\n```\nand inline `@alsonot` here"
	got := usernames(ParseMentions(text))
	want := []string{"real"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseMentions_ExcludesLinks(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"markdown link url", "see [profile](https://x.test/@handle) and @real", []string{"real"}},
		{"bare url", "https://x.test/users/@ghost pinged @real", []string{"real"}},
		{"mailto", "email me at mailto:foo@bar.test — cc @real", []string{"real"}},
		{"email in prose", "reach me foo@bar.test but @real is the mention", []string{"real"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := usernames(ParseMentions(tc.text))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseMentions_MultipleInOneBody(t *testing.T) {
	got := usernames(ParseMentions("@a @b @c @a @B"))
	// De-duplicated case-insensitively, first-seen order preserved.
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseMentions_ReturnsInstanceHostEmpty(t *testing.T) {
	// Federation seam: v0.1.0 never populates InstanceHost, even when
	// the text looks like @user@host (the second @host is just more
	// text — the regex stops at the first non-word char).
	ms := ParseMentions("@alice@peer.test")
	if len(ms) == 0 {
		t.Fatal("expected at least one mention")
	}
	for _, m := range ms {
		if m.InstanceHost != "" {
			t.Fatalf("InstanceHost must be empty in v0.1.0, got %q", m.InstanceHost)
		}
	}
	// @alice resolves; the @peer.test tail is dropped (peer preceded by
	// '.', a non-word boundary, so "test" would match — but "peer" is
	// preceded by '@' which is fine... assert alice is present).
	if ms[0].Username != "alice" {
		t.Fatalf("first mention should be alice, got %q", ms[0].Username)
	}
}

func TestParseMentions_NoMentions(t *testing.T) {
	for _, text := range []string{"", "no at signs here", "email foo@bar.test only"} {
		if got := ParseMentions(text); len(got) != 0 {
			t.Fatalf("text %q: expected no mentions, got %v", text, usernames(got))
		}
	}
}

func TestParseMentions_UsernameLengthCap(t *testing.T) {
	// The regex caps at 32 chars (matching the registration pattern
	// [a-zA-Z0-9_-]{3,32}). A longer run captures the first 32.
	long := "@" + repeat("a", 60)
	got := ParseMentions(long)
	if len(got) != 1 {
		t.Fatalf("expected 1 mention, got %d", len(got))
	}
	if len(got[0].Username) != 32 {
		t.Fatalf("expected username capped at 32, got %d", len(got[0].Username))
	}
}

func TestParseMentions_HyphenatedUsername(t *testing.T) {
	// Usernames may contain hyphens (auth.registerUsernamePattern);
	// the whole thing must be captured, not truncated at the hyphen.
	got := usernames(ParseMentions("welcome @mary-jane_99 to the team"))
	want := []string{"mary-jane_99"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
