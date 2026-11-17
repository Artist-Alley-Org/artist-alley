// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package twostack_test

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestMain makes the instances' own outbound clients trust the
// httptest certificate (TEST-ONLY TRUST ROOT). The outbox delivery
// worker builds its own transport on the system roots, so the gate
// points SSL_CERT_FILE at the one certificate every httptest TLS server
// presents, before anything in this process loads the system pool. No
// production code is changed: delivery, the inbox and the handshake run
// exactly as deployed, against a root store holding only that
// certificate.
func TestMain(m *testing.M) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	cert := srv.Certificate()
	srv.Close()
	dir, err := os.MkdirTemp("", "twostack-roots-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "twostack: temp dir:", err)
		os.Exit(1)
	}
	path := filepath.Join(dir, "httptest-root.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "twostack: write root:", err)
		os.Exit(1)
	}
	os.Setenv("SSL_CERT_FILE", path)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
