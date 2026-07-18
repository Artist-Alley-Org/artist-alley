// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

// Sweep job kinds (#403). Both run through the normal job queue so a
// long scan is visible in the jobs admin surface built in S0/S1 rather
// than being an invisible background thread.
const (
	JobOrphanScan     jobs.JobType = "storage.orphan_scan"
	JobChecksumVerify jobs.JobType = "storage.checksum_verify"
)

// sweepBatch is how many objects one Handle() pass examines before
// checkpointing. Sweeps walk potentially large object sets, so they
// checkpoint and re-enqueue rather than holding a single transaction —
// or a single lease — open across the whole scan.
const sweepBatch = 500

// finding kinds, mirroring the CHECK in migration 00007.
const (
	FindingMissingObject    = "missing_object"
	FindingOrphanObject     = "orphan_object"
	FindingChecksumMismatch = "checksum_mismatch"
	FindingSizeMismatch     = "size_mismatch"
)

// sweepPayload is the job payload for both sweep kinds. RunID ties
// every batch of a resumed sweep to one run row; Cursor is the resume
// point handed to the next batch.
type sweepPayload struct {
	RunID  uuid.UUID `json:"run_id"`
	Cursor string    `json:"cursor,omitempty"`
	// Checksum verify walks (hash, variant) pairs, so it needs both
	// halves of the cursor.
	CursorVariant string `json:"cursor_variant,omitempty"`
}

// SweepHandler implements both integrity sweeps. One type serves both
// kinds because they share all the plumbing — run bookkeeping,
// batching, checkpointing — and differ only in the scan itself.
type SweepHandler struct {
	kind    jobs.JobType
	pool    *pgxpool.Pool
	q       *Queries
	backend Backend
	jobsSvc *jobs.Service
	logger  *slog.Logger
}

// NewOrphanScanHandler scans disk->DB: every object the backend holds
// that no storage_variants row references. This is the direction that
// needs Backend.List, and it is where reclaimable waste hides.
func NewOrphanScanHandler(pool *pgxpool.Pool, svc *Service, jobsSvc *jobs.Service, logger *slog.Logger) *SweepHandler {
	return &SweepHandler{kind: JobOrphanScan, pool: pool, q: New(pool), backend: svc.Backend, jobsSvc: jobsSvc, logger: logger}
}

// NewChecksumVerifyHandler scans DB->disk: for every storage_variants
// row, confirm the bytes exist and that they still hash to the key
// they are stored under. Content-addressing makes this cheap to state
// — the hash IS the expected checksum, so no separate checksum column
// is needed.
func NewChecksumVerifyHandler(pool *pgxpool.Pool, svc *Service, jobsSvc *jobs.Service, logger *slog.Logger) *SweepHandler {
	return &SweepHandler{kind: JobChecksumVerify, pool: pool, q: New(pool), backend: svc.Backend, jobsSvc: jobsSvc, logger: logger}
}

// Type implements jobs.Handler.
func (h *SweepHandler) Type() jobs.JobType { return h.kind }

// Handle runs one batch and either re-enqueues itself with an advanced
// cursor or closes the run out. Returning after each batch keeps every
// individual job short, which means a sweep survives a restart, shows
// progress in the admin queue, and never sits on a lease for the
// duration of a full scan.
func (h *SweepHandler) Handle(ctx context.Context, job *jobs.Claim) (json.RawMessage, error) {
	var p sweepPayload
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return nil, fmt.Errorf("storage.sweep: payload: %w", err)
		}
	}
	if p.RunID == uuid.Nil {
		return nil, errors.New("storage.sweep: payload missing run_id")
	}

	var (
		next    sweepPayload
		scanned int64
		found   int64
		err     error
	)
	switch h.kind {
	case JobOrphanScan:
		next, scanned, found, err = h.scanOrphans(ctx, p)
	case JobChecksumVerify:
		next, scanned, found, err = h.verifyChecksums(ctx, p)
	default:
		err = fmt.Errorf("storage.sweep: unknown kind %q", h.kind)
	}
	if err != nil {
		h.fail(ctx, p.RunID, err)
		return nil, err
	}

	done := next.Cursor == "" && next.CursorVariant == ""
	cursor := next.Cursor
	if next.CursorVariant != "" {
		cursor = next.Cursor + "\x00" + next.CursorVariant
	}
	if uerr := h.q.AdvanceSweepRun(ctx, AdvanceSweepRunParams{
		ID:             pgtype.UUID{Bytes: p.RunID, Valid: true},
		Cursor:         strPtr(cursor),
		ObjectsScanned: scanned,
		FindingsCount:  found,
	}); uerr != nil {
		return nil, fmt.Errorf("storage.sweep: checkpoint: %w", uerr)
	}

	if done {
		if ferr := h.q.FinishSweepRun(ctx, FinishSweepRunParams{
			ID:     pgtype.UUID{Bytes: p.RunID, Valid: true},
			Status: "completed",
		}); ferr != nil {
			return nil, fmt.Errorf("storage.sweep: finish: %w", ferr)
		}
		return json.Marshal(map[string]any{"run_id": p.RunID, "status": "completed"})
	}

	// More to do — re-enqueue the next batch.
	nextPayload, merr := json.Marshal(next)
	if merr != nil {
		return nil, fmt.Errorf("storage.sweep: next payload: %w", merr)
	}
	if _, eerr := h.jobsSvc.Enqueue(ctx, h.kind, json.RawMessage(nextPayload), jobs.EnqueueOpts{}); eerr != nil {
		return nil, fmt.Errorf("storage.sweep: re-enqueue: %w", eerr)
	}
	return json.Marshal(map[string]any{"run_id": p.RunID, "status": "continued", "scanned": scanned})
}

