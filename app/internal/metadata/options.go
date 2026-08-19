// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// The options document (ADR 0012 + its 2026-07-30 amendment)
// ---------------------------------------------------------------------------
//
// field_definition.options is a free-form jsonb whose meaning depends
// on the field's type. For select / multi_select / tree it carries a
// controlled vocabulary under "values".
//
// Two entry shapes exist in the wild and both must keep working:
//
//	{"values": ["sRGB", "Linear"]}                       // bare slug
//	{"values": [{"value":"srgb","label":"sRGB"}]}        // object
//
// The bare form is what the seeder writes (seed/runner.go) and what
// all five live option-carrying fields hold today; the object form is
// what schema.sql and ADR 0012 document. Rather than migrate, we
// decode both into one model and re-encode each entry in the
// *narrowest* form that still carries its information — so a document
// that predates option lifecycle round-trips byte-identical, and only
// entries an operator actually gave a label or a status to grow into
// objects.
//
// Per the amendment an option carries the same lifecycle its field
// does: active (default when absent) / deprecated / archived, plus an
// optional replaced_by naming another slug in the same field. Options
// are never hard-deleted — that would orphan values already stored on
// assets, and the orphan surfaces as a blank on an asset nobody
// edited.

// OptionStatus is an option's lifecycle state. Deliberately the same
// vocabulary field_definition.status uses, one level down.
type OptionStatus string

const (
	// OptionActive is offered for selection, resolves and displays.
	// It is the meaning of an absent status.
	OptionActive OptionStatus = "active"
	// OptionDeprecated is not offered for new values, but existing
	// values still resolve and display. Where ReplacedBy is set the
	// editing surface suggests the successor.
	OptionDeprecated OptionStatus = "deprecated"
	// OptionArchived is not offered and not resolved — a hard retire
	// for terms that were mistakes rather than terms superseded.
	OptionArchived OptionStatus = "archived"
)

// validOptionStatus reports whether s is a status we accept. The empty
// string is accepted and means active.
func validOptionStatus(s OptionStatus) bool {
	switch s {
	case "", OptionActive, OptionDeprecated, OptionArchived:
		return true
	}
	return false
}

// FieldOption is one entry in options.values.
//
// Value is the slug asset_field_value stores. It is the only part of
// an option that any asset references, which is what makes relabelling
// and deprecating free — do not denormalise Label onto any record.
type FieldOption struct {
	Value      string       `json:"value"`
	Label      string       `json:"label,omitempty"`
	Status     OptionStatus `json:"status,omitempty"`
	ReplacedBy string       `json:"replaced_by,omitempty"`
	// Aliases are operator-entered extra MATCH KEYS for this term
	// (ADR 0092 §4, #789). A value naming an alias stores this
	// option's Value.
	//
	// # Why a match key and not a stored form
	//
	// The alternative — a read-time union, where querying `gb` also
	// matches rows that stored `uk` — is not what production systems
	// do, and #789's research says why: it breaks silently on a
	// federated peer that does not have the alias table, because the
	// peer holds the row but not the rule that makes it findable. A
	// write-time redirect stores the canonical slug on OUR side and
	// the peer receives a value it can resolve with nothing but the
	// field definition it already has.
	//
	// Non-retroactive by construction: nothing rewrites a stored row,
	// so removing an alias simply stops the redirect and the rows it
	// steered keep their canonical value. That is the reversibility
	// that makes an alias the cheap first move and a merge the
	// expensive second one.
	//
	// Normalised to lowercase and deduplicated by NormalizeOptionsDoc,
	// which also refuses an alias colliding with any slug, label or
	// other alias in the same field — an ambiguous key has no correct
	// resolution, and picking one silently is how a vocabulary starts
	// lying about where a value went.
	Aliases  []string      `json:"aliases,omitempty"`
	Children []FieldOption `json:"children,omitempty"`
}

// NOTE: the "which terms may be offered" half of the lifecycle still
// lives in web/src/lib/fieldOptions.ts — only an editing surface picks
// terms, and only it holds the field definition. The "how does a
// stored slug display" half is resolveOptionSlugs below, on the server,
// because every read surface needs it and none of them holds the
// definition.

