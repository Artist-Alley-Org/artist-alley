// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package trash serves GET /account/trash — the caller's own
// soft-deleted assets, posts and collections as ONE mixed list (#937).
//
// WHY A PACKAGE OF ITS OWN, AND WHY ONE ENDPOINT RATHER THAN THREE.
//
// Self-service restore shipped in #936 and was unreachable: every list
// surface drops `include_deleted=true` for anyone below system.admin,
// so an owner could restore an item but had no way to learn its id. The
// fix could not be to widen that flag — those listings are not
// owner-scoped, so honouring it for an ordinary caller would turn each
// of them into a probe for deleted rows they cannot otherwise observe.
// The question this endpoint answers is a different, strictly narrower
// one: "what of MINE is in the bin".
//
// It is one endpoint because it is one PROJECTION — kind, id, title,
// deleted_at, and whether the caller may undo it. The three entities'
// delete SIDE EFFECTS differ substantially (assets unpin storage and
// fan out cache invalidation to posts + IIIF manifests; collections
// emit a federation Tombstone inside the delete transaction; posts do
// neither), and their RESTORE paths differ for the same reasons — which
// is why restore stays three endpoints and this change does not touch
// them. But none of that divergence is visible in five columns, and a
// page that had to merge three cursors client-side to show one
// chronological bin would be paying for a distinction it cannot render.
//
// The package is separate from assets/posts/collections because it
// belongs to none of them and a read-only projection across all three
// has no business importing any: it would make one domain package
// depend on the other two purely to select five columns.
package trash

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// maxPageLimit caps ?limit, matching the other account listings.
const maxPageLimit = 200

// defaultPageLimit matches the openapi default.
const defaultPageLimit = 50

// Handler serves the account trash listing.
//
// Sysconfig is optional: without it the rows still list, they just
// carry no purge_after. That is the honest degradation — "we cannot
// tell you when this expires" is a missing field, not a guess.
type Handler struct {
	Pool      *pgxpool.Pool
	Sysconfig *sysconfig.Store
	Logger    *slog.Logger
}

// NewHandler builds the trash handler.
func NewHandler(pool *pgxpool.Pool, sysCfg *sysconfig.Store, logger *slog.Logger) *Handler {
	return &Handler{Pool: pool, Sysconfig: sysCfg, Logger: logger}
}

// row is one raw union row. deletedBy never leaves this package — it
// feeds the predicate and is then discarded (see ListMyTrash).
type row struct {
	kind      string
	id        pgtype.UUID
	title     string
	deletedAt pgtype.Timestamptz
	deletedBy *int64
}

// keysetAndOrder renders the two halves of ONE (deleted_at, id)
// composite key: the "strictly past the cursor" predicate and the
// matching ORDER BY. Both come out of the same `cmp`/`dir` pair on
// purpose — that is the #868 discipline, and this surface is where it
// bites hardest.
//
// The id tiebreak carries the SAME comparison as the timestamp because
// it is the low-order digit of one key, not a second ordering. On a
// feed, rows sharing a timestamp are the rare case that hides the bug;
// here they are the NORMAL case — deleting a five-item selection writes
// five rows within the same statement, and two of the three sources
// (posts, collections) stamp deleted_at from the same transaction
// clock. A tiebreak pinned the wrong way would drop or repeat exactly
// those rows, which is to say most of them.
//
// tsN/idN are the placeholder numbers for the cursor timestamp and the
// cursor id. NULL timestamp means "first page" and admits every row.
func keysetAndOrder(tsN, idN int) (where, order string) {
	const cmp, dir = "<", "DESC"
	where = fmt.Sprintf(
		`($%d::TIMESTAMPTZ IS NULL
       OR deleted_at %s $%d::TIMESTAMPTZ
       OR (deleted_at = $%d::TIMESTAMPTZ AND id %s $%d::UUID))`,
		tsN, cmp, tsN, tsN, cmp, idN,
	)
	order = fmt.Sprintf("ORDER BY deleted_at %s, id %s", dir, dir)
	return where, order
}

