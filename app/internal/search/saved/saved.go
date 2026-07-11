// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package saved

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultMaxPerUser is the fallback caller cap when the sysconfig
// key search.saved_search.max_per_user is unset.
const DefaultMaxPerUser = 10

// DefaultMinIntervalMinutes is the fallback floor when the
// sysconfig key search.saved_search.min_interval_minutes is unset.
const DefaultMinIntervalMinutes = 15

// Store wraps the sqlc-generated queries with the plain-Go Row +
// param types + rate-limit enforcement. Stateless; one per
// process.
type Store struct {
	Pool *pgxpool.Pool
	// MaxPerUser caps a caller's enabled-row count. Zero means
	// DefaultMaxPerUser.
	MaxPerUser int
	// MinIntervalMinutes floors notify_interval_minutes at write.
	// Zero means DefaultMinIntervalMinutes.
	MinIntervalMinutes int
}

// NewStore constructs a Store with the sysconfig-tunable defaults.
// Boot overrides MaxPerUser + MinIntervalMinutes from the
// sysconfig Store when those keys are set.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		Pool:               pool,
		MaxPerUser:         DefaultMaxPerUser,
		MinIntervalMinutes: DefaultMinIntervalMinutes,
	}
}

// Create inserts a new row. Enforces the max-per-user cap +
// interval floor at write time. Sqlc-generated 23505 unique-
// violation maps to ErrNameConflict.
func (s *Store) Create(ctx context.Context, p CreateParams) (Row, error) {
	if !ValidNotifyChannel(p.NotifyChannel) {
		return Row{}, ErrInvalidNotifyChannel
	}
	minInterval := s.effectiveMinInterval()
	if p.NotifyIntervalMinutes < minInterval {
		return Row{}, fmt.Errorf("%w: minimum is %d minutes", ErrIntervalTooSmall, minInterval)
	}
	// Cap check inside the same tx as the insert so two concurrent
	// Create calls can't both slip past the cap.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Row{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)
	count, err := q.CountEnabledSavedSearchesForOwner(ctx, p.OwnerUserRef)
	if err != nil {
		return Row{}, fmt.Errorf("saved.Create: count enabled: %w", err)
	}
	if int(count) >= s.effectiveMaxPerUser() {
		return Row{}, ErrMaxPerUser
	}
	row, err := q.CreateSavedSearch(ctx, CreateSavedSearchParams{
		OwnerUserRef:          p.OwnerUserRef,
		Name:                  p.Name,
		Dsl:                   p.DSL,
		NotifyChannel:         p.NotifyChannel,
		NotifyIntervalMinutes: int32(p.NotifyIntervalMinutes),
	})
	if err != nil {
		if isPgUniqueViolation(err) {
			return Row{}, ErrNameConflict
		}
		return Row{}, fmt.Errorf("saved.Create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Row{}, err
	}
	return rowFromSQLC(row), nil
}

// Get returns a row by ID. Owner-check happens at the HTTP layer;
// this method is unaware of caller identity.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (Row, error) {
	row, err := New(s.Pool).GetSavedSearch(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Row{}, ErrNotFound
		}
		return Row{}, fmt.Errorf("saved.Get: %w", err)
	}
	return rowFromSQLC(row), nil
}

// List returns the caller's rows in reverse-chronological order,
// limited to `limit` (defaults to 50 when 0).
func (s *Store) List(ctx context.Context, ownerUserRef int64, limit int32) ([]Row, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := New(s.Pool).ListSavedSearchesForOwner(ctx, ListSavedSearchesForOwnerParams{
		OwnerUserRef: ownerUserRef,
		Limit:        limit,
	})
	if err != nil {
		return nil, fmt.Errorf("saved.List: %w", err)
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowFromSQLC(r))
	}
	return out, nil
}

// Update applies a partial patch. Every param field is optional
// (nil = leave alone); the query builds a COALESCE per column so
// one Store call maps to one UPDATE.
func (s *Store) Update(ctx context.Context, id uuid.UUID, p UpdateParams) (Row, error) {
	if p.NotifyChannel != nil && !ValidNotifyChannel(*p.NotifyChannel) {
		return Row{}, ErrInvalidNotifyChannel
	}
	if p.NotifyIntervalMinutes != nil {
		minInterval := s.effectiveMinInterval()
		if *p.NotifyIntervalMinutes < minInterval {
			return Row{}, fmt.Errorf("%w: minimum is %d minutes", ErrIntervalTooSmall, minInterval)
		}
	}
	pg := pgtype.UUID{Bytes: id, Valid: true}
	args := UpdateSavedSearchParams{ID: pg}
	if p.Name != nil {
		args.Name = p.Name
	}
	if p.DSL != nil {
		args.Dsl = p.DSL
	}
	if p.NotifyChannel != nil {
		args.NotifyChannel = p.NotifyChannel
	}
	if p.NotifyIntervalMinutes != nil {
		v := int32(*p.NotifyIntervalMinutes)
		args.NotifyIntervalMinutes = &v
	}
	if p.Enabled != nil {
		args.Enabled = p.Enabled
	}
	row, err := New(s.Pool).UpdateSavedSearch(ctx, args)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Row{}, ErrNotFound
		}
		if isPgUniqueViolation(err) {
			return Row{}, ErrNameConflict
		}
		return Row{}, fmt.Errorf("saved.Update: %w", err)
	}
	return rowFromSQLC(row), nil
}