// ResolvedOption is one stored slug as a reader needs to see it: the
// option itself plus the label path from the root of the field's
// vocabulary down to and including the option.
//
// Path has one element for a flat vocabulary (select / multi_select)
// and N elements for a node N levels deep in a `tree` field's nested
// options. It is what lets a display surface print
// "Europe / United Kingdom / London" while asset_field_value stores
// nothing but `london` — see the 2026-07-31 tree-storage amendment to
// ADR 0012.
type ResolvedOption struct {
	FieldOption
	Path []string
}

// resolveOptionSlugs looks up each slug in a field's options document
// and returns what a reader needs to display it: label, lifecycle, and
// the ancestor label path.
//
// The walk descends into children. It did not always: this function
// scanned only the top level, on the stated grounds that "nested
// children belong to tree fields, whose values live in value_ref
// rather than as slugs". That premise was wrong in both halves — no
// writer has ever put a tree value in value_ref, and a tree value is
// exactly a vocabulary slug. A nested slug therefore never resolved,
// which is one of the ways the tree path stayed broken while nothing
// exercised it.
//
// Slugs are unique across a field's whole option tree, not merely
// within a level: NormalizeOptionsDoc runs collectSlugs over the full
// depth and rejects duplicates anywhere in it, on every create and
// every update. That global uniqueness is what makes a bare leaf slug
// a complete address for a node, and so what lets the stored value
// stay a single slug rather than a path.
//
// Deliberately tolerant, because this is a read path serving values
// that are already stored. A malformed options document, an unknown
// slug, a duplicate — none of these are worth failing an asset page
// over. Anything that does not resolve is simply absent from the
// result, and the caller falls back to rendering the raw slug, which
// is exactly what the surface did before resolution existed.
//
// An archived term still resolves here, unlike in the picker. The
// picker's job is to stop offering it; a detail page's job is to
// describe a value that exists, and blanking it would hide data from
// the one person who could fix it.
func resolveOptionSlugs(raw []byte, slugs []string) map[string]ResolvedOption {
	if len(slugs) == 0 {
		return nil
	}
	values, _, err := decodeOptionValues(raw)
	if err != nil || len(values) == 0 {
		return nil
	}

	want := make(map[string]struct{}, len(slugs))
	for _, s := range slugs {
		if s != "" {
			want[s] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil
	}

	out := make(map[string]ResolvedOption, len(want))
	walkOptions(values, nil, func(o FieldOption, ancestors []string) {
		v := strings.TrimSpace(o.Value)
		if v == "" {
			return
		}
		if _, ok := want[v]; !ok {
			return
		}
		if _, dup := out[v]; dup {
			return // first wins; NormalizeOptionsDoc rejects dupes on write
		}
		// A bare-string entry carries no label — the slug IS the
		// display text — and no status, which means active. Fill both
		// in here so no caller has to know the difference.
		if o.Label == "" {
			o.Label = o.Value
		}
		if o.Status == "" {
			o.Status = OptionActive
		}
		// Copy: ancestors is reused across the walk.
		path := make([]string, len(ancestors), len(ancestors)+1)
		copy(path, ancestors)
		out[v] = ResolvedOption{FieldOption: o, Path: append(path, o.Label)}
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// The write-path vocabulary gate (#824)
// ---------------------------------------------------------------------------
//
// resolveOptionSlugs above is deliberately tolerant, because it serves
// values that are ALREADY stored. This is its counterpart on the way
// in, and it is deliberately strict: a slug that is not a term of the
// field is refused rather than written.
//
// Before this, `PUT country {"value_text":"atlantis"}` returned 200.
// The slug was stored, never resolved, and rendered as the raw string
// on every read surface — indistinguishable from a good value until
// someone looked for the term and found no such country. It was
// unobservable while `country` and `keywords` shipped with an empty
// vocabulary (nothing resolved, so a bogus slug looked exactly like a
// real one); giving them real terms in #820 is what made the gap
// visible and worth closing.
//
// # One rule, two callers
//
// [SetAssetFieldValue] and [SetCollectionFieldValue] are separate
// handlers with separate type checks, and this is exactly the kind of
// rule that gets copied into both and then fixed in one. So the rule
// lives here once and both call it. The upload-defaults validator
// calls it too — see validateDefaultSlugs, which is the same
// membership question asked with no held value.
//
// # What this does NOT gate
//
// Every other writer of asset_field_value goes around this handler and
// is already safe by construction:
//
//   - Extraction upserts directly (metaValueWriterAdapter) and resolves
//     its own slugs first — see resolveVocabularySlug in
//     app/internal/asset/metadata/vocabulary.go, which stores the term
//     or stores nothing.
//   - Upload defaults write via their own if-absent query, and the
//     default itself was validated against the vocabulary when the
//     field definition was saved.
//   - The seeder writes DB-direct (app/internal/seed/runner.go),
//     deliberately outside the API. Manifest correctness is #807's.

// slugRejection is one slug a write may not store, and the reason.
//
// Status is the empty string when the slug is not a term of the field
// at all, and the option's lifecycle state when the term exists but is
// no longer offered — the two cases the API reports as
// `unknown_option` and `option_not_offerable`.
type slugRejection struct {
	Slug   string
	Status OptionStatus

	// ExtensionForbidden marks the one refusal that is about the
	// CALLER rather than the vocabulary: the field is open, the term
	// would have been created, and this caller does not hold
	// `fields.vocabulary.extend` (ADR 0092 §2).
	//
	// Kept distinct from a bare unknown-slug refusal because the fixes
	// differ and only the server can tell them apart. "That term does
	// not exist" is answered by picking another word; "you may not
	// create terms here" is answered by an operator granting a
	// capability, and a client that receives the first when the second
	// is true will send its user in circles.
	ExtensionForbidden bool
}

// unknown reports whether the slug is absent from the vocabulary
// entirely, as opposed to present but retired.
func (r *slugRejection) unknown() bool { return r.Status == "" }

func (r *slugRejection) Error() string {
	if r.ExtensionForbidden {
		return fmt.Sprintf(
			"%q is not one of this field's options, and creating it requires the %s capability",
			r.Slug, CapVocabularyExtend)
	}
	if r.unknown() {
		return fmt.Sprintf("%q is not one of this field's options", r.Slug)
	}
	return fmt.Sprintf(
		"option %q is %s and is no longer offered for new values", r.Slug, r.Status)
}

// vocabularySlugs pulls out the slugs a value puts on a vocabulary
// field, from the two columns that can carry them.
//
// The asset and collection write bodies are distinct generated types
// with identical members, and a stored row exposes the same two — so
// this takes the members rather than any one struct, and every caller
// on both paths uses it for both the incoming value and the held one.
//
// Returns nil for every type without a vocabulary, which is what makes
// [checkVocabulary] a no-op on `text`, `number`, dates, `boolean` and
// `reference`.
func vocabularySlugs(fieldType string, valueText *string, valueOptions []string) []string {
	switch fieldType {
	case "select", "tree":
		// `tree` sits with `select` because its value is ONE slug in
		// value_text, matched across the WHOLE option tree rather than
		// at the top level: slugs are unique tree-wide, so a bare slug
		// addresses a node at any depth (2026-07-31 amendment to ADR
		// 0012). A branch slug is as valid as a leaf — the picker
		// offers every term at every depth on the stated grounds that
		// "Europe" is a legitimate answer when the operator does not
		// know the city (selectableTreeOptions, web/src/lib/fieldOptions.ts).
		if valueText == nil || *valueText == "" {
			return nil
		}
		return []string{*valueText}
	case "multi_select":
		return valueOptions
	default:
		return nil
	}
}

// checkVocabulary is THE membership rule for a controlled vocabulary,
// and the only one. Returns nil when every incoming slug may be
// stored, or the first slug that may not.
//
// held is what the record ALREADY stores for this field, and it is the
// whole of the lifecycle rule:
//
//   - An unknown slug is refused always. There is no reading under
//     which storing it is right.
//   - A deprecated or archived slug is refused only when the value is
//     CHANGING to it. Options are never hard-deleted precisely so that
//     a record carrying a since-retired term keeps working, and a save
//     that leaves that term where it was must not fail — otherwise
//     deprecating a term silently freezes every record holding it.
//     This mirrors the editing surface, which stops OFFERING a retired
//     term but keeps showing one a record holds (selectableOptions,
//     web/src/lib/fieldOptions.ts).
//
// For multi_select the grandfather test is per-element MEMBERSHIP in
// the stored set, not equality of the sets: an operator removing three
// keywords from a set that also contains a grandfathered one is
// changing the value, but is not choosing the retired term, and
// refusing that save would make a deprecated keyword impossible to
// edit around. Set equality would do exactly that.
//
// Pass held=nil to ask the strict question — "may this slug be chosen
// fresh?" — which is what a field default needs.
//
// A field with no readable vocabulary refuses every slug, for the same
// reason an unknown one is refused: a field that offers no terms has
// no term to choose. That is also what this rule already did on the
// defaults path before it was shared, so generalising it changes no
// behaviour there.
func checkVocabulary(fieldType string, options []byte, incoming, held []string) *slugRejection {
	if len(incoming) == 0 {
		return nil
	}
	switch fieldType {
	case "select", "multi_select", "tree":
	default:
		return nil
	}

	grandfathered := make(map[string]struct{}, len(held))
	for _, s := range held {
		grandfathered[s] = struct{}{}
	}

	present := resolveOptionSlugs(options, incoming)
	for _, s := range incoming {
		got, found := present[s]
		if !found {
			return &slugRejection{Slug: s}
		}
		if got.Status == OptionActive {
			continue
		}
		if _, kept := grandfathered[s]; kept {
			continue
		}
		return &slugRejection{Slug: s, Status: got.Status}
	}
	return nil
}

// rejectionBody renders a slugRejection as the 422 body BOTH value
// writers return. The asset and collection handlers wrap it in their
// own generated response type — which embeds this one shape — so the
// two cannot report the same refusal with different words or a
// different reason code.
//
// The `error` string leads with the field's code because it is the
// only part a human reads, and "atlantis is not one of this field's
// options" is not actionable without knowing which field said it.
func rejectionBody(fieldCode string, rej *slugRejection) openapi.FieldValueUnprocessableJSONResponse {
	reason := openapi.OptionNotOfferable
	switch {
	case rej.ExtensionForbidden:
		reason = openapi.VocabularyExtensionForbidden
	case rej.unknown():
		reason = openapi.UnknownOption
	}
	code, slug := fieldCode, rej.Slug
	return openapi.FieldValueUnprocessableJSONResponse{
		Error:  fmt.Sprintf("%s: %s", fieldCode, rej.Error()),
		Reason: reason,
		Field:  &code,
		Option: &slug,
	}
}

// walkOptions visits every option in the document depth-first, passing
// each one the display labels of its ancestors (root first, the
// option's own label excluded). The ancestors slice is reused between
// calls — copy it if you keep it.
func walkOptions(opts []FieldOption, ancestors []string, visit func(FieldOption, []string)) {
	for _, o := range opts {
		visit(o, ancestors)
		if len(o.Children) == 0 {
			continue
		}
		label := strings.TrimSpace(o.Label)
		if label == "" {
			label = strings.TrimSpace(o.Value)
		}
		walkOptions(o.Children, append(ancestors, label), visit)
	}
}

// bare reports whether the option carries nothing beyond its slug and
// so can be written back as a plain JSON string.
func (o FieldOption) bare() bool {
	return (o.Label == "" || o.Label == o.Value) &&
		(o.Status == "" || o.Status == OptionActive) &&
		o.ReplacedBy == "" &&
		len(o.Aliases) == 0 &&
		len(o.Children) == 0
}

// UnmarshalJSON accepts either a bare slug string or the full object.
func (o *FieldOption) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*o = FieldOption{Value: s}
		return nil
	}
	// Alias breaks the recursion into this method.
	type alias FieldOption
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("option must be a string or an object: %w", err)
	}
	*o = FieldOption(a)
	return nil
}

// MarshalJSON writes the narrowest form that still carries the
// option's information: a bare string when it is nothing but a slug,
// the object otherwise. This is what lets a pre-lifecycle document
// survive a save unchanged.
func (o FieldOption) MarshalJSON() ([]byte, error) {
	if o.bare() {
		return json.Marshal(o.Value)
	}
	type alias FieldOption
	// An explicit "active" is noise — absent already means active.
	if o.Status == OptionActive {
		o.Status = ""
	}
	if o.Label == o.Value {
		o.Label = ""
	}
	return json.Marshal(alias(o))
}

// errNoValues is the sentinel for "this options document has no
// values key", which is legitimate: number fields carry min/max, text
// fields carry nothing at all.
var errNoValues = errors.New("options: no values key")

// decodeOptionValues pulls the values array out of a raw options
// document. Returns errNoValues when the document has no values key.
// Other keys in the document are returned untouched in rest so a
// re-encode preserves them.
func decodeOptionValues(raw []byte) (values []FieldOption, rest map[string]json.RawMessage, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, errNoValues
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("options must be a JSON object: %w", err)
	}
	rawValues, ok := doc["values"]
	if !ok || string(rawValues) == "null" {
		return nil, doc, errNoValues
	}
	if err := json.Unmarshal(rawValues, &values); err != nil {
		return nil, doc, fmt.Errorf("options.values must be an array of slugs or option objects: %w", err)
	}
	return values, doc, nil
}

