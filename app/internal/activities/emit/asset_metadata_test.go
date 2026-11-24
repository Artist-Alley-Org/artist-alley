// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package emit

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// #1577: a metadata Update is a flags Update of one peer with the sealed
// version's id and the capture, as one string, added to its local record;
// no wire member is in the record.
func TestAssetMetadataUpdate_TheLocalRecord(t *testing.T) {
	actor := ActorContext{UserRef: 7, Username: "alice", BaseURL: "https://local.example"}
	asset, share, shareActivity, version := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	published := time.Date(2026, 11, 23, 10, 0, 0, 123000, time.UTC)
	capture := `{"v":1,"snapshot":{"title":"<b>Harbour</b>","description":null,"rights":null,"requiredStatement":null}}`
	ref := AssetMetadataUpdateRef{
		AssetFlagUpdateRef: AssetFlagUpdateRef{AssetID: asset, ShareID: share, ShareActivityID: shareActivity, PeerURL: "https://peer.example",
			VersionNumber: 3, Sensitivity: "team", Flags: AssetFlags{Mature: true, AIProvenance: "assisted"}, Published: published},
		VersionID: version,
		Capture:   capture,
	}
	em := AssetMetadataUpdate(actor, ref)
	flags := AssetFlagUpdate(actor, ref.AssetFlagUpdateRef)
	a := em.Activity
	if a.Type != federation.ActivityUpdate || a.ActorURI != actor.URI() || a.ActorUserRef == nil || *a.ActorUserRef != 7 ||
		!slices.Equal(a.To, []string{"https://peer.example"}) || !a.Published.Equal(published) || len(em.Notifications) != 0 {
		t.Fatalf("activity %+v", a)
	}
	if a.Object == nil || a.Object.Kind != activities.ObjectKindAsset || a.Object.LocalID != asset.String() ||
		a.Object.URI != actor.ObjectURI(activities.ObjectKindAsset, asset.String()) {
		t.Fatalf("object %+v", a.Object)
	}
	if a.ActivityURI == flags.Activity.ActivityURI || a.ActivityURI == "" {
		t.Fatal("not a new activity id")
	}
	want := map[string]any{}
	for k, v := range flags.Activity.Payload {
		want[k] = v
	}
	want["version_id"] = version.String()
	want[federation.MetadataCaptureKey] = capture
	got, _ := json.Marshal(a.Payload)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Fatalf("payload\n got %s\nwant %s", got, exp)
	}
	if s, ok := a.Payload[federation.MetadataCaptureKey].(string); !ok || s != capture {
		t.Fatalf("the capture is %T", a.Payload[federation.MetadataCaptureKey])
	}
	for _, k := range []string{federation.AssetVersionMemberKey, federation.AssetMetadataMemberKey, federation.MemberAssetMetadata,
		federation.MemberArchivalAsset, "descriptive"} {
		if _, ok := a.Payload[k]; ok {
			t.Errorf("the local record holds %q", k)
		}
	}
}
