package assets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/lint"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// LintAsset — POST /assets/{id}/lint.
//
// Reads the asset's source bytes (capped at 4 MB so an accidental
// log upload doesn't pin a worker), dispatches to the lint package's
// per-language checker, and returns a flat Diagnostic list the doc
// viewer hands to CodeMirror's lint extension.
//
// The endpoint is intentionally synchronous: every supported linter
// is Go-native and runs in single-digit milliseconds for typical
// docs. Subprocess-based linters (py_compile, eslint, shellcheck)
// will need an async-job pattern when they land; the dispatcher in
// lint.Run already abstracts that future split.
//
// Cache: the diagnostics live on the file's content hash, so a
// hash-aware cache would be free here. Phase C ships uncached;
// if it shows in profiles we'll add a domain to cache.Registry.
const maxLintBytes = 4 * 1024 * 1024

func (h *Handler) LintAsset(
	ctx context.Context,
	req openapi.LintAssetRequestObject,
) (openapi.LintAssetResponseObject, error) {
	if auth.IdentityFromContext(ctx) == nil {
		return openapi.LintAsset401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	q := New(h.Pool)
	row, err := q.GetAsset(ctx, pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return openapi.LintAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset not found"},
			}, nil
		}
		return nil, fmt.Errorf("assets: lint get: %w", err)
	}
	if row.FileHash == nil || *row.FileHash == "" {
		return openapi.LintAsset404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset has no source file"},
		}, nil
	}
	extRaw := ""
	if row.FileExtension != nil {
		extRaw = *row.FileExtension
	}
	ext := strings.ToLower(strings.TrimPrefix(extRaw, "."))
	if ext == "" {
		return openapi.LintAsset200JSONResponse(openapi.LintResult{
			Linter: "none", Skipped: true,
		}), nil
	}

	body, _, err := h.Storage.Download(ctx, *row.FileHash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return openapi.LintAsset404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "asset source missing"},
			}, nil
		}
		return nil, fmt.Errorf("assets: lint download: %w", err)
	}
	defer body.Close()
	text, err := io.ReadAll(io.LimitReader(body, maxLintBytes))
	if err != nil {
		return nil, fmt.Errorf("assets: lint read: %w", err)
	}

	result := lint.Run(ext, text)
	diags := make([]openapi.LintDiagnostic, 0, len(result.Diagnostics))
	for _, d := range result.Diagnostics {
		entry := openapi.LintDiagnostic{
			Line:     d.Line,
			Col:      d.Col,
			Severity: openapi.LintDiagnosticSeverity(d.Severity),
			Message:  d.Message,
			Source:   d.Source,
		}
		if d.EndLine > 0 {
			el := d.EndLine
			entry.EndLine = &el
		}
		if d.EndCol > 0 {
			ec := d.EndCol
			entry.EndCol = &ec
		}
		diags = append(diags, entry)
	}
	return openapi.LintAsset200JSONResponse(openapi.LintResult{
		Linter:      result.Linter,
		Skipped:     result.Skipped,
		Diagnostics: &diags,
	}), nil
}
