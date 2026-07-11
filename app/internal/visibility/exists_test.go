// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// capturingPool records the SQL + args every CanSee call passes so
// the tests can assert the exact composed query without a real DB.
type capturingPool struct {
	sql        string
	args       []any
	scanValue  bool
	scanErr    error
	callCount  int
}

type capturedRow struct {
	value bool
	err   error
}

func (r capturedRow) Scan(dst ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dst) == 0 {
		return nil
	}
	if p, ok := dst[0].(*bool); ok {
		*p = r.value
	}
	return nil
}

func (p *capturingPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	p.callCount++
	p.sql = sql
	p.args = args
	return capturedRow{value: p.scanValue, err: p.scanErr}
}

// TestCanSee_Asset_ComposesExpectedSQL — the generated SQL must
// exactly match the pre-retrofit shape used by feedback's
// PoolVisibility: SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1
// AND (deleted_at IS NULL)).
func TestCanSee_Asset_ComposesExpectedSQL(t *testing.T) {
	pool := &capturingPool{scanValue: true}
	id := uuid.New()
	ok, err := CanSee(context.Background(), pool, EntityAsset,
		Caller{UserRef: 42}, id)
	if err != nil {
		t.Fatalf("CanSee: %v", err)
	}
	if !ok {
		t.Fatal("expected true, got false")
	}
	want := `SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1 AND (deleted_at IS NULL))`
	if strings.TrimSpace(pool.sql) != want {
		t.Fatalf("SQL mismatch:\nwant %q\n got %q", want, pool.sql)
	}
	if len(pool.args) != 1 || pool.args[0] != id {
		t.Fatalf("args: got %v, want [%s]", pool.args, id)
	}
}

// TestCanSee_Anonymous_Asset_SameSQL — anonymous caller doesn't add
// any args for EntityAsset (predicate is just deleted_at).
func TestCanSee_Anonymous_Asset_SameSQL(t *testing.T) {
	pool := &capturingPool{scanValue: true}
	_, _ = CanSee(context.Background(), pool, EntityAsset,
		Caller{UserRef: AnonymousCaller, IsAnonymous: true}, uuid.New())
	want := `SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1 AND (deleted_at IS NULL))`
	if strings.TrimSpace(pool.sql) != want {
		t.Fatalf("SQL mismatch:\nwant %q\n got %q", want, pool.sql)
	}
	if len(pool.args) != 1 {
		t.Fatalf("anonymous EntityAsset must not bind any extra args, got %v", pool.args)
	}
}

// TestCanSee_Collection_AnonymousShortCircuits_ReturnsFalse — the
// EntityCollection predicate for anonymous callers is "AND (FALSE)".
// The composed EXISTS query still runs but always yields FALSE,
// which matches the row-level collections handler behaviour.
func TestCanSee_Collection_AnonymousShortCircuits_ReturnsFalse(t *testing.T) {
	pool := &capturingPool{scanValue: false}
	ok, err := CanSee(context.Background(), pool, EntityCollection,
		Caller{UserRef: AnonymousCaller, IsAnonymous: true}, uuid.New())
	if err != nil {
		t.Fatalf("CanSee: %v", err)
	}
	if ok {
		t.Fatal("anonymous EntityCollection must return false")
	}
	if !strings.Contains(pool.sql, "AND (FALSE)") {
		t.Fatalf("expected FALSE short-circuit fragment, got %q", pool.sql)
	}
}

// TestCanSee_UnknownEntityType_ReturnsError — Filter validates.
func TestCanSee_UnknownEntityType_ReturnsError(t *testing.T) {
	pool := &capturingPool{}
	_, err := CanSee(context.Background(), pool, EntityType(99),
		Caller{}, uuid.New())
	if !errors.Is(err, ErrUnknownEntityType) {
		t.Fatalf("expected ErrUnknownEntityType, got %v", err)
	}
	if pool.callCount != 0 {
		t.Fatal("unknown entity type should not touch the pool")
	}
}

// TestCanSee_ScanError_Propagates — driver-layer failures surface.
func TestCanSee_ScanError_Propagates(t *testing.T) {
	sentinel := errors.New("connection reset")
	pool := &capturingPool{scanErr: sentinel}
	_, err := CanSee(context.Background(), pool, EntityAsset,
		Caller{UserRef: 1}, uuid.New())
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

// TestCanSee_ErrNoRows_CollapsesToFalse — defence in depth against a
// driver-level oddity where EXISTS somehow returns zero rows. The
// helper collapses to (false, nil) rather than returning ErrNoRows
// to the caller — the enumeration-safe collapse is that both
// "not visible" and "internal weirdness" both return the same
// terminal false.
func TestCanSee_ErrNoRows_CollapsesToFalse(t *testing.T) {
	pool := &capturingPool{scanErr: pgx.ErrNoRows}
	ok, err := CanSee(context.Background(), pool, EntityAsset,
		Caller{UserRef: 1}, uuid.New())
	if err != nil {
		t.Fatalf("ErrNoRows should collapse, got %v", err)
	}
	if ok {
		t.Fatal("ErrNoRows should collapse to false, got true")
	}
}
