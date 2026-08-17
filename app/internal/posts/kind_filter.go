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

// coverAssetSQL resolves ONE post's cover asset id, as a scalar
// expression correlated to the feed query's `posts` row.
//
// It restates the card's own choice — `post.cover_asset_id ?? the first
// member` (PostCard.svelte's `coverAssetId`) — because the filter has
// to select the asset whose badge the reader will actually see. A post
// with no explicit cover still draws a badge, off its first member, so
// filtering only on `cover_asset_id` would have made every such post
// unreachable by any kind while still showing a kind.
//
// `ORDER BY sort_order, added_at` and the `deleted_at IS NULL` join are
// ListPostAssets' ordering and filter, not a second opinion about what
// "first member" means: that query is what populates `post.members`, so
// its first row is the one the card resolves against.
const coverAssetSQL = `COALESCE(posts.cover_asset_id, (
             SELECT pa.asset_id
               FROM post_assets pa
               JOIN assets fa ON fa.id = pa.asset_id
              WHERE pa.post_id = posts.id AND fa.deleted_at IS NULL
              ORDER BY pa.sort_order ASC, pa.added_at ASC
              LIMIT 1))`

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
// # The readability conjunct is not optional
//
// visibility.FieldsReadableSQL is the SQL twin of the exact Go call
// enrichPreview makes to decide `PostMember.Restricted`, so "this
// cover matched" and "this cover's badge is drawn" are one decision.
// Dropping it would turn the filter into an oracle for a value the card
// deliberately withholds: a restricted cover shows no kind badge and no
// extension band, and a filter that could still select the post lets a
// reader recover the kind by asking for each one in turn. That is the
// derived-copy defect class of #902/#1066, arriving through a new
// channel — so the channel carries the rule with it.
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

	return `
  AND EXISTS (SELECT 1 FROM assets ca
                WHERE ca.id = ` + coverAssetSQL + `
                  AND ca.deleted_at IS NULL
                  AND (` + strings.Join(arms, `
                       OR `) + `)` + readable + `)`, args
}
