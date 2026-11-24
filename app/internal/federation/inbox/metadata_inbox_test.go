// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package inbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

// #1577 (spec v1 §18.1, §18.5): an asset Update carrying aa:metadata
// through the real inbox (singleton and batch, real actor and HTTP
// signatures) and dispatcher (plaintext and sealed). These are fixture
// routing tests: the updater is a fake, so they prove admission, reasons
// and routing, not storage. The storage worker against a real sender is
// tested in assetingest and in twostack (metadata_inbox_gate_test.go).

var metadataProfiles = []string{federation.ProfileArchival1, federation.ProfileOriginalTransfer1, federation.ProfileAssetMetadata1}

// mdBS is one backslash, so escaped raw names are built from bytes that
// no editor or generator can collapse.
var mdBS = string(rune(92))

func mdValidMember() map[string]any {
	return map[string]any{"title": "Harbour at dusk", "description": nil,
		"rights":            "https://creativecommons.org/licenses/by/4.0/",
		"requiredStatement": map[string]any{"label": map[string]any{"en": []string{"Attribution"}}, "value": map[string]any{"none": []string{"Studio A"}}}}
}

// metaMembers is assetMember(object) plus aa:metadata (any JSON value).
func metaMembers(object string, metadata any) map[string]any {
	m := assetMember(object)
	m[federation.MemberAssetMetadata] = metadata
	return m
}

// metaUpdateWithCompanion is a signed metadata Update whose aa:asset is
// companion ("SELF" stands for the object URI).
func (fx *fixture) metaUpdateWithCompanion(companion map[string]any) []byte {
	fx.t.Helper()
	o := fx.peerURL + "/assets/" + uuid.NewString()
	c := map[string]any{}
	for k, v := range companion {
		if v == "SELF" {
			v = o
		}
		c[k] = v
	}
	m := metaMembers(o, mdValidMember())
	m[federation.MemberArchivalAsset] = c
	return fx.shareEnvelope(federation.ActivityUpdate, "", o, m)
}

func (fx *fixture) metaUpdate(object string, metadata any) []byte {
	fx.t.Helper()
	if object == "" {
		object = fx.peerURL + "/assets/" + uuid.NewString()
	}
	return fx.shareEnvelope(federation.ActivityUpdate, "", object, metaMembers(object, metadata))
}

// withRawMetadata replaces the signed body's compact aa:metadata value by
// raw, byte for byte, without re-encoding it.
func withRawMetadata(t *testing.T, body []byte, raw string) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	old := `"` + federation.MemberAssetMetadata + `":` + string(m[federation.MemberAssetMetadata])
	if !bytes.Contains(body, []byte(old)) {
		t.Fatalf("setup: the compact member is not in the body")
	}
	return bytes.Replace(body, []byte(old), []byte(`"`+federation.MemberAssetMetadata+`":`+raw), 1)
}

