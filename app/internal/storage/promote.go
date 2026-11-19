// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

// Promoting a derived variant to an immutable access copy (#1596).
//
// A derived variant lives UNDER ITS PARENT's hash and is overwritten by
// every regeneration (PutVariant bumps its generation). A version's file
// set must never change once sealed, so it cannot point at one. Promotion
// copies the variant's bytes into their OWN content-addressed object,
// addressed by the variant's own SHA-256. A later regeneration of the
// parent's variant then cannot alter or remove the copy.
//
// # What is promoted
//
// Only a variant whose row says verified (#1597), with a recorded digest
// and size, and whose stored bytes, read under the variant's shared lock,
// hash to exactly that digest and size. Anything else fails closed and
// changes nothing: an unverifiable, mismatched or missing row, a missing
// key, missing bytes, or bytes that changed since they were recorded. The
// variant's row is only read; recording a verdict is the sweep's job.
//
// # How it is stored
//
// The bytes are staged to a private temp file under the lock. The lock
// (and its connection) is given up BEFORE any other database work, so no
// second pool connection is taken while it is held (#1616). Then:
//
//   - if an object with the digest already exists (an upload of the same
//     bytes, or an earlier promotion), it is only pinned: never written
//     again and never relabelled;
//   - otherwise the staged copy goes through ImportVerified, the verified
//     import path, with KeepExistingOriginal, so an object that appears
//     concurrently is not rewritten either.
//
// Either way the result is confirmed before it is returned, while the
// promotion's pin holds the object: it is recorded at the digest's size,
// its original row records that digest as verified, and its stored bytes,
// read in full, hash to exactly that digest and size. An existing object
// that cannot show this (an original recorded before #1597 with no
// digest, or bytes that changed under a row that still says verified) is
// refused, and only this promotion's pin is released.
//
// Concurrent writers: the import runs with KeepExistingOriginal, which
// takes the per-hash import lock EXCLUSIVELY and checks again under it.
// Every other writer of an original takes the same lock SHARED around its
// own check, Put and record: ImportVerified, and UploadOriginal. So a
// writer that records the hash after the promotion's first check has
// either finished (then the promotion writes nothing and only pins) or
// waits, and once the promotion has recorded the hash a waiting upload
// finds the row and deduplicates. A recorded original is never written
// again by a promotion.
//
// # Lifetime: the transient pin
//
// The copy is held by a pin of its own, (`version_promote`, <unique id>),
// written in the same transaction that records or claims the object
// (lock first, then pin: the order of every pin writer). So the copy is
// never left unpinned and GC-eligible between promotion and use:
//
//   - PromoteVariant returns with that pin HELD. The caller (the
//     reconcile slice) seals the version, which pins the copy to the
//     version (migration 00084), and only then calls ReleasePromotion. A
//     sealed, published copy is therefore pinned at every moment.
//   - On every failure after the pin is written (the confirmation),
//     PromoteVariant removes its own pin before returning. A failure
//     before it leaves nothing: the pin commits only with the object.
//   - ReleasePromotion is RemovePin: an object left with no pin at all
//     (a copy nothing sealed) is scheduled for GC after the grace, as it
//     should be.
//   - A process that dies between promotion and release leaves the pin
//     behind, which keeps the bytes (the safe direction); ReleaseStalePins
//     with PinSubjectTypeVersionPromote removes such pins.
//
// Each call has its own pin id, so concurrent promotions of the same
// bytes hold and release independently.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PinSubjectTypeVersionPromote is the transient pin a promotion holds on
// its copy until a sealed version pins it (#1596).
const PinSubjectTypeVersionPromote = "version_promote"

