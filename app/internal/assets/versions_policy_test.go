// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1596: the access selection policy. Pure: no database, no bytes.

package assets_test

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/asset/metadata"
	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/viewkind"
)

// Every key some preview handler writes today, ladder and non-ladder.
var everyWrittenKey = []string{
	"original", "col", "preview", "screen", "hires",
	"hls/master.m3u8", "hls/1080p/index.m3u8", "hls/1080p/seg_000.ts", "hls/1080p.mp4",
	"poster", "sprites.jpg", "sprites.vtt", "turntable/0000.png", "turntable/0035.png",
	"views/front.png", "iso", "cover", "embedded-preview", "sub.en.vtt",
}

func choice(role assets.AccessRole, key string) assets.AccessChoice {
	return assets.AccessChoice{Role: role, VariantKey: key}
}

func TestSelectAccessVariants_PerKind(t *testing.T) {
	ladder := sysconfig.DefaultPreviewConfig()
	preview := choice(assets.AccessRolePreview, "preview")
	cases := []struct {
		name      string
		kind      viewkind.Kind
		available []string
		want      []assets.AccessChoice
	}{
		{"image: smallest contain rung + largest", viewkind.KindImage, everyWrittenKey,
			[]assets.AccessChoice{preview, choice(assets.AccessRoleAccess, "hires")}},
		{"image without hires: screen is the largest", viewkind.KindImage, []string{"col", "preview", "screen"},
			[]assets.AccessChoice{preview, choice(assets.AccessRoleAccess, "screen")}},
		{"image with one contain rung: a preview only, never twice", viewkind.KindImage, []string{"col", "hires"},
			[]assets.AccessChoice{choice(assets.AccessRolePreview, "hires")}},
		{"image with only the cropped col: nothing", viewkind.KindImage, []string{"original", "col"}, nil},
		{"video: preview only, HLS never", viewkind.KindVideo, everyWrittenKey, []assets.AccessChoice{preview}},
		{"3d: preview only, turntable and views never", viewkind.Kind3D, everyWrittenKey, []assets.AccessChoice{preview}},
		{"pdf: preview only", viewkind.KindPDF, everyWrittenKey, []assets.AccessChoice{preview}},
		{"audio: preview only, the cover never", viewkind.KindAudio, everyWrittenKey, []assets.AccessChoice{preview}},
		{"archive: unsupported", viewkind.KindArchive, everyWrittenKey, nil},
		{"placeholder: unsupported", viewkind.KindPlaceholder, everyWrittenKey, nil},
		{"unknown kind: unsupported", viewkind.Kind("hologram"), everyWrittenKey, nil},
		{"nothing written yet", viewkind.KindImage, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := assets.SelectAccessVariants(c.kind, ladder, c.available); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestSelectAccessVariants_OnlyContainLadderRungsAreCandidates(t *testing.T) {
	// A ladder an operator retuned, including keys that must never be a
	// candidate even when configured: the original, a path-like key, a
	// cover rung, a zero cap and a duplicate.
	ladder := sysconfig.PreviewConfig{Variants: []sysconfig.PreviewVariant{
		{Key: "original", Fit: sysconfig.PreviewFitContain, MaxDim: 99999},
		{Key: "hls/1080p.mp4", Fit: sysconfig.PreviewFitContain, MaxDim: 8000},
		{Key: `win\path`, Fit: sysconfig.PreviewFitContain, MaxDim: 7000},
		{Key: "square", Fit: sysconfig.PreviewFitCover, MaxDim: 6000},
		{Key: "zero", Fit: sysconfig.PreviewFitContain, MaxDim: 0},
		{Key: "small", Fit: sysconfig.PreviewFitContain, MaxDim: 800},
		{Key: "big", Fit: sysconfig.PreviewFitContain, MaxDim: 3000},
		{Key: "small", Fit: sysconfig.PreviewFitContain, MaxDim: 5000},
	}}
	available := []string{"original", "hls/1080p.mp4", `win\path`, "square", "zero", "small", "big"}
	want := []assets.AccessChoice{choice(assets.AccessRolePreview, "small"), choice(assets.AccessRoleAccess, "big")}
	if got := assets.SelectAccessVariants(viewkind.KindImage, ladder, available); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSelectAccessVariants_Deterministic(t *testing.T) {
	// Equal caps break on the key; input order never matters.
	ladder := sysconfig.PreviewConfig{Variants: []sysconfig.PreviewVariant{
		{Key: "b-large", Fit: sysconfig.PreviewFitContain, MaxDim: 2000},
		{Key: "a-large", Fit: sysconfig.PreviewFitContain, MaxDim: 2000},
		{Key: "z-small", Fit: sysconfig.PreviewFitContain, MaxDim: 500},
		{Key: "y-small", Fit: sysconfig.PreviewFitContain, MaxDim: 500},
	}}
	keys := []string{"b-large", "a-large", "z-small", "y-small", "col", "hls/master.m3u8"}
	want := []assets.AccessChoice{choice(assets.AccessRolePreview, "y-small"), choice(assets.AccessRoleAccess, "b-large")}
	rng := rand.New(rand.NewSource(1596))
	for i := 0; i < 50; i++ {
		rng.Shuffle(len(keys), func(a, b int) { keys[a], keys[b] = keys[b], keys[a] })
		vs := append([]sysconfig.PreviewVariant(nil), ladder.Variants...)
		rng.Shuffle(len(vs), func(a, b int) { vs[a], vs[b] = vs[b], vs[a] })
		got := assets.SelectAccessVariants(viewkind.KindImage, sysconfig.PreviewConfig{Variants: vs}, keys)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("shuffle %d: got %+v, want %+v", i, got, want)
		}
	}
}

func TestSelectAccessVariants_ReservedKeysNeverChosenEvenWhenConfigured(t *testing.T) {
	// sysconfig accepts any key as a contain rung. Each name another writer
	// uses for non-ladder bytes, configured larger than every real rung,
	// must still never be chosen; a legitimate custom key still is.
	reserved := []string{
		"original", "poster", "cover", metadata.EmbeddedPreviewVariantKey, "iso",
		"sprites.jpg", "sprites.vtt", "sub.en.vtt", "sub.pt-br.vtt",
		"iso/source.png", "views/top.png", "turntable/0000.png", "hls/master.m3u8",
		"Hires", "with space", "",
	}
	ladder := sysconfig.PreviewConfig{Variants: []sysconfig.PreviewVariant{
		{Key: "thumb_s", Fit: sysconfig.PreviewFitContain, MaxDim: 400},
		{Key: "display-4k", Fit: sysconfig.PreviewFitContain, MaxDim: 3840},
	}}
	available := []string{"thumb_s", "display-4k"}
	for i, k := range reserved {
		ladder.Variants = append(ladder.Variants, sysconfig.PreviewVariant{Key: k, Fit: sysconfig.PreviewFitContain, MaxDim: 10000 + i})
		ladder.Variants = append(ladder.Variants, sysconfig.PreviewVariant{Key: k, Fit: sysconfig.PreviewFitContain, MaxDim: 1 + i})
		available = append(available, k)
	}
	want := []assets.AccessChoice{choice(assets.AccessRolePreview, "thumb_s"), choice(assets.AccessRoleAccess, "display-4k")}
	if got := assets.SelectAccessVariants(viewkind.KindImage, ladder, available); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// With only reserved keys configured and present, nothing is chosen.
	only := sysconfig.PreviewConfig{Variants: ladder.Variants[2:]}
	if got := assets.SelectAccessVariants(viewkind.KindImage, only, available); got != nil {
		t.Fatalf("reserved keys alone chose %+v", got)
	}
}
