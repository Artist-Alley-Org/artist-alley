// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package softdelete implements the recovery-window + hard-delete-
// by-gc pattern for entities whose DELETE handler is a soft-delete
// (assets, posts, collections) plus the archived-user hard-delete
// path.
//
// Two responsibilities:
//
//  1. Service.Restore{Asset,Post,Collection} — flip deleted_at
//     back to NULL + clear deleted_reason inside a Tx that also
//     writes an audit event. Idempotent-friendly: restore on a
//     row that's already live returns ErrNotDeleted so the admin
//     UI can 404 cleanly.
//
//  2. Service.HardDeletePast — one-batch pass over rows whose
//     soft-delete anchor (deleted_at, or archived_at for users)
//     is past sysconfig retention. Fires the hard-delete SQL +
//     one audit row per victim. Returns the count so the
//     coordinator can decide whether to loop.
//
// The GC coordinator itself lives in coordinator.go — the Service
// here is the pure per-op primitive so handlers + the coordinator
// share the same delete/restore semantics.
package softdelete

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
)

// ErrNotDeleted is returned by Restore when the target row's
// deleted_at is NULL (row is already live). The HTTP layer maps
// this to 404.
var ErrNotDeleted = errors.New("softdelete: row not soft-deleted")

// ErrNotFound is returned by Restore when the target row doesn't
// exist at all. The HTTP layer maps this to 404 too — the two
// error classes surface identically to clients but the softdelete
// package distinguishes them for logging.
var ErrNotFound = errors.New("softdelete: row not found")

// Service carries the pool + audit recorder. Construct once at boot
// and share across handlers + the coordinator.
type Service struct {
	Pool *pgxpool.Pool
	Rec  *audit.Recorder

	// OnAssetsHardDeleted fires once per batch, after the DELETE
	// commits, with the ids that are now gone (#935).
	//
	// A hard delete is the one asset write that reaches other domains
	// through the SCHEMA rather than through code: the FKs on
	// asset_subtitle_tracks and post_assets are ON DELETE CASCADE, so
	// rows vanish in packages this one has never heard of. The database
	// ends up consistent; the in-process LRUs those packages keep do
	// not, and nothing in the DB can tell them.
	//
	// This is a hook rather than a direct call because the GC is a
	// generic primitive — giving it imports of subtitles, posts and
	// iiif/presentation would invert the dependency direction for
	// three domains at once. The composition root owns the fan-out and
	// stays the one place you can read off which caches a hard delete
	// touches.
	//
	// Best-effort and optional: nil means "nothing to notify", and a
	// panicking or slow hook is the composition root's problem, not a
	// reason to fail a delete that has already committed.
	OnAssetsHardDeleted func(ctx context.Context, ids []uuid.UUID)
}

// NewService returns a Service. Audit recorder may be nil (tests);
// nil-recorder calls skip the audit write.
func NewService(pool *pgxpool.Pool, rec *audit.Recorder) *Service {
	return &Service{Pool: pool, Rec: rec}
}

// RestoreAsset flips assets.deleted_at back to NULL, clears
// deleted_reason, and fires the AdminAssetRestored audit event.
//
// Returns ErrNotDeleted if the row exists but isn't soft-deleted,
// or ErrNotFound if the row doesn't exist. Actor is the admin's
// user_ref (audit metadata).
func (s *Service) RestoreAsset(ctx context.Context, req *http.Request, assetID uuid.UUID, actorUserRef int64) error {
	var priorReason *string
	var priorDeletedAt time.Time

	// CTE snapshots the pre-UPDATE deleted_at + deleted_reason so
	// RETURNING can carry them into the audit event; RETURNING on
	// the naked UPDATE reads the post-SET values (both NULL) which
	// can't scan into time.Time. The `AND EXISTS (SELECT 1 FROM old)`
	// guard drops the UPDATE to zero rows when the row isn't
	// soft-deleted, so pgx.ErrNoRows still fires ErrNotDeleted /
	// ErrNotFound via classifyRestoreError.
	err := s.Pool.QueryRow(ctx, `
		WITH old AS (
		    SELECT deleted_reason, deleted_at
		      FROM assets
		     WHERE id = $1 AND deleted_at IS NOT NULL
		)
		UPDATE assets
		   SET deleted_at = NULL,
		       deleted_reason = NULL,
		       updated_at = NOW()
		 WHERE id = $1
		   AND EXISTS (SELECT 1 FROM old)
		RETURNING (SELECT deleted_reason FROM old), (SELECT deleted_at FROM old)
	`, assetID).Scan(&priorReason, &priorDeletedAt)

	if err != nil {
		return s.classifyRestoreError(ctx, "assets", assetID.String(), err)
	}

	reason := ""
	if priorReason != nil {
		reason = *priorReason
	}
	age := time.Since(priorDeletedAt)
	if s.Rec != nil {
		s.Rec.AdminAssetRestored(ctx, req, assetID.String(), actorUserRef, reason, int64(age.Seconds()))
	}
	return nil
}