// collectSlugs walks the option tree accumulating every slug, and
// rejects duplicates. Duplicate slugs would make replaced_by and value
// resolution ambiguous.
func collectSlugs(opts []FieldOption, into map[string]FieldOption, path string) error {
	for i, o := range opts {
		where := fmt.Sprintf("%s[%d]", path, i)
		slug := strings.TrimSpace(o.Value)
		if slug == "" {
			return fmt.Errorf("options.values%s: option value (slug) must not be empty", where)
		}
		if _, dup := into[slug]; dup {
			return fmt.Errorf("options.values%s: duplicate option value %q", where, slug)
		}
		into[slug] = o
		if err := collectSlugs(o.Children, into, where+".children"); err != nil {
			return err
		}
	}
	return nil
}

// checkOptions validates statuses and replacement pointers against the
// set of slugs present in the same field.
func checkOptions(opts []FieldOption, all map[string]FieldOption, path string) error {
	for i, o := range opts {
		where := fmt.Sprintf("%s[%d]", path, i)
		if !validOptionStatus(o.Status) {
			return fmt.Errorf(
				"options.values%s: unknown status %q — must be one of active, deprecated, archived",
				where, o.Status)
		}
		if o.ReplacedBy != "" {
			// A replaced_by pointing nowhere is the same orphan class
			// ADR 0012 rejects hard deletion for: the editing surface
			// would offer a successor that does not exist.
			if o.ReplacedBy == o.Value {
				return fmt.Errorf("options.values%s: replaced_by cannot point at itself (%q)", where, o.Value)
			}
			if _, ok := all[o.ReplacedBy]; !ok {
				return fmt.Errorf(
					"options.values%s: replaced_by %q is not an option of this field",
					where, o.ReplacedBy)
			}
		}
		if err := checkOptions(o.Children, all, where+".children"); err != nil {
			return err
		}
	}
	return nil
}

