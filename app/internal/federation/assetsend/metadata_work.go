// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Durable metadata work (#1577, spec v1 §18.3): obligations to send asset
// metadata Updates to peers, recorded in the transaction of the change
// that requires them and processed later in bounded pages
// (federation_metadata_work, migration 00099).
//
// # Unwired
//
// Nothing in production enqueues work or runs MetadataWork yet: the
// producers (asset, field value, mirror, batch, import and definition
// changes) and the local runtime that calls RunOnce are later #1577 work.
//
// # Enqueue
//
// EnqueueAssetMetadataWork and EnqueueAllMetadataWork insert ONE new row
// in the caller's transaction, so the obligation commits or rolls back
// with the change. They never merge into a row already in progress: a
// change committed after a page passed a peer gets its own row, which
// starts from the first peer. They read no value and loop over nothing.
//
// # Processing (MetadataWork.RunOnce)
//
// One call handles ONE bounded page of ONE row, in one transaction:
//
//  1. the oldest row not locked by another processor is claimed
//     (FOR UPDATE SKIP LOCKED), so no two processors spend one page;
//  2. an 'all' row adds one 'asset' row per local live asset after its
//     cursor, at most a page of them, and does no peer work in that call;
//  3. an 'asset' row locks that asset's row (and only it), reads at most a
//     page of distinct peers holding a live peer-wide original-file share
//     of it after its cursor, and calls RecordMetadataUpdate for each: the
//     recorder alone decides eligibility, the version, the capture, the
//     identity and the order. A missing, deleted, remote or ineligible
//     asset or peer records nothing;
//  4. the cursor advances, or the row is deleted when the page was not
//     full (a full last page leaves one more, empty, turn).
//
// Everything a page writes (activities, added rows, the cursor, the
// deletion) commits together; an error or a cancellation rolls all of it
// back, and the next call repeats the page. The capture is taken when the
// recorder runs, as §18.3 permits; once recorded, an Update never changes.
package assetsend

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
)

// The page bounds of MetadataWork.RunOnce.
const (
	DefaultMetadataWorkPage = 16
	MaxMetadataWorkPage     = 64
)

// ErrMetadataWorkPage is a page size out of range (negative or above
// MaxMetadataWorkPage).
var ErrMetadataWorkPage = errors.New("assetsend: metadata work page size out of range")

// EnqueueAssetMetadataWork adds, in tx, one obligation to send assetID's
// current metadata to its peers, returning its id.
func EnqueueAssetMetadataWork(ctx context.Context, tx pgx.Tx, assetID uuid.UUID) (uuid.UUID, error) {
	if tx == nil {
		return uuid.Nil, errors.New("assetsend: metadata work needs the caller's transaction")
	}
	if assetID == uuid.Nil {
		return uuid.Nil, errors.New("assetsend: metadata work needs an asset")
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO federation_metadata_work (kind, asset_id) VALUES ('asset', $1) RETURNING id`, assetID).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("assetsend: enqueue asset metadata work: %w", err)
	}
	return id, nil
}

// EnqueueAllMetadataWork adds, in tx, one obligation that concerns every
// local asset (a policy change), returning its id.
func EnqueueAllMetadataWork(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
	if tx == nil {
		return uuid.Nil, errors.New("assetsend: metadata work needs the caller's transaction")
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO federation_metadata_work (kind) VALUES ('all') RETURNING id`).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("assetsend: enqueue metadata work for every asset: %w", err)
	}
	return id, nil
}

// MetadataWork processes the metadata work ledger (file header).
type MetadataWork struct {
	Pool    *pgxpool.Pool
	Writer  *activities.Writer
	BaseURL string
	// Username resolves an owner's username, as for RecordMetadataUpdate.
	Username func(ctx context.Context, userRef int64) string
}

