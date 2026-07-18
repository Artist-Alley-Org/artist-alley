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
	// parent comment for replies.
	inReplyTo := postObjectURI
	if comment.ParentID != "" {
		inReplyTo = actor.ObjectURI(activities.ObjectKindComment, comment.ParentID)
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
// No notification — comment deletes are silent.
func DeleteComment(actor ActorContext, commentID, postID string) Emission {
	actorRef := actor.UserRef
	commentObjectURI := actor.ObjectURI(activities.ObjectKindComment, commentID)
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
			Payload: map[string]any{
				"object_type":    "Note",
				"target_post_id": postID,
			},
		},
	}
}
