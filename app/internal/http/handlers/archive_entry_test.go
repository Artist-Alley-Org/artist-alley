// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// withTestIdentity stashes a non-nil Identity in ctx so the handler
// passes its auth gate. The downstream DB call is what the test
// expects to skip via the bad-asset-id branch, so the Identity's
// fields don't need to be meaningful.
func withTestIdentity(req *http.Request) *http.Request {
	id := &auth.Identity{}
	return req.WithContext(auth.WithIdentity(req.Context(), id))
}

// chiCtxRequest wires `{id}` into the chi URL-param table so the
// handler's `chi.URLParam(r, "id")` lookup finds something.
func chiCtxRequest(method, target, idParam string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", idParam)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// TestArchiveEntry_AnonymousIsNotAdmittedByDefault — since #415 an
// anonymous caller is no longer rejected at the door (public-tier assets
// are readable by anyone); the content gate decides instead. What must
// never happen is an anonymous caller being ADMITTED to an arbitrary
// asset, so this asserts the request does not succeed.
func TestArchiveEntry_AnonymousIsNotAdmittedByDefault(t *testing.T) {
	h := &ArchiveEntryHandler{}
	req := chiCtxRequest(http.MethodGet, "/assets/x/archive/entry?path=a.txt", "x")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Errorf("anonymous caller was admitted (status %d); must never be 200", rr.Code)
	}
}

func TestArchiveEntry_BadUUID(t *testing.T) {
	h := &ArchiveEntryHandler{}
	req := withTestIdentity(chiCtxRequest(http.MethodGet, "/assets/x/archive/entry?path=a.txt", "not-a-uuid"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestArchiveEntry_MissingPath(t *testing.T) {
	h := &ArchiveEntryHandler{}
	req := withTestIdentity(chiCtxRequest(http.MethodGet, "/assets/00000000-0000-0000-0000-000000000001/archive/entry", "00000000-0000-0000-0000-000000000001"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// TestArchiveEntry_PathTraversal asserts the early guard rejects
// anything that tries to escape the archive root. The handler must
// reject without ever touching storage so attackers can't probe for
// file existence on the host.
func TestArchiveEntry_PathTraversal(t *testing.T) {
	h := &ArchiveEntryHandler{}
	cases := []string{
		"/etc/passwd",
		"../../etc/passwd",
		"foo/../../secret",
		"a/../b",
	}
	for _, p := range cases {
		req := withTestIdentity(chiCtxRequest(http.MethodGet, "/assets/00000000-0000-0000-0000-000000000001/archive/entry?path="+p, "00000000-0000-0000-0000-000000000001"))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("path %q: status = %d, want 400", p, rr.Code)
		}
	}
}
