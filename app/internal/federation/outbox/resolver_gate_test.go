// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.22.I-d resolver-gate tests. Drives applyCapabilityGate
// directly via mocked hooks — no DB dependency, no skip on
// AA_DB_PASSWORD. The wiring is exercised end-to-end via scenario
// 08 in the dogfood loop; this file pins the unit-level behaviour.

package outbox

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// gateFixture wraps a Resolver with capturing hooks for the
// capability gate. The audit hook records every fire; the
// supports-encryption hook returns according to a peer→bool map.
type gateFixture struct {
	resolver *Resolver
	supports map[uuid.UUID]bool

	mu     sync.Mutex
	audits []skippedRecord
}

type skippedRecord struct {
	PeerID uuid.UUID
	Reason SkippedReason
	Verb   string
}

func newGateFixture(supports map[uuid.UUID]bool) *gateFixture {
	f := &gateFixture{
		resolver: &Resolver{}, // no pool/cache needed for the gate test
		supports: supports,
	}
	f.resolver.SetPeerSupportsEncryption(func(_ context.Context, peerID uuid.UUID) bool {
		return f.supports[peerID]
	})
	f.resolver.SetEmissionSkippedForPeer(func(_ context.Context, peerID uuid.UUID, reason SkippedReason, verb string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.audits = append(f.audits, skippedRecord{PeerID: peerID, Reason: reason, Verb: verb})
	})
	return f
}

func TestApplyCapabilityGate_Dormant_WhenRequiresEncryptionFalse(t *testing.T) {
	// Production traffic at 1.22.I-d sets RequiresEncryption=false
	// (default). The gate MUST NOT trigger — recipient set
	// passes through unchanged even when a peer doesn't support
	// encryption. Otherwise the existing 1.22.D plaintext path
	// would silently break.
	supportingPeer := uuid.New()
	missingPeer := uuid.New()
	f := newGateFixture(map[uuid.UUID]bool{
		supportingPeer: true,
		missingPeer:    false,
	})
	in := Input{Verb: "Like", RequiresEncryption: false}
	recipients := []Recipient{
		{PeerID: supportingPeer},
		{PeerID: missingPeer},
	}
	got := f.resolver.applyCapabilityGate(context.Background(), in, recipients)
	if len(got) != 2 {
		t.Errorf("gate dropped recipients when RequiresEncryption=false: got %d, want 2", len(got))
	}
	if len(f.audits) != 0 {
		t.Errorf("audit fired with RequiresEncryption=false: %d events", len(f.audits))
	}
}

func TestApplyCapabilityGate_DropsMissingPeer_AndFiresAudit(t *testing.T) {
	// Synthetic envelope with RequiresEncryption=true against a
	// broadcast (two recipients): one peer supports e2e, one
	// doesn't. The missing peer is dropped + the audit hook
	// fires with the e2e reason code. The supporting peer
	// remains in the recipient set.
	supportingPeer := uuid.New()
	missingPeer := uuid.New()
	f := newGateFixture(map[uuid.UUID]bool{
		supportingPeer: true,
		missingPeer:    false,
	})
	in := Input{Verb: "aa:Annotation", RequiresEncryption: true}
	recipients := []Recipient{
		{PeerID: supportingPeer},
		{PeerID: missingPeer},
	}
	got := f.resolver.applyCapabilityGate(context.Background(), in, recipients)
	if len(got) != 1 || got[0].PeerID != supportingPeer {
		t.Fatalf("gate did not drop the missing peer: got %+v", got)
	}
	if len(f.audits) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(f.audits))
	}
	if f.audits[0].PeerID != missingPeer {
		t.Errorf("audit fired for wrong peer: %v", f.audits[0].PeerID)
	}
	if f.audits[0].Reason != SkippedCapabilityMissingE2E {
		t.Errorf("audit reason = %q, want %q", f.audits[0].Reason, SkippedCapabilityMissingE2E)
	}
	if f.audits[0].Verb != "aa:Annotation" {
		t.Errorf("audit verb = %q, want %q", f.audits[0].Verb, "aa:Annotation")
	}
}

func TestApplyCapabilityGate_PassesThroughWhenAllPeersSupport(t *testing.T) {
	// Happy path of the future I-e/I-g traffic: every recipient
	// peer has negotiated e2e support. No drops, no audits.
	p1, p2 := uuid.New(), uuid.New()
	f := newGateFixture(map[uuid.UUID]bool{p1: true, p2: true})
	in := Input{Verb: "Create", RequiresEncryption: true}
	recipients := []Recipient{{PeerID: p1}, {PeerID: p2}}
	got := f.resolver.applyCapabilityGate(context.Background(), in, recipients)
	if len(got) != 2 {
		t.Errorf("gate dropped peers that support e2e: got %d, want 2", len(got))
	}
	if len(f.audits) != 0 {
		t.Errorf("audit fired despite no peers being dropped: %d events", len(f.audits))
	}
}

func TestApplyCapabilityGate_NilHookSafe(t *testing.T) {
	// Boot configurations without the supports-encryption hook
	// wired (e.g. tests, degraded modes) must NOT crash the
	// dispatcher. The gate stays dormant + every recipient
	// passes through, mirroring the RequiresEncryption=false
	// dormant case but for a different reason.
	resolver := &Resolver{} // no SetPeerSupportsEncryption call
	in := Input{Verb: "Like", RequiresEncryption: true}
	recipients := []Recipient{{PeerID: uuid.New()}, {PeerID: uuid.New()}}
	got := resolver.applyCapabilityGate(context.Background(), in, recipients)
	if len(got) != 2 {
		t.Errorf("unwired hook dropped recipients: got %d, want 2", len(got))
	}
}
