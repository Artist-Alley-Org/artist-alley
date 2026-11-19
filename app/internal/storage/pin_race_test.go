// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1596: a storage object that is pinned is never left GC-eligible, even
// when a pin removal races a new pin of the same shared object. Pinned on
// a real database:
//
//   - every pin writer locks the object row before it changes a pin row
//     (AddPin, casfetch, migration 00084's version pins), and RemovePin
//     decides "last pin?" in a separate statement after taking that lock,
//     so its snapshot is fresh;
//   - both orderings end with the object pinned and gc_eligible_at NULL:
//     the new pin commits while the removal waits, and the removal
//     commits while the new pin waits; the new pin is an asset version's
//     (an asset inserted with the bytes) or a plain AddPin;
//   - AddPin's clear and pin, and RemovePin's unpin and mark, each commit
//     or roll back together.
//
// The waits are observed in Postgres (pg_blocking_pids), not assumed
// from a sleep.

package storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/storage"
)

type pinRaceRig struct {
	*fixityRig
	t   *testing.T
	ctx context.Context
}

func newPinRaceRig(t *testing.T) *pinRaceRig {
	return &pinRaceRig{fixityRig: newFixityRig(t), t: t, ctx: t.Context()}
}

var racePin = storage.PinRef{SubjectType: storage.PinSubjectTypeUser, SubjectID: "pin-race"}

