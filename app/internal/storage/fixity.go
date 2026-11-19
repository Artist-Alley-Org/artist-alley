// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

// Variant fixity (#1597). Every stored variant carries its own SHA-256
// and size, computed by this server from the bytes it handed the backend,
// bound to the write (generation) that produced them, with an explicit
// status (migration 00082):
//
//	verified      a recorded digest that, if a sweep has read the bytes
//	              since, still matches
//	unverifiable  no trustworthy digest (every row older than 00082, a
//	              row healed from a backend Stat, a write in progress or
//	              interrupted); never promoted except by an explicit
//	              recheck (RecheckVariant) or a regeneration
//	mismatch      the stored bytes no longer match the recorded digest or
//	              size
//	missing       the row's bytes are not on the backend
//
// The one way to write a derived variant is PutVariant. It takes the
// variant's write lock, marks the row unverifiable (committed), streams
// the bytes to the backend through a hasher, then records the digest for
// that generation. Writers of one variant are serialized by the lock, so
// a row can only ever describe the bytes that were Put last, and a writer
// that dies mid-write leaves a row that says unverifiable rather than one
// that claims the previous bytes' digest. Readers that need the row and
// the bytes to agree (VerifyVariant, RecheckVariant, ReadVariantFixity)
// take the same lock, shared or exclusive.
//
// Originals are different: an original is addressed by its own SHA-256,
// so its digest is object_hash, computed from the streamed bytes by
// UploadOriginal and re-checked against the staged file by
// ImportVerified. Two writers of one hash can only write identical
// bytes, so originals need no write lock (RecordOriginalVariant).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Fixity statuses: storage_variants.fixity_status (migration 00082).
const (
	FixityVerified     = "verified"
	FixityUnverifiable = "unverifiable"
	FixityMismatch     = "mismatch"
	FixityMissing      = "missing"
)

// Digest sources: storage_variants.digest_source.
const (
	DigestFromWrite   = "write"
	DigestFromRecheck = "recheck"
)

// ErrVariantNotRecorded wraps a PutVariant failure that came AFTER the
// bytes were written: the backend holds them, but the row was not
// recorded (or a newer write moved it). The row then says unverifiable,
// never a stale digest. Callers that used to tolerate a failed row write
// can keep doing so with errors.Is.
var ErrVariantNotRecorded = errors.New("storage: variant bytes written but its digest was not recorded")

// variantWriteSlots bounds how many PutVariant calls hold a database
// connection (and the variant's write lock) for the length of a backend
// Put. At the smallest supported pool (AA_DB_MAX_CONNS=8, of which three
// are held by LISTEN workers, #1605) that leaves three of the five query
// connections for requests and jobs however many variants are uploading.
// A writer waits for a slot BEFORE it asks the pool for a connection, so
// the bound adds no hold-and-wait.
const variantWriteSlots = 2

// VariantWrite is what PutVariant recorded.
type VariantWrite struct {
	SHA256     string
	Size       int64
	Generation int64
}

// VariantFixity is what a variant's row says about its bytes, read under
// the variant's lock. SHA256 is empty and Size -1 when no digest is
// recorded. Nothing emits derived variants to peers yet: the CAS handler
// (#72) serves only originals by hash, so archival-1 cannot list a derived
// copy until #1574 designs a grant-gated, fetchable path for derived
// bytes. When it does, a variant may be listed only with its OWN verified
// digest from here, never on the strength of the parent object's hash.
type VariantFixity struct {
	ObjectHash string
	VariantKey string
	SHA256     string
	Size       int64
	Source     string
	Status     string
	Generation int64
}

// variantHooks are #1597 test seams; zero in production.
type variantHooks struct {
	afterPut func(variant string) // after the backend Put, before the record step
}

type variantSem struct {
	once  sync.Once
	slots chan struct{}
}

