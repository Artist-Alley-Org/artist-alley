// Unit tests for the inbound-envelope encryption-key parser
// (Phase 1.22.I-c). Exercises every path through extractEncryptionKey
// + the matching upsertActorBestEffort wiring.

package inbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeEnvelopeWithEncKey(t *testing.T, raw json.RawMessage) *federation.Envelope {
	t.Helper()
	env := &federation.Envelope{
		Actor: "https://test-peer.local/users/alice",
		Extra: map[string]json.RawMessage{
			federation.PropEncryptionPublicKey: raw,
		},
	}
	return env
}

func randomKeyBytes(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

// --- happy path ---------------------------------------------------

func TestExtractEncryptionKey_ParsesWellFormedBlock(t *testing.T) {
	key := randomKeyBytes(t)
	raw, _ := json.Marshal(map[string]any{
		"type":            federation.TypeX25519PublicKey,
		"publicKeyBase64": base64.StdEncoding.EncodeToString(key),
		"version":         3,
	})
	env := makeEnvelopeWithEncKey(t, raw)

	got := extractEncryptionKey(env, newSilentLogger())
	if got == nil {
		t.Fatalf("got nil, want EncryptionKeyInline")
	}
	if got.Version != 3 {
		t.Errorf("version = %d, want 3", got.Version)
	}
	if !bytes.Equal(got.PublicKey, key) {
		t.Errorf("PublicKey mismatch")
	}
}

func TestExtractEncryptionKey_AcceptsBlockWithoutTypeDiscriminator(t *testing.T) {
	// The discriminator is the forward-compat hook for future
	// algorithms; today omitting it is acceptable and the parser
	// treats it as "the only algorithm we know about, X25519".
	key := randomKeyBytes(t)
	raw, _ := json.Marshal(map[string]any{
		"publicKeyBase64": base64.StdEncoding.EncodeToString(key),
		"version":         1,
	})
	env := makeEnvelopeWithEncKey(t, raw)

	got := extractEncryptionKey(env, newSilentLogger())
	if got == nil {
		t.Fatalf("got nil, want EncryptionKeyInline")
	}
}

// --- absent / malformed paths ------------------------------------

func TestExtractEncryptionKey_NilWhenFieldMissing(t *testing.T) {
	env := &federation.Envelope{Actor: "x", Extra: map[string]json.RawMessage{}}
	if got := extractEncryptionKey(env, newSilentLogger()); got != nil {
		t.Errorf("got %v, want nil for envelope without aa:encryptionPublicKey", got)
	}
}

func TestExtractEncryptionKey_NilOnMalformedJSON(t *testing.T) {
	env := makeEnvelopeWithEncKey(t, json.RawMessage(`{not valid json`))
	if got := extractEncryptionKey(env, newSilentLogger()); got != nil {
		t.Errorf("got %v, want nil for malformed JSON", got)
	}
}

func TestExtractEncryptionKey_NilOnUnknownTypeDiscriminator(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"type":            "aa:UnobtainiumPublicKey",
		"publicKeyBase64": base64.StdEncoding.EncodeToString(randomKeyBytes(t)),
		"version":         1,
	})
	env := makeEnvelopeWithEncKey(t, raw)
	if got := extractEncryptionKey(env, newSilentLogger()); got != nil {
		t.Errorf("got %v, want nil for unknown type", got)
	}
}

func TestExtractEncryptionKey_NilOnVersionZero(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"type":            federation.TypeX25519PublicKey,
		"publicKeyBase64": base64.StdEncoding.EncodeToString(randomKeyBytes(t)),
		"version":         0,
	})
	env := makeEnvelopeWithEncKey(t, raw)
	if got := extractEncryptionKey(env, newSilentLogger()); got != nil {
		t.Errorf("got %v, want nil for version=0", got)
	}
}

