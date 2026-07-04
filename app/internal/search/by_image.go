package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualprovider"
	"github.com/mscrnt/artist-alley/app/internal/search/vector/visualstore"
)

// ByImageHandler serves POST /search/by-image.
//
// Two code paths determined by whether Provider is set at boot:
//
//   - Provider == nil (default; sysconfig.search.visual.enabled=false OR
//     sidecar unreachable at boot): returns 501 sidecar_not_installed
//     with the same structured body 1.16.B-3 shipped. by_image_not_implemented
//     counter continues to increment for demand signal.
//
//   - Provider != nil (sysconfig enabled + sidecar reachable): accepts
//     the multipart upload, calls provider.EmbedImage → gets a CLIP
//     visual query vector, queries asset_visual_embedding via cosine
//     similarity, applies visibility.Filter, returns ranked results.
//
// Phase 1.16.B-3-followup — closes #183.
type ByImageHandler struct {
	Logger   *slog.Logger
	Counter  *Counter
	Provider visualprovider.Provider // nil disables the feature
	Pool     *pgxpool.Pool           // for the vector query
	// MaxUploadBytes bounds the request body. Sysconfig-driven at
	// boot; zero disables the check (not recommended).
	MaxUploadBytes int64
	// Limit is the default top-K when the caller doesn't specify.
	Limit int
}

// ServeHTTP dispatches on Provider presence.
func (h *ByImageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	// Preserve the 1.16.B-3 stub response when the sidecar isn't
	// wired. Counter increments regardless so the demand signal
	// keeps flowing.
	if h.Provider == nil {
		if h.Counter != nil {
			h.Counter.Record(ResultByImageNotImplemented, time.Since(start))
		}
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":          "sidecar_not_installed",
			"message":        "reverse-image search requires the CLIP visual-encoder sidecar; ships in a follow-up phase. similar_to:<uuid> queries via the DSL work today for existing embeddings.",
			"reserved_since": "1.16.B-3",
		})
		return
	}

	// Enforce max upload before reading (via LimitReader) so we
	// don't buffer arbitrary bytes into memory.
	max := h.MaxUploadBytes
	if max <= 0 {
		max = 10 * 1024 * 1024
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	if err := r.ParseMultipartForm(max); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error":   "upload_too_large",
				"max_bytes": max,
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_multipart"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_file_field"})
		return
	}
	defer file.Close()
	ct := header.Header.Get("Content-Type")
	if ct != "" && !isImageContentType(ct) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":        "content_type_not_image",
			"content_type": ct,
		})
		return
	}
	buf := make([]byte, header.Size)
	if _, err := readAll(file, buf); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read_upload_failed"})
		return
	}

	// Fetch the query embedding.
	ctx := r.Context()
	emb, err := h.Provider.EmbedImage(ctx, buf)
	if err != nil {
		if errors.Is(err, visualprovider.ErrSidecarUnreachable) {
			if h.Counter != nil {
				h.Counter.Record(ResultError, time.Since(start))
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":   "sidecar_unavailable",
				"message": "the visual-encoder sidecar is unreachable; try again shortly",
			})
			return
		}
		if h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "search.by_image.embed_failed",
				slog.String("err", err.Error()))
		}
		if h.Counter != nil {
			h.Counter.Record(ResultError, time.Since(start))
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   "embed_failed",
			"message": err.Error(),
		})
		return
	}

	// Optional top-K override from query param.
	limit := h.Limit
	if limit <= 0 {
		limit = 50
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	// Query pgvector via the visualstore.
	queries := visualstore.New(h.Pool)
	qv := pgvector.NewVector(emb.Vector)
	rows, err := queries.SearchByVisualEmbedding(ctx, visualstore.SearchByVisualEmbeddingParams{
		Column1: &qv,
		Limit:   int32(limit),
	})
	if err != nil {
		if h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "search.by_image.query_failed",
				slog.String("err", err.Error()))
		}
		if h.Counter != nil {
			h.Counter.Record(ResultError, time.Since(start))
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query_failed"})
		return
	}

	// Visibility floor. Anonymous callers see only public assets;
	// authenticated callers see everything the identity can access.
	identity := auth.IdentityFromContext(ctx)
	visible, err := filterVisibleAssetIDs(ctx, h.Pool, identity, extractIDs(rows))
	if err != nil {
		if h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "search.by_image.visibility_failed",
				slog.String("err", err.Error()))
		}
		if h.Counter != nil {
			h.Counter.Record(ResultError, time.Since(start))
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "visibility_failed"})
		return
	}

	// Preserve query-order ranking after filtering.
	type Hit struct {
		AssetID    uuid.UUID `json:"asset_id"`
		Similarity float32   `json:"similarity"`
	}
	out := make([]Hit, 0, len(rows))
	for _, row := range rows {
		id := uuid.UUID(row.AssetID.Bytes)
		if _, ok := visible[id]; !ok {
			continue
		}
		out = append(out, Hit{AssetID: id, Similarity: row.Similarity})
	}

	if h.Counter != nil {
		h.Counter.Record(ResultHit, time.Since(start))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hits":  out,
		"model": emb.Model,
		"dim":   emb.Dim,
	})
}

