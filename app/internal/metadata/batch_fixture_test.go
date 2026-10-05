// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The batch editor's own fixture: the two endpoints, the token's
// durable state, and the audit envelope (#1173, #1119, ADR 0019).
//
// Split from metadata_fixture_test.go deliberately. Everything in that
// file describes the world the SINGLE-TARGET writer already lives in —
// users, teams, grants, assets, posts, field definitions, stored values
// — and compiles against dev@80028e36 unchanged, which is what lets the
// Class B counterweights RUN there and prove they pass before and
// after. Everything here names a type, a table or an endpoint that does
// not exist at that commit, so keeping the two apart is what makes the
// baseline demonstrable at all.
package metadata_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const capBulkEdit = "assets.metadata.bulk_edit"

// bulkOperator is the ordinary principal: it may reach for the
// instrument, it owns what it edits, and nothing else is granted.
func (f *batchFixture) bulkOperator(label string) (int64, context.Context) {
	f.t.Helper()
	ref := f.user(label)
	f.grant(ref, capBulkEdit, nil)
	return ref, f.identity(ref)
}

// envelopes counts the audit envelopes for one operation id, which is
// how EXACTLY ONE PER TOKEN is asserted against the DATABASE rather
// than against a response body.
func (f *batchFixture) envelopes(operationID string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_events
		  WHERE event_type = $1 AND metadata->>'operation_id' = $2`,
		audit.EventAssetFieldBatchEditApplied, operationID).Scan(&n); err != nil {
		f.t.Fatalf("count envelopes: %v", err)
	}
	return n
}

func (f *batchFixture) envelope(operationID string) map[string]any {
	f.t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(f.ctx,
		`SELECT metadata FROM audit_events
		  WHERE event_type = $1 AND metadata->>'operation_id' = $2`,
		audit.EventAssetFieldBatchEditApplied, operationID).Scan(&raw); err != nil {
		f.t.Fatalf("read envelope: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatalf("decode envelope: %v", err)
	}
	return out
}

func (f *batchFixture) tokenConsumed(operationID string) bool {
	f.t.Helper()
	var consumed *time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT consumed_at FROM metadata_batch_preview WHERE id = $1`, operationID).Scan(&consumed); err != nil {
		f.t.Fatalf("read consumed_at: %v", err)
	}
	return consumed != nil
}

