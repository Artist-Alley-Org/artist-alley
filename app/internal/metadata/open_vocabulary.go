// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// ---------------------------------------------------------------------------
// Open vocabularies — the sanctioned way past the membership gate (#830)
// ---------------------------------------------------------------------------
//
// checkVocabulary (options.go) refuses a term the field does not offer,
// always. That is right for `country` — a catalogue does not acquire a
// new country because a camera spelled one oddly — and wrong for
// `keywords`, the one field whose vocabulary is SUPPOSED to grow from
// the material. Before this, the only way to add a keyword was the
// admin options editor, one term at a time, which is not a workflow
// anybody cataloguing real files would use.
//
// field_definition.open_vocabulary (migration 00028) says which fields
// grow. On one of those, a term that matches nothing is CREATED rather
// than refused: the trimmed input becomes the label, its slugified form
// becomes the stored value.
//
// # Why this is a flag and not a twelfth type
//
// An open vocabulary is a multi_select in storage (value_options), in
// rendering (chips resolved through options.values), in search and in
// federation. It differs in its WRITE POLICY and in nothing else. A
// twelfth `type` would have to be re-handled in every switch that
// already handles multi_select — buildUpsertParams, vocabularySlugs,
// valueRowToJSON, the frontend's field renderer — and every one of
// those would be a place the two could drift.
//
// # Honoured for multi_select only, this sprint
//
// The column is legal on any type; openVocabularyApplies is the one
// place that narrows it. Setting it on a `text` field is inert rather
// than an error, so a later sprint can open `select` (or `tree`, which
// additionally has to decide WHERE in the hierarchy a new term lands —
// a real design question, not an oversight) by widening this function
// alone.

// openVocabularyApplies reports whether a field's open_vocabulary flag
// is honoured on this write. See the type note above.
func openVocabularyApplies(fieldType string, open bool) bool {
	return open && fieldType == "multi_select"
}

// slugMaxLen caps a minted slug. Matches the seeder's team-slug cap
// (app/internal/seed/runner.go) — long enough for any real term,
// short enough that a pasted paragraph cannot become a slug.
const slugMaxLen = 80

// Slugify converts free text into the lowercase, hyphenated form a
// stored option value takes.
//
// Deliberately the same convention the seeder uses for team slugs and
// web/src/lib/admin/teams.ts uses in the browser: lowercase, every run
// of non-alphanumerics collapsed to one hyphen, trimmed, capped. Three
// implementations of one convention is two too many, but they sit in
// packages that cannot import each other (the seeder is DB-direct and
// below the API; teams.ts is in the browser), and the convention is
// four lines. What matters is that they agree, which is why this says
// so out loud rather than quietly reinventing the rule.
//
// Returns "" for input with no alphanumerics at all — a term of "!!!"
// has no addressable form, and the callers treat an empty slug as a
// term that cannot be created.
func Slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > slugMaxLen {
		out = strings.Trim(out[:slugMaxLen], "-")
	}
	return out
}

// OpenVocabularyResult is what one accept-and-create pass produced.
type OpenVocabularyResult struct {
	// Slugs is the canonical value the row must store: one slug per
	// distinct incoming term, in the order the terms arrived. This is
	// what replaces the caller's raw input — a row must never keep the
	// text a client typed.
	Slugs []string

	// Created lists the slugs this pass minted, in creation order.
	// Empty when every term matched something. Callers log it and use
	// its length to decide whether cache invalidation is needed.
	Created []string

	// Matched counts incoming terms that resolved to a term the field
	// already had. Created + Matched can exceed len(Slugs) only if the
	// input repeated a term, which is deduped.
	Matched int

	// Options is the LIVE options document — the one read under the row
	// lock, with any created terms already appended. Callers that go on
	// to run checkVocabulary must use this and not their cached copy,
	// which by construction does not contain what was just created.
	Options []byte
}

