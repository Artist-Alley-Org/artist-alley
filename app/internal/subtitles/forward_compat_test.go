// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.18.B-3 — federation forward-compatibility tests.
//
// The subtitle-tracks feature surfaces a new optional `subtitle_tracks`
// array on the Asset payload. Per the ArchivePub forward-compat
// clause, additive fields on existing object kinds are soak-safe
// (peers MUST ignore unknown fields, not reject the activity); these
// tests pin that contract so future changes can't regress it.
//
// What's tested:
//
//   1. A peer running the OLD schema (no subtitle_tracks awareness)
//      decoding an Asset with subtitle_tracks set → no error, the
//      known fields round-trip cleanly.
//   2. A peer running the NEW schema decoding an Asset emitted by
//      an OLD peer (no subtitle_tracks field) → no error, the new
//      field defaults to nil/empty.
//   3. The subtitle_tracks field marshals to the documented JSON
//      shape (lang/label/file_hash/source_format/confidence/created_at).
//
// These run as pure-Go tests (no Postgres dependency).

package subtitles

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// oldPeerAsset mimics the asset shape a peer would have decoded
// BEFORE the subtitle-tracks feature shipped — every Asset field
// except subtitle_tracks. Decoding the new wire shape into this
// struct must NOT error.
type oldPeerAsset struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Status      string  `json:"status"`
	OwnerRef    int64   `json:"owner_user_ref"`
	AssetType   int64   `json:"asset_type"`
	StorageHash *string `json:"file_hash,omitempty"`
}

func TestForwardCompat_OldPeerDecodesNewAsset(t *testing.T) {
	// Synthesize the wire shape a new instance would emit, with
	// subtitle_tracks populated.
	newWire := map[string]any{
		"id":             uuid.New().String(),
		"title":          "A Video With Subs",
		"status":         "active",
		"owner_user_ref": int64(42),
		"asset_type":     int64(3),
		"file_hash":      strings.Repeat("a", 64),
		"subtitle_tracks": []map[string]any{
			{
				"lang":          "en",
				"label":         "English",
				"file_hash":     strings.Repeat("b", 64),
				"source_format": "srt",
				"confidence":    1.0,
				"created_at":    time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	wireBytes, err := json.Marshal(newWire)
	if err != nil {
		t.Fatalf("marshal new wire: %v", err)
	}

	var old oldPeerAsset
	if err := json.Unmarshal(wireBytes, &old); err != nil {
		t.Fatalf("old peer decoder errored on new wire — additive field broke wire compat: %v", err)
	}
	if old.Title != "A Video With Subs" {
		t.Errorf("known fields didn't round-trip: title=%q", old.Title)
	}
	if old.StorageHash == nil || *old.StorageHash != strings.Repeat("a", 64) {
		t.Errorf("known fields didn't round-trip: file_hash=%v", old.StorageHash)
	}
}

func TestForwardCompat_NewPeerDecodesOldAsset(t *testing.T) {
	// An old instance emits the wire WITHOUT subtitle_tracks at
	// all. The new peer's openapi.Asset must decode cleanly with
	// the field defaulting to nil.
	oldWire := []byte(`{
		"id": "` + uuid.New().String() + `",
		"title": "A Pre-Subs Video",
		"status": "active",
		"owner_user_ref": 42,
		"asset_type": 3,
		"file_hash": "` + strings.Repeat("c", 64) + `"
	}`)

	var asset openapi.Asset
	if err := json.Unmarshal(oldWire, &asset); err != nil {
		t.Fatalf("new peer decoder errored on old wire: %v", err)
	}
	if asset.SubtitleTracks != nil && len(*asset.SubtitleTracks) > 0 {
		t.Errorf("subtitle_tracks should default to nil/empty on pre-subs wire, got %d entries", len(*asset.SubtitleTracks))
	}
	if asset.Title == nil || *asset.Title != "A Pre-Subs Video" {
		t.Errorf("known fields didn't round-trip: title=%v", asset.Title)
	}
}

func TestForwardCompat_SubtitleTrackWireShape(t *testing.T) {
	// Pin the documented JSON shape so any rename / case change
	// in the OpenAPI schema would surface here as a clear failure.
	track := openapi.SubtitleTrack{
		Lang:         "ja",
		Label:        ptrString("日本語"),
		FileHash:     strings.Repeat("d", 64),
		SourceFormat: openapi.SubtitleTrackSourceFormat("srt"),
		Confidence:   1.0,
	}
	b, err := json.Marshal(track)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(b)
	for _, key := range []string{
		`"lang":"ja"`,
		`"label":"日本語"`,
		`"file_hash":"` + strings.Repeat("d", 64) + `"`,
		`"source_format":"srt"`,
		`"confidence":1`,
	} {
		if !strings.Contains(wire, key) {
			t.Errorf("wire shape missing %q\nfull: %s", key, wire)
		}
	}
}

func ptrString(s string) *string { return &s }
