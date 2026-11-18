// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package emit

import (
	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// CommentRef is the typed reference an emit helper needs about a
// comment. Handlers build this from their existing comment-row
// state. The parent fields are zero when the comment is top-level.
type CommentRef struct {
	ID              string // UUID stringified
	PostID          string
	PostTitle       string
	PostAuthorRef   int64
	PostAuthorURI   string
	ParentID        string // empty for top-level
	ParentAuthorRef int64
	ParentAuthorURI string
	Body            string
	Depth           int32 // 0 for top-level, 1+ for replies

	// TargetURI is the commented object's canonical URI on its own
	// origin, the wire `object` of the federated Create (spec v1
	// §7.5.5, #1604). Empty means the local post (PostID on this
	// instance), whose comments have no other instance to go to.
	TargetURI string
	// ParentURI is the parent comment's identity on the wire, for a
	// reply: a comment URI on the target's origin, or the Create
	// activity id of a comment that was itself federated. Empty for a
	// reply means the local parent comment (ParentID on this instance).
	ParentURI string
}

// Excerpt returns the first 120 chars of the comment body,
// suitable for the inbox-card render. Centralised here so the
// notification payload + the activity payload agree on what
// "excerpt" means.
func (c CommentRef) Excerpt() string {
	if len(c.Body) <= 120 {
		return c.Body
	}
	return c.Body[:120]
}

// CreateComment builds the Emission for a Create activity wrapping
// a Note with inReplyTo per AP §3 / §B. The Note's `inReplyTo`
// field points at the parent (post for top-level, comment for
// replies); per AP §7.1.2 receivers may forward the activity to
// the parent's audience.
//
// Two notifications fire:
//  1. comment_on_my_post to the post author (always).
//  2. reply_to_my_comment to the parent comment's author (only
//     when this is a reply AND the parent author is different
//     from the post author — the writer's notifier guards on
//     actor != recipient but doesn't dedup across notifications).
func CreateComment(actor ActorContext, comment CommentRef) Emission {
	actorRef := actor.UserRef
	commentObjectURI := actor.ObjectURI(activities.ObjectKindComment, comment.ID)
	postObjectURI := actor.ObjectURI(activities.ObjectKindPost, comment.PostID)

	// inReplyTo is the parent — the post for top-level, the
	// parent comment for replies. It stays in the LOCAL record as
	// before; the wire uses target_uri and in_reply_to_uri (#1604).
	inReplyTo := postObjectURI
	if comment.ParentID != "" {
		inReplyTo = actor.ObjectURI(activities.ObjectKindComment, comment.ParentID)
	}
	// The wire target and parent (spec v1 §7.5.5): `object` is the
	// commented object on its origin, `inReplyTo` only the parent
	// comment of a reply. Explicit, never guessed from a title or host.
	targetURI := comment.TargetURI
	if targetURI == "" {
		targetURI = postObjectURI
	}
	parentURI := comment.ParentURI
	if parentURI == "" && comment.ParentID != "" {
		parentURI = actor.ObjectURI(activities.ObjectKindComment, comment.ParentID)
	}

	em := Emission{
		Activity: activities.Input{
			Type:         federation.ActivityCreate,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     commentObjectURI,
				Kind:    activities.ObjectKindComment,
				LocalID: comment.ID,
			},
			To: []string{comment.PostAuthorURI},
			Payload: map[string]any{
				"object_type":       "Note",
				"inReplyTo":         inReplyTo,
				"content":           comment.Body,
				"comment_depth":     comment.Depth + 1, // 1-indexed for human readability
				"target_post_id":    comment.PostID,
				"target_post_title": comment.PostTitle,
				"target_uri":        targetURI,
			},
		},
		Notifications: []NotificationFanout{{
			Recipient:  comment.PostAuthorRef,
			Verb:       "comment_on_my_post",
			TargetKind: "post",
			TargetID:   comment.PostID,
			Payload: map[string]any{
				"post_title":    comment.PostTitle,
				"excerpt":       comment.Excerpt(),
				"comment_id":    comment.ID,
				"comment_depth": comment.Depth + 1,
			},
		}},
	}

	if parentURI != "" {
		em.Activity.Payload["in_reply_to_uri"] = parentURI
	}

	// Reply to a comment whose author is NOT the post author?
	// Fire a second notification for the parent's author. Self-
	// reply dedup happens at the writer's notifier (actor !=
	// recipient gate).
	if comment.ParentID != "" && comment.ParentAuthorRef != 0 && comment.ParentAuthorRef != comment.PostAuthorRef {
		em.Activity.CC = []string{comment.ParentAuthorURI}
		em.Notifications = append(em.Notifications, NotificationFanout{
			Recipient:  comment.ParentAuthorRef,
			Verb:       "reply_to_my_comment",
			TargetKind: "comment",
			TargetID:   comment.ParentID,
			Payload: map[string]any{
				"post_title":    comment.PostTitle,
				"excerpt":       comment.Excerpt(),
				"comment_id":    comment.ID,
				"comment_depth": comment.Depth + 1,
			},
		})
	}
	return em
}

// DeleteComment builds the Emission for a Delete activity per AP
// §6.4. Same Tombstone semantics as DeletePost.
//
// createActivityURI and to are set only when the actor deleting the
// comment is its own local author (#1572): the id of the Create that
// federated it, and that Create's `to` (the commented object's author, the
// actor an encrypted Delete is sealed to). The Delete is then federated to
// exactly where that Create went, naming it by that id. With them empty
// (a moderator deleting someone else's comment, a comment with no Create
// on record) the Delete stays a local record, never sent under an actor
// who did not write the comment.
//
// No notification — comment deletes are silent.
func DeleteComment(actor ActorContext, commentID, postID, createActivityURI string, to []string) Emission {
	actorRef := actor.UserRef
	commentObjectURI := actor.ObjectURI(activities.ObjectKindComment, commentID)
	payload := map[string]any{
		"object_type":    "Note",
		"target_post_id": postID,
	}
	if createActivityURI != "" {
		payload["create_activity_uri"] = createActivityURI
	}
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityDelete,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     commentObjectURI,
				Kind:    activities.ObjectKindComment,
				LocalID: commentID,
			},
			To:      to,
			Payload: payload,
		},
	}
}
