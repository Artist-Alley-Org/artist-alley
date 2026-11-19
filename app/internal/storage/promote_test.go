// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1596: PromoteVariant on a real database and the fs backend.
//
//   - a verified variant becomes its own content-addressed object, held
//     by a transient pin until released; released with no other pin, the
//     copy is scheduled for GC, and with one it is not;
//   - an unverified, mismatched or missing row, a missing key, missing
//     bytes and bytes changed since they were recorded all fail closed:
//     no object, no pin, and the variant's row unchanged;
//   - a regeneration waits for a promotion in progress (shared variant
//     lock); the copy keeps the old bytes, and a re-promotion yields a new
//     copy, with the old one untouched;
//   - an existing object with the same digest is only pinned, never
//     rewritten or relabelled; two promotions hold independent pins;
//   - an existing object that cannot show verified bytes is refused and
//     the promotion's pin is released: its row may still say verified,
//     so its stored bytes are read and hashed (same-length corruption,
//     short, long and missing all fail); an import that fails records
//     nothing and leaves no pin;
//   - an importer that records the same new bytes after the promotion's
//     first check is never written over: the promotion's exclusive
//     import lock waits for it, and the re-check under the lock only pins;
//   - a removal racing a promotion of the same bytes never leaves the
//     copy GC-eligible while the promotion holds it.

package storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/storage"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// variant writes a derived variant of parent through PutVariant.
func (r *pinRaceRig) variant(parent, key string, body []byte) {
	r.t.Helper()
	if _, err := r.svc.PutVariant(r.ctx, parent, key, bytes.NewReader(body), "image/webp"); err != nil {
		r.t.Fatalf("PutVariant %s: %v", key, err)
	}
}

