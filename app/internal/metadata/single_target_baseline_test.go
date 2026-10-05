// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE SINGLE-TARGET BASELINE — Class B counterweights for the batch
// metadata editor (#1173, #1119, ADR 0019).
//
// # Why these exist
//
// Every assertion here PASSES ON dev@80028e36 AND AFTER, unchanged.
// They are the counterweights that prove the batch REPRODUCES shipped
// behaviour rather than inventing a second, subtly different rule.
//
// Two of the three describe behaviour nothing else in the suite pins
// down:
//
//   - B7. The field's own `write_capability` is GLOBAL-ONLY on the
//     single-target writer, while its READ gate is team-scope aware.
//     That asymmetry is REAL, it is NOT FIXED by this sprint, and the
//     batch reproduces it exactly. Nothing asserted it before, so
//     "fixing" it in the batch alone would have looked like a
//     divergence in the batch rather than what it is.
//   - B10. Per-type EMPTY and WHITESPACE behaviour on the single-target
//     writer, which is the semantic baseline the batch's per-type table
//     must match. In particular: `text` stores "   " UNTRIMMED, and
//     `select` stores "" while REFUSING "   " as an unknown slug —
//     a split that reads like a bug until you see that "" never enters
//     the vocabulary pipeline and "   " does.
//
// If a later sprint fixes the write_capability asymmetry, B7 is the
// test that must change, and its failure is the signal that the batch's
// G4 needs to move to per-target with it.
package metadata_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// setSingle drives the SHIPPED single-target writer.
func (f *batchFixture) setSingle(
	ctx context.Context, asset, field uuid.UUID, v openapi.AssetFieldValueWrite,
) (int, string) {
	f.t.Helper()
	resp, err := f.h.SetAssetFieldValue(ctx, openapi.SetAssetFieldValueRequestObject{
		Id: openapi_types.UUID(asset), FieldId: openapi_types.UUID(field), Body: &v,
	})
	if err != nil {
		f.t.Fatalf("single-target write: %v", err)
	}
	switch r := resp.(type) {
	case openapi.SetAssetFieldValue200JSONResponse:
		return 200, ""
	case openapi.SetAssetFieldValue400JSONResponse:
		return 400, r.Error
	case openapi.SetAssetFieldValue403JSONResponse:
		return 403, r.Error
	case openapi.SetAssetFieldValue422JSONResponse:
		return 422, string(r.Reason)
	default:
		f.t.Fatalf("unexpected single-target response %T", resp)
		return 0, ""
	}
}

func writeText(s string) openapi.AssetFieldValueWrite {
	v := s
	return openapi.AssetFieldValueWrite{ValueText: &v}
}

// ---------------------------------------------------------------------------
// B7 — write_capability is GLOBAL-ONLY on the single-target writer
// ---------------------------------------------------------------------------

// The field's write gate is `id.Can(code)` with NO InTeam, while
// fieldReadableOnSubject prefers the scoped disjunct when the subject
// has a team. This asserts BOTH halves, so the asymmetry is visible as
// an asymmetry rather than as two unrelated facts.
//
// ⛔ NOT A BUG REPORT AND NOT A FIX. It records the shipped rule so the
// batch's reproduction of it is provably a reproduction.
func TestSingleTarget_WriteCapabilityIsGlobalOnly(t *testing.T) {
	f := newBatchFixture(t)
	team := f.team("b7")

	t.Run("a TEAM-SCOPED holding does NOT satisfy the write gate", func(t *testing.T) {
		user := f.user("b7scoped")
		f.grant(user, "fields.admin", &team)
		ctx := f.identity(user)
		field := f.field("wc", fieldSpec{Type: "text", WriteCapability: "fields.admin"})
		asset := f.asset(&user, &team)

		status, _ := f.setSingle(ctx, asset, field, writeText("x"))
		if status != 403 {
			t.Fatalf("the write gate is GLOBAL-ONLY; a scoped holding must not satisfy it, got %d", status)
		}
		if f.rowExists(asset, field) {
			t.Fatal("nothing may be written")
		}
	})

	t.Run("a GLOBAL holding does", func(t *testing.T) {
		user := f.user("b7global")
		f.grant(user, "fields.admin", nil)
		ctx := f.identity(user)
		field := f.field("wc", fieldSpec{Type: "text", WriteCapability: "fields.admin"})
		asset := f.asset(&user, &team)

		if status, msg := f.setSingle(ctx, asset, field, writeText("x")); status != 200 {
			t.Fatalf("a global holding must pass, got %d %s", status, msg)
		}
	})

	// THE OTHER HALF OF THE ASYMMETRY: the READ gate on the very same
	// capability IS team-scope aware, so a scoped holder who cannot
	// write the field can read it on a subject in that team.
	t.Run("the READ gate on the same capability IS team-scope aware", func(t *testing.T) {
		user := f.user("b7read")
		f.grant(user, "fields.admin", &team)
		ctx := f.identity(user)
		field := f.field("rc", fieldSpec{Type: "text", ReadCapability: "fields.admin"})
		asset := f.asset(&user, &team)
		f.setValue(asset, field, map[string]any{"text": "readable"})

		resp, err := f.h.GetAssetFields(ctx, openapi.GetAssetFieldsRequestObject{
			Id: openapi_types.UUID(asset),
		})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		list, ok := resp.(openapi.GetAssetFields200JSONResponse)
		if !ok {
			t.Fatalf("unexpected read response %T", resp)
		}
		found := false
		for _, v := range list {
			if uuid.UUID(v.FieldId) == field {
				found = true
			}
		}
		if !found {
			t.Fatal("the READ gate honours a TEAM-SCOPED holding; the write gate does not. " +
				"That asymmetry is the shipped behaviour this test records.")
		}
	})
}