// Delete removes a row by ID. Owner-check at the HTTP layer.
func (s *Store) Delete(ctx context.Context, id uuid.UUID) error {
	return New(s.Pool).DeleteSavedSearch(ctx, pgtype.UUID{Bytes: id, Valid: true})
}

// ListDue is the coordinator walk — enabled rows past their
// next-run threshold. Limit caps a single tick's work.
func (s *Store) ListDue(ctx context.Context, limit int32) ([]Row, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := New(s.Pool).ListDueSavedSearches(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("saved.ListDue: %w", err)
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowFromSQLC(r))
	}
	return out, nil
}

// RecordRun persists the fresh delta state + updates the run
// timestamp. notifiedNow=true additionally updates last_notified_
// at. Returns the fresh Row post-write.
func (s *Store) RecordRun(ctx context.Context, id uuid.UUID, hash string, ids []uuid.UUID, notifiedNow bool) (Row, error) {
	pgIDs := make([]pgtype.UUID, len(ids))
	for i, u := range ids {
		pgIDs[i] = pgtype.UUID{Bytes: u, Valid: true}
	}
	// The sqlc-generated method uses positional args; hash and ids
	// are the 2nd/3rd column-writes.
	row, err := New(s.Pool).RecordSavedSearchRun(ctx, RecordSavedSearchRunParams{
		ID:             pgtype.UUID{Bytes: id, Valid: true},
		LastResultHash: &hash,
		LastResultIds:  pgIDs,
		Column4:        notifiedNow,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Row{}, ErrNotFound
		}
		return Row{}, fmt.Errorf("saved.RecordRun: %w", err)
	}
	return rowFromSQLC(row), nil
}

// CountActive returns the enabled-row gauge value.
func (s *Store) CountActive(ctx context.Context) (int64, error) {
	return New(s.Pool).CountActiveSavedSearches(ctx)
}

func (s *Store) effectiveMaxPerUser() int {
	if s.MaxPerUser <= 0 {
		return DefaultMaxPerUser
	}
	return s.MaxPerUser
}

func (s *Store) effectiveMinInterval() int {
	if s.MinIntervalMinutes <= 0 {
		return DefaultMinIntervalMinutes
	}
	return s.MinIntervalMinutes
}

// rowFromSQLC translates the sqlc-generated SavedSearch struct
// into the exported Row type. Handles the pgtype ↔ Go type
// conversions so downstream code stays in stdlib.
func rowFromSQLC(s SavedSearch) Row {
	r := Row{
		Name:                  s.Name,
		DSL:                   s.Dsl,
		NotifyChannel:         s.NotifyChannel,
		NotifyIntervalMinutes: int(s.NotifyIntervalMinutes),
		Enabled:               s.Enabled,
		LastResultHash:        s.LastResultHash,
	}
	if s.ID.Valid {
		r.ID = uuid.UUID(s.ID.Bytes)
	}
	r.OwnerUserRef = s.OwnerUserRef
	if len(s.LastResultIds) > 0 {
		r.LastResultIDs = make([]uuid.UUID, 0, len(s.LastResultIds))
		for _, pg := range s.LastResultIds {
			if pg.Valid {
				r.LastResultIDs = append(r.LastResultIDs, uuid.UUID(pg.Bytes))
			}
		}
	}
	if s.LastRunAt.Valid {
		t := s.LastRunAt.Time
		r.LastRunAt = &t
	}
	if s.LastNotifiedAt.Valid {
		t := s.LastNotifiedAt.Time
		r.LastNotifiedAt = &t
	}
	if s.OriginServerID.Valid {
		u := uuid.UUID(s.OriginServerID.Bytes)
		r.OriginServerID = &u
	}
	if s.CreatedAt.Valid {
		r.CreatedAt = s.CreatedAt.Time
	}
	if s.UpdatedAt.Valid {
		r.UpdatedAt = s.UpdatedAt.Time
	}
	return r
}

// isPgUniqueViolation reports whether err carries the Postgres
// 23505 unique-violation code. Copied from the existing helper
// in the assets package rather than exported publicly.
func isPgUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "23505"
	}
	return false
}
