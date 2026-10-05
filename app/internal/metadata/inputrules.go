// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// The two field-configuration settings that govern what a PERSON may
// write to a field: `read_only` and `regexp_filter` (#1173, ADR 0012,
// migration 00064).
//
// Everything here is about HUMAN input. The enforcement seam is the
// CALL SITE, not a flag: the four value handlers that call into this
// file all begin with `auth.IdentityFromContext` and 401 without an
// identity, while the writers that skip it — ApplyAssetDefaults, the
// extraction adapter's WriteAssetFieldValue, mirrorFill — are separate
// Go functions with no OpenAPI operation and no route. That is what
// makes the exemption non-forgeable: a client has nothing to send that
// would claim it.
//
// The consequence, stated so it is not mistaken for a bug: a value the
// system wrote may fail a pattern a person would be held to. Nothing
// rewrites stored values when a pattern is configured or changed.

// regexpFilterSupportedTypes lists the field types that honour a
// pattern. Two, and both store the operator's own text verbatim in
// `value_text`.
//
// This is a Go narrowing rather than a CHECK constraint, following
// `open_vocabulary`'s precedent (ADR 0012, 2026-08-02 amendment):
// widening it later should be a decision rather than a migration.
//
// `rich_text` is excluded even though it shares the column, and the
// reason is what the column HOLDS. `richtext.SanitizeValueText` runs
// before every write, so a rich-text value is policy-clean markup:
// `<p>Cleared for <strong>internal</strong> use.</p>`. A pattern would
// be matched against tags rather than against anything the operator
// can see — `^[A-Z]` fails on every value ever stored, and two values
// reading identically carry different markup. That is the same
// objection that keeps numbers and dates out: the stored form is not
// the form the rule is written about.
var regexpFilterSupportedTypes = []string{"text", "longtext"}

// regexpFilterApplies reports whether a field of this type honours a
// configured pattern.
func regexpFilterApplies(fieldType string) bool {
	for _, t := range regexpFilterSupportedTypes {
		if t == fieldType {
			return true
		}
	}
	return false
}

// regexpFilterSupportedList renders the supported types for an error
// message, so the sentence an operator reads cannot drift from the
// list the server enforces.
func regexpFilterSupportedList() string {
	return strings.Join(regexpFilterSupportedTypes, ", ")
}

// patternCache memoises compiled patterns.
//
// Unbounded on purpose and safe: the key space is the set of patterns
// configured on field definitions, which only a `fields.admin` caller
// can write and which is bounded by the number of fields. Values are
// compiled ONCE per pattern rather than per value write.
var patternCache sync.Map // map[string]compiledPattern

type compiledPattern struct {
	re  *regexp.Regexp
	err error
}

// compileFieldPattern compiles an operator's pattern into the form the
// server actually matches with.
//
// THE WRAPPING IS THE CONTRACT. A pattern describes the WHOLE value, so
// it is compiled as `\A(?:<pattern>)\z` rather than asking the operator
// to write `^…$` themselves. Two reasons, and both are failures a
// person writing the anchors by hand would hit:
//
//   - `^` and `$` are LINE anchors the moment a pattern turns on
//     `(?m)`, so `^AAA_\d{4}$` would happily accept a two-line value
//     whose second line is junk.
//   - anchors bind tighter than a top-level alternation, so `^a|b$`
//     means "starts with a, or ends with b". The non-capturing group is
//     what makes `a|b` mean "the whole value is a, or the whole value
//     is b", which is what the operator who wrote it meant.
//
// `\A` and `\z` are unaffected by `(?m)`, so a multiline pattern still
// obeys whole-value semantics.
//
// The operator's text is never trimmed or rewritten. It is wrapped at
// match time and stored verbatim.
//
// Go's regexp is RE2: no backtracking, linear time in the input. That
// is why accepting a free-text pattern from an operator is safe here at
// all, and it is the reason there is no length or complexity cap.
func compileFieldPattern(pattern string) (*regexp.Regexp, error) {
	if v, ok := patternCache.Load(pattern); ok {
		c := v.(compiledPattern)
		return c.re, c.err
	}
	re, err := regexp.Compile(`\A(?:` + pattern + `)\z`)
	if err != nil {
		// The compiler's message names the wrapped form, which would
		// point an operator at anchors they never wrote. Report the
		// syntax problem against their own pattern instead.
		err = fmt.Errorf("regexp_filter is not a valid pattern: %s", syntaxDetail(err))
		re = nil
	}
	patternCache.Store(pattern, compiledPattern{re: re, err: err})
	return re, err
}

