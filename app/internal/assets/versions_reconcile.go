// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

// Version reconciliation (#1596, slice 2): the job that turns an
// object's verified renditions into sealed version file sets.
//
// # When it runs
//
// One `asset.version_reconcile` job per object hash, requested by the
// database (migration 00085), always in the same transaction as the
// change that calls for it:
//   - every terminal processing event (an asset set to ready or failed,
//     which covers every preview handler);
//   - an insert of an asset that needs no processing;
//   - the statement that records a verified digest for a derived variant
//     whose key the access policy can choose, unless a preview job for
//     the bytes is in flight (its terminal event requests it, so a
//     re-render never seals a half-written ladder; HLS segments, frames
//     and sprites never request one);
//   - and the operator command for backfilled drafts.
// Requests coalesce per hash, and one that arrives while a job runs is
// queued after it, so none is lost.
//
// # What it does, for one hash
//
//  1. Releases stale transient promotion pins (bounded: a process that
//     died between a promotion and its release).
//  2. Finds every LOCAL asset whose current version's original is the
//     hash (shared bytes fan out). Nothing happens unless the original's
//     stored bytes are verified (#1597): a sealed file set never lists
//     bytes without a verified SHA-256.
//  3. Chooses each asset's access copies with SelectAccessVariants and
//     promotes each chosen variant once (storage.Service.PromoteVariant).
//     A variant that is unverified, missing or changed is omitted, never
//     published; any other failure fails the job, which retries.
//  4. Per asset, in one transaction holding the asset row and then the
//     version row (the order the version triggers use):
//     - an asset still in processing (pending, processing) is skipped:
//     its terminal event requests another reconcile;
//     - a DRAFT version is sealed with the original and the copies;
//     - a SEALED version whose copies already match (role by role, by
//     digest) is left alone, so a rerun is a no-op;
//     - a sealed version with a changed or new copy gets exactly ONE
//     successor (origin rendition, the same original, predecessor the
//     current version), sealed with the new copies plus every unchanged
//     one carried forward, and the asset moves to it. The predecessor
//     keeps its file set and its pins, so old bytes stay.
//     A copy that cannot be promoted now (say its variant became
//     unverifiable) is not a change: the sealed copy stays.
//  5. Releases every promotion's transient pin, on success and on every
//     failure, only after the transactions that pin the copies to their
//     versions have committed or rolled back.
//
// The DB enforces the rest (00085): a copy is sealed only if it is a
// verified object of its own, held by a promotion or carried forward from
// an earlier version of the asset, and its digest is in the immutable
// digest history of exactly the named variant of the version's original
// (storage_variant_digest_history), so no unrelated object can be
// presented as this asset's rendition.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/viewkind"
)

// staleVersionPromotePins bounds the stale-pin recovery of one run.
const staleVersionPromotePins = 100

// VersionReconciler is the `asset.version_reconcile` job handler.
type VersionReconciler struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
	// Ladder returns the configured still ladder (sysconfig previews).
	// Nil uses sysconfig.DefaultPreviewConfig.
	Ladder func(ctx context.Context) (sysconfig.PreviewConfig, error)
	Logger *slog.Logger

	hooks reconcileHooks

	// announce (#1575) runs inside the transaction that seals a version
	// (a draft sealed, or a rendition successor sealed and made
	// current), after the asset points at it and while the asset row is
	// held: the federation sender announces the new version to the peers
	// of the asset's existing shares (assetsend.RecordSealed). An error
	// rolls the seal back, so the job retries both. Nil announces nothing.
	announce func(ctx context.Context, tx pgx.Tx, assetID uuid.UUID) error
}

// SetAnnouncer installs the sealed-version announcer (see announce).
// Call before the reconciler runs.
func (r *VersionReconciler) SetAnnouncer(f func(ctx context.Context, tx pgx.Tx, assetID uuid.UUID) error) {
	r.announce = f
}

// reconcileHooks are #1596 test seams; zero in production.
type reconcileHooks struct {
	afterPromote  func(hash string)             // after every promotion, before any transaction
	beforeAssetTx func(assetID uuid.UUID) error // before each asset's transaction
}

// ReconcileResult counts what one reconcile did.
type ReconcileResult struct {
	ObjectHash        string `json:"object_hash"`
	Assets            int    `json:"assets"`
	Sealed            int    `json:"sealed"`
	Successors        int    `json:"successors"`
	Unchanged         int    `json:"unchanged"`
	Skipped           int    `json:"skipped"`
	Promoted          int    `json:"promoted"`
	Omitted           int    `json:"omitted"`
	StalePinsReleased int    `json:"stale_pins_released"`
}

