// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FieldsRow is one asset row reduced to exactly the columns
// [FieldsReadable] consults. Every surface that emits asset COLUMNS
// selects these five and nothing else has to be threaded through.
//
// IsTeamMember must already fold in "the asset is team-tier AND the
// caller belongs to THIS asset's team", same contract as
// [ContentReadable]'s parameter of the same name — the queries compute
// it with an EXISTS join in the same pass.
type FieldsRow struct {
	Sensitivity      string
	Status           string
	ProcessingStatus string
	OwnerUserRef     *int64
	IsTeamMember     bool

	// TeamID is the asset's `team_id`, nil when it has none. RAW data,
	// unlike IsTeamMember beside it: it is the scope
	// [AssetMutationCaps.MayMutate] matches a scoped `assets.admin`
	// grant against, and the answer depends on the CALLER's team set,
	// which no per-row SQL expression here has.
	TeamID *uuid.UUID

	// CallerMayMutate is the pre-computed answer to "may this caller
	// edit or delete this asset by capability" — #939 / ADR 0064.
	// Set it with [FieldsRow.ApplyMutationCaps]; the zero value is
	// false, which fails CLOSED (a surface that never sets it withholds
	// exactly as it did before the capability existed).
	CallerMayMutate bool
}

// ApplyMutationCaps fills [FieldsRow.CallerMayMutate] from the caller's
// resolved capabilities and this row's TeamID.
//
// One call per scanned row, immediately after the scan, at every
// surface — rather than each surface open-coding the match — so the
// team-less-asset trap documented on [AssetMutationCaps.MayMutate] has
// exactly one expression.
func (r *FieldsRow) ApplyMutationCaps(m AssetMutationCaps) {
	r.CallerMayMutate = m.MayMutate(r.TeamID)
}