func (f *batchFixture) expireToken(operationID string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE metadata_batch_preview SET expires_at = NOW() - INTERVAL '1 hour' WHERE id = $1`,
		operationID); err != nil {
		f.t.Fatalf("expire token: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Driving the two endpoints
// ---------------------------------------------------------------------------

func assetEntries(ids ...uuid.UUID) []openapi.BatchAssetFieldSelectionEntry {
	out := make([]openapi.BatchAssetFieldSelectionEntry, 0, len(ids))
	for _, id := range ids {
		out = append(out, openapi.BatchAssetFieldSelectionEntry{
			Kind: openapi.BatchSelectionAsset, Id: openapi_types.UUID(id),
		})
	}
	return out
}

func postEntries(ids ...uuid.UUID) []openapi.BatchAssetFieldSelectionEntry {
	out := make([]openapi.BatchAssetFieldSelectionEntry, 0, len(ids))
	for _, id := range ids {
		out = append(out, openapi.BatchAssetFieldSelectionEntry{
			Kind: openapi.BatchSelectionPost, Id: openapi_types.UUID(id),
		})
	}
	return out
}

func textValue(s string) openapi.BatchAssetFieldValue {
	v := s
	return openapi.BatchAssetFieldValue{ValueText: &v}
}

func optionsValue(slugs ...string) openapi.BatchAssetFieldValue {
	v := append([]string(nil), slugs...)
	return openapi.BatchAssetFieldValue{ValueOptions: &v}
}

func numValue(n float32) openapi.BatchAssetFieldValue {
	return openapi.BatchAssetFieldValue{ValueNum: &n}
}

func dateValue(t time.Time) openapi.BatchAssetFieldValue {
	return openapi.BatchAssetFieldValue{ValueDate: &t}
}

func refValue(id uuid.UUID) openapi.BatchAssetFieldValue {
	v := openapi_types.UUID(id)
	return openapi.BatchAssetFieldValue{ValueRef: &v}
}

// previewResult carries whichever arm the preview took, so an assertion
// can name the one it expects rather than type-switching.
type previewResult struct {
	OK      *openapi.BatchAssetFieldPreview
	Status  int
	Refusal *openapi.BatchAssetFieldRefusal
	Raw     openapi.PreviewBatchAssetFieldEditResponseObject
}

func (f *batchFixture) preview(
	ctx context.Context,
	mode openapi.BatchAssetFieldMode,
	field uuid.UUID,
	value openapi.BatchAssetFieldValue,
	selection []openapi.BatchAssetFieldSelectionEntry,
) previewResult {
	f.t.Helper()
	body := openapi.BatchAssetFieldPreviewRequest{
		Mode: mode, FieldId: openapi_types.UUID(field), Value: value, Selection: selection,
	}
	resp, err := f.h.PreviewBatchAssetFieldEdit(ctx,
		openapi.PreviewBatchAssetFieldEditRequestObject{Body: &body})
	if err != nil {
		f.t.Fatalf("preview: %v", err)
	}
	out := previewResult{Raw: resp}
	switch v := resp.(type) {
	case openapi.PreviewBatchAssetFieldEdit200JSONResponse:
		p := openapi.BatchAssetFieldPreview(v)
		out.OK, out.Status = &p, 200
	case openapi.PreviewBatchAssetFieldEdit400JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 400
	case openapi.PreviewBatchAssetFieldEdit403JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 403
	case openapi.PreviewBatchAssetFieldEdit422JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 422
	case openapi.PreviewBatchAssetFieldEdit401JSONResponse:
		out.Status = 401
	case openapi.PreviewBatchAssetFieldEdit404JSONResponse:
		out.Status = 404
	default:
		f.t.Fatalf("unexpected preview response %T", resp)
	}
	return out
}

// mustPreview fails on anything but a 200, so a happy-path test does
// not silently assert against a refusal body.
func (f *batchFixture) mustPreview(
	ctx context.Context,
	mode openapi.BatchAssetFieldMode,
	field uuid.UUID,
	value openapi.BatchAssetFieldValue,
	selection []openapi.BatchAssetFieldSelectionEntry,
) *openapi.BatchAssetFieldPreview {
	f.t.Helper()
	res := f.preview(ctx, mode, field, value, selection)
	if res.OK == nil {
		f.t.Fatalf("expected a preview, got %d %+v", res.Status, res.Refusal)
	}
	return res.OK
}

type applyResult struct {
	OK      *openapi.BatchAssetFieldApplyResult
	Status  int
	Refusal *openapi.BatchAssetFieldRefusal
}

func (f *batchFixture) apply(ctx context.Context, token, reason string, confirm *int) applyResult {
	f.t.Helper()
	body := openapi.BatchAssetFieldApplyRequest{Token: token, Reason: reason, ConfirmCount: confirm}
	resp, err := f.h.ApplyBatchAssetFieldEdit(ctx,
		openapi.ApplyBatchAssetFieldEditRequestObject{Body: &body})
	if err != nil {
		f.t.Fatalf("apply: %v", err)
	}
	out := applyResult{}
	switch v := resp.(type) {
	case openapi.ApplyBatchAssetFieldEdit200JSONResponse:
		r := openapi.BatchAssetFieldApplyResult(v)
		out.OK, out.Status = &r, 200
	case openapi.ApplyBatchAssetFieldEdit400JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 400
	case openapi.ApplyBatchAssetFieldEdit403JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 403
	case openapi.ApplyBatchAssetFieldEdit409JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 409
	case openapi.ApplyBatchAssetFieldEdit422JSONResponse:
		r := openapi.BatchAssetFieldRefusal(v)
		out.Refusal, out.Status = &r, 422
	case openapi.ApplyBatchAssetFieldEdit401JSONResponse:
		out.Status = 401
	default:
		f.t.Fatalf("unexpected apply response %T", resp)
	}
	return out
}

func intp(n int) *int { return &n }

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

func (f *batchFixture) wantRefusal(got applyResult, status int, reason openapi.BatchAssetFieldRefusalReason) {
	f.t.Helper()
	if got.Status != status || got.Refusal == nil || got.Refusal.Reason != reason {
		f.t.Fatalf("want %d %s, got %d %+v", status, reason, got.Status, got.Refusal)
	}
}

func (f *batchFixture) wantPreviewRefusal(got previewResult, status int, reason openapi.BatchAssetFieldRefusalReason) {
	f.t.Helper()
	if got.Status != status || got.Refusal == nil || got.Refusal.Reason != reason {
		f.t.Fatalf("want %d %s, got %d %+v", status, reason, got.Status, got.Refusal)
	}
}

// partitionOf reports one target's preview partition.
func partitionOf(p *openapi.BatchAssetFieldPreview, id uuid.UUID) (openapi.BatchAssetFieldPartition, bool) {
	for _, t := range p.Targets {
		if uuid.UUID(t.AssetId) == id {
			return t.Partition, true
		}
	}
	return "", false
}

func outcomeOf(r *openapi.BatchAssetFieldApplyResult, id uuid.UUID) (openapi.BatchAssetFieldOutcome, bool) {
	for _, t := range r.Targets {
		if uuid.UUID(t.AssetId) == id {
			return t.Outcome, true
		}
	}
	return "", false
}

// assertReconciles is the partition arithmetic, asserted rather than
// described. Every target of a successful preview belongs to exactly
// one partition, so this is a real invariant.
func assertReconciles(t *testing.T, c openapi.BatchAssetFieldCounts) {
	t.Helper()
	sum := c.WouldChange + c.NoOp + c.Refused + c.Inapplicable + c.Unreadable + c.Unauthorized
	if c.Expanded != sum {
		t.Fatalf("expanded %d != sum of partitions %d (%+v)", c.Expanded, sum, c)
	}
	if c.Eligible != c.WouldChange+c.NoOp {
		t.Fatalf("eligible %d != would_change + no_op (%+v)", c.Eligible, c)
	}
}

// assertApplyReconciles is the second equation:
// would_change = changed + conflict + gone + unauthorized_at_apply + error
func assertApplyReconciles(t *testing.T, r *openapi.BatchAssetFieldApplyResult) {
	t.Helper()
	o := r.OutcomeCounts
	sum := o.Changed + o.Conflict + o.Gone + o.UnauthorizedAtApply + o.Error
	if r.Counts.WouldChange != sum {
		t.Fatalf("would_change %d != changed+conflict+gone+unauthorized+error %d (%+v)",
			r.Counts.WouldChange, sum, o)
	}
}
