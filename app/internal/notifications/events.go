// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package notifications writes + serves the per-user in-app
// notification feed (Phase 1.17.I2, feat/user-surfaces).
//
// The Notify(ctx, ...) writer is the single entry point every
// emitter (comments, likes, follows, request approvals in 1.17.L,
// license-expiry warnings) calls when something happened that some
// user might want to know about. Three permission/preference gates
// apply at write time:
//
//  1. Actor != recipient (no self-notifications).
//  2. No block edge between actor and recipient (HasBlockBetween).
//  3. Recipient's channel pref for this verb includes "in_app".
//
// If all three pass, an in-app row lands. If the recipient's
// channel pref includes "email" as well, the writer queues a
// notification.email job (Phase I2-b — the job handler lands then;
// for now the column stays NULL and the row is in-app only).
//
// Verb taxonomy is shared with userprefs.KnownEventTypes — adding
// or renaming a verb requires touching both files. The shared
// strings here are the contract.

package notifications

// Verb constants. KEEP THESE IN SYNC with userprefs.KnownEventTypes —
// the prefs UI renders a toggle per known event type, and a verb
// emitted here that's NOT in the prefs catalog will resolve to
// system defaults (in_app only) silently. Drift breaks the user-
// facing settings page.
//
// Each constant's comment names the source emitter — useful for
// "who's writing this verb?" code review.
const (
	// Wired in 1.17.I2 itself (social handler comment/like/follow paths).
	VerbCommentOnMyPost  = "comment_on_my_post"   // emitter: social.CreatePostComment
	VerbLikeOnMyPost     = "like_on_my_post"      // emitter: social.LikePost
	VerbReplyToMyComment = "reply_to_my_comment"  // emitter: social.CreatePostComment (parent_id path)
	VerbNewFollower      = "new_follower"         // emitter: social.FollowUser

	// Wired by later sub-phases on this branch.
	VerbMentionOfMe              = "mention_of_me"                         // emitter: I2 mention parser (planned)
	VerbFollowedPosts            = "post_from_followed_user"               // emitter: posts.CreatePost (planned)
	VerbDirectMessageReceived    = "direct_message_received"               // emitter: I (1.17.I DMs)
	VerbBroadcastReceived        = "broadcast_received"                    // emitter: I admin broadcast
	VerbResourceRequestReceived  = "resource_request_received_to_approve" // emitter: L (1.17.L resource_requests)
	VerbResourceRequestApproved  = "resource_request_approved"             // emitter: L
	VerbResourceRequestDenied    = "resource_request_denied"               // emitter: L

	// System-generated (actor NULL); already-shipped emitters call
	// these via the same Notify writer.
	VerbLicenseExpiringSoon = "license_expiring_soon" // emitter: licensing.State re-verify ticker (planned hook)
	VerbLicenseExpired      = "license_expired"       // emitter: licensing.State re-verify ticker (planned hook)
)

// TargetKind values discriminate the polymorphic target_id column.
// Frontend renderer keys on (verb, target_kind) to route to the
// right notification card. Empty string is fine — system events
// (license, broadcast) carry no target.
const (
	TargetKindPost       = "post"
	TargetKindComment    = "comment"
	TargetKindAsset      = "asset"
	TargetKindUser       = "user"
	TargetKindCollection = "collection"
	TargetKindLicense    = "license"
	TargetKindRequest    = "request"
)

// Payload keys reserved for typed per-verb extra context. Documented
// here so writers + the frontend renderer stay aligned. Adding a key
// is additive — older notifications without it still render fine.
const (
	// All comment verbs: short excerpt of the comment body (first
	// ~120 chars, plain text). Lets the inbox card render "Alice
	// commented: 'Great work!'" without a second fetch.
	PayloadKeyExcerpt = "excerpt"

	// like_on_my_post + comment_on_my_post: title of the affected
	// post, so the inbox card reads "Alice liked 'Sunset Study #4'"
	// without a separate /posts/{id} fetch.
	PayloadKeyPostTitle = "post_title"

	// Comment-thread depth (1 = top-level, 2+ = nested reply).
	// Frontend uses this to disambiguate "reply to your comment"
	// from "reply on your post you also commented on".
	PayloadKeyCommentDepth = "comment_depth"

	// License verbs: kid + tier identifier so admins can correlate
	// notification to specific .lic file.
	PayloadKeyLicenseKid  = "license_kid"
	PayloadKeyLicenseTier = "license_tier"

	// Days-until-expiry for license_expiring_soon (positive = future,
	// negative = already expired — surfaced as a separate verb but
	// included here for completeness).
	PayloadKeyDaysUntilExpiry = "days_until_expiry"
)