func TestInbox_MetadataUpdate_AdmittedOnlyUnderAllThreeProfiles(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(metadataProfiles...)
	t.Run("all three: accepted, singleton and batch", func(t *testing.T) {
		env := fx.metaUpdate("", mdValidMember())
		if rr := fx.postSingle(env); rr.Code != http.StatusAccepted {
			t.Fatalf("status %d (%s)", rr.Code, rr.Body.String())
		}
		row, ok := fx.storedRow(activityIDOf(t, env))
		if !ok || row.Status != "pending" || !bytes.Contains(row.EnvelopeJson, []byte(federation.MemberAssetMetadata)) {
			t.Fatalf("stored %+v ok=%v", row.Status, ok)
		}
		clear := fx.metaUpdate("", map[string]any{"title": nil, "description": nil, "rights": nil, "requiredStatement": nil})
		if res := fx.postBatch(clear); res[0].Status != "accepted" {
			t.Fatalf("batch clear-all: %+v", res[0])
		}
	})
	for _, caps := range [][]string{
		{federation.ProfileArchival1, federation.ProfileOriginalTransfer1},
		{federation.ProfileArchival1, federation.ProfileAssetMetadata1},
		{federation.ProfileOriginalTransfer1, federation.ProfileAssetMetadata1},
	} {
		t.Run("only "+strings.Join(caps, "+"), func(t *testing.T) {
			fx.withProfiles(caps...)
			defer fx.withProfiles(metadataProfiles...)
			// A valid companion, and a malformed one: the profiles are
			// decided before any companion is parsed.
			for _, env := range []func() []byte{
				func() []byte { return fx.metaUpdate("", mdValidMember()) },
				func() []byte { return fx.metaUpdateWithCompanion(map[string]any{"id": "SELF"}) },
			} {
				fx.refused(env(), http.StatusUnprocessableEntity, federation.InboxStatusProfileNotNegotiated)
				b := env()
				if res := fx.postBatch(b); res[0].Status != "rejected" || res[0].Reason != string(federation.InboxStatusProfileNotNegotiated) {
					t.Fatalf("batch: %+v", res[0])
				}
				if fx.countRows(activityIDOf(t, b)) != 0 {
					t.Fatal("stored")
				}
			}
		})
	}
	t.Run("a flags-only Update needs no new token", func(t *testing.T) {
		fx.withProfiles(assetProfiles...)
		defer fx.withProfiles(metadataProfiles...)
		o := fx.peerURL + "/assets/" + uuid.NewString()
		if rr := fx.postSingle(fx.shareEnvelope(federation.ActivityUpdate, "", o, assetMember(o))); rr.Code != http.StatusAccepted {
			t.Fatalf("status %d (%s)", rr.Code, rr.Body.String())
		}
	})
}

func TestInbox_MetadataUpdate_MemberStageRefusals(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(metadataProfiles...)
	own := func() string { return fx.peerURL + "/assets/" + uuid.NewString() }
	withCompanion := func(key string, value any) func() []byte {
		return func() []byte {
			o := own()
			m := metaMembers(o, mdValidMember())
			a := m[federation.MemberArchivalAsset].(map[string]any)
			a[key] = value
			return fx.shareEnvelope(federation.ActivityUpdate, "", o, m)
		}
	}
	missing := mdValidMember()
	delete(missing, "rights")
	cases := []struct {
		name   string
		env    func() []byte
		status int
		reason federation.InboxStatus
	}{
		{"an empty title", func() []byte { m := mdValidMember(); m["title"] = ""; return fx.metaUpdate("", m) }, http.StatusBadRequest, federation.InboxStatusMetadataInvalid},
		{"a missing key", func() []byte { return fx.metaUpdate("", missing) }, http.StatusBadRequest, federation.InboxStatusMetadataInvalid},
		{"a whole-member null", func() []byte { return fx.metaUpdate("", nil) }, http.StatusBadRequest, federation.InboxStatusMetadataInvalid},
		{"a descriptive companion member, even null", withCompanion("rights", nil), http.StatusBadRequest, federation.InboxStatusMetadataInvalid},
		{"a descriptive companion member of a wrong type", withCompanion("rights", 42), http.StatusBadRequest, federation.InboxStatusMetadataInvalid},
		{"on an aa:AssetVersion", func() []byte {
			o := own()
			return fx.shareEnvelope(federation.ActivityAAAssetVersion, "", o, metaMembers(o, mdValidMember()))
		}, http.StatusBadRequest, federation.InboxStatusMetadataInvalid},
		{"an unrelated member beside it", func() []byte {
			o := own()
			m := metaMembers(o, mdValidMember())
			m["content"] = "edited"
			return fx.shareEnvelope(federation.ActivityUpdate, "", o, m)
		}, http.StatusBadRequest, federation.InboxStatusUnknownField},
		{"a malformed companion", func() []byte {
			o := own()
			m := metaMembers(o, mdValidMember())
			m[federation.MemberArchivalAsset] = map[string]any{"id": o}
			return fx.shareEnvelope(federation.ActivityUpdate, "", o, m)
		}, http.StatusBadRequest, federation.InboxStatusArchivalInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx.refused(c.env(), c.status, c.reason)
			env := c.env()
			if res := fx.postBatch(env); res[0].Status != "rejected" || res[0].Reason != string(c.reason) {
				t.Fatalf("batch: %+v, want %s", res[0], c.reason)
			}
			if fx.countRows(activityIDOf(t, env)) != 0 {
				t.Fatal("a refused batch envelope was stored")
			}
		})
	}
	t.Run("over 32,768 raw bytes, whitespace inside the member", func(t *testing.T) {
		signed := fx.metaUpdate("", mdValidMember())
		var compact map[string]json.RawMessage
		_ = json.Unmarshal(signed, &compact)
		member := string(compact[federation.MemberAssetMetadata])
		padded := "{" + strings.Repeat(" ", 32768) + member[1:]
		body := withRawMetadata(t, signed, padded)
		env, err := federation.Unmarshal(body)
		if err != nil || len(env.Extra[federation.MemberAssetMetadata]) <= federation.AssetMetadataMaxMemberBytes {
			t.Fatalf("setup: the raw member did not survive parsing (%v, %d bytes)", err, len(env.Extra[federation.MemberAssetMetadata]))
		}
		// The actor signature covers the canonical form, unchanged by the
		// whitespace, so the envelope reaches the member stage.
		fx.refused(body, http.StatusBadRequest, federation.InboxStatusMetadataInvalid)
	})
}