// FieldsReadable decides whether a caller may receive an asset's
// COLUMNS — title, description, tags, metadata, file hash, extension,
// byte size, thumbhash, dimensions (#883, #899).
//
// # Which surfaces
//
// All of them. This started (#883) as the container-member rule and was
// named MemberReadable for it; #899 found the same leak on the two
// surfaces that reach an asset directly, so the rule is now
// surface-neutral and every path that serialises asset columns routes
// through it:
//
//   - a post member and a collection resource (#883);
//   - `GET /assets/{id}` and the asset browse list (#899);
//   - a search hit, a suggest completion and the asset facets (#899);
//   - IIIF presentation manifests.
//
// # The rule it enforces
//
// An asset you cannot OPEN must not hand you its metadata, and reaching
// it through a container must never WIDEN it. So this is the
// CONJUNCTION of the two planes an asset already lives under, evaluated
// for the same caller:
//
//   - the ROW plane — [Predicate.ToSQL] for EntityAsset, which for an
//     anonymous caller demands status='active' AND
//     processing_status='ready' AND sensitivity='public'; and
//   - the CONTENT plane — [ContentReadable], ADR 0064, which is the tier
//     rule: public admits everyone, team admits the asset's team, and
//     restricted / embargo / anything unrecognised admit only the owner
//     and the two capability holders.
//
// Conjunction, not a new rule: the columns are readable iff the caller
// could have reached that asset's row AND could have reached its bytes.
// That is what makes the direction one-way — no serialisation of an
// asset can be wider than the narrowest plane it sits under, and by
// construction cannot become wider when either plane is edited.
//
// # Why the CONTENT plane gates METADATA
//
// ADR 0064 decided that sensitivity gates content, not rows: a
// restricted asset stays LISTED so browse can show it as a placeholder
// with its owner's name, which is what makes "request access" (#881)
// mean anything. That decision is unchanged — this does not remove a
// single row from a single feed.
//
// What it changes is the PAYLOAD on the row that stays. The owner's
// rule (2026-08-03) is that a viewer who cannot see an item sees a
// placeholder, and *"the placeholder should never leak info. Not even
// title. Only the owner's name."* So the tier that gates the bytes
// gates the columns too. The result is strictly NARROWER than the row
// the predicate returned, which is the safe direction and is why it
// does not contradict 0064. The ADR 0020 amendment (2026-08-04) records
// the split explicitly: 0020 governs the IMAGE, this governs the
// FIELDS.
//
// # What is deliberately NOT closed
//
// EXISTENCE is still disclosed, by design — decision 1 of #899. A
// caller who can list an asset still learns that it is there, who owns
// it, and that they may not open it.
//
// What used to be open beside it, and is NOT any more (#902): this doc
// recorded that "a restricted asset's indexed text MATCHES a given
// search query, because the row is still returned by search — a
// determined caller can probe that oracle word by word", and called it a
// product call rather than a bug. It was a bug: every word this function
// removes from the payload came straight back through the `@@` channel,
// one token at a time. [FieldsReadableSQL] is the SQL twin that closes
// it and [AssetSearchMatchSQL] is the single fragment every full-text
// asset surface matches through. Note what did NOT change: the row is
// still LISTED by an unfiltered browse, still carrying its owner's name,
// so ADR 0064's placeholder stands. Only a TEXT QUERY stops matching it
// — and it stops matching EVERY text query equally, which is why the
// absence is not itself an oracle.
//
// # Fails closed
//
// Every unknown sensitivity value denies, inherited from
// [ContentReadable]; a NULL owner never matches; and the anonymous
// sentinel (UserRef 0) can never match an asset owned by ref 0, both
// guards inherited from the same place.
// # The mutation disjunct (#939, ADR 0064)
//
// ADR 0064 decided on 2026-08-06 that *"a capability that permits
// mutation confers FIELD-plane readability for the objects it governs.
// It never confers the binary plane."* So a team-scoped `assets.admin`
// holder sees the title they are editing, and still cannot download a
// restricted asset — nobody deletes a thing they were never shown, and
// an ADR 0010 capability grant still does not become a content-tier
// grant.
//
// That disjunct is HERE and deliberately NOT inside [ContentReadable].
// ContentReadable governs the BYTES: it backs CanReadContent and the
// binary handlers, and it has a SQL twin, [ContentReadableSQL], held to
// it by TestContentReadableSQL_MatchesGo. Putting the disjunct there
// would hand a mutation holder the originals of every restricted asset
// in their team — the exact coupling every amendment in ADR 0064 has
// avoided — and would have to be transcribed into the SQL twin to keep
// that test passing, which is how you would notice too late.
//
// It is also deliberately not in [PreviewReadable], which is the same
// conjunction WITHOUT this disjunct. See that function for why the
// picture does not follow the fields.
func FieldsReadable(row FieldsRow, caller Caller, caps CapabilityChecker) bool {
	if PreviewReadable(row, caller, caps) {
		return true
	}
	// The FIELD plane only. row.CallerMayMutate is the resolved
	// `assets.admin` answer for THIS asset's team — see
	// [FieldsRow.ApplyMutationCaps]. Zero value denies, so a surface
	// that has not been wired behaves exactly as it did before.
	return row.CallerMayMutate
}