// cleanupObject removes a promoted copy's rows when the test ends.
func (r *pinRaceRig) cleanupObject(hash string) {
	r.t.Cleanup(func() {
		c := context.Background()
		_, _ = r.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, hash)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, hash)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_import_intents WHERE object_hash = $1`, hash)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hash)
	})
}

func (r *pinRaceRig) objectRows(hash string) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM storage_objects WHERE hash = $1`, hash).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *pinRaceRig) promotePins() int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM storage_pins WHERE pin_subject_type = $1`, storage.PinSubjectTypeVersionPromote).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *pinRaceRig) stored(hash, key string) []byte {
	r.t.Helper()
	rc, _, err := r.svc.Backend.Get(r.ctx, hash, key)
	if err != nil {
		r.t.Fatalf("get %s/%s: %v", hash[:8], key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func (r *pinRaceRig) fixityOf(hash, key string) storage.VariantFixity {
	r.t.Helper()
	fx, err := r.svc.ReadVariantFixity(r.ctx, hash, key)
	if err != nil {
		r.t.Fatalf("fixity %s/%s: %v", hash[:8], key, err)
	}
	return fx
}

func TestPromoteVariant_VerifiedVariantBecomesItsOwnHeldObject(t *testing.T) {
	r := newPinRaceRig(t)
	parent := r.object()
	body := randomBytes(t, 4096)
	r.variant(parent, "hires", body)
	r.cleanupObject(sha(body))

	p, err := r.svc.PromoteVariant(r.ctx, parent, "hires")
	if err != nil {
		t.Fatalf("PromoteVariant: %v", err)
	}
	if p.Hash != sha(body) || p.Size != int64(len(body)) || p.MediaType != "image/webp" || p.Generation != 1 || p.Deduped {
		t.Fatalf("promotion = %+v, want the variant's own digest, size, type, generation 1, not deduped", p)
	}
	if got := r.stored(p.Hash, storage.VariantOriginal); !bytes.Equal(got, body) {
		t.Fatal("the copy's stored bytes are not the variant's")
	}
	if fx := r.fixityOf(p.Hash, storage.VariantOriginal); fx.Status != storage.FixityVerified || fx.SHA256 != p.Hash || fx.Size != p.Size {
		t.Fatalf("the copy's original row = %+v, want verified at its own digest", fx)
	}
	if fx := r.fixityOf(parent, "hires"); fx.Status != storage.FixityVerified || fx.Generation != 1 {
		t.Fatalf("promotion changed the parent's variant row: %+v", fx)
	}
	if n := r.pins(p.Hash, storage.PinSubjectTypeVersionPromote); n != 1 || r.gcEligibleAt(p.Hash) != nil {
		t.Fatalf("held copy: %d promote pins, gc %v; want 1 and none", n, r.gcEligibleAt(p.Hash))
	}

	// Released after a durable pin holds it: stays ineligible.
	durable := storage.PinRef{SubjectType: "asset_version_test", SubjectID: "durable"}
	if err := r.svc.AddPin(r.ctx, durable, p.Hash); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.ReleasePromotion(r.ctx, p); err != nil {
		t.Fatalf("ReleasePromotion: %v", err)
	}
	if r.pins(p.Hash, storage.PinSubjectTypeVersionPromote) != 0 || r.gcEligibleAt(p.Hash) != nil {
		t.Fatal("after release under a durable pin the copy must keep no promote pin and stay ineligible")
	}
	// A second, unused promotion released with nothing else holding the
	// copy: scheduled for GC, as an unpublished copy should be.
	if err := r.svc.RemovePin(r.ctx, durable, p.Hash); err != nil {
		t.Fatal(err)
	}
	p2, err := r.svc.PromoteVariant(r.ctx, parent, "hires")
	if err != nil || !p2.Deduped || p2.Hash != p.Hash {
		t.Fatalf("re-promotion: %+v %v, want the same object, deduped", p2, err)
	}
	if r.gcEligibleAt(p.Hash) != nil {
		t.Fatal("a held copy is GC-eligible")
	}
	if err := r.svc.ReleasePromotion(r.ctx, p2); err != nil {
		t.Fatal(err)
	}
	if r.gcEligibleAt(p.Hash) == nil {
		t.Fatal("an unused copy released with no other pin was not scheduled for GC")
	}
}

func TestPromoteVariant_FailsClosed(t *testing.T) {
	r := newPinRaceRig(t)
	cases := []struct {
		name  string
		setup func(parent string, body []byte) (key string)
		want  error
	}{
		{"unverifiable row", func(parent string, body []byte) string {
			r.variant(parent, "hires", body)
			if _, err := r.pool.Exec(r.ctx, `UPDATE storage_variants SET fixity_status = 'unverifiable', content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL WHERE object_hash = $1 AND variant_key = 'hires'`, parent); err != nil {
				t.Fatal(err)
			}
			return "hires"
		}, storage.ErrPromoteNotVerified},
		{"mismatch row", func(parent string, body []byte) string {
			r.variant(parent, "hires", body)
			if _, err := r.pool.Exec(r.ctx, `UPDATE storage_variants SET fixity_status = 'mismatch' WHERE object_hash = $1 AND variant_key = 'hires'`, parent); err != nil {
				t.Fatal(err)
			}
			return "hires"
		}, storage.ErrPromoteNotVerified},
		{"no such variant", func(string, []byte) string { return "screen" }, storage.ErrPromoteMissing},
		{"bytes gone from the backend", func(parent string, body []byte) string {
			r.variant(parent, "hires", body)
			if err := r.svc.Backend.Delete(r.ctx, parent, "hires"); err != nil {
				t.Fatal(err)
			}
			return "hires"
		}, storage.ErrPromoteMissing},
		{"bytes changed since recorded", func(parent string, body []byte) string {
			r.variant(parent, "hires", body)
			if _, err := r.svc.Backend.Put(r.ctx, parent, "hires", bytes.NewReader(randomBytes(t, len(body)))); err != nil {
				t.Fatal(err)
			}
			return "hires"
		}, storage.ErrPromoteChanged},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parent := r.object()
			body := randomBytes(t, 2048)
			r.cleanupObject(sha(body))
			key := c.setup(parent, body)
			var before *storage.VariantFixity
			if fx, err := r.svc.ReadVariantFixity(r.ctx, parent, key); err == nil {
				before = &fx
			}
			pins := r.promotePins()
			p, err := r.svc.PromoteVariant(r.ctx, parent, key)
			if !errors.Is(err, c.want) {
				t.Fatalf("PromoteVariant: %+v, %v; want %v", p, err, c.want)
			}
			if r.objectRows(sha(body)) != 0 || r.promotePins() != pins {
				t.Fatal("a refused promotion recorded an object or left a pin")
			}
			if before != nil {
				if after := r.fixityOf(parent, key); after != *before {
					t.Fatalf("a refused promotion changed the variant row: %+v -> %+v", *before, after)
				}
			}
		})
	}
	if _, err := r.svc.PromoteVariant(r.ctx, r.object(), storage.VariantOriginal); err == nil {
		t.Fatal("an original was promoted")
	}
}

func TestPromoteVariant_RegenerationWaitsAndRepromotionLeavesTheOldCopy(t *testing.T) {
	r := newPinRaceRig(t)
	parent := r.object()
	oldBody, newBody := randomBytes(t, 3000), randomBytes(t, 3100)
	r.cleanupObject(sha(oldBody))
	r.cleanupObject(sha(newBody))
	r.variant(parent, "hires", oldBody)

	holding, release := make(chan struct{}), make(chan struct{})
	// Released exactly once, also when the test stops early, so a held
	// goroutine never outlives a failure.
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	r.svc.SetPromoteAfterReadHook(func(string, string) { close(holding); <-release })
	promoted := make(chan *storage.Promotion, 1)
	perr := make(chan error, 1)
	go func() {
		p, err := r.svc.PromoteVariant(context.Background(), parent, "hires")
		promoted <- p
		perr <- err
	}()
	<-holding
	r.svc.SetPromoteAfterReadHook(nil)
	rewritten := make(chan error, 1)
	go func() {
		_, err := r.svc.PutVariant(context.Background(), parent, "hires", bytes.NewReader(newBody), "image/webp")
		rewritten <- err
	}()
	r.waitBlocked("the regeneration", rewritten)
	unblock()
	if err := <-perr; err != nil {
		t.Fatalf("PromoteVariant: %v", err)
	}
	first := <-promoted
	if err := <-rewritten; err != nil {
		t.Fatalf("PutVariant: %v", err)
	}
	if first.Hash != sha(oldBody) || first.Generation != 1 {
		t.Fatalf("the promotion that started first copied %s gen %d, want the old bytes at gen 1", first.Hash[:8], first.Generation)
	}
	second, err := r.svc.PromoteVariant(r.ctx, parent, "hires")
	if err != nil {
		t.Fatalf("re-promotion: %v", err)
	}
	if second.Hash != sha(newBody) || second.Generation != 2 || second.Hash == first.Hash {
		t.Fatalf("re-promotion = %s gen %d, want the regenerated bytes at gen 2", second.Hash[:8], second.Generation)
	}
	if got := r.stored(first.Hash, storage.VariantOriginal); !bytes.Equal(got, oldBody) {
		t.Fatal("the regeneration altered the first copy's bytes")
	}
	if r.pins(first.Hash, storage.PinSubjectTypeVersionPromote) != 1 || r.gcEligibleAt(first.Hash) != nil {
		t.Fatal("the first copy lost its hold")
	}
}

func TestPromoteVariant_ExistingDigestIsPinnedNeverRewritten(t *testing.T) {
	r := newPinRaceRig(t)
	body := randomBytes(t, 2500)
	// The same bytes already uploaded as an original, as image/png.
	up, err := r.svc.UploadOriginal(r.ctx, bytes.NewReader(body), "image/png", storage.PinRef{SubjectType: "user", SubjectID: "promote-dedup"})
	if err != nil {
		t.Fatal(err)
	}
	r.cleanupObject(up.Hash)
	beforeRow := r.fixityOf(up.Hash, storage.VariantOriginal)
	beforeInfo, err := r.svc.Backend.Stat(r.ctx, up.Hash, storage.VariantOriginal)
	if err != nil {
		t.Fatal(err)
	}
	var beforeType string
	if err := r.pool.QueryRow(r.ctx, `SELECT content_type FROM storage_variants WHERE object_hash = $1 AND variant_key = 'original'`, up.Hash).Scan(&beforeType); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // a rewrite would move the file's mtime past this point

	parent := r.object()
	r.variant(parent, "hires", body)
	a, err := r.svc.PromoteVariant(r.ctx, parent, "hires")
	if err != nil {
		t.Fatalf("PromoteVariant: %v", err)
	}
	b, err := r.svc.PromoteVariant(r.ctx, parent, "hires")
	if err != nil {
		t.Fatalf("second PromoteVariant: %v", err)
	}
	if a.Hash != up.Hash || b.Hash != up.Hash || !a.Deduped || !b.Deduped || a.Pin == b.Pin {
		t.Fatalf("promotions %+v / %+v, want the existing object, deduped, with independent pins", a, b)
	}
	var afterType string
	if err := r.pool.QueryRow(r.ctx, `SELECT content_type FROM storage_variants WHERE object_hash = $1 AND variant_key = 'original'`, up.Hash).Scan(&afterType); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := r.svc.Backend.Stat(r.ctx, up.Hash, storage.VariantOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if after := r.fixityOf(up.Hash, storage.VariantOriginal); after != beforeRow || afterType != beforeType || !afterInfo.ModifiedAt.Equal(beforeInfo.ModifiedAt) {
		t.Fatalf("the existing original was rewritten or relabelled: row %+v -> %+v, type %s -> %s, mtime %v -> %v",
			beforeRow, after, beforeType, afterType, beforeInfo.ModifiedAt, afterInfo.ModifiedAt)
	}
	if err := r.svc.ReleasePromotion(r.ctx, a); err != nil {
		t.Fatal(err)
	}
	if r.pins(up.Hash, storage.PinSubjectTypeVersionPromote) != 1 {
		t.Fatal("releasing one promotion released the other's pin")
	}
}

func TestPromoteVariant_FailureAfterPinningReleasesIt(t *testing.T) {
	r := newPinRaceRig(t)
	body := randomBytes(t, 1800)
	// An existing object of the same digest whose original row cannot
	// show verified bytes (as an original recorded before #1597 would).
	up, err := r.svc.UploadOriginal(r.ctx, bytes.NewReader(body), "image/png", racePin)
	if err != nil {
		t.Fatal(err)
	}
	r.cleanupObject(up.Hash)
	if _, err := r.pool.Exec(r.ctx, `UPDATE storage_variants SET fixity_status = 'unverifiable', content_sha256 = NULL, content_size_bytes = NULL, digest_source = NULL WHERE object_hash = $1 AND variant_key = 'original'`, up.Hash); err != nil {
		t.Fatal(err)
	}
	parent := r.object()
	r.variant(parent, "hires", body)
	if _, err := r.svc.PromoteVariant(r.ctx, parent, "hires"); !errors.Is(err, storage.ErrPromoteExistingUnverified) {
		t.Fatalf("PromoteVariant onto an unverified object: %v, want ErrPromoteExistingUnverified", err)
	}
	if r.pins(up.Hash, storage.PinSubjectTypeVersionPromote) != 0 {
		t.Fatal("the refused promotion left its pin")
	}
	if r.pins(up.Hash, racePin.SubjectType) != 1 || r.gcEligibleAt(up.Hash) != nil {
		t.Fatal("the refused promotion disturbed the existing object's own pins or GC state")
	}

	// An import that fails before its commit records nothing.
	body2 := randomBytes(t, 1900)
	r.cleanupObject(sha(body2))
	r.variant(parent, "screen", body2)
	injected := errors.New("injected before commit")
	r.svc.SetPromoteBeforeCommitHook(func(context.Context) error { return injected })
	_, err = r.svc.PromoteVariant(r.ctx, parent, "screen")
	r.svc.SetPromoteBeforeCommitHook(nil)
	if !errors.Is(err, injected) {
		t.Fatalf("PromoteVariant with a failing import: %v, want the injected failure", err)
	}
	if r.objectRows(sha(body2)) != 0 || r.pins(sha(body2), storage.PinSubjectTypeVersionPromote) != 0 {
		t.Fatal("a failed import recorded the copy or left a pin")
	}
}

func TestPromoteVariant_HoldsTheCopyAgainstARacingRemoval(t *testing.T) {
	r := newPinRaceRig(t)
	body := randomBytes(t, 2200)
	// The copy's digest exists with one ordinary pin, being removed.
	up, err := r.svc.UploadOriginal(r.ctx, bytes.NewReader(body), "image/webp", racePin)
	if err != nil {
		t.Fatal(err)
	}
	r.cleanupObject(up.Hash)
	parent := r.object()
	r.variant(parent, "hires", body)

	marked, release := make(chan struct{}), make(chan struct{})
	// Released exactly once, also when the test stops early, so a held
	// goroutine never outlives a failure.
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	r.svc.SetPinBeforeRemoveCommitHook(func(h string) error {
		if h == up.Hash {
			close(marked)
			<-release
		}
		return nil
	})
	// No deferred reset: this test's Service is its own, and a reset
	// would race a goroutine still running after an early failure.
	removed := make(chan error, 1)
	go func() { removed <- r.svc.RemovePin(context.Background(), racePin, up.Hash) }()
	<-marked
	r.svc.SetPinBeforeRemoveCommitHook(nil)
	promoted := make(chan *storage.Promotion, 1)
	perr := make(chan error, 1)
	go func() {
		p, err := r.svc.PromoteVariant(context.Background(), parent, "hires")
		promoted <- p
		perr <- err
	}()
	r.waitBlocked("the promotion's pin", perr)
	unblock()
	if err := <-removed; err != nil {
		t.Fatalf("RemovePin: %v", err)
	}
	if err := <-perr; err != nil {
		t.Fatalf("PromoteVariant: %v", err)
	}
	p := <-promoted
	if r.pins(up.Hash, storage.PinSubjectTypeVersionPromote) != 1 || r.gcEligibleAt(up.Hash) != nil {
		t.Fatalf("after the race the held copy has %d promote pins and gc %v, want 1 and none",
			r.pins(up.Hash, storage.PinSubjectTypeVersionPromote), r.gcEligibleAt(up.Hash))
	}
	if err := r.svc.ReleasePromotion(r.ctx, p); err != nil {
		t.Fatal(err)
	}
	if r.gcEligibleAt(up.Hash) == nil {
		t.Fatal("released with no pin left, the unused copy was not scheduled")
	}
}

func TestImportVerified_KeepExistingOriginalNeverRewrites(t *testing.T) {
	r := newPinRaceRig(t)
	body := randomBytes(t, 1500)
	up, err := r.svc.UploadOriginal(r.ctx, bytes.NewReader(body), "image/png", racePin)
	if err != nil {
		t.Fatal(err)
	}
	r.cleanupObject(up.Hash)
	staged := t.TempDir() + "/staged"
	if err := os.WriteFile(staged, body, 0o600); err != nil {
		t.Fatal(err)
	}
	pin := storage.PinRef{SubjectType: storage.PinSubjectTypeVersionPromote, SubjectID: "keep-existing"}
	req := storage.ImportRequest{TempPath: staged, Hash: up.Hash, Size: int64(len(body)), ContentType: "image/webp",
		IntentID: uuid.New(), Pin: pin, KeepExistingOriginal: true}
	if _, err := r.svc.ImportVerified(r.ctx, req); err != nil {
		t.Fatalf("ImportVerified (existing, keep): %v", err)
	}
	var ct string
	if err := r.pool.QueryRow(r.ctx, `SELECT content_type FROM storage_variants WHERE object_hash = $1 AND variant_key = 'original'`, up.Hash).Scan(&ct); err != nil || ct != "image/png" {
		t.Fatalf("the existing original was relabelled to %q (%v)", ct, err)
	}
	// Existing row, bytes gone: refused, not written again.
	if err := r.svc.Backend.Delete(r.ctx, up.Hash, storage.VariantOriginal); err != nil {
		t.Fatal(err)
	}
	req.IntentID = uuid.New()
	if _, err := r.svc.ImportVerified(r.ctx, req); !errors.Is(err, storage.ErrImportExistingNotRewritten) {
		t.Fatalf("ImportVerified onto a recorded hash without bytes: %v, want ErrImportExistingNotRewritten", err)
	}
	if _, _, err := r.svc.Backend.Get(r.ctx, up.Hash, storage.VariantOriginal); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the refused import wrote the bytes: %v", err)
	}
}

func TestPromoteVariant_ExistingOriginalIsHashedNotTrusted(t *testing.T) {
	// The digest object already exists and its row still says verified,
	// but its stored bytes are not what the hash names. The dedup must
	// read the bytes, refuse, rewrite nothing, and release only its pin.
	r := newPinRaceRig(t)
	cases := []struct {
		name    string
		corrupt func(hash string, body []byte)
	}{
		{"same length, different bytes", func(h string, body []byte) { r.putRaw(h, randomBytes(t, len(body))) }},
		{"short", func(h string, body []byte) { r.putRaw(h, body[:len(body)-1]) }},
		{"long", func(h string, body []byte) { r.putRaw(h, append(append([]byte{}, body...), 0)) }},
		{"missing", func(h string, _ []byte) {
			if err := r.svc.Backend.Delete(r.ctx, h, storage.VariantOriginal); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := randomBytes(t, 2048)
			up, err := r.svc.UploadOriginal(r.ctx, bytes.NewReader(body), "image/png", racePin)
			if err != nil {
				t.Fatal(err)
			}
			r.cleanupObject(up.Hash)
			other := storage.PinRef{SubjectType: "asset", SubjectID: c.name}
			if err := r.svc.AddPin(r.ctx, other, up.Hash); err != nil {
				t.Fatal(err)
			}
			c.corrupt(up.Hash, body)
			rowBefore := r.fixityOf(up.Hash, storage.VariantOriginal)
			bytesBefore, hadBytes := r.storedIfAny(up.Hash)
			parent := r.object()
			r.variant(parent, "hires", body)

			p, err := r.svc.PromoteVariant(r.ctx, parent, "hires")
			if !errors.Is(err, storage.ErrPromoteExistingUnverified) {
				t.Fatalf("PromoteVariant onto corrupted existing bytes: %+v, %v; want ErrPromoteExistingUnverified", p, err)
			}
			if after, has := r.storedIfAny(up.Hash); has != hadBytes || !bytes.Equal(after, bytesBefore) {
				t.Fatal("the refused promotion rewrote the existing object's bytes")
			}
			if r.fixityOf(up.Hash, storage.VariantOriginal) != rowBefore {
				t.Fatal("the refused promotion changed the existing original's row")
			}
			if r.pins(up.Hash, storage.PinSubjectTypeVersionPromote) != 0 {
				t.Fatal("the refused promotion leaked its pin")
			}
			if r.pins(up.Hash, racePin.SubjectType) != 1 || r.pins(up.Hash, "asset") != 1 || r.gcEligibleAt(up.Hash) != nil {
				t.Fatal("the refused promotion disturbed the existing object's other pins or GC state")
			}
		})
	}
}

func TestPromoteVariant_ImporterRecordingAfterTheFirstCheckIsNotOverwritten(t *testing.T) {
	// A peer import of the same new bytes holds the import lock (shared)
	// when the promotion arrives: the promotion's first check finds no
	// object, its exclusive import lock waits, the peer import records the
	// object, and the promotion, re-checking under its lock, only pins it.
	r := newPinRaceRig(t)
	body := randomBytes(t, 2600)
	h := sha(body)
	r.cleanupObject(h)
	parent := r.object()
	r.variant(parent, "hires", body)
	staged := t.TempDir() + "/peer"
	if err := os.WriteFile(staged, body, 0o600); err != nil {
		t.Fatal(err)
	}

	held, release := make(chan struct{}), make(chan struct{})
	// Released exactly once, also when the test stops early, so a held
	// goroutine never outlives a failure.
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var first sync.Once
	r.svc.SetImportAfterLockHook(func() {
		first.Do(func() { close(held); <-release })
	})
	// No deferred reset: this test's Service is its own, and a reset
	// would race a goroutine still running after an early failure.
	peer := storage.PinRef{SubjectType: "federation_ingest", SubjectID: "peer-race"}
	imported := make(chan error, 1)
	go func() {
		_, err := r.svc.ImportVerified(context.Background(), storage.ImportRequest{
			TempPath: staged, Hash: h, Size: int64(len(body)), ContentType: "image/avif", IntentID: uuid.New(), Pin: peer,
		})
		imported <- err
	}()
	<-held
	promoted := make(chan *storage.Promotion, 1)
	perr := make(chan error, 1)
	go func() {
		p, err := r.svc.PromoteVariant(context.Background(), parent, "hires")
		promoted <- p
		perr <- err
	}()
	r.waitBlocked("the promotion's exclusive import lock", perr)
	unblock()
	if err := <-imported; err != nil {
		t.Fatalf("peer import: %v", err)
	}
	afterPeer, err := r.svc.Backend.Stat(r.ctx, h, storage.VariantOriginal)
	if err != nil {
		t.Fatal(err)
	}
	rowAfterPeer := r.fixityOf(h, storage.VariantOriginal)
	if err := <-perr; err != nil {
		t.Fatalf("PromoteVariant: %v", err)
	}
	p := <-promoted
	if !p.Deduped || p.Hash != h {
		t.Fatalf("promotion = %+v, want the peer's object, deduped", p)
	}
	final, err := r.svc.Backend.Stat(r.ctx, h, storage.VariantOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if !final.ModifiedAt.Equal(afterPeer.ModifiedAt) || r.fixityOf(h, storage.VariantOriginal) != rowAfterPeer {
		t.Fatalf("the promotion wrote over the peer's recorded original: mtime %v -> %v", afterPeer.ModifiedAt, final.ModifiedAt)
	}
	var ct string
	if err := r.pool.QueryRow(r.ctx, `SELECT content_type FROM storage_variants WHERE object_hash = $1 AND variant_key = 'original'`, h).Scan(&ct); err != nil || ct != "image/avif" {
		t.Fatalf("the peer's original was relabelled to %q (%v)", ct, err)
	}
	if r.pins(h, "federation_ingest") != 1 || r.pins(h, storage.PinSubjectTypeVersionPromote) != 1 || r.gcEligibleAt(h) != nil {
		t.Fatal("both the peer's and the promotion's pins must hold the object")
	}
}

// putRaw plants bytes as an object's original, bypassing every record.
func (r *pinRaceRig) putRaw(hash string, b []byte) {
	r.t.Helper()
	if _, err := r.svc.Backend.Put(r.ctx, hash, storage.VariantOriginal, bytes.NewReader(b)); err != nil {
		r.t.Fatal(err)
	}
}

// storedIfAny reads an object's original, reporting whether it exists.
func (r *pinRaceRig) storedIfAny(hash string) ([]byte, bool) {
	r.t.Helper()
	rc, _, err := r.svc.Backend.Get(r.ctx, hash, storage.VariantOriginal)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, false
	}
	if err != nil {
		r.t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		r.t.Fatal(err)
	}
	return b, true
}
