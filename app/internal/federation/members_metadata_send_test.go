// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1577: the sender's projection of a metadata Update (WireMembers). A
// local record carrying a capture is projected to exactly aa:asset and
// aa:metadata, only under all three profiles, and refused whole for
// anything else: it is never stripped to a flags-only Update.

package federation_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// The local keys of a metadata Update's record, as the delivery worker
// fills it (federation.MetadataCaptureKey, federation.AssetMetadataMemberKey).
const (
	sendCaptureKey = "metadata_capture"
	sendBuiltKey   = "asset_metadata"
)

const sendFourNull = `{"title":null,"description":null,"rights":null,"requiredStatement":null}`

func sendAll(string) bool { return true }

func sendWithout(missing string) func(string) bool {
	return func(tok string) bool { return tok != missing }
}

// sendPayload is a metadata Update's local record after the worker built
// its members: local fields, the capture, the built aa:asset and the
// built aa:metadata (nil leaves a key out).
func sendPayload(t *testing.T, asset, metadata json.RawMessage, capture bool) []byte {
	t.Helper()
	m := map[string]any{
		"share_id": "8d2e6c1a-1111-4222-8333-444455556666", "asset_id": "6f1c2b8e-2d4a-4c1e-9b7a-0c3d5e7f9a11",
		"version_number": 1, "sensitivity": "public", "mature": false, "notes": "LOCAL-ONLY",
	}
	if capture {
		m[sendCaptureKey] = `{"v":1,"snapshot":` + sendFourNull + `,"omitted":{"title":"gated"}}`
	}
	if asset != nil {
		m["asset"] = asset
	}
	if metadata != nil {
		m[sendBuiltKey] = metadata
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sendKeys(out map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func TestWireMembers_MetadataUpdateCarriesExactlyBothMembers(t *testing.T) {
	for name, member := range map[string]json.RawMessage{"claims": amValid(), "four nulls": json.RawMessage(sendFourNull)} {
		out, err := federation.WireMembers(federation.ActivityUpdate, "asset", sendPayload(t, avMember(t), member, true), sendAll)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := sendKeys(out); !slices.Equal(got, []string{federation.MemberArchivalAsset, federation.MemberAssetMetadata}) {
			t.Fatalf("%s: members %v", name, got)
		}
		if string(out[federation.MemberAssetMetadata]) != string(member) {
			t.Fatalf("%s: aa:metadata %s", name, out[federation.MemberAssetMetadata])
		}
		if _, err := federation.ParseAssetMetadata(out[federation.MemberAssetMetadata]); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, _ := json.Marshal(out)
		for _, local := range []string{"LOCAL-ONLY", "share_id", sendCaptureKey, sendBuiltKey, "gated", `"omitted"`} {
			if strings.Contains(string(raw), local) {
				t.Errorf("%s: %q is on the wire", name, local)
			}
		}
	}
	// The four null keys are all present.
	out, err := federation.WireMembers(federation.ActivityUpdate, "asset", sendPayload(t, avMember(t), json.RawMessage(sendFourNull), true), sendAll)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(out[federation.MemberAssetMetadata], &keys); err != nil || len(keys) != 4 {
		t.Fatalf("four-null member %s %v", out[federation.MemberAssetMetadata], err)
	}
}

func TestWireMembers_MetadataUpdateNeedsAllThreeProfiles(t *testing.T) {
	payload := sendPayload(t, avMember(t), amValid(), true)
	for _, missing := range []string{federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1} {
		out, err := federation.WireMembers(federation.ActivityUpdate, "asset", payload, sendWithout(missing))
		if !errors.Is(err, federation.ErrNotFederated) || !errors.Is(err, federation.ErrProfileNotNegotiated) || out != nil {
			t.Errorf("without %s: %v %v", missing, sendKeys(out), err)
		}
	}
}

// The companion aa:asset carries no descriptive member beside aa:metadata,
// not as null and not in another case; such an Update is refused whole.
func TestWireMembers_MetadataUpdateRefusesADescriptiveCompanion(t *testing.T) {
	for _, c := range []struct{ key, value string }{
		{"description", `{"title":"x"}`}, {"description", `null`}, {"rights", `null`},
		{"rights", `"https://creativecommons.org/licenses/by/4.0/"`}, {"requiredStatement", `null`},
		{"identifiers", `[]`}, {"accessionNumber", `null`}, {"iiifManifest", `null`},
		{"Description", `null`}, {"RIGHTS", `null`},
	} {
		out, err := federation.WireMembers(federation.ActivityUpdate, "asset", sendPayload(t, amCompanion(t, c.key, c.value), amValid(), true), sendAll)
		if !errors.Is(err, federation.ErrNotFederated) || out != nil {
			t.Errorf("companion %s=%s: %v %v", c.key, c.value, sendKeys(out), err)
		}
	}
}

// A metadata Update is refused whole, never sent as flags only, when its
// built member is missing, null or invalid, when only one of the capture
// and the built member is present, when the local record holds a wire
// aa:metadata, and when a capture rides on any other activity.
func TestWireMembers_AMarkedRecordIsNeverStripped(t *testing.T) {
	type tc struct {
		typ     federation.ActivityType
		kind    string
		payload []byte
	}
	rawWire := func(extra map[string]any) []byte {
		m := map[string]any{"asset": avMember(t), "share_id": "x"}
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return b
	}
	cases := map[string]tc{
		"a capture without its built member": {federation.ActivityUpdate, "asset", sendPayload(t, avMember(t), nil, true)},
		"a built member without a capture":   {federation.ActivityUpdate, "asset", sendPayload(t, avMember(t), amValid(), false)},
		"a null built member":                {federation.ActivityUpdate, "asset", sendPayload(t, avMember(t), json.RawMessage(`null`), true)},
		"a built member that is not an object": {federation.ActivityUpdate, "asset",
			sendPayload(t, avMember(t), json.RawMessage(`"Harbour"`), true)},
		"a built member without a key": {federation.ActivityUpdate, "asset",
			sendPayload(t, avMember(t), json.RawMessage(`{"title":null,"description":null,"rights":null}`), true)},
		"an oversized built member": {federation.ActivityUpdate, "asset", sendPayload(t, avMember(t),
			json.RawMessage(`{"title":"`+strings.Repeat("x", 40000)+`","description":null,"rights":null,"requiredStatement":null}`), true)},
		"a wire aa:metadata in a flags-only record": {federation.ActivityUpdate, "asset",
			rawWire(map[string]any{federation.MemberAssetMetadata: json.RawMessage(amValid())})},
		"a wire aa:metadata beside a capture": {federation.ActivityUpdate, "asset", func() []byte {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(sendPayload(t, avMember(t), amValid(), true), &m)
			m[federation.MemberAssetMetadata] = amValid()
			b, _ := json.Marshal(m)
			return b
		}()},
		"a capture on an aa:AssetVersion": {federation.ActivityAAAssetVersion, "asset", sendPayload(t, avMember(t), amValid(), true)},
		"a wire aa:metadata on an aa:AssetVersion": {federation.ActivityAAAssetVersion, "asset",
			rawWire(map[string]any{federation.MemberAssetMetadata: json.RawMessage(amValid())})},
		"a capture on a comment Update": {federation.ActivityUpdate, "comment", func() []byte {
			b, _ := json.Marshal(map[string]any{"content": "An edited comment", sendCaptureKey: "x"})
			return b
		}()},
	}
	for name, c := range cases {
		out, err := federation.WireMembers(c.typ, c.kind, c.payload, sendAll)
		if !errors.Is(err, federation.ErrNotFederated) || out != nil {
			t.Errorf("%s: %v %v", name, sendKeys(out), err)
		}
	}
}

// An unmarked flags-only Update is projected exactly as before: its
// aa:asset only, under the two profiles it always needed.
func TestWireMembers_AFlagsOnlyUpdateIsUnchanged(t *testing.T) {
	payload := sendPayload(t, avMember(t), nil, false)
	out, err := federation.WireMembers(federation.ActivityUpdate, "asset", payload, sendWithout(federation.ProfileAssetMetadata1))
	if err != nil || !slices.Equal(sendKeys(out), []string{federation.MemberArchivalAsset}) || string(out[federation.MemberArchivalAsset]) != string(avMember(t)) {
		t.Fatalf("%v %v", sendKeys(out), err)
	}
}