// RestorePost mirrors RestoreAsset for posts.
func (s *Service) RestorePost(ctx context.Context, req *http.Request, postID uuid.UUID, actorUserRef int64) error {
	var priorReason *string
	var priorDeletedAt time.Time

	err := s.Pool.QueryRow(ctx, `
		WITH old AS (
		    SELECT deleted_reason, deleted_at
		      FROM posts
		     WHERE id = $1 AND deleted_at IS NOT NULL
		)
		UPDATE posts
		   SET deleted_at = NULL,
		       deleted_reason = NULL,
		       updated_at = NOW()
		 WHERE id = $1
		   AND EXISTS (SELECT 1 FROM old)
		RETURNING (SELECT deleted_reason FROM old), (SELECT deleted_at FROM old)
	`, postID).Scan(&priorReason, &priorDeletedAt)

	if err != nil {
		return s.classifyRestoreError(ctx, "posts", postID.String(), err)
	}

	reason := ""
	if priorReason != nil {
		reason = *priorReason
	}
	age := time.Since(priorDeletedAt)
	if s.Rec != nil {
		s.Rec.AdminPostRestored(ctx, req, postID.String(), actorUserRef, reason, int64(age.Seconds()))
	}
	return nil
}

// RestoreCollection mirrors RestoreAsset for collections. Collections
// gained deleted_at in migration 00001; pre-migration collection
// rows are all live (deleted_at IS NULL by default), so the
// "already-live" case returns ErrNotDeleted as usual.
func (s *Service) RestoreCollection(ctx context.Context, req *http.Request, collectionID uuid.UUID, actorUserRef int64) error {
	var priorReason *string
	var priorDeletedAt time.Time

	err := s.Pool.QueryRow(ctx, `
		WITH old AS (
		    SELECT deleted_reason, deleted_at
		      FROM collections
		     WHERE id = $1 AND deleted_at IS NOT NULL
		)
		UPDATE collections
		   SET deleted_at = NULL,
		       deleted_reason = NULL,
		       updated_at = NOW()
		 WHERE id = $1
		   AND EXISTS (SELECT 1 FROM old)
		RETURNING (SELECT deleted_reason FROM old), (SELECT deleted_at FROM old)
	`, collectionID).Scan(&priorReason, &priorDeletedAt)

	if err != nil {
		return s.classifyRestoreError(ctx, "collections", collectionID.String(), err)
	}

	reason := ""
	if priorReason != nil {
		reason = *priorReason
	}
	age := time.Since(priorDeletedAt)
	if s.Rec != nil {
		s.Rec.AdminCollectionRestored(ctx, req, collectionID.String(), actorUserRef, reason, int64(age.Seconds()))
	}
	return nil
}

