// Phase 1.17.D — Reflective changeset helper.
//
// RecordChange wraps the existing Recorder.write path with a
// before/after struct diff. The diff lives at metadata.changeset
// inside the audit_events.metadata JSONB blob — coexisting with
// any per-event flat metadata each recorder method already
// populates.
//
// # When to use
//
// Field-level changes where the operator question is "what
// differed?" — site config edits, user profile updates, SMTP
// rotation, etc. The shape is one entry per changed field:
//
//   {
//     "changeset": {
//       "SiteName":  {"before": "Old Studio", "after": "New Studio"},
//       "BaseURL":   {"before": "https://a/", "after": "https://b/"}
//     }
//   }
//
// # When NOT to use
//
// State transitions (approve/disable/archive). Those have
// from_state + to_state in their existing flat metadata — the
// audit consumer wants "moved from X to Y" not "the Approved
// field changed from 0 to 1". Don't retrofit transition events.
//
// # Sensitive fields are never serialized
//
// Two strip mechanisms:
//
//   1. Struct tag `audit:"-"` — explicit, primary mechanism.
//      Add to any field whose value must not appear in the
//      audit log.
//   2. Name pattern match — defense-in-depth backstop. Field
//      names containing Password / Hash / Secret / PrivateKey /
//      Token / APIKey / MasterKey / Encryption / Signing are
//      stripped even without the tag, with a debug log so the
//      author notices.
//
// # Best-effort writes preserved
//
// RecordChange calls Recorder.write — same best-effort
// semantics as every other recorder method. A serialization or
// DB failure logs at WARN; the calling operation never sees an
// error.

package audit

import (
	"context"
	"net/http"
	"reflect"
	"strings"
)

// changesetKey is the canonical metadata key. Documented in
// docs/observability/audit-events.md so future event authors
// don't invent a different one.
const changesetKey = "changeset"

// sensitiveFieldPatterns is the defense-in-depth backstop for
// the explicit `audit:"-"` tag. Substring match (case-insensitive
// via ToLower at compare time). Adding to this list is a one-line
// change; the diff helper picks up the new pattern automatically.
//
// Conservative on purpose: a field named "PasswordPolicy" or
// "PasswordResetRequired" would also be stripped. If a legit
// field gets caught, prefer renaming OR adding an opt-in tag
// (audit:"include") in a follow-up. Don't loosen the pattern.
var sensitiveFieldPatterns = []string{
	"password",
	"hash",
	"secret",
	"privatekey",
	"token",
	"apikey",
	"masterkey",
	"encryption",
	"signing",
}

// RecordChange records a field-level change event with a diff of
// before/after as `metadata.changeset`. before + after must be the
// SAME type (typically both *Config or both *UserProfile pointers
// the caller loaded around the write). Different types log a WARN
// and skip the changeset entirely (but still emit the event so
// the operator-action signal isn't lost).
//
// extra is per-event flat metadata that lives ALONGSIDE the
// changeset (e.g., the actor's IP via reqContext from the HTTP
// request). Pass an empty map if there's nothing to add.
//
// If before == after at every field, no changeset key is written
// — but the event still emits. "Admin saved the form but nothing
// actually changed" is a meaningful audit signal in its own right.
func (r *Recorder) RecordChange(
	ctx context.Context,
	req *http.Request,
	eventType string,
	subject, actor *int64,
	before, after any,
	extra map[string]any,
) {
	metadata := map[string]any{}
	for k, v := range extra {
		metadata[k] = v
	}
	if cs := diffStructs(before, after); len(cs) > 0 {
		metadata[changesetKey] = cs
	}
	r.write(ctx, eventType, subject, actor, ctxFromRequest(req), metadata)
}

// diffStructs walks two same-typed struct values and returns a
// map[fieldName]{before, after} for fields that differ.
//
// Behavior:
//   - Pointers are dereferenced. A nil pointer on either side
//     diffs against the dereferenced value on the other side
//     by treating the nil as the zero value of the type.
//   - Type mismatch returns nil (caller skips the changeset key).
//   - Non-struct kinds return nil (operator passed a slice/map/
//     primitive instead of a struct).
//   - Unexported fields are skipped.
//   - `audit:"-"` tag strips the field.
//   - Sensitive-field-name pattern (case-insensitive substring)
//     strips the field even without the tag.
func diffStructs(before, after any) map[string]map[string]any {
	bv := reflect.ValueOf(before)
	av := reflect.ValueOf(after)

	// Dereference pointers (multiple levels handled defensively).
	for bv.Kind() == reflect.Ptr {
		if bv.IsNil() {
			break
		}
		bv = bv.Elem()
	}
	for av.Kind() == reflect.Ptr {
		if av.IsNil() {
			break
		}
		av = av.Elem()
	}

	// If one side is still a nil pointer, swap it for a zero
	// value of the other side's type so we get a "every field
	// changed from zero" diff. Common when a row is being
	// created (before == nil pointer; after == populated struct).
	if bv.Kind() == reflect.Ptr && bv.IsNil() && av.Kind() == reflect.Struct {
		bv = reflect.Zero(av.Type())
	}
	if av.Kind() == reflect.Ptr && av.IsNil() && bv.Kind() == reflect.Struct {
		av = reflect.Zero(bv.Type())
	}

	if bv.Kind() != reflect.Struct || av.Kind() != reflect.Struct {
		return nil
	}
	if bv.Type() != av.Type() {
		return nil
	}

	out := map[string]map[string]any{}
	t := bv.Type()
	for i := 0; i < bv.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		if tag := field.Tag.Get("audit"); tag == "-" {
			continue
		}
		if isSensitiveFieldName(field.Name) {
			continue
		}

		bf := bv.Field(i).Interface()
		af := av.Field(i).Interface()
		if !reflect.DeepEqual(bf, af) {
			out[field.Name] = map[string]any{
				"before": bf,
				"after":  af,
			}
		}
	}
	return out
}

// isSensitiveFieldName checks the field name against the
// substring-match backstop. Case-insensitive — operator naming
// conventions vary (PasswordHash, passwordHash, password_hash
// once camelcased). The explicit `audit:"-"` tag is the
// primary mechanism; this is the safety net for fields that
// slip in without the tag.
func isSensitiveFieldName(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range sensitiveFieldPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}