// EnsureOpenVocabularyTerms resolves incoming free text against a
// field's live options document, creating the terms that match
// nothing, and returns the canonical slugs the value row must store.
//
// MUST be called inside the caller's transaction — the one that goes on
// to write the value. q is that transaction's Queries.
//
// # Matching
//
// Slug or label, case-insensitive, whitespace-trimmed, at full depth.
// Deliberately the same rule resolveVocabularySlug applies on the
// extraction path (app/internal/asset/metadata/vocabulary.go), because
// "Character" and "character" naming the same term is a property of the
// vocabulary, not of the path a value arrived on. Archived terms do not
// match: an archived term is one an operator retired hard, and typing
// its label should not resurrect it.
//
// # Atomicity
//
// Adding a term rewrites the WHOLE options document, so a plain
// read-modify-write loses a concurrent one — the last-write-wins gap
// #737 records for the admin editor, except here it would happen on an
// ordinary value save nobody thinks of as editing the field. The read
// is therefore SELECT … FOR UPDATE, and it re-resolves against the
// document it just locked rather than against the caller's copy: the
// field-by-id LRU can be one write old, and resolving against a stale
// document is exactly how a term gets minted twice.
//
// The residual race is with the admin options editor's whole-document
// PATCH, which does not take this lock and can still clobber a term
// created a moment earlier. That is #737's known gap and out of scope
// here; closing it means moving the editor onto the same lock.
//
// # Who may create
//
// canExtend is the caller's `fields.vocabulary.extend` capability
// (ADR 0092 §2), and it is passed in rather than read here because
// this function runs inside a transaction and knows nothing about the
// request. False turns the pass into pure RESOLUTION: existing terms,
// aliases and merge tombstones all still match — matching is not
// extension — and only a term that would have been NEW is refused,
// with ExtensionForbidden set so the 422 can say which of the two
// reasons applied.
//
// # Errors
//
// Returns a wrapped *slugRejection when a term cannot be created —
// when its slug collides with an ARCHIVED term, or when the caller may
// not extend. Callers use errors.As to turn it into the same 422 a
// closed field produces.
func EnsureOpenVocabularyTerms(
	ctx context.Context,
	q *Queries,
	fieldID pgtype.UUID,
	incoming []string,
	canExtend bool,
) (OpenVocabularyResult, error) {
	var out OpenVocabularyResult

	locked, err := q.LockFieldDefinitionVocabulary(ctx, fieldID)
	if err != nil {
		return out, fmt.Errorf("metadata: lock vocabulary: %w", err)
	}
	out.Options = locked.Options

	values, rest, err := decodeOptionValues(locked.Options)
	if err != nil && !errors.Is(err, errNoValues) {
		return out, fmt.Errorf("metadata: read vocabulary: %w", err)
	}
	if rest == nil {
		rest = map[string]json.RawMessage{}
	}

	idx := indexVocabulary(values)

	// The flag is read from the LOCKED row, not from the caller's copy.
	// A field's openness is a live property, and a caller reaching this
	// on a cached `true` that an admin has since turned off must not
	// mint on it. When it reads false the pass degrades to pure
	// resolution: every miss comes back as an unknown-slug rejection,
	// which is the answer checkVocabulary would have given.
	mint := openVocabularyApplies(locked.Type, locked.OpenVocabulary) && canExtend

	// Told apart so the refusal can be. Both produce "this term does
	// not exist and will not be created", and they are different
	// answers: one is a property of the FIELD (it is closed, or was
	// closed between the caller's cached read and this row lock), the
	// other is a property of the CALLER.
	openHere := openVocabularyApplies(locked.Type, locked.OpenVocabulary)

	seen := make(map[string]struct{}, len(incoming))
	for _, raw := range incoming {
		term := strings.TrimSpace(raw)
		if term == "" {
			// An empty term is not a value. Dropping it rather than
			// refusing keeps a trailing comma in an IPTC keyword list
			// from failing a whole write.
			continue
		}
		slug, matched, rej := idx.resolveOrMint(term, mint)
		if rej != nil {
			if openHere && !canExtend && rej.unknown() {
				rej.ExtensionForbidden = true
			}
			return out, fmt.Errorf("metadata: %w", rej)
		}
		if matched {
			out.Matched++
		} else {
			out.Created = append(out.Created, slug)
			values = append(values, FieldOption{Value: slug, Label: term})
		}
		if _, dup := seen[slug]; dup {
			// "Sunset, sunset" is one term twice, not two terms. The
			// dedupe is on the CANONICAL slug so it catches the casing
			// and whitespace variants too, which is the whole point.
			continue
		}
		seen[slug] = struct{}{}
		out.Slugs = append(out.Slugs, slug)
	}

	if len(out.Created) == 0 {
		return out, nil
	}

	encoded, err := json.Marshal(values)
	if err != nil {
		return out, fmt.Errorf("metadata: encode vocabulary: %w", err)
	}
	rest["values"] = encoded
	doc, err := json.Marshal(rest)
	if err != nil {
		return out, fmt.Errorf("metadata: encode vocabulary: %w", err)
	}
	// Through the same normaliser the admin editor uses, so a document
	// this path grew is validated by the rule that validates one an
	// operator edits — tree-wide slug uniqueness above all.
	doc, err = NormalizeOptionsDoc(doc)
	if err != nil {
		return out, fmt.Errorf("metadata: normalise vocabulary: %w", err)
	}
	if err := q.SetFieldDefinitionOptions(ctx, SetFieldDefinitionOptionsParams{
		ID:      fieldID,
		Options: doc,
	}); err != nil {
		return out, fmt.Errorf("metadata: write vocabulary: %w", err)
	}
	out.Options = doc
	return out, nil
}