// RunOnce processes one page of at most limit items (0 means
// DefaultMetadataWorkPage) of the oldest unclaimed row. worked is false
// when no row was available. On an error nothing of the page is kept.
func (m MetadataWork) RunOnce(ctx context.Context, limit int) (worked bool, err error) {
	if limit == 0 {
		limit = DefaultMetadataWorkPage
	}
	if limit < 0 || limit > MaxMetadataWorkPage {
		return false, ErrMetadataWorkPage
	}
	if m.Pool == nil || m.Writer == nil || m.Username == nil {
		return false, errors.New("assetsend: metadata work needs a pool, a writer and a username resolver")
	}
	err = pgx.BeginFunc(ctx, m.Pool, func(tx pgx.Tx) error {
		var (
			id                               uuid.UUID
			kind                             string
			assetID, assetCursor, peerCursor *uuid.UUID
		)
		err := tx.QueryRow(ctx, `SELECT id, kind, asset_id, asset_cursor, peer_cursor FROM federation_metadata_work
			ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id, &kind, &assetID, &assetCursor, &peerCursor)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("assetsend: claim metadata work: %w", err)
		}
		worked = true
		switch {
		case kind == "all":
			return m.expand(ctx, tx, id, assetCursor, limit)
		case kind == "asset" && assetID != nil:
			return m.peers(ctx, tx, id, *assetID, peerCursor, limit)
		}
		return fmt.Errorf("assetsend: metadata work %s has an unknown shape", id)
	})
	if err != nil {
		return false, err
	}
	return worked, nil
}

// expand adds one 'asset' row per local live asset after cursor, at most
// limit of them, and advances or completes 'all' row id.
func (m MetadataWork) expand(ctx context.Context, tx pgx.Tx, id uuid.UUID, cursor *uuid.UUID, limit int) error {
	page, err := uuidPage(ctx, tx, `SELECT id FROM assets
		WHERE origin_server_id IS NULL AND deleted_at IS NULL AND ($1::uuid IS NULL OR id > $1)
		ORDER BY id LIMIT $2`, cursor, limit)
	if err != nil {
		return fmt.Errorf("assetsend: read the assets for metadata work: %w", err)
	}
	if len(page) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO federation_metadata_work (kind, asset_id)
			SELECT 'asset', a FROM unnest($1::uuid[]) WITH ORDINALITY AS t(a, n) ORDER BY n`, page); err != nil {
			return fmt.Errorf("assetsend: add asset metadata work: %w", err)
		}
	}
	return advance(ctx, tx, id, "asset_cursor", page, limit)
}

// peers records one metadata Update of assetID for each peer of a page
// after cursor, and advances or completes 'asset' row id.
func (m MetadataWork) peers(ctx context.Context, tx pgx.Tx, id, assetID uuid.UUID, cursor *uuid.UUID, limit int) error {
	var held bool
	err := tx.QueryRow(ctx, `SELECT TRUE FROM assets WHERE id = $1 FOR UPDATE`, assetID).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return advance(ctx, tx, id, "peer_cursor", nil, limit)
	}
	if err != nil {
		return fmt.Errorf("assetsend: lock the asset for metadata work: %w", err)
	}
	page, err := uuidPage(ctx, tx, `SELECT p.id FROM federation_peers p
		WHERE ($2::uuid IS NULL OR p.id > $2)
		  AND EXISTS (SELECT 1 FROM federation_shares s
		               WHERE s.peer_id = p.id AND s.object_kind = 'asset' AND s.object_id = $1
		                 AND s.allow_original AND s.target_user_url IS NULL AND s.revoked_at IS NULL
		                 AND (s.expires_at IS NULL OR s.expires_at > now()))
		ORDER BY p.id LIMIT $3`, assetID, cursor, limit)
	if err != nil {
		return fmt.Errorf("assetsend: read the peers for metadata work: %w", err)
	}
	for _, peer := range page {
		if _, err := RecordMetadataUpdate(ctx, tx, m.Writer, MetadataChange{AssetID: assetID, PeerID: peer, BaseURL: m.BaseURL,
			Username: m.Username}); err != nil {
			return err
		}
	}
	return advance(ctx, tx, id, "peer_cursor", page, limit)
}

// uuidPage reads at most limit UUIDs, closing the rows before returning.
func uuidPage(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []uuid.UUID
	for rows.Next() {
		var u uuid.UUID
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		page = append(page, u)
	}
	return page, rows.Err()
}

// advance moves row id's cursor column to the page's last item, or deletes
// the row when the page was not full.
func advance(ctx context.Context, tx pgx.Tx, id uuid.UUID, column string, page []uuid.UUID, limit int) error {
	if len(page) < limit {
		if _, err := tx.Exec(ctx, `DELETE FROM federation_metadata_work WHERE id = $1`, id); err != nil {
			return fmt.Errorf("assetsend: complete metadata work: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE federation_metadata_work SET `+column+` = $2 WHERE id = $1`, id, page[len(page)-1]); err != nil {
		return fmt.Errorf("assetsend: advance metadata work: %w", err)
	}
	return nil
}