// NormalizeOptionsDoc validates an incoming options document and
// re-encodes it canonically. Documents with no values key (number
// constraints, empty objects) pass through untouched.
//
// Validation is deliberately shape-level only: it does not require a
// select field to have any options, because a field is routinely
// created before its vocabulary is filled in.
//
// Exported (#808) so the seed catalogue goes through the SAME checks
// the admin write path runs — tree-wide slug uniqueness above all.
// Decoding a document is free once FieldOption is exported, and that
// is the trap: a caller who only unmarshals gets a document that
// parses but was never checked. ADR 0012's tree amendment stores a
// value as one leaf slug and relies on slugs being unique across the
// whole tree for that leaf to be a complete address, so a duplicate
// slug in a hand-edited catalogue resolves values to the WRONG node,
// silently. Two enforcement paths for one invariant is how they
// diverge; there is one.
func NormalizeOptionsDoc(raw []byte) ([]byte, error) {
	values, rest, err := decodeOptionValues(raw)
	if errors.Is(err, errNoValues) {
		return raw, nil
	}
	if err != nil {
		return nil, err
	}

	all := make(map[string]FieldOption, len(values))
	if err := collectSlugs(values, all, ""); err != nil {
		return nil, err
	}
	if err := checkOptions(values, all, ""); err != nil {
		return nil, err
	}

	// Trim slugs/labels in place so a stray space can't create a slug
	// no UI can reproduce. Also normalises aliases, which is why the
	// alias collision check runs AFTER this and not beside
	// checkOptions: it has to test the keys matching will actually use.
	trimAll(values)

	if err := checkAliases(values); err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	rest["values"] = encoded
	return json.Marshal(rest)
}

