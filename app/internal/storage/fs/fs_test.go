// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package fs_test

import (
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/storage/fs"
	"github.com/mscrnt/artist-alley/app/internal/storage/storagetest"
)

func TestFS_BackendContract(t *testing.T) {
	storagetest.RunBackendContract(t, func(t *testing.T) storage.Backend {
		t.Helper()
		root := t.TempDir()
		b, err := fs.New(root)
		if err != nil {
			t.Fatalf("fs.New: %v", err)
		}
		return b
	})
}

func TestFS_New_RejectsEmptyRoot(t *testing.T) {
	if _, err := fs.New(""); err == nil {
		t.Errorf("fs.New(\"\") should error")
	}
}

func TestFS_New_CreatesMissingRoot(t *testing.T) {
	tmp := t.TempDir() + "/nested/created/by-new"
	b, err := fs.New(tmp)
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	if b.Root != tmp {
		t.Errorf("Root=%q want %q", b.Root, tmp)
	}
}

func TestFS_Name(t *testing.T) {
	b, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	if got := b.Name(); got != "fs" {
		t.Errorf("Name()=%q want \"fs\"", got)
	}
}
