package content_search

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search"
)

// Counter is the health-hook interface. Nil-safe. Latency is the
// per-request wall time (loader + engine dispatch); the health
// snapshot rolls into p50/p95/p99.
type Counter interface {
	RecordContentSearch(scope string, hitCount int, latency time.Duration)
}

// AssetPairSource loads a target asset's metadata pairs so the
// asset-scope handler can substring-scan them. Concrete impl is
// presentation.Loader; declared as an interface so tests inject
// fixtures.
type AssetPairSource interface {
	LoadMetadataPairs(ctx context.Context, assetID uuid.UUID, isAnonymous bool) ([]Pair, error)
}

// Pair is the {label, value} shape returned by AssetPairSource.
// Duplicated here (rather than reusing presentation.MetadataPair)
// so the package compiles standalone.
type Pair struct {
	Label string
	Value string
}

// Handler serves both asset-scope + collection-scope Content Search
// endpoints. SiteBaseURL is a boot-time default; the per-request
// origin is derived from publicBaseURL(r) so the emitted IDs match
// the request's actual scheme + host (behind a proxy, from
// X-Forwarded-{Proto,Host}).
type Handler struct {
	Pool        *pgxpool.Pool
	Engine      *search.Engine
	Pairs       AssetPairSource
	SiteBaseURL string
	Counter     Counter
	Logger      *slog.Logger
}

// Mount attaches routes to r. The Presentation Handler's SeeAlso
// links point at these URLs.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/iiif/3/asset/{id}/search", h.serveAsset)
	r.Get("/iiif/3/collection/{id}/search", h.serveCollection)
}

// serveAsset handles GET /iiif/3/asset/{id}/search.
//
// Every metadata pair on the asset whose label or value contains
// the query string (case-insensitive) emits one Annotation targeting
// canvas/1. Empty result pages still return HTTP 200 with an empty
// items list — per spec, search-not-found is expressed as zero
// items, not a 404.
func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	q, ignored := parseQuery(r)
	isAnon := auth.IdentityFromContext(r.Context()) == nil

	baseURL := publicBaseURL(r)
	items := []Annotation{}
	if q != "" && h.Pairs != nil {
		pairs, err := h.Pairs.LoadMetadataPairs(r.Context(), id, isAnon)
		if err == nil {
			needle := strings.ToLower(q)
			canvasID := assetCanvasID(baseURL, id) + "/canvas/1"
			for i, p := range pairs {
				if strings.Contains(strings.ToLower(p.Label), needle) ||
					strings.Contains(strings.ToLower(p.Value), needle) {
					items = append(items, Annotation{
						ID:         assetSearchID(baseURL, id) + "/annotation/" + itoa(i+1),
						Type:       "Annotation",
						Motivation: "supplementing",
						Body: TextualBody{
							Type:        "TextualBody",
							Value:       p.Label + ": " + p.Value,
							Format:      "text/plain",
							Language:    "en",
							Granularity: "line",
						},
						Target: canvasID,
					})
				}
			}
		}
	}

	h.record("asset", len(items), time.Since(start))
	page := AnnotationPage{
		Context: Context,
		ID:      assetSearchID(baseURL, id) + "?q=" + q,
		Type:    "AnnotationPage",
		Items:   items,
		Ignored: ignored,
	}
	if len(items) > 0 {
		page.PartOf = []PartOfRef{{
			ID:    assetSearchID(baseURL, id),
			Type:  "SearchService2",
			Label: en("Search within this asset"),
		}}
	}
	writeSearchJSON(w, page)
}

