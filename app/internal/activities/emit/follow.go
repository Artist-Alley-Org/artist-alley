// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package emit

import (
	"strconv"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// UserRef is the typed reference an emit helper needs about a
// target user. Handlers build this from their existing user-row
// state.
type UserRef struct {
	UserRef  int64
	Username string
	URI      string // pre-computed actor URI (avoids a second baseURL+username concat)
}

// Follow builds the Emission for a Follow activity per AP §7.5.
// In our walled garden, all follows are auto-accepted (no manual
// approval flow in v1), but we still emit the Follow + a paired
// Accept so the wire format matches AP semantics — see
// AutoAcceptFollow below for the paired emit.
//
// Notification: new_follower to the followee (the person being
// followed). No self-follows (the writer's notifier guards on
// actor != recipient).
func Follow(follower ActorContext, followee UserRef) Emission {
	actorRef := follower.UserRef
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityFollow,
			ActivityURI:  follower.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     follower.URI(),
			Object: &activities.ObjectRef{
				URI:     followee.URI,
				Kind:    activities.ObjectKindUser,
				LocalID: strconv.FormatInt(followee.UserRef, 10),
			},
			To: []string{followee.URI},
		},
		Notifications: []NotificationFanout{{
			Recipient:  followee.UserRef,
			Verb:       "new_follower",
			TargetKind: "user",
			TargetID:   strconv.FormatInt(follower.UserRef, 10),
		}},
	}
}

// AutoAcceptFollow is the paired Accept activity that records our
// walled-garden's auto-approval of a Follow. Per AP §7.6, an
// Accept whose object is a Follow is what officially adds the
// follower to the followee's `followers` collection. Even though
// our domain table commits the follow immediately, the wire
// record needs the Accept so a future-public-fediverse translator
// can degrade gracefully.
//
// Actor of the Accept is the FOLLOWEE (they're accepting). No
// notification — the new_follower notification already fired on
// the Follow itself.
func AutoAcceptFollow(followee ActorContext, follower UserRef, followActivityURI string) Emission {
	actorRef := followee.UserRef
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityAccept,
			ActivityURI:  followee.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     followee.URI(),
			Object: &activities.ObjectRef{
				URI:     followActivityURI,
				Kind:    activities.ObjectKindActivity,
				LocalID: followActivityURI,
			},
			To: []string{follower.URI},
		},
	}
}

// UndoFollow builds the Emission for an Undo wrapping a previous
// Follow activity per AP §6.10. Same shape as UndoLike.
//
// No notification — unfollow is silent.
func UndoFollow(actor ActorContext, followActivityURI string, followeeUserRef int64) Emission {
	actorRef := actor.UserRef
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityUndo,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     followActivityURI,
				Kind:    activities.ObjectKindActivity,
				LocalID: followActivityURI,
			},
			Payload: map[string]any{
				"target_followee_user_ref": followeeUserRef,
				"target_type":              "Follow",
			},
		},
	}
}