func (s *Service) acquireVariantSlot(ctx context.Context) (func(), error) {
	s.variantSem.once.Do(func() { s.variantSem.slots = make(chan struct{}, variantWriteSlots) })
	select {
	case s.variantSem.slots <- struct{}{}:
		return func() { <-s.variantSem.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// withVariantLock runs fn on ONE pooled connection holding the variant's
// session lock (exclusive for writers, shared for readers). fn must do
// all of its database work on q: a second pool acquire while holding
// this connection is the hold-and-wait that deadlocks a small pool
// (#1616). The unlock runs on a fresh context so a cancelled caller still
// gives the lock back; if even that fails, the connection is closed, and
// the session (with its lock) ends.
func (s *Service) withVariantLock(ctx context.Context, hash, variant string, exclusive bool, fn func(q *Queries) error) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("storage: variant lock: acquire connection: %w", err)
	}
	defer conn.Release()
	q := New(conn)
	if exclusive {
		err = q.LockVariantWrite(ctx, LockVariantWriteParams{ObjectHash: hash, VariantKey: variant})
	} else {
		err = q.LockVariantRead(ctx, LockVariantReadParams{ObjectHash: hash, VariantKey: variant})
	}
	if err != nil {
		return fmt.Errorf("storage: variant lock %s/%s: %w", short(hash), variant, err)
	}
	defer func() {
		uctx := context.WithoutCancel(ctx)
		var ok bool
		var uerr error
		if exclusive {
			ok, uerr = q.UnlockVariantWrite(uctx, UnlockVariantWriteParams{ObjectHash: hash, VariantKey: variant})
		} else {
			ok, uerr = q.UnlockVariantRead(uctx, UnlockVariantReadParams{ObjectHash: hash, VariantKey: variant})
		}
		if uerr != nil || !ok {
			_ = conn.Conn().Close(uctx)
		}
	}()
	return fn(q)
}

// countingReader counts what the backend actually read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// PutVariant writes a DERIVED variant's bytes and records their digest
// (#1597). It is the only way to write one: every preview, ladder,
// poster, sprite and embedded-preview writer goes through it (a guard
// test fails on any other writer of variant rows or bytes). The SHA-256
// and size are computed from the stream the backend read, never taken
// from the caller; r is read once, start to end, without buffering, so a
// large variant streams.
//
// A failure before or during the Put leaves the row (if there was one)
// unverifiable and the old bytes in place (backends are atomic). A
// failure after it returns ErrVariantNotRecorded, again with the row
// unverifiable.
func (s *Service) PutVariant(ctx context.Context, hash, variant string, r io.Reader, contentType string) (*VariantWrite, error) {
	if hash == "" || variant == "" {
		return nil, errors.New("storage: PutVariant needs a hash and a variant key")
	}
	if variant == VariantOriginal {
		return nil, errors.New("storage: PutVariant writes derived variants; an original is written by UploadOriginal or ImportVerified")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	release, err := s.acquireVariantSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var out *VariantWrite
	err = s.withVariantLock(ctx, hash, variant, true, func(q *Queries) error {
		gen, err := q.BeginVariantWrite(ctx, BeginVariantWriteParams{ObjectHash: hash, VariantKey: variant})
		if errors.Is(err, pgx.ErrNoRows) {
			gen = 0
		} else if err != nil {
			return fmt.Errorf("storage: begin variant write %s/%s: %w", short(hash), variant, err)
		}

		sum := sha256.New()
		cr := &countingReader{r: io.TeeReader(r, sum)}
		info, err := s.Backend.Put(ctx, hash, variant, cr)
		if err != nil {
			return fmt.Errorf("storage: backend put %s/%s: %w", short(hash), variant, err)
		}
		// The digest describes what the backend read. A backend that
		// reports a different stored length did not store those bytes.
		if info != nil && info.Size != cr.n {
			return fmt.Errorf("%w: backend stored %d bytes of %s/%s, %d were streamed",
				ErrVariantNotRecorded, info.Size, short(hash), variant, cr.n)
		}
		digest := hex.EncodeToString(sum.Sum(nil))
		if s.variantHooks.afterPut != nil {
			s.variantHooks.afterPut(variant)
		}
		n, err := q.RecordVariantWrite(ctx, RecordVariantWriteParams{
			ObjectHash:    hash,
			VariantKey:    variant,
			SizeBytes:     cr.n,
			ContentType:   contentType,
			Metadata:      []byte("{}"),
			ContentSha256: &digest,
			Generation:    gen,
		})
		if err != nil {
			return fmt.Errorf("%w: %s/%s: %v", ErrVariantNotRecorded, short(hash), variant, err)
		}
		if n == 0 {
			return fmt.Errorf("%w: %s/%s moved past generation %d", ErrVariantNotRecorded, short(hash), variant, gen)
		}
		if gen == 0 {
			gen = 1
		}
		out = &VariantWrite{SHA256: digest, Size: cr.n, Generation: gen}
		return nil
	})
	return out, err
}

// ReadVariantFixity returns what a variant's row says about its bytes,
// read under the variant's shared lock, so it never sees a write half
// done. pgx.ErrNoRows when the variant has no row.
func (s *Service) ReadVariantFixity(ctx context.Context, hash, variant string) (VariantFixity, error) {
	var out VariantFixity
	err := s.withVariantLock(ctx, hash, variant, false, func(q *Queries) error {
		row, err := q.GetVariantFixity(ctx, GetVariantFixityParams{ObjectHash: hash, VariantKey: variant})
		if err != nil {
			return err
		}
		out = fixityFromRow(row)
		return nil
	})
	return out, err
}

func fixityFromRow(row GetVariantFixityRow) VariantFixity {
	fx := VariantFixity{
		ObjectHash: row.ObjectHash,
		VariantKey: row.VariantKey,
		Size:       -1,
		Status:     row.FixityStatus,
		Generation: row.Generation,
	}
	if row.ContentSha256 != nil {
		fx.SHA256 = *row.ContentSha256
	}
	if row.ContentSizeBytes != nil {
		fx.Size = *row.ContentSizeBytes
	}
	if row.DigestSource != nil {
		fx.Source = *row.DigestSource
	}
	return fx
}

// streamDigest reads a variant's stored bytes to the end. found is false
// when the backend has no such bytes.
func (s *Service) streamDigest(ctx context.Context, hash, variant string) (digest string, n int64, found bool, err error) {
	rc, _, err := s.Backend.Get(ctx, hash, variant)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("get %s/%s: %w", short(hash), variant, err)
	}
	defer rc.Close()
	sum := sha256.New()
	n, err = io.Copy(sum, rc)
	if err != nil {
		return "", 0, true, fmt.Errorf("read %s/%s: %w", short(hash), variant, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), n, true, nil
}

// VariantObservation is what one integrity check of one stored variant
// saw (#1597, #1598). The sweep turns it into a finding (when Finding is
// set) and one preservation event per asset referencing the object.
// Gone means the row vanished before it could be checked: nothing was
// observed. Found is false when the row's bytes were not on the backend.
// ReadFailed means the backend could not deliver them (VerifyVariant
// then also returns the error); a lock, database or cancelled-context
// error is never a read failure, because nothing about the file was
// learned.
type VariantObservation struct {
	ObjectHash     string
	VariantKey     string
	Gone           bool
	ReadFailed     bool
	Found          bool
	Generation     int64
	ObservedSHA256 string // of the bytes read; "" when !Found or ReadFailed
	ObservedSize   int64
	RecordedSHA256 string // the row's own digest; "" when it has none
	StatusAfter    string // the row's fixity status after the check
	Finding        string // sweep finding kind; "" when healthy
	Detail         string // operator-facing finding detail (not for peers)
	ObservedAt     time.Time
}

// Fixity-check outcome reasons: asset_preservation_events.outcome_reason
// (migration 00083). A closed vocabulary, never free text.
const (
	ReasonDigestMatch      = "digest_match"
	ReasonNoRecordedDigest = "no_recorded_digest"
	ReasonChecksumMismatch = "checksum_mismatch"
	ReasonSizeMismatch     = "size_mismatch"
	ReasonMissingObject    = "missing_object"
	ReasonReadError        = "read_error"
)

// Outcome maps an observation to its PREMIS outcome and reason, the same
// rules migration 00083's outcome CHECK enforces. SUCCESS needs all three
// of: a recorded digest, the bytes read hashing to it, and the row left
// verified. A clean read of a row with no recorded digest (a legacy row,
// original or derived: an original's address can expose corruption but
// is not a recorded digest) is a WARNING and the row stays unverifiable.
// An observation that fits no rule returns ("", ""), and
// RecordSweepObservation refuses it rather than guess.
func (o VariantObservation) Outcome() (outcome, reason string) {
	switch {
	case o.ReadFailed:
		return "failure", ReasonReadError
	case !o.Found:
		return "failure", ReasonMissingObject
	case o.Finding == FindingChecksumMismatch:
		return "failure", ReasonChecksumMismatch
	case o.Finding == FindingSizeMismatch:
		return "failure", ReasonSizeMismatch
	case o.Finding != "":
		return "", ""
	case o.RecordedSHA256 == "" && o.StatusAfter == FixityUnverifiable:
		return "warning", ReasonNoRecordedDigest
	case o.RecordedSHA256 != "" && o.ObservedSHA256 == o.RecordedSHA256 && o.StatusAfter == FixityVerified:
		return "success", ReasonDigestMatch
	}
	return "", ""
}

// VerifyVariant streams one variant's stored bytes and compares them with
// its OWN recorded digest and size, under its shared lock (#1597), and
// returns what it observed. It moves the row's status (compare-and-set
// on the generation it read): missing bytes to missing, a recorded digest
// or size that no longer matches to mismatch, and a mismatch or missing
// row whose bytes match again back to verified. An unverifiable row is
// never promoted here: only its size is checked, and it stays
// unverifiable. Recording the observation (finding, preservation events)
// is the caller's, AFTER this returns and the lock is released, so no
// second pool connection is taken under the lock (#1616).
func (s *Service) VerifyVariant(ctx context.Context, hash, variant string) (VariantObservation, error) {
	obs := VariantObservation{ObjectHash: hash, VariantKey: variant}
	err := s.withVariantLock(ctx, hash, variant, false, func(q *Queries) error {
		row, err := q.GetVariantFixity(ctx, GetVariantFixityParams{ObjectHash: hash, VariantKey: variant})
		if errors.Is(err, pgx.ErrNoRows) {
			obs.Gone = true // removed since the sweep listed it
			return nil
		}
		if err != nil {
			return err
		}
		fx := fixityFromRow(row)
		obs.Generation, obs.RecordedSHA256, obs.StatusAfter = fx.Generation, fx.SHA256, fx.Status
		mark := func(status string) error {
			if fx.Status == status {
				return nil
			}
			n, err := q.MarkVariantFixity(ctx, MarkVariantFixityParams{
				FixityStatus: status, ObjectHash: hash, VariantKey: variant, Generation: fx.Generation,
			})
			if err == nil && n == 1 {
				obs.StatusAfter = status // the event reports the row as it is
			}
			return err
		}
		got, n, found, err := s.streamDigest(ctx, hash, variant)
		if err != nil {
			// streamDigest only talks to the backend: unless the context
			// ended, the backend failed to deliver the bytes (#1598).
			if ctx.Err() == nil {
				obs.ReadFailed, obs.ObservedAt = true, timeNow()
			}
			return err
		}
		obs.ObservedAt = timeNow()
		if !found {
			obs.Finding, obs.Detail = FindingMissingObject, "row references bytes that are not on "+s.Backend.Name()
			return mark(FixityMissing)
		}
		obs.Found, obs.ObservedSHA256, obs.ObservedSize = true, got, n
		hasDigest := fx.SHA256 != ""
		switch {
		case variant == VariantOriginal && got != hash:
			obs.Finding, obs.Detail = FindingChecksumMismatch, fmt.Sprintf("stored bytes hash to %s, expected %s", got[:16], hash[:16])
		case hasDigest && got != fx.SHA256:
			obs.Finding, obs.Detail = FindingChecksumMismatch, fmt.Sprintf("stored bytes hash to %s, generation %d recorded %s", got[:16], fx.Generation, fx.SHA256[:16])
		case hasDigest && n != fx.Size:
			obs.Finding, obs.Detail = FindingSizeMismatch, fmt.Sprintf("on disk %d bytes, generation %d recorded %d", n, fx.Generation, fx.Size)
		// A recorded size of 0 is checked too: 0 is a size, not "unknown".
		case !hasDigest && n != row.SizeBytes:
			obs.Finding, obs.Detail = FindingSizeMismatch, fmt.Sprintf("on disk %d bytes, storage_variants says %d", n, row.SizeBytes)
		}
		switch {
		case obs.Finding != "" && hasDigest:
			return mark(FixityMismatch)
		case obs.Finding != "":
			return nil // no digest to contradict: stays unverifiable, reported
		case hasDigest:
			return mark(FixityVerified)
		case fx.Status == FixityMissing:
			// The bytes are back, but nothing proves which: unverifiable.
			return mark(FixityUnverifiable)
		}
		return nil
	})
	return obs, err
}

// Recheck outcomes.
const (
	RecheckVerified     = "verified"      // recorded digest matches, or one was recorded from the bytes
	RecheckMismatch     = "mismatch"      // stored bytes contradict the recorded digest, size or address
	RecheckMissing      = "missing"       // no bytes on the backend
	RecheckGone         = "gone"          // no row
	RecheckSizeMismatch = "size_mismatch" // unverifiable row whose bytes do not match its recorded size
)

// RecheckVariant is the explicit backfill and re-verification of one
// variant (#1597), under its WRITE lock because it may record a digest.
//
//   - A row with a recorded digest is compared with the stored bytes, as
//     VerifyVariant does; its digest is never replaced.
//   - An UNVERIFIABLE row gets the digest of the bytes stored now,
//     marked digest_source 'recheck', only when they are consistent with
//     what the row already says: they must have its recorded size_bytes,
//     and an original's must also hash to its own address. Otherwise it
//     stays unverifiable (or missing) and the outcome says why; the row's
//     size_bytes is never changed by a recheck.
func (s *Service) RecheckVariant(ctx context.Context, hash, variant string) (string, error) {
	var outcome string
	err := s.withVariantLock(ctx, hash, variant, true, func(q *Queries) error {
		row, err := q.GetVariantFixity(ctx, GetVariantFixityParams{ObjectHash: hash, VariantKey: variant})
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = RecheckGone
			return nil
		}
		if err != nil {
			return err
		}
		fx := fixityFromRow(row)
		mark := func(status string) error {
			_, err := q.MarkVariantFixity(ctx, MarkVariantFixityParams{
				FixityStatus: status, ObjectHash: hash, VariantKey: variant, Generation: fx.Generation,
			})
			return err
		}
		got, n, found, err := s.streamDigest(ctx, hash, variant)
		if err != nil {
			return err
		}
		if !found {
			outcome = RecheckMissing
			return mark(FixityMissing)
		}
		if fx.SHA256 != "" {
			if got != fx.SHA256 || n != fx.Size || (variant == VariantOriginal && got != hash) {
				outcome = RecheckMismatch
				return mark(FixityMismatch)
			}
			outcome = RecheckVerified
			return mark(FixityVerified)
		}
		switch {
		case variant == VariantOriginal && got != hash:
			outcome = RecheckMismatch
			return nil
		case n != row.SizeBytes:
			// Originals too: a right hash does not make a wrong recorded
			// size right. The row's size_bytes is never rewritten here; an
			// operator corrects it (or the variant is regenerated) and the
			// next recheck may then record the digest.
			outcome = RecheckSizeMismatch
			return nil
		}
		if fx.Status == FixityMissing {
			// Bytes reappeared on a row with no digest: back to
			// unverifiable first, so the recheck below applies.
			if err := mark(FixityUnverifiable); err != nil {
				return err
			}
		}
		rn, err := q.RecordVariantRecheck(ctx, RecordVariantRecheckParams{
			ContentSha256:    &got,
			ContentSizeBytes: &n,
			ObjectHash:       hash,
			VariantKey:       variant,
			Generation:       fx.Generation,
		})
		if err != nil {
			return err
		}
		if rn == 0 {
			return fmt.Errorf("storage: recheck %s/%s: row changed under the write lock", short(hash), variant)
		}
		outcome = RecheckVerified
		return nil
	})
	return outcome, err
}

// BackfillBatch reports one page of BackfillVariantDigests.
type BackfillBatch struct {
	Scanned     int
	Outcomes    map[string]int
	NextHash    string
	NextVariant string
	Done        bool
}

// BackfillVariantDigests rechecks up to limit rows WITHOUT A DIGEST after
// the (afterHash, afterVariant) cursor, in primary-key order: unverifiable
// rows, and rows marked missing that never had a digest, so a later run
// picks up bytes that have come back. A row with a recorded digest is the
// sweep's business, not the backfill's. Run it again from
// NextHash/NextVariant until Done. `aa backfill-variant-digests` drives
// it; nothing runs it implicitly.
func (s *Service) BackfillVariantDigests(ctx context.Context, afterHash, afterVariant string, limit int32) (BackfillBatch, error) {
	out := BackfillBatch{Outcomes: map[string]int{}}
	rows, err := New(s.Pool).ListVariantsWithoutDigest(ctx, ListVariantsWithoutDigestParams{
		AfterHash: afterHash, AfterVariant: afterVariant, MaxRows: limit,
	})
	if err != nil {
		return out, fmt.Errorf("storage: list variants without a digest: %w", err)
	}
	for _, r := range rows {
		outcome, err := s.RecheckVariant(ctx, r.ObjectHash, r.VariantKey)
		if err != nil {
			return out, err
		}
		out.Scanned++
		out.Outcomes[outcome]++
		out.NextHash, out.NextVariant = r.ObjectHash, r.VariantKey
	}
	out.Done = len(rows) < int(limit)
	return out, nil
}

func short(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

// timeNow is the observation clock, UTC.
func timeNow() time.Time { return time.Now().UTC() }