// ---------------------------------------------------------------------------
// B10 — per-type EMPTY and WHITESPACE on the single-target writer
// ---------------------------------------------------------------------------

// The batch's per-type table must reproduce exactly this. Standalone,
// because nothing else asserts the select/tree split and it is the row
// most likely to be "tidied" into agreeing with text.
func TestSingleTarget_PerTypeEmptyAndWhitespace(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("b10")
	ctx := f.identity(owner)
	vocab := []map[string]any{vocabOption("a", "A", "active")}

	t.Run("OPTIONAL", func(t *testing.T) {
		cases := []struct {
			name       string
			typ        string
			opts       []map[string]any
			send       string
			wantStatus int
			wantStored *string
		}{
			{"text empty stores ''", "text", nil, "", 200, strp("")},
			{"text whitespace stores UNTRIMMED", "text", nil, "   ", 200, strp("   ")},
			{"longtext empty stores ''", "longtext", nil, "", 200, strp("")},
			{"longtext whitespace stores UNTRIMMED", "longtext", nil, "   ", 200, strp("   ")},
			{"rich_text semantic-empty stores the SANITISED markup", "rich_text", nil, "<p><br></p>", 200, strp("<p><br></p>")},
			// ⚠️ THE SPLIT. "" returns nil from vocabularySlugs and never
			// enters the vocabulary pipeline; "   " enters it as the slug
			// "   ", canonicalises to itself, and misses.
			{"select empty STORES", "select", vocab, "", 200, strp("")},
			{"select whitespace is REFUSED as an unknown slug", "select", vocab, "   ", 422, nil},
			{"tree empty STORES", "tree", vocab, "", 200, strp("")},
			{"tree whitespace is REFUSED as an unknown slug", "tree", vocab, "   ", 422, nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				field := f.field("b10", fieldSpec{Type: tc.typ, Options: tc.opts})
				asset := f.asset(&owner, nil)
				status, msg := f.setSingle(ctx, asset, field, writeText(tc.send))
				if status != tc.wantStatus {
					t.Fatalf("want %d, got %d (%s)", tc.wantStatus, status, msg)
				}
				if tc.wantStored == nil {
					if f.rowExists(asset, field) {
						t.Fatal("a refused value must not be stored")
					}
					return
				}
				got, present := f.storedText(asset, field)
				if !present || got != *tc.wantStored {
					t.Fatalf("want %q stored, got %q (present=%v)", *tc.wantStored, got, present)
				}
			})
		}
	})

	// REQUIRED refuses every one of them by R1, and R1 sits ABOVE the
	// vocabulary gate — which is why the whitespace select case answers
	// `field_required` here rather than the unknown-slug refusal it
	// gets when the field is optional.
	t.Run("REQUIRED refuses all of them by R1", func(t *testing.T) {
		cases := []struct {
			name string
			typ  string
			opts []map[string]any
			send string
		}{
			{"text empty", "text", nil, ""},
			{"text whitespace", "text", nil, "   "},
			{"longtext empty", "longtext", nil, ""},
			{"rich_text semantic-empty", "rich_text", nil, "<p><br></p>"},
			{"select empty", "select", vocab, ""},
			{"select whitespace", "select", vocab, "   "},
			{"tree empty", "tree", vocab, ""},
			{"tree whitespace", "tree", vocab, "   "},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				field := f.field("b10r", fieldSpec{Type: tc.typ, Required: true, Options: tc.opts})
				asset := f.asset(&owner, nil)
				status, reason := f.setSingle(ctx, asset, field, writeText(tc.send))
				if status != 422 {
					t.Fatalf("want 422, got %d", status)
				}
				if reason != string(openapi.FieldRequired) {
					t.Fatalf("R1 sits ABOVE the vocabulary gate; want field_required, got %q", reason)
				}
				if f.rowExists(asset, field) {
					t.Fatal("nothing may be stored")
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// B6 — the single-target BLIND WRITE is unchanged
// ---------------------------------------------------------------------------

// The ordinary unguarded write is last-write-wins and asks NOTHING about
// asset ownership — which the upload flush depends on. The batch adds a
// subject gate; THE SINGLE-TARGET WRITER DOES NOT GET ONE IN THIS
// SPRINT, and this records that so the absence is deliberate rather than
// an oversight somebody closes by accident.
func TestSingleTarget_BlindWriteUnchanged(t *testing.T) {
	f := newBatchFixture(t)
	writer := f.user("b6writer")
	stranger := f.user("b6owner")
	ctx := f.identity(writer)

	field := f.field("b6", fieldSpec{Type: "text"})
	notTheirs := f.asset(&stranger, nil)

	// No guard, no ownership: 200, last write wins.
	if status, msg := f.setSingle(ctx, notTheirs, field, writeText("first")); status != 200 {
		t.Fatalf("the ordinary writer asks nothing about ownership; got %d %s", status, msg)
	}
	if status, _ := f.setSingle(ctx, notTheirs, field, writeText("second")); status != 200 {
		t.Fatalf("last write wins; got %d", status)
	}
	if got, _ := f.storedText(notTheirs, field); got != "second" {
		t.Fatalf("want the last write, got %q", got)
	}
}
