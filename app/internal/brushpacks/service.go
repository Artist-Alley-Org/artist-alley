// Package brushpacks — service layer that imports brush packs
// (Photoshop .abr files, our own native format in the future) and
// serves the resulting stamps back to the frontend renderer.
//
// Responsibilities:
//   - Parse uploaded .abr bytes via internal/abr.
//   - Re-encode each decoded brush stamp as PNG, content-address it,
//     and Put() it through the storage backend (fs in dev, S3 in
//     prod). Content-addressed = same bitmap → same hash → one
//     storage object even across packs.
//   - Insert pack + per-stamp manifest rows in Postgres, owner-
//     scoped so users can't enumerate other users' packs.
//   - Stream stamp bytes back to clients on demand.
//
// What lives elsewhere:
//   - HTTP routing + authn → internal/http (handler wraps Service).
//   - SQL queries → queries.sql (sqlc-generated queries.sql.go).
//   - Parsing → internal/abr (already shipped Phase 1.21a).

package brushpacks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image/png"
	"io"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/abr"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// stampStorageVariant is the variant key we Put() brush stamps
// under. Picking a stable, single-token variant means future
// alternate-encoding variants (e.g., a 2× downsampled thumbnail
// for the pack-picker UI) can be added as separate variants
// without breaking the storage_key column shape.
const stampStorageVariant = "brush-stamp.png"

// ErrPackNotFound is returned by GetPack / DeletePack when the
// requested pack doesn't exist for the asking owner. We deliberately
// do NOT distinguish "doesn't exist" from "exists but owned by
// someone else" — same error, same HTTP 404 at the handler — so
// pack existence doesn't leak across users.
var ErrPackNotFound = errors.New("brushpacks: pack not found")

// ErrStampNotFound is the per-stamp equivalent.
var ErrStampNotFound = errors.New("brushpacks: stamp not found")

// Service binds the package's three dependencies. Constructed once
// in main + injected into the HTTP handler.
type Service struct {
	db      *pgxpool.Pool
	q       *Queries
	storage storage.Backend
}

// NewService wires the dependencies. Caller owns the pool +
// storage lifecycle.
func NewService(pool *pgxpool.Pool, store storage.Backend) *Service {
	return &Service{db: pool, q: New(pool), storage: store}
}

// ImportResult is what ImportABR returns on success.
type ImportResult struct {
	Pack   BrushPack
	Stamps []BrushPackStamp
}

// ImportABR reads a complete .abr file from r, parses it, writes
// every decoded stamp to storage as a PNG, and persists the
// manifest. The whole import is atomic at the *DB* level (single
// pgx transaction); failed storage writes leave orphaned bytes
// that a future GC pass can clean up. We accept that tradeoff
// because pre-flighting every storage write into a 2-phase commit
// would double the wire traffic for no real win — the storage GC
// is something we'll need anyway for asset uploads.
//
// `name` is the user-supplied pack name (the panel shows this);
// `sourceFile` is the original .abr filename, kept for display.
func (s *Service) ImportABR(ctx context.Context, ownerRef int64, name, sourceFile string, r io.Reader) (*ImportResult, error) {
	brushes, err := abr.ParseBrushes(r)
	if err != nil {
		return nil, fmt.Errorf("brushpacks: parse: %w", err)
	}
	if len(brushes) == 0 {
		return nil, fmt.Errorf("brushpacks: no decodable brushes in pack")
	}

	// Encode every stamp + content-address it FIRST, before we touch
	// the DB. That way a partial encode failure (e.g., one brush has
	// invalid pixel data) aborts the import with no DB or storage
	// state changed. If all encodes succeed, the rest of the import
	// is mostly idempotent (storage Put is idempotent; the DB
	// transaction either commits or rolls back).
	encoded := make([]encodedStamp, 0, len(brushes))
	for _, b := range brushes {
		buf := &bytes.Buffer{}
		if err := png.Encode(buf, b.AsImage()); err != nil {
			return nil, fmt.Errorf("brushpacks: encode stamp %s: %w", b.ID, err)
		}
		hash := sha256.Sum256(buf.Bytes())
		hexHash := hex.EncodeToString(hash[:])
		encoded = append(encoded, encodedStamp{brush: b, png: buf.Bytes(), hash: hexHash})
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("brushpacks: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	pack, err := qtx.CreatePack(ctx, CreatePackParams{
		OwnerUserRef:   ownerRef,
		Name:       name,
		SourceFile: nilStr(sourceFile),
	})
	if err != nil {
		return nil, fmt.Errorf("brushpacks: create pack: %w", err)
	}

	// Storage writes happen outside the transaction since the
	// backend isn't transactional. We Put() each stamp before
	// inserting its row so a Put failure halts the import before
	// the DB row claims a bitmap that isn't there.
	stampRows := make([]BrushPackStamp, 0, len(encoded))
	for _, e := range encoded {
		if _, err := s.storage.Put(ctx, e.hash, stampStorageVariant, bytes.NewReader(e.png)); err != nil {
			return nil, fmt.Errorf("brushpacks: storage put %s: %w", e.hash, err)
		}
		row, err := qtx.InsertStamp(ctx, InsertStampParams{
			PackID:        pack.ID,
			AbrID:         nilStr(e.brush.ID),
			Label:         nil, // TODO Phase 1.21c-bis: pull from desc block
			Width:         int32(e.brush.Width),
			Height:        int32(e.brush.Height),
			StorageKey:    e.hash,
			Spacing:       defaultStampSpacing,
			AlignToPath:   false,
			SizeJitter:    nil,
			OpacityJitter: nil,
			AngleJitter:   nil,
		})
		if err != nil {
			return nil, fmt.Errorf("brushpacks: insert stamp: %w", err)
		}
		stampRows = append(stampRows, row)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("brushpacks: commit: %w", err)
	}
	return &ImportResult{Pack: pack, Stamps: stampRows}, nil
}