// listPage reads one page of the caller's own soft-deleted rows.
//
// NO VISIBILITY GATE, DELIBERATELY — and this comment exists so the
// absence does not read as an oversight to the next person auditing
// read paths. Every other list surface in the codebase splices in
// visibility.Predicate or a read rule; this one cannot become a probe
// because the ownership conjunct IS the whole selection. `owner_user_ref
// = caller` (author_user_ref for posts) admits nothing the caller did
// not create, so there is no row here whose existence they could learn
// from the response. Adding the read rule on top would be strictly
// narrowing and would hide the caller's own private items from their
// own bin — the exact failure the rule is meant to prevent elsewhere.
//
// `assets.owner_user_ref` is nullable; `= $1` never matches NULL, so
// orphaned assets stay out on their own. The handler additionally
// refuses a zero caller ref before getting here, so a ref-0 row (none
// exist today; that is data, not a guarantee) cannot be claimed by an
// anonymous caller.
//
// One extra row is fetched beyond the limit — that is how the caller
// distinguishes "last page" from "exactly full page".
func (h *Handler) listPage(
	ctx context.Context,
	userRef int64,
	cursorTs pgtype.Timestamptz,
	cursorID pgtype.UUID,
	rowLimit int32,
) ([]row, error) {
	where, order := keysetAndOrder(2, 3)

	// Each branch applies the keyset itself rather than filtering the
	// union afterwards: the predicate is sargable against each table's
	// own (deleted_at) rows, and the union then only merges candidates.
	var b strings.Builder
	b.WriteString(`WITH page AS (
    SELECT 'asset'::TEXT AS kind, id, title, deleted_at, deleted_by_user_ref
      FROM assets
     WHERE owner_user_ref = $1::BIGINT AND deleted_at IS NOT NULL AND ` + where + `
  UNION ALL
    SELECT 'post'::TEXT, id, title, deleted_at, deleted_by_user_ref
      FROM posts
     WHERE author_user_ref = $1::BIGINT AND deleted_at IS NOT NULL AND ` + where + `
  UNION ALL
    SELECT 'collection'::TEXT, id, name, deleted_at, deleted_by_user_ref
      FROM collections
     WHERE owner_user_ref = $1::BIGINT AND deleted_at IS NOT NULL AND ` + where + `
)
SELECT kind, id, title, deleted_at, deleted_by_user_ref
FROM page
` + order + `
LIMIT $4::INTEGER`)

	rows, err := h.Pool.Query(ctx, b.String(), userRef, cursorTs, cursorID, rowLimit)
	if err != nil {
		return nil, fmt.Errorf("trash: list page: %w", err)
	}
	defer rows.Close()

	var out []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.id, &r.title, &r.deletedAt, &r.deletedBy); err != nil {
			return nil, fmt.Errorf("trash: list page scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trash: list page rows: %w", err)
	}
	return out, nil
}

// ListMyTrash implements GET /account/trash.
func (h *Handler) ListMyTrash(
	ctx context.Context,
	req openapi.ListMyTrashRequestObject,
) (openapi.ListMyTrashResponseObject, error) {
	caller := auth.IdentityFromContext(ctx)
	if caller == nil || caller.IsAnonymous() || caller.UserRef == 0 {
		return openapi.ListMyTrash401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}

	limit := int32(defaultPageLimit)
	if req.Params.Limit != nil {
		l := *req.Params.Limit
		if l < 1 {
			l = 1
		}
		if l > maxPageLimit {
			l = maxPageLimit
		}
		limit = int32(l)
	}

	var cursorTs pgtype.Timestamptz
	var cursorID pgtype.UUID
	if req.Params.Cursor != nil && *req.Params.Cursor != "" {
		ts, id, err := decodeCursor(*req.Params.Cursor)
		if err != nil {
			// A malformed cursor is the client's mistake, not ours.
			// The sibling listings answer 500 here; that predates the
			// BadRequest response and is not worth copying.
			return openapi.ListMyTrash400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "invalid cursor"},
			}, nil
		}
		cursorTs = pgtype.Timestamptz{Time: ts, Valid: true}
		cursorID = pgtype.UUID{Bytes: id, Valid: true}
	}

	rows, err := h.listPage(ctx, caller.UserRef, cursorTs, cursorID, limit+1)
	if err != nil {
		return nil, err
	}

	retention := h.retentionDays(ctx)

	items := make([]openapi.TrashItem, 0, limit)
	var lastDeletedAt time.Time
	var lastID uuid.UUID
	for i, r := range rows {
		if i >= int(limit) {
			break
		}
		item := openapi.TrashItem{
			Kind:      openapi.TrashItemKind(r.kind),
			Id:        openapi_types.UUID(r.id.Bytes),
			Title:     r.title,
			DeletedAt: r.deletedAt.Time,
			// The ONE rule, obtained rather than restated (#665). A
			// second copy here would be a list that can offer a
			// Restore button the restore endpoint then refuses.
			RestorableByCaller: auth.CanRestoreDeleted(caller, r.deletedBy),
		}
		if days, ok := retention[r.kind]; ok {
			t := r.deletedAt.Time.AddDate(0, 0, days)
			item.PurgeAfter = &t
		}
		items = append(items, item)
		lastDeletedAt = r.deletedAt.Time
		lastID = uuid.UUID(r.id.Bytes)
	}

	resp := openapi.TrashList{Items: items}
	if len(rows) > int(limit) {
		next := encodeCursor(lastDeletedAt, lastID)
		resp.NextCursor = &next
	}
	return openapi.ListMyTrash200JSONResponse(resp), nil
}

// retentionDays returns the per-kind hard-delete window, keyed by the
// same `kind` string the union emits.
//
// Best-effort by design: on a read failure it returns an empty map and
// every row simply ships without purge_after. "We could not read the
// retention window" must not fail the listing, and inventing a default
// here would be worse than the silence — it would state an expiry the
// GC does not honour.
func (h *Handler) retentionDays(ctx context.Context) map[string]int {
	if h.Sysconfig == nil {
		return nil
	}
	cfg, err := h.Sysconfig.GetSoftDelete(ctx)
	if err != nil {
		if h.Logger != nil {
			h.Logger.LogAttrs(ctx, slog.LevelWarn, "trash.retention.read_error",
				slog.String("err", err.Error()))
		}
		return nil
	}
	return map[string]int{
		"asset":      cfg.AssetRetentionDays,
		"post":       cfg.PostRetentionDays,
		"collection": cfg.CollectionRetentionDays,
	}
}

// encodeCursor / decodeCursor use the same opaque
// base64(RFC3339Nano|uuid) shape as the assets and posts listings, so a
// cursor is recognisable across surfaces and carries no more than the
// keyset it stands for.
func encodeCursor(t time.Time, id uuid.UUID) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, errors.New("bad cursor shape")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return t, id, nil
}
