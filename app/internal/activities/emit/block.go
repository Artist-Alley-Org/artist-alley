// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package emit

import (
	"strconv"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// Block builds the Emission for a Block activity per AP §6.9.
//
// Critical AP invariant: Block MUST NOT be delivered to the
// blocked actor. We enforce this two ways:
//
//  1. The `to` field is EMPTY (no addressee) so the federation
//     outbox dispatcher in Phase 1.22.D has nothing to deliver
//     to.
//  2. The blocked user's ref goes in the payload under
//     target_user_ref so admin audit + DSAR queries can still
//     answer "who has user X blocked"; this is a local-only
//     detail, never serialized to the federation wire to the
//     blocked actor.
//
// No notification — the blocked party doesn't get notified.
func Block(blocker ActorContext, blocked UserRef, reason string) Emission {
	actorRef := blocker.UserRef
	payload := map[string]any{
		"target_user_ref": blocked.UserRef,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityBlock,
			ActivityURI:  blocker.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     blocker.URI(),
			Object: &activities.ObjectRef{
				URI:     blocked.URI,
				Kind:    activities.ObjectKindUser,
				LocalID: strconv.FormatInt(blocked.UserRef, 10),
			},
			// To intentionally empty — AP §6.9 forbids delivery
			// to the blocked actor. The federation outbox
			// dispatcher honours this.
			Payload: payload,
		},
	}
}

// UndoBlock builds the Emission for an Undo wrapping a previous
// Block activity. Same delivery constraint as Block — the
// formerly-blocked actor does not receive the Undo either (they
// shouldn't learn they were ever blocked).
//
// No notification.
func UndoBlock(actor ActorContext, blockActivityURI string, formerlyBlockedUserRef int64) Emission {
	actorRef := actor.UserRef
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityUndo,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     blockActivityURI,
				Kind:    activities.ObjectKindActivity,
				LocalID: blockActivityURI,
			},
			Payload: map[string]any{
				"target_user_ref": formerlyBlockedUserRef,
				"target_type":     "Block",
			},
		},
	}
}
