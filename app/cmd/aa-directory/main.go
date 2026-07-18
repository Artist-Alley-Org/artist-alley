// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Binary aa-directory is the reference implementation of an
// artist-alley federation directory per
// docs/spec/federation-directory/v1.md.
//
// One process, one JSON file (atomically rewritten on every
// write), one Ed25519 signing keypair on disk. Anyone can run
// this — the artist-alley project's hosted artist-alley.org
// directory is one instance of this binary, but the wire
// format is open + nothing forces operators to use the hosted
// one. Run your own; run several; subscribe to all of them.
//
// Storage:
//
//   - <data-dir>/store.json    — listings + challenges (JSON)
//   - <data-dir>/operator.pub  — Ed25519 public key (PEM)
//   - <data-dir>/operator.priv — Ed25519 private key (PEM)
//   - <data-dir>/auth-token    — bearer token for DELETE
//                                /v1/listings/{instance_url}
//                                (one-line text file)
//
// On first boot the binary generates everything missing + logs
// the operator fingerprint + the bearer token. Subsequent boots
// reuse the persisted files.

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mscrnt/artist-alley/app/cmd/aa-directory/internal/store"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/dnstxt"
)

// Version is overwritten at build time via -ldflags.
var Version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("startup failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	var (
		addr         = flag.String("addr", ":8090", "HTTP listen address")
		dataDir      = flag.String("data", "./aa-directory-data", "data directory (created if missing)")
		operatorURL  = flag.String("operator-url", "https://localhost:8090", "this directory's canonical URL")
		operatorName = flag.String("operator-name", "Local aa-directory", "human-readable operator name")
		contact      = flag.String("contact", "", "operator contact (email / URL)")
		challengeTTL = flag.Duration("challenge-ttl", 1*time.Hour, "DNS-TXT challenge expiry window")
		skipDNS      = flag.Bool("dev-skip-dns", false, "DEV ONLY: accept registrations without DNS-TXT verification")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("aa-directory starting",
		slog.String("version", Version),
		slog.String("addr", *addr),
		slog.String("data", *dataDir),
		slog.String("operator_url", *operatorURL),
		slog.Bool("dev_skip_dns", *skipDNS),
	)
	if *skipDNS {
		logger.Warn("DEV MODE: DNS-TXT verification is BYPASSED — never use --dev-skip-dns in production")
	}

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*dataDir, "store.json"))
	if err != nil {
		return err
	}

	pub, priv, err := loadOrGenerateKey(*dataDir, logger)
	if err != nil {
		return err
	}
	pubPEM, _ := federation.PublicKeyToPEM(pub)
	op := store.Operator{
		Name:         *operatorName,
		OperatorURL:  strings.TrimRight(*operatorURL, "/"),
		Contact:      *contact,
		SpecVersion:  "aa-directory/v1",
		PublicKeyPEM: string(pubPEM),
		Fingerprint:  federation.PublicKeyFingerprint(pub),
	}
	if err := st.SetOperator(op); err != nil {
		return err
	}
	logger.Info("operator identity loaded",
		slog.String("fingerprint", op.Fingerprint),
		slog.String("operator_url", op.OperatorURL),
	)

	bearer, err := loadOrGenerateAuthToken(*dataDir, logger)
	if err != nil {
		return err
	}

	srvCfg := serverConfig{
		store:        st,
		signingKey:   priv,
		operatorHost: hostOf(op.OperatorURL),
		challengeTTL: *challengeTTL,
		skipDNS:      *skipDNS,
		bearerToken:  bearer,
		logger:       logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/operator", srvCfg.handleGetOperator)
	mux.HandleFunc("GET /v1/listing", srvCfg.handleGetListing)
	mux.HandleFunc("POST /v1/challenge", srvCfg.handlePostChallenge)
	mux.HandleFunc("POST /v1/register", srvCfg.handlePostRegister)
	mux.HandleFunc("DELETE /v1/listings/", srvCfg.handleDeleteListing)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Background challenge pruner.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go pruneLoop(ctx, st, logger)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	logger.Info("http.listen", slog.String("addr", *addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadOrGenerateKey reads the Ed25519 operator keypair from
// data dir, generating + persisting one if either file is missing.
func loadOrGenerateKey(dataDir string, logger *slog.Logger) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pubPath := filepath.Join(dataDir, "operator.pub")
	privPath := filepath.Join(dataDir, "operator.priv")
	pubBytes, errP := os.ReadFile(pubPath)
	privBytes, errPr := os.ReadFile(privPath)
	if errP == nil && errPr == nil {
		pub, err := federation.PublicKeyFromPEM(pubBytes)
		if err != nil {
			return nil, nil, fmt.Errorf("load operator.pub: %w", err)
		}
		priv, err := federation.PrivateKeyFromPEM(privBytes)
		if err != nil {
			return nil, nil, fmt.Errorf("load operator.priv: %w", err)
		}
		return pub, priv, nil
	}
	pub, priv, err := federation.GenerateActorKeyPair()
	if err != nil {
		return nil, nil, err
	}
	pubPEM, _ := federation.PublicKeyToPEM(pub)
	privPEM, _ := federation.PrivateKeyToPEM(priv)
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return nil, nil, err
	}
	logger.Info("operator keypair generated",
		slog.String("fingerprint", federation.PublicKeyFingerprint(pub)),
	)
	return pub, priv, nil
}

// loadOrGenerateAuthToken reads (or generates) the bearer token
// the operator uses for DELETE /v1/listings/. Logged on first
// generation so the operator can copy it from boot logs.
func loadOrGenerateAuthToken(dataDir string, logger *slog.Logger) (string, error) {
	path := filepath.Join(dataDir, "auth-token")
	existing, err := os.ReadFile(path)
	if err == nil && len(existing) > 0 {
		return strings.TrimSpace(string(existing)), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", err
	}
	logger.Warn("operator bearer token generated — copy from data dir",
		slog.String("path", path),
		slog.String("usage", "Authorization: Bearer <token> on DELETE /v1/listings/"),
	)
	return token, nil
}

// pruneLoop runs every hour while ctx is live.
func pruneLoop(ctx context.Context, st *store.Store, logger *slog.Logger) {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pruned, err := st.PruneExpiredChallenges()
			if err != nil {
				logger.Warn("prune challenges failed", slog.String("err", err.Error()))
			} else if pruned > 0 {
				logger.Info("pruned expired challenges", slog.Int("count", pruned))
			}
		}
	}
}

func hostOf(u string) string {
	h, _ := dnstxt.RecordName(u) // gives "_artist-alley.<host>" on success
	if h == "" {
		return ""
	}
	const prefix = "_artist-alley."
	return strings.TrimPrefix(h, prefix)
}