func TestExtractEncryptionKey_NilOnNegativeVersion(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"type":            federation.TypeX25519PublicKey,
		"publicKeyBase64": base64.StdEncoding.EncodeToString(randomKeyBytes(t)),
		"version":         -1,
	})
	env := makeEnvelopeWithEncKey(t, raw)
	if got := extractEncryptionKey(env, newSilentLogger()); got != nil {
		t.Errorf("got %v, want nil for negative version", got)
	}
}

func TestExtractEncryptionKey_NilOnBadBase64(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"type":            federation.TypeX25519PublicKey,
		"publicKeyBase64": "!!! not base64 !!!",
		"version":         1,
	})
	env := makeEnvelopeWithEncKey(t, raw)
	if got := extractEncryptionKey(env, newSilentLogger()); got != nil {
		t.Errorf("got %v, want nil for bad base64", got)
	}
}

func TestExtractEncryptionKey_NilOnWrongLength(t *testing.T) {
	for _, badLen := range []int{0, 1, 31, 33, 64} {
		short := make([]byte, badLen)
		raw, _ := json.Marshal(map[string]any{
			"type":            federation.TypeX25519PublicKey,
			"publicKeyBase64": base64.StdEncoding.EncodeToString(short),
			"version":         1,
		})
		env := makeEnvelopeWithEncKey(t, raw)
		if got := extractEncryptionKey(env, newSilentLogger()); got != nil {
			t.Errorf("len=%d: got %v, want nil", badLen, got)
		}
	}
}

// --- upsertActorBestEffort wiring --------------------------------

// fakeUpserter captures the args of every Upsert call.
type fakeUpserter struct {
	calls []fakeUpsertCall
}

type fakeUpsertCall struct {
	ActorURI    string
	PeerID      uuid.UUID
	DisplayName string
	AvatarURL   string
	EncKey      *EncryptionKeyInline
}

func (f *fakeUpserter) Upsert(_ context.Context, actorURI string, peerID uuid.UUID, displayName, avatarURL string, encKey *EncryptionKeyInline) error {
	f.calls = append(f.calls, fakeUpsertCall{
		ActorURI: actorURI, PeerID: peerID, DisplayName: displayName, AvatarURL: avatarURL, EncKey: encKey,
	})
	return nil
}

func TestUpsertActorBestEffort_PassesParsedEncryptionKeyDownstream(t *testing.T) {
	key := randomKeyBytes(t)
	raw, _ := json.Marshal(map[string]any{
		"type":            federation.TypeX25519PublicKey,
		"publicKeyBase64": base64.StdEncoding.EncodeToString(key),
		"version":         2,
	})
	env := &federation.Envelope{
		Actor: "https://test-peer.local/users/alice",
		Extra: map[string]json.RawMessage{
			federation.PropEncryptionPublicKey: raw,
		},
	}
	d := &Dispatcher{logger: newSilentLogger()}
	cache := &fakeUpserter{}
	d.actorCache = cache

	d.upsertActorBestEffort(context.Background(), env, uuid.New())
	if len(cache.calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(cache.calls))
	}
	got := cache.calls[0]
	if got.EncKey == nil {
		t.Fatalf("upserter called with nil EncKey; want parsed key")
	}
	if got.EncKey.Version != 2 {
		t.Errorf("version = %d, want 2", got.EncKey.Version)
	}
	if !bytes.Equal(got.EncKey.PublicKey, key) {
		t.Errorf("PublicKey mismatch")
	}
}

func TestUpsertActorBestEffort_PassesNilEncryptionKeyOnAbsent(t *testing.T) {
	// Envelope without the aa:encryptionPublicKey field — the
	// pre-I-c peer case. Display upsert still fires; EncKey is nil.
	env := &federation.Envelope{
		Actor: "https://test-peer.local/users/alice",
		Extra: map[string]json.RawMessage{},
	}
	d := &Dispatcher{logger: newSilentLogger()}
	cache := &fakeUpserter{}
	d.actorCache = cache

	d.upsertActorBestEffort(context.Background(), env, uuid.New())
	if len(cache.calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(cache.calls))
	}
	if cache.calls[0].EncKey != nil {
		t.Errorf("got %v, want nil EncKey", cache.calls[0].EncKey)
	}
}
