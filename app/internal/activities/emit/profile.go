// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package emit

import (
	"strconv"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// ProfileSnapshot is the post-write subset of a user_profiles row
// federated peers care about. Display name + avatar URL + bio
// drift if not propagated; per-user prefs (language, theme) are
// deliberately omitted — those are local UI state, not federated
// actor properties.
type ProfileSnapshot struct {
	DisplayName string
	Bio         string
	AvatarURL   string
	Location    string
	WebsiteURL  string
}

// UpdateProfile — Update(Actor) per AP §6.3 / §7.3. The actor's
// own profile is the activity's object; payload carries the
// post-write snapshot so federated peers can apply it to their
// local actor copy.
//
// No notification — your own profile edits don't ping anyone.
// (Followers' "your friend updated their profile" digest is a
// future preference-controlled feature, not a v1 deliverable.)
func UpdateProfile(actor ActorContext, profile ProfileSnapshot) Emission {
	actorRef := actor.UserRef
	// The Update activity's object is the actor itself — AP §6.3
	// pattern is "Update with object = the thing being updated".
	// For actor self-updates that means object = actor URI.
	actorURI := actor.URI()
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityUpdate,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actorURI,
			Object: &activities.ObjectRef{
				URI:     actorURI,
				Kind:    activities.ObjectKindUser,
				LocalID: strconv.FormatInt(actor.UserRef, 10),
			},
			Payload: map[string]any{
				"object_type":   "Person",
				"display_name":  profile.DisplayName,
				"summary":       profile.Bio, // AP convention: actor summary == bio
				"icon":          profile.AvatarURL,
				"location":      profile.Location,
				"website":       profile.WebsiteURL,
			},
		},
	}
}
