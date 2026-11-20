// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package federation_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// #1574 sender: an aa:AssetVersion of an asset federates; its only wire
// member is the aa:asset the delivery worker built, sent only to a peer
// with archival-1 and original-transfer-1 and only when it matches §17.2.

const avAsset = "https://a.example/assets/6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11"

func avMember(t *testing.T) json.RawMessage {
	t.Helper()
	h := strings.Repeat("ab", 32)
	raw, err := json.Marshal(map[string]any{
		"id":      avAsset,
		"version": map[string]any{"id": avAsset + "/versions/1", "number": 1, "created": "2026-11-20T10:00:00Z", "predecessor": nil},
		"files": []map[string]any{{"role": "original", "contentUri": "cas://" + h,
			"checksums": []map[string]string{{"algorithm": "sha256", "value": h}}, "size": 3, "mediaType": "image/png"}},
		"sensitivity": "public", "mature": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// avPayload is a local record plus the built member, as the delivery
// worker hands it to WireMembers.
func avPayload(t *testing.T, member json.RawMessage) []byte {
	t.Helper()
	local := map[string]json.RawMessage{
		"share_id":          json.RawMessage(`"0b2b4f5e-6a3b-4c1d-9e8f-1a2b3c4d5e6f"`),
		"share_activity_id": json.RawMessage(`"1b2b4f5e-6a3b-4c1d-9e8f-1a2b3c4d5e6f"`),
		"asset_id":          json.RawMessage(`"6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11"`),
		"version_id":        json.RawMessage(`"2b2b4f5e-6a3b-4c1d-9e8f-1a2b3c4d5e6f"`),
	}
	if member != nil {
		local[federation.AssetVersionMemberKey] = member
	}
	b, err := json.Marshal(local)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFederated_AssetVersionOfAnAssetOnly(t *testing.T) {
	if !federation.Federated(federation.ActivityAAAssetVersion, "asset", nil) {
		t.Fatal("an aa:AssetVersion of an asset does not federate")
	}
	for _, kind := range []string{"", "post", "collection", "comment"} {
		if federation.Federated(federation.ActivityAAAssetVersion, kind, nil) {
			t.Errorf("an aa:AssetVersion of %q federates", kind)
		}
	}
}

func TestWireMembers_AssetVersion(t *testing.T) {
	both := func(tok string) bool {
		return tok == federation.ProfileArchival1 || tok == federation.ProfileOriginalTransfer1
	}
	for name, negotiated := range map[string]func(string) bool{
		"no profile":               nil,
		"archival-1 only":          func(tok string) bool { return tok == federation.ProfileArchival1 },
		"original-transfer-1 only": func(tok string) bool { return tok == federation.ProfileOriginalTransfer1 },
	} {
		_, err := federation.WireMembers(federation.ActivityAAAssetVersion, "asset", avPayload(t, avMember(t)), negotiated)
		if !errors.Is(err, federation.ErrNotFederated) || !errors.Is(err, federation.ErrProfileNotNegotiated) {
			t.Errorf("%s: %v, want not federated: profile not negotiated", name, err)
		}
	}

	wire, err := federation.WireMembers(federation.ActivityAAAssetVersion, "asset", avPayload(t, avMember(t)), both)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != 1 || string(wire[federation.MemberArchivalAsset]) != string(avMember(t)) {
		t.Fatalf("wire members %v, want exactly the built aa:asset", wire)
	}

	// No member built, or one §17.2 refuses: never sent.
	if _, err := federation.WireMembers(federation.ActivityAAAssetVersion, "asset", avPayload(t, nil), both); !errors.Is(err, federation.ErrNotFederated) {
		t.Errorf("without a member: %v", err)
	}
	var bad map[string]any
	_ = json.Unmarshal(avMember(t), &bad)
	bad["files"] = []any{}
	raw, _ := json.Marshal(bad)
	if _, err := federation.WireMembers(federation.ActivityAAAssetVersion, "asset", avPayload(t, raw), both); !errors.Is(err, federation.ErrNotFederated) ||
		!errors.Is(err, federation.ErrArchivalInvalid) {
		t.Errorf("an empty file set: %v, want not federated: archival invalid", err)
	}
	if _, err := federation.WireMembers(federation.ActivityAAAssetVersion, "post", avPayload(t, avMember(t)), both); !errors.Is(err, federation.ErrNotFederated) {
		t.Errorf("of a post: %v", err)
	}
}
