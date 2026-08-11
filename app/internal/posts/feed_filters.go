// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// feedFilterReader is the userprefs slice this package needs: one
// boolean, read once per feed page through that handler's LRU. Declared
// locally, like `notifier` above it, so posts doesn't import userprefs
// (which imports auth, which... — the local interface is what keeps the
// dependency arrow pointing one way).
type feedFilterReader interface {
	ShowRestrictedFeedMembers(ctx context.Context, userRef int64) (bool, error)
}

// SetFeedFilters installs the per-user browse-preference reader (#891).
// Post-construction setter, same shape as SetNotifier / SetMentions.
//
// nil-safe, and nil resolves to `false` — which since #921 means the
// DEFAULT feed, placeholders hidden. A test or a boot-order slip gets
// what every account without a stored preference gets, rather than a
// panic or a feed shaped unlike anyone else's.
func (h *Handler) SetFeedFilters(r feedFilterReader) { h.feedFilters = r }

// showRestricted resolves the caller's #891 browse preference for one
// feed page: does this reader want the #883 placeholders kept?
//
// # Which way the nil and error seams fail, and why that INVERTED at #921
//
// Both answer `false`, which is the same LITERAL this seam has always
// returned and the opposite BEHAVIOUR. Under #891 the preference was
// `hide_restricted` and `false` meant "show everything"; under #921 it
// is `show_restricted` and `false` means "hide the placeholders". The
// value did not change because the correct answer did not change: both
// seams fail to THE BUILD'S DEFAULT, and #921 moved what that is.
//
// That is a deliberate call, and the argument for it is that the
// alternative is worse in the direction that matters. Failing toward
// "show everything" would mean a prefs-lookup blip repaints every
// affected reader's feed as the wall of locked doors #921 exists to
// remove — a loud, instance-wide surprise, triggered by the component
// least related to what the reader is looking at. Failing toward the
// default shortens the feed of the minority who opted INTO placeholders,
// and shortens it to exactly what everyone else sees. A surprising
// experience for a few beats a rejected experience for all.
//
// It leaks nothing either way. The redaction that protects the content
// already happened in enrichPreview; this only decides whether the
// reader is shown the redaction or spared it, and neither direction can
// hand over a payload — a restricted member carries no `asset` at all.
// The Warn stays: a persistently failing lookup is still a bug, it is
// just no longer one the reader can see.
func (h *Handler) showRestricted(ctx context.Context, userRef int64) bool {
	if h.feedFilters == nil {
		return false
	}
	show, err := h.feedFilters.ShowRestrictedFeedMembers(ctx, userRef)
	if err != nil {
		if h.Logger != nil {
			h.Logger.Warn("posts: feed preference lookup failed; serving the default feed",
				"user_ref", userRef, "err", err.Error())
		}
		return false
	}
	return show
}

// applyHideRestricted drops the placeholders from one page of
// ALREADY-ENRICHED posts, returning the posts that survive.
//
// Since #921 this runs for EVERY caller who has not opted back into
// seeing them — it is the default feed rather than a minority
// preference. Nothing about the function changed; what changed is which
// branch of the caller in ListPosts is the common one. The name still
// describes what it does, which is hide restricted members.
//
// # It composes with the read rule; it never re-states it
//
// The only thing it reads is `PostMember.Restricted`, and that flag is
// written in exactly one place: enrichPreview, off the single
// visibility.FieldsReadable call that also decides preview_available,
// ladder_available and scrub_available (handler.go, "ONE readability
// decision"). So "which members can this caller see" is a field read
// here rather than a second evaluation. A second expression of the read
// rule is the defect class epic #665 exists for, and #892 and #904 each
// spent a sprint deleting one; this function is deliberately incapable
// of disagreeing with the rule because it does not know the rule.
//
// It also cannot WIDEN anything. `restricted: true` members carry no
// `asset` at all — the payload was withheld upstream, not blanked — so
// the most this can do is decline to render a placeholder. Skipping this
// call — which is what `show_restricted` does — restores the rule's own
// output and cannot exceed it, and running it can only ever remove
// things from that output. Neither direction touches who may read what.
//
// # The three rules
//
//  1. Restricted members are dropped from `members`.
//  2. A post left with no visible members is dropped from the page —
//     but ONLY if it had members to begin with. A post with no members
//     at all (an article, ADR 0073) was never showing the caller
//     something they couldn't see, so there is nothing here to hide;
//     collapsing "had members, none visible" into "has no members" is a
//     real off-by-one and it is pinned by a test.
//  3. A post the caller AUTHORED is never dropped, whatever its members
//     look like. A post can carry other people's assets — its author
//     can be exactly the person who cannot read them — so rule 2 alone
//     would make an author's own work disappear from their own feed
//     because of a display preference. Their members are still filtered
//     (rule 1 is about what you can see, and that does not change
//     because you wrote the caption); the post itself stays, and
//     PostHost keeps its edit/delete/manage-access menu on a memberless
//     post as of #918.
//
// Mutation safety: enrichPreview has already replaced every post's
// Members with a fresh slice detached from the cross-caller cache, so
// re-slicing it here cannot write into another caller's response. Do
// not call this before enrichPreview — it would both read an unwritten
// Restricted flag and alias the cache.
func applyHideRestricted(items []openapi.Post, callerRef int64) []openapi.Post {
	// A fresh slice rather than the usual filter-in-place: ListPosts
	// still holds a []*openapi.Post of pointers INTO `items` from the
	// enrich pass, and compacting the backing array under them would
	// leave those pointing at the wrong posts.
	out := make([]openapi.Post, 0, len(items))
	for _, p := range items {
		hadMembers := len(p.Members) > 0
		visible := make([]openapi.PostMember, 0, len(p.Members))
		for _, m := range p.Members {
			if !m.Restricted {
				visible = append(visible, m)
			}
		}
		p.Members = visible
		if hadMembers && len(visible) == 0 && p.AuthorUserRef != callerRef {
			continue
		}
		out = append(out, p)
	}
	return out
}