var (
	// ErrPromoteNotVerified: the variant's row is not verified, or has no
	// recorded digest or size.
	ErrPromoteNotVerified = errors.New("storage: promote: variant is not verified")
	// ErrPromoteMissing: the variant has no row, or its bytes are not on
	// the backend.
	ErrPromoteMissing = errors.New("storage: promote: variant is missing")
	// ErrPromoteChanged: the stored bytes no longer hash to the recorded
	// digest and size.
	ErrPromoteChanged = errors.New("storage: promote: variant bytes do not match their recorded digest")
	// ErrPromoteExistingUnverified: an object with the variant's digest
	// already exists but cannot show verified bytes of that size.
	ErrPromoteExistingUnverified = errors.New("storage: promote: the existing object for this digest is not verified")
)

// Promotion is one promoted access copy, held by Pin until released.
type Promotion struct {
	ParentHash string
	VariantKey string
	Generation int64  // the variant's generation that was promoted
	Hash       string // the copy's own SHA-256: its object hash
	Size       int64
	MediaType  string
	// Deduped: the object already existed (an upload or an earlier
	// promotion of the same bytes) and was only pinned.
	Deduped bool
	// Pin holds the copy. Release it with ReleasePromotion once a durable
	// pin (the sealed version's) holds the copy, or when it is not used.
	Pin PinRef
}

// promoteHooks are #1596 test seams; zero in production.
type promoteHooks struct {
	afterRead    func(parentHash, key string)   // under the variant's shared lock, after the bytes were read
	beforeCommit func(ctx context.Context) error // the import's BeforeCommit
}

// PromoteVariant copies the verified derived variant (parentHash, key)
// into its own content-addressed object and returns it held by a
// transient pin. See this file's header for every rule.
func (s *Service) PromoteVariant(ctx context.Context, parentHash, key string) (*Promotion, error) {
	if err := ValidateHash(parentHash); err != nil {
		return nil, err
	}
	if key == "" || key == VariantOriginal {
		return nil, errors.New("storage: promote: a derived variant key is required; an original is never promoted")
	}
	tmpDir := s.TempDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: promote: temp dir: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "aa-promote-*")
	if err != nil {
		return nil, fmt.Errorf("storage: promote: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	p := &Promotion{ParentHash: parentHash, VariantKey: key}
	err = s.withVariantLock(ctx, parentHash, key, false, func(q *Queries) error {
		row, err := q.GetVariantFixity(ctx, GetVariantFixityParams{ObjectHash: parentHash, VariantKey: key})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s/%s has no row", ErrPromoteMissing, short(parentHash), key)
		}
		if err != nil {
			return fmt.Errorf("storage: promote: read %s/%s: %w", short(parentHash), key, err)
		}
		fx := fixityFromRow(row)
		if fx.Status != FixityVerified || fx.SHA256 == "" || fx.Size < 0 {
			return fmt.Errorf("%w: %s/%s is %s", ErrPromoteNotVerified, short(parentHash), key, fx.Status)
		}
		rc, _, err := s.Backend.Get(ctx, parentHash, key)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: %s/%s has no stored bytes", ErrPromoteMissing, short(parentHash), key)
		}
		if err != nil {
			return fmt.Errorf("storage: promote: get %s/%s: %w", short(parentHash), key, err)
		}
		sum := sha256.New()
		n, err := io.Copy(io.MultiWriter(tmp, sum), rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("storage: promote: read %s/%s: %w", short(parentHash), key, err)
		}
		if digest := hex.EncodeToString(sum.Sum(nil)); digest != fx.SHA256 || n != fx.Size {
			return fmt.Errorf("%w: %s/%s recorded %s (%d bytes), stored %s (%d bytes)",
				ErrPromoteChanged, short(parentHash), key, short(fx.SHA256), fx.Size, short(digest), n)
		}
		if s.promoteHooks.afterRead != nil {
			s.promoteHooks.afterRead(parentHash, key)
		}
		p.Hash, p.Size, p.MediaType, p.Generation = fx.SHA256, fx.Size, row.ContentType, fx.Generation
		return nil
	})
	if cerr := tmp.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("storage: promote: close temp: %w", cerr)
	}
	if err != nil {
		return nil, err
	}

	// The variant's lock and connection are released: from here on, pool
	// work only (#1616).
	p.Pin = PinRef{SubjectType: PinSubjectTypeVersionPromote, SubjectID: uuid.NewString()}
	exists, err := New(s.Pool).ObjectRowExists(ctx, p.Hash)
	if err != nil {
		return nil, fmt.Errorf("storage: promote: lookup %s: %w", short(p.Hash), err)
	}
	if exists {
		// Never written again, never relabelled: only claimed.
		if err := s.AddPin(ctx, p.Pin, p.Hash); err != nil {
			return nil, fmt.Errorf("storage: promote: pin existing %s: %w", short(p.Hash), err)
		}
		p.Deduped = true
	} else {
		res, err := s.ImportVerified(ctx, ImportRequest{
			TempPath:             tmpName,
			Hash:                 p.Hash,
			Size:                 p.Size,
			ContentType:          p.MediaType,
			IntentID:             uuid.New(),
			Pin:                  p.Pin,
			BeforeCommit:         s.promoteHooks.beforeCommit,
			KeepExistingOriginal: true,
		})
		if err != nil {
			return nil, fmt.Errorf("storage: promote: import %s: %w", short(p.Hash), err)
		}
		p.Deduped = res.Deduped
	}
	if err := s.confirmPromoted(ctx, p); err != nil {
		if rerr := s.ReleasePromotion(context.WithoutCancel(ctx), p); rerr != nil {
			return nil, fmt.Errorf("%w (and releasing its pin failed: %v)", err, rerr)
		}
		return nil, err
	}
	return p, nil
}