// openOrCheckVocabulary is the whole write policy for one value save:
// grow the vocabulary first if the field is open, then apply the
// membership rule either way.
//
// Both value writers call it and neither implements any part of it
// themselves, for the same reason checkVocabulary is shared — a rule
// with two implementations is a rule that will be fixed in one of them.
//
// Returns what the write settled on (see [vocabularyWrite]) and a
// rejection when it may not proceed. MUST be called inside the write's
// transaction: a rejection rolls the created terms back with the value.
//
// An open field still gets checkVocabulary, run against the LIVE
// document rather than the caller's cached one. That is not belt and
// braces: openness is about terms the field does not HAVE, and says
// nothing about terms it has RETIRED. A deprecated term matches by
// label like any other, and the lifecycle half of the rule still
// refuses it as a fresh choice while grandfathering a record that
// already holds it — the same answer a closed field gives.
func openOrCheckVocabulary(
	ctx context.Context,
	qTx *Queries,
	field FieldDefinition,
	incoming []string,
	held []string,
	canExtend bool,
) (vocabularyWrite, *slugRejection, error) {
	out := vocabularyWrite{Slugs: incoming, Options: field.Options}
	if len(incoming) == 0 {
		return out, nil, nil
	}
	if !openVocabularyApplies(field.Type, field.OpenVocabulary) {
		// A CLOSED vocabulary still redirects. Aliases and merge
		// tombstones are curation, not extension: an operator who
		// aliased "uk" onto `gb` on a `country` select meant it to work
		// there, and a `country` value that survived a merge has to
		// reach its successor exactly as a keyword does. Restricting
		// either to open fields would make the normalisation tooling
		// useless on precisely the fields most likely to be curated.
		//
		// No row lock: nothing is written to the options document on
		// this branch, so the cached copy is enough. Anything the index
		// cannot canonicalise passes through untouched and
		// checkVocabulary gives its own answer — which is what keeps
		// the grandfathering rule (a record may re-save a retired term
		// it already holds) working exactly as before.
		out.Slugs = canonicaliseVocabulary(field.Options, incoming)
		return out, checkVocabulary(field.Type, field.Options, out.Slugs, held), nil
	}
	res, err := EnsureOpenVocabularyTerms(ctx, qTx, field.ID, incoming, canExtend)
	if err != nil {
		var mintRej *slugRejection
		if errors.As(err, &mintRej) {
			return out, mintRej, nil
		}
		return out, nil, err
	}
	if len(res.Slugs) == 0 {
		// Every term was whitespace. buildUpsertParams has already
		// refused an EMPTY value_options, so getting here means the
		// client sent something that looked like a value and was not
		// one — writing it would leave a multi_select row holding NULL.
		return out, &slugRejection{Slug: incoming[0]}, nil
	}
	out = vocabularyWrite{Slugs: res.Slugs, Created: res.Created, Options: res.Options}
	return out, checkVocabulary(field.Type, res.Options, res.Slugs, held), nil
}

