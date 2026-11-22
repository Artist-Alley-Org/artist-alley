// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package receivedassets

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// The operator's view (#1574 slice 2), gated on system.admin:
//
//   - ListFederationTransferEvents reads the transfer and fixity event
//     ledger (federation_transfer_events, migration 00089) alone, so a row
//     still answers after its record or peer is gone; record_exists and
//     peer_exists say whether they still are.
//   - ListAdminReceivedAssets lists received records in every state and
//     tier, team included, without bytes and without any grant check: it
//     is what the receiver holds and why, not a view of the assets.

// adminDenied answers 401 for an anonymous caller and 403 for a caller
// without system.admin.
func adminDenied(ctx context.Context) (unauth, forbidden bool) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil || caller.UserRef <= 0 {
		return true, false
	}
	return false, !caller.Can("system.admin")
}

const (
	adminDefaultLimit = 100
	adminMaxLimit     = 500
)

// oneOf reports whether v is nil or one of the allowed values.
func oneOf(v *string, allowed ...string) bool {
	if v == nil {
		return true
	}
	for _, a := range allowed {
		if *v == a {
			return true
		}
	}
	return false
}

func adminLimit(p *int) (int, bool) {
	if p == nil {
		return adminDefaultLimit, true
	}
	if *p < 1 || *p > adminMaxLimit {
		return 0, false
	}
	return *p, true
}

