// Phase 1.18.B-3 sidecar parser tests.
//
// The parser walks a primary asset filename + a list of co-uploaded
// candidates and surfaces (lang, label, format) hints for each
// detectable sidecar. These tests pin the recognition rules + the
// "when in doubt drop it" behaviour for ambiguous inputs.

package subtitles

import "testing"

func TestSidecar_BasenameMatch_NoLang(t *testing.T) {
	hints := ParseSidecars("clip.mp4", []string{"clip.srt"})
	if len(hints) != 1 {
		t.Fatalf("hints=%d, want 1", len(hints))
	}
	if hints[0].Lang != "und" {
		t.Errorf("lang=%q, want und", hints[0].Lang)
	}
	if hints[0].SourceFormat != "srt" {
		t.Errorf("source_format=%q, want srt", hints[0].SourceFormat)
	}
	if hints[0].Filename != "clip.srt" {
		t.Errorf("filename=%q, want clip.srt", hints[0].Filename)
	}
}

func TestSidecar_BasenameMatch_WithLang(t *testing.T) {
	hints := ParseSidecars("clip.mp4", []string{"clip.en.srt"})
	if len(hints) != 1 {
		t.Fatalf("hints=%d, want 1", len(hints))
	}
	if hints[0].Lang != "en" {
		t.Errorf("lang=%q, want en", hints[0].Lang)
	}
}

func TestSidecar_BasenameMatch_WithRegionLang(t *testing.T) {
	hints := ParseSidecars("clip.mp4", []string{"clip.en-US.srt"})
	if len(hints) != 1 {
		t.Fatalf("hints=%d, want 1", len(hints))
	}
	if hints[0].Lang != "en-US" {
		t.Errorf("lang=%q, want en-US", hints[0].Lang)
	}
}

func TestSidecar_BasenameMismatch_Ignored(t *testing.T) {
	hints := ParseSidecars("clip.mp4", []string{"other.srt"})
	if len(hints) != 0 {
		t.Errorf("hints=%d, want 0 (basename mismatch)", len(hints))
	}
}

func TestSidecar_NonSubtitleExt_Ignored(t *testing.T) {
	hints := ParseSidecars("clip.mp4", []string{"clip.txt", "clip.nfo", "clip.jpg"})
	if len(hints) != 0 {
		t.Errorf("hints=%d, want 0 (non-subtitle extensions)", len(hints))
	}
}

func TestSidecar_MultipleSidecars_MultipleHints(t *testing.T) {
	hints := ParseSidecars("clip.mp4", []string{"clip.en.srt", "clip.ja.srt"})
	if len(hints) != 2 {
		t.Fatalf("hints=%d, want 2", len(hints))
	}
	gotLangs := map[string]bool{hints[0].Lang: true, hints[1].Lang: true}
	if !gotLangs["en"] || !gotLangs["ja"] {
		t.Errorf("got langs=%v, want en+ja", gotLangs)
	}
}

func TestSidecar_ForcedFlag_Parsed(t *testing.T) {
	hints := ParseSidecars("clip.mp4", []string{"clip.en.forced.srt"})
	if len(hints) != 1 {
		t.Fatalf("hints=%d, want 1", len(hints))
	}
	if hints[0].Lang != "en" {
		t.Errorf("lang=%q, want en", hints[0].Lang)
	}
	if hints[0].Label != "forced" {
		t.Errorf("label=%q, want 'forced'", hints[0].Label)
	}
}

func TestSidecar_CaseInsensitiveExt(t *testing.T) {
	hints := ParseSidecars("clip.mp4", []string{"clip.SRT"})
	if len(hints) != 1 {
		t.Fatalf("hints=%d, want 1 (uppercase SRT should match)", len(hints))
	}
	if hints[0].SourceFormat != "srt" {
		t.Errorf("source_format=%q, want srt (lowercased)", hints[0].SourceFormat)
	}
}

func TestSidecar_PrimaryFilename_NotAHint(t *testing.T) {
	hints := ParseSidecars("clip.mp4", []string{"clip.mp4", "clip.en.srt"})
	if len(hints) != 1 {
		t.Errorf("hints=%d, want 1 (primary filename should not be returned as its own hint)", len(hints))
	}
	if len(hints) == 1 && hints[0].Filename != "clip.en.srt" {
		t.Errorf("filename=%q, want clip.en.srt", hints[0].Filename)
	}
}

func TestSidecar_PrefixCollision_Ignored(t *testing.T) {
	// "clipper.srt" starts with "clip" but isn't a sibling — the
	// parser must reject this rather than treat "per" as the
	// language tag.
	hints := ParseSidecars("clip.mp4", []string{"clipper.srt"})
	if len(hints) != 0 {
		t.Errorf("hints=%d, want 0 (prefix collision should not match)", len(hints))
	}
}

func TestSidecar_AllFormats_Recognised(t *testing.T) {
	cases := []struct {
		filename string
		want     string
	}{
		{"clip.vtt", "vtt"},
		{"clip.srt", "srt"},
		{"clip.ssa", "ssa"},
		{"clip.ass", "ass"},
		{"clip.sub", "sub"},
		{"clip.idx", "idx"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			hints := ParseSidecars("clip.mp4", []string{c.filename})
			if len(hints) != 1 {
				t.Fatalf("hints=%d, want 1", len(hints))
			}
			if hints[0].SourceFormat != c.want {
				t.Errorf("source_format=%q, want %q", hints[0].SourceFormat, c.want)
			}
		})
	}
}
