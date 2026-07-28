// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.14.A-bridge — assets.Handler implements the AI bridge
// interfaces declared in app/internal/ai/bridge.go.
//
// Two surfaces:
//
//   ai.AssetLookup  (GetAssetForAI) → read-side projection that AI
//                                     handlers consume. Returns
//                                     ai.AssetForAI; existing tags
//                                     are joined-aggregated so the
//                                     handler can include them as
//                                     prompt context.
//   ai.TagWriter    (SetAITagsForAsset) → preserve-manual merge
//                                          semantics inside a tx.
//                                          AI tags removed +
//                                          re-inserted; manual /
//                                          import tags untouched.
//
// CaptionWriter / EmbeddingWriter / TranscriptWriter are stubbed
// at the boot layer until their providing phases ship.

package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/ai"

	"github.com/mscrnt/artist-alley/app/internal/asset/imagefmt"
)

// GetAssetForAI satisfies ai.AssetLookup. Returns the typed
// AssetForAI projection with existing tags grouped by source.
//
// Maps pgx.ErrNoRows to ai.ErrAssetNotFound so job handlers can
// classify via errors.Is and map to TerminalError.
func (h *Handler) GetAssetForAI(ctx context.Context, id uuid.UUID) (ai.AssetForAI, error) {
	row, err := New(h.Pool).GetAssetForAIBridge(ctx,
		pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ai.AssetForAI{}, ai.ErrAssetNotFound
		}
		return ai.AssetForAI{}, fmt.Errorf("assets: GetAssetForAI: %w", err)
	}

	out := ai.AssetForAI{
		ID:          id,
		Sensitivity: ai.SensitivityTier(row.Sensitivity),
		Title:       row.Title,
		ContentHash: nullableString(row.FileHash),
	}

	if row.TeamID.Valid {
		teamID := uuid.UUID(row.TeamID.Bytes)
		out.OwningTeamID = &teamID
	}

	// MimeType isn't stored on the assets row — derive it from the file
	// extension (#579).
	//
	// This used to read `if row.HasImage`, a column that is DEFAULT
	// false with no writer anywhere, so the hint was never set for ANY
	// asset and the AI handler never learned that an asset was an image.
	// The extension is real data, so this now yields the actual MIME
	// ("image/png") rather than the "image/*" wildcard the old code
	// aspired to — the wildcard existed because has_image could only
	// ever say "image, kind unknown".
	if mime := imagefmt.MimeTypeForExtension(nullableString(row.FileExtension)); mime != "" {
		out.MimeType = mime
	}

	// existing_tags arrives as a JSON string from json_agg. Parse it
	// into the typed TagInput slice. Empty array is the default for
	// untagged assets.
	if row.ExistingTagsJson != "" {
		var existing []struct {
			Tag    string `json:"tag"`
			Source string `json:"source"`
		}
		if err := json.Unmarshal([]byte(row.ExistingTagsJson), &existing); err != nil {
			// Don't fail the lookup over malformed JSON — log + skip
			// the tag context. The AI handler still gets the asset
			// projection and can run without context.
			h.Logger.Warn("assets.GetAssetForAI: parse existing_tags",
				"asset_id", id.String(), "err", err.Error())
		} else {
			out.ExistingTags = make([]ai.TagInput, 0, len(existing))
			for _, t := range existing {
				out.ExistingTags = append(out.ExistingTags, ai.TagInput{
					Value:  t.Tag,
					Source: ai.TagSource(t.Source),
				})
			}
		}
	}

	return out, nil
}

// SetAITagsForAsset satisfies ai.TagWriter. Wraps the delete +
// inserts in a single transaction so the merge is atomic — no
// window where the asset has zero tags.
//
// Merge semantics:
//   - DELETE WHERE source = 'ai' removes only AI-source tags
//   - INSERT loop adds fresh AI-source tags with provenance
//   - Manual + import tags are untouched (separate rows; the
//     DELETE's WHERE clause excludes them)
func (h *Handler) SetAITagsForAsset(
	ctx context.Context,
	assetID uuid.UUID,
	tags []ai.TagOutput,
	prov ai.AIProvenance,
) error {
	// Pre-check the asset exists. Cheap; lets us return the
	// sentinel before opening a tx.
	exists, err := New(h.Pool).AssetExistsForAI(ctx,
		pgtype.UUID{Bytes: assetID, Valid: true})
	if err != nil {
		return fmt.Errorf("assets: SetAITagsForAsset: check exists: %w", err)
	}
	if !exists {
		return ai.ErrAssetNotFound
	}

	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("assets: SetAITagsForAsset: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := New(tx)

	if err := q.DeleteAITagsForAsset(ctx,
		pgtype.UUID{Bytes: assetID, Valid: true}); err != nil {
		return fmt.Errorf("assets: SetAITagsForAsset: delete: %w", err)
	}

	for _, t := range tags {
		// Skip empty tags defensively — a provider might emit a
		// blank line that survived parsing. Lets the operator's
		// downstream filters work on a clean dataset.
		if t.Value == "" {
			continue
		}
		var conf *float32
		if t.Confidence > 0 {
			c := float32(t.Confidence)
			conf = &c
		}
		if err := q.InsertAITagForAsset(ctx, InsertAITagForAssetParams{
			AssetID:    pgtype.UUID{Bytes: assetID, Valid: true},
			Tag:        t.Value,
			Confidence: conf,
			Provider:   nullableProvenance(prov.Provider),
			Model:      nullableProvenance(prov.Model),
		}); err != nil {
			return fmt.Errorf("assets: SetAITagsForAsset: insert %q: %w", t.Value, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("assets: SetAITagsForAsset: commit: %w", err)
	}

	h.Logger.Info("assets.ai.tags.set",
		"asset_id", assetID.String(),
		"count", len(tags),
		"provider", prov.Provider,
		"model", prov.Model,
		"prompt_version", prov.PromptVersion)

	return nil
}

// nullableString returns the value when the pointer is non-nil and
// non-empty, else "". Convenience over `if row.X != nil && *row.X != ""`.
func nullableString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// nullableProvenance wraps a provenance string as a *string
// (sqlc's nullable narg parameter type). Empty input maps to nil
// so SQL NULL ends up in the column rather than the empty string —
// lets analytics distinguish "no provider recorded" from
// "provider name was literally empty".
func nullableProvenance(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Compile-time interface satisfaction checks.
var _ ai.AssetLookup = (*Handler)(nil)
var _ ai.TagWriter = (*Handler)(nil)
