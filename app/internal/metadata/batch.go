// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// BATCH METADATA EDIT — the domain core (#1173, #1119, ADR 0019).
//
// One field, one proposed value, one mode, many assets. This file holds
// what the operation IS; batch_preview.go and batch_apply.go hold the
// two endpoints that use it, and batch_token.go holds the binding that
// ties them together.
//
// # The four questions the single-target writer never had to answer
//
// SetAssetFieldValue is an eleven-stage pipeline over ONE asset, and
// every stage of it is correct. Four things simply never came up:
//
//  1. WHICH assets. The shipped selection store is a bare list of ids
//     with no kind discriminator, so the client cannot say whether an
//     id is an asset or a post. The batch takes a TYPED selection and
//     expands posts itself.
//  2. WAS THE PREVIEWED SET THE APPLIED SET. Nothing in the repo had a
//     preview of any kind, so there was nothing to answer with.
//  3. MAY THIS CALLER EDIT THIS ASSET. Nothing in this package asks —
//     there is no OwnerRef, no owner_ref and no MayMutate anywhere in
//     it. The first-class asset-column plane acquired an
//     ownership-and-team subject gate (assets.canMutateAsset); the
//     ordinary metadata field-value plane did not. That is the whole
//     claim and it is not extended into a causal story here.
//     ⛔ The single-target endpoints are NOT changed by this sprint.
//     The batch asks the question because a thousand-record operation
//     that does not is a different order of problem; the ordinary
//     writer's gap is tracked separately.
//  4. THE OPERATION IS ITSELF A READ ORACLE. A batch that reported
//     "40 of your 100 selected assets already hold this value" would
//     answer a question about fields the caller may not read, forty at
//     a time. So readability is a GATE on the write path here, not
//     only on the read path.
//
// # The five gates, and what each is for
//
// They run in this order, all AFTER the caller has been proved to own
// their token, and the value is not inspected until every one of them
// has had its say:
//
//	G1  bulk instrument in the target's scope   per target
//	G2  ordinary subject authority              per target
//	G3  applicability                           per target
//	G4  the field's own write_capability        batch-wide
//	G5  the field's read_capability             per target
//	--- THE STORED-VALUE LINE ------------------------------------
//	    value inspection                        no_op / refused / would_change
//
// Neither the bulk capability nor the subject rule REPLACES the field's
// own write gate, and the field's own write gate does not replace them.
// A caller holding `assets.metadata.bulk_edit` globally who does not
// own a team-less asset and holds no `assets.admin` still cannot write
// it, and a caller who owns every asset in the selection still cannot
// write a field whose `write_capability` they lack.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/richtext"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// CapBulkEdit is the batch editor's INSTRUMENT capability (migration
// 00066). TEAM-SCOPE-AWARE, unlike the two vocabulary capabilities: a
// bulk edit's blast radius is its selection, so an operator trusted
// with one team's catalogue is not thereby trusted with another's.
const CapBulkEdit = "assets.metadata.bulk_edit"

// The two ceilings (ADR 0019, section 15 of the sprint contract).
//
// batchSelectionEntryCeiling is checked BEFORE any membership query
// runs, so a selection nobody could have meant costs one comparison
// rather than a five-hundred-post expansion.
//
// batchExpandedTargetCeiling is the AUTHORITATIVE one: it bounds what
// actually gets written, which is what the latency budget, the audit
// envelope's size and the batch-wide guards' hold time are all sized
// against. When a SINGLE post's membership exceeds it on its own the
// ceiling is still absolute — no partial expansion, no trimming, no
// "the first thousand". A batch that quietly wrote a different set than
// the operator selected would be a worse failure than refusing.
const (
	batchSelectionEntryCeiling = 500
	batchExpandedTargetCeiling = 1000
)

// batchMode is the operator's verb. Four of them, and deliberately not
// five: there is no batch CLEAR (see clearIsNotAMode below).
type batchMode = openapi.BatchAssetFieldMode

const (
	modeOverwrite   = openapi.BatchModeOverwrite
	modeFillEmpties = openapi.BatchModeFillEmpties
	modeAppend      = openapi.BatchModeAppend
	modeRemove      = openapi.BatchModeRemove
)

// THERE IS NO BATCH CLEAR, and the absence is a decision rather than an
// omission — which is worth saying because it is the kind of gap a
// later reader closes on sight.
//
// An empty OVERWRITE and a CLEAR are different operations, not two
// spellings of one. An empty overwrite is a SET: the row exists
// afterwards, its `set_at` advances, a history row is written, and R1's
// requiredSetRefusal governs it. A clear REMOVES the row and R2's
// requiredClearRefusal governs it. 20c has no mode that does the latter
// — with exactly one exception, `remove` emptying an OPTIONAL
// multi_select, which deletes the row because writing `[]` into
// value_options is a shape the single-target writer refuses.
//
// The consequence worth stating out loud: NO MODE CREATES AN ACCIDENTAL
// EMPTY ROW. fill_empties cannot, because a semantically empty proposed
// value is refused batch-wide before any target is touched; append and
// remove are multi_select-only; and overwrite creates one only where an
// operator explicitly supplied an empty value for an optional field,
// which is a thing they asked for.

