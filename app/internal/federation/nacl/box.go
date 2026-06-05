// Package nacl wraps the NaCl-box multi-recipient encryption
// envelope per the artist-alley federation v1 spec.
//
// Spec reference: docs/spec/federation/v1.md §6.
//
// The wire shape is a single ephemeral X25519 keypair generated
// per envelope + a per-recipient (nonce, ciphertext) pair sealed
// to each recipient's X25519 public key. Per spec §6.2 each actor
// holds a DISTINCT X25519 keypair (separate from the Ed25519
// signing keypair) — we do not derive X25519 from Ed25519 in v1
// to keep cryptographic primitives separated and to avoid
// inlining the GF(2^255-19) field arithmetic the birational map
// would otherwise need.
package nacl

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

// Public errors callers may distinguish on.
var (
	// ErrInvalidPublicKey indicates a supplied X25519 public key
	// is the wrong length.
	ErrInvalidPublicKey = errors.New("nacl: x25519 public key has wrong length")

	// ErrInvalidPrivateKey indicates a supplied X25519 private
	// key is the wrong length.
	ErrInvalidPrivateKey = errors.New("nacl: x25519 private key has wrong length")

	// ErrInvalidEphemeralKey indicates the ephemeral public key
	// in a sealed envelope is the wrong length.
	ErrInvalidEphemeralKey = errors.New("nacl: ephemeral public key has wrong length")

	// ErrInvalidNonce indicates the nonce in a sealed entry is
	// the wrong length.
	ErrInvalidNonce = errors.New("nacl: nonce has wrong length")

	// ErrDecryptFailed indicates Open could not authenticate the
	// ciphertext under the supplied keys — wrong recipient, wrong
	// sender, tampered ciphertext. Collapsed to avoid leaking
	// which specific failure occurred.
	ErrDecryptFailed = errors.New("nacl: ciphertext could not be authenticated")
)

// X25519 byte lengths per RFC 7748.
const (
	X25519KeyLen = 32
	NonceLen     = 24
)

// SealedRecipient is one (nonce, ciphertext) pair for one
// recipient inside a multi-recipient envelope.
type SealedRecipient struct {
	Nonce      []byte // 24 bytes
	Ciphertext []byte
}

// SealedEnvelope is the multi-recipient output of Seal — one
// ephemeral public key plus one SealedRecipient per recipient,
// in the same order as the recipient public keys passed to Seal.
type SealedEnvelope struct {
	EphemeralPublicKey []byte // 32 bytes
	Recipients         []SealedRecipient
}

// GenerateActorEncryptionKeypair returns a fresh X25519 keypair
// for an actor (a user). The public key is published in the
// actor doc; the private key is encrypted at rest (atrest
// package) and decrypted in memory only when the user receives a
// NaCl-box envelope.
func GenerateActorEncryptionKeypair() (pub, priv []byte, err error) {
	private := make([]byte, X25519KeyLen)
	if _, err := rand.Read(private); err != nil {
		return nil, nil, fmt.Errorf("nacl: read random for x25519 private key: %w", err)
	}
	// curve25519.X25519(scalar, basepoint) gives the public key.
	// The library applies the standard clamp internally.
	public, err := curve25519.X25519(private, curve25519.Basepoint)
	if err != nil {
		return nil, nil, fmt.Errorf("nacl: derive x25519 public key: %w", err)
	}
	return public, private, nil
}

// Seal encrypts plaintext to N recipients (their X25519 public
// keys) under a single freshly-generated ephemeral X25519
// keypair. Each recipient gets a fresh random 24-byte nonce.
//
// The ephemeral keypair is generated inside Seal — the caller
// does not see or supply one. This is deliberate: every Seal
// call produces a fresh ephemeral key so the same plaintext
// encrypted twice never produces identical ciphertexts.
//
// Tests that need deterministic output use SealDet (below), which
// accepts an explicit ephemeral keypair + per-recipient nonces.
// Production code MUST use Seal.
func Seal(plaintext []byte, recipientPubs [][]byte) (*SealedEnvelope, error) {
	ephPub, ephPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("nacl: generate ephemeral key: %w", err)
	}
	return sealWithEphemeral(plaintext, recipientPubs, ephPub, ephPriv, nil)
}

