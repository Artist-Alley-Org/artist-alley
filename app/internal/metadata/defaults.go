// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// zeroUUID stands in for the asset and field ids when validation runs
// buildUpsertParams purely for its type checking. Neither id reaches
// storage on that path — the params are discarded.
var zeroUUID = pgtype.UUID{}

// ---------------------------------------------------------------------------
// Upload defaults (#793) — ADR 0081 §3
// ---------------------------------------------------------------------------
//
// A default answers "what should this field be when nothing else says",
// and it is exactly one of two things:
//
//   1. a literal value valid for the field's type, and for a vocabulary
//      type a slug that exists in the field's options and is ACTIVE;
//   2. a named context value from a closed set the server resolves.
//
// It is never an expression. The prior art keeps executable PHP in
// `autocomplete_macro` / `onchange_macro`; that surface cannot be
// validated on write, it is an injection vector, and a field definition
// carrying it cannot cross a federation boundary. When a value must
// genuinely be COMPUTED, extraction_source / extraction_mode already
// exist for that and have a whole pipeline behind them.
//
// The literal shape mirrors AssetFieldValueWrite deliberately — same
// value_* names, same meanings — because the applier converts a default
// into exactly that struct and hands it to buildUpsertParams. The
// column a default lands in is therefore the column the manual write
// path picks, by construction rather than by a second switch statement
// that has to be kept in step. That is the #778 lesson: three writers
// with three opinions about `tree` and nothing to catch it.

// SetByDefault is the asset_field_value.set_by provenance for a value a
// default put there.
//
// It exists so the extraction applier can tell "a default is sitting
// here" from "a human typed this". Without it, skip_if_set — which 13
// of the 15 live field definitions use — sees only that a value is
// PRESENT and skips, so a default written at creation would beat every
// later extraction. That is the inverse of ADR 0081 §3's precedence.
const SetByDefault = "default"

// FieldDefaultKind discriminates the two default shapes. There is no
// third, and adding one is an ADR-level change, not a column value.
type FieldDefaultKind string

const (
	// DefaultKindLiteral carries a value in whichever value_* member
	// matches the field's type.
	DefaultKindLiteral FieldDefaultKind = "literal"
	// DefaultKindContext names something the server resolves at
	// creation time from the upload's own context.
	DefaultKindContext FieldDefaultKind = "context"
)

// DefaultContext is the closed enumeration of server-resolved values.
// Closed is the point: an operator picks from a list, the server knows
// how to answer each entry, and nothing an operator writes is evaluated.
type DefaultContext string

const (
	// ContextUploadingUser is the uploading user's display name
	// (fullname, falling back to username).
	ContextUploadingUser DefaultContext = "uploading_user"
	// ContextUploadingTeam is the name of the team the upload belongs
	// to. Unresolvable when the uploader's team is ambiguous — see
	// resolveTeamForDefaults.
	ContextUploadingTeam DefaultContext = "uploading_team"
	// ContextCurrentDate is the moment the asset was created.
	ContextCurrentDate DefaultContext = "current_date"
)

// contextTargetType maps each context value to the ONE storage column
// it can populate, expressed as the field types that use that column.
//
// ADR 0081 §3 also listed "the target collection". It is not here, and
// that is a correction to the ADR rather than an omission: an asset is
// created by POST /assets, which has no collection in scope at all —
// collection membership is a later, separate write. A context value the
// resolver can never answer is a menu entry that silently does nothing,
// which is precisely the failure mode #774 established we do not ship.
var contextTargetType = map[DefaultContext]valueKind{
	ContextUploadingUser: kindText,
	ContextUploadingTeam: kindText,
	ContextCurrentDate:   kindDate,
}

// valueKind is the coarse storage shape a default resolves into. It is
// intentionally NOT a second copy of valueColumnFor — it exists only to
// answer "can this context value populate this field type", and it
// derives its answer from the same place everything else does.
type valueKind int