// classifyRestoreError disambiguates pgx.ErrNoRows into
// ErrNotDeleted vs ErrNotFound. The RETURNING clause on the UPDATE
// yields no row on BOTH "row doesn't exist" AND "row exists but
// deleted_at IS NULL" — a follow-up SELECT tells them apart.
func (s *Service) classifyRestoreError(ctx context.Context, table, id string, err error) error {
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("softdelete: restore %s: %w", table, err)
	}
	var exists bool
	//nolint:gosec // table is a compile-time constant from the caller
	q := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id = $1)`, table)
	if selErr := s.Pool.QueryRow(ctx, q, id).Scan(&exists); selErr != nil {
		return fmt.Errorf("softdelete: restore %s: classify: %w", table, selErr)
	}
	if exists {
		return ErrNotDeleted
	}
	return ErrNotFound
}

// HardDeleteResult reports the outcome of a HardDeletePast pass.
type HardDeleteResult struct {
	Entity       string // "assets" / "posts" / "collections" / "user"
	DeletedCount int
}

// HardDeletePastAssets hard-deletes assets whose deleted_at is
// older than retentionDays. Returns the number of rows deleted.
// Fires one audit event per victim (bounded batch — the coordinator
// caps the pass at 100 rows/tick to keep the audit-fanout finite).
func (s *Service) HardDeletePastAssets(ctx context.Context, retentionDays int, batchSize int) (int, error) {
	if retentionDays < 1 {
		return 0, fmt.Errorf("softdelete: retention_days must be >= 1")
	}
	if batchSize < 1 {
		batchSize = 100
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, deleted_reason, deleted_at
		  FROM assets
		 WHERE deleted_at IS NOT NULL
		   AND deleted_at < NOW() - make_interval(days => $1)
		 ORDER BY deleted_at
		 LIMIT $2
	`, retentionDays, batchSize)
	if err != nil {
		return 0, fmt.Errorf("softdelete: assets query: %w", err)
	}
	victims, err := collectVictims(rows)
	if err != nil {
		return 0, err
	}
	if len(victims) == 0 {
		return 0, nil
	}
	ids := make([]uuid.UUID, len(victims))
	for i, v := range victims {
		ids[i] = v.ID
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM assets WHERE id = ANY($1)`, ids); err != nil {
		return 0, fmt.Errorf("softdelete: assets delete: %w", err)
	}
	// The CASCADEs have fired; tell the packages whose caches they
	// silently emptied. After the DELETE and before the audit fanout,
	// so a stale cache window can't outlive the transaction by the
	// length of a batch's worth of audit writes.
	if s.OnAssetsHardDeleted != nil {
		s.OnAssetsHardDeleted(ctx, ids)
	}
	if s.Rec != nil {
		for _, v := range victims {
			daysOver := int(time.Since(v.DeletedAt).Hours()/24) - retentionDays
			if daysOver < 0 {
				daysOver = 0
			}
			s.Rec.AdminAssetHardDeletedByGC(ctx, v.ID.String(), v.Reason, retentionDays, daysOver)
		}
	}
	return len(victims), nil
}

// HardDeletePastPosts mirrors HardDeletePastAssets for posts.
func (s *Service) HardDeletePastPosts(ctx context.Context, retentionDays, batchSize int) (int, error) {
	if retentionDays < 1 {
		return 0, fmt.Errorf("softdelete: retention_days must be >= 1")
	}
	if batchSize < 1 {
		batchSize = 100
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, deleted_reason, deleted_at
		  FROM posts
		 WHERE deleted_at IS NOT NULL
		   AND deleted_at < NOW() - make_interval(days => $1)
		 ORDER BY deleted_at
		 LIMIT $2
	`, retentionDays, batchSize)
	if err != nil {
		return 0, fmt.Errorf("softdelete: posts query: %w", err)
	}
	victims, err := collectVictims(rows)
	if err != nil {
		return 0, err
	}
	if len(victims) == 0 {
		return 0, nil
	}
	ids := make([]uuid.UUID, len(victims))
	for i, v := range victims {
		ids[i] = v.ID
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM posts WHERE id = ANY($1)`, ids); err != nil {
		return 0, fmt.Errorf("softdelete: posts delete: %w", err)
	}
	if s.Rec != nil {
		for _, v := range victims {
			daysOver := int(time.Since(v.DeletedAt).Hours()/24) - retentionDays
			if daysOver < 0 {
				daysOver = 0
			}
			s.Rec.AdminPostHardDeletedByGC(ctx, v.ID.String(), v.Reason, retentionDays, daysOver)
		}
	}
	return len(victims), nil
}

// HardDeletePastCollections mirrors HardDeletePastAssets for
// collections. Cascade to collection_resources / collection_posts /
// collection_acls fires via the existing FK ON DELETE CASCADE
// relationships in the baseline schema.
func (s *Service) HardDeletePastCollections(ctx context.Context, retentionDays, batchSize int) (int, error) {
	if retentionDays < 1 {
		return 0, fmt.Errorf("softdelete: retention_days must be >= 1")
	}
	if batchSize < 1 {
		batchSize = 100
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, deleted_reason, deleted_at
		  FROM collections
		 WHERE deleted_at IS NOT NULL
		   AND deleted_at < NOW() - make_interval(days => $1)
		 ORDER BY deleted_at
		 LIMIT $2
	`, retentionDays, batchSize)
	if err != nil {
		return 0, fmt.Errorf("softdelete: collections query: %w", err)
	}
	victims, err := collectVictims(rows)
	if err != nil {
		return 0, err
	}
	if len(victims) == 0 {
		return 0, nil
	}
	ids := make([]uuid.UUID, len(victims))
	for i, v := range victims {
		ids[i] = v.ID
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM collections WHERE id = ANY($1)`, ids); err != nil {
		return 0, fmt.Errorf("softdelete: collections delete: %w", err)
	}
	if s.Rec != nil {
		for _, v := range victims {
			daysOver := int(time.Since(v.DeletedAt).Hours()/24) - retentionDays
			if daysOver < 0 {
				daysOver = 0
			}
			s.Rec.AdminCollectionHardDeletedByGC(ctx, v.ID.String(), v.Reason, retentionDays, daysOver)
		}
	}
	return len(victims), nil
}

// HardDeletePastArchivedUsers hard-deletes users whose approved
// field equals UserStateArchived (=3) AND whose most-recent
// user.status_changed audit event transitioning TO archived is
// older than retentionDays. Uses the audit-events feed as the
// archived-at anchor because the user table has no dedicated
// deleted_at column (hybrid scope per 1.55.C-1 pre-audit).
//
// Anchor query: for each archived user, take MAX(created_at) FROM
// audit_events WHERE event_type='admin.users.archived' AND
// subject_user_ref = user.ref. If no such row exists (edge case:
// user manually flipped to archived via direct DB write), the
// user is skipped (no anchor → no reliable timeline).
func (s *Service) HardDeletePastArchivedUsers(ctx context.Context, retentionDays, batchSize int) (int, error) {
	if retentionDays < 1 {
		return 0, fmt.Errorf("softdelete: retention_days must be >= 1")
	}
	if batchSize < 1 {
		batchSize = 100
	}
	// The archived-at anchor is the MAX(created_at) on the
	// admin.users.archived audit row per user. Users with no
	// audit row → no reliable anchor → skipped.
	rows, err := s.Pool.Query(ctx, `
		WITH anchor AS (
		    SELECT (subject_user_ref)::BIGINT AS user_ref,
		           MAX(created_at)            AS archived_at
		      FROM audit_events
		     WHERE event_type = 'admin.users.archived'
		       AND subject_user_ref IS NOT NULL
		     GROUP BY subject_user_ref
		)
		SELECT u.ref, a.archived_at, COALESCE(u.comments, '')
		  FROM "user" u
		  JOIN anchor a ON a.user_ref = u.ref
		 WHERE u.approved = 3
		   AND a.archived_at < NOW() - make_interval(days => $1)
		 ORDER BY a.archived_at
		 LIMIT $2
	`, retentionDays, batchSize)
	if err != nil {
		return 0, fmt.Errorf("softdelete: user query: %w", err)
	}
	defer rows.Close()

	type userVictim struct {
		Ref        int64
		ArchivedAt time.Time
		Reason     string
	}
	var victims []userVictim
	for rows.Next() {
		var v userVictim
		if err := rows.Scan(&v.Ref, &v.ArchivedAt, &v.Reason); err != nil {
			return 0, fmt.Errorf("softdelete: user scan: %w", err)
		}
		victims = append(victims, v)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("softdelete: user rows: %w", err)
	}
	if len(victims) == 0 {
		return 0, nil
	}
	refs := make([]int64, len(victims))
	for i, v := range victims {
		refs[i] = v.Ref
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM "user" WHERE ref = ANY($1)`, refs); err != nil {
		return 0, fmt.Errorf("softdelete: user delete: %w", err)
	}
	if s.Rec != nil {
		for _, v := range victims {
			daysOver := int(time.Since(v.ArchivedAt).Hours()/24) - retentionDays
			if daysOver < 0 {
				daysOver = 0
			}
			s.Rec.AdminUserHardDeletedByGC(ctx, v.Ref, v.Reason, retentionDays, daysOver)
		}
	}
	return len(victims), nil
}

type victim struct {
	ID        uuid.UUID
	Reason    string
	DeletedAt time.Time
}

func collectVictims(rows pgx.Rows) ([]victim, error) {
	defer rows.Close()
	var out []victim
	for rows.Next() {
		var v victim
		var reason *string
		if err := rows.Scan(&v.ID, &reason, &v.DeletedAt); err != nil {
			return nil, fmt.Errorf("softdelete: scan: %w", err)
		}
		if reason != nil {
			v.Reason = *reason
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("softdelete: rows: %w", err)
	}
	return out, nil
}
