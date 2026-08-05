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
	HideRestrictedFeedMembers(ctx context.Context, userRef int64) (bool, error)
}

// SetFeedFilters installs the per-user browse-filter reader (#891).
// Post-construction setter, same shape as SetNotifier / SetMentions.
//
// nil-safe, and nil means "no filtering": a test or a boot-order slip
// gets the pre-#891 feed rather than a panic or a silently-shortened
// page. That is the correct direction to degrade in for a filter that
// can only SUBTRACT — the redaction that protects the content already
// happened in enrichPreview, and this only decides whether the reader
// is shown the redaction or spared it.
func (h *Handler) SetFeedFilters(r feedFilterReader) { h.feedFilters = r }

// hideRestricted resolves the caller's #891 preference for one feed
// page. Errors are logged and answered `false` for the same reason the
// nil seam is: the filter is a display preference, so failing toward
// "show everything" shows the caller nothing they were not already
// entitled to.
func (h *Handler) hideRestricted(ctx context.Context, userRef int64) bool {
	if h.feedFilters == nil {
		return false
	}
	hide, err := h.feedFilters.HideRestrictedFeedMembers(ctx, userRef)
	if err != nil {
		if h.Logger != nil {
			h.Logger.Warn("posts: feed filter lookup failed; showing unfiltered",
				"user_ref", userRef, "err", err.Error())
		}
		return false
	}
	return hide
}

// applyHideRestricted drops what the caller asked not to be shown from
// one page of ALREADY-ENRICHED posts, returning the posts that survive.
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
// the most this can do is decline to render a placeholder. Turning the
// preference on can only ever remove things from a response.
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
