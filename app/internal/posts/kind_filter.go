// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"fmt"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/viewkind"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// kindFilterSQL renders the `?kind=` conjunct for the feed query
// (#1166), beginning with " AND …" so callers concatenate it into an
// existing WHERE clause, plus the arguments it binds.
//
// `argOffset` is how many placeholders the caller has already bound —
// ADR 0063's discipline, the same contract readRuleSQL works under.
// Only placeholders the emitted text actually REFERENCES are bound: an
// unreferenced parameter is a 42P18 ("could not determine data type"),
// not a harmless extra, which is the trap the mature conjunct above the
// call site documents at length.
//
// # ANY MEMBER THE CALLER CAN READ, not the cover (#1190)
//
// A post matches kind K when ANY of its members that this caller may
// read resolves to K. The owner's ruling: "a post containing an ebook
// matches the ebook filter, cover or not". #1166 shipped the cover-only
// reading and it answered the wrong question — a five-file art drop
// whose first image happens to be the cover was unreachable by every
// kind it actually contains, so a reader looking for the epub inside it
// got an empty wall while the epub sat in the post.
//
// The membership is `post_assets`, and the EXPLICIT COVER IS NOT ADDED
// BESIDE IT even though `posts.cover_asset_id` can name a non-member
// (nothing in CreatePost/UpdatePost requires the cover to be attached —
// only that the caller may read it). Widening to it would be widening
// past the card: PostCard resolves its cover as
// `post.cover_asset_id ?? members[0]` and then LOOKS IT UP IN
// `post.members`, so a cover that is not a member yields no
// `coverAsset`, no kind badge and no extension band. Selecting a post
// by a fact its card cannot draw is the disagreement this filter's
// whole test suite is built to catch. Members are what the reader can
// see; members are what the filter selects on.
//
// # The shape of the predicate
//
// The kind of an asset is `asset_type` override first, extension
// second (viewkind.ForAsset). In SQL that is two arms:
//
//  1. the asset carries one of the OVERRIDING refs whose kind was
//     selected — extension irrelevant;
//  2. the asset carries NO overriding ref, and its extension resolves
//     into the selection.
//
// Arm 2's "no overriding ref" half is why [viewkind.OverrideRefs]
// exists separately from the selected ones: without it, a sprite atlas
// (a PNG with ref 13) would match `?kind=image` through its extension
// even though its badge says sprite.
//
// # The readability conjunct is not optional, and it lives PER MEMBER
//
// visibility.FieldsReadableSQL is the SQL twin of the exact Go call
// enrichPreview makes to decide `PostMember.Restricted`, so "this
// member matched" and "this member's kind is drawable" are one
// decision. It sits INSIDE the per-member EXISTS, beside the arms, so
// widening the search from the cover to the whole membership does not
// widen what may be probed: an unreadable member is not a candidate at
// all.
//
// Dropping it — or hoisting it out to the post — would turn the filter
// into an oracle for a value the card deliberately withholds: a
// restricted member shows no kind and no extension anywhere on the
// card, and a filter that could still select the post lets a reader
// recover that member's kind by asking for each kind in turn. That is
// the derived-copy defect class of #902/#1066, arriving through a new
// channel — so the channel carries the rule with it. #1190 widens WHICH
// assets are looked at and changes nothing about WHICH may be looked at.
//
// It cannot widen: it is a conjunct INSIDE an EXISTS that is itself a
// conjunct, and the whole fragment only ever removes rows.
func kindFilterSQL(id *auth.Identity, sel viewkind.Selection, argOffset int) (string, []any) {
	// A selection nothing can satisfy — `?kind=sequence`, or a name
	// that is not a kind at all. It has to be a never-satisfied
	// conjunct rather than no conjunct; see ListPostsPageParams.Kinds.
	if sel.Empty() {
		return "\n  AND FALSE", nil
	}

	var args []any
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", argOffset+len(args))
	}

	var arms []string
	if len(sel.AssetTypeRefs) > 0 {
		arms = append(arms, `ca.asset_type = ANY(`+bind(sel.AssetTypeRefs)+`::BIGINT[])`)
	}
	if len(sel.Extensions) > 0 || sel.IncludePlaceholder {
		// The frontend normalises with `toLowerCase().replace(/^\./,'')`;
		// this is that expression in SQL. Written once, into a name, so
		// the positive and the placeholder halves cannot normalise
		// differently.
		const normExt = `regexp_replace(lower(ca.file_extension), '^\.', '')`

		var extArms []string
		if len(sel.Extensions) > 0 {
			extArms = append(extArms, normExt+` = ANY(`+bind(sel.Extensions)+`::TEXT[])`)
		}
		if sel.IncludePlaceholder {
			// "Resolves to nothing" — the badge's blank-page glyph — is
			// the complement of the resolver's whole vocabulary, so it
			// is expressed as one, not as a hand-listed set that would
			// silently stop agreeing the moment an extension is added.
			extArms = append(extArms,
				`(ca.file_extension IS NULL OR `+normExt+` <> ALL(`+bind(viewkind.KnownExtensions())+`::TEXT[]))`)
		}
		arms = append(arms, `((ca.asset_type IS NULL
                OR ca.asset_type <> ALL(`+bind(viewkind.OverrideRefs())+`::BIGINT[]))
               AND (`+strings.Join(extArms, `
                 OR `)+`))`)
	}

	caller := visibility.NewCaller(nil)
	var caps visibility.ContentCaps
	var mut visibility.AssetMutationCaps
	var callerRef int64
	if id != nil {
		callerRef = id.UserRef
		caller = visibility.NewCaller(&id.UserRef)
		check := func(code string) bool { return id.Can(code) }
		caps = visibility.ResolveContentCaps(check)
		mut = visibility.ResolveAssetMutationCaps(check, id.ScopedTeams(visibility.AssetsAdmin))
	}
	// The ref is bound only when the fragment names it — an admin whose
	// capabilities short-circuit the rule gets the empty string back,
	// and a parameter no statement mentions is a 42P18.
	readable := visibility.FieldsReadableSQL("ca", fmt.Sprintf("$%d", argOffset+len(args)+1), caller, caps, mut)
	if readable != "" {
		args = append(args, callerRef)
	}

	// The membership join, not a correlated scalar. `deleted_at IS NULL`
	// is ListPostAssets' own filter, so the assets considered here are
	// exactly the ones that reach `post.members` — a soft-deleted asset
	// is not a member of anything the reader can see.
	return `
  AND EXISTS (SELECT 1 FROM post_assets kpa
                JOIN assets ca ON ca.id = kpa.asset_id
               WHERE kpa.post_id = posts.id
                 AND ca.deleted_at IS NULL
                 AND (` + strings.Join(arms, `
                      OR `) + `)` + readable + `)`, args
}