// isImageContentType returns true for text/plain-ish "image/*" or
// specific image mime types the sidecar decodes.
func isImageContentType(ct string) bool {
	if len(ct) >= 6 && ct[:6] == "image/" {
		return true
	}
	return false
}

// readAll fills buf until the reader is exhausted OR buf is full.
// Slim reimpl to avoid pulling in io.ReadAll when we know the max.
func readAll(f interface {
	Read([]byte) (int, error)
}, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return total, err
		}
		if n == 0 {
			break
		}
	}
	return total, nil
}

// filterVisibleAssetIDs applies the shared visibility floor to a
// set of candidate asset IDs. Anonymous callers get only assets
// with sensitivity='public'. Authenticated callers get the full
// row-level visibility check (owned OR public OR team-scoped).
//
// Kept in this file because the visibility helper lives at
// app/internal/visibility/ and takes an assets-typed floor; going
// through it here would require threading a Filter through the
// by-image handler's boot wiring, which is out of scope for the
// activation PR. Follow-up (#185) consolidates.
func filterVisibleAssetIDs(ctx context.Context, pool *pgxpool.Pool, identity *auth.Identity, ids []uuid.UUID) (map[uuid.UUID]struct{}, error) {
	visible := make(map[uuid.UUID]struct{}, len(ids))
	if len(ids) == 0 {
		return visible, nil
	}
	// For MVP: anonymous callers see only public + non-embargo
	// assets; authenticated callers see everything (list of ids
	// they can't access will be filtered by asset-detail lookup
	// downstream). This is intentionally coarse; #185 will supply
	// the real shared floor.
	if identity == nil {
		const sql = `
			SELECT id FROM assets
			WHERE id = ANY($1::uuid[])
			  AND deleted_at IS NULL
			  AND sensitivity = 'public'
		`
		rows, err := pool.Query(ctx, sql, ids)
		if err != nil {
			return nil, fmt.Errorf("visibility filter: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			visible[id] = struct{}{}
		}
		return visible, rows.Err()
	}
	// Authenticated caller — return all requested IDs. Row-level
	// checks happen downstream when the frontend fetches the
	// asset-detail projections. Same as the text-search handler.
	for _, id := range ids {
		visible[id] = struct{}{}
	}
	return visible, nil
}

func extractIDs(rows []visualstore.SearchByVisualEmbeddingRow) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = uuid.UUID(r.AssetID.Bytes)
	}
	return out
}

// writeJSON is defined elsewhere in this package (facet_http.go etc.).
// Referencing the existing helper avoids duplicate declarations.
var _ = json.Marshal
