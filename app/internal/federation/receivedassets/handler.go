// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package receivedassets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// Handler serves /federation/received-assets (#1574). Every answer is
// decided by the rules in rule.go, evaluated in the query that reads the
// record, on every request.
type Handler struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
}

// callerRef is the signed-in caller's user ref, or 0 for an anonymous or
// public-mode request.
func callerRef(ctx context.Context) int64 {
	if id := auth.IdentityFromContext(ctx); id != nil && id.UserRef > 0 {
		return id.UserRef
	}
	return 0
}

func notFound() openapi.NotFoundJSONResponse {
	return openapi.NotFoundJSONResponse{Error: "received asset not found"}
}

// declaredKeys are the aa:asset claims reported as declared.
var declaredKeys = []string{"rights", "requiredStatement", "identifiers", "accessionNumber", "description", "aiProvenance", "mature"}

const summaryCols = `r.id, r.peer_id, p.instance_url, p.display_name, r.origin_asset_uri, r.version_uri, r.version_number,
	r.sensitivity, r.received_at, r.completed_at,
	f.position, f.role, f.object_hash, f.size_bytes, f.declared_media_type, f.declared_original_filename,
	f.detected_media_type, f.detected_by, f.state`

// summaryFrom joins each record to its peer and its first original.
const summaryFrom = `FROM federation_received_assets r
	JOIN federation_peers p ON p.id = r.peer_id
	JOIN LATERAL (SELECT * FROM federation_received_asset_files
	               WHERE received_asset_id = r.id AND role = 'original' ORDER BY position LIMIT 1) f ON TRUE`

func scanSummary(row pgx.Row, extra ...any) (openapi.ReceivedAsset, error) {
	var a openapi.ReceivedAsset
	var id, peer uuid.UUID
	var name *string
	var completed *time.Time
	var f openapi.ReceivedAssetFile
	var role, state string
	err := row.Scan(append([]any{&id, &peer, &a.PeerUrl, &name, &a.OriginAssetUri, &a.VersionUri, &a.VersionNumber,
		&a.Sensitivity, &a.ReceivedAt, &completed,
		&f.Position, &role, &f.Sha256, &f.SizeBytes, &f.DeclaredMediaType, &f.DeclaredOriginalFilename,
		&f.DetectedMediaType, &f.DetectedBy, &state}, extra...)...)
	if err != nil {
		return a, err
	}
	a.Id, a.PeerId = openapi_types.UUID(id), openapi_types.UUID(peer)
	if name != nil && *name != "" {
		a.PeerName = name
	}
	if completed != nil {
		a.CompletedAt = *completed
	}
	f.Role, f.State = openapi.ReceivedAssetFileRole(role), openapi.ReceivedAssetFileState(state)
	a.Original = f
	return a, nil
}

// cursor is (completed_at, id) of the last item, base64url.
func encodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 10) + "." + id.String()))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	ts, idStr, ok := strings.Cut(string(b), ".")
	if !ok {
		return time.Time{}, uuid.Nil, errors.New("malformed cursor")
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return time.Unix(0, n).UTC(), id, nil
}

// ListReceivedAssets lists the received records the caller may view,
// newest first.
func (h *Handler) ListReceivedAssets(ctx context.Context, req openapi.ListReceivedAssetsRequestObject) (openapi.ListReceivedAssetsResponseObject, error) {
	caller := callerRef(ctx)
	if caller <= 0 {
		return openapi.ListReceivedAssets401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	limit := 50
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if limit < 1 || limit > 200 {
		return openapi.ListReceivedAssets400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "limit must be 1 to 200"},
		}, nil
	}
	var after *time.Time
	afterID := uuid.Nil
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		t, id, err := decodeCursor(*req.Params.Cursor)
		if err != nil {
			return openapi.ListReceivedAssets400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		after, afterID = &t, id
	}
	rows, err := h.Pool.Query(ctx, `SELECT `+summaryCols+` `+summaryFrom+`
		WHERE `+ViewableSQL("r", "$1")+`
		  AND ($2::timestamptz IS NULL OR (r.completed_at, r.id) < ($2::timestamptz, $3::uuid))
		ORDER BY r.completed_at DESC, r.id DESC
		LIMIT $4`, caller, after, afterID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("receivedassets: list: %w", err)
	}
	defer rows.Close()
	out := openapi.ReceivedAssetList{Items: []openapi.ReceivedAsset{}}
	for rows.Next() {
		a, err := scanSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("receivedassets: list: %w", err)
		}
		out.Items = append(out.Items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("receivedassets: list: %w", err)
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		last := out.Items[limit-1]
		c := encodeCursor(last.CompletedAt, uuid.UUID(last.Id))
		out.NextCursor = &c
	}
	return openapi.ListReceivedAssets200JSONResponse{Body: out,
		Headers: openapi.ListReceivedAssets200ResponseHeaders{CacheControl: noStore()}}, nil
}

