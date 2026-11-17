// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package casfetch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// leaseKey is the full fetch identity. A lease, and therefore a result,
// is never shared across peers, origin assets, hashes or signers.
type leaseKey struct {
	PeerID  uuid.UUID
	AssetID uuid.UUID
	Hash    string
	Signer  string
}

func (k leaseKey) String() string {
	return k.PeerID.String() + "|" + k.AssetID.String() + "|" + k.Hash + "|" + k.Signer
}

// leaseStore is federation_cas_fetch_leases. Every call is one short
// statement; no transaction is held across network I/O.
type leaseStore struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

// acquire takes the lease if it is free or expired. ok=false means
// another holder has it.
func (s leaseStore) acquire(ctx context.Context, k leaseKey, owner string) (uuid.UUID, bool, error) {
	id := uuid.New()
	var got uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO federation_cas_fetch_leases
		    (peer_id, asset_id, object_hash, signer, lease_id, owner, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + make_interval(secs => $7))
		ON CONFLICT (peer_id, asset_id, object_hash, signer) DO UPDATE
		   SET lease_id = EXCLUDED.lease_id, owner = EXCLUDED.owner,
		       expires_at = EXCLUDED.expires_at, created_at = now()
		 WHERE federation_cas_fetch_leases.expires_at < now()
		RETURNING lease_id`,
		k.PeerID, k.AssetID, k.Hash, k.Signer, id, owner, s.ttl.Seconds(),
	).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("casfetch: acquire lease: %w", err)
	}
	return got, true, nil
}

// heartbeat extends the lease while this holder still owns it.
func (s leaseStore) heartbeat(ctx context.Context, k leaseKey, id uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE federation_cas_fetch_leases
		   SET expires_at = now() + make_interval(secs => $6)
		 WHERE peer_id = $1 AND asset_id = $2 AND object_hash = $3 AND signer = $4 AND lease_id = $5`,
		k.PeerID, k.AssetID, k.Hash, k.Signer, id, s.ttl.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// releaseSQL deletes this holder's lease. Used directly on failure and
// inside the import transaction on success.
const releaseSQL = `DELETE FROM federation_cas_fetch_leases
	WHERE peer_id = $1 AND asset_id = $2 AND object_hash = $3 AND signer = $4 AND lease_id = $5`

func (s leaseStore) release(ctx context.Context, k leaseKey, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, releaseSQL, k.PeerID, k.AssetID, k.Hash, k.Signer, id)
	return err
}

// held reports whether a live (unexpired) lease exists for k.
func (s leaseStore) held(ctx context.Context, k leaseKey) (bool, error) {
	var live bool
	err := s.pool.QueryRow(ctx, `
		SELECT expires_at > now() FROM federation_cas_fetch_leases
		 WHERE peer_id = $1 AND asset_id = $2 AND object_hash = $3 AND signer = $4`,
		k.PeerID, k.AssetID, k.Hash, k.Signer).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return live, err
}

// deleteExpired removes leases expired for longer than grace.
func (s leaseStore) deleteExpired(ctx context.Context, grace time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM federation_cas_fetch_leases WHERE expires_at < now() - make_interval(secs => $1)`,
		grace.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
