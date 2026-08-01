// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	Value      string        `json:"value"`
	Label      string        `json:"label,omitempty"`
	Status     OptionStatus  `json:"status,omitempty"`
	ReplacedBy string        `json:"replaced_by,omitempty"`
	Children   []FieldOption `json:"children,omitempty"`
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
	// no UI can reproduce.
	trimAll(values)

	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	rest["values"] = encoded
	return json.Marshal(rest)
}

func trimAll(opts []FieldOption) {
	for i := range opts {
		opts[i].Value = strings.TrimSpace(opts[i].Value)
		opts[i].Label = strings.TrimSpace(opts[i].Label)
		opts[i].ReplacedBy = strings.TrimSpace(opts[i].ReplacedBy)
		trimAll(opts[i].Children)
	}
}
