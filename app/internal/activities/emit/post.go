// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package emit

import (
	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// PostVisibility mirrors the post visibility column shipped in
// Phase 1.13. Drives the activity addressing — public posts CC
// followers, private posts go to no one (federation outbox
// won't deliver), followers-only posts CC just the followers
// collection.
type PostVisibility string

const (
	PostVisibilityPublic    PostVisibility = "public"
	PostVisibilityFollowers PostVisibility = "followers"
	PostVisibilityPrivate   PostVisibility = "private"
)

// CreatePost builds the Emission for a Create activity wrapping
// an aa:Post object per ADR 0043 §"Custom Object Types". The
// post itself becomes the Create's object; addressing flows from
// the post's visibility.
//
// No notification — the post author posting their own post
// doesn't notify themselves. Followers' "new post from someone
// you follow" notification is a future Phase 1.13.K consideration;
// for now Create just records.
func CreatePost(actor ActorContext, post PostRef, visibility PostVisibility) Emission {
	actorRef := actor.UserRef
	objectURI := actor.ObjectURI(activities.ObjectKindPost, post.ID)
	em := Emission{
		Activity: activities.Input{
			Type:         federation.ActivityCreate,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     objectURI,
				Kind:    activities.ObjectKindPost,
				LocalID: post.ID,
			},
			Payload: map[string]any{
				"object_type": "aa:Post",
				"title":       post.Title,
				"visibility":  string(visibility),
			},
		},
	}
	// Followers-collection addressing requires §8 to be wired (the
	// followers collection URL). Until that lands, we leave To/CC
	// empty for non-public posts and the federation outbox
	// dispatcher consults federation_shares — which is the
	// authoritative gate per ADR 0043 anyway.
	if visibility == PostVisibilityPublic {
		em.Activity.CC = []string{actor.URI() + "/followers"}
	} else if visibility == PostVisibilityFollowers {
		em.Activity.To = []string{actor.URI() + "/followers"}
	}
	return em
}

// UpdatePost builds the Emission for an Update activity per AP
// §6.3 / §7.3. The full post is the activity's object (S2S
// Update is total replacement per spec); the payload carries the
// post id for fast local lookup.
//
// No notification — passive updates don't ping anyone.
func UpdatePost(actor ActorContext, post PostRef, visibility PostVisibility) Emission {
	actorRef := actor.UserRef
	objectURI := actor.ObjectURI(activities.ObjectKindPost, post.ID)
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityUpdate,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     objectURI,
				Kind:    activities.ObjectKindPost,
				LocalID: post.ID,
			},
			Payload: map[string]any{
				"object_type": "aa:Post",
				"title":       post.Title,
				"visibility":  string(visibility),
			},
		},
	}
}

// DeletePost builds the Emission for a Delete activity per AP
// §6.4 / §7.4. Receivers replace the local object with a Tombstone
// (404 → 410 Gone). Payload carries the local post id so the
// federation outbox + admin audit can answer "what was deleted"
// without needing the (now-tombstoned) object.
//
// No notification.
func DeletePost(actor ActorContext, postID, postTitle string) Emission {
	actorRef := actor.UserRef
	objectURI := actor.ObjectURI(activities.ObjectKindPost, postID)
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityDelete,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     objectURI,
				Kind:    activities.ObjectKindPost,
				LocalID: postID,
			},
			Payload: map[string]any{
				"object_type": "aa:Post",
				"title":       postTitle, // last-known for audit
			},
		},
	}
}