// PreviewReadable decides whether a caller may receive an asset's
// PICTURE — the `thumbhash` blur-up placeholder, and the
// `preview_available` / `ladder_available` / `scrub_available` flags
// that tell a client a rendition is fetchable.
//
// It is [FieldsReadable] MINUS the mutation disjunct, and the two
// separated on 2026-08-06 (#939). Before that they were one function,
// because before that there was no way to pass one and fail the other.
//
// # Why the picture does not follow the fields
//
// ADR 0064's decision confers the field plane on a mutation holder and
// explicitly does NOT confer the binary plane, and it places the
// thumbnail on the BINARY side: the thumbhash is withheld precisely
// because *"a thumbhash IS a blur"* — it is a low-fidelity copy of the
// image, so shipping it to someone refused the original ships the
// original's content at lower resolution. The intended result is a
// RICHER PLACEHOLDER — real fields, no picture — not a readable asset.
//
// The three availability flags ride the same plane for a different
// reason: they are a promise the binary handlers must keep. Deriving
// them from the field plane would set them true for a caller whose
// /file and /variants requests are then refused by [ContentReadable],
// which is a 403 the client walks straight into.
//
// So every surface makes TWO decisions from one row: FieldsReadable for
// the columns, PreviewReadable for the picture and the flags. A surface
// that uses FieldsReadable for both silently hands a mutation holder
// the blur.
func PreviewReadable(row FieldsRow, caller Caller, caps CapabilityChecker) bool {
	// SystemAdmin (wildcard) and ContentReadAll (binary plane, #474)
	// short-circuit both planes, exactly as they do in ContentReadable.
	// ContentReadAll admitting METADATA as well as bytes is deliberate:
	// its whole purpose is a demo-viewer role that renders a
	// mostly-restricted catalogue, and a catalogue of placeholders is
	// not a rendered catalogue.
	if caps != nil && (caps(SystemAdmin) || caps(ContentReadAll)) {
		return true
	}
	// The owner reaches their own asset on every surface, at any tier
	// and in any workflow state — including a draft they are still
	// preparing.
	if !caller.IsAnonymous && row.OwnerUserRef != nil && *row.OwnerUserRef == caller.UserRef {
		return true
	}
	// The ROW plane's anonymous conjuncts. These mirror the anonymous
	// EntityAsset branch of Predicate.ToSQL; the third conjunct there
	// (sensitivity='public') is not repeated because ContentReadable
	// below decides the tier, and duplicating it would be a second
	// expression of one rule — the defect ADR 0063 exists to prevent.
	// Soft-delete is NOT here: a deleted asset is not a placeholder, it
	// is gone, and every caller's query drops those rows in SQL.
	if caller.IsAnonymous {
		if row.Status != "active" || row.ProcessingStatus != "ready" {
			return false
		}
	}
	// The CONTENT plane.
	return ContentReadable(row.Sensitivity, row.OwnerUserRef, caller, caps, row.IsTeamMember)
}

