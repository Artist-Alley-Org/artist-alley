// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package reindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps the search_reindex_run table with the plain-Go Row +
// param types the HTTP handler + job consume. sqlc-free — the
// table's four writer methods (start, progress, complete, cancel)
// are simple enough that hand-written SQL is clearer than sqlc's
// generated Row types for the JSONB scope column.
type Store struct {
	Pool *pgxpool.Pool
}

// NewStore constructs a Store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// Start inserts a new run row. Returns ErrActiveRunExists when
// the partial UNIQUE INDEX rejects the insert (another run is
// already in-progress).
func (s *Store) Start(ctx context.Context, p StartParams) (Row, error) {
	if !ValidTarget(string(p.Target)) {
		return Row{}, fmt.Errorf("reindex.Start: invalid target %q", p.Target)
	}
	scopeJSON, err := json.Marshal(p.Scope)
	if err != nil {
		return Row{}, fmt.Errorf("reindex.Start: marshal scope: %w", err)
	}
	var id uuid.UUID
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO search_reindex_run (scope, target, started_by_user_ref)
		VALUES ($1::JSONB, $2, $3)
		RETURNING id
	`, scopeJSON, string(p.Target), p.StartedBy).Scan(&id)
	if err != nil {
		if isPgUniqueViolation(err) {
			return Row{}, ErrActiveRunExists
		}
		return Row{}, fmt.Errorf("reindex.Start: %w", err)
	}
	return s.Get(ctx, id)
}

// Get fetches one row by id. Returns ErrNotFound when absent.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Row, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, started_at, completed_at, cancelled_at,
		       scope, target, total_estimated,
		       processed, succeeded, failed,
		       started_by_user_ref, last_error
		FROM search_reindex_run WHERE id = $1
	`, id)
	if err != nil {
		return Row{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Row{}, ErrNotFound
	}
	return scanRow(rows)
}

// List returns the most-recent runs in descending start-time order.
// Limit caps the response; default 20 when limit <= 0.
func (s *Store) List(ctx context.Context, limit int32) ([]Row, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, started_at, completed_at, cancelled_at,
		       scope, target, total_estimated,
		       processed, succeeded, failed,
		       started_by_user_ref, last_error
		FROM search_reindex_run
		ORDER BY started_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Row, 0, limit)
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordProgress bumps the per-batch counters. Called from the
// job handler at every batch boundary; a single UPDATE keeps the
// admin UI's polling see forward motion.
func (s *Store) RecordProgress(ctx context.Context, id uuid.UUID, processed, succeeded, failed int64) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE search_reindex_run
		   SET processed = $2,
		       succeeded = $3,
		       failed    = $4
		 WHERE id = $1
	`, id, processed, succeeded, failed)
	return err
}

// Cancel marks the run as cancelled if it's still in-progress. The
// worker picks this up at the next batch boundary + short-circuits
// the walk. Idempotent — cancelling an already-cancelled or
// already-completed row is a no-op.
func (s *Store) Cancel(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE search_reindex_run
		   SET cancelled_at = NOW()
		 WHERE id = $1 AND completed_at IS NULL AND cancelled_at IS NULL
	`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Either the row doesn't exist OR it's already terminal.
		// Verify existence so the HTTP layer can distinguish 404
		// from "already done".
		if _, err := s.Get(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// Complete marks the run as done. lastErr, when non-empty, gets
// persisted alongside so the failure surface shows the reason.
func (s *Store) Complete(ctx context.Context, id uuid.UUID, lastErr string) error {
	var errRef *string
	if lastErr != "" {
		errRef = &lastErr
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE search_reindex_run
		   SET completed_at = NOW(),
		       last_error = $2
		 WHERE id = $1
	`, id, errRef)
	return err
}

// IsCancelled returns true if the run's cancelled_at is set. Used
// by the worker's per-batch cancel probe.
func (s *Store) IsCancelled(ctx context.Context, id uuid.UUID) (bool, error) {
	var cancelledAt *time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT cancelled_at FROM search_reindex_run WHERE id = $1
	`, id).Scan(&cancelledAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	return cancelledAt != nil, nil
}

func scanRow(rows pgx.Rows) (Row, error) {
	var (
		r         Row
		scopeRaw  []byte
		target    string
	)
	if err := rows.Scan(
		&r.ID, &r.StartedAt, &r.CompletedAt, &r.CancelledAt,
		&scopeRaw, &target, &r.TotalEstimated,
		&r.Processed, &r.Succeeded, &r.Failed,
		&r.StartedByUserRef, &r.LastError,
	); err != nil {
		return Row{}, err
	}
	r.Target = Target(target)
	if len(scopeRaw) > 0 {
		_ = json.Unmarshal(scopeRaw, &r.Scope)
	}
	return r, nil
}

func isPgUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "23505"
	}
	return false
}