// scanOrphans walks the backend and records objects with no
// storage_variants row. Existence is checked per ref against the DB;
// the index on (object_hash, variant_key) makes that a point lookup.
func (h *SweepHandler) scanOrphans(ctx context.Context, p sweepPayload) (sweepPayload, int64, int64, error) {
	refs, next, err := h.backend.List(ctx, p.Cursor, sweepBatch)
	if err != nil {
		return sweepPayload{}, 0, 0, fmt.Errorf("list: %w", err)
	}
	var found int64
	for _, r := range refs {
		exists, err := h.q.VariantExists(ctx, VariantExistsParams{ObjectHash: r.Hash, VariantKey: r.Variant})
		if err != nil {
			return sweepPayload{}, 0, 0, fmt.Errorf("variant exists: %w", err)
		}
		if exists {
			continue
		}
		if err := h.record(ctx, p.RunID, FindingOrphanObject, r.Hash, r.Variant,
			fmt.Sprintf("%d bytes on %s with no storage_variants row", r.Size, h.backend.Name())); err != nil {
			return sweepPayload{}, 0, 0, err
		}
		found++
	}
	return sweepPayload{RunID: p.RunID, Cursor: next}, int64(len(refs)), found, nil
}

// verifyChecksums walks storage_variants and, for each row, confirms
// the bytes are present and still hash to the key they live under.
// Because storage is content-addressed, re-hashing the stream and
// comparing to object_hash IS the integrity check.
func (h *SweepHandler) verifyChecksums(ctx context.Context, p sweepPayload) (sweepPayload, int64, int64, error) {
	rows, err := h.q.ListVariantsForVerify(ctx, ListVariantsForVerifyParams{
		Column1: p.Cursor,
		Column2: p.CursorVariant,
		Limit:   sweepBatch,
	})
	if err != nil {
		return sweepPayload{}, 0, 0, fmt.Errorf("list variants: %w", err)
	}
	var found int64
	for _, row := range rows {
		f, detail, err := h.verifyOne(ctx, row.ObjectHash, row.VariantKey, row.SizeBytes)
		if err != nil {
			return sweepPayload{}, 0, 0, err
		}
		if f == "" {
			continue
		}
		if err := h.record(ctx, p.RunID, f, row.ObjectHash, row.VariantKey, detail); err != nil {
			return sweepPayload{}, 0, 0, err
		}
		found++
	}
	if len(rows) == 0 {
		return sweepPayload{RunID: p.RunID}, 0, 0, nil
	}
	last := rows[len(rows)-1]
	return sweepPayload{RunID: p.RunID, Cursor: last.ObjectHash, CursorVariant: last.VariantKey}, int64(len(rows)), found, nil
}

// verifyOne returns the finding kind for a single variant, or "" when
// it is healthy.
func (h *SweepHandler) verifyOne(ctx context.Context, hash, variant string, wantSize int64) (finding, detail string, err error) {
	rc, info, err := h.backend.Get(ctx, hash, variant)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return FindingMissingObject, "row references bytes that are not on " + h.backend.Name(), nil
		}
		return "", "", fmt.Errorf("get %s/%s: %w", hash[:8], variant, err)
	}
	defer rc.Close()

	sum := sha256.New()
	n, cerr := io.Copy(sum, rc)
	if cerr != nil {
		return "", "", fmt.Errorf("read %s/%s: %w", hash[:8], variant, cerr)
	}
	got := hex.EncodeToString(sum.Sum(nil))

	// Only the ORIGINAL variant is addressed by its own content hash.
	// Derivatives live under the parent object's hash, so re-hashing
	// them proves nothing about object_hash — for those we can only
	// check that the recorded length still matches.
	if variant == VariantOriginal && got != hash {
		return FindingChecksumMismatch, fmt.Sprintf("stored bytes hash to %s, expected %s", got[:16], hash[:16]), nil
	}
	if wantSize > 0 && n != wantSize {
		return FindingSizeMismatch, fmt.Sprintf("on disk %d bytes, storage_variants says %d", n, wantSize), nil
	}
	_ = info
	return "", "", nil
}

func (h *SweepHandler) record(ctx context.Context, runID uuid.UUID, finding, hash, variant, detail string) error {
	if err := h.q.RecordSweepFinding(ctx, RecordSweepFindingParams{
		ID:         pgtype.UUID{Bytes: uuid.New(), Valid: true},
		RunID:      pgtype.UUID{Bytes: runID, Valid: true},
		Finding:    finding,
		ObjectHash: hash,
		VariantKey: variant,
		Detail:     detail,
	}); err != nil {
		return fmt.Errorf("storage.sweep: record finding: %w", err)
	}
	return nil
}

func (h *SweepHandler) fail(ctx context.Context, runID uuid.UUID, cause error) {
	msg := cause.Error()
	if err := h.q.FinishSweepRun(ctx, FinishSweepRunParams{
		ID:     pgtype.UUID{Bytes: runID, Valid: true},
		Status: "failed",
		Error:  &msg,
	}); err != nil && h.logger != nil {
		h.logger.Error("storage.sweep: marking run failed", "run_id", runID, "err", err)
	}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
