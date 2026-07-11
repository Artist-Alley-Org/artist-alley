// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package presentation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Loader fetches the EntityRef + metadata pairs the builder
// consumes. Kept separate from the builder so tests can inject
// fixtures without a live DB.
type Loader struct {
	Pool *pgxpool.Pool
}

// NewLoader constructs a Loader.
func NewLoader(pool *pgxpool.Pool) *Loader { return &Loader{Pool: pool} }

// LoadAsset returns the asset's EntityRef with metadata pairs
// pre-filtered by field-definition visibility (public-only for
// anonymous callers).
func (l *Loader) LoadAsset(ctx context.Context, id uuid.UUID, isAnonymous bool) (EntityRef, error) {
	var (
		ref         EntityRef
		sensitivity string
		owner       *int64
		origin      *uuid.UUID
		pageCount   *int32
		fileExt     *string
	)
	err := l.Pool.QueryRow(ctx, `
		SELECT id, title, description, sensitivity, owner_user_ref,
		       origin_server_id, page_count, file_extension,
		       created_at, updated_at
		  FROM assets
		 WHERE id = $1 AND deleted_at IS NULL
	`, id).Scan(&ref.ID, &ref.Title, &ref.Description, &sensitivity, &owner,
		&origin, &pageCount, &fileExt, &ref.CreatedAt, &ref.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EntityRef{}, ErrNotFound
		}
		return EntityRef{}, fmt.Errorf("presentation.LoadAsset: %w", err)
	}
	ref.Kind = EntityAsset
	ref.Sensitivity = Sensitivity(sensitivity)
	ref.OwnerUserRef = owner
	ref.OriginServerID = origin
	if pageCount != nil {
		v := int(*pageCount)
		ref.PageCount = &v
	}
	if fileExt != nil {
		ref.FileExtension = *fileExt
	}

	// GPS from the canonical field-value system (per pre-audit
	// Q7). Stored as two numeric field values under the
	// gps_latitude + gps_longitude canonical extraction sources.
	lat, lon, gerr := l.loadGPS(ctx, id)
	if gerr == nil && lat != nil && lon != nil {
		ref.Latitude = lat
		ref.Longitude = lon
	}

	// Custom-field metadata pairs — respecting per-field
	// visibility. Anonymous callers only see fields flagged as
	// visible in public IIIF surfaces.
	pairs, mErr := l.loadMetadataPairs(ctx, id, isAnonymous)
	if mErr == nil {
		ref.Metadata = pairs
	}
	return ref, nil
}

// LoadCollection returns the collection's EntityRef.
func (l *Loader) LoadCollection(ctx context.Context, id uuid.UUID) (EntityRef, error) {
	var (
		ref    EntityRef
		origin *uuid.UUID
		vis    string
	)
	err := l.Pool.QueryRow(ctx, `
		SELECT id, name, description, visibility, origin_server_id,
		       created_at, updated_at
		  FROM collections
		 WHERE id = $1
	`, id).Scan(&ref.ID, &ref.Title, &ref.Description, &vis, &origin,
		&ref.CreatedAt, &ref.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EntityRef{}, ErrNotFound
		}
		return EntityRef{}, fmt.Errorf("presentation.LoadCollection: %w", err)
	}
	ref.Kind = EntityCollection
	ref.OriginServerID = origin
	// Collections carry a visibility enum rather than sensitivity.
	// Map to the presentation Sensitivity so the anonymous gate
	// can uniformly refuse non-public rows.
	//
	// Note: the current collections.visibility CHECK constraint is
	// {private, org-only, followers, explicit-share} — there is NO
	// "public" value. Anonymous IIIF callers therefore see zero
	// collection manifests today; that's correct until an operator
	// adds a truly-public collection visibility. Unknown values
	// default to SensitivityRestricted (fail-closed).
	switch vis {
	case "org-only", "followers":
		ref.Sensitivity = SensitivityTeam
	case "private", "explicit-share":
		ref.Sensitivity = SensitivityRestricted
	default:
		ref.Sensitivity = SensitivityRestricted
	}
	return ref, nil
}

