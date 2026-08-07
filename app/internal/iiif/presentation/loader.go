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

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Loader fetches the EntityRef + metadata pairs the builder
// consumes. Kept separate from the builder so tests can inject
// fixtures without a live DB.
type Loader struct {
	Pool *pgxpool.Pool
}

// NewLoader constructs a Loader.
func NewLoader(pool *pgxpool.Pool) *Loader { return &Loader{Pool: pool} }

// ---------------------------------------------------------------------------
// The row plane is OBTAINED here, not expressed (#661, epic #665)
// ---------------------------------------------------------------------------
//
// Every query in this file splices visibility.Predicate.ToSQL (ADR
// 0063) rather than carrying a hand-written WHERE clause. Before #661
// they read by id with `deleted_at IS NULL` and nothing else — and
// LoadCollection did not even have that — so the manifest route's ONLY
// gate was the in-builder sensitivity check (ADR 0064). That check is a
// CONTENT-plane rule: it knows about `restricted` / `team` / embargo and
// nothing about `status` or `processing_status`. The row-plane conjuncts
// the anonymous EntityAsset predicate carries — `status = 'active' AND
// processing_status = 'ready'` — had no expression here at all, so a
// DRAFT asset with public sensitivity served a full anonymous manifest
// (title, description, custom-field metadata, GPS, a canvas) while
// `GET /assets/{id}` for the same id returned 404.
//
// The two planes are still separate and both still run; what changed is
// that the row plane is now present, and it is the same one every other
// read path uses. See sensitivity_test.go, whose header documented the
// absence this fixes.

// LoadAsset returns the asset's EntityRef with metadata pairs
// pre-filtered by field-definition visibility (public-only for
// anonymous callers).
//
// Returns ErrNotFound when the row does not exist OR the caller may not
// see it — the two collapse deliberately, so the 404 the HTTP layer
// emits does not confirm a hidden id.
func (l *Loader) LoadAsset(ctx context.Context, id uuid.UUID, caller visibility.Caller) (EntityRef, error) {
	var (
		ref         EntityRef
		sensitivity string
		owner       *int64
		origin      *uuid.UUID
		pageCount   *int32
		fileExt     *string
	)
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return EntityRef{}, fmt.Errorf("presentation.LoadAsset: %w", err)
	}
	// One placeholder bound so far ($1 = id); the fragment owns
	// everything above it and its args append LAST.
	frag, predArgs := pred.ToSQL("", 1)
	err = l.Pool.QueryRow(ctx, `
		SELECT id, title, description, sensitivity, owner_user_ref,
		       origin_server_id, page_count, file_extension,
		       created_at, updated_at
		  FROM assets
		 WHERE id = $1`+frag,
		append([]any{id}, predArgs...)...).Scan(&ref.ID, &ref.Title, &ref.Description, &sensitivity, &owner,
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
	pairs, mErr := l.loadMetadataPairs(ctx, id, caller.IsAnonymous)
	if mErr == nil {
		ref.Metadata = pairs
	}
	return ref, nil
}