// vocabularyWrite is what one value write's vocabulary pass settled on.
type vocabularyWrite struct {
	// Slugs is what the row must store: canonical, deduped, in the
	// order the terms arrived.
	Slugs []string
	// Created lists the slugs this write minted. Non-empty means the
	// field definition changed and its caches must be dropped.
	Created []string
	// Options is the options document as it stands AFTER this write —
	// the caller's copy when nothing was created, the freshly written
	// one when something was.
	//
	// The response body is built from it rather than from the field row
	// the handler loaded, because that row predates the terms this very
	// write created: resolving against it returns every term EXCEPT the
	// new ones, and a client rendering the response would show the raw
	// slug `macro-detail` where the label "Macro Detail" belongs, until
	// something forced a refetch.
	Options []byte
}

// vocabularyIndex is a field's option tree flattened into the two
// lookups an incoming term needs: what it can match, and what slugs are
// already taken.
type vocabularyIndex struct {
	// matchable maps a normalised slug OR label to the canonical slug.
	// Archived terms are absent — they are not matchable — but their
	// slugs are still in taken.
	matchable map[string]string
	// taken maps a normalised slug to the option holding it, INCLUDING
	// archived ones, so a mint can detect a collision it must not make.
	taken map[string]FieldOption
	// redirects is the CURATION subset of matchable: the keys that
	// exist because an operator aliased a term or merged one away, and
	// none of the keys a vocabulary has by simply having terms.
	//
	// Separate because the closed-field write path applies exactly this
	// subset (see canonicaliseVocabulary). Curation must work on a
	// `select` field — that is where curation happens — but the
	// membership rule for a closed vocabulary is otherwise unchanged by
	// this sprint, and folding matchable's case-insensitive slug and
	// label keys into it would quietly widen what a closed field
	// accepts, which is a different decision that nobody made.
	redirects map[string]string
}

// indexVocabulary flattens a field's option tree into the lookups a
// write needs.
//
// # Three passes, and the order is the precedence rule
//
// Every match key — slug, label, alias, tombstone — lives in ONE map,
// and idx.add is first-writer-wins. So the pass order IS the precedence
// order, and it is:
//
//  1. Slugs and labels of live terms. A term that exists always wins.
//  2. Aliases (ADR 0092 §4). An alias may only fill a key nothing real
//     occupies — so adding an alias can never shadow, hijack or
//     silently redirect an existing term, whatever an operator types.
//     NormalizeOptionsDoc refuses such a collision on write; this makes
//     the runtime behaviour safe even for a document that predates that
//     check.
//  3. Merge tombstones. An ARCHIVED term carrying ReplacedBy is not a
//     retirement, it is a forwarding address: a value naming it
//     resolves to its successor rather than being refused. That is what
//     lets a value written before a merge — by a queued client, or by a
//     federated peer that has not seen the merge — still land on
//     something real instead of 422ing forever.
//
// A plain archived term (no ReplacedBy) is absent from every pass: it
// is a hard retire, and typing its label must not resurrect it. Its
// slug is still in `taken`, so a mint cannot collide with it.
func indexVocabulary(values []FieldOption) *vocabularyIndex {
	idx := &vocabularyIndex{
		matchable: make(map[string]string, len(values)*2),
		taken:     make(map[string]FieldOption, len(values)),
		redirects: map[string]string{},
	}
	walkOptions(values, nil, func(o FieldOption, _ []string) {
		slug := strings.TrimSpace(o.Value)
		if slug == "" {
			return
		}
		key := strings.ToLower(slug)
		if _, dup := idx.taken[key]; !dup {
			// Store the TRIMMED slug: this entry is what a slugified-form
			// match returns as the canonical value, and a stray space in
			// it would land in value_options.
			o.Value = slug
			idx.taken[key] = o
		}
		if o.Status == OptionArchived {
			return
		}
		idx.add(key, slug)
		if label := strings.ToLower(strings.TrimSpace(o.Label)); label != "" {
			idx.add(label, slug)
		}
	})

	walkOptions(values, nil, func(o FieldOption, _ []string) {
		slug := strings.TrimSpace(o.Value)
		if slug == "" || o.Status == OptionArchived {
			return
		}
		for _, a := range o.Aliases {
			idx.addRedirect(a, slug)
		}
	})

	walkOptions(values, nil, func(o FieldOption, _ []string) {
		slug := strings.TrimSpace(o.Value)
		if slug == "" || o.Status != OptionArchived || o.ReplacedBy == "" {
			return
		}
		target := idx.resolveTombstone(o, 0)
		if target == "" {
			// A chain that loops, runs too deep, or ends somewhere that
			// is not a live term. Treated as a plain archive rather than
			// followed — a forwarding address must forward SOMEWHERE,
			// and refusing the write is a better answer than storing a
			// slug no picker offers.
			return
		}
		idx.addRedirect(strings.ToLower(slug), target)
		if label := strings.ToLower(strings.TrimSpace(o.Label)); label != "" {
			idx.addRedirect(label, target)
		}
		for _, a := range o.Aliases {
			idx.addRedirect(a, target)
		}
	})
	return idx
}

