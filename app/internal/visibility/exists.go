// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Pool is the narrow surface CanSee needs. Implemented by
// *pgxpool.Pool + any pgx-compatible tx handle. Kept as an
// interface so tests substitute a fake without importing
// pgxpool.
type Pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CanSee reports whether `caller` is allowed to see one row of
// `entityType` identified by `id`. Returns (false, nil) for both
// "row exists but predicate rejects" AND "row does not exist" —
// the two cases collapse into a single boolean answer so callers
// can render an enumeration-safe response body without a second
// existence probe.
//
// Rationale: the feedback POST /search/feedback handler needs to
// return 403 hit_not_visible for BOTH cases (unknown UUID + hidden
// asset) so an attacker can't distinguish existence from
// non-visibility. Prior to this helper, the feedback package
// wrote its own EXISTS query inline (Phase 1.16.B-5-followup
// PoolVisibility). The retrofit here consolidates that duplication
// without changing observable behaviour.
//
// The generated SQL is `SELECT EXISTS (SELECT 1 FROM <table>
// WHERE id = $1 <predicate>)` where <predicate> is the same fragment
// Filter().ToSQL(alias) returns for the given entityType. Because
// EntityAsset's current predicate is just `deleted_at IS NULL`, the
// generated SQL matches the pre-retrofit inline query byte-for-byte
// modulo whitespace.
func CanSee(ctx context.Context, pool Pool, entityType EntityType, caller Caller, id uuid.UUID) (bool, error) {
	pred, err := Filter(ctx, entityType, caller)
	if err != nil {
		return false, err
	}
	table, err := tableName(entityType)
	if err != nil {
		return false, err
	}
	// Filter().ToSQL(alias, argOffset) always emits a fragment
	// prefixed with " AND (...)". The base predicate here is
	// `id = $1`; the visibility fragment appends its own $2, $3,
	// ... starting at argOffset=1.
	frag, extraArgs := pred.ToSQL("", 1)

	sql := "SELECT EXISTS (SELECT 1 FROM " + table + " WHERE id = $1" + frag + ")"
	args := append([]any{id}, extraArgs...)

	var ok bool
	if err := pool.QueryRow(ctx, sql, args...).Scan(&ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// EXISTS query cannot return zero rows — every branch
			// yields exactly one row containing TRUE or FALSE. But
			// keep the guard so a driver-level oddity surfaces
			// cleanly as (false, nil) rather than confusing the
			// caller.
			return false, nil
		}
		return false, err
	}
	return ok, nil
}

// tableName maps EntityType to the physical table name used in
// CanSee's SELECT. Kept private + on the visibility package so the
// mapping stays in one place — the search Engine's query builders
// currently duplicate the same mapping inline (a future PR could
// consolidate those too, but per #185's scope-trim we don't touch
// them here).
func tableName(entityType EntityType) (string, error) {
	switch entityType {
	case EntityAsset:
		return "assets", nil
	case EntityCollection:
		return "collections", nil
	case EntityPost:
		return "posts", nil
	}
	return "", ErrUnknownEntityType
}