// ListPacks returns every pack the owner has imported, most-recent
// first.
func (s *Service) ListPacks(ctx context.Context, ownerRef int64) ([]BrushPack, error) {
	return s.q.ListPacksForOwner(ctx, ownerRef)
}

// GetPack returns one pack + its stamps. Returns ErrPackNotFound
// when the pack doesn't belong to the owner (which intentionally
// looks identical to "doesn't exist").
func (s *Service) GetPack(ctx context.Context, ownerRef int64, packID pgtype.UUID) (*BrushPack, []BrushPackStamp, error) {
	pack, err := s.q.GetPackForOwner(ctx, GetPackForOwnerParams{ID: packID, OwnerUserRef: ownerRef})
	if err != nil {
		if isNoRows(err) {
			return nil, nil, ErrPackNotFound
		}
		return nil, nil, err
	}
	stamps, err := s.q.ListStampsForPack(ctx, pack.ID)
	if err != nil {
		return nil, nil, err
	}
	return &pack, stamps, nil
}

// DeletePack drops a pack + (via ON DELETE CASCADE) its stamp
// manifest rows. We do NOT delete the bitmaps from storage here
// because they may be referenced by other packs (content-
// addressing makes dedup the default). A future GC pass walks
// brush_pack_stamps.storage_key to find orphaned objects.
func (s *Service) DeletePack(ctx context.Context, ownerRef int64, packID pgtype.UUID) error {
	n, err := s.q.DeletePackForOwner(ctx, DeletePackForOwnerParams{ID: packID, OwnerUserRef: ownerRef})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrPackNotFound
	}
	return nil
}

// OpenStamp returns a stream of the stamp bitmap bytes for the
// asking owner. Caller MUST close the returned reader. Validates
// ownership via the stamps→packs join before opening storage.
func (s *Service) OpenStamp(ctx context.Context, ownerRef int64, stampID pgtype.UUID) (io.ReadCloser, *BrushPackStamp, error) {
	row, err := s.q.GetStampForOwner(ctx, GetStampForOwnerParams{ID: stampID, OwnerUserRef: ownerRef})
	if err != nil {
		if isNoRows(err) {
			return nil, nil, ErrStampNotFound
		}
		return nil, nil, err
	}
	body, _, err := s.storage.Get(ctx, row.StorageKey, stampStorageVariant)
	if err != nil {
		return nil, nil, fmt.Errorf("brushpacks: storage get %s: %w", row.StorageKey, err)
	}
	return body, &row, nil
}

// ── internals ────────────────────────────────────────────────────

const defaultStampSpacing = 0.1 // GIMP / Photoshop's "smooth" default

type encodedStamp struct {
	brush abr.Brush
	png   []byte
	hash  string
}

// nilStr returns *string pointing at s, or nil when s is empty —
// the convention sqlc's pointer-for-null mapping expects.
func nilStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// isNoRows checks for pgx.ErrNoRows without importing pgx at every
// call site. The literal error matters because pgx wraps it.
func isNoRows(err error) bool {
	return err != nil && err.Error() == "no rows in result set"
}