// tombstoneChainMax bounds how far a forwarding address is followed.
// Merges compose — `uk` → `gb` → `united-kingdom` is two ordinary
// merges — but a document can also carry a cycle (nothing stops an
// operator writing replaced_by both ways through the options editor),
// and an unbounded walk on one would hang a value write.
const tombstoneChainMax = 8

// resolveTombstone follows an archived term's ReplacedBy chain to the
// first LIVE term, or returns "" when the chain does not reach one.
//
// Resolving to a live term rather than to the immediate successor is
// the point: after `uk` → `gb` and later `gb` → `united-kingdom`, a
// value naming `uk` must reach `united-kingdom`. Stopping at `gb` would
// store a tombstone as if it were a value, which is exactly the state
// the merge existed to remove.
func (idx *vocabularyIndex) resolveTombstone(o FieldOption, depth int) string {
	if depth >= tombstoneChainMax {
		return ""
	}
	next, ok := idx.taken[strings.ToLower(strings.TrimSpace(o.ReplacedBy))]
	if !ok {
		return ""
	}
	if next.Status != OptionArchived {
		return next.Value
	}
	if next.ReplacedBy == "" {
		return ""
	}
	return idx.resolveTombstone(next, depth+1)
}

// add records a match key, first writer wins. First-wins mirrors
// resolveOptionSlugs, which takes the first of a duplicate pair for the
// same reason: NormalizeOptionsDoc rejects duplicate slugs on write, so
// a collision here can only come from a document that predates it, and
// picking deterministically beats picking last.
func (idx *vocabularyIndex) add(key, slug string) {
	if key == "" {
		return
	}
	if _, exists := idx.matchable[key]; !exists {
		idx.matchable[key] = slug
	}
}

// addRedirect records a key that exists because of curation — an alias
// or a merge tombstone — in both maps. Same first-wins rule, and the
// matchable write is what makes an alias usable by the minting path
// (a term that resolves is a term that is not created).
func (idx *vocabularyIndex) addRedirect(key, slug string) {
	if key == "" {
		return
	}
	if _, exists := idx.matchable[key]; exists {
		// Something real already owns this key. An alias may not take
		// it — see indexVocabulary's precedence note.
		return
	}
	idx.matchable[key] = slug
	idx.redirects[key] = slug
}

