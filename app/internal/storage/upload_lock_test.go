// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1596: an ordinary upload and a promotion of the same new bytes never
// both write them. UploadOriginal takes the per-hash import lock SHARED
// around its existence check, Put, rows and pin; a promotion's import
// takes it EXCLUSIVELY and re-checks under it. Pinned on a real database
// and the fs backend, with every backend Put counted:
//
//   - upload first: the promotion waits, then finds the upload's row and
//     only pins it (one Put in all);
//   - promotion first: the upload waits, then finds the promotion's row
//     and deduplicates as an upload of an existing hash always has (one
//     Put in all);
//   - both end with both pins and no GC mark;
//   - at a 3-connection pool, at most two uploads hold a connection at
//     once (the put slots they share with variant writes), so a request
//     still gets one; and concurrent uploads, promotions and pin changes
//     all finish, since nothing holds one connection while waiting for
//     another.

package storage_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// countingBackend counts Puts per (hash, variant).
type countingBackend struct {
	storage.Backend
	mu   sync.Mutex
	puts map[string]int
}

func countPuts(svc *storage.Service) *countingBackend {
	cb := &countingBackend{Backend: svc.Backend, puts: map[string]int{}}
	svc.Backend = cb
	return cb
}

func (c *countingBackend) Put(ctx context.Context, hash, variant string, r io.Reader) (*storage.ObjectInfo, error) {
	c.mu.Lock()
	c.puts[hash+"/"+variant]++
	c.mu.Unlock()
	return c.Backend.Put(ctx, hash, variant, r)
}

func (c *countingBackend) originalPuts(hash string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts[hash+"/"+storage.VariantOriginal]
}

var uploadPin = storage.PinRef{SubjectType: storage.PinSubjectTypeUser, SubjectID: "upload-race"}

func TestUploadOriginal_UploadFirst_PromotionOnlyPins(t *testing.T) {
	r := newPinRaceRig(t)
	cb := countPuts(r.svc)
	body := randomBytes(t, 3300)
	h := sha(body)
	r.cleanupObject(h)
	parent := r.object()
	r.variant(parent, "hires", body)

	checked, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	r.svc.SetUploadAfterCheckHook(func(hash string) {
		if hash == h {
			close(checked)
			<-release
		}
	})
	uploaded := make(chan *storage.UploadResult, 1)
	uerr := make(chan error, 1)
	go func() {
		res, err := r.svc.UploadOriginal(context.Background(), bytes.NewReader(body), "image/png", uploadPin)
		uploaded <- res
		uerr <- err
	}()
	<-checked // the upload found no row and holds the lock
	promoted := make(chan *storage.Promotion, 1)
	perr := make(chan error, 1)
	go func() {
		p, err := r.svc.PromoteVariant(context.Background(), parent, "hires")
		promoted <- p
		perr <- err
	}()
	r.waitBlocked("the promotion's exclusive import lock", perr)
	unblock()
	if err := <-uerr; err != nil {
		t.Fatalf("upload: %v", err)
	}
	up := <-uploaded
	afterUpload, err := r.svc.Backend.Stat(r.ctx, h, storage.VariantOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-perr; err != nil {
		t.Fatalf("PromoteVariant: %v", err)
	}
	p := <-promoted
	final, err := r.svc.Backend.Stat(r.ctx, h, storage.VariantOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if up.Deduped || !p.Deduped || p.Hash != h {
		t.Fatalf("upload deduped=%v, promotion %+v; want the upload to write and the promotion to only pin", up.Deduped, p)
	}
	if n := cb.originalPuts(h); n != 1 || !final.ModifiedAt.Equal(afterUpload.ModifiedAt) {
		t.Fatalf("%d Puts of the original (mtime %v -> %v), want exactly the upload's one", n, afterUpload.ModifiedAt, final.ModifiedAt)
	}
	if r.pins(h, uploadPin.SubjectType) != 1 || r.pins(h, storage.PinSubjectTypeVersionPromote) != 1 || r.gcEligibleAt(h) != nil {
		t.Fatal("both the upload's and the promotion's pins must hold the object")
	}
}

func TestUploadOriginal_PromotionFirst_UploadDeduplicates(t *testing.T) {
	r := newPinRaceRig(t)
	cb := countPuts(r.svc)
	body := randomBytes(t, 3400)
	h := sha(body)
	r.cleanupObject(h)
	parent := r.object()
	r.variant(parent, "hires", body)

	held, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var first sync.Once
	r.svc.SetImportAfterLockHook(func() { first.Do(func() { close(held); <-release }) })
	promoted := make(chan *storage.Promotion, 1)
	perr := make(chan error, 1)
	go func() {
		p, err := r.svc.PromoteVariant(context.Background(), parent, "hires")
		promoted <- p
		perr <- err
	}()
	<-held // the promotion holds the exclusive lock, nothing written yet
	uploaded := make(chan *storage.UploadResult, 1)
	uerr := make(chan error, 1)
	go func() {
		res, err := r.svc.UploadOriginal(context.Background(), bytes.NewReader(body), "image/png", uploadPin)
		uploaded <- res
		uerr <- err
	}()
	r.waitBlocked("the upload's import lock", uerr)
	unblock()
	if err := <-perr; err != nil {
		t.Fatalf("PromoteVariant: %v", err)
	}
	p := <-promoted
	if err := <-uerr; err != nil {
		t.Fatalf("upload: %v", err)
	}
	up := <-uploaded
	if p.Deduped || !up.Deduped {
		t.Fatalf("promotion deduped=%v, upload deduped=%v; want the promotion to write and the upload to deduplicate", p.Deduped, up.Deduped)
	}
	if n := cb.originalPuts(h); n != 1 {
		t.Fatalf("%d Puts of the original, want exactly the promotion's one", n)
	}
	if got := r.stored(h, storage.VariantOriginal); !bytes.Equal(got, body) {
		t.Fatal("the stored original is not the bytes")
	}
	if r.pins(h, uploadPin.SubjectType) != 1 || r.pins(h, storage.PinSubjectTypeVersionPromote) != 1 || r.gcEligibleAt(h) != nil {
		t.Fatal("both the upload's and the promotion's pins must hold the object")
	}
}

// smallPoolService is a Service on the rig's database and backend through
// a 3-connection pool, below the smallest supported deployment (8 minus 3
// LISTEN workers = 5 query connections).
func smallPoolService(t *testing.T, r *pinRaceRig) (*storage.Service, *pgxpool.Pool) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	cfg, err := pgxpool.ParseConfig("host=" + envOr("AA_DB_HOST", "postgres") + " port=" + envOr("AA_DB_PORT", "5432") +
		" user=" + envOr("AA_DB_USER", "artist_alley") + " dbname=" + testdb.Name(t) + " sslmode=disable password=" + pwd)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 3
	small, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(small.Close)
	svc := storage.NewService(r.svc.Backend, small)
	svc.TempDir = t.TempDir()
	return svc, small
}

func TestUploadOriginal_PutSlotsKeepAConnectionFree(t *testing.T) {
	// An upload holds its connection from its existence check to its pin.
	// Three uploads held there on a 3-connection pool: only two may be in
	// (the shared put slots), so a request still gets a connection.
	r := newPinRaceRig(t)
	svc, small := smallPoolService(t, r)
	var mu sync.Mutex
	inside := 0
	arrived := make(chan struct{}, 3)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	svc.SetUploadAfterCheckHook(func(string) {
		mu.Lock()
		inside++
		mu.Unlock()
		arrived <- struct{}{}
		<-release
	})
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		body := randomBytes(t, 1000+i)
		r.cleanupObject(sha(body))
		go func() {
			_, err := svc.UploadOriginal(context.Background(), bytes.NewReader(body), "image/png", uploadPin)
			errs <- err
		}()
	}
	<-arrived
	<-arrived
	qctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	var one int
	if err := small.QueryRow(qctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("with two uploads holding connections, a request got none: %v", err)
	}
	mu.Lock()
	n := inside
	mu.Unlock()
	if n > 2 {
		t.Fatalf("%d uploads held connections at once, want at most the 2 put slots", n)
	}
	unblock()
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			t.Errorf("upload: %v", err)
		}
	}
}