// Type implements jobs.Handler.
func (r *VersionReconciler) Type() jobs.JobType { return jobs.TypeAssetVersionReconcile }

// Handle implements jobs.Handler.
func (r *VersionReconciler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	var p struct {
		ObjectHash string `json:"object_hash"`
	}
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("version reconcile: payload: %w", err)}
	}
	if err := storage.ValidateHash(p.ObjectHash); err != nil {
		return nil, &jobs.TerminalError{Err: fmt.Errorf("version reconcile: %w", err)}
	}
	res, err := r.Reconcile(ctx, p.ObjectHash)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

type boundAsset struct {
	id         uuid.UUID
	assetType  *int64
	ext        *string
	processing string
	choices    []AccessChoice
}

// versionCopy is one access entry, in the JSON shape asset_version_seal
// takes.
type versionCopy struct {
	Role              string `json:"role"`
	ObjectHash        string `json:"object_hash"`
	SizeBytes         int64  `json:"size_bytes"`
	MediaType         string `json:"media_type"`
	DerivedFromHash   string `json:"derived_from_hash"`
	DerivedVariantKey string `json:"derived_variant_key"`
}

// Reconcile reconciles the versions of every local asset on hash. See
// this file's header for every rule.
func (r *VersionReconciler) Reconcile(ctx context.Context, hash string) (*ReconcileResult, error) {
	res := &ReconcileResult{ObjectHash: hash}
	if n, err := r.Storage.ReleaseStalePins(ctx, storage.PinSubjectTypeVersionPromote, storage.DefaultImportStaleAfter, staleVersionPromotePins); err != nil {
		r.logger().LogAttrs(ctx, slog.LevelWarn, "assets.version_reconcile.stale_pins", slog.String("err", err.Error()))
	} else {
		res.StalePinsReleased = n
	}

	bound, err := r.boundAssets(ctx, hash)
	if err != nil {
		return nil, err
	}
	res.Assets = len(bound)
	if len(bound) == 0 {
		return res, nil
	}
	var verified bool
	if err := r.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM storage_variants
		WHERE object_hash = $1 AND variant_key = 'original' AND fixity_status = 'verified' AND content_sha256 = $1)`, hash).Scan(&verified); err != nil {
		return nil, fmt.Errorf("version reconcile %s: original fixity: %w", hash[:12], err)
	}
	if !verified {
		// Never sealed over unverified bytes: the operator path
		// (aa backfill-variant-digests, then aa reconcile-asset-versions)
		// brings these back once the original is verified.
		res.Skipped = len(bound)
		return res, nil
	}

	ladder := sysconfig.DefaultPreviewConfig()
	if r.Ladder != nil {
		if ladder, err = r.Ladder(ctx); err != nil {
			return nil, fmt.Errorf("version reconcile: preview ladder: %w", err)
		}
	}
	keys, err := r.variantKeys(ctx, hash)
	if err != nil {
		return nil, err
	}
	needed := map[string]bool{}
	for i := range bound {
		bound[i].choices = SelectAccessVariants(viewkind.ForAsset(bound[i].assetType, bound[i].ext), ladder, keys)
		if bound[i].processing == "pending" || bound[i].processing == "processing" {
			continue
		}
		for _, c := range bound[i].choices {
			needed[c.VariantKey] = true
		}
	}

	// Promote each needed variant once; hold every promotion until the
	// transactions that pin the copies have finished, then release.
	promotions := map[string]*storage.Promotion{}
	defer func() {
		for _, p := range promotions {
			if rerr := r.Storage.ReleasePromotion(context.WithoutCancel(ctx), p); rerr != nil {
				r.logger().LogAttrs(ctx, slog.LevelWarn, "assets.version_reconcile.release",
					slog.String("hash", p.Hash), slog.String("err", rerr.Error()))
			}
		}
	}()
	for _, key := range sortedKeys(needed) {
		p, err := r.Storage.PromoteVariant(ctx, hash, key)
		switch {
		case err == nil:
			promotions[key] = p
			res.Promoted++
		case errors.Is(err, storage.ErrPromoteNotVerified), errors.Is(err, storage.ErrPromoteMissing),
			errors.Is(err, storage.ErrPromoteChanged), errors.Is(err, storage.ErrPromoteExistingUnverified):
			res.Omitted++
			r.logger().LogAttrs(ctx, slog.LevelInfo, "assets.version_reconcile.omitted",
				slog.String("hash", hash), slog.String("variant", key), slog.String("why", err.Error()))
		default:
			return nil, fmt.Errorf("version reconcile %s/%s: %w", hash[:12], key, err)
		}
	}
	if r.hooks.afterPromote != nil {
		r.hooks.afterPromote(hash)
	}

	for _, a := range bound {
		if r.hooks.beforeAssetTx != nil {
			if err := r.hooks.beforeAssetTx(a.id); err != nil {
				return nil, err
			}
		}
		outcome, err := r.reconcileAsset(ctx, a, hash, promotions)
		if err != nil {
			return nil, fmt.Errorf("version reconcile %s: asset %s: %w", hash[:12], a.id, err)
		}
		switch outcome {
		case "sealed":
			res.Sealed++
		case "successor":
			res.Successors++
		case "unchanged":
			res.Unchanged++
		default:
			res.Skipped++
		}
	}
	return res, nil
}

func (r *VersionReconciler) boundAssets(ctx context.Context, hash string) ([]boundAsset, error) {
	rows, err := r.Pool.Query(ctx, `SELECT id, asset_type, file_extension, processing_status
		FROM assets WHERE file_hash = $1 AND origin_server_id IS NULL AND current_version_id IS NOT NULL
		ORDER BY id`, hash)
	if err != nil {
		return nil, fmt.Errorf("version reconcile %s: assets: %w", hash[:12], err)
	}
	defer rows.Close()
	var out []boundAsset
	for rows.Next() {
		var a boundAsset
		var id uuid.UUID
		if err := rows.Scan(&id, &a.assetType, &a.ext, &a.processing); err != nil {
			return nil, err
		}
		a.id = id
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *VersionReconciler) variantKeys(ctx context.Context, hash string) ([]string, error) {
	rows, err := r.Pool.Query(ctx, `SELECT variant_key FROM storage_variants WHERE object_hash = $1`, hash)
	if err != nil {
		return nil, fmt.Errorf("version reconcile %s: variants: %w", hash[:12], err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// reconcileAsset applies the rules to one asset in one transaction and
// reports "sealed", "successor", "unchanged" or "skipped".
func (r *VersionReconciler) reconcileAsset(ctx context.Context, a boundAsset, hash string, promotions map[string]*storage.Promotion) (string, error) {
	desired := map[string]versionCopy{}
	for _, c := range a.choices {
		if p, ok := promotions[c.VariantKey]; ok {
			desired[string(c.Role)] = versionCopy{
				Role: string(c.Role), ObjectHash: p.Hash, SizeBytes: p.Size, MediaType: p.MediaType,
				DerivedFromHash: hash, DerivedVariantKey: c.VariantKey,
			}
		}
	}
	outcome := "skipped"
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		var (
			cur        *uuid.UUID
			fileHash   *string
			remote     bool
			processing string
		)
		err := tx.QueryRow(ctx, `SELECT current_version_id, file_hash, origin_server_id IS NOT NULL, processing_status
			FROM assets WHERE id = $1 FOR UPDATE`, a.id).Scan(&cur, &fileHash, &remote, &processing)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if remote || cur == nil || fileHash == nil || *fileHash != hash || processing == "pending" || processing == "processing" {
			return nil
		}
		var (
			number   int64
			sealed   bool
			ext      *string
			basename *string
			source   string
		)
		if err := tx.QueryRow(ctx, `SELECT number, sealed_at IS NOT NULL, original_extension, original_basename, original_basename_source
			FROM asset_versions WHERE id = $1 FOR UPDATE`, *cur).Scan(&number, &sealed, &ext, &basename, &source); err != nil {
			return err
		}
		if !sealed {
			if err := sealVersion(ctx, tx, *cur, desired); err != nil {
				return err
			}
			if err := r.announceSealed(ctx, tx, a.id); err != nil {
				return err
			}
			outcome = "sealed"
			return nil
		}
		current, err := sealedCopies(ctx, tx, *cur)
		if err != nil {
			return err
		}
		merged := map[string]versionCopy{}
		for role, c := range current {
			merged[role] = c
		}
		for role, c := range desired {
			merged[role] = c
		}
		if sameCopies(merged, current) {
			outcome = "unchanged"
			return nil
		}
		next := uuid.New()
		if _, err := tx.Exec(ctx, `SELECT asset_version_create($1, $2, $3, $4, 'rendition', $5, $6, $7, $8, NULL)`,
			a.id, next, number+1, *cur, hash, ext, basename, source); err != nil {
			return fmt.Errorf("create rendition successor: %w", err)
		}
		if err := sealVersion(ctx, tx, next, merged); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE assets SET current_version_id = $2 WHERE id = $1`, a.id, next); err != nil {
			return fmt.Errorf("move to the successor: %w", err)
		}
		if err := r.announceSealed(ctx, tx, a.id); err != nil {
			return err
		}
		outcome = "successor"
		return nil
	})
	return outcome, err
}