// syntaxDetail strips regexp.Compile's `error parsing regexp: ` prefix
// and the quoted wrapped source, leaving the part that describes what
// is wrong.
func syntaxDetail(err error) string {
	msg := err.Error()
	const prefix = "error parsing regexp: "
	msg = strings.TrimPrefix(msg, prefix)
	if i := strings.LastIndex(msg, ": `"); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

// validateRegexpFilterConfig checks a pattern an operator is trying to
// CONFIGURE on a field, and returns the sentence to refuse it with, or
// "" when it is acceptable.
//
// Three refusals, in the order an operator meets them. All of them are
// about a NON-EMPTY configured pattern: the unset state is legal on
// every field of every type, which is what lets a setting always be
// taken back off.
//
// The blank case is handled by the caller, because the answer there is
// "send clear_regexp_filter" rather than "this field cannot have one".
func validateRegexpFilterConfig(cur FieldDefinition, pattern string) string {
	if col, ok := MirrorColumnOf(cur); ok {
		return fmt.Sprintf(
			"%s is a view onto assets.%s, so it cannot carry an input pattern: the asset's own create and update paths write that column too and would not apply one",
			cur.Code, col)
	}
	if !regexpFilterApplies(cur.Type) {
		if cur.Type == richTextType {
			return fmt.Sprintf(
				"a %s field stores sanitised markup rather than the text an operator typed, so a pattern would match tags instead of words; input patterns apply to %s fields",
				cur.Type, regexpFilterSupportedList())
		}
		return fmt.Sprintf(
			"input patterns apply to %s fields; %s is a %s field",
			regexpFilterSupportedList(), cur.Code, cur.Type)
	}
	if _, err := compileFieldPattern(pattern); err != nil {
		return err.Error()
	}
	return ""
}

// richTextType is named rather than spelled inline so the one place
// that treats it specially is greppable.
const richTextType = "rich_text"

// validateReadOnlyConfig checks a `read_only` an operator is trying to
// configure, and returns the refusal sentence or "".
//
// Only the non-default value is restricted. Turning the flag OFF is
// legal everywhere, so a field can always be made writable again.
func validateReadOnlyConfig(cur FieldDefinition, readOnly bool) string {
	if !readOnly {
		return ""
	}
	if col, ok := MirrorColumnOf(cur); ok {
		return fmt.Sprintf(
			"%s is a view onto assets.%s, so it cannot be made read-only: the asset's own create and update paths write that column too and would not obey the setting",
			cur.Code, col)
	}
	return ""
}

// readOnlyRefusal returns the sentence refusing a human write to a
// read-only field, or "" when the field is writable.
//
// `verb` is what the caller was trying to do, so the message says which
// of the two refusals happened rather than leaving an operator to guess
// why a delete failed.
func readOnlyRefusal(f FieldDefinition, verb string) string {
	if !f.ReadOnly {
		return ""
	}
	return fmt.Sprintf(
		"%s is read-only: its values cannot be %s here. Upload defaults and extraction still fill it; change the field's configuration to edit it by hand",
		f.Code, verb)
}

// patternRefusal checks a HUMAN-supplied value against the field's
// configured pattern and returns the refusal sentence, or "".
//
// `value` is the text that will actually be STORED, which for the two
// supported types is exactly what the caller sent: neither the asset
// nor the collection writer transforms `text` or `longtext` on the way
// in. Passing the stored form rather than the request field is what
// stops the rule and the row disagreeing.
//
// A value of "" is checked like any other. A field whose pattern should
// tolerate an empty value says so in the pattern; removing a value
// entirely is DELETE, which carries nothing to match.
//
// A pattern that no longer compiles cannot refuse anything, because it
// could only have been stored by a path that never validated it. It is
// reported rather than silently ignored, since a configuration that
// does nothing is the failure this whole setting exists to avoid.
func patternRefusal(f FieldDefinition, value *string) string {
	if f.RegexpFilter == nil || *f.RegexpFilter == "" || !regexpFilterApplies(f.Type) {
		return ""
	}
	if value == nil {
		return ""
	}
	re, err := compileFieldPattern(*f.RegexpFilter)
	if err != nil {
		return fmt.Sprintf("%s has an input pattern the server cannot use: %s", f.Code, err.Error())
	}
	if re.MatchString(*value) {
		return ""
	}
	return fmt.Sprintf("%s must match the pattern %s", f.Code, *f.RegexpFilter)
}