// SealDet is the deterministic-output sibling of Seal — accepts
// caller-supplied ephemeral keys and (optionally) per-recipient
// nonces. Intended for conformance vectors + reproducible test
// fixtures only.
//
// Production code MUST call Seal so that ephemeral keys + nonces
// are randomly generated.
//
// If perRecipientNonces is nil, fresh random nonces are generated.
// If non-nil, it MUST have len == len(recipientPubs).
func SealDet(
	plaintext []byte,
	recipientPubs [][]byte,
	ephemeralPub, ephemeralPriv *[32]byte,
	perRecipientNonces [][]byte,
) (*SealedEnvelope, error) {
	if ephemeralPub == nil || ephemeralPriv == nil {
		return nil, errors.New("nacl: SealDet requires both ephemeral keys")
	}
	if perRecipientNonces != nil && len(perRecipientNonces) != len(recipientPubs) {
		return nil, errors.New("nacl: perRecipientNonces length must equal recipients length")
	}
	return sealWithEphemeral(plaintext, recipientPubs, ephemeralPub, ephemeralPriv, perRecipientNonces)
}

// sealWithEphemeral does the work for both Seal and SealDet.
// perRecipientNonces may be nil (generate random nonces).
func sealWithEphemeral(
	plaintext []byte,
	recipientPubs [][]byte,
	ephPub, ephPriv *[32]byte,
	perRecipientNonces [][]byte,
) (*SealedEnvelope, error) {
	out := &SealedEnvelope{
		EphemeralPublicKey: append([]byte(nil), ephPub[:]...),
		Recipients:         make([]SealedRecipient, len(recipientPubs)),
	}
	for i, rxPubBytes := range recipientPubs {
		if len(rxPubBytes) != X25519KeyLen {
			return nil, fmt.Errorf("nacl: recipient %d: %w", i, ErrInvalidPublicKey)
		}
		var rxPub [X25519KeyLen]byte
		copy(rxPub[:], rxPubBytes)

		var nonce [NonceLen]byte
		if perRecipientNonces != nil {
			if len(perRecipientNonces[i]) != NonceLen {
				return nil, fmt.Errorf("nacl: nonce[%d]: %w", i, ErrInvalidNonce)
			}
			copy(nonce[:], perRecipientNonces[i])
		} else {
			if _, err := rand.Read(nonce[:]); err != nil {
				return nil, fmt.Errorf("nacl: nonce: %w", err)
			}
		}
		ct := box.Seal(nil, plaintext, &nonce, &rxPub, ephPriv)
		out.Recipients[i] = SealedRecipient{
			Nonce:      append([]byte(nil), nonce[:]...),
			Ciphertext: ct,
		}
	}
	return out, nil
}

// Open decrypts the ciphertext for the recipient whose X25519
// private key is supplied. The caller selects which entry from
// SealedEnvelope.Recipients corresponds to them; this function
// just unwraps that one entry.
//
// Returns ErrDecryptFailed on any cryptographic failure (wrong
// recipient key, tampered ciphertext, wrong sender). The error is
// collapsed to avoid side-channel-flavoured leakage.
func Open(ciphertext, nonce, ephemeralPub, recipientPriv []byte) ([]byte, error) {
	if len(ephemeralPub) != X25519KeyLen {
		return nil, ErrInvalidEphemeralKey
	}
	if len(nonce) != NonceLen {
		return nil, ErrInvalidNonce
	}
	if len(recipientPriv) != X25519KeyLen {
		return nil, ErrInvalidPrivateKey
	}
	var nonceArr [NonceLen]byte
	copy(nonceArr[:], nonce)
	var ephArr [X25519KeyLen]byte
	copy(ephArr[:], ephemeralPub)
	var rxPriv [X25519KeyLen]byte
	copy(rxPriv[:], recipientPriv)
	plain, ok := box.Open(nil, ciphertext, &nonceArr, &ephArr, &rxPriv)
	if !ok {
		return nil, ErrDecryptFailed
	}
	return plain, nil
}