const (
	kindText valueKind = iota
	kindNum
	kindDate
	kindOptions
	kindRef
)

// kindForFieldType is the single mapping from a field type to the
// storage shape its value takes. valuecolumn_test.go pins that this
// agrees with valueColumnFor for every accepted type, so a new field
// type cannot be added to one and forgotten in the other.
func kindForFieldType(fieldType string) (valueKind, bool) {
	switch fieldType {
	case "text", "longtext", "rich_text", "select", "tree":
		return kindText, true
	case "number", "boolean":
		return kindNum, true
	case "date", "datetime":
		return kindDate, true
	case "multi_select":
		return kindOptions, true
	case "reference":
		return kindRef, true
	}
	return 0, false
}

// FieldDefault is the stored document. Marshalled into
// field_definition.default_value and field_default_override.default_value.
type FieldDefault struct {
	Kind FieldDefaultKind `json:"kind"`

	// Literal payload. Exactly one is set, and which one is decided by
	// the field's type — same rule, same names as AssetFieldValueWrite.
	ValueText    *string    `json:"value_text,omitempty"`
	ValueNum     *float64   `json:"value_num,omitempty"`
	ValueDate    *time.Time `json:"value_date,omitempty"`
	ValueOptions []string   `json:"value_options,omitempty"`
	ValueRef     *uuid.UUID `json:"value_ref,omitempty"`

	// Context payload.
	Context DefaultContext `json:"context,omitempty"`
}