// A repeated name anywhere, inside aa:metadata included, is refused by
// the strict envelope parser before any profile stage (§18.5).
func TestInbox_MetadataUpdate_RepeatedNamesKeepTheEnvelopeReason(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(metadataProfiles...)
	for name, raw := range map[string]string{
		"title twice":               `{"title":"a","title":"b","description":null,"rights":null,"requiredStatement":null}`,
		"an escaped alias of title": `{"` + mdBS + `u0074itle":"a","title":"b","description":null,"rights":null,"requiredStatement":null}`,
		"a language tag twice, nested": `{"title":null,"description":null,"rights":null,"requiredStatement":{"label":{"en":["A"],"` +
			mdBS + `u0065n":["B"]},"value":{"none":["x"]}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := withRawMetadata(t, fx.metaUpdate("", mdValidMember()), raw)
			if strings.Contains(name, "escaped") || strings.Contains(name, "nested") {
				if !bytes.Contains(body, []byte{92, 'u', '0', '0'}) {
					t.Fatal("setup: the body lacks the literal escape")
				}
			}
			if _, err := federation.Unmarshal(body); !errors.Is(err, federation.ErrInvalidMember) {
				t.Fatalf("setup: the parser says %v", err)
			}
			fx.refused(body, http.StatusBadRequest, federation.InboxStatusInvalidMember)
			batchBody := withRawMetadata(t, fx.metaUpdate("", mdValidMember()), raw)
			if res := fx.postBatch(batchBody); res[0].Status != "rejected" || res[0].Reason != string(federation.InboxStatusInvalidMember) {
				t.Fatalf("batch: %+v", res[0])
			}
		})
	}
}

func TestInbox_MetadataUpdate_SignatureAndReceipt(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(metadataProfiles...)
	t.Run("a member changed after signing fails the actor signature", func(t *testing.T) {
		m := mdValidMember()
		m["title"] = "Signed title"
		body := fx.metaUpdate("", m)
		tampered := bytes.Replace(body, []byte(`"Signed title"`), []byte(`"Other title"`), 1)
		if bytes.Equal(tampered, body) {
			t.Fatal("setup: nothing was changed")
		}
		fx.refused(tampered, http.StatusUnauthorized, federation.InboxStatusSigInvalid)
	})
	t.Run("aa:metadata in clear beside an encryption block is refused at receipt", func(t *testing.T) {
		// The dispatcher restores Extra from the sealed payload, so the
		// clear-member rule is the receipt's (ValidateMembers' encrypted
		// branch), checked before anything is stored.
		o := fx.peerURL + "/assets/" + uuid.NewString()
		block := federation.EncryptionBlock{Algorithm: federation.EncryptionAlgNaClBoxV1,
			SenderKeyID: fx.peerURL + "/users/alice#encryption-key", SenderKeyVersion: 1,
			RecipientKeyID: shareLocal + "/users/bob#encryption-key", RecipientKeyVersion: 1,
			Nonce: federation.Base64Bytes(make([]byte, 24)), Ciphertext: federation.Base64Bytes([]byte("sealed"))}
		env := fx.shareEnvelope(federation.ActivityUpdate, "", o, map[string]any{
			federation.MemberAssetMetadata: mdValidMember(), "encryption": block})
		if !bytes.Contains(env, []byte(`"encryption"`)) || !bytes.Contains(env, []byte(federation.MemberAssetMetadata)) {
			t.Fatal("setup: the body lacks the block or the clear member")
		}
		fx.refused(env, http.StatusBadRequest, federation.InboxStatusInvalidMember)
	})
	t.Run("a stored id answers from its row; another body is not admitted", func(t *testing.T) {
		o := fx.peerURL + "/assets/" + uuid.NewString()
		id := fx.peerURL + "/activities/" + uuid.NewString()
		first := fx.shareEnvelope(federation.ActivityUpdate, id, o, metaMembers(o, mdValidMember()))
		if rr := fx.postSingle(first); rr.Code != http.StatusAccepted {
			t.Fatalf("first: %d (%s)", rr.Code, rr.Body.String())
		}
		before, _ := fx.storedRow(id)
		m := mdValidMember()
		m["title"] = "Replacement"
		second := fx.shareEnvelope(federation.ActivityUpdate, id, o, metaMembers(o, m))
		if rr := fx.postSingle(second); rr.Code != http.StatusOK {
			t.Fatalf("replay: %d (%s)", rr.Code, rr.Body.String())
		}
		after, _ := fx.storedRow(id)
		if fx.countRows(id) != 1 || !bytes.Equal(before.EnvelopeJson, after.EnvelopeJson) || bytes.Contains(after.EnvelopeJson, []byte("Replacement")) {
			t.Fatal("a replayed id changed the stored envelope")
		}
	})
}

// metaDispatcher is the production dispatcher for a peer with caps, with
// u as its asset receiver.
func (fx *fixture) metaDispatcher(u inbox.AssetFlagUpdater, caps []string) *inbox.Dispatcher {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := inbox.NewDispatcher(inbox.DispatcherConfig{Interval: time.Hour, BatchSize: 100, MaxAttempts: 5}, fx.q,
		func(_ context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
			return inbox.PeerInfo{ID: id, InstanceURL: fx.peerURL, Enabled: true, Connected: true, Capabilities: caps}, nil
		}, nil, logger)
	d.SetRegistry(inbox.BuildRegistry(d, logger))
	d.SetLocalBaseURL(func(context.Context) string { return shareLocal })
	d.SetAssetFlagUpdater(u)
	return d
}

func TestDispatch_MetadataUpdate(t *testing.T) {
	fx := newFixture(t)
	fx.withProfiles(metadataProfiles...)
	stored := func(t *testing.T) (inbox.FederationInbox, string) {
		t.Helper()
		env := fx.metaUpdate("", mdValidMember())
		if rr := fx.postSingle(env); rr.Code != http.StatusAccepted {
			t.Fatalf("post: %d %s", rr.Code, rr.Body.String())
		}
		row, _ := fx.storedRow(activityIDOf(t, env))
		return row, activityIDOf(t, env)
	}
	reload := func(t *testing.T, id string) inbox.FederationInbox {
		t.Helper()
		row, ok := fx.storedRow(id)
		if !ok {
			t.Fatal("row gone")
		}
		return row
	}
	t.Run("all three profiles: to the asset receiver, processed", func(t *testing.T) {
		u := &fakeFlagUpdater{}
		row, id := stored(t)
		if out := fx.metaDispatcher(u, metadataProfiles).DispatchRow(context.Background(), row); out != inbox.OutcomeProcessed {
			t.Fatalf("dispatch: %v (%s)", out, reload(t, id).LastError)
		}
		if u.count() != 1 || u.calls[0] != id || reload(t, id).Status != "processed" {
			t.Fatalf("calls %v, status %s", u.calls, reload(t, id).Status)
		}
	})
	t.Run("the profile dropped after queueing: rejected, never a flags-only success", func(t *testing.T) {
		u := &fakeFlagUpdater{}
		row, id := stored(t)
		if out := fx.metaDispatcher(u, assetProfiles).DispatchRow(context.Background(), row); out != inbox.OutcomeRejected {
			t.Fatalf("dispatch: %v", out)
		}
		if r := reload(t, id); u.count() != 0 || r.RejectReason == nil || *r.RejectReason != string(federation.InboxStatusProfileNotNegotiated) {
			t.Fatalf("calls %d, row %s %v", u.count(), r.Status, r.RejectReason)
		}
	})
	t.Run("a receiver's metadata_invalid refusal keeps its reason", func(t *testing.T) {
		u := &fakeFlagUpdater{res: inbox.AssetScheduled{Reason: "metadata_invalid"}, err: errors.New("refused")}
		row, id := stored(t)
		if out := fx.metaDispatcher(u, metadataProfiles).DispatchRow(context.Background(), row); out != inbox.OutcomeRejected {
			t.Fatalf("dispatch: %v", out)
		}
		if r := reload(t, id); r.RejectReason == nil || *r.RejectReason != string(federation.InboxStatusMetadataInvalid) {
			t.Fatalf("row %s %v", r.Status, r.RejectReason)
		}
	})
	if got := inbox.AssetRejectStatus("metadata_invalid"); got != federation.InboxStatusMetadataInvalid {
		t.Fatalf("AssetRejectStatus(metadata_invalid) = %s", got)
	}
}

// --- sealed (encrypted) dispatch -------------------------------------------

// metaSealedDispatcher is the production dispatcher with the decrypt fixture's
// keys, for a peer with all three profiles, with u as its asset receiver.
func (fx *decryptFx) metaSealedDispatcher(u inbox.AssetFlagUpdater) *inbox.Dispatcher {
	d := inbox.NewDispatcher(inbox.DispatcherConfig{Interval: time.Hour, BatchSize: 100, MaxAttempts: 5}, inbox.New(fx.pool),
		func(_ context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
			return inbox.PeerInfo{ID: id, InstanceURL: "https://studio-b.example", Enabled: true, Connected: true, Capabilities: metadataProfiles}, nil
		}, nil, fx.logger)
	d.SetRegistry(inbox.BuildRegistry(d, fx.logger))
	d.SetLocalBaseURL(func(context.Context) string { return "https://studio-a.example" })
	d.SetAssetFlagUpdater(u)
	d.SetRawPool(fx.pool)
	d.SetSenderEncKey(func(_ context.Context, actorURI string) ([]byte, int32, error) {
		if actorURI != fx.senderActor {
			return nil, 0, errors.New("synthetic: unknown sender")
		}
		return fx.senderPub, 1, nil
	})
	d.SetRecipientUserRef(func(ctx context.Context, actorURI string) (int64, error) {
		username := actorURI[strings.LastIndex(actorURI, "/users/")+len("/users/"):]
		var ref int64
		err := fx.pool.QueryRow(ctx, `SELECT ref FROM "user" WHERE username = $1`, username).Scan(&ref)
		return ref, err
	})
	return d
}

// sealedUpdate is an asset Update whose members (plaintext JSON) are
// sealed to the recipient, with clear beside the encryption block.
func (fx *decryptFx) sealedUpdate(plaintext []byte, clear map[string]json.RawMessage) ([]byte, string) {
	fx.t.Helper()
	recipientPub := fetchUserCurrentPub(fx.t, fx.pool, fx.recipientRef)
	id := "https://studio-b.example/activities/" + uuid.NewString()
	object := "https://studio-b.example/assets/" + uuid.NewString()
	plaintext = bytes.ReplaceAll(plaintext, []byte("OBJECT"), []byte(object))
	nonce, ct := fx.encryptPayload(plaintext, recipientPub)
	env := &federation.Envelope{
		Context: federation.ContextV1, Type: federation.ActivityUpdate, ID: id, Actor: fx.senderActor,
		Object: object, Published: time.Now().UTC(), To: []string{fx.recipientURI},
		Signature: &federation.Signature{Type: federation.SignatureAlgEd25519, PublicKey: fx.senderActor + "#main-key", Value: "AAAAAAAAAAAA"},
		Encryption: &federation.EncryptionBlock{
			Algorithm: federation.EncryptionAlgNaClBoxV1, SenderKeyID: fx.senderActor + "#encryption-key", SenderKeyVersion: 1,
			RecipientKeyID: fx.recipientURI + "#encryption-key", RecipientKeyVersion: 1,
			Nonce: federation.Base64Bytes(nonce), Ciphertext: federation.Base64Bytes(ct),
		},
		Extra: clear,
	}
	b, err := env.Marshal()
	if err != nil {
		fx.t.Fatal(err)
	}
	return b, id
}

func sealedMembers(t *testing.T, metadata string) []byte {
	t.Helper()
	asset, _ := json.Marshal(assetMember("OBJECT")[federation.MemberArchivalAsset])
	return []byte(`{"` + federation.MemberArchivalAsset + `":` + string(asset) + `,"` + federation.MemberAssetMetadata + `":` + metadata + `}`)
}

func TestDispatch_SealedMetadataUpdate(t *testing.T) {
	fx := newDecryptFx(t)
	valid, _ := json.Marshal(mdValidMember())
	for _, tc := range []struct {
		name      string
		plaintext []byte
		clear     map[string]json.RawMessage
		outcome   inbox.DispatchOutcome
		reason    federation.InboxStatus
		calls     int
	}{
		{"valid restored metadata: to the asset receiver", sealedMembers(t, string(valid)), nil, inbox.OutcomeProcessed, "", 1},
		{"malformed restored metadata: metadata_invalid", sealedMembers(t, `{"title":"","description":null,"rights":null,"requiredStatement":null}`),
			nil, inbox.OutcomeRejected, federation.InboxStatusMetadataInvalid, 0},
		{"a repeated name in the sealed members: invalid_member", sealedMembers(t, `{"title":"a","title":"b","description":null,"rights":null,"requiredStatement":null}`),
			nil, inbox.OutcomeRejected, federation.InboxStatusInvalidMember, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &fakeFlagUpdater{}
			body, id := fx.sealedUpdate(tc.plaintext, tc.clear)
			rowID, row := fx.insertInbox(body, id, federation.ActivityUpdate)
			if out := fx.metaSealedDispatcher(u).DispatchRow(context.Background(), row); out != tc.outcome {
				t.Fatalf("dispatch: %v, want %v (%+v)", out, tc.outcome, fx.readRow(rowID))
			}
			if u.count() != tc.calls {
				t.Fatalf("%d calls to the asset receiver, want %d", u.count(), tc.calls)
			}
			if tc.reason != "" {
				var reason *string
				if err := fx.pool.QueryRow(context.Background(), `SELECT reject_reason FROM federation_inbox WHERE id = $1`, rowID).Scan(&reason); err != nil ||
					reason == nil || *reason != string(tc.reason) {
					t.Fatalf("reject_reason %v (%v), want %s", reason, err, tc.reason)
				}
			}
		})
	}
}
