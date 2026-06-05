package emit

import (
	"strconv"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// DirectMessage builds the Emission for a Create activity wrapping
// a Note addressed to a single recipient — the AP idiom for a DM
// per §3 (object) + §6.1 (addressing).
//
// Notification: direct_message_received to the recipient, with
// the message excerpt + DM id in the payload so the inbox bell
// renders without a second fetch.
func DirectMessage(sender ActorContext, recipient UserRef, messageID, body string) Emission {
	senderRef := sender.UserRef
	messageObjectURI := sender.ObjectURI(activities.ObjectKindMessage, messageID)
	excerpt := body
	if len(excerpt) > 120 {
		excerpt = excerpt[:120]
	}
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityCreate,
			ActivityURI:  sender.MintActivityURI(),
			ActorUserRef: &senderRef,
			ActorURI:     sender.URI(),
			Object: &activities.ObjectRef{
				URI:     messageObjectURI,
				Kind:    activities.ObjectKindMessage,
				LocalID: messageID,
			},
			To: []string{recipient.URI},
			Payload: map[string]any{
				"object_type": "Note",
				"content":     body,
				// "directMessage" marker so consumers (federation
				// outbox, admin audit, future spam-filter) can
				// distinguish a DM from a public Note without
				// re-checking the addressing fields.
				"directMessage": true,
			},
		},
		Notifications: []NotificationFanout{{
			Recipient:  recipient.UserRef,
			Verb:       "direct_message_received",
			TargetKind: "user",
			TargetID:   strconv.FormatInt(sender.UserRef, 10),
			Payload: map[string]any{
				"excerpt":    excerpt,
				"message_id": messageID,
			},
		}},
	}
}
