package nacl_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation/nacl"
)

// helper: fresh X25519 keypair via the package's exported helper.
func freshPair(t *testing.T) (pub, priv []byte) {
	t.Helper()
	pub, priv, err := nacl.GenerateActorEncryptionKeypair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(pub) != nacl.X25519KeyLen || len(priv) != nacl.X25519KeyLen {
		t.Fatalf("key lengths: pub=%d priv=%d", len(pub), len(priv))
	}
	return pub, priv
}

func TestSealOpenRoundTrip(t *testing.T) {
	rxPub, rxPriv := freshPair(t)
	plain := []byte("hello, peer.")
	sealed, err := nacl.Seal(plain, [][]byte{rxPub})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if len(sealed.Recipients) != 1 {
		t.Fatalf("recipients: got %d want 1", len(sealed.Recipients))
	}
	got, err := nacl.Open(
		sealed.Recipients[0].Ciphertext,
		sealed.Recipients[0].Nonce,
		sealed.EphemeralPublicKey,
		rxPriv,
	)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("plaintext mismatch")
	}
}

func TestSealMultiRecipient(t *testing.T) {
	pub1, priv1 := freshPair(t)
	pub2, priv2 := freshPair(t)
	pub3, priv3 := freshPair(t)
	plain := []byte("group announcement")
	sealed, err := nacl.Seal(plain, [][]byte{pub1, pub2, pub3})
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed.Recipients) != 3 {
		t.Fatalf("recipients: got %d want 3", len(sealed.Recipients))
	}
	for i, priv := range [][]byte{priv1, priv2, priv3} {
		got, err := nacl.Open(
			sealed.Recipients[i].Ciphertext,
			sealed.Recipients[i].Nonce,
			sealed.EphemeralPublicKey,
			priv,
		)
		if err != nil {
			t.Errorf("recipient %d open: %v", i, err)
			continue
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("recipient %d plaintext mismatch", i)
		}
	}
}

func TestSealNondeterministic(t *testing.T) {
	pub, _ := freshPair(t)
	plain := []byte("same payload")
	a, _ := nacl.Seal(plain, [][]byte{pub})
	b, _ := nacl.Seal(plain, [][]byte{pub})
	// Fresh ephemeral key per Seal + fresh nonce per recipient →
	// two sealings of the same plaintext MUST differ. Catastrophic
	// regression if this ever ties.
	if bytes.Equal(a.EphemeralPublicKey, b.EphemeralPublicKey) {
		t.Error("two Seal calls produced identical ephemeral keys (nonce reuse risk)")
	}
	if bytes.Equal(a.Recipients[0].Nonce, b.Recipients[0].Nonce) {
		t.Error("two Seal calls produced identical nonces (catastrophic)")
	}
	if bytes.Equal(a.Recipients[0].Ciphertext, b.Recipients[0].Ciphertext) {
		t.Error("two Seal calls produced identical ciphertexts")
	}
}

func TestOpenRejectsWrongRecipientKey(t *testing.T) {
	pub1, _ := freshPair(t)
	_, priv2 := freshPair(t)
	sealed, _ := nacl.Seal([]byte("secret"), [][]byte{pub1})
	_, err := nacl.Open(
		sealed.Recipients[0].Ciphertext,
		sealed.Recipients[0].Nonce,
		sealed.EphemeralPublicKey,
		priv2, // wrong key
	)
	if !errors.Is(err, nacl.ErrDecryptFailed) {
		t.Errorf("expected ErrDecryptFailed, got %v", err)
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	pub, priv := freshPair(t)
	sealed, _ := nacl.Seal([]byte("authentic"), [][]byte{pub})
	ct := append([]byte(nil), sealed.Recipients[0].Ciphertext...)
	ct[len(ct)-1] ^= 0xff
	_, err := nacl.Open(ct, sealed.Recipients[0].Nonce, sealed.EphemeralPublicKey, priv)
	if !errors.Is(err, nacl.ErrDecryptFailed) {
		t.Errorf("expected ErrDecryptFailed on tampered ciphertext, got %v", err)
	}
}

func TestOpenRejectsTamperedNonce(t *testing.T) {
	pub, priv := freshPair(t)
	sealed, _ := nacl.Seal([]byte("authentic"), [][]byte{pub})
	bad := append([]byte(nil), sealed.Recipients[0].Nonce...)
	bad[0] ^= 0xff
	_, err := nacl.Open(sealed.Recipients[0].Ciphertext, bad, sealed.EphemeralPublicKey, priv)
	if !errors.Is(err, nacl.ErrDecryptFailed) {
		t.Errorf("expected ErrDecryptFailed on tampered nonce, got %v", err)
	}
}

func TestOpenRejectsBadKeyLengths(t *testing.T) {
	cases := []struct {
		name string
		eph  []byte
		non  []byte
		priv []byte
		want error
	}{
		{"short ephemeral", make([]byte, 31), make([]byte, 24), make([]byte, 32), nacl.ErrInvalidEphemeralKey},
		{"short nonce", make([]byte, 32), make([]byte, 23), make([]byte, 32), nacl.ErrInvalidNonce},
		{"short priv", make([]byte, 32), make([]byte, 24), make([]byte, 31), nacl.ErrInvalidPrivateKey},
	}
	for _, c := range cases {
		_, err := nacl.Open([]byte("ct"), c.non, c.eph, c.priv)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: expected %v, got %v", c.name, c.want, err)
		}
	}
}

func TestSealRejectsBadRecipientKey(t *testing.T) {
	_, err := nacl.Seal([]byte("x"), [][]byte{make([]byte, 31)}) // wrong length
	if !errors.Is(err, nacl.ErrInvalidPublicKey) {
		t.Errorf("expected ErrInvalidPublicKey, got %v", err)
	}
}

func TestSealDetRejectsNonceLengthMismatch(t *testing.T) {
	pub, _ := freshPair(t)
	var ephPub, ephPriv [32]byte
	_, _ = rand.Read(ephPriv[:])
	_, _ = rand.Read(ephPub[:])
	// pass per-recipient nonces but wrong COUNT
	_, err := nacl.SealDet([]byte("x"), [][]byte{pub}, &ephPub, &ephPriv, [][]byte{
		make([]byte, 24), make([]byte, 24), // 2 nonces for 1 recipient
	})
	if err == nil {
		t.Error("SealDet should reject nonce-count != recipient-count")
	}
}

func TestSealDetRequiresEphemeral(t *testing.T) {
	pub, _ := freshPair(t)
	_, err := nacl.SealDet([]byte("x"), [][]byte{pub}, nil, nil, nil)
	if err == nil {
		t.Error("SealDet should reject nil ephemeral keys")
	}
}
