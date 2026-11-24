// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Conformance vectors for profile asset-metadata-1 (spec v1 §18, #1577).
//
// Each vector under testdata/conformance/asset-metadata-1/ names the pure
// check it runs NOW (enforcement.now) and the open issue that applies the
// rule in a running receiver (enforcement.pending). Every profile vector
// carrying aa:metadata is also run through the inbox's own member check
// (federation.ValidateMembers), which must reach the same outcome: the
// running receiver admits the member under all three profiles (#1577).

package federation_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// assetMetadataVector is the on-disk shape of an asset-metadata-1 vector.
// A member-stage vector carries the raw member bytes as a JSON string
// (member) or, for bytes that are not valid UTF-8, as base64
// (member_b64), so a repeated name or a raw escape reaches the parser
// exactly as written.
type assetMetadataVector struct {
	Description string          `json:"description"`
	Spec        string          `json:"spec"`
	Stage       string          `json:"stage"`
	Member      *string         `json:"member,omitempty"`
	MemberB64   string          `json:"member_b64,omitempty"`
	Negotiated  []string        `json:"negotiated,omitempty"`
	Envelope    json.RawMessage `json:"envelope,omitempty"`
	Expected    struct {
		Outcome  string          `json:"outcome"`
		Reason   string          `json:"reason"`
		Snapshot json.RawMessage `json:"snapshot,omitempty"`
	} `json:"expected"`
	Enforcement struct {
		Now     string `json:"now"`
		Pending string `json:"pending"`
	} `json:"enforcement"`
}

// assetMetadataReasons are the reasons these pure checks can give:
// the profile's own, and the existing ones it keeps (§18.5).
var assetMetadataReasons = []string{"metadata_invalid", "profile_not_negotiated", "archival_invalid", "invalid_member", "unknown_field"}

func assetMetadataReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, federation.ErrMetadataInvalid):
		return "metadata_invalid"
	case errors.Is(err, federation.ErrProfileNotNegotiated):
		return string(federation.InboxStatusProfileNotNegotiated)
	case errors.Is(err, federation.ErrArchivalInvalid):
		return string(federation.InboxStatusArchivalInvalid)
	case errors.Is(err, federation.ErrInvalidMember):
		return "invalid_member"
	case errors.Is(err, federation.ErrUnknownField):
		return "unknown_field"
	default:
		return "unexpected error: " + err.Error()
	}
}

func (v assetMetadataVector) memberBytes(t *testing.T) json.RawMessage {
	t.Helper()
	switch {
	case v.Member != nil && v.MemberB64 == "":
		return json.RawMessage(*v.Member)
	case v.Member == nil && v.MemberB64 != "":
		b, err := base64.StdEncoding.DecodeString(v.MemberB64)
		if err != nil {
			t.Fatalf("member_b64: %v", err)
		}
		return b
	default:
		t.Fatal("a member vector carries exactly one of member and member_b64")
		return nil
	}
}

// checkSnapshot compares a parsed snapshot with the vector's expected one,
// both canonicalized.
func checkSnapshot(t *testing.T, got *federation.AssetMetadataSnapshot, want json.RawMessage) {
	t.Helper()
	if len(want) == 0 {
		t.Fatal("an accepted member vector states its expected snapshot")
	}
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	a, err := federation.Canonicalize(out)
	if err != nil {
		t.Fatal(err)
	}
	b, err := federation.Canonicalize(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("snapshot\n got %s\nwant %s", a, b)
	}
}

func TestConformance_AssetMetadata1(t *testing.T) {
	dir := filepath.Join(conformanceDir, "asset-metadata-1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	counts := map[string]int{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("%s: only .json vectors belong here", e.Name())
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var v assetMetadataVector
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("%s: fixture shape: %v", name, err)
		}
		counts[v.Stage+"/"+v.Expected.Outcome]++
		t.Run(name, func(t *testing.T) {
			if v.Description == "" || !strings.Contains(v.Spec, "§18") {
				t.Fatal("description and a §18 spec reference are required")
			}
			if !strings.HasPrefix(v.Enforcement.Pending, "#1577:") {
				t.Fatalf("enforcement.pending %q must name #1577", v.Enforcement.Pending)
			}
			switch v.Expected.Outcome {
			case "accept":
				if v.Expected.Reason != "" {
					t.Fatal("an accepted vector has no reason")
				}
			case "reject":
				if !slices.Contains(assetMetadataReasons, v.Expected.Reason) {
					t.Fatalf("reason %q is not one this profile's checks give", v.Expected.Reason)
				}
			default:
				t.Fatalf("outcome %q", v.Expected.Outcome)
			}
			var got string
			switch v.Stage {
			case "member":
				if v.Enforcement.Now != "federation.ParseAssetMetadata" || v.Envelope != nil || v.Negotiated != nil {
					t.Fatal("a member vector runs federation.ParseAssetMetadata on member bytes only")
				}
				in := v.memberBytes(t)
				before := bytes.Clone(in)
				s, err := federation.ParseAssetMetadata(in)
				got = assetMetadataReason(err)
				if err == nil && v.Expected.Outcome == "accept" {
					checkSnapshot(t, s, v.Expected.Snapshot)
				}
				if !bytes.Equal(before, in) {
					t.Fatal("the parser changed its input")
				}
			case "round-trip":
				if v.Enforcement.Now != "federation.ParseAssetMetadata" || v.Expected.Outcome != "accept" {
					t.Fatal("a round-trip vector is an accepted federation.ParseAssetMetadata vector")
				}
				s, err := federation.ParseAssetMetadata(v.memberBytes(t))
				got = assetMetadataReason(err)
				if err == nil {
					checkSnapshot(t, s, v.Expected.Snapshot)
					out, _ := json.Marshal(s)
					var keys map[string]json.RawMessage
					if err := json.Unmarshal(out, &keys); err != nil || len(keys) != 4 {
						t.Fatalf("marshalled snapshot has %d keys: %s", len(keys), out)
					}
					back, err := federation.ParseAssetMetadata(out)
					if err != nil || !reflect.DeepEqual(back, s) {
						t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, back, s)
					}
				}
			case "profile":
				if v.Enforcement.Now != "federation.ValidateAssetMetadataMembers" || v.Member != nil || v.MemberB64 != "" {
					t.Fatal("a profile vector runs federation.ValidateAssetMetadataMembers on an envelope")
				}
				env, err := federation.Unmarshal(v.Envelope)
				if err != nil {
					t.Fatalf("not a valid v1 envelope: %v", err)
				}
				mc := federation.MemberContext{
					Negotiated: func(tok string) bool { return slices.Contains(v.Negotiated, tok) },
					Applied:    func(federation.ActivityType) bool { return true },
				}
				got = assetMetadataReason(federation.ValidateAssetMetadataMembers(env, mc))
				// The running member check reaches the same outcome.
				if _, has := env.Extra[federation.MemberAssetMetadata]; has {
					if running := assetMetadataReason(federation.ValidateMembers(env, mc)); running != got {
						t.Fatalf("the running ValidateMembers gives %q, the profile check %q", running, got)
					}
				}
			default:
				t.Fatalf("stage %q", v.Stage)
			}
			if got != v.Expected.Reason {
				t.Errorf("got %q want %q (%s)", got, v.Expected.Reason, v.Description)
			}
		})
	}
	// Deleting vectors must not make this pass.
	for _, need := range []string{"member/accept", "member/reject", "round-trip/accept", "profile/accept", "profile/reject"} {
		if counts[need] == 0 {
			t.Errorf("no %s vector", need)
		}
	}
}