// serveCollection dispatches through search.Engine restricted to
// asset hits, then filters the result set to only those hits whose
// asset ID is a member of the target collection.
//
// The per-hit Annotation targets the member's manifest URL so
// viewers open the matched asset when the user clicks the search
// result. Motivation is "supplementing" per spec.
func (h *Handler) serveCollection(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	q, ignored := parseQuery(r)
	baseURL := publicBaseURL(r)

	items := []Annotation{}
	if q != "" && h.Engine != nil {
		memberSet, mErr := h.loadCollectionMemberIDs(r.Context(), id)
		if mErr == nil && len(memberSet) > 0 {
			identity := auth.IdentityFromContext(r.Context())
			var callerRef *int64
			if identity != nil {
				ref := identity.UserRef
				callerRef = &ref
			}
			result, sErr := h.Engine.Run(r.Context(), search.Query{
				Text:          q,
				Types:         []search.HitType{search.HitTypeAsset},
				Limit:         50,
				CallerUserRef: callerRef,
			})
			if sErr == nil {
				for i, hit := range result.Hits {
					if _, ok := memberSet[hit.ID]; !ok {
						continue
					}
					items = append(items, Annotation{
						ID:         collectionSearchID(baseURL, id) + "/annotation/" + itoa(i+1),
						Type:       "Annotation",
						Motivation: "supplementing",
						Body: TextualBody{
							Type:        "TextualBody",
							Value:       hit.Title,
							Format:      "text/plain",
							Language:    "en",
							Granularity: "manifest",
						},
						Target: assetManifestID(baseURL, hit.ID),
					})
				}
			}
		}
	}

	h.record("collection", len(items), time.Since(start))
	page := AnnotationPage{
		Context: Context,
		ID:      collectionSearchID(baseURL, id) + "?q=" + q,
		Type:    "AnnotationPage",
		Items:   items,
		Ignored: ignored,
	}
	if len(items) > 0 {
		page.PartOf = []PartOfRef{{
			ID:    collectionSearchID(baseURL, id),
			Type:  "SearchService2",
			Label: en("Search within this collection"),
		}}
	}
	writeSearchJSON(w, page)
}

// loadCollectionMemberIDs returns the set of asset IDs pinned to
// the collection. Matches the presentation.Loader query so both
// use the same canonical member set (pinned, unexpired).
func (h *Handler) loadCollectionMemberIDs(ctx context.Context, collectionID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	rows, err := h.Pool.Query(ctx, `
		SELECT asset_id
		  FROM collection_resources
		 WHERE collection_id = $1
		   AND pinned = TRUE
		   AND (expires_at IS NULL OR expires_at > NOW())
	`, collectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]struct{}, 32)
	for rows.Next() {
		var a uuid.UUID
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = struct{}{}
	}
	return out, rows.Err()
}

// parseQuery pulls q + returns the list of ignored spec-defined
// parameters so the response can surface them.
func parseQuery(r *http.Request) (query string, ignored []string) {
	query = strings.TrimSpace(r.URL.Query().Get("q"))
	// motivation / user / date / box are the other spec-defined
	// params. We don't implement filtering by them yet; report as
	// ignored per spec.
	for _, k := range []string{"motivation", "user", "date", "box"} {
		if r.URL.Query().Get(k) != "" {
			ignored = append(ignored, k)
		}
	}
	return query, ignored
}

func (h *Handler) record(scope string, hits int, latency time.Duration) {
	if h.Counter != nil {
		h.Counter.RecordContentSearch(scope, hits, latency)
	}
}

// URL builders. baseURL is the per-request public origin
// (publicBaseURL(r)); NOT stored on the Handler so tests + prod
// requests both get URLs consistent with the request that came in.
func assetSearchID(baseURL string, id uuid.UUID) string {
	return strings.TrimRight(baseURL, "/") + "/iiif/3/asset/" + id.String() + "/search"
}
func collectionSearchID(baseURL string, id uuid.UUID) string {
	return strings.TrimRight(baseURL, "/") + "/iiif/3/collection/" + id.String() + "/search"
}
func assetCanvasID(baseURL string, id uuid.UUID) string {
	return strings.TrimRight(baseURL, "/") + "/iiif/3/asset/" + id.String() + "/manifest.json"
}
func assetManifestID(baseURL string, id uuid.UUID) string {
	return strings.TrimRight(baseURL, "/") + "/iiif/3/asset/" + id.String() + "/manifest.json"
}

// publicBaseURL — mirror of iiif/http.go:239 + iiif/presentation.
// PublicBaseURL. Local unexported copy so the two sub-packages
// share no compile-time surface but derive URLs identically.
func publicBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = strings.SplitN(v, ",", 2)[0]
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = strings.SplitN(v, ",", 2)[0]
	}
	return scheme + "://" + host
}

func writeSearchJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json;profile=\"http://iiif.io/api/search/2/context.json\"")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// itoa avoids strconv for a single call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+(n%10))) + digits
		n /= 10
	}
	if neg {
		return "-" + digits
	}
	return digits
}
