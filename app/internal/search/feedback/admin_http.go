package feedback

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// AdminAuditor is the seam for audit-logging the abuse-review page
// access. Implemented by app/internal/audit.Recorder.
type AdminAuditor interface {
	AdminSearchFeedbackAuditViewed(ctx context.Context, r *http.Request, subjectUserRef, actorUserRef int64)
}

// AdminHandler mounts the admin-facing aggregation + abuse-review
// endpoints. All routes require the "system.admin" capability.
type AdminHandler struct {
	Service *Service
	Auditor AdminAuditor
	Logger  *slog.Logger
}

// Mount attaches the admin routes.
func (h *AdminHandler) Mount(r chi.Router) {
	r.Get("/admin/search/feedback", h.aggregation)
	r.Get("/admin/search/feedback/audit/{user_ref}", h.perUser)
}

func (h *AdminHandler) aggregation(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	limit := int32(20)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 100 {
			limit = int32(n)
		}
	}
	top, err := h.Service.TopQueriesByDownvote(r.Context(), limit)
	if err != nil {
		h.logError(r, "feedback.admin.top_queries", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	under, err := h.Service.UnderRankedHits(r.Context(), 5, limit)
	if err != nil {
		h.logError(r, "feedback.admin.under_ranked", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"top_queries":       topQueryRowsToJSON(top),
		"under_ranked_hits": underRankedRowsToJSON(under),
	})
}

func (h *AdminHandler) perUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	subjectRef, err := strconv.ParseInt(chi.URLParam(r, "user_ref"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_user_ref"})
		return
	}
	limit := int32(50)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 500 {
			limit = int32(n)
		}
	}
	// Audit-log BEFORE the query so the access is recorded even if
	// the query fails. Best-effort — a failing audit write doesn't
	// block the admin from doing their job.
	id := auth.IdentityFromContext(r.Context())
	if id != nil && h.Auditor != nil {
		h.Auditor.AdminSearchFeedbackAuditViewed(r.Context(), r, subjectRef, id.UserRef)
	}
	rows, err := h.Service.ListForUser(r.Context(), subjectRef, limit)
	if err != nil {
		h.logError(r, "feedback.admin.per_user", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_ref": subjectRef,
		"items":    perUserRowsToJSON(rows),
	})
}

// --- helpers --------------------------------------------------

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return false
	}
	if !id.Can("system.admin") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin_required"})
		return false
	}
	return true
}

func (h *AdminHandler) logError(r *http.Request, op string, err error) {
	if h.Logger != nil {
		h.Logger.LogAttrs(r.Context(), slog.LevelWarn, op,
			slog.String("err", err.Error()))
	}
}

func topQueryRowsToJSON(rows []TopQueryRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"query_hash":     r.QueryHash,
			"dsl_query":      r.DSLQuery,
			"total_votes":    r.TotalVotes,
			"down_votes":     r.DownVotes,
			"down_vote_pct":  r.DownVotePct,
		})
	}
	return out
}

func underRankedRowsToJSON(rows []UnderRankedHitRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"hit_asset_id": r.HitAssetID.String(),
			"query_hash":   r.QueryHash,
			"dsl_query":    r.DSLQuery,
			"avg_position": r.AvgPos,
			"up_votes":     r.UpVotes,
			"asset_title":  r.AssetTitle,
		})
	}
	return out
}

func perUserRowsToJSON(rows []PerUserRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m := map[string]any{
			"id":            r.ID.String(),
			"query_hash":    r.QueryHash,
			"dsl_query":     r.DSLQuery,
			"hit_asset_id":  r.HitAssetID.String(),
			"hit_position":  r.HitPosition,
			"direction":     string(r.Direction),
			"feedback_at":   r.FeedbackAt.Format("2006-01-02T15:04:05.000000000Z07:00"),
			"asset_title":   r.AssetTitle,
		}
		if r.IPHash != nil {
			m["ip_hash"] = *r.IPHash
		}
		out = append(out, m)
	}
	return out
}