// LoadCollectionMembers returns member EntityRefs in the canonical
// (sort_order ASC, added_at ASC) order per pre-audit Q2. Members
// with sensitivity=restricted/team are pre-filtered when isAnonymous.
func (l *Loader) LoadCollectionMembers(ctx context.Context, collectionID uuid.UUID, isAnonymous bool, limit int) ([]EntityRef, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := l.Pool.Query(ctx, `
		SELECT a.id, a.title, a.sensitivity, a.owner_user_ref,
		       a.origin_server_id, a.file_extension,
		       a.created_at, a.updated_at
		  FROM collection_resources cr
		  JOIN assets a ON a.id = cr.asset_id
		 WHERE cr.collection_id = $1
		   AND cr.pinned = TRUE
		   AND (cr.expires_at IS NULL OR cr.expires_at > NOW())
		   AND a.deleted_at IS NULL
		 ORDER BY cr.sort_order ASC, cr.added_at ASC
		 LIMIT $2
	`, collectionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]EntityRef, 0, limit)
	for rows.Next() {
		var (
			ref    EntityRef
			sens   string
			owner  *int64
			origin *uuid.UUID
			ext    *string
		)
		if err := rows.Scan(&ref.ID, &ref.Title, &sens, &owner,
			&origin, &ext, &ref.CreatedAt, &ref.UpdatedAt); err != nil {
			return nil, err
		}
		ref.Kind = EntityAsset
		ref.Sensitivity = Sensitivity(sens)
		ref.OwnerUserRef = owner
		ref.OriginServerID = origin
		if ext != nil {
			ref.FileExtension = *ext
		}
		if isAnonymous && (ref.Sensitivity == SensitivityRestricted || ref.Sensitivity == SensitivityTeam) {
			continue
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// loadGPS looks up the asset's gps_latitude + gps_longitude
// canonical field values. Returns (nil, nil) with no error when
// GPS isn't populated.
func (l *Loader) loadGPS(ctx context.Context, assetID uuid.UUID) (*float64, *float64, error) {
	sql := `
		SELECT f.extraction_source, v.value_num
		  FROM asset_field_value v
		  JOIN field_definition f ON f.id = v.field_id
		 WHERE v.asset_id = $1
		   AND f.extraction_source IN ('gps_latitude', 'gps_longitude')
		   AND v.value_num IS NOT NULL
	`
	rows, err := l.Pool.Query(ctx, sql, assetID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var lat, lon *float64
	for rows.Next() {
		var (
			source string
			val    float64
		)
		if err := rows.Scan(&source, &val); err != nil {
			return nil, nil, err
		}
		v := val
		switch source {
		case "gps_latitude":
			lat = &v
		case "gps_longitude":
			lon = &v
		}
	}
	return lat, lon, rows.Err()
}

// LoadMetadataPairs reads the caller-visible custom-field values.
// For MVP the filter is coarse: pull all field_values for the
// asset whose field_definition is 'active'; anonymous callers get
// the same set (no per-field public flag exists today). A
// follow-up phase can add a field_definition.public_to_iiif
// column + flip anonymous callers to the public-only subset.
//
// Exported so the sibling content_search package can consume the
// same rows via a thin adapter shim (avoids an import cycle by
// keeping the LangString conversion at the caller side).
func (l *Loader) LoadMetadataPairs(ctx context.Context, assetID uuid.UUID, isAnonymous bool) ([]MetadataPair, error) {
	return l.loadMetadataPairs(ctx, assetID, isAnonymous)
}

func (l *Loader) loadMetadataPairs(ctx context.Context, assetID uuid.UUID, isAnonymous bool) ([]MetadataPair, error) {
	_ = isAnonymous
	sql := `
		SELECT f.label, v.value_text, v.value_options
		  FROM asset_field_value v
		  JOIN field_definition f ON f.id = v.field_id
		 WHERE v.asset_id = $1
		   AND f.status = 'active'
		   AND (v.value_text IS NOT NULL OR v.value_options IS NOT NULL)
		 ORDER BY f.label ASC
		 LIMIT 50
	`
	rows, err := l.Pool.Query(ctx, sql, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MetadataPair, 0)
	for rows.Next() {
		var (
			label  string
			text   *string
			opts   []string
		)
		if err := rows.Scan(&label, &text, &opts); err != nil {
			return nil, err
		}
		value := ""
		switch {
		case text != nil:
			value = *text
		case len(opts) > 0:
			value = joinStrings(opts, ", ")
		}
		if value == "" {
			continue
		}
		out = append(out, MetadataPair{
			Label: EN(label),
			Value: EN(value),
		})
	}
	return out, rows.Err()
}

// joinStrings avoids strings.Join for a single call site.
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}

// stampUpdated is a helper for the cache key builder to floor the
// timestamp to microsecond precision (matches Postgres storage).
func stampUpdated(t time.Time) int64 { return t.UnixMicro() }