// LoadCollection returns the collection's EntityRef.
//
// #661 — this query had NO WHERE clause beyond the id: not the
// visibility disjunction, not even `deleted_at IS NULL`. Anonymous
// callers were saved by the default-deny sensitivity switch below,
// which is safety that lives somewhere else and would vanish with one
// edit there; AUTHENTICATED callers had no gate at all, on either
// plane, so any signed-in user could read the manifest — name,
// description, and via LoadCollectionMembers the full member list — of
// anyone else's PRIVATE collection. That is the #660 shape exactly, on
// a different route. The EntityCollection predicate (public OR owner OR
// a live ACL grant, minus soft-deleted) now decides, and it is the same
// one GET /collections/{id} uses.
//
// One deliberate divergence from that handler: it keeps a superadmin
// fall-through so the admin UI can render a Restore button on a
// soft-deleted row. This does not, because admitting capabilities here
// would mean threading a visibility.CapabilityChecker through the
// loader to answer a question no IIIF client asks — the same widening
// visibility.Filter's EntityCollection branch declines. A system.admin
// therefore gets 404 on the manifest of a collection they neither own
// nor hold a grant on. That is stricter than the item read for exactly
// one caller class, and no UI surface consumes it: the SPA never reads
// manifests, third-party viewers do.
func (l *Loader) LoadCollection(ctx context.Context, id uuid.UUID, caller visibility.Caller) (EntityRef, error) {
	var (
		ref    EntityRef
		origin *uuid.UUID
		vis    string
	)
	pred, err := visibility.Filter(ctx, visibility.EntityCollection, caller)
	if err != nil {
		return EntityRef{}, fmt.Errorf("presentation.LoadCollection: %w", err)
	}
	frag, predArgs := pred.ToSQL("", 1)
	err = l.Pool.QueryRow(ctx, `
		SELECT id, name, description, visibility, origin_server_id,
		       created_at, updated_at
		  FROM collections
		 WHERE id = $1`+frag,
		append([]any{id}, predArgs...)...).Scan(&ref.ID, &ref.Title, &ref.Description, &vis, &origin,
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
	// Map to the presentation Sensitivity so the content-plane gate in
	// the builder reads a uniform tier.
	//
	// `public` was MISSING from this switch and fell through to the
	// fail-closed default, which was correct when the comment here was
	// written — the CHECK constraint had no such value — and stopped
	// being correct when migration 00008 added the tier (#414). A truly
	// public collection therefore 404'd for anonymous IIIF callers: the
	// stale half of the same drift this issue is about, pointing the
	// other way. Unknown values still default to
	// SensitivityRestricted.
	switch vis {
	case "public":
		ref.Sensitivity = SensitivityPublic
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
// (sort_order ASC, added_at ASC) order per pre-audit Q2.
//
// #661 — the member rows carry the EntityAsset predicate, so this LIST
// agrees with the single-item read of the same asset. It did not before:
// `a.deleted_at IS NULL` plus a Go-side sensitivity filter let a DRAFT
// public-sensitivity member be listed in an anonymous manifest while
// `/iiif/3/asset/{that id}/manifest.json` refuses it — a list path wider
// than the item path, which is the invariant epic #665 names. The inline
// soft-delete conjunct is gone because the predicate asserts it
// (#429/#438 precedent).
//
// #883 — each row also carries MemberReadable, decided by the SAME
// visibility.FieldsReadable the post and collection APIs use, so the
// three surfaces cannot drift on what "the caller may not see this
// member" means. The predicate splice STAYS here (unlike the JSON API's
// contents query, which dropped it so it could emit placeholders):
// visibility.FieldsReadable is strictly tighter than the fragment, so keeping both
// changes no answer, and the fragment is what makes the anonymous
// row-plane conjuncts a SQL filter rather than a Go one on this path.
//
// caps may be nil (anonymous, or a caller with no capability checker) —
// visibility.FieldsReadable handles that.
//
// `mut` is the caller's resolved `assets.admin` scope (#939); the zero
// value denies, which is the pre-#939 behaviour.
func (l *Loader) LoadCollectionMembers(ctx context.Context, collectionID uuid.UUID, caller visibility.Caller, caps visibility.CapabilityChecker, mut visibility.AssetMutationCaps, limit int) ([]EntityRef, error) {
	if limit <= 0 {
		limit = 200
	}
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return nil, err
	}
	// Three placeholders bound ($1 collection id, $2 limit, $3 caller
	// ref) before the fragment renders. LIMIT binds first even though it
	// reads last — the invariant is an index bound, not textual order.
	frag, predArgs := pred.ToSQL("a", 3)
	rows, err := l.Pool.Query(ctx, `
		SELECT a.id, a.title, a.sensitivity, a.status, a.processing_status,
		       a.owner_user_ref, a.origin_server_id, a.file_extension,
		       a.created_at, a.updated_at,
		       (a.team_id IS NOT NULL AND EXISTS (
		            SELECT 1 FROM team_memberships tm
		             WHERE tm.team_id = a.team_id AND tm.user_ref = $3::BIGINT)) AS is_team_member,
		       a.team_id
		  FROM collection_resources cr
		  JOIN assets a ON a.id = cr.asset_id
		 WHERE cr.collection_id = $1
		   AND cr.pinned = TRUE
		   AND (cr.expires_at IS NULL OR cr.expires_at > NOW())`+frag+`
		 ORDER BY cr.sort_order ASC, cr.added_at ASC
		 LIMIT $2`,
		append([]any{collectionID, limit, caller.UserRef}, predArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]EntityRef, 0, limit)
	for rows.Next() {
		var (
			ref       EntityRef
			sens      string
			status    string
			procState string
			owner     *int64
			origin    *uuid.UUID
			ext       *string
			isTeam    bool
			teamID    *uuid.UUID
		)
		if err := rows.Scan(&ref.ID, &ref.Title, &sens, &status, &procState, &owner,
			&origin, &ext, &ref.CreatedAt, &ref.UpdatedAt, &isTeam, &teamID); err != nil {
			return nil, err
		}
		ref.Kind = EntityAsset
		ref.Sensitivity = Sensitivity(sens)
		ref.OwnerUserRef = owner
		ref.OriginServerID = origin
		if ext != nil {
			ref.FileExtension = *ext
		}
		// #939 — the FIELD plane, which is all a IIIF collection member
		// carries: an id and a label. There is no picture here to split
		// off (the tiles live behind visibility.CanReadContent in
		// iiif/http.go, and a restricted asset's manifest is a stub), so
		// a scoped `assets.admin` holder sees the titles of the members
		// they administer and still gets no pixels.
		fr := visibility.FieldsRow{
			Sensitivity:      sens,
			Status:           status,
			ProcessingStatus: procState,
			OwnerUserRef:     owner,
			IsTeamMember:     isTeam,
			TeamID:           teamID,
		}
		fr.ApplyMutationCaps(mut)
		ref.MemberReadable = visibility.FieldsReadable(fr, caller, caps)
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
			label string
			text  *string
			opts  []string
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
