// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-f — receiver-side decryption helper with the
// retained-key fallback walk.
//
// I-e shipped the sender side: the dispatcher seals env.Extra
// against the recipient's current encryption pubkey + emits an
// envelope with the encryption block populated. I-f is the
// counterpart: the inbox dispatcher unwraps + opens that block
// using the recipient's current key (the common case) OR walks
// the retained-key set if the sender used an older recipient
// key version (the rotation grace window).
//
// # Scope
//
// This file ships the user-key-walk + decrypt primitive. The
// dispatcher wiring lives in commit 2 — Dispatcher.dispatchOne
// gains a stage-4 call into [DecryptForUser] between envelope
// re-parse + per-verb handler invocation.
//
// # Why DecryptForUser is package-level + not a method
//
// The helper has two collaborators: a `*pgxpool.Pool` (for the
// userkeys sqlc query) and the federation.DecryptActivityPayload
// primitive. Neither belongs to the Dispatcher's identity. A
// package-level function tests easily without standing up a
// Dispatcher; integration tests in commit 2 cover the
// orchestration end-to-end.

package inbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
)

// DecryptResult is what [DecryptForUser] returns on success.
// Captures which receiver-key version actually opened the
// ciphertext + how many attempts the walk needed.
//
// DecryptedWithKeyVersion + AttemptCount are observability for
// the inbox row's `decrypted_with_key_version` column +
// `federation.inbox.decrypted` audit event (both wired by
// commit 2). AttemptCount=1 is the steady state; >1 means the
// rotation grace window kicked in.
type DecryptResult struct {
	Plaintext               []byte
	DecryptedWithKeyVersion int32
	AttemptCount            int
}

// ErrSenderKeyMissing is returned when the caller's sender-pubkey
// argument is empty. Distinct from
// [federation.ErrEncryptionDecryptFailed] (which collapses all
// "tried + failed" paths) so the dispatcher can route this
// specific case to the right audit reason — pre-I-c peer that
// never advertised an encryption key.
var ErrSenderKeyMissing = errors.New("inbox: sender encryption key not available")

// ErrNoReceiverKey is returned when the recipient user has no
// current + no retained keys. Post-I-b invariant says every
// user has a current key; this surfaces a defensive failure
// rather than a misleading "wrong key" rejection.
var ErrNoReceiverKey = errors.New("inbox: recipient has no current or retained keys")

// DecryptForUser walks the recipient's keys in (is_current DESC,
// version DESC) order — the [userkeys.Queries.ListUserKeysForDecrypt]
// query's ordering — and attempts NaCl-box decrypt against the
// supplied sender public key + envelope nonce + ciphertext.
//
// Returns the first success. After every attempt (success OR
// failure), the unwrapped private key bytes are zeroed so a
// later goroutine can't read them off the stack via reflection
// or unsafe. The deferred zero runs even if the function
// returns early via panic.
//
// Returns [ErrNoReceiverKey] if the recipient has no current +
// no retained keys, [ErrSenderKeyMissing] if senderPub is empty,
// [federation.ErrEncryptionDecryptFailed] if no retained key
// works. Any DB or unwrap error propagates as a wrapped error.
func DecryptForUser(
	ctx context.Context,
	pool *pgxpool.Pool,
	recipientUserRef int64,
	senderPub, nonce, ciphertext []byte,
) (*DecryptResult, error) {
	if len(senderPub) == 0 {
		return nil, ErrSenderKeyMissing
	}

	rows, err := userkeys.New(pool).ListUserKeysForDecrypt(ctx, recipientUserRef)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrNoReceiverKey
	}

	for attempt, row := range rows {
		plaintext, ok := tryOne(row.PrivateKeyEnc, senderPub, nonce, ciphertext)
		if ok {
			return &DecryptResult{
				Plaintext:               plaintext,
				DecryptedWithKeyVersion: row.Version,
				AttemptCount:            attempt + 1,
			}, nil
		}
	}
	return nil, federation.ErrEncryptionDecryptFailed
}

// tryOne unwraps a single wrapped private key + attempts decrypt.
// Returns (plaintext, true) on success, (nil, false) on any
// failure (unwrap error, decrypt auth tag mismatch). All failure
// modes collapse to false so the caller can retry the next
// retained key without information leak on which specific
// stage failed.
//
// The unwrapped 32-byte scalar is zeroed via the defer before
// returning. nacl/box doesn't expose its internal copy lifetime
// + we can't reach inside it, but the caller's view of the
// secret has a known short lifetime.
func tryOne(wrappedPriv, senderPub, nonce, ciphertext []byte) ([]byte, bool) {
	priv, err := userkeys.Unwrap(wrappedPriv)
	if err != nil {
		return nil, false
	}
	privBytes := priv.Bytes()
	defer func() {
		for i := range privBytes {
			privBytes[i] = 0
		}
	}()

	plaintext, err := federation.DecryptActivityPayload(ciphertext, nonce, privBytes, senderPub)
	if err != nil {
		return nil, false
	}
	return plaintext, true
}