// DefaultContexts lists the closed set in a stable order, for the admin
// UI and for the error message a bad context produces. Sorted so the
// message is deterministic.
func DefaultContexts() []DefaultContext {
	out := make([]DefaultContext, 0, len(contextTargetType))
	for c := range contextTargetType {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ContextsForFieldType lists the context values an operator may pick
// for a field of this type. An empty result means this type takes
// literal defaults only, and the admin UI hides the context option
// rather than offering one that will 400.
func ContextsForFieldType(fieldType string) []DefaultContext {
	want, ok := kindForFieldType(fieldType)
	if !ok {
		return nil
	}
	var out []DefaultContext
	for _, c := range DefaultContexts() {
		if contextTargetType[c] == want {
			out = append(out, c)
		}
	}
	return out
}

// ParseFieldDefault decodes a stored or submitted default document.
// A nil/empty/JSON-null input is "no default", reported by the bool.
func ParseFieldDefault(raw []byte) (FieldDefault, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return FieldDefault{}, false, nil
	}
	var d FieldDefault
	if err := json.Unmarshal(raw, &d); err != nil {
		return FieldDefault{}, false, fmt.Errorf("default_value: %w", err)
	}
	return d, true, nil
}

// ValidateFieldDefault is the write-time gate. Everything that can be
// checked without an upload in hand is checked HERE, at the door, so a
// default that cannot apply can never be stored:
//
//   - the kind is one of the two;
//   - a literal populates exactly the one value_* member its field type
//     uses, and no other (a literal aimed at the wrong column is a value
//     nothing will ever read — the #778 failure, prevented rather than
//     detected later);
//   - for select / multi_select / tree, every slug exists in the field's
//     live options document AND is active. A deprecated or archived term
//     is rejected: defaulting to a retired term quietly spreads it,
//     which is the one thing the option lifecycle exists to stop;
//   - a context is a member of the closed set and targets the same
//     storage shape the field type uses.
//
// `options` is the field's raw options jsonb, read in the same request
// that performs the write, so the check is against live data rather
// than against what a doc says the shape is (ADR 0012's own documented
// options shape had drifted from every live row — see #774's neighbours).
func ValidateFieldDefault(fieldType string, options []byte, d FieldDefault) error {
	kind, ok := kindForFieldType(fieldType)
	if !ok {
		return fmt.Errorf("default_value: unknown field type %q", fieldType)
	}

	switch d.Kind {
	case DefaultKindContext:
		target, known := contextTargetType[d.Context]
		if !known {
			return fmt.Errorf(
				"default_value: unknown context %q — must be one of %s. "+
					"Context values are a closed set the server resolves; there is no expression language",
				d.Context, joinContexts(DefaultContexts()))
		}
		if target != kind {
			allowed := ContextsForFieldType(fieldType)
			if len(allowed) == 0 {
				return fmt.Errorf(
					"default_value: context %q cannot fill a %s field, and no context value can — use a literal",
					d.Context, fieldType)
			}
			return fmt.Errorf(
				"default_value: context %q cannot fill a %s field; the contexts that can are %s",
				d.Context, fieldType, joinContexts(allowed))
		}
		if hasLiteralPayload(d) {
			return fmt.Errorf("default_value: a context default must not also carry a literal value")
		}
		return nil

	case DefaultKindLiteral:
		if d.Context != "" {
			return fmt.Errorf("default_value: a literal default must not also name a context")
		}
		// Round-trip through the real writer. If buildUpsertParams
		// accepts it, the value lands in the pinned column; if it
		// refuses, the operator gets the same message a manual write
		// would have given them.
		write, err := d.toAssetWrite()
		if err != nil {
			return err
		}
		if _, err := buildUpsertParams(zeroUUID, zeroUUID, fieldType, write, nil); err != nil {
			return fmt.Errorf("default_value: %w", err)
		}
		// Reject payload aimed at a column this type does not use, which
		// buildUpsertParams tolerates (it reads only the member it wants).
		if err := onlyExpectedLiteralSet(fieldType, kind, d); err != nil {
			return err
		}
		return validateDefaultSlugs(fieldType, options, d)

	case "":
		return fmt.Errorf(`default_value: "kind" is required — either "literal" or "context"`)
	default:
		return fmt.Errorf(`default_value: unknown kind %q — must be "literal" or "context"`, d.Kind)
	}
}

// validateDefaultSlugs enforces the vocabulary half: a default may only
// name terms that exist and are offerable.
//
// The membership walk itself is checkVocabulary — the same rule the
// asset and collection value writers apply (#824). A default asks it
// with NO held value, which is the strict form: nothing is
// grandfathered, so a deprecated or archived term is refused outright
// rather than merely refused as a change. That is the right question
// here for a reason the value path does not share — a default is not
// one record's value, it is the value every future asset gets, so
// there is no existing holder to protect and every reason not to
// spread a retired term.
//
// Only the wording is local: this path can say what a DEFAULT may
// name, which is more useful to an operator editing a field definition
// than the generic value-path message.
func validateDefaultSlugs(fieldType string, options []byte, d FieldDefault) error {
	slugs := vocabularySlugs(fieldType, d.ValueText, d.ValueOptions)
	rej := checkVocabulary(fieldType, options, slugs, nil)
	if rej == nil {
		return nil
	}
	if rej.unknown() {
		return fmt.Errorf(
			"default_value: %q is not an option of this field — a default may only name a term the field actually offers",
			rej.Slug)
	}
	return fmt.Errorf(
		"default_value: option %q is %s and cannot be a default — "+
			"defaulting to a retired term would quietly spread it onto every new asset",
		rej.Slug, rej.Status)
}

// hasLiteralPayload reports whether any value_* member is populated.
func hasLiteralPayload(d FieldDefault) bool {
	return d.ValueText != nil || d.ValueNum != nil || d.ValueDate != nil ||
		d.ValueOptions != nil || d.ValueRef != nil
}

// onlyExpectedLiteralSet rejects a literal that populates a column its
// field type does not use. buildUpsertParams reads only the member it
// needs, so `{"kind":"literal","value_text":"x","value_num":3}` on a
// number field would be accepted and the value_text silently dropped —
// an operator who mis-set the field type would see their default vanish
// with no error.
func onlyExpectedLiteralSet(fieldType string, want valueKind, d FieldDefault) error {
	set := map[valueKind]string{}
	if d.ValueText != nil {
		set[kindText] = "value_text"
	}
	if d.ValueNum != nil {
		set[kindNum] = "value_num"
	}
	if d.ValueDate != nil {
		set[kindDate] = "value_date"
	}
	if d.ValueOptions != nil {
		set[kindOptions] = "value_options"
	}
	if d.ValueRef != nil {
		set[kindRef] = "value_ref"
	}
	for k, name := range set {
		if k != want {
			return fmt.Errorf(
				"default_value: %s is set but a %s field's value lives elsewhere — "+
					"populate only the member this type uses",
				name, fieldType)
		}
	}
	return nil
}

// toAssetWrite renders a LITERAL default as the same write body the
// manual PUT path takes. Returns an error for a context default, which
// must be resolved first.
func (d FieldDefault) toAssetWrite() (*openapi.AssetFieldValueWrite, error) {
	if d.Kind != DefaultKindLiteral {
		return nil, fmt.Errorf("default_value: %q default has no literal value", d.Kind)
	}
	if !hasLiteralPayload(d) {
		return nil, fmt.Errorf("default_value: a literal default carries no value")
	}
	out := &openapi.AssetFieldValueWrite{}
	if d.ValueText != nil {
		v := *d.ValueText
		out.ValueText = &v
	}
	if d.ValueNum != nil {
		v := float32(*d.ValueNum)
		out.ValueNum = &v
	}
	if d.ValueDate != nil {
		v := *d.ValueDate
		out.ValueDate = &v
	}
	if d.ValueOptions != nil {
		v := append([]string(nil), d.ValueOptions...)
		out.ValueOptions = &v
	}
	if d.ValueRef != nil {
		v := openapi_types.UUID(*d.ValueRef)
		out.ValueRef = &v
	}
	return out, nil
}

// DefaultResolveContext is everything the server knows about an upload
// that a context default is allowed to see.
//
// Flat, typed, and small on purpose — the same reasoning ADR 0081's
// email amendment applies to template context. A resolver handed the
// whole identity or the whole asset row would let a future context
// value reach anything that happens to sit beside it.
type DefaultResolveContext struct {
	// UserDisplay is the uploading user's display name.
	UserDisplay string
	// TeamName is the name of the team this upload belongs to, empty
	// when there is no unambiguous team.
	TeamName string
	// Now is the asset's creation instant.
	Now time.Time
}

// ResolveFieldDefault turns a stored default into the write body the
// asset path applies. Reports false when the default cannot be resolved
// for this upload — an unresolvable context is simply not applied,
// because a blank is honest and a guess is not.
func ResolveFieldDefault(fieldType string, d FieldDefault, rc DefaultResolveContext) (*openapi.AssetFieldValueWrite, bool) {
	switch d.Kind {
	case DefaultKindLiteral:
		w, err := d.toAssetWrite()
		if err != nil {
			return nil, false
		}
		return w, true

	case DefaultKindContext:
		switch d.Context {
		case ContextUploadingUser:
			v := strings.TrimSpace(rc.UserDisplay)
			if v == "" {
				return nil, false
			}
			return &openapi.AssetFieldValueWrite{ValueText: &v}, true
		case ContextUploadingTeam:
			v := strings.TrimSpace(rc.TeamName)
			if v == "" {
				return nil, false
			}
			return &openapi.AssetFieldValueWrite{ValueText: &v}, true
		case ContextCurrentDate:
			if rc.Now.IsZero() {
				return nil, false
			}
			t := rc.Now
			// A `date` field carries a day, not an instant. Truncating
			// here rather than at read time keeps every reader from
			// having to know which of the two types it is looking at.
			if fieldType == "date" {
				t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
			}
			return &openapi.AssetFieldValueWrite{ValueDate: &t}, true
		}
	}
	return nil, false
}

func joinContexts(cs []DefaultContext) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = string(c)
	}
	return strings.Join(parts, ", ")
}
