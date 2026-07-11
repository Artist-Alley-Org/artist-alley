// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Catalogue membership tests. Per ADR 0042 every typed catalogue
// has a closed set + a .Valid() method; this file asserts both
// the membership map and the validity predicate stay in lockstep.
// A new value added to the const block without an entry in the
// matching KnownXxx map (or vice-versa) fails here.

package federation_test

import (
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

func TestActivityTypeValid(t *testing.T) {
	known := []federation.ActivityType{
		federation.ActivityCreate, federation.ActivityUpdate, federation.ActivityDelete,
		federation.ActivityFollow, federation.ActivityAccept, federation.ActivityReject,
		federation.ActivityUndo, federation.ActivityLike, federation.ActivityAnnounce,
		federation.ActivityBlock,
		federation.ActivityAdd, federation.ActivityRemove,
		federation.ActivityAAShare, federation.ActivityAAUnshare,
		federation.ActivityAARevokeShare,
		federation.ActivityAAApprove, federation.ActivityAARequestChanges,
		federation.ActivityAAMarkReviewed, federation.ActivityAAAnnotation,
		federation.ActivityAAWorkflowTransition, federation.ActivityAAAssetVersion,
		federation.ActivityAASubscribe, federation.ActivityAAMention,
	}
	for _, t_ := range known {
		if !t_.Valid() {
			t.Errorf("declared activity type %q not in KnownActivityTypes", t_)
		}
	}
	if federation.ActivityType("aa:NotAType").Valid() {
		t.Error("unknown type should be invalid")
	}
	if federation.ActivityType("").Valid() {
		t.Error("empty type should be invalid")
	}
}

func TestActivityTypeMembershipParity(t *testing.T) {
	// Every declared const above MUST be in KnownActivityTypes
	// (the consts are the authoritative declaration; the map is
	// the O(1) lookup mirror). If they drift, this test catches.
	const expectedCount = 23
	if len(federation.KnownActivityTypes) != expectedCount {
		t.Errorf("KnownActivityTypes count drifted: got %d want %d (update both this test and the consts)",
			len(federation.KnownActivityTypes), expectedCount)
	}
}

func TestObjectTypeValid(t *testing.T) {
	known := []federation.ObjectType{
		federation.ObjectNote, federation.ObjectImage, federation.ObjectVideo,
		federation.ObjectDocument, federation.ObjectCollection, federation.ObjectOrderedCollection,
		federation.ObjectAAAsset, federation.ObjectAAPost, federation.ObjectAAWorkspace,
		federation.ObjectAABrandKit, federation.ObjectAACollection,
	}
	for _, k := range known {
		if !k.Valid() {
			t.Errorf("declared object type %q not in KnownObjectTypes", k)
		}
	}
	if federation.ObjectType("aa:Unknown").Valid() {
		t.Error("unknown object type should be invalid")
	}
}

func TestSignatureAlgorithmValid(t *testing.T) {
	if !federation.SignatureAlgEd25519.Valid() {
		t.Error("Ed25519 must be valid")
	}
	// v1 allowlist is exactly Ed25519 — no RSA, no HMAC, nothing else.
	for _, alg := range []federation.SignatureAlgorithm{
		"RSA", "rsa-sha256", "HMAC-SHA256", "hs2019", "", "ed25519", // case-sensitive: lowercase must NOT pass
	} {
		if alg.Valid() {
			t.Errorf("v1 allowlist breach: %q should not validate", alg)
		}
	}
}

func TestEncryptionAlgorithmValid(t *testing.T) {
	if !federation.EncryptionAlgNaClBox.Valid() {
		t.Error("nacl-box must be valid")
	}
	for _, alg := range []federation.EncryptionAlgorithm{"aes-gcm", "chacha20-poly1305", ""} {
		if alg.Valid() {
			t.Errorf("v1 allowlist breach: %q should not validate", alg)
		}
	}
}

func TestObjectKindValid(t *testing.T) {
	known := []federation.ObjectKind{
		federation.ObjectKindPost, federation.ObjectKindCollection,
		federation.ObjectKindWorkspace, federation.ObjectKindBrandKit,
		federation.ObjectKindAsset, federation.ObjectKindUser,
	}
	for _, k := range known {
		if !k.Valid() {
			t.Errorf("declared object kind %q not in KnownObjectKinds", k)
		}
	}
	if federation.ObjectKind("comment").Valid() {
		t.Error("'comment' should not be a valid object kind at v1 (comments aren't first-class shareable)")
	}
}

func TestTrustTierValid(t *testing.T) {
	for _, k := range []federation.TrustTier{
		federation.TrustConnected, federation.TrustDirectoryListed, federation.TrustAutoSync,
	} {
		if !k.Valid() {
			t.Errorf("declared trust tier %q not in KnownTrustTiers", k)
		}
	}
	if federation.TrustTier("trusted").Valid() {
		t.Error("'trusted' is not a defined tier; reject")
	}
}

func TestEncryptionPolicyValid(t *testing.T) {
	for _, k := range []federation.EncryptionPolicy{
		federation.EncryptionPlaintext, federation.EncryptionE2E,
	} {
		if !k.Valid() {
			t.Errorf("declared encryption policy %q not in KnownEncryptionPolicies", k)
		}
	}
}

func TestShareScopeValid(t *testing.T) {
	for _, k := range federation.ShareScopeOrdered {
		if !k.Valid() {
			t.Errorf("ordered scope %q not in KnownShareScopes", k)
		}
	}
	if federation.ShareScope("admin").Valid() {
		t.Error("'admin' is an ACL grant, not a share scope; reject")
	}
}

func TestShareScopeOrderedMatchesKnown(t *testing.T) {
	// Ordered must enumerate exactly the known set, no
	// drift in either direction.
	if len(federation.ShareScopeOrdered) != len(federation.KnownShareScopes) {
		t.Fatalf("Ordered and Known share-scope sets diverge in size: %d vs %d",
			len(federation.ShareScopeOrdered), len(federation.KnownShareScopes))
	}
	for _, s := range federation.ShareScopeOrdered {
		if _, ok := federation.KnownShareScopes[s]; !ok {
			t.Errorf("Ordered scope %q missing from KnownShareScopes", s)
		}
	}
}

func TestInboxStatusValid_AllSentinelsRegistered(t *testing.T) {
	// Every sentinel referenced in v1.md §12.1 must be in the
	// KnownInboxStatuses map. Iterate the values we expose +
	// confirm membership.
	sentinels := []federation.InboxStatus{
		federation.InboxStatusPending, federation.InboxStatusProcessed,
		federation.InboxStatusInvalidContext, federation.InboxStatusUnsigned,
		federation.InboxStatusUnsupportedAlgorithm, federation.InboxStatusSigMalformed,
		federation.InboxStatusSigInvalid, federation.InboxStatusUnknownKey,
		federation.InboxStatusUnknownField, federation.InboxStatusInvalidType,
		federation.InboxStatusInvalidActor, federation.InboxStatusInvalidObject,
		federation.InboxStatusInvalidPublished, federation.InboxStatusUnknownActor,
		federation.InboxStatusUnknownPeer, federation.InboxStatusPeerDisabled,
		federation.InboxStatusUnknownObject,
		federation.InboxStatusUnsharedObject,
		federation.InboxStatusEnvelopeSigMissing,
		federation.InboxStatusEncryptionRequired,
		federation.InboxStatusEncryptionNotSupported,
		federation.InboxStatusPlaintextTypeMismatch,
		federation.InboxStatusDecryptFailed,
		federation.InboxStatusStaleRequest,
		federation.InboxStatusReplay, federation.InboxStatusError,
	}
	for _, s := range sentinels {
		if _, ok := federation.KnownInboxStatuses[s]; !ok {
			t.Errorf("sentinel %q missing from KnownInboxStatuses", s)
		}
	}
	if len(federation.KnownInboxStatuses) != len(sentinels) {
		t.Errorf("count drift: KnownInboxStatuses=%d sentinels=%d",
			len(federation.KnownInboxStatuses), len(sentinels))
	}
}

func TestOutboxStatusValid(t *testing.T) {
	for _, s := range []federation.OutboxStatus{
		federation.OutboxStatusQueued, federation.OutboxStatusSent,
		federation.OutboxStatusFailed, federation.OutboxStatusCancelled,
	} {
		if !s.Valid() {
			t.Errorf("outbox status %q must validate", s)
		}
	}
	if federation.OutboxStatus("retrying").Valid() {
		t.Error("'retrying' is not a defined status; reject")
	}
}

func TestContextV1Pinned(t *testing.T) {
	// The wire-format @context string is a contract surface;
	// changing it without a coordinated protocol-version bump
	// breaks every paired instance. Pin it.
	if federation.ContextV1 != "https://artist-alley.org/protocol/v1" {
		t.Errorf("ContextV1 drift — got %q, want artist-alley v1", federation.ContextV1)
	}
}