func TestUploadOriginal_SmallPoolNeverStarves(t *testing.T) {
	r := newPinRaceRig(t)
	svc, small := smallPoolService(t, r)

	// Two promotable variants, prepared on the ordinary pool.
	var parents []string
	for i := 0; i < 2; i++ {
		parent := r.object()
		body := randomBytes(t, 1500+i)
		r.cleanupObject(sha(body))
		r.variant(parent, "hires", body)
		parents = append(parents, parent)
	}
	shared := randomBytes(t, 1700) // uploaded by several at once
	r.cleanupObject(sha(shared))

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	run := func(f func(ctx context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f(context.Background())
		}()
	}
	for i := 0; i < 6; i++ {
		body := randomBytes(t, 1200+i)
		r.cleanupObject(sha(body))
		run(func(ctx context.Context) error {
			_, err := svc.UploadOriginal(ctx, bytes.NewReader(body), "image/png", uploadPin)
			return err
		})
		run(func(ctx context.Context) error {
			_, err := svc.UploadOriginal(ctx, bytes.NewReader(shared), "image/png", uploadPin)
			return err
		})
	}
	for _, parent := range parents {
		run(func(ctx context.Context) error {
			p, err := svc.PromoteVariant(ctx, parent, "hires")
			if err != nil {
				return err
			}
			return svc.ReleasePromotion(ctx, p)
		})
	}
	for i := 0; i < 4; i++ {
		run(func(ctx context.Context) error {
			pin := storage.PinRef{SubjectType: "asset", SubjectID: "small-pool"}
			if err := svc.AddPin(ctx, pin, sha(shared)); err != nil {
				return nil // the object may not exist yet; only progress matters here
			}
			return svc.RemovePin(ctx, pin, sha(shared))
		})
		run(func(ctx context.Context) error {
			var one int
			return small.QueryRow(ctx, `SELECT 1`).Scan(&one)
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("work at a 3-connection pool did not finish in 60s: starvation or deadlock")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("small-pool operation: %v", err)
		}
	}
}
