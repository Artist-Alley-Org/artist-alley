// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// EventAssetFieldBatchEditApplied is the batch metadata editor's ONE
// event (#1173, #1119, ADR 0019). One constant and one typed method,
// per this package's convention: there is no generic Record here, so a
// new operation states its shape in a signature rather than in a map
// literal at the call site.
//
// No audit_retention_policy row is needed. A category with none falls
// back to DefaultRetention, which is seven years — comfortably longer
// than anybody would want a record of who rewrote a thousand fields.
const EventAssetFieldBatchEditApplied = "assets.metadata.batch_edit.applied"

// BatchAssetFieldEditEnvelope is the whole audit record of one applied
// batch. It is a struct rather than a map because every member here is
// a deliberate decision about disclosure, and a map literal at the call
// site is where the next member gets added without one.
//
// # What is deliberately NOT here
//
// NO FIELD VALUE. Not the value that was written, not the values that
// were replaced, not a redacted or truncated form of either.
//
// The audit log is read under `system.audit.read`, which the Auditor
// role holds. That is a DIFFERENT POPULATION from the people entitled
// to read the field: a field carrying a `read_capability` is readable
// by its grantees on subjects in scope, and an auditor is not thereby
// one of them. Putting the value in the envelope would make the audit
// log a side channel around the field's own read gate, and it would do
// it for a thousand records at a time.
//
// Nothing is lost by leaving it out. Every value this operation wrote
// is already recorded per target in `asset_field_value_history`, behind
// the field's own gate, which is exactly where a reader entitled to see
// values goes to see them.
//
// The same rule governs the targets that were NOT written: an
// `unreadable` or `refused` target contributes its id and its partition
// label and nothing else. "This asset was refused because the value it
// holds is a retired term" would disclose the held value to a reader
// who may not read it.
type BatchAssetFieldEditEnvelope struct {
	// OperationID ties the preview to the apply. It is the preview
	// row's own id, so an operator reading the audit feed can line one
	// envelope up against the token that produced it.
	OperationID string `json:"operation_id"`

	Mode      string `json:"mode"`
	FieldID   string `json:"field_id"`
	FieldCode string `json:"field_code"`

	// Reason is the operator's justification, TRIMMED and validated,
	// recorded VERBATIM. It is the one free-text member and it is the
	// point of the envelope: a bulk change with no undo is answerable
	// only if the person who made it said why.
	Reason string `json:"reason"`

	// ConfirmCount is the typed confirmation the operator supplied,
	// present only for the two modes that demand one. Recording it
	// alongside `WouldChange` is what makes the confirmation auditable
	// rather than merely enforced.
	ConfirmCount *int `json:"confirm_count,omitempty"`

	// The six preview partition counts plus both reconciliation
	// totals, so a reader can verify the arithmetic without the
	// preview.
	Expanded     int `json:"expanded"`
	Eligible     int `json:"eligible"`
	WouldChange  int `json:"would_change"`
	NoOp         int `json:"no_op"`
	Refused      int `json:"refused"`
	Inapplicable int `json:"inapplicable"`
	Unreadable   int `json:"unreadable"`
	Unauthorized int `json:"unauthorized"`

	// The apply-side outcome counts.
	Changed             int `json:"changed"`
	Conflict            int `json:"conflict"`
	Gone                int `json:"gone"`
	UnauthorizedAtApply int `json:"unauthorized_at_apply"`
	Errored             int `json:"error"`

	// UnauthorizedAtApplyReasons counts the sub-reasons behind
	// UnauthorizedAtApply, so "the caller lost the instrument" and "the
	// assets were transferred away" are distinguishable after the fact.
	UnauthorizedAtApplyReasons map[string]int `json:"unauthorized_at_apply_reasons,omitempty"`

	// SelectionEntryCount is what the operator actually selected,
	// before expansion. Recording both it and Expanded is what shows an
	// operator later that four posts reached nine hundred assets.
	SelectionEntryCount int `json:"selection_entry_count"`

	// CommittedTerms lists vocabulary terms this apply actually
	// created. A term that no successful write stored is not here,
	// because it was not created.
	CommittedTerms []string `json:"committed_terms,omitempty"`

	// TargetIDs is the resolved target set with each target's outcome
	// label. Ids and labels only — see the type's header.
	TargetIDs map[string][]string `json:"target_ids"`
}

// RecordBatchAssetFieldEditInTx commits the batch editor's SINGLE audit
// envelope inside the caller's transaction, and RETURNS ITS ERROR.
//
// # Why this one does not swallow its failure
//
// Every other writer in this package is best-effort, on the stated
// grounds that the database is the source of truth and the audit log is
// observability: a failed audit insert must not roll back a user-facing
// write, and its absence shows up as a gap an operator can investigate.
//
// That trade is wrong here, and the reason is the contract rather than
// a change of heart about audit. The batch's apply is defined as ONE
// ATOMIC COMMITTED OUTCOME comprising the field mutations, any
// vocabulary mint, the token's consumption AND EXACTLY ONE ENVELOPE. A
// best-effort envelope makes the fourth member optional, which means a
// spent token and a thousand changed records could exist with no record
// of who changed them or why — and the operator reason this operation
// demands would have been collected for nothing. So a failure to record
// fails the apply, the transaction rolls back, the token stays
// spendable, and the operator retries. Nothing is written that is not
// accounted for.
//
// `q` MUST be bound to the same pgx.Tx as the field writes.
func (r *Recorder) RecordBatchAssetFieldEditInTx(
	ctx context.Context,
	q *Queries,
	req *http.Request,
	actorUserRef int64,
	env BatchAssetFieldEditEnvelope,
) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("audit: encode batch envelope: %w", err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return fmt.Errorf("audit: encode batch envelope: %w", err)
	}

	rc := ctxFromRequest(req)
	if err := q.InsertAuditEvent(ctx, InsertAuditEventParams{
		EventType: EventAssetFieldBatchEditApplied,
		// No subject: the subjects are the thousand assets named in
		// the envelope, and subject_user_ref is a USER column. The
		// actor is the initiating human.
		SubjectUserRef: nil,
		ActorUserRef:   &actorUserRef,
		Ip:             rc.ip,
		UserAgent:      rc.userAgent,
		Metadata:       raw,
	}); err != nil {
		return fmt.Errorf("audit: record batch envelope: %w", err)
	}
	return nil
}