// confirmPromoted checks that the pinned object is what the promotion
// read: recorded at its size, its original row verified with that digest
// and size, and its stored bytes, read in full, hashing to that digest at
// that size (a same-length corruption under a stale verified row fails).
func (s *Service) confirmPromoted(ctx context.Context, p *Promotion) error {
	q := New(s.Pool)
	obj, err := q.FindObject(ctx, p.Hash)
	if err != nil {
		return fmt.Errorf("%w: %s: object row: %v", ErrPromoteExistingUnverified, short(p.Hash), err)
	}
	row, err := q.GetVariantFixity(ctx, GetVariantFixityParams{ObjectHash: p.Hash, VariantKey: VariantOriginal})
	if err != nil {
		return fmt.Errorf("%w: %s: original row: %v", ErrPromoteExistingUnverified, short(p.Hash), err)
	}
	fx := fixityFromRow(row)
	if obj.SizeBytes != p.Size || fx.Status != FixityVerified || fx.SHA256 != p.Hash || fx.Size != p.Size {
		return fmt.Errorf("%w: %s recorded %d bytes, original %s %s (%d bytes)",
			ErrPromoteExistingUnverified, short(p.Hash), obj.SizeBytes, fx.Status, short(fx.SHA256), fx.Size)
	}
	// The row is a claim; the bytes are the proof. Read the stored
	// original in full and hash it (an original has no write lock: every
	// writer of a hash writes the bytes that hash names). Nothing is held
	// but this promotion's pin, so no second connection is involved.
	digest, n, found, err := s.streamDigest(ctx, p.Hash, VariantOriginal)
	if err != nil {
		return fmt.Errorf("%w: %s: reading the stored bytes: %v", ErrPromoteExistingUnverified, short(p.Hash), err)
	}
	if !found || digest != p.Hash || n != p.Size {
		return fmt.Errorf("%w: %s: stored bytes found=%v hash to %s (%d bytes), want %d bytes",
			ErrPromoteExistingUnverified, short(p.Hash), found, short(digest), n, p.Size)
	}
	return nil
}

// ReleasePromotion removes the promotion's transient pin. Call it after a
// durable pin (the sealed version's) holds the copy, or to abandon an
// unused promotion; an object left with no pin is then scheduled for GC
// after the grace period (RemovePin). Idempotent.
func (s *Service) ReleasePromotion(ctx context.Context, p *Promotion) error {
	if p == nil || p.Hash == "" {
		return errors.New("storage: release promotion: nothing to release")
	}
	return s.RemovePin(ctx, p.Pin, p.Hash)
}