// GetReceivedAsset returns one record the caller may view.
func (h *Handler) GetReceivedAsset(ctx context.Context, req openapi.GetReceivedAssetRequestObject) (openapi.GetReceivedAssetResponseObject, error) {
	caller := callerRef(ctx)
	if caller <= 0 {
		return openapi.GetReceivedAsset404JSONResponse{NotFoundJSONResponse: notFound()}, nil
	}
	var d openapi.ReceivedAssetDetail
	var record []byte
	a, err := scanSummary(h.Pool.QueryRow(ctx, `SELECT `+summaryCols+`, r.activity_uri, r.actor_uri, r.record,
		       `+OriginalReadableSQL("r", "$1")+`
		  `+summaryFrom+`
		 WHERE r.id = $2 AND `+ViewableSQL("r", "$1"), caller, uuid.UUID(req.Id)),
		&d.ActivityUri, &d.ActorUri, &record, &d.OriginalAvailable)
	if errors.Is(err, pgx.ErrNoRows) {
		w, ok, werr := h.withdrawal(ctx, caller, uuid.UUID(req.Id))
		if werr != nil {
			return nil, werr
		}
		if ok {
			return openapi.GetReceivedAsset410JSONResponse(w), nil
		}
		return openapi.GetReceivedAsset404JSONResponse{NotFoundJSONResponse: notFound()}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("receivedassets: get: %w", err)
	}
	d.Asset = a
	rows, err := h.Pool.Query(ctx, `SELECT position, role, object_hash, size_bytes, declared_media_type,
		    declared_original_filename, detected_media_type, detected_by, state
		  FROM federation_received_asset_files WHERE received_asset_id = $1 ORDER BY position`, uuid.UUID(req.Id))
	if err != nil {
		return nil, fmt.Errorf("receivedassets: files: %w", err)
	}
	defer rows.Close()
	d.Files = []openapi.ReceivedAssetFile{}
	for rows.Next() {
		var f openapi.ReceivedAssetFile
		var role, state string
		if err := rows.Scan(&f.Position, &role, &f.Sha256, &f.SizeBytes, &f.DeclaredMediaType, &f.DeclaredOriginalFilename,
			&f.DetectedMediaType, &f.DetectedBy, &state); err != nil {
			return nil, fmt.Errorf("receivedassets: files: %w", err)
		}
		f.Role, f.State = openapi.ReceivedAssetFileRole(role), openapi.ReceivedAssetFileState(state)
		d.Files = append(d.Files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("receivedassets: files: %w", err)
	}
	// The sender's descriptive claims, as received.
	var claims map[string]any
	if err := json.Unmarshal(record, &claims); err != nil {
		return nil, fmt.Errorf("receivedassets: record: %w", err)
	}
	d.Declared = map[string]interface{}{}
	for _, k := range declaredKeys {
		if v, ok := claims[k]; ok {
			d.Declared[k] = v
		}
	}
	if err := h.overlayFlags(ctx, uuid.UUID(a.PeerId), a.OriginAssetUri, d.Declared); err != nil {
		return nil, err
	}
	return openapi.GetReceivedAsset200JSONResponse{Body: d,
		Headers: openapi.GetReceivedAsset200ResponseHeaders{CacheControl: noStore()}}, nil
}

// overlayFlags replaces the declared mature and aiProvenance with those of
// the asset's newest revision from its peer (#1575, spec v1 §17.2.8): the
// greatest by the carrying activity's signed published, then its URI. A
// record from before revisions were kept has none, and keeps its own
// claims. Display only: nothing here decides who sees the record.
func (h *Handler) overlayFlags(ctx context.Context, peerID uuid.UUID, assetURI string, declared map[string]interface{}) error {
	var mature bool
	var ai *string
	err := h.Pool.QueryRow(ctx, `SELECT mature, ai_provenance FROM federation_received_asset_revisions
		WHERE peer_id = $1 AND origin_asset_uri = $2
		ORDER BY published DESC, activity_uri DESC LIMIT 1`, peerID, assetURI).Scan(&mature, &ai)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("receivedassets: flags: %w", err)
	}
	declared["mature"] = mature
	if ai != nil {
		declared["aiProvenance"] = *ai
	} else {
		delete(declared, "aiProvenance")
	}
	return nil
}