// ListFederationTransferEvents lists ledger rows, newest first.
func (h *Handler) ListFederationTransferEvents(ctx context.Context, req openapi.ListFederationTransferEventsRequestObject) (openapi.ListFederationTransferEventsResponseObject, error) {
	if unauth, forbidden := adminDenied(ctx); unauth {
		return openapi.ListFederationTransferEvents401JSONResponse{UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"}}, nil
	} else if forbidden {
		return openapi.ListFederationTransferEvents403JSONResponse{ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"}}, nil
	}
	p := req.Params
	limit, ok := adminLimit(p.Limit)
	if !ok {
		return openapi.ListFederationTransferEvents400JSONResponse{BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "limit must be 1 to 500"}}, nil
	}
	if !oneOf(p.EventType, "transfer", "fixity-check", "ingestion") || !oneOf(p.Outcome, "success", "failure", "warning") {
		return openapi.ListFederationTransferEvents400JSONResponse{BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "unknown event_type or outcome"}}, nil
	}
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if p.PeerId != nil {
		add("e.peer_id = ?", uuid.UUID(*p.PeerId))
	}
	if p.ReceivedAssetId != nil {
		add("e.received_asset_id = ?", uuid.UUID(*p.ReceivedAssetId))
	}
	if p.EventType != nil {
		add("e.event_type = ?", *p.EventType)
	}
	if p.Outcome != nil {
		add("e.outcome = ?", *p.Outcome)
	}
	if p.Reason != nil {
		add("e.reason = ?", *p.Reason)
	}
	if p.Since != nil {
		add("e.occurred_at >= ?", *p.Since)
	}
	if p.Cursor != nil {
		t, id, err := decodeCursor(*p.Cursor)
		if err != nil {
			return openapi.ListFederationTransferEvents400JSONResponse{BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid cursor"}}, nil
		}
		args = append(args, t, id)
		where = append(where, fmt.Sprintf("(e.occurred_at, e.id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	q := `SELECT e.id, e.occurred_at, e.peer_id, e.peer_instance_url, e.peer_display_name, e.received_asset_id,
	        e.activity_uri, e.origin_asset_uri, e.version_uri, e.version_number, e.sensitivity,
	        e.file_position, e.file_role, e.object_hash, e.declared_media_type,
	        e.event_type, e.outcome, e.reason, e.expected_sha256, e.expected_size_bytes,
	        e.observed_basis, e.observed_sha256, e.observed_size_bytes, e.observed_truncated,
	        e.mismatch_algorithm, e.mismatch_expected, e.mismatch_observed,
	        e.attempt, e.agent_name, e.agent_version, e.record_state_after, e.detail,
	        EXISTS (SELECT 1 FROM federation_received_assets r WHERE r.id = e.received_asset_id),
	        EXISTS (SELECT 1 FROM federation_peers rp WHERE rp.id = e.peer_id)
	   FROM federation_transfer_events e`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit+1)
	q += fmt.Sprintf(" ORDER BY e.occurred_at DESC, e.id DESC LIMIT $%d", len(args))
	rows, err := h.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("receivedassets: transfer events: %w", err)
	}
	defer rows.Close()
	out := openapi.ListFederationTransferEvents200JSONResponse{Items: []openapi.FederationTransferEvent{}}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("receivedassets: transfer events: %w", err)
		}
		out.Items = append(out.Items, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("receivedassets: transfer events: %w", err)
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		last := out.Items[limit-1]
		c := encodeCursor(last.OccurredAt, uuid.UUID(last.Id))
		out.NextCursor = &c
	}
	return out, nil
}

func scanEvent(rows pgx.Rows) (openapi.FederationTransferEvent, error) {
	var e openapi.FederationTransferEvent
	var id, peerID, recID uuid.UUID
	var pos *int32
	var att int32
	err := rows.Scan(&id, &e.OccurredAt, &peerID, &e.PeerInstanceUrl, &e.PeerDisplayName, &recID,
		&e.ActivityUri, &e.OriginAssetUri, &e.VersionUri, &e.VersionNumber, &e.Sensitivity,
		&pos, &e.FileRole, &e.ObjectHash, &e.DeclaredMediaType,
		&e.EventType, &e.Outcome, &e.Reason, &e.ExpectedSha256, &e.ExpectedSizeBytes,
		&e.ObservedBasis, &e.ObservedSha256, &e.ObservedSizeBytes, &e.ObservedTruncated,
		&e.MismatchAlgorithm, &e.MismatchExpected, &e.MismatchObserved,
		&att, &e.AgentName, &e.AgentVersion, &e.RecordStateAfter, &e.Detail,
		&e.RecordExists, &e.PeerExists)
	if err != nil {
		return e, err
	}
	e.Id, e.PeerId, e.ReceivedAssetId = id, peerID, recID
	if pos != nil {
		v := int(*pos)
		e.FilePosition = &v
	}
	e.Attempt = int(att)
	return e, nil
}

// ListAdminReceivedAssets lists received records, newest first.
func (h *Handler) ListAdminReceivedAssets(ctx context.Context, req openapi.ListAdminReceivedAssetsRequestObject) (openapi.ListAdminReceivedAssetsResponseObject, error) {
	if unauth, forbidden := adminDenied(ctx); unauth {
		return openapi.ListAdminReceivedAssets401JSONResponse{UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"}}, nil
	} else if forbidden {
		return openapi.ListAdminReceivedAssets403JSONResponse{ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin required"}}, nil
	}
	p := req.Params
	limit, ok := adminLimit(p.Limit)
	if !ok {
		return openapi.ListAdminReceivedAssets400JSONResponse{BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "limit must be 1 to 500"}}, nil
	}
	if !oneOf(p.State, "pending", "complete", "rejected", "withdrawn") {
		return openapi.ListAdminReceivedAssets400JSONResponse{BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "unknown state"}}, nil
	}
	var where []string
	var args []any
	if p.State != nil {
		args = append(args, *p.State)
		where = append(where, fmt.Sprintf("r.state = $%d", len(args)))
	}
	if p.PeerId != nil {
		args = append(args, uuid.UUID(*p.PeerId))
		where = append(where, fmt.Sprintf("r.peer_id = $%d", len(args)))
	}
	if p.Cursor != nil {
		t, id, err := decodeCursor(*p.Cursor)
		if err != nil {
			return openapi.ListAdminReceivedAssets400JSONResponse{BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid cursor"}}, nil
		}
		args = append(args, t, id)
		where = append(where, fmt.Sprintf("(r.received_at, r.id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	q := `SELECT r.id, r.peer_id, p.instance_url, p.display_name, r.activity_uri, r.origin_asset_uri, r.version_uri,
	        r.version_number, r.sensitivity, r.state, r.reason, r.attempts, r.received_at, r.completed_at,
	        (SELECT count(*) FROM federation_received_asset_files f WHERE f.received_asset_id = r.id),
	        (SELECT count(*) FROM federation_received_asset_files f WHERE f.received_asset_id = r.id AND f.role = 'original'),
	        (SELECT COALESCE(sum(f.size_bytes), 0) FROM federation_received_asset_files f WHERE f.received_asset_id = r.id AND f.role = 'original')
	   FROM federation_received_assets r JOIN federation_peers p ON p.id = r.peer_id`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit+1)
	q += fmt.Sprintf(" ORDER BY r.received_at DESC, r.id DESC LIMIT $%d", len(args))
	rows, err := h.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("receivedassets: admin records: %w", err)
	}
	defer rows.Close()
	out := openapi.ListAdminReceivedAssets200JSONResponse{Items: []openapi.AdminReceivedAsset{}}
	for rows.Next() {
		var a openapi.AdminReceivedAsset
		var id, peerID uuid.UUID
		var attempts int32
		var files, originals int64
		var completed *time.Time
		if err := rows.Scan(&id, &peerID, &a.PeerUrl, &a.PeerName, &a.ActivityUri, &a.OriginAssetUri, &a.VersionUri,
			&a.VersionNumber, &a.Sensitivity, &a.State, &a.Reason, &attempts, &a.ReceivedAt, &completed,
			&files, &originals, &a.OriginalBytes); err != nil {
			return nil, fmt.Errorf("receivedassets: admin records: %w", err)
		}
		a.Id, a.PeerId, a.Attempts = id, peerID, int(attempts)
		a.CompletedAt, a.FileCount, a.OriginalCount = completed, int(files), int(originals)
		out.Items = append(out.Items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("receivedassets: admin records: %w", err)
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		last := out.Items[limit-1]
		c := encodeCursor(last.ReceivedAt, uuid.UUID(last.Id))
		out.NextCursor = &c
	}
	return out, nil
}
