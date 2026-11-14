// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/seed"
)

// peerFlags collects repeated `--peer name=root` values.
type peerFlags map[string]string

func (p peerFlags) String() string { return fmt.Sprint(map[string]string(p)) }

func (p peerFlags) Set(v string) error {
	name, root, ok := strings.Cut(v, "=")
	if !ok || name == "" || root == "" {
		return fmt.Errorf("want name=root, got %q", v)
	}
	if _, dup := p[name]; dup {
		return fmt.Errorf("peer %q given twice", name)
	}
	p[name] = root
	return nil
}

// `aa seed-overlap ...` checks the cross-peer overlap contract (#1514)
// against explicitly named peer roots and prints the result as JSON. It is
// offline and read-only: no config, database, network or writes, and no
// default root for any peer, so it can never wander into a private corpus
// by omission. See internal/seed/overlap.go.
func runSeedOverlap(args []string) error {
	return runSeedOverlapTo(os.Stdout, args)
}

func runSeedOverlapTo(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("seed-overlap", flag.ContinueOnError)
	contract := fs.String("contract", "", "overlap contract (for example seed/contracts/peer-overlap.v1.json); required")
	peers := peerFlags{}
	fs.Var(peers, "peer", "name=root for one contract peer (repeat per peer); no peer has a default root")
	partial := fs.Bool("partial", false,
		"accept a subset of the contract's peers; the cross-peer pair checks are then NOT run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *contract == "" {
		return errors.New("seed-overlap: --contract is required")
	}
	if len(peers) == 0 {
		return errors.New("seed-overlap: at least one --peer name=root is required")
	}
	rep, err := seed.ValidatePeerOverlap(seed.OverlapOptions{
		ContractPath: *contract, Peers: peers, AllowPartial: *partial,
	})
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s\n", b); err != nil {
		return err
	}
	if !rep.PairsChecked {
		fmt.Fprintln(os.Stderr, "seed-overlap: PARTIAL: only some peers were checked; the cross-peer pairs were NOT validated")
	}
	return nil
}
