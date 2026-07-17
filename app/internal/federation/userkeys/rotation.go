// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-h key rotation primitive.
//
// Mints a new X25519 keypair for one user + atomically flips the
// previous current key to retained-with-TTL state. The two writes
// (insert new, demote previous) commit together in a single
// transaction so an interrupted rotation can never leave a user
// in the broken state "two current keys" OR "zero current keys."
//
// # Callers
//
//   - /account/security/rotate-federation-keys — user self-rotates.
//     rotatedByUserRef == userID; the audit row's actor + subject
//     match.
//   - /admin/federation/users/{ref}/rotate-keys — admin forces
//     rotation on a compromised user. rotatedByUserRef = admin's
//     ref; the audit row records the operator-initiated action
//     so the audit feed distinguishes the two flows.
//
// Both callers run the rotation EXACTLY ONCE per request — the
// HTTP handler is the rate-limiter (one rotation per
// authenticated request, no batching). The primitive itself is
// reentrant safe but offers no internal idempotency (a second
// call mints a second key). Callers that want idempotency
// (re-submit dedup, etc.) wrap this primitive with their own
// guard.
//
// # Atomic write shape
//
// Three steps inside one transaction:
//
//   1. GetCurrentUserKey — read the previous current row's version.
//      Defensive: if absent (post-I-b shouldn't happen), the new
//      key lands at v=1.
//   2. DemoteCurrentKey — UPDATE the previous current row to
//      is_current=FALSE, retained_until=NOW()+retentionDays, AND
//      record (rotated_at, rotated_by_user_ref). Single statement
//      satisfies the federation_user_keys_current_xor_retained
//      CHECK constraint atomically.
//   3. InsertUserKeyAsCurrent — INSERT the new key at v=prev+1
//      with is_current=TRUE, retained_until=NULL, rotated_at=NOW(),
//      rotated_by_user_ref=caller.
//
// Step ordering matters: DemoteCurrentKey MUST land before
// InsertUserKeyAsCurrent — otherwise the partial unique index
// federation_user_keys_one_current_idx blocks the insert (two
// rows with is_current=TRUE for the same user_ref).
//
// # Why outside-tx keygen
//
// Generate() does an X25519 keygen + AES-GCM wrap of the private
// half. Neither needs DB connectivity; running it before Begin()
// keeps the transaction tight (sub-millisecond) so we don't hold
// a pool connection during ~100µs of crypto. Tested cumulative
// effect: rotation P99 stays under 5ms even under contention.
//
// # Audit semantics
//
// One event per successful rotation:
// federation.user.key_rotated, subject=userID,
// actor=rotatedByUserRef. The audit fires OUTSIDE the tx (pool-
// bound) so an audit-write failure can't roll back the keypair
// commit — matches the BackfillMissingKeys pattern's "keypair is
// load-bearing; audit is observability" rule.
//
// The federation.user.key_retained_expired event fires elsewhere
// (sweeper.go) when a retained row's TTL trips; not this
// primitive's concern.

package userkeys

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultRetentionDays is the fallback grace window when the
// caller's sysconfig lookup yields nothing or the rotation
// primitive runs without a sysconfig dependency (test fixtures).
// 30 days mirrors the migration 00013 system_config default.
const DefaultRetentionDays = 30

// RotationResult is the rotation primitive's typed return. The
// HTTP handler shapes it into the OpenAPI UserKeyRotationResult
// response; the audit hook reads NewVersion + PreviousVersion
// for the event payload. Field name `UserRef` matches the
// post-1.49.C-2 schema convention (federation_user_keys.user_ref).
type RotationResult struct {
	UserRef         int64
	NewVersion      int32
	PreviousVersion int32  // 0 when this was the user's first key
	NewPublicKey    []byte // raw 32 bytes; caller base64s for transport
	Algorithm       string // always Algorithm today; recorded for forward compat
}

