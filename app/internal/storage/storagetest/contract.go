// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package storagetest is the shared contract harness every
// storage.Backend implementation runs through. Each backend's *_test.go
// calls RunBackendContract with a factory; the harness drives every
// method through happy and edge paths.
//
// Keeping the harness in its own subpackage means production code in
// the storage package does not depend on the testing package.
package storagetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// RunBackendContract exercises a Backend through every method in the
// interface. Implementations export a test wrapper that constructs a
// fresh backend per call. Keeping the contract in this package means
// adding a new backend (s3, gcs, ...) reduces to wiring up
// RunBackendContract — the behaviour we expect is documented in code.
//
// Backends that genuinely can't support an operation (e.g. fs +
// presigned URLs) should return storage.ErrUnsupported; the contract test
// recognises and skips those assertions.
func RunBackendContract(t *testing.T, makeBackend func(t *testing.T) storage.Backend) {
	t.Helper()

	t.Run("PutGetStat", func(t *testing.T) {
		b := makeBackend(t)
		body := bytes.Repeat([]byte("hello "), 1024) // 6 KiB
		hash := sha256Hex(body)

		// Put
		info, err := b.Put(context.Background(), hash, storage.VariantOriginal, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if info.Size != int64(len(body)) {
			t.Errorf("Put.Size=%d want %d", info.Size, len(body))
		}

		// Stat
		st, err := b.Stat(context.Background(), hash, storage.VariantOriginal)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if st.Size != int64(len(body)) {
			t.Errorf("Stat.Size=%d want %d", st.Size, len(body))
		}

		// Get round-trips the bytes exactly.
		r, _, err := b.Get(context.Background(), hash, storage.VariantOriginal)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("round-trip mismatch")
		}
	})

	t.Run("Idempotent_Put_Overwrites", func(t *testing.T) {
		b := makeBackend(t)
		hash := sha256Hex([]byte("v1"))
		ctx := context.Background()
		if _, err := b.Put(ctx, hash, storage.VariantOriginal, bytes.NewReader([]byte("v1"))); err != nil {
			t.Fatalf("first Put: %v", err)
		}
		if _, err := b.Put(ctx, hash, storage.VariantOriginal, bytes.NewReader([]byte("v2-LONGER"))); err != nil {
			t.Fatalf("second Put: %v", err)
		}
		st, _ := b.Stat(ctx, hash, storage.VariantOriginal)
		if st.Size != int64(len("v2-LONGER")) {
			t.Errorf("overwrite did not update size: %d", st.Size)
		}
	})

	t.Run("Delete_Idempotent", func(t *testing.T) {
		b := makeBackend(t)
		hash := sha256Hex([]byte("to-delete"))
		ctx := context.Background()
		// Delete-before-Put is a no-op.
		if err := b.Delete(ctx, hash, storage.VariantOriginal); err != nil {
			t.Errorf("delete missing: %v", err)
		}
		if _, err := b.Put(ctx, hash, storage.VariantOriginal, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := b.Delete(ctx, hash, storage.VariantOriginal); err != nil {
			t.Errorf("delete present: %v", err)
		}
		// After delete, Stat reports not-found.
		if _, err := b.Stat(ctx, hash, storage.VariantOriginal); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("Stat after delete: %v (want storage.ErrNotFound)", err)
		}
	})

	t.Run("NotFound_Errors", func(t *testing.T) {
		b := makeBackend(t)
		hash := sha256Hex([]byte("never-uploaded"))
		ctx := context.Background()
		if _, _, err := b.Get(ctx, hash, storage.VariantOriginal); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("Get missing: %v want storage.ErrNotFound", err)
		}
		if _, err := b.Stat(ctx, hash, storage.VariantOriginal); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("Stat missing: %v want storage.ErrNotFound", err)
		}
		if _, err := b.GetRange(ctx, hash, storage.VariantOriginal, 0, 10); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("GetRange missing: %v want storage.ErrNotFound", err)
		}
	})

	t.Run("GetRange", func(t *testing.T) {
		b := makeBackend(t)
		body := []byte("0123456789ABCDEFGHIJ") // 20 bytes
		hash := sha256Hex(body)
		ctx := context.Background()
		if _, err := b.Put(ctx, hash, storage.VariantOriginal, bytes.NewReader(body)); err != nil {
			t.Fatalf("Put: %v", err)
		}

		// Middle slice.
		r, err := b.GetRange(ctx, hash, storage.VariantOriginal, 5, 10)
		if err != nil {
			t.Fatalf("GetRange: %v", err)
		}
		got, _ := io.ReadAll(r)
		_ = r.Close()
		if string(got) != "56789ABCDE" {
			t.Errorf("range slice: got %q want %q", got, "56789ABCDE")
		}

		// length=0 means until EOF.
		r, err = b.GetRange(ctx, hash, storage.VariantOriginal, 15, 0)
		if err != nil {
			t.Fatalf("GetRange to EOF: %v", err)
		}
		got, _ = io.ReadAll(r)
		_ = r.Close()
		if string(got) != "FGHIJ" {
			t.Errorf("range to EOF: got %q want %q", got, "FGHIJ")
		}
	})

	t.Run("Variants_Are_Independent", func(t *testing.T) {
		b := makeBackend(t)
		hash := sha256Hex([]byte("source-of-variants"))
		ctx := context.Background()
		variants := map[string][]byte{
			storage.VariantOriginal: []byte("ORIGINAL"),
			"thumb_512":             []byte("THUMB-bytes"),
			"hls/index.m3u8":        []byte("#EXTM3U\n"),
			"hls/seg00001.ts":       []byte("SEG1"),
		}
		for k, v := range variants {
			if _, err := b.Put(ctx, hash, k, bytes.NewReader(v)); err != nil {
				t.Fatalf("Put %s: %v", k, err)
			}
		}
		// Verify each variant round-trips exactly and others aren't disturbed.
		for k, want := range variants {
			r, _, err := b.Get(ctx, hash, k)
			if err != nil {
				t.Errorf("Get %s: %v", k, err)
				continue
			}
			got, _ := io.ReadAll(r)
			_ = r.Close()
			if !bytes.Equal(got, want) {
				t.Errorf("variant %s: got %q want %q", k, got, want)
			}
		}
		// Deleting one variant leaves the others intact.
		if err := b.Delete(ctx, hash, "thumb_512"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := b.Stat(ctx, hash, "thumb_512"); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("deleted variant should be gone: %v", err)
		}
		if _, err := b.Stat(ctx, hash, storage.VariantOriginal); err != nil {
			t.Errorf("other variant should still exist: %v", err)
		}
	})

	t.Run("Concurrent_Puts_Safe", func(t *testing.T) {
		// Hammering the same (hash, variant) from multiple goroutines
		// should not corrupt the destination. The last writer wins;
		// every concurrent reader sees a fully-written file.
		b := makeBackend(t)
		hash := sha256Hex([]byte("concurrent"))
		ctx := context.Background()

		var wg sync.WaitGroup
		const N = 8
		for i := 0; i < N; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				body := bytes.Repeat([]byte{byte('A' + i)}, 1024)
				if _, err := b.Put(ctx, hash, storage.VariantOriginal, bytes.NewReader(body)); err != nil {
					t.Errorf("Put #%d: %v", i, err)
				}
			}()
		}
		wg.Wait()

		st, err := b.Stat(ctx, hash, storage.VariantOriginal)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if st.Size != 1024 {
			t.Errorf("size=%d want 1024 (each writer wrote 1024 bytes)", st.Size)
		}
		r, _, err := b.Get(ctx, hash, storage.VariantOriginal)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		got, _ := io.ReadAll(r)
		_ = r.Close()
		// One of A..H repeated 1024 times.
		if len(got) != 1024 {
			t.Errorf("read len=%d want 1024", len(got))
		}
		// All bytes should be identical (no torn writes).
		for i := 1; i < len(got); i++ {
			if got[i] != got[0] {
				t.Errorf("torn write at byte %d", i)
				break
			}
		}
	})

	t.Run("Context_Cancellation_Stops_Put", func(t *testing.T) {
		b := makeBackend(t)
		hash := sha256Hex([]byte("never-finish"))
		// Use a context that's already cancelled.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := b.Put(ctx, hash, storage.VariantOriginal, &slowReader{delay: 50 * time.Millisecond})
		if err == nil {
			t.Errorf("Put with cancelled ctx should error")
		}
	})

	t.Run("Presign_May_Be_Unsupported", func(t *testing.T) {
		b := makeBackend(t)
		hash := sha256Hex([]byte("any"))
		ctx := context.Background()
		_, errGet := b.PresignGet(ctx, hash, storage.VariantOriginal, time.Minute)
		_, errPut := b.PresignPut(ctx, hash, storage.VariantOriginal, time.Minute)
		// Backends that don't support presigning must say so via
		// storage.ErrUnsupported, never silently misbehave.
		if errGet != nil && !errors.Is(errGet, storage.ErrUnsupported) {
			t.Errorf("PresignGet unexpected err: %v", errGet)
		}
		if errPut != nil && !errors.Is(errPut, storage.ErrUnsupported) {
			t.Errorf("PresignPut unexpected err: %v", errPut)
		}
	})
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// slowReader is used to assert that backends honour context
// cancellation mid-Put.
type slowReader struct{ delay time.Duration }

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.delay)
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