// FieldsReadableSQL is the SQL transcription of [FieldsReadable], as a
// WHERE-clause conjunct (it starts with " AND ", like
// [ContentReadableSQL], so a splice site concatenates it with no
// pre-processing). `alias` is the assets table alias ("" for none) and
// `callerArg` is the placeholder holding the caller's user_ref.
//
// # Why a SQL twin is sanctioned here (#902)
//
// [ContentReadableSQL]'s doc states the exception it was created under:
// a twin exists for the surfaces "which reduce many rows to one number
// or one string and so have no per-row Go step to decide readability
// in", and every other surface should keep calling the Go form. A
// FULL-TEXT MATCH is squarely that case, and more so than an aggregate:
// the `@@` operator decides in SQL whether the row is returned AT ALL,
// so a Go step downstream of it never sees the rows it needed to judge.
// Withholding the columns after the fact — which is all #899 could do —
// leaves the MATCH itself as a channel, and a matched-or-not answer is
// one bit of the withheld title per query.
//
// It is held to the Go form by TestFieldsReadableSQL_MatchesGo, the
// exhaustive twin of TestContentReadableSQL_MatchesGo. If you edit
// [FieldsReadable] or [PreviewReadable] and that test goes red, edit
// this — that is what it is for.
//
// # The three disjuncts, in the order [FieldsReadable] evaluates them
//
//  1. The capability short-circuit. system.admin / content.read.all
//     (from PreviewReadable) and a GLOBAL assets.admin (from the
//     mutation disjunct) all fold to an empty fragment rather than a
//     bound TRUE: the caller already resolved them, and a missing
//     conjunct lets Postgres plan as though the gate were not there.
//  2. The PREVIEW plane — the content tier, plus the anonymous-only
//     status conjuncts. The owner branch is not repeated separately:
//     ContentReadable's own first clause is the same comparison with
//     the same anonymous-sentinel guard, and stating it twice is how
//     the NULLIF trap gets fixed in one place and not the other. For an
//     ANONYMOUS caller the status conjuncts wrap the whole plane, which
//     matches PreviewReadable's early return — an anonymous caller can
//     never reach the owner branch there either, because of the
//     !IsAnonymous guard.
//  3. The MUTATION disjunct (#939, ADR 0064) — a team-scoped
//     assets.admin holder is owed the FIELDS of the assets they
//     administer, so they must keep matching them. The team set is
//     rendered as UUID LITERALS, not a bound array: these UUIDs came
//     from the auth resolver inside this process, never from caller
//     text, and threading an extra placeholder through six splice
//     sites' arg lists is where an off-by-one lives (the same call the
//     facet aggregators made for the caller ref). uuid.Nil entries are
//     DROPPED, mirroring [AssetMutationCaps.MayMutate], which refuses a
//     nil team scope rather than treating it as "no scope required".
//
// A row whose team_id IS NULL makes disjunct 3 evaluate to NULL rather
// than false; in a WHERE clause NULL and false are indistinguishable, so
// the SQL still agrees with the Go form's `teamID == nil → false`.
func FieldsReadableSQL(alias, callerArg string, caller Caller, caps ContentCaps, mut AssetMutationCaps) string {
	if caps.SystemAdmin || caps.ContentReadAll || mut.Global {
		return ""
	}
	p := columnPrefix(alias)

	preview := `(` + contentReadableCoreSQL(p, callerArg) + `)`
	if caller.IsAnonymous {
		preview = `(` + p + `status = 'active' AND ` + p + `processing_status = 'ready'
	       AND ` + preview + `)`
	}
	disjuncts := []string{preview}

	teams := make([]string, 0, len(mut.Teams))
	for _, t := range mut.Teams {
		if t == uuid.Nil {
			continue
		}
		teams = append(teams, `'`+t.String()+`'::UUID`)
	}
	if len(teams) > 0 {
		disjuncts = append(disjuncts, p+`team_id IN (`+strings.Join(teams, ", ")+`)`)
	}
	return ` AND (` + strings.Join(disjuncts, `
	       OR `) + `)`
}

// AssetSearchMatchSQL is the ONE expression of "this asset's indexed
// text matches this caller's query", and every full-text surface over
// `assets` composes its WHERE clause from it (#902).
//
// `tsqueryExpr` is the already-built right-hand side of the `@@`
// operator — `plainto_tsquery('english', $1)`, placeholder and all.
// Every remaining parameter is what [FieldsReadableSQL] needs.
//
// # Why this exists rather than six hand-written conjuncts
//
// Because there are six of them. `search_text @@ …` appears in the
// search hits query, the search COUNT, the browse page's `?q=`, and the
// facet aggregators, and #902 is precisely what happens when a security
// rule is spliced into a text match at one of those and not the others:
// a fix confined to /search leaves the identical word-by-word recovery
// available through /assets?q=. Six independently-edited copies of one
// rule is six chances for it to drift, which is ADR 0063's whole
// argument, so the column choice is made HERE and the splice sites only
// name their alias and their tsquery.
//
// # What the readable-side document is, and why the withheld side is
// empty
//
// The gate is a conjunct on the existing `search_text`, not a second
// reduced tsvector column, because the reduced document would be EMPTY.
// `rebuild_asset_search_text` builds the document out of exactly three
// things — title (weight A), description (B) and the `searchable`
// active field values (D) — and [FieldsReadable] withholds all three
// from a caller who fails it. There is no fourth ingredient that
// survives withholding, so a second column would be an all-empty
// tsvector, a second GIN index over nothing, and a fourth thing the two
// rebuild triggers have to keep in sync. `@@ AND readable` and
// `@@ reduced-document` return the identical row set; only the first
// costs nothing. If a genuinely public ingredient is ever added to the
// document (the owner's display name is the obvious candidate, since the
// placeholder already carries it), it belongs in that reduced column and
// this is the one function that has to learn about it.
func AssetSearchMatchSQL(alias, tsqueryExpr, callerArg string, caller Caller, caps ContentCaps, mut AssetMutationCaps) string {
	return `(` + columnPrefix(alias) + `search_text @@ ` + tsqueryExpr +
		FieldsReadableSQL(alias, callerArg, caller, caps, mut) + `)`
}

