// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

// The access selection policy (#1596): which derived variants of an
// asset's bytes become the access copies of its version, by spec §7.5.7's
// byte classes. One place, one rule:
//
//   - A version lists at most one PREVIEW (a display rendition) and at
//     most one ACCESS derivative ("the largest rendition the origin serves
//     for viewing; never the original").
//   - Candidates are ONLY the configured still ladder's `contain` rungs
//     (sysconfig previews: longest-side caps, aspect kept) whose key is a
//     plain ladder token: lowercase letters, digits, `-` and `_`, at most
//     64 characters. That shape alone excludes every dotted or path-like
//     key other writers use (HLS playlists and segments `hls/...`,
//     turntable frames `turntable/...`, view and iso renders `views/...`,
//     `iso/...`, sprite sheets and cues `sprites.jpg`/`sprites.vtt`,
//     subtitle tracks `sub.<lang>.vtt`). The bare names other writers use
//     for non-ladder bytes are reserved and excluded even when an operator
//     configures them as a contain rung (sysconfig accepts any key):
//     `original`, `poster`, `cover`, `embedded-preview`, `iso`. `cover`
//     rungs (cropped squares such as `col`) are excluded by their fit.
//   - The preview is the smallest available rung. The access derivative
//     exists only for kinds whose viewer shows a derived still in place
//     of the original (images): the largest available rung, when it is
//     not the rung already chosen as the preview. Every other kind's
//     viewer shows its original, or plays a stream that is not a single
//     file (video HLS), so it gets a preview only.
//   - Kinds with no still ladder (archives, placeholders, unknown) get
//     nothing.
//   - Deterministic: rungs are ordered by their cap, then by key, so the
//     same ladder and the same available keys always give the same
//     choice, whatever order they arrive in.
//
// The policy only chooses keys. Whether a chosen variant is verified is
// promotion's decision (storage.Service.PromoteVariant), which refuses
// anything unverified; nothing here reads bytes or reaches a peer.

import (
	"regexp"
	"sort"

	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/viewkind"
)

// AccessRole is a version file role chosen by the policy (§17.2.2).
type AccessRole string

const (
	AccessRolePreview AccessRole = "preview"
	AccessRoleAccess  AccessRole = "access"
)

// AccessChoice is one variant the policy selected.
type AccessChoice struct {
	Role       AccessRole
	VariantKey string
}

// ladderKey is the shape of a key the policy can choose: a plain token.
var ladderKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// reservedVariantKeys are the bare keys other writers use for bytes that
// are not ladder rungs (the original, the video poster frame, the audio
// cover, the metadata adapter's embedded preview, the 3D iso render).
var reservedVariantKeys = map[string]bool{
	storage.VariantOriginal: true,
	"poster":                true,
	"cover":                 true,
	"embedded-preview":      true,
	"iso":                   true,
}

// IsAccessCandidateKey reports whether a variant key has the shape the
// policy can ever choose: a plain ladder token that no other writer
// reserves. The version-reconcile trigger for variant writes uses it to
// ignore keys that can never change a version (HLS segments, frames,
// sprites, covers).
func IsAccessCandidateKey(key string) bool {
	return ladderKey.MatchString(key) && !reservedVariantKeys[key]
}

// accessDisplayKinds are the kinds whose viewer shows a derived still in
// place of the original, so their largest rung is an access derivative.
var accessDisplayKinds = map[viewkind.Kind]bool{
	viewkind.KindImage: true,
}

// previewKinds are the kinds whose processing writes the still ladder
// (preview handlers call fanToLadder): they get a preview.
var previewKinds = map[viewkind.Kind]bool{
	viewkind.KindImage:     true,
	viewkind.KindVideo:     true,
	viewkind.KindPDF:       true,
	viewkind.KindAudio:     true,
	viewkind.KindAudiobook: true,
	viewkind.KindSequence:  true,
	viewkind.KindFont:      true,
	viewkind.KindSprite:    true,
	viewkind.Kind3D:        true,
	viewkind.KindEbook:     true,
	viewkind.KindDoc:       true,
}

// SelectAccessVariants returns the policy's choice for an asset of kind,
// given the configured ladder and the variant keys that exist for its
// bytes: at most one preview, then at most one access derivative, in that
// order.
func SelectAccessVariants(kind viewkind.Kind, ladder sysconfig.PreviewConfig, available []string) []AccessChoice {
	if !previewKinds[kind] {
		return nil
	}
	have := make(map[string]bool, len(available))
	for _, k := range available {
		have[k] = true
	}
	var rungs []sysconfig.PreviewVariant
	seen := map[string]bool{}
	for _, v := range ladder.Variants {
		if v.Fit != sysconfig.PreviewFitContain || v.MaxDim <= 0 || !have[v.Key] || seen[v.Key] ||
			!IsAccessCandidateKey(v.Key) {
			continue
		}
		seen[v.Key] = true
		rungs = append(rungs, v)
	}
	if len(rungs) == 0 {
		return nil
	}
	sort.Slice(rungs, func(i, j int) bool {
		if rungs[i].MaxDim != rungs[j].MaxDim {
			return rungs[i].MaxDim < rungs[j].MaxDim
		}
		return rungs[i].Key < rungs[j].Key
	})
	out := []AccessChoice{{Role: AccessRolePreview, VariantKey: rungs[0].Key}}
	if accessDisplayKinds[kind] && len(rungs) > 1 {
		out = append(out, AccessChoice{Role: AccessRoleAccess, VariantKey: rungs[len(rungs)-1].Key})
	}
	return out
}