// object uploads fresh bytes whose only pin is racePin.
func (r *pinRaceRig) object() string {
	r.t.Helper()
	body := make([]byte, 64)
	if _, err := rand.Read(body); err != nil {
		r.t.Fatal(err)
	}
	res, err := r.svc.UploadOriginal(r.ctx, bytes.NewReader(body), "image/png", racePin)
	if err != nil {
		r.t.Fatalf("upload: %v", err)
	}
	r.t.Cleanup(func() {
		c := context.Background()
		_, _ = r.pool.Exec(c, `DELETE FROM assets WHERE file_hash = $1`, res.Hash)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, res.Hash)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_variants WHERE object_hash = $1`, res.Hash)
		_, _ = r.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, res.Hash)
	})
	return res.Hash
}

func (r *pinRaceRig) gcEligibleAt(hash string) *time.Time {
	r.t.Helper()
	var at *time.Time
	if err := r.pool.QueryRow(r.ctx, `SELECT gc_eligible_at FROM storage_objects WHERE hash = $1`, hash).Scan(&at); err != nil {
		r.t.Fatal(err)
	}
	return at
}

func (r *pinRaceRig) pins(hash, subjectType string) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = $2`, hash, subjectType).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// waitBlocked returns once some backend of this database waits on a lock,
// failing if done reports first or nothing waits within 10s.
func (r *pinRaceRig) waitBlocked(what string, done <-chan error) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND cardinality(pg_blocking_pids(pid)) > 0`).Scan(&n); err != nil {
			r.t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		select {
		case err := <-done:
			r.t.Fatalf("%s finished before it waited on the object's lock: %v", what, err)
		default:
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%s never waited on the object's lock", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// insertAsset is the version pinner: a local asset with these bytes gets
// its v1 and the version's pin (migration 00084) in the same transaction.
const insertAsset = `INSERT INTO assets (id, title, asset_type, file_hash, file_extension)
	VALUES ($1, 'pin race', (SELECT MIN(ref) FROM asset_types), $2, 'png')`

// requirePinnedNotEligible is the invariant at the final commit.
func (r *pinRaceRig) requirePinnedNotEligible(who, hash, pinType string) {
	r.t.Helper()
	if n := r.pins(hash, pinType); n != 1 {
		r.t.Errorf("%s: %d %s pins, want 1", who, n, pinType)
	}
	if n := r.pins(hash, racePin.SubjectType); n != 0 {
		r.t.Errorf("%s: the removed pin is still there", who)
	}
	if at := r.gcEligibleAt(hash); at != nil {
		r.t.Errorf("%s: pinned bytes are GC-eligible at %v", who, at)
	}
}

func TestPinRace_NewPinCommitsWhileRemovalWaits(t *testing.T) {
	r := newPinRaceRig(t)

	t.Run("asset version pin", func(t *testing.T) {
		h := r.object()
		pinner, err := r.pool.Begin(r.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pinner.Rollback(context.Background()) }()
		if _, err := pinner.Exec(r.ctx, insertAsset, uuid.New(), h); err != nil {
			t.Fatalf("insert asset: %v", err)
		}
		removed := make(chan error, 1)
		go func() { removed <- r.svc.RemovePin(context.Background(), racePin, h) }()
		r.waitBlocked("RemovePin", removed)
		if err := pinner.Commit(r.ctx); err != nil {
			t.Fatalf("pinner commit: %v", err)
		}
		if err := <-removed; err != nil {
			t.Fatalf("RemovePin: %v", err)
		}
		r.requirePinnedNotEligible("version pin committed first", h, "asset_version")
	})

	t.Run("AddPin", func(t *testing.T) {
		h := r.object()
		holding, release := make(chan struct{}), make(chan struct{})
		r.svc.SetPinAfterAddHook(func(string) error { close(holding); <-release; return nil })
		defer r.svc.SetPinAfterAddHook(nil)
		added := make(chan error, 1)
		go func() {
			added <- r.svc.AddPin(context.Background(), storage.PinRef{SubjectType: "asset", SubjectID: uuid.NewString()}, h)
		}()
		<-holding
		r.svc.SetPinAfterAddHook(nil)
		removed := make(chan error, 1)
		go func() { removed <- r.svc.RemovePin(context.Background(), racePin, h) }()
		r.waitBlocked("RemovePin", removed)
		close(release)
		if err := <-added; err != nil {
			t.Fatalf("AddPin: %v", err)
		}
		if err := <-removed; err != nil {
			t.Fatalf("RemovePin: %v", err)
		}
		r.requirePinnedNotEligible("AddPin committed first", h, "asset")
	})
}

func TestPinRace_RemovalCommitsWhileNewPinWaits(t *testing.T) {
	r := newPinRaceRig(t)
	for _, c := range []struct {
		name, pinType string
		pin           func(hash string) error
	}{
		{"asset version pin", "asset_version", func(h string) error {
			_, err := r.pool.Exec(context.Background(), insertAsset, uuid.New(), h)
			return err
		}},
		{"AddPin", "asset", func(h string) error {
			return r.svc.AddPin(context.Background(), storage.PinRef{SubjectType: "asset", SubjectID: uuid.NewString()}, h)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := r.object()
			// The removal stops with its GC mark written, uncommitted.
			marked, release := make(chan struct{}), make(chan struct{})
			r.svc.SetPinBeforeRemoveCommitHook(func(string) error { close(marked); <-release; return nil })
			defer r.svc.SetPinBeforeRemoveCommitHook(nil)
			removed := make(chan error, 1)
			go func() { removed <- r.svc.RemovePin(context.Background(), racePin, h) }()
			<-marked
			pinned := make(chan error, 1)
			go func() { pinned <- c.pin(h) }()
			r.waitBlocked(c.name, pinned)
			close(release)
			if err := <-removed; err != nil {
				t.Fatalf("RemovePin: %v", err)
			}
			if err := <-pinned; err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			r.requirePinnedNotEligible("removal committed first", h, c.pinType)
		})
	}
}

func TestPinWrites_CommitOrRollBackTogether(t *testing.T) {
	r := newPinRaceRig(t)
	injected := errors.New("injected failure")

	t.Run("AddPin", func(t *testing.T) {
		h := r.object()
		if err := r.svc.RemovePin(r.ctx, racePin, h); err != nil {
			t.Fatal(err)
		}
		before := r.gcEligibleAt(h)
		if before == nil {
			t.Fatal("an object with no pin was not scheduled")
		}
		r.svc.SetPinAfterAddHook(func(string) error { return injected })
		err := r.svc.AddPin(r.ctx, storage.PinRef{SubjectType: "asset", SubjectID: uuid.NewString()}, h)
		r.svc.SetPinAfterAddHook(nil)
		if !errors.Is(err, injected) {
			t.Fatalf("AddPin: %v, want the injected failure", err)
		}
		if n := r.pins(h, "asset"); n != 0 {
			t.Errorf("a failed AddPin left %d pins", n)
		}
		if at := r.gcEligibleAt(h); at == nil || !at.Equal(*before) {
			t.Errorf("a failed AddPin changed gc_eligible_at from %v to %v", before, at)
		}
	})

	t.Run("RemovePin", func(t *testing.T) {
		h := r.object()
		r.svc.SetPinBeforeRemoveCommitHook(func(string) error { return injected })
		err := r.svc.RemovePin(r.ctx, racePin, h)
		r.svc.SetPinBeforeRemoveCommitHook(nil)
		if !errors.Is(err, injected) {
			t.Fatalf("RemovePin: %v, want the injected failure", err)
		}
		if n := r.pins(h, racePin.SubjectType); n != 1 {
			t.Errorf("a failed RemovePin left %d of its pin, want 1", n)
		}
		if at := r.gcEligibleAt(h); at != nil {
			t.Errorf("a failed RemovePin scheduled GC at %v", at)
		}
	})

	t.Run("RemovePin of a live version pin is refused at commit", func(t *testing.T) {
		h := r.object()
		if _, err := r.pool.Exec(r.ctx, insertAsset, uuid.New(), h); err != nil {
			t.Fatal(err)
		}
		if err := r.svc.RemovePin(r.ctx, racePin, h); err != nil {
			t.Fatal(err)
		}
		var version string
		if err := r.pool.QueryRow(r.ctx, `SELECT pin_subject_id FROM storage_pins WHERE object_hash = $1 AND pin_subject_type = 'asset_version'`, h).Scan(&version); err != nil {
			t.Fatal(err)
		}
		err := r.svc.RemovePin(r.ctx, storage.PinRef{SubjectType: "asset_version", SubjectID: version}, h)
		if err == nil || !strings.Contains(err.Error(), "storage: remove pin: commit:") || !strings.Contains(err.Error(), "#1596") {
			t.Fatalf("RemovePin of a live version pin: %v, want the #1596 commit refusal", err)
		}
		if n := r.pins(h, "asset_version"); n != 1 {
			t.Errorf("the refused removal left %d version pins, want 1", n)
		}
		if at := r.gcEligibleAt(h); at != nil {
			t.Errorf("the refused removal scheduled GC at %v", at)
		}
	})

	t.Run("idempotent add, no-op removal", func(t *testing.T) {
		h := r.object()
		asset := storage.PinRef{SubjectType: "asset", SubjectID: uuid.NewString()}
		for range 2 {
			if err := r.svc.AddPin(r.ctx, asset, h); err != nil {
				t.Fatal(err)
			}
		}
		if n := r.pins(h, "asset"); n != 1 {
			t.Errorf("AddPin twice: %d pins, want 1", n)
		}
		if err := r.svc.RemovePin(r.ctx, storage.PinRef{SubjectType: "asset", SubjectID: "absent"}, h); err != nil {
			t.Fatalf("removing an absent pin: %v", err)
		}
		if r.gcEligibleAt(h) != nil {
			t.Error("removing an absent pin scheduled a pinned object")
		}
		for _, p := range []storage.PinRef{asset, racePin} {
			if err := r.svc.RemovePin(r.ctx, p, h); err != nil {
				t.Fatal(err)
			}
		}
		if r.gcEligibleAt(h) == nil {
			t.Error("removing the last pin did not schedule GC")
		}
	})
}