// resolveOrMint returns the canonical slug for one term, whether it
// matched an existing one, and a rejection when it can neither match
// nor be created.
//
// There are three ways to match and only one way to mint, in order:
//
//  1. The term IS a slug or a label the field carries, case-insensitive.
//  2. The term SLUGIFIES ONTO one. "black_and_white" and "Black & White"
//     both address `black-and-white`, and neither is a new term.
//     Checking this BEFORE minting is also what stops a term repeated in
//     one request from colliding with the copy its first occurrence just
//     minted.
//  3. Otherwise it is new, and its slugified form becomes the value.
//
// A collision with an ARCHIVED term is refused rather than matched or
// disambiguated into `foo-2`. All three readings were available and the
// other two are worse: matching revives a term an operator retired
// deliberately, and minting a near-miss leaves the catalogue with
// `sunset` and `sunset-2` meaning one thing, which is the duplicate mess
// an open vocabulary has to avoid to be worth having. Refusing says so
// in the words the closed path already uses, and the operator
// un-archives the term or picks another word.
//
// mint=false turns this into pure resolution: a miss is refused rather
// than created, which is what a field whose flag was turned off between
// the caller's cached read and the row lock needs. Matching is
// unaffected — matching is matching.
func (idx *vocabularyIndex) resolveOrMint(term string, mint bool) (slug string, matched bool, rej *slugRejection) {
	term = strings.TrimSpace(term)
	key := strings.ToLower(term)
	if canonical, ok := idx.matchable[key]; ok {
		return canonical, true, nil
	}
	minted := Slugify(term)
	if minted == "" {
		// No alphanumerics at all: there is no addressable form of this
		// term, so it is not a term. Reported as unknown, which is what
		// it is — nothing in the field matches it and nothing can.
		return "", false, &slugRejection{Slug: term}
	}
	// The slugified form goes through the SAME key space the typed form
	// did, not through `taken`. That is what makes "U.K." reach `gb` via
	// the alias `uk`, and "U.K." reach `united-kingdom` via the `uk`
	// tombstone — asking `taken` here would see only the raw options and
	// refuse both.
	if canonical, ok := idx.matchable[minted]; ok {
		// Remember this spelling so a third one short-circuits at (1).
		idx.add(key, canonical)
		return canonical, true, nil
	}
	if existing, clash := idx.taken[minted]; clash {
		// Reachable only for an ARCHIVED term with no usable forwarding
		// address — everything else is in matchable and was answered
		// above. A hard-retired term is refused rather than revived.
		return "", false, &slugRejection{Slug: existing.Value, Status: OptionArchived}
	}
	if !mint {
		return "", false, &slugRejection{Slug: term}
	}
	idx.taken[minted] = FieldOption{Value: minted, Label: term}
	idx.add(minted, minted)
	idx.add(key, minted)
	return minted, false, nil
}

// canonicaliseVocabulary maps each incoming slug through the field's
// alias and tombstone redirects, deduplicating the result while keeping
// the order the terms arrived in.
//
// Deliberately NOT a gate. A slug the index cannot resolve comes back
// unchanged rather than being refused, because [checkVocabulary] is the
// membership rule and it knows things this does not — chiefly which
// retired terms the record already holds, and so may keep. Refusing
// here would silently take grandfathering away from every closed field.
//
// Reads the document the caller already has. On the closed path nothing
// writes to that document, so there is nothing to lock and nothing to
// race with.
func canonicaliseVocabulary(options []byte, incoming []string) []string {
	values, _, err := decodeOptionValues(options)
	if err != nil || len(values) == 0 {
		return incoming
	}
	idx := indexVocabulary(values)

	out := make([]string, 0, len(incoming))
	seen := make(map[string]struct{}, len(incoming))
	changed := false
	for _, raw := range incoming {
		slug := strings.TrimSpace(raw)
		if canonical, ok := idx.redirects[strings.ToLower(slug)]; ok && canonical != slug {
			slug = canonical
			changed = true
		}
		if _, dup := seen[slug]; dup {
			// Two incoming terms landed on one — an alias beside its
			// target, or both sides of a merge. One value, once.
			changed = true
			continue
		}
		seen[slug] = struct{}{}
		out = append(out, slug)
	}
	if !changed {
		// Return the caller's slice untouched when nothing moved, so a
		// pointer comparison in a test means what it looks like and no
		// caller pays for an allocation on the overwhelmingly common
		// path.
		return incoming
	}
	return out
}