// OwnerDisplayNameSQL is the SQL transcription of
// [users.PlaceholderOwnerName], as a scalar SELECT-list expression that
// resolves the owner's name for a WITHHELD-asset placeholder.
// `ownerRefExpr` is whatever names the owning ref in the surrounding
// query (`assets.owner_user_ref`, `a.owner_user_ref`), and `anonymous`
// is the caller's [Caller.IsAnonymous].
//
// It always yields TEXT, never NULL: "" is the single "no name" answer,
// covering an ownerless asset, an owner row with nothing to render, and
// an owner who opted out of anonymous exposure alike. Every placeholder
// builder turns "" into an ABSENT key, so those three cases are
// indistinguishable on the wire — which is the point, since a client
// that could tell "withheld" from "empty" could read the opt-out off the
// difference.
//
// # Why a SQL twin is sanctioned here (#1023)
//
// Same exception [ContentReadableSQL] and [FieldsReadableSQL] were
// created under, for a different reason: the name is not a decision, it
// is a JOIN. Every surface that emits a placeholder is already reading
// the asset row, and the owner's name is the one asset-derived value the
// placeholder carries — so resolving it in Go afterwards means a second
// round trip per page on exactly the pages that have restricted rows,
// which is the N+1 [users.LookupAuthors] exists to avoid.
//
// Before #1023 there were THREE hand-written copies of this ladder — one
// here, one in posts' preview enrich, one in collections' resources page
// — and all three had the same two defects, because they were the same
// text pasted three times:
//
//   - they never consulted `hide_from_anonymous`, so an owner who took
//     ADR 0024's opt-out had their USERNAME rendered to an anonymous
//     caller on any public post or collection holding one of their
//     restricted assets. That is the opt-out defeated by a JOIN, which
//     is the exact wording of the rule in users/author.go; and
//   - they skipped the `fullname` rung ADR 0070 §3 gives an
//     AUTHENTICATED caller, so a signed-in caller saw a different name on
//     a placeholder than on the same user's post header.
//
// #557 created [users.ResolveDisplayName] because this rule had been
// transcribed once before and the copy dropped a rung. This is that
// again, so the copies are gone and the one that remains is held to the
// Go form by TestOwnerDisplayNameSQL_MatchesGo, which drives every rung
// through both. If you edit [users.PlaceholderOwnerName] and that test
// goes red, edit this.
//
// The subquery aliases are deliberately ugly (`odn_u`, `odn_p`): this
// fragment is spliced into queries that have their own `u` / `up` joins,
// and an alias collision here would resolve silently to the OUTER row.
func OwnerDisplayNameSQL(ownerRefExpr string, anonymous bool) string {
	// Rungs 1–3 of the ladder. `fullname` is rung 2 and is
	// AUTHENTICATED-ONLY (ADR 0070 §3) — an anonymous caller's ladder
	// skips straight from display_name to username, which is what stops
	// this leaking the real name of every user who never set a display
	// name. NULLIF on each rung so a row storing '' rather than NULL
	// falls through, matching the Go form's `!= ""` tests.
	name := `COALESCE(NULLIF(odn_p.display_name, ''), NULLIF(odn_u.fullname, ''), NULLIF(odn_u.username, ''))`
	if anonymous {
		// The ADR 0024 opt-out, and the missing rung 2. NULL, not '',
		// so the outer COALESCE produces the same "" an unresolvable
		// owner produces.
		name = `CASE WHEN COALESCE(odn_p.hide_from_anonymous, FALSE) THEN NULL
	                    ELSE COALESCE(NULLIF(odn_p.display_name, ''), NULLIF(odn_u.username, '')) END`
	}
	return `COALESCE((SELECT ` + name + `
	                   FROM "user" odn_u
	                   LEFT JOIN user_profiles odn_p ON odn_p.user_ref = odn_u.ref
	                  WHERE odn_u.ref = ` + ownerRefExpr + `), '')`
}

