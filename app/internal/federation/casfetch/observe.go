// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package casfetch

import (
	"crypto/md5" //nolint:gosec // additional legacy fixity only, as federation.VerifyFileFixity
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// Where an [Observation] came from (#1574 transfer ledger,
// federation_transfer_events.observed_basis).
const (
	// ObservedReceived: the bytes the origin sent, as they streamed in.
	ObservedReceived = "received_bytes"
	// ObservedStored: bytes this instance already held, re-verified
	// before reuse.
	ObservedStored = "stored_bytes"
	// ObservedHeader: the origin's Content-Length, refused before any
	// byte was read.
	ObservedHeader = "response_header"
	// ObservedUnavailable: a mismatch was detected where nothing was
	// observed (the storage import's own re-check).
	ObservedUnavailable = "unavailable"
)

// Observation is what a fixity or length mismatch saw, against the
// request's listed SHA256 and Size. The bytes themselves are never kept.
type Observation struct {
	Basis string
	// Size is the number of bytes read (or the Content-Length, for
	// ObservedHeader). For a Truncated read it is a lower bound.
	Size    int64
	HasSize bool
	// SHA256 is the digest of every byte received; empty when the read
	// was Truncated or nothing was read.
	SHA256 string
	// Truncated: the body was longer than listed and was read only to
	// one byte past the listed size.
	Truncated bool
	// MismatchAlgorithm, MismatchExpected, MismatchObserved: an
	// additional listed checksum (sha512, md5) that did not match while
	// size and sha256 did.
	MismatchAlgorithm, MismatchExpected, MismatchObserved string
}

// observer counts and digests every byte federation.VerifyFileFixity
// reads (it reads at most the listed size plus one), with the same
// algorithms the file lists.
type observer struct {
	n      int64
	sha256 hash.Hash
	extra  map[string]hash.Hash
}

func newObserver(f federation.ArchivalFile) *observer {
	o := &observer{sha256: sha256.New(), extra: map[string]hash.Hash{}}
	for _, c := range f.Checksums {
		switch c.Algorithm {
		case "sha512":
			o.extra[c.Algorithm] = sha512.New()
		case "md5":
			o.extra[c.Algorithm] = md5.New() //nolint:gosec // additional legacy fixity only
		}
	}
	return o
}

func (o *observer) Write(p []byte) (int, error) {
	o.n += int64(len(p))
	o.sha256.Write(p)
	for _, h := range o.extra {
		h.Write(p)
	}
	return len(p), nil
}

// observation reports what was read of f, from basis.
func (o *observer) observation(basis string, f federation.ArchivalFile) *Observation {
	ob := &Observation{Basis: basis, Size: o.n, HasSize: true}
	if f.Size != nil && o.n > *f.Size {
		ob.Truncated = true
		return ob
	}
	ob.SHA256 = hex.EncodeToString(o.sha256.Sum(nil))
	if f.Size == nil || o.n != *f.Size {
		return ob
	}
	for _, c := range f.Checksums {
		if c.Algorithm == "sha256" && c.Value != ob.SHA256 {
			return ob // the sha256 itself is the evidence
		}
	}
	for _, c := range f.Checksums {
		h, ok := o.extra[c.Algorithm]
		if !ok {
			continue
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != c.Value {
			ob.MismatchAlgorithm, ob.MismatchExpected, ob.MismatchObserved = c.Algorithm, c.Value, got
			return ob
		}
	}
	return ob
}