// checkAliases refuses an alias that is ambiguous within its field.
//
// An alias is a match key, and the whole vocabulary shares one key
// space: slugs, labels and aliases all resolve through it (see
// indexVocabulary). So an alias that repeats a slug, a label or another
// option's alias names two terms at once, and there is no correct
// answer to give a value that uses it. The write path could pick the
// first — indexVocabulary does, deterministically — but "deterministic"
// is not "right", and an operator who typed the collision meant one of
// the two.
//
// The comparison is on the lowercased form for all three, matching
// indexVocabulary exactly. An alias equal to its OWN option's slug or
// label is refused too rather than silently dropped: it is a no-op the
// operator believes did something.
func checkAliases(opts []FieldOption) error {
	// key -> what claims it, for the error message. First writer wins,
	// exactly as indexVocabulary does.
	claimed := map[string]string{}
	claim := func(key, by string) {
		if key == "" {
			return
		}
		if _, dup := claimed[key]; !dup {
			claimed[key] = by
		}
	}

	// Pass 1 records the keys real terms occupy and reports NOTHING.
	//
	// That restraint is deliberate. Two terms can collide in this key
	// space today — a label repeated across branches of a tree, or two
	// slugs differing only in case, which collectSlugs allows because
	// it compares exactly. Both are pre-existing states, both resolve
	// first-wins, and turning either into a hard error here would make
	// this function refuse documents that have been stored and served
	// for months. Widening what a vocabulary must satisfy is a separate
	// decision from adding aliases, and it is not this sprint's.
	//
	// Two passes rather than one so a collision is reported the same
	// way whether the alias was declared before or after the term it
	// collides with. Declaration order must not decide.
	walkOptions(opts, nil, func(o FieldOption, _ []string) {
		claim(strings.ToLower(strings.TrimSpace(o.Value)), fmt.Sprintf("the slug %q", o.Value))
		claim(strings.ToLower(strings.TrimSpace(o.Label)), fmt.Sprintf("the label of %q", o.Value))
	})

	// Pass 2 is where the rule bites, and only for aliases: an alias is
	// NEW information an operator just typed, so refusing an ambiguous
	// one costs them a correction instead of costing every reader a
	// silently wrong resolution.
	var walkErr error
	walkOptions(opts, nil, func(o FieldOption, _ []string) {
		if walkErr != nil {
			return
		}
		for _, a := range o.Aliases {
			if prev, dup := claimed[a]; dup {
				walkErr = fmt.Errorf(
					"options.values: the alias %q on %q is already %s — a match key must name one term",
					a, o.Value, prev)
				return
			}
			claimed[a] = fmt.Sprintf("an alias of %q", o.Value)
		}
	})
	return walkErr
}

func trimAll(opts []FieldOption) {
	for i := range opts {
		opts[i].Value = strings.TrimSpace(opts[i].Value)
		opts[i].Label = strings.TrimSpace(opts[i].Label)
		opts[i].ReplacedBy = strings.TrimSpace(opts[i].ReplacedBy)
		opts[i].Aliases = normalizeAliases(opts[i].Aliases)
		trimAll(opts[i].Children)
	}
}

// normalizeAliases lowercases, trims, drops empties and deduplicates,
// preserving the operator's order. Returns nil for an empty result so
// the option serialises back to its narrowest form.
//
// Lowercasing here rather than at match time is deliberate: the match
// index keys on the lowercased form, and storing the operator's casing
// would mean two places decide what "the same alias" is. checkAliases
// runs its collision test against THIS output, so the uniqueness it
// enforces is the uniqueness matching will see.
func normalizeAliases(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, a := range in {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