// FieldsColumnsSQL is the SELECT-list fragment carrying exactly the
// columns [FieldsRow] needs, plus the owner's display name for the
// placeholder. `alias` is the assets table alias ("" for none),
// `callerArg` is the placeholder holding the caller's user_ref for the
// team-membership EXISTS, and `caller` is the caller itself — the owner
// name is resolved differently for an anonymous one, see
// [OwnerDisplayNameSQL].
//
// One fragment rather than five hand-copied column lists: a surface
// that selects four of the five silently decides the fifth, and the
// fifth is usually sensitivity.
//
// Column order, which callers scan positionally:
//
//	sensitivity, status, processing_status, owner_user_ref,
//	team_id, is_team_member, owner_display_name
//
// `team_id` is RAW, not a decided answer like `is_team_member` beside
// it, and it is the one column here that is not decidable in SQL: it
// gets matched in Go against the caller's closure-expanded
// `assets.admin` team set (see [AssetMutationCaps]), which the auth
// resolver computed at request time from role inheritance, grants,
// revokes and `team_closure` together. Re-deriving that here would be a
// second, narrower expression of the capability resolver.
//
// `caller` is a whole [Caller] and not a bare `anonymous bool` so that a
// splice site cannot pass the wrong one: every one of them already holds
// the Caller it hands to [FieldsReadable] on the way back out, and the
// two answers must be about the same caller.
func FieldsColumnsSQL(alias, callerArg string, caller Caller) string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	return p + `sensitivity, ` + p + `status, ` + p + `processing_status, ` + p + `owner_user_ref,
	       ` + p + `team_id,
	       (` + p + `team_id IS NOT NULL AND EXISTS (
	            SELECT 1 FROM team_memberships tm
	             WHERE tm.team_id = ` + p + `team_id AND tm.user_ref = ` + callerArg + `::BIGINT)) AS is_team_member,
	       ` + OwnerDisplayNameSQL(p+`owner_user_ref`, caller.IsAnonymous) + ` AS owner_display_name`
}

// FieldsPool is the subset of pgxpool.Pool LoadFieldsRow uses.
type FieldsPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadFieldsRow reads one asset's [FieldsRow] plus its owner's display
// name, resolving team membership for the caller in the same round
// trip. It is the pool-loading sibling of the query-free
// [FieldsReadable], exactly as [CanReadContent] is to
// [ContentReadable]: surfaces that already have the columns (the browse
// list, the search projections) call FieldsReadable directly; surfaces
// that reach a single asset call this.
//
// Fails CLOSED — a missing row returns the zero FieldsRow and an error,
// and the zero FieldsRow denies on every tier, because an asset we
// cannot load is an asset whose columns we do not hand out.
// `mut` is the caller's resolved asset-mutation capabilities (#939);
// pass the zero value for a surface that does not honour them, which
// denies. It is a parameter rather than something the query resolves
// because the team set lives on the caller's Identity, which this
// package deliberately cannot see.
func LoadFieldsRow(
	ctx context.Context,
	pool FieldsPool,
	caller Caller,
	assetID uuid.UUID,
	mut AssetMutationCaps,
) (FieldsRow, string, error) {
	var (
		row       FieldsRow
		ownerName string
	)
	err := pool.QueryRow(ctx,
		`SELECT `+FieldsColumnsSQL("assets", "$2", caller)+` FROM assets WHERE id = $1`,
		assetID, caller.UserRef,
	).Scan(&row.Sensitivity, &row.Status, &row.ProcessingStatus,
		&row.OwnerUserRef, &row.TeamID, &row.IsTeamMember, &ownerName)
	if err != nil {
		return FieldsRow{}, "", fmt.Errorf("visibility.LoadFieldsRow: %w", err)
	}
	row.ApplyMutationCaps(mut)
	return row, ownerName, nil
}
