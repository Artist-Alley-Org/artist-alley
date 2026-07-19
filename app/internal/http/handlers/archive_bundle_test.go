// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The malformed id is deliberate: it returns before the DB call, so
// this handler test needs no pool.
//
// Do NOT add an authorization assertion here. Since #415 removed the
// per-handler 401, uuid.Parse is the FIRST statement, so a request
// with a bad id never reaches requireContentAccess — any "anonymous
// is denied" check placed here would pass even with the content gate
// deleted. The authorization contract is covered in
// visibility/content_test.go, against real rows.
func TestArchiveBundle_BadUUID(t *testing.T) {
	h := &ArchiveBundleHandler{}
	req := withTestIdentity(chiCtxRequest(http.MethodGet, "/assets/x/archive/bundle.zip", "not-a-uuid"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// TestSuggestBundleName guards the filename pattern surfaced to the
// browser's Save-as dialog. Short id prefix + source extension lets
// the user keep multiple bundles distinguishable in a downloads
// folder without leaking the full asset UUID.
func TestSuggestBundleName(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	name := suggestBundleName(id, "tar.gz")
	if !strings.HasSuffix(name, "-bundle.zip") {
		t.Errorf("name %q missing -bundle.zip suffix", name)
	}
	if !strings.HasPrefix(name, "01234567-") {
		t.Errorf("name %q missing short-id prefix", name)
	}
	if !strings.Contains(name, "tar.gz") {
		t.Errorf("name %q missing ext token", name)
	}
	// Empty extension still produces a valid filename.
	if got := suggestBundleName(id, ""); !strings.HasSuffix(got, "-bundle.zip") {
		t.Errorf("empty-ext name %q malformed", got)
	}
}
