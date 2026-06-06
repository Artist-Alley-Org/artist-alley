package emit

import (
	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// AnnotationRef is the typed reference an emit helper needs for
// the aa:Annotation activity. Whiteboards are the v1 instance —
// stored as comments with annotation_type='whiteboard' so they
// inherit comment threading + likes + soft-delete + audit, but
// the federation shape is an aa:Annotation activity per ADR 0043
// §"Custom activity types".
type AnnotationRef struct {
	// CommentID is the comments.id (annotations are stored as
	// comments per the existing 1.18 design).
	CommentID string

	// PostID is the post the annotation is attached to.
	PostID    string
	PostTitle string

	// PostAuthor lets the emit fire a notification to the post
	// author. Set to (0, "") to skip the notification.
	PostAuthorRef int64
	PostAuthorURI string

	// AnnotationKind is the comments.annotation_type column value
	// — "whiteboard", "brush", "region", etc. Carried in the
	// payload so the federation receiver dispatches to the right
	// rendering surface.
	AnnotationKind string
}

// CreateAnnotation — aa:Annotation activity per ADR 0043. The
// activity is its own type (NOT a Create wrapper) per the spec
// because annotations have richer per-recipient semantics than
// generic Notes — receivers may want to apply the annotation to
// their local copy of the asset/post or render it as a layer.
//
// Notification: comment_on_my_post is reused since annotations
// surface in the same inbox as regular comments. The frontend
// renderer can key on `annotation_kind` if it wants to render
// "Alice annotated your post" differently from "Alice commented
// on your post".
func CreateAnnotation(actor ActorContext, ann AnnotationRef) Emission {
	actorRef := actor.UserRef
	commentURI := actor.ObjectURI(activities.ObjectKindComment, ann.CommentID)
	postURI := actor.ObjectURI(activities.ObjectKindPost, ann.PostID)

	em := Emission{
		Activity: activities.Input{
			Type:         federation.ActivityAAAnnotation,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     commentURI,
				Kind:    activities.ObjectKindComment,
				LocalID: ann.CommentID,
			},
			Payload: map[string]any{
				"annotation_kind": ann.AnnotationKind,
				"target_post_id":  ann.PostID,
				"target_post_uri": postURI,
			},
		},
	}
	if ann.PostAuthorURI != "" {
		em.Activity.To = []string{ann.PostAuthorURI}
	}
	if ann.PostAuthorRef != 0 {
		em.Notifications = []NotificationFanout{{
			Recipient:  ann.PostAuthorRef,
			Verb:       "comment_on_my_post", // reuse — annotations show in the same inbox
			TargetKind: "post",
			TargetID:   ann.PostID,
			Payload: map[string]any{
				"post_title":      ann.PostTitle,
				"annotation_kind": ann.AnnotationKind,
				"comment_id":      ann.CommentID,
				"is_annotation":   true,
			},
		}}
	}
	return em
}
