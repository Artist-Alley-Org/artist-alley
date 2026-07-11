// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// HTTP handlers for the reference directory server.

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mscrnt/artist-alley/app/cmd/aa-directory/internal/store"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/dnstxt"
)

// serverConfig is the shared state every handler closure needs.
type serverConfig struct {
	store        *store.Store
	signingKey   ed25519.PrivateKey
	operatorHost string
	challengeTTL time.Duration
	skipDNS      bool
	bearerToken  string
	logger       *slog.Logger
}

// --- GET /v1/operator ----------------------------------------------------

func (s *serverConfig) handleGetOperator(w http.ResponseWriter, _ *http.Request) {
	op := s.store.GetOperator()
	writeJSON(w, http.StatusOK, op)
}

// --- GET /v1/listing -----------------------------------------------------

type listingResponse struct {
	Directory listingDirectory `json:"directory"`
	Entries   []store.Listing  `json:"entries"`
	NextCursor *string         `json:"next_cursor"`
}

type listingDirectory struct {
	Name         string    `json:"name"`
	OperatorURL  string    `json:"operator_url"`
	SpecVersion  string    `json:"spec_version"`
	GeneratedAt  time.Time `json:"generated_at"`
	Signature    string    `json:"signature"`
	PublicKeyPEM string    `json:"public_key_pem"`
}

func (s *serverConfig) handleGetListing(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := atoiClamp(v, 1, 500); err == nil {
			limit = n
		}
	}
	entries := s.store.ListListings(limit)
	// Sign the canonical encoding of `entries` per the spec.
	rawEntries, err := json.Marshal(entries)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	canonical, err := federation.Canonicalize(rawEntries)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	sig := ed25519.Sign(s.signingKey, canonical)
	op := s.store.GetOperator()
	writeJSON(w, http.StatusOK, listingResponse{
		Directory: listingDirectory{
			Name:         op.Name,
			OperatorURL:  op.OperatorURL,
			SpecVersion:  op.SpecVersion,
			GeneratedAt:  time.Now().UTC(),
			Signature:    base64.StdEncoding.EncodeToString(sig),
			PublicKeyPEM: op.PublicKeyPEM,
		},
		Entries: entries,
	})
}

// --- POST /v1/challenge --------------------------------------------------

type challengeRequest struct {
	InstanceURL string `json:"instance_url"`
}

type challengeResponse struct {
	Token       string    `json:"token"`
	RecordName  string    `json:"record_name"`
	RecordValue string    `json:"record_value"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (s *serverConfig) handlePostChallenge(w http.ResponseWriter, r *http.Request) {
	var req challengeRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	recName, err := dnstxt.RecordName(req.InstanceURL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	tokBuf := make([]byte, 16)
	if _, err := rand.Read(tokBuf); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	token := hex.EncodeToString(tokBuf)
	expires := time.Now().UTC().Add(s.challengeTTL)
	if err := s.store.PutChallenge(store.Challenge{
		Token:       token,
		InstanceURL: req.InstanceURL,
		ExpiresAt:   expires,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, challengeResponse{
		Token:       token,
		RecordName:  recName,
		RecordValue: dnstxt.RecordValue(s.operatorHost, token),
		ExpiresAt:   expires,
	})
}

// --- POST /v1/register ---------------------------------------------------

type registerRequest struct {
	InstanceURL           string   `json:"instance_url"`
	DisplayName           string   `json:"display_name"`
	InstancePublicKeyPEM  string   `json:"instance_public_key_pem"`
	Region                string   `json:"region,omitempty"`
	Description           string   `json:"description,omitempty"`
	Tags                  []string `json:"tags,omitempty"`
	DNSTXTToken           string   `json:"dns_txt_token"`
}

func (s *serverConfig) handlePostRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	if req.InstanceURL == "" || req.DisplayName == "" || req.InstancePublicKeyPEM == "" || req.DNSTXTToken == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "instance_url, display_name, instance_public_key_pem, dns_txt_token all required"})
		return
	}
	pub, err := federation.PublicKeyFromPEM([]byte(req.InstancePublicKeyPEM))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "instance_public_key_pem: " + err.Error()})
		return
	}

	// Consume the challenge atomically.
	c, err := s.store.ConsumeChallenge(req.DNSTXTToken)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrChallengeNotFound):
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "token unknown"})
			return
		case errors.Is(err, store.ErrChallengeExpired):
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "token expired"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	// Challenge was issued for THIS instance_url specifically.
	if c.InstanceURL != req.InstanceURL {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "token bound to different instance_url"})
		return
	}

	// DNS-TXT verification (unless dev bypass).
	if !s.skipDNS {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		err := dnstxt.Verify(ctx, dnstxt.DefaultResolver, dnstxt.VerifyInput{
			InstanceURL:   req.InstanceURL,
			DirectoryHost: s.operatorHost,
			Token:         req.DNSTXTToken,
		})
		if err != nil {
			if errors.Is(err, dnstxt.ErrNoMatchingRecord) || errors.Is(err, dnstxt.ErrLookupFailed) {
				// 202 = "DNS not yet propagated" — let the operator
				// retry once their record propagates.
				writeJSON(w, http.StatusAccepted, errorBody{Error: "DNS-TXT not (yet) verifiable: " + err.Error()})
				return
			}
			writeJSON(w, http.StatusForbidden, errorBody{Error: err.Error()})
			return
		}
	}

	listing := store.Listing{
		InstanceURL:          req.InstanceURL,
		DisplayName:          req.DisplayName,
		InstancePublicKeyPEM: req.InstancePublicKeyPEM,
		Fingerprint:          federation.PublicKeyFingerprint(pub),
		Region:               req.Region,
		Description:          req.Description,
		Tags:                 req.Tags,
		VerifiedAt:           time.Now().UTC(),
		VerifiedVia:          verifiedViaLabel(s.skipDNS),
	}
	if err := s.store.UpsertListing(listing); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	s.logger.Info("registered listing",
		slog.String("instance_url", listing.InstanceURL),
		slog.String("fingerprint", listing.Fingerprint),
		slog.String("verified_via", listing.VerifiedVia),
	)
	writeJSON(w, http.StatusCreated, listing)
}

func verifiedViaLabel(skipDNS bool) string {
	if skipDNS {
		return "dev-bypass"
	}
	return "dns-txt"
}

// --- DELETE /v1/listings/ ------------------------------------------------

func (s *serverConfig) handleDeleteListing(w http.ResponseWriter, r *http.Request) {
	if !s.checkBearer(r) {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "operator bearer token required"})
		return
	}
	// Path is /v1/listings/{instance_url} — URL-encoded.
	rest := strings.TrimPrefix(r.URL.Path, "/v1/listings/")
	if rest == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "instance_url path segment required"})
		return
	}
	// Path-segment URL-decode happens automatically for the
	// router; what we get is the literal URL.
	if err := s.store.DeleteListingByURL(rest); err != nil {
		if errors.Is(err, store.ErrListingNotFound) {
			writeJSON(w, http.StatusNotFound, errorBody{Error: "listing not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	s.logger.Info("removed listing", slog.String("instance_url", rest))
	w.WriteHeader(http.StatusNoContent)
}

func (s *serverConfig) checkBearer(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return strings.TrimSpace(h[len(prefix):]) == s.bearerToken
}

// --- shared helpers ------------------------------------------------------

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64*1024))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func atoiClamp(s string, min, max int) (int, error) {
	// Tiny parser, no strconv dep needed — but use strconv for
	// clarity. Returns error on non-numeric; clamps to [min,max]
	// on parse success.
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a positive integer")
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			break // saturate
		}
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n, nil
}