// validateBatchMode refuses a mode the enum does not define.
//
// ⚠️ NOTHING ELSE DOES THIS. There is no spec-validation middleware in
// front of these handlers and the generated `BatchAssetFieldMode` is a
// bare string type whose `Valid()` nothing calls, so an unrecognised
// mode arrives here intact. Left unchecked it was worse than cosmetic:
// it matched none of the mode-specific arms, so a REQUIRED field could
// be given a semantically empty value without tripping R1, every target
// fell through the partition switch as no_op, and the request then died
// on the preview table's CHECK constraint as a 500.
//
// A 400: an undefined mode could not have been valid for any state of
// the system.
func validateBatchMode(mode batchMode) error {
	switch mode {
	case modeOverwrite, modeFillEmpties, modeAppend, modeRemove:
		return nil
	}
	return refuse(400, openapi.BatchUnknownMode,
		"%q is not one of overwrite, fill_empties, append, remove", string(mode))
}

// appendRemoveSupported reports whether a field type has a set
// semantics for `append` and `remove`.
//
// EXACTLY ONE type does. The other ten — text, longtext, rich_text,
// select, tree, number, boolean, date, datetime, reference — are
// refused batch-wide with 422 mode_not_supported_for_type rather than
// being given an invented one. "Append to a number" and "remove from a
// date" have no meaning that two people would agree on, and inventing
// one to make four mode names look uniform across eleven types is how
// an operator ends up with a catalogue full of concatenated dates.
func appendRemoveSupported(fieldType string) bool {
	return fieldType == "multi_select"
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

// batchRefusal is a refusal with its wire status, travelling as an
// error so a deeply nested check can decline without every layer
// between having a return path for it.
type batchRefusal struct {
	Status     int
	Reason     openapi.BatchAssetFieldRefusalReason
	Message    string
	Field      *string
	Option     *string
	Expected   *int
	Actual     *int
	EntryCount *int
}

func (r *batchRefusal) Error() string { return r.Message }

func refuse(status int, reason openapi.BatchAssetFieldRefusalReason, format string, args ...any) *batchRefusal {
	return &batchRefusal{Status: status, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func (r *batchRefusal) withField(code string) *batchRefusal {
	c := code
	r.Field = &c
	return r
}

func (r *batchRefusal) withOption(slug string) *batchRefusal {
	s := slug
	r.Option = &s
	return r
}

func (r *batchRefusal) body() openapi.BatchAssetFieldRefusal {
	return openapi.BatchAssetFieldRefusal{
		Error:      r.Message,
		Reason:     r.Reason,
		Field:      r.Field,
		Option:     r.Option,
		Expected:   r.Expected,
		Actual:     r.Actual,
		EntryCount: r.EntryCount,
	}
}

func asBatchRefusal(err error) (*batchRefusal, bool) {
	var r *batchRefusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Selection and expansion
// ---------------------------------------------------------------------------

// batchExpansion is what a typed selection resolved to.
type batchExpansion struct {
	// TargetIDs is DISTINCT and ordered BY ASSET ID. Not by selection
	// order (client-supplied, and a client that reorders its selection
	// would derive a different set from the same intent) and not by
	// any sort_order (mutable, so the same selection could expand
	// differently a second later). Asset id is the only ordering both
	// endpoints can derive identically from the same inputs, which is
	// the property the whole preview-then-apply contract rests on.
	TargetIDs []uuid.UUID

	// EmptyPosts are selected posts that hold no live members. They
	// contribute nothing, and they are REPORTED rather than dropped: a
	// post that yields no targets is a thing the operator should see,
	// not a silent subtraction from a count they are about to confirm.
	EmptyPosts []uuid.UUID

	EntryCount int
}

// expandSelection resolves a typed selection to the ordered distinct
// target set, SERVER-SIDE.
//
// The entry ceiling is checked FIRST, before a single membership query
// runs, so an absurd selection costs one comparison. The expanded
// ceiling is checked after expansion and before anything is done with
// the result — including the case where ONE post's membership exceeds
// it alone, which is refused on exactly the same terms.
func (h *Handler) expandSelection(
	ctx context.Context,
	q *Queries,
	id *auth.Identity,
	entries []openapi.BatchAssetFieldSelectionEntry,
) (batchExpansion, error) {
	out := batchExpansion{EntryCount: len(entries)}

	if len(entries) == 0 {
		return out, refuse(400, openapi.BatchEmptySelection,
			"the selection is empty; there is nothing to preview")
	}
	if len(entries) > batchSelectionEntryCeiling {
		r := refuse(422, openapi.BatchSelectionEntryCeiling,
			"a batch may name at most %d selection entries; this one names %d",
			batchSelectionEntryCeiling, len(entries))
		limit, actual := batchSelectionEntryCeiling, len(entries)
		r.Expected, r.Actual = &limit, &actual
		return out, r
	}

	var postIDs []pgtype.UUID
	var assetIDs []pgtype.UUID
	seen := make(map[uuid.UUID]struct{}, len(entries))
	targets := make([]uuid.UUID, 0, len(entries))

	for _, e := range entries {
		id := uuid.UUID(e.Id)
		switch e.Kind {
		case openapi.BatchSelectionAsset:
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			targets = append(targets, id)
			assetIDs = append(assetIDs, pgtype.UUID{Bytes: id, Valid: true})
		case openapi.BatchSelectionPost:
			postIDs = append(postIDs, pgtype.UUID{Bytes: id, Valid: true})
		default:
			// Its OWN reason, not empty_selection: the selection was
			// not empty, and telling a client it was describes a
			// different fact from the one that refused it.
			return out, refuse(400, openapi.BatchUnknownSelectionKind,
				"selection entry kind %q is not one of asset, post", string(e.Kind))
		}
	}

	if len(postIDs) > 0 {
		// ⛔ EXPANSION IS A READ, AND IT NEEDS THE POST'S OWN READ GATE.
		//
		// Without this, a caller holding the bulk instrument ANYWHERE —
		// a grant scoped to one team is enough to pass admission — could
		// name any post id and get its member asset ids back in
		// `targets`, each politely labelled `unauthorized`, plus its
		// non-emptiness in `empty_posts` and its size in
		// `counts.expanded`. The asset-existence oracle is closed
		// (absent and out-of-scope answer alike); membership of a post
		// the caller may not even see was not, which contradicts the
		// reason admission is asked BEFORE expansion in the first place.
		//
		// visibility.PostReadable is the SHIPPED single-post gate,
		// obtained rather than restated — it consults `visibility`,
		// `post_acls`, drafts and `posts.admin`, and a second copy of
		// that would drift. A post the caller cannot read contributes
		// NOTHING and is not reported: not as a target, and not as an
		// empty post, because "your selection reached nothing" and "you
		// may not see that" must look the same from outside.
		readable, err := h.readablePosts(ctx, id, postIDs)
		if err != nil {
			return out, err
		}
		postIDs = readable

	}

	// ── THE CEILING, COUNTED BEFORE ANYTHING IS MATERIALISED ───────
	//
	// Two separable requirements pull in opposite directions, and both
	// are met here rather than one at the other's expense.
	//
	// The refusal must name the TRUE distinct expanded count. A bounded
	// read alone cannot: `LIMIT 1001` reports 1001 whether the
	// selection reaches 1,001 assets or fifty thousand, and an operator
	// told the smaller number would trim one post at a time towards a
	// target that was never within reach.
	//
	// And the server must not pull an unbounded id set into
	// application memory to find that out. So the COUNT is computed in
	// the database, where the set never leaves, and the ids are read
	// only once the count is known to fit — still bounded, because a
	// bound that is only ever redundant is still the thing that makes
	// the memory claim true rather than incidental.
	//
	// Counted over BOTH halves of the selection at once, because the
	// distinct total is not the sum of the parts: an asset named
	// directly and also reached through two posts is one target.
	total, err := q.CountBatchExpandedTargets(ctx, CountBatchExpandedTargetsParams{
		AssetIds: assetIDs,
		PostIds:  postIDs,
	})
	if err != nil {
		return out, fmt.Errorf("metadata: count expanded targets: %w", err)
	}
	if total > int64(batchExpandedTargetCeiling) {
		r := refuse(422, openapi.BatchExpandedTargetCeiling,
			"a batch may reach at most %d distinct assets; these %d selection entries reach %d",
			batchExpandedTargetCeiling, len(entries), total)
		limit, actual, entryCount := batchExpandedTargetCeiling, int(total), len(entries)
		r.Expected, r.Actual, r.EntryCount = &limit, &actual, &entryCount
		// NO PARTIAL EXPANSION reaches a partition or an apply: the
		// refusal returns before a single target id is read.
		return out, r
	}

	if len(postIDs) > 0 {
		// Bounded at the ceiling PLUS ONE. Redundant now that the count
		// above has already refused anything larger — and kept anyway,
		// because "this read is bounded" should be a property of the
		// read rather than of the check that happens to precede it.
		members, err := q.ExpandPostsToAssets(ctx, ExpandPostsToAssetsParams{
			PostIds: postIDs,
			Limit:   int32(batchExpandedTargetCeiling + 1),
		})
		if err != nil {
			return out, fmt.Errorf("metadata: expand posts: %w", err)
		}
		for _, m := range members {
			id := uuid.UUID(m.Bytes)
			if _, dup := seen[id]; dup {
				// An asset reachable through two selected posts is ONE
				// target and is written ONCE. The measured corpus has
				// assets in as many as ten posts, so this is the
				// ordinary case rather than a defensive one, and
				// double-writing would put two history rows on one
				// asset for one operator action.
				continue
			}
			seen[id] = struct{}{}
			targets = append(targets, id)
		}

		withMembers, err := q.ListPostsWithMembers(ctx, postIDs)
		if err != nil {
			return out, fmt.Errorf("metadata: post membership: %w", err)
		}
		have := make(map[uuid.UUID]struct{}, len(withMembers))
		for _, p := range withMembers {
			have[uuid.UUID(p.Bytes)] = struct{}{}
		}
		reported := make(map[uuid.UUID]struct{}, len(postIDs))
		for _, raw := range postIDs {
			p := uuid.UUID(raw.Bytes)
			if _, ok := have[p]; ok {
				continue
			}
			if _, dup := reported[p]; dup {
				continue
			}
			reported[p] = struct{}{}
			out.EmptyPosts = append(out.EmptyPosts, p)
		}
	}

	sort.Slice(targets, func(i, j int) bool {
		return targets[i].String() < targets[j].String()
	})
	out.TargetIDs = targets
	return out, nil
}

// ---------------------------------------------------------------------------
// The five gates
// ---------------------------------------------------------------------------

// batchSubject is one target's authority-relevant state.
type batchSubject struct {
	ID        uuid.UUID
	OwnerRef  *int64
	TeamID    *uuid.UUID
	AssetType int64
	Live      bool
}

// bulkAdmitted is G1's BATCH-WIDE half, asked before expansion.
//
// A caller with no holding of the instrument anywhere — neither global
// nor scoped to any team — is refused before a single membership query
// runs. Asking it early is not an optimisation: expanding a selection
// for a caller who cannot act on any of it would answer a question
// about post membership for somebody with no business asking.
//
// `Can(cap) || len(ScopedTeams(cap)) > 0` and not `Can(cap)` alone,
// because ScopedTeams deliberately excludes global holdings and the
// system.admin wildcard while Can covers both. Neither is the whole
// answer on its own.
func bulkAdmitted(id *auth.Identity) bool {
	return id != nil && (id.Can(CapBulkEdit) || len(id.ScopedTeams(CapBulkEdit)) > 0)
}

// bulkScopeCovers is G1's PER-TARGET half.
//
// ⚠️ THE TEAM-LESS TRAP. `assets.team_id` is nullable, and a team-less
// asset has no scope for a scoped grant to match. It must NEVER be read
// as "no scope is required here, therefore anybody passes" — that is
// the exact inversion visibility.MayMutate documents, and here it would
// hand every scoped grant-holder authority over every asset that
// happens to have no team. Only a GLOBAL holding reaches one.
//
// The wildcard is not spelled out. `Can` short-circuits on
// system.admin before any scope work, so writing
// `Can(x) || Can("system.admin")` would be a second, drifting copy of
// a rule the checker already has.
func bulkScopeCovers(id *auth.Identity, teamID *uuid.UUID) bool {
	if id == nil {
		return false
	}
	if teamID == nil || *teamID == uuid.Nil {
		return id.Can(CapBulkEdit)
	}
	return id.Can(CapBulkEdit, auth.InTeam(*teamID))
}

// subjectAuthorised is G2 — the ORDINARY subject authority rule, the
// same one PATCH /assets/{id} asks through assets.canMutateAsset.
//
// It is obtained from visibility rather than restated, for the reason
// MayMutateOwned's own header gives: two statements of an authorisation
// rule is the defect, and this package cannot import the assets package
// (that one imports this one).
//
// An anonymous caller is refused outright before any comparison: user
// ref 0 is the anonymous sentinel and a bare `*ownerRef == UserRef`
// would hand ownership of every unowned asset to every visitor.
func subjectAuthorised(id *auth.Identity, s batchSubject) bool {
	if id == nil || id.IsAnonymous() {
		return false
	}
	caps := visibility.ResolveAssetMutationCaps(
		func(code string) bool { return id.Can(code) },
		id.ScopedTeams(visibility.AssetsAdmin),
	)
	return caps.MayMutateOwned(id.UserRef, s.OwnerRef, s.TeamID)
}

// fieldApplies is G3 — applicability, the field's applies_to against
// this asset's type.
//
// An EMPTY applies_to is UNIVERSAL, not "applies to nothing". That is
// the same reading ListAssetFieldValues' own SQL takes
// (`cardinality(f.applies_to) = 0 OR $2 = ANY(f.applies_to)`), and
// getting it backwards would make every unrestricted field
// inapplicable to everything.
//
// Its verdict is `inapplicable`, which is NOT an error. Selecting a
// mixed bag of assets and asking for a change to a field that covers
// only some of them is ordinary operator behaviour; the batch reports
// which ones it skipped.
func fieldApplies(f FieldDefinition, assetType int64) bool {
	if len(f.AppliesTo) == 0 {
		return true
	}
	for _, t := range f.AppliesTo {
		if t == assetType {
			return true
		}
	}
	return false
}

// effectiveWritePermission is G4 — the field's OWN write_capability,
// reproduced EXACTLY as the single-target writer enforces it.
//
// ⚠️ IT IS GLOBAL-ONLY, and that is deliberate here: the shipped rule
// at SetAssetFieldValue is `id.Can(*write_capability)` with NO InTeam,
// while the field's READ gate (fieldReadableOnSubject) IS team-scope
// aware. The asymmetry is REAL and it is NOT FIXED BY THIS SPRINT —
// the batch reproducing a different rule from the single-target writer
// would be two statements of one rule, which is the defect, and
// widening it here would quietly grant scoped holders a write they do
// not have on the ordinary endpoint.
//
// It is named for the CONCEPT rather than for its current scope so that
// when the asymmetry is fixed, the fix moves this to per-target without
// changing the batch's contract or its call sites.
//
// Resolves BATCH-WIDE: a field-level capability is a property of the
// field, not of any target, so failing it refuses the whole operation
// with 403 rather than partitioning every target as unauthorized.
func effectiveWritePermission(id *auth.Identity, f FieldDefinition) bool {
	if f.WriteCapability == nil || *f.WriteCapability == "" {
		return true
	}
	return id.Can(*f.WriteCapability)
}

// fieldReadableForBatch is G5 — the field's read_capability on this
// subject, and the anti-oracle boundary.
//
// # Why a WRITE path consults a READ gate
//
// Because the operation reports. A batch that told a caller "twelve of
// your targets already hold this value" would have answered twelve
// questions about a field they may not read, and it would have done it
// through a write endpoint where nobody was looking for a read gate.
// The same goes for emptiness (fill_empties), for membership
// (append/remove), and for `set_at`, which discloses that a value
// exists and when it was last touched.
//
// So a target the caller cannot READ is partitioned `unreadable` and is
// never written, never inspected, and never described beyond its id and
// that label — including in the audit envelope.
//
// Delegates to fieldReadableOnSubject, which is the ONE readability
// rule in this package. Two copies of a security rule drift, and the
// drift is the bug.
func fieldReadableForBatch(id *auth.Identity, f FieldDefinition, teamID *uuid.UUID) bool {
	var pg pgtype.UUID
	if teamID != nil && *teamID != uuid.Nil {
		pg = pgtype.UUID{Bytes: *teamID, Valid: true}
	}
	return fieldReadableOnSubject(id, f.ReadCapability, pg)
}

// ---------------------------------------------------------------------------
// Per-type value semantics
// ---------------------------------------------------------------------------

// batchValue is the proposed value after shape validation, sanitising
// and vocabulary canonicalisation — the exact bytes the apply stores.
type batchValue struct {
	Text    *string
	Num     *float64
	Date    pgtype.Timestamptz
	Options []string
	Ref     pgtype.UUID

	// Mintable lists canonical slugs that do not yet exist in an open
	// vocabulary. Naming them is not creating them: a preview mutates
	// no options document.
	Mintable []string
}

func (v batchValue) required() requiredValue {
	return requiredValue{Text: v.Text, Num: v.Num, Date: v.Date, Options: v.Options, Ref: v.Ref}
}

func (v batchValue) wire() openapi.BatchAssetFieldValue {
	out := openapi.BatchAssetFieldValue{ValueText: v.Text}
	if v.Num != nil {
		n := float32(*v.Num)
		out.ValueNum = &n
	}
	if v.Date.Valid {
		t := v.Date.Time
		out.ValueDate = &t
	}
	if v.Options != nil {
		opts := append([]string(nil), v.Options...)
		out.ValueOptions = &opts
	}
	if v.Ref.Valid {
		id := openapi_types.UUID(uuid.UUID(v.Ref.Bytes))
		out.ValueRef = &id
	}
	return out
}

// buildBatchValue validates the proposed value's SHAPE against the
// field's declared type and sanitises it, mirroring buildUpsertParams
// exactly — including that a `multi_select` with an empty option set is
// a 400 rather than an empty write.
//
// Five types cannot express a semantically empty value at all: number,
// boolean, date, datetime and reference each REQUIRE their typed
// member, so omitting it is a malformed request rather than an empty
// one. The other six can, and what happens then is a property of the
// FIELD (required or not) rather than of the request — which is why
// emptiness is judged later, by the caller, and not here.
func buildBatchValue(fieldType string, in openapi.BatchAssetFieldValue) (batchValue, error) {
	var v batchValue
	mismatch := func(format string, args ...any) error {
		return refuse(400, openapi.BatchValueTypeMismatch, format, args...)
	}

	switch fieldType {
	case "text", "longtext", richtext.FieldType, "select", "tree":
		if in.ValueText == nil {
			return v, mismatch("field type %q requires value_text", fieldType)
		}
		// SANITISE FIRST, and judge emptiness on the OUTPUT. For
		// rich_text that is the whole story: the sanitiser strips no
		// empty elements, so `<p><br></p>` survives it and reads empty,
		// and a required check run against the INPUT would let it
		// through. For the other four this is a no-op — SanitizeValueText
		// narrows to rich_text itself — and nothing here trims, which
		// is why a `text` field given "   " stores "   ".
		v.Text = richtext.SanitizeValueText(fieldType, in.ValueText)
	case "number":
		if in.ValueNum == nil {
			return v, mismatch("field type %q requires value_num", fieldType)
		}
		n := float64(*in.ValueNum)
		v.Num = &n
	case "boolean":
		if in.ValueNum == nil {
			return v, mismatch("boolean field requires value_num (0 or 1)")
		}
		n := float64(*in.ValueNum)
		if n != 0 && n != 1 {
			return v, mismatch("boolean field accepts 0 or 1 only")
		}
		// FALSE IS A REAL VALUE. It is stored as value_num = 0 and only
		// a NULL value_num is empty, so a fill_empties over a field
		// holding `false` reports no_op. A rule that tested truthiness
		// would overwrite every deliberate "no" in the catalogue.
		v.Num = &n
	case "date", "datetime":
		if in.ValueDate == nil {
			return v, mismatch("field type %q requires value_date", fieldType)
		}
		v.Date = pgtype.Timestamptz{Time: *in.ValueDate, Valid: true}
	case "multi_select":
		if in.ValueOptions == nil || len(*in.ValueOptions) == 0 {
			return v, mismatch("multi_select field requires non-empty value_options")
		}
		v.Options = append([]string(nil), (*in.ValueOptions)...)
	case "reference":
		if in.ValueRef == nil {
			return v, mismatch("reference field requires value_ref")
		}
		v.Ref = pgtype.UUID{Bytes: uuid.UUID(*in.ValueRef), Valid: true}
	default:
		return v, mismatch("unknown field type %q", fieldType)
	}
	return v, nil
}

// storedValue is one target's currently stored value.
type storedValue struct {
	Present bool
	Text    *string
	Num     *float64
	Date    pgtype.Timestamptz
	Options []string
	Ref     pgtype.UUID
	SetAt   pgtype.Timestamptz
}

func (s storedValue) required() requiredValue {
	return requiredValue{Text: s.Text, Num: s.Num, Date: s.Date, Options: s.Options, Ref: s.Ref}
}

// isEmpty answers the fill_empties question for one target.
//
// An ABSENT row is empty, which is the ordinary case the mode exists
// for. A present row is judged by valueIsEmpty — the shipped per-type
// predicate, all eleven types, obtained rather than restated.
func (s storedValue) isEmpty(fieldType string) bool {
	if !s.Present {
		return true
	}
	return valueIsEmpty(fieldType, s.required())
}

// batchTargetResult is what one mode does to one target's value.
type batchTargetResult struct {
	// Partition is the verdict. `refused` carries a Reason.
	Partition openapi.BatchAssetFieldPartition
	Reason    *openapi.BatchAssetFieldTargetRefusalReason

	// Next is the value to store. Meaningful only for would_change.
	Next batchValue

	// Delete means the row is REMOVED rather than written — reachable
	// only by `remove` emptying an OPTIONAL multi_select, because
	// writing `[]` into value_options is a shape the single-target
	// writer refuses and there is no reason for the batch to invent it.
	Delete bool
}

// resolveTargetValue applies one mode to one target's stored value and
// says which partition it lands in.
//
// Called ONLY for targets that have already passed all five gates —
// this is what sits below THE STORED-VALUE LINE, and everything above
// it decides whether the caller is entitled to be told anything about
// the value at all.
func resolveTargetValue(
	f FieldDefinition,
	mode batchMode,
	proposed batchValue,
	held storedValue,
	optionStatus map[string]OptionStatus,
) batchTargetResult {
	switch mode {
	case modeOverwrite:
		// OVERWRITE REPORTS no_op AS ZERO, even against a target that
		// already holds byte-identical bytes. A set advances `set_at`
		// and writes a history row, so it changes the record even when
		// it does not change the value — and a preview that called it
		// a no-op would under-report a confirmation count the operator
		// is about to type.
		if r := retiredNotHeld(f, proposed, held, optionStatus); r != nil {
			return *r
		}
		return batchTargetResult{Partition: openapi.BatchPartitionWouldChange, Next: proposed}

	case modeFillEmpties:
		if !held.isEmpty(f.Type) {
			return batchTargetResult{Partition: openapi.BatchPartitionNoOp}
		}
		if r := retiredNotHeld(f, proposed, held, optionStatus); r != nil {
			return *r
		}
		return batchTargetResult{Partition: openapi.BatchPartitionWouldChange, Next: proposed}

	case modeAppend:
		next, changed := appendSlugs(held.Options, proposed.Options)
		if !changed {
			return batchTargetResult{Partition: openapi.BatchPartitionNoOp}
		}
		if r := retiredNotHeld(f, proposed, held, optionStatus); r != nil {
			return *r
		}
		out := proposed
		out.Options = next
		return batchTargetResult{Partition: openapi.BatchPartitionWouldChange, Next: out}

	case modeRemove:
		next, changed := removeSlugs(held.Options, proposed.Options)
		if !changed {
			return batchTargetResult{Partition: openapi.BatchPartitionNoOp}
		}
		if len(next) == 0 {
			if f.Required {
				// R1 at the batch layer: emptying a REQUIRED field is
				// refused PER TARGET, because whether the removal
				// empties it depends on what THIS target holds. The
				// three-way case — the same removal refusing one
				// target, writing a second and no-opping a third — is
				// the ordinary consequence and all three coexist in
				// one batch.
				reason := openapi.BatchRefusalRequiredWouldEmpty
				return batchTargetResult{Partition: openapi.BatchPartitionRefused, Reason: &reason}
			}
			// OPTIONAL and now empty: DELETE THE ROW.
			return batchTargetResult{Partition: openapi.BatchPartitionWouldChange, Delete: true}
		}
		// The residual is a SUBSET of what the target already held, so
		// every term in it is grandfathered by construction. A removal
		// must NEVER fail merely because some other retired term the
		// target already holds is still in the set — that would make a
		// deprecated keyword impossible to edit around, which is the
		// exact freeze grandfathering exists to prevent.
		out := proposed
		out.Options = next
		return batchTargetResult{Partition: openapi.BatchPartitionWouldChange, Next: out}
	}
	return batchTargetResult{Partition: openapi.BatchPartitionNoOp}
}

// retiredNotHeld is the PER-TARGET half of the vocabulary lifecycle
// rule: a deprecated or archived term may be kept, but not chosen.
//
// The batch-wide half — canonicalisation, unknown slugs and mintability
// — is settled once for the whole operation, because it depends only on
// the field's document. THIS half cannot be, because the grandfather
// test needs the target's OWN held set: the very same slug in the very
// same request is grandfathered on a target that holds it and refused
// on a sibling that does not. So the verdict splits, and this side of
// it is a target-level `refused`.
//
// Membership, never set equality — the shipped rule. An operator
// removing three keywords from a set that also contains a grandfathered
// one is changing the value but is not CHOOSING the retired term.
func retiredNotHeld(
	f FieldDefinition,
	proposed batchValue,
	held storedValue,
	optionStatus map[string]OptionStatus,
) *batchTargetResult {
	incoming := vocabularySlugs(f.Type, proposed.Text, proposed.Options)
	if len(incoming) == 0 || len(optionStatus) == 0 {
		return nil
	}
	grandfathered := make(map[string]struct{}, len(held.Options)+1)
	for _, s := range vocabularySlugs(f.Type, held.Text, held.Options) {
		grandfathered[s] = struct{}{}
	}
	for _, s := range incoming {
		status, known := optionStatus[s]
		if !known || status == OptionActive {
			continue
		}
		if _, kept := grandfathered[s]; kept {
			continue
		}
		reason := openapi.BatchRefusalVocabularyRetiredNotHeld
		return &batchTargetResult{Partition: openapi.BatchPartitionRefused, Reason: &reason}
	}
	return nil
}

// appendSlugs adds terms to a held set, preserving the held order and
// then the incoming order, and reports whether anything actually moved.
//
// A term already present is not added twice and does not make the
// operation a change — which is the PHANTOM-WOULD_CHANGE case seen from
// the set side: an operator input that canonicalises onto a slug the
// target already holds is a no-op at preview and stays one at apply.
func appendSlugs(held, incoming []string) ([]string, bool) {
	have := make(map[string]struct{}, len(held))
	out := make([]string, 0, len(held)+len(incoming))
	for _, s := range held {
		have[s] = struct{}{}
		out = append(out, s)
	}
	changed := false
	for _, s := range incoming {
		if _, dup := have[s]; dup {
			continue
		}
		have[s] = struct{}{}
		out = append(out, s)
		changed = true
	}
	return out, changed
}

// removeSlugs takes terms out of a held set and reports whether
// anything actually moved. A removal naming terms the target does not
// hold is a no_op, not an error.
func removeSlugs(held, incoming []string) ([]string, bool) {
	drop := make(map[string]struct{}, len(incoming))
	for _, s := range incoming {
		drop[s] = struct{}{}
	}
	out := make([]string, 0, len(held))
	changed := false
	for _, s := range held {
		if _, gone := drop[s]; gone {
			changed = true
			continue
		}
		out = append(out, s)
	}
	return out, changed
}

// ---------------------------------------------------------------------------
// Shared field-level validation
// ---------------------------------------------------------------------------

// batchConfigurationRefusal applies every batch-wide refusal that
// depends ONLY on the field's configuration and the operator's chosen
// mode — nothing here reads the proposed value.
//
// It runs BEFORE the value's shape is validated, and that order is a
// decision. Asking a `text` field to `append` is refused as
// mode_not_supported_for_type rather than as value_type_mismatch,
// because the operator asked for something that has no meaning on this
// field at all: telling them their value arrived in the wrong member
// would send them off to fix a value for an operation that can never
// work whatever they put in it.
func batchConfigurationRefusal(f FieldDefinition, mode batchMode) error {
	code := f.Code

	if f.Status == "archived" {
		return refuse(422, openapi.BatchFieldArchived,
			"%s is archived; its values cannot be set", code).withField(code)
	}
	if _, mirrored := MirrorColumnOf(f); mirrored {
		// A mirrored field is a VIEW onto an assets column, and that
		// column carries its own human write plane with its own
		// concurrency and its own required rule. Batching through the
		// value plane would write a divergent copy — migration 00044's
		// trigger refuses the row outright — so this is a refusal
		// rather than a second implementation of the mirror.
		return refuse(422, openapi.BatchFieldMirrored,
			"%s mirrors an asset column and cannot be batch-edited through the field-value plane", code).withField(code)
	}
	if msg := readOnlyRefusal(f, "set"); msg != "" {
		return refuse(422, openapi.BatchFieldReadOnly, "%s", msg).withField(code)
	}
	if (mode == modeAppend || mode == modeRemove) && !appendRemoveSupported(f.Type) {
		return refuse(422, openapi.BatchModeNotSupportedForType,
			"%s is a %s field; the %s mode applies to multi_select only",
			code, f.Type, string(mode)).withField(code)
	}
	return nil
}

// batchFieldRefusal applies the batch-wide refusals that DO read the
// proposed value.
//
// Order matters and is the shipped order, not a convenient one. In
// particular R1 sits ABOVE the vocabulary pipeline, which is what makes
// a required `select` given "   " answer required_value_empty rather
// than unknown_slug: on the single-target path requiredSetRefusal runs
// before the transaction the vocabulary gate lives in, and the batch
// reproduces that rather than inventing its own precedence.
func batchFieldRefusal(f FieldDefinition, mode batchMode, v batchValue) error {
	code := f.Code

	if msg := patternRefusal(f, v.Text); msg != "" {
		return refuse(422, openapi.BatchPatternMismatch, "%s", msg).withField(code)
	}

	empty := valueIsEmpty(f.Type, v.required())

	if mode == modeFillEmpties && empty {
		// BATCH-WIDE on REQUIRED AND OPTIONAL fields alike, and on
		// every type. The mode means "give the empty ones a value";
		// a value that is itself empty makes it a contradiction, and
		// on an optional field it would write empty rows over exactly
		// the targets that had none.
		return refuse(422, openapi.BatchFillEmptiesValueEmpty,
			"%s: fill_empties needs a value to fill with, and the one supplied is empty", code).withField(code)
	}
	if mode == modeOverwrite && f.Required && empty {
		return refuse(422, openapi.BatchRequiredValueEmpty,
			"%s is required, so it cannot be given an empty value. Write a value, or change the field's configuration if it should be optional",
			code).withField(code)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reference liveness
// ---------------------------------------------------------------------------

// referenceTargetLive answers whether the proposed reference resolves.
//
// `deleted_at IS NULL` and NEVER `status`, because that is what
// GetReferencedAsset does and the two must mean one thing. ARCHIVE IS
// NOT DELETION: an archived asset is a perfectly valid reference
// target, on the way in and on the way out.
//
// The value-wide contract: one reference is proposed for the whole
// batch, so its liveness is a batch-wide fact and its failure is a
// batch-wide refusal rather than a thousand identical target-level ones.
func referenceTargetLive(ctx context.Context, q *Queries, ref pgtype.UUID) (bool, error) {
	if !ref.Valid {
		return true, nil
	}
	if _, err := q.GetReferencedAsset(ctx, ref); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("metadata: verify reference target: %w", err)
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Reason validation
// ---------------------------------------------------------------------------

// The operator reason's TWO DISTINCT BOUNDS.
//
// batchReasonSemanticLimit is the PRODUCT rule, measured in Unicode
// CODE POINTS on the value AFTER TRIMMING. It matches the convention
// the two shipped operator-reason fields already use.
//
// batchReasonRawCeiling is a DEFENSIVE cap on the value AS RECEIVED,
// checked BEFORE trimming, and it is deliberately looser. It is what
// the OpenAPI schema encodes as `maxLength`, so that a conforming
// external validator never rejects a value this server would accept —
// whitespace around a 500-code-point body is valid, and a schema that
// encoded 500 would refuse it. Exceeding it is a DIFFERENT refusal
// from exceeding the semantic limit, because they call for different
// corrections: one is a client sending too much, the other is an
// operator writing too much.
//
// ⚠️ `maxLength` is enforced NOWHERE in generated code — `grep -c
// maxLength app/internal/openapi/openapi.gen.go` is 0. Both bounds are
// enforced here or they are not enforced at all.
const (
	batchReasonSemanticLimit = 500
	batchReasonRawCeiling    = 2000
)

// validateBatchReason applies the reason's rules in the ONE order that
// makes them consistent: raw ceiling, trim, requiredness, semantic
// limit.
//
// Any other order produces a contradiction. Trimming before the raw
// check would let an arbitrarily large payload through as long as most
// of it was whitespace, which is the thing the raw ceiling exists to
// stop; checking the semantic limit before trimming would refuse the
// 504-raw / 500-trimmed case the two bounds exist to accept.
//
// Code points, not bytes: a 500-character reason written in a
// multi-byte script is 500 code points and roughly 1,500 bytes, and it
// is accepted.
//
// TOKEN-INDEPENDENT, so it may run in Phase 0 before the token is
// looked at — it leaks nothing about anybody's preview.
func validateBatchReason(raw string) (string, error) {
	if len([]rune(raw)) > batchReasonRawCeiling {
		return "", refuse(400, openapi.BatchReasonPayloadTooLarge,
			"the reason may be at most %d characters as sent; this one is %d",
			batchReasonRawCeiling, len([]rune(raw)))
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", refuse(400, openapi.BatchReasonRequired,
			"a reason is required: say why this change is being made across many records")
	}
	if n := len([]rune(trimmed)); n > batchReasonSemanticLimit {
		return "", refuse(400, openapi.BatchReasonTooLong,
			"the reason may be at most %d characters; this one is %d",
			batchReasonSemanticLimit, n)
	}
	return trimmed, nil
}

// readablePosts narrows a selection's post ids to the ones this caller
// may actually read, through the shipped per-post gate.
//
// One EXISTS per post rather than one spliced predicate: sqlc's static
// SQL cannot take a runtime fragment (the same constraint
// ListAssetsPageGated documents), and the alternative — re-deriving the
// rule in a query of our own — is a second copy of a read gate that
// consults post visibility, ACLs, drafts and `posts.admin`. The cost is
// bounded by the SELECTION ENTRY CEILING, which is checked before this
// runs, so it is at most 500 index lookups on a surface the operator
// themselves assembled.
func (h *Handler) readablePosts(
	ctx context.Context,
	id *auth.Identity,
	postIDs []pgtype.UUID,
) ([]pgtype.UUID, error) {
	caps := visibility.ResolvePostCaps(func(code string) bool { return id.Can(code) })
	caller := visibility.Caller{UserRef: id.UserRef, IsAnonymous: id.IsAnonymous()}

	out := make([]pgtype.UUID, 0, len(postIDs))
	for _, p := range postIDs {
		ok, err := visibility.PostReadable(ctx, h.Pool, caller, caps, uuid.UUID(p.Bytes))
		if err != nil {
			// Propagated, never folded into "no". A read gate that
			// answers denied on a transport blip is indistinguishable
			// from a permissions bug to whoever hits it.
			return nil, fmt.Errorf("metadata: post read gate: %w", err)
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}
