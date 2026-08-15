// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CallAuditor writes per-call rows to ai_provider_call. Every
// successful + failed inference call (and every privacy/budget block
// short of an actual HTTP call) records a row — the table is the
// source of truth for the operator cost dashboard, the
// budget.Tracker rollup, and the per-asset "what AI did to this
// asset" lookup.
//
// Best-effort recording: a DB error logs but doesn't fail the
// calling operation. The audit row missing is a worse failure mode
// than the original error; we let the call complete and surface the
// DB error separately for the operator to see in logs.

// CallRecord is the per-call shape. Aligns one-for-one with the
// ai_provider_call columns from migration 00001.
type CallRecord struct {
	Provider               string
	Model                  string
	Concern                Concern
	PromptTemplate         string // free-text identifier (e.g. "tag")
	PromptVersion          string // e.g. "v1.0"
	AssetID                *uuid.UUID
	JobID                  *uuid.UUID
	InputHash              string // SHA-256 of canonical input payload
	InputTokens            int
	OutputTokens           int
	Duration               time.Duration
	EstimatedCostUSDMicros int64
	Status                 CallStatus
	ErrorMessage           string
	ActorUserRef           *int64
}

// CallStatus mirrors the ai_provider_call.status CHECK enum.
type CallStatus string

const (
	CallStatusSuccess        CallStatus = "success"
	CallStatusRateLimited    CallStatus = "rate_limited"
	CallStatusTransientError CallStatus = "transient_error"
	CallStatusPermanentError CallStatus = "permanent_error"
	CallStatusBudgetBlocked  CallStatus = "budget_blocked"
	CallStatusPrivacyBlocked CallStatus = "privacy_blocked"
)

// CallAuditor records per-call rows. Nil-safe at the call site:
// providers should NOT panic when the auditor is nil (tests can
// build providers without one); RecordCall on a nil auditor is a
// no-op.
type CallAuditor struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewCallAuditor binds an auditor to a pool. Pool may be nil for
// tests that don't need real DB writes.
func NewCallAuditor(pool *pgxpool.Pool, logger *slog.Logger) *CallAuditor {
	if logger == nil {
		logger = slog.Default()
	}
	return &CallAuditor{pool: pool, logger: logger}
}

// RecordCall writes one row best-effort. Nil-safe on the receiver:
// a nil auditor (or one with nil pool) silently no-ops.
func (a *CallAuditor) RecordCall(ctx context.Context, rec CallRecord) {
	if a == nil || a.pool == nil {
		return
	}

	durationMS := int(rec.Duration / time.Millisecond)
	if durationMS == 0 && rec.Duration > 0 {
		// Sub-millisecond calls still record as 1ms so analytics
		// rollups don't show 0-duration anomalies.
		durationMS = 1
	}

	const q = `
		INSERT INTO ai_provider_call (
			provider, model, concern, prompt_template, prompt_version,
			asset_id, job_id, input_hash, input_tokens, output_tokens,
			duration_ms, estimated_cost_usd_micros, status, error_message,
			actor_user_ref
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`

	status := rec.Status
	if status == "" {
		status = CallStatusSuccess
	}

	if _, err := a.pool.Exec(ctx, q,
		rec.Provider,
		rec.Model,
		string(rec.Concern),
		nullableString(rec.PromptTemplate),
		nullableString(rec.PromptVersion),
		uuidOrNil(rec.AssetID),
		uuidOrNil(rec.JobID),
		nullableString(rec.InputHash),
		nullableInt(rec.InputTokens),
		nullableInt(rec.OutputTokens),
		durationMS,
		nullableInt64(rec.EstimatedCostUSDMicros),
		string(status),
		nullableString(rec.ErrorMessage),
		rec.ActorUserRef,
	); err != nil {
		// Best-effort — log + drop. The audit row is post-hoc; failing
		// here would mask the actual call's error to the caller.
		a.logger.LogAttrs(ctx, slog.LevelWarn, "ai.audit.record.error",
			slog.String("provider", rec.Provider),
			slog.String("model", rec.Model),
			slog.String("concern", string(rec.Concern)),
			slog.String("err", err.Error()),
		)
	}
}

// nullableString returns nil when s is empty so the column receives
// SQL NULL rather than the empty string. Lets analytics distinguish
// "no template" from "template name was literally empty".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

func nullableInt64(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}

func uuidOrNil(u *uuid.UUID) any {
	if u == nil || *u == uuid.Nil {
		return nil
	}
	return *u
}

// CanonicalInputHash produces a stable SHA-256 of a request payload
// so two equivalent calls (same model, same messages, same opts) hash
// identically. Used for idempotency-key derivation in the job layer
// AND for the input_hash column in the audit row — operators can
// answer "did we already pay $X to ask this exact question?".
//
// The hash spans the JSON encoding of the input slice; deterministic
// because Go's encoding/json emits map keys sorted.
func CanonicalInputHash(parts ...any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	for _, p := range parts {
		// Each part on its own line keeps the boundary unambiguous;
		// json.Encoder always writes a trailing \n.
		_ = enc.Encode(p)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}