// announceSealed runs the announcer for the asset whose current version
// tx just sealed.
func (r *VersionReconciler) announceSealed(ctx context.Context, tx pgx.Tx, assetID uuid.UUID) error {
	if r.announce == nil {
		return nil
	}
	if err := r.announce(ctx, tx, assetID); err != nil {
		return fmt.Errorf("announce the sealed version: %w", err)
	}
	return nil
}

func sealVersion(ctx context.Context, tx pgx.Tx, version uuid.UUID, copies map[string]versionCopy) error {
	entries := make([]versionCopy, 0, len(copies))
	for _, role := range sortedKeys(copiesRoles(copies)) {
		entries = append(entries, copies[role])
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT asset_version_seal($1, $2::jsonb)`, version, string(b)); err != nil {
		return fmt.Errorf("seal %s: %w", version, err)
	}
	return nil
}

func sealedCopies(ctx context.Context, tx pgx.Tx, version uuid.UUID) (map[string]versionCopy, error) {
	rows, err := tx.Query(ctx, `SELECT role, object_hash, size_bytes, media_type, derived_from_hash, derived_variant_key
		FROM asset_version_files WHERE version_id = $1 AND role <> 'original'`, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]versionCopy{}
	for rows.Next() {
		var c versionCopy
		var from, key *string
		if err := rows.Scan(&c.Role, &c.ObjectHash, &c.SizeBytes, &c.MediaType, &from, &key); err != nil {
			return nil, err
		}
		if from != nil {
			c.DerivedFromHash = *from
		}
		if key != nil {
			c.DerivedVariantKey = *key
		}
		out[c.Role] = c
	}
	return out, rows.Err()
}

// sameCopies compares two file sets role by role, by digest.
func sameCopies(a, b map[string]versionCopy) bool {
	if len(a) != len(b) {
		return false
	}
	for role, c := range a {
		if d, ok := b[role]; !ok || d.ObjectHash != c.ObjectHash {
			return false
		}
	}
	return true
}

func copiesRoles(m map[string]versionCopy) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (r *VersionReconciler) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// EnqueueVersionReconcile asks for one reconcile of hash (migration
// 00085's asset_version_reconcile_enqueue: coalesced, never absorbed by a
// running job).
func EnqueueVersionReconcile(ctx context.Context, pool *pgxpool.Pool, hash string) error {
	_, err := pool.Exec(ctx, `SELECT asset_version_reconcile_enqueue($1)`, hash)
	return err
}

// BackfilledDrafts lists, per object hash, the local assets whose current
// version is a draft made by migration 00084's backfill: eligible hashes
// have a verified original (they can be reconciled now), ineligible ones
// do not yet (run aa backfill-variant-digests first). For the operator
// command aa reconcile-asset-versions.
func BackfilledDrafts(ctx context.Context, pool *pgxpool.Pool) (eligible, ineligible []string, drafts int, err error) {
	rows, err := pool.Query(ctx, `SELECT v.original_hash, count(*),
		       EXISTS (SELECT 1 FROM storage_variants sv WHERE sv.object_hash = v.original_hash AND sv.variant_key = 'original'
		                 AND sv.fixity_status = 'verified' AND sv.content_sha256 = v.original_hash)
		  FROM assets a JOIN asset_versions v ON v.id = a.current_version_id
		 WHERE a.origin_server_id IS NULL AND v.sealed_at IS NULL AND v.origin = 'backfill_current'
		 GROUP BY v.original_hash ORDER BY v.original_hash`)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		var n int
		var ok bool
		if err := rows.Scan(&h, &n, &ok); err != nil {
			return nil, nil, 0, err
		}
		drafts += n
		if ok {
			eligible = append(eligible, h)
		} else {
			ineligible = append(ineligible, h)
		}
	}
	return eligible, ineligible, drafts, rows.Err()
}
