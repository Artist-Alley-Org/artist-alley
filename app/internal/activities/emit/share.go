// aa:Share + aa:Unshare emit helpers per the 1.22.C design
// proposal §2.3 + §4. Phase 1.22.C-c.
//
// Both emit helpers return Emission so handlers can wrap them in
// activities.WithEmissionFn — the share row insert, the activity
// row, and the federation.share.granted audit event ALL commit
// in the same transaction, satisfying the design's write-ahead-
// audit invariant (§7.2).

package emit

import (
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// ShareRef is the typed reference an emit helper needs about
// the share itself. Handlers build this from the post-insert
// federation_shares row.
type ShareRef struct {
	ShareID      uuid.UUID
	ObjectKind   federation.ShareObjectKind
	ObjectID     uuid.UUID
	PeerURL      string                       // peer's instance URL (for envelope `to`)
	TargetUserURL string                      // recipient's actor URL; "" = broadcast within peer
	Scope        federation.ShareScope
	ExpiresAt    *time.Time
	Notes        string
}

// Share emits an aa:Share activity per ADR 0043 §"Custom activity
// types" + the design §2.3. The activity's `object` is the
// resource being shared; `target` is the peer the share is going
// to; the payload carries scope + expiry so the receiver can
// project the same state locally.
//
// The activity addressing (`to`) names the recipient: a specific
// user URL when target_user_url is set, the peer's instance
// actor when broadcasting.
//
// No notification: shares are operator actions, not user-visible
// social events. The user whose object got shared MAY want a
// "your post was shared with Peer X by Admin Y" notification in
// a future phase; not v1.
func Share(actor ActorContext, ref ShareRef) Emission {
	actorRef := actor.UserRef
	objectURI := actor.ObjectURI(shareKindToActivityKind(ref.ObjectKind), ref.ObjectID.String())

	// Address: specific recipient if target_user_url set,
	// otherwise the peer-broadcast address.
	to := []string{}
	if ref.TargetUserURL != "" {
		to = append(to, ref.TargetUserURL)
	} else if ref.PeerURL != "" {
		// Per AP §5.6, the bare peer URL stands in for "all
		// actors on that peer" when no specific actor is named.
		to = append(to, ref.PeerURL)
	}

	payload := map[string]any{
		"share_id":      ref.ShareID.String(),
		"object_kind":   string(ref.ObjectKind),
		"object_id":     ref.ObjectID.String(),
		"object_uri":    objectURI,
		"scope":         string(ref.Scope),
		"peer_url":      ref.PeerURL,
	}
	if ref.TargetUserURL != "" {
		payload["target_user_url"] = ref.TargetUserURL
	}
	if ref.ExpiresAt != nil {
		payload["expires_at"] = ref.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if ref.Notes != "" {
		// Notes are operator-private — DON'T emit them on the
		// wire. We carry them in the local activity row's payload
		// for audit (the activities admin view shows them) but
		// strip when delivering to the peer. Delivery worker
		// (1.22.D) reads payload + redacts before send.
		payload["notes"] = ref.Notes
		payload["notes_private"] = true
	}

	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityAAShare,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     objectURI,
				Kind:    shareKindToActivityKind(ref.ObjectKind),
				LocalID: ref.ObjectID.String(),
			},
			// Target is the share itself — local-only URI; the
			// receiver doesn't need to dereference it but it
			// preserves the chain for audit.
			Target: &activities.ObjectRef{
				URI:     actor.BaseURL + "/federation/shares/" + ref.ShareID.String(),
				LocalID: ref.ShareID.String(),
			},
			To:      to,
			Payload: payload,
		},
	}
}

// Unshare emits an aa:Unshare activity per AP §6.4 (Tombstone
// semantics) + the design §4.1. The activity references the
// originating aa:Share via the previous_activity_uri payload
// field so the receiver knows which share to drop.
//
// Per the locked-in answer §12.5 #3: aa:RevokeShare is reserved
// for forward-compat but v1 emits only aa:Unshare. Receivers
// MUST treat any inbound aa:RevokeShare as aa:Unshare.
func Unshare(actor ActorContext, ref ShareRef, originalShareActivityURI string) Emission {
	actorRef := actor.UserRef
	objectURI := actor.ObjectURI(shareKindToActivityKind(ref.ObjectKind), ref.ObjectID.String())
	to := []string{}
	if ref.TargetUserURL != "" {
		to = append(to, ref.TargetUserURL)
	} else if ref.PeerURL != "" {
		to = append(to, ref.PeerURL)
	}
	payload := map[string]any{
		"share_id":                ref.ShareID.String(),
		"object_kind":             string(ref.ObjectKind),
		"object_id":               ref.ObjectID.String(),
		"object_uri":              objectURI,
		"peer_url":                ref.PeerURL,
		"previous_activity_uri":   originalShareActivityURI,
	}
	if ref.TargetUserURL != "" {
		payload["target_user_url"] = ref.TargetUserURL
	}
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityAAUnshare,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     objectURI,
				Kind:    shareKindToActivityKind(ref.ObjectKind),
				LocalID: ref.ObjectID.String(),
			},
			Target: &activities.ObjectRef{
				URI:     actor.BaseURL + "/federation/shares/" + ref.ShareID.String(),
				LocalID: ref.ShareID.String(),
			},
			To:      to,
			Payload: payload,
		},
	}
}

// shareKindToActivityKind bridges federation.ShareObjectKind
// (the 1.22.C catalogue) to activities.ActivityObjectKind (the
// 1.22.A catalogue). They're conceptually the same set with
// overlapping values; bridging here keeps the federation-layer
// types decoupled from the activities-layer types.
func shareKindToActivityKind(k federation.ShareObjectKind) activities.ActivityObjectKind {
	switch k {
	case federation.ShareObjectKindAsset:
		return activities.ObjectKindAsset
	case federation.ShareObjectKindPost:
		return activities.ObjectKindPost
	case federation.ShareObjectKindCollection:
		return activities.ObjectKindCollection
	case federation.ShareObjectKindWorkspace:
		return activities.ObjectKindWorkspace
	case federation.ShareObjectKindBrandKit:
		return activities.ObjectKindBrandKit
	case federation.ShareObjectKindUser:
		return activities.ObjectKindUser
	}
	// Unknown — return the input as-is; the activities-layer
	// CHECK will reject if it's not in its catalogue, surfacing
	// the catalogue drift as an immediate error.
	return activities.ActivityObjectKind(string(k))
}