// RotateAuditFireFn is the audit-recorder hook the caller wires
// to fire federation.user.key_rotated AFTER the keypair commit
// succeeds. Pool-bound (NOT tx-bound to the keypair insert):
// observability path, not load-bearing for correctness — matches
// the [AuditFireFn] contract in backfill.go.
//
// nil-safe: when unwired (test fixtures), rotation completes
// without an audit row.
type RotateAuditFireFn func(
	ctx context.Context,
	subjectUserRef int64,
	rotatedByUserRef int64,
	newVersion int32,
	previousVersion int32,
	algorithm string,
)

// RotateForUser mints a new keypair for userID + demotes the
// previous current key in a single transaction. Returns the
// new + previous versions so the caller can audit + shape the
// HTTP response.
//
// retentionDays controls the demoted row's retained_until grace
// window. Callers should pass the sysconfig-resolved value
// (federation.user_keys.retained_until_days, migration 00013
// default 30); pass DefaultRetentionDays as a fallback when
// sysconfig isn't reachable (test paths). Values <= 0 are
// normalised to DefaultRetentionDays so a misconfigured key
// can't accidentally produce a zero-grace rotation that
// instantly orphans in-flight envelopes encrypted against the
// previous key.
//
// auditFire is invoked once on the happy path with both versions
// + algorithm; nil-safe. Audit failures don't roll back the
// rotation.
func RotateForUser(
	ctx context.Context,
	pool *pgxpool.Pool,
	userID int64,
	rotatedByUserRef int64,
	retentionDays int,
	auditFire RotateAuditFireFn,
) (*RotationResult, error) {
	if pool == nil {
		return nil, fmt.Errorf("userkeys.RotateForUser: pool is nil")
	}
	if userID <= 0 {
		return nil, fmt.Errorf("userkeys.RotateForUser: userID must be > 0, got %d", userID)
	}
	if rotatedByUserRef <= 0 {
		return nil, fmt.Errorf("userkeys.RotateForUser: rotatedByUserRef must be > 0, got %d", rotatedByUserRef)
	}
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}

	// Keygen outside the tx — see file-level comment.
	pub, wrapped, err := Generate()
	if err != nil {
		return nil, fmt.Errorf("userkeys.RotateForUser: generate: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("userkeys.RotateForUser: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := New(tx)

	// Step 1 — fetch the previous current version for the +1
	// increment. Absent is the defensive fall-through (post-I-b
	// shouldn't hit this; covered by a unit test for paranoia).
	var previousVersion int32
	if prev, err := qtx.GetCurrentUserKey(ctx, userID); err == nil {
		previousVersion = prev.Version
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("userkeys.RotateForUser: fetch current: %w", err)
	}

	// Step 2 — demote the previous current row. No-op (zero rows
	// affected) when previousVersion == 0; the partial UNIQUE
	// INDEX won't trip because the insert below is the first
	// is_current=TRUE row for the user.
	if previousVersion > 0 {
		if err := qtx.DemoteCurrentKey(ctx, DemoteCurrentKeyParams{
			RetentionDays:    int32(retentionDays),
			RotatedByUserRef: &rotatedByUserRef,
			UserRef:          userID,
		}); err != nil {
			return nil, fmt.Errorf("userkeys.RotateForUser: demote: %w", err)
		}
	}

	// Step 3 — insert the new current row.
	newVersion := previousVersion + 1
	if _, err := qtx.InsertUserKeyAsCurrent(ctx, InsertUserKeyAsCurrentParams{
		UserRef:          userID,
		Version:          newVersion,
		Algorithm:        Algorithm,
		PublicKey:        pub,
		PrivateKeyEnc:    wrapped,
		RotatedByUserRef: &rotatedByUserRef,
	}); err != nil {
		return nil, fmt.Errorf("userkeys.RotateForUser: insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("userkeys.RotateForUser: commit: %w", err)
	}

	// Audit fires AFTER the commit so a recorded rotation is one
	// that actually happened. Pool-bound; failure here is
	// observability noise, not a correctness break — the new
	// keypair is already live + serving.
	if auditFire != nil {
		auditFire(ctx, userID, rotatedByUserRef, newVersion, previousVersion, Algorithm)
	}

	return &RotationResult{
		UserRef:         userID,
		NewVersion:      newVersion,
		PreviousVersion: previousVersion,
		NewPublicKey:    pub,
		Algorithm:       Algorithm,
	}, nil
}