// DownloadReceivedAssetOriginal streams the record's first verified
// original, under the original-file rule.
func (h *Handler) DownloadReceivedAssetOriginal(ctx context.Context, req openapi.DownloadReceivedAssetOriginalRequestObject) (openapi.DownloadReceivedAssetOriginalResponseObject, error) {
	caller := callerRef(ctx)
	if caller <= 0 {
		return openapi.DownloadReceivedAssetOriginal404JSONResponse{NotFoundJSONResponse: notFound()}, nil
	}
	var hash string
	err := h.Pool.QueryRow(ctx, `SELECT f.object_hash
		  FROM federation_received_assets r
		  JOIN federation_received_asset_files f ON f.received_asset_id = r.id AND f.role = 'original' AND f.state = 'verified'
		 WHERE r.id = $2 AND `+OriginalReadableSQL("r", "$1")+`
		 ORDER BY f.position LIMIT 1`, caller, uuid.UUID(req.Id)).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		w, ok, werr := h.withdrawal(ctx, caller, uuid.UUID(req.Id))
		if werr != nil {
			return nil, werr
		}
		if ok {
			return openapi.DownloadReceivedAssetOriginal410JSONResponse(w), nil
		}
		return openapi.DownloadReceivedAssetOriginal404JSONResponse{NotFoundJSONResponse: notFound()}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("receivedassets: original: %w", err)
	}
	body, info, err := h.Storage.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return openapi.DownloadReceivedAssetOriginal404JSONResponse{NotFoundJSONResponse: notFound()}, nil
		}
		return nil, fmt.Errorf("receivedassets: original: %w", err)
	}
	return openapi.DownloadReceivedAssetOriginal200ApplicationoctetStreamResponse{Body: body, ContentLength: info.Size,
		Headers: openapi.DownloadReceivedAssetOriginal200ResponseHeaders{CacheControl: noStore()}}, nil
}

// withdrawal is record id's withdrawal notice when caller may see it
// (WithdrawnVisibleSQL, #1575), with the origin's tombstone values; ok is
// false for every other record, which the caller then gets as 404.
func (h *Handler) withdrawal(ctx context.Context, caller int64, id uuid.UUID) (openapi.ReceivedAssetWithdrawal, bool, error) {
	var w openapi.ReceivedAssetWithdrawal
	err := h.Pool.QueryRow(ctx, `SELECT r.id, r.origin_asset_uri, r.withdrawn_at, t.deleted, t.reason, t.last_version
		  FROM federation_received_assets r
		  JOIN federation_received_tombstones t ON t.peer_id = r.peer_id AND t.origin_asset_uri = r.origin_asset_uri
		 WHERE r.id = $2 AND `+WithdrawnVisibleSQL("r", "$1"), caller, id).
		Scan(&w.Id, &w.OriginAssetUri, &w.WithdrawnAt, &w.Deleted, &w.Reason, &w.LastVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, false, nil
	}
	if err != nil {
		return w, false, fmt.Errorf("receivedassets: withdrawal: %w", err)
	}
	return w, true, nil
}

// noStore is the Cache-Control the 200 responses declare (the OpenAPI
// header); [NoStore] sets the same on every answer of these routes.
func noStore() *string {
	s := "no-store"
	return &s
}

// PathPrefix is where the received-asset routes are served.
const PathPrefix = "/api/v1/federation/received-assets"

// NoStore sets Cache-Control: no-store on EVERY answer under
// [PathPrefix], before any handler runs: the 200s, this package's 400,
// 401 and 404, and the generated parameter binder's 400 for a malformed
// id. Each answer is decided on its request. A cached refusal would hide
// a later regrant, and a cached positive answer would show a record
// after its grant ended. no-store keeps HTTP caches from keeping a copy;
// it cannot recall bytes a client already received.
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p == PathPrefix || strings.HasPrefix(p, PathPrefix+"/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
