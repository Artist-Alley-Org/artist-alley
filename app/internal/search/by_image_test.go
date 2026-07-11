// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/search"
)

// TestByImage_ProviderNil_501StubPreserved — with no provider wired,
// the handler serves the pre-existing 501 sidecar_not_installed
// response 1.16.B-3 shipped. Regression guard: even after
// 1.16.B-3-followup lands, operators who haven't enabled the sidecar
// get the same UX.
func TestByImage_ProviderNil_501StubPreserved(t *testing.T) {
	h := &search.ByImageHandler{Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "/search/by-image", strings.NewReader(""))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if body["error"] != "sidecar_not_installed" {
		t.Fatalf("expected sidecar_not_installed, got %v", body["error"])
	}
	if body["reserved_since"] != "1.16.B-3" {
		t.Fatalf("expected reserved_since=1.16.B-3, got %v", body["reserved_since"])
	}
}

// TestByImage_WrongMethod_405 — reject GET etc.
func TestByImage_WrongMethod_405(t *testing.T) {
	h := &search.ByImageHandler{Logger: slog.Default()}
	req := httptest.NewRequest(http.MethodGet, "/search/by-image", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

// buildMultipart returns a request body with one file part named
// "file" carrying the given bytes + content-type.
func buildMultipart(t *testing.T, ct string, body []byte) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	partHdr := make(map[string][]string)
	partHdr["Content-Disposition"] = []string{`form-data; name="file"; filename="test.bin"`}
	partHdr["Content-Type"] = []string{ct}
	part, err := mw.CreatePart(partHdr)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}
	return buf, mw.FormDataContentType()
}
