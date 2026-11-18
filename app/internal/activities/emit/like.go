// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package emit

import (
	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// PostRef is the typed reference an emit helper needs about a
// post. Handlers build this from their existing post-row state.
type PostRef struct {
	ID            string // UUID stringified
	Title         string
	AuthorUserRef int64
	AuthorURI     string
	// TargetURI and OwnerPeerID name a post held by ANOTHER instance
	// (#1611): its canonical URI on that instance and the paired peer
	// that owns it. The caller supplies both explicitly; neither is ever
	// derived from a title, a local id or a host. Empty means a post on
	// this instance, whose Like stays local.
	TargetURI   string
	OwnerPeerID string
}

// Like builds the Emission for a Like activity per AP §6.8.
//
// Addressing: to=[post author]. Per our walled-garden model (ADR
// 0043 §"Trust model") addressing is descriptive — federation
// delivery is gated by federation_shares, not by the to/cc fields.
//
// Notification: like_on_my_post to the post author, with the post
// title in the payload so the inbox card renders without an extra
// fetch. Self-likes don't emit a notification (the writer's
// notifier guards on actor != recipient).
//
// A Like of another instance's post (post.TargetURI set, #1611) has
// that URI as its object, keeps the owning peer in the payload
// (target_uri, target_peer_id) for the outbox to address it to that
// peer only, and is addressed to the post's author there (post.AuthorURI,
// an actor on the target's instance). It notifies nobody here: the
// author is not a local user, and the owner notifies on receipt. Its
// activity id is the stable handle a later Undo names (#1572).
func Like(actor ActorContext, post PostRef) Emission {
	actorRef := actor.UserRef
	if post.TargetURI != "" {
		return Emission{
			Activity: activities.Input{
				Type:         federation.ActivityLike,
				ActivityURI:  actor.MintActivityURI(),
				ActorUserRef: &actorRef,
				ActorURI:     actor.URI(),
				Object: &activities.ObjectRef{
					URI:  post.TargetURI,
					Kind: activities.ObjectKindPost,
				},
				To: []string{post.AuthorURI},
				Payload: map[string]any{
					"post_title":     post.Title,
					"target_uri":     post.TargetURI,
					"target_peer_id": post.OwnerPeerID,
				},
			},
		}
	}
	objectURI := actor.ObjectURI(activities.ObjectKindPost, post.ID)
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityLike,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     objectURI,
				Kind:    activities.ObjectKindPost,
				LocalID: post.ID,
			},
			To: []string{post.AuthorURI},
			Payload: map[string]any{
				// Like has no required extra fields; carry the post
				// title so the federation outbox + admin audit UI
				// don't have to JOIN for the obvious context.
				"post_title": post.Title,
			},
		},
		Notifications: []NotificationFanout{{
			Recipient:  post.AuthorUserRef,
			Verb:       "like_on_my_post",
			TargetKind: "post",
			TargetID:   post.ID,
			Payload: map[string]any{
				"post_title": post.Title,
			},
		}},
	}
}

// UndoLike builds the Emission for an Undo wrapping a previous
// Like activity per AP §6.10. The Undo's object is the LIKE
// activity's URI (not the liked post).
//
// It is addressed like the Like it undoes (#1572): to=[post author]
// when post.AuthorURI is set, the actor an encrypted Undo is sealed to
// on the post's instance. post is the same reference the Like was built
// from; only its ID and AuthorURI are used.
//
// No notification — undoing a Like is silent.
func UndoLike(actor ActorContext, likeActivityURI string, post PostRef) Emission {
	actorRef := actor.UserRef
	var to []string
	if post.AuthorURI != "" {
		to = []string{post.AuthorURI}
	}
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityUndo,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     likeActivityURI,
				Kind:    activities.ObjectKindActivity,
				LocalID: likeActivityURI,
			},
			To: to,
			Payload: map[string]any{
				// Carry the original post ID so consumers don't
				// have to dereference the Undo's object to know
				// what was un-liked.
				"target_post_id": post.ID,
				"target_type":    "Like",
			},
		},
	}
}
