// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The operator promo band (#1118) — the domain half.
//
// A band is a full-width strip the operator authors (title, blurb, a
// call-to-action, and an ordered row of curated cards) which the browse
// feed renders BETWEEN pages. Its cards are ordinary `featured_items`
// rows carrying the band's id, so every one of them resolves through
// [ListPlacements] — the same query, the same predicate splices, the
// same sensitivity and mature gates as the featured rail. See migration
// 00053 for why there is no second membership table.
//
// # Which collapse rule governs
//
// ADR 0079 §2 says an unfilled slot is REPLACED BY ORDINARY FEED CONTENT
// rather than collapsed, and that rule does not apply here. It is scoped
// to in-grid sized slots — a 2x2 cell inside the masonry, where
// collapsing leaves a hole in the middle of a wall. A band is a
// full-width strip BETWEEN two walls, which is ADR 0030's banner
// geometry, and ADR 0030's rule is the one 0079 explicitly left in
// place for that kind: collapse entirely.
//
// So: a band with no title, no cards, or no cards THIS READER may see
// renders nothing at all, and the decision is made HERE rather than in
// the client. [RenderableBand] returns nil in every one of those cases,
// so the wire carries no band object and a client cannot get the
// collapse wrong by forgetting a length check.

package featured

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ErrNoBand is returned by the admin band reads and by Remove when the
// install has no band. Mapped to HTTP 404.
var ErrNoBand = errors.New("featured: no promo band")

// ErrBadCTA is returned when the call-to-action pair is unusable.
// Mapped to HTTP 400.
var ErrBadCTA = errors.New("featured: cta label and url must be supplied together, and the url must be an absolute http(s) url or a site-relative path")

// ErrBadAfterPage is returned for a feed position below 1. Mapped to
// HTTP 400.
var ErrBadAfterPage = errors.New("featured: after_page must be at least 1")

// ErrBandScopeNotWritable is returned for an audience the band does not
// model. Mapped to HTTP 400.
var ErrBandScopeNotWritable = errors.New("featured: band scope must be org or public")

// BandInput is the whole band definition. Every field is written on
// every save — see UpdatePromoBand for why this is not a PATCH.
type BandInput struct {
	Title     string
	Blurb     string
	CTALabel  string
	CTAURL    string
	Enabled   bool
	AfterPage int32
	Scope     string
	CreatedBy *int64
}

// RenderedBand is a band plus the cards one caller may see.
type RenderedBand struct {
	Band  PromoBand
	Items []RailRow
}

// validateBand holds the write path to what the CHECK constraints will
// accept, so the operator gets a 400 that names the problem instead of
// a 500 carrying a 23514.
//
// ⚠️ The URL check is a SECURITY check, not a formatting one. The CTA
// becomes an `href` on the browse page of every reader the band is shown
// to, and Svelte does not sanitise hrefs — a `javascript:` URL here is
// stored XSS. The admissible shapes are an absolute http(s) URL and a
// site-relative path; a scheme-relative `//host/x` is refused because a
// reader cannot tell it from a local link. The same rule is a CHECK in
// migration 00053, which is the backstop; this is the contract.
func validateBand(in *BandInput) error {
	in.Title = strings.TrimSpace(in.Title)
	in.Blurb = strings.TrimSpace(in.Blurb)
	in.CTALabel = strings.TrimSpace(in.CTALabel)
	in.CTAURL = strings.TrimSpace(in.CTAURL)

	if in.Scope == "" {
		in.Scope = ScopeOrg
	}
	if in.Scope != ScopeOrg && in.Scope != ScopePublic {
		return ErrBandScopeNotWritable
	}
	if in.AfterPage < 1 {
		return ErrBadAfterPage
	}
	if (in.CTALabel == "") != (in.CTAURL == "") {
		return ErrBadCTA
	}
	if in.CTAURL == "" {
		return nil
	}
	if strings.HasPrefix(in.CTAURL, "/") {
		// A site-relative path, but NOT a scheme-relative "//host".
		if strings.HasPrefix(in.CTAURL, "//") {
			return ErrBadCTA
		}
		return nil
	}
	u, err := url.Parse(in.CTAURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ErrBadCTA
	}
	return nil
}

// Band returns the install's band definition, enabled or not, with no
// audience gate. The ADMIN read.
func (h *Handler) Band(ctx context.Context) (PromoBand, error) {
	row, err := New(h.Pool).GetPromoBand(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return PromoBand{}, ErrNoBand
	}
	if err != nil {
		return PromoBand{}, fmt.Errorf("featured: get band: %w", err)
	}
	return row, nil
}

// SaveBand upserts the singleton band.
//
// The upsert runs in ONE transaction with the read that decides which
// half of it to run, because two admins saving at once would otherwise
// both find no band and both insert — leaving an install with two, which
// the reader would silently resolve by picking one and the second admin
// would experience as their edit vanishing.
func (h *Handler) SaveBand(ctx context.Context, in BandInput) (PromoBand, error) {
	if err := validateBand(&in); err != nil {
		return PromoBand{}, err
	}
	var out PromoBand
	err := pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		q := New(tx)
		existing, err := q.GetPromoBand(ctx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			out, err = q.InsertPromoBand(ctx, InsertPromoBandParams{
				Title:            in.Title,
				Blurb:            in.Blurb,
				CtaLabel:         in.CTALabel,
				CtaUrl:           in.CTAURL,
				Enabled:          in.Enabled,
				AfterPage:        in.AfterPage,
				Scope:            in.Scope,
				CreatedByUserRef: in.CreatedBy,
			})
			return err
		case err != nil:
			return err
		default:
			out, err = q.UpdatePromoBand(ctx, UpdatePromoBandParams{
				ID:        existing.ID,
				Title:     in.Title,
				Blurb:     in.Blurb,
				CtaLabel:  in.CTALabel,
				CtaUrl:    in.CTAURL,
				Enabled:   in.Enabled,
				AfterPage: in.AfterPage,
				Scope:     in.Scope,
			})
			return err
		}
	})
	if err != nil {
		return PromoBand{}, fmt.Errorf("featured: save band: %w", err)
	}
	return out, nil
}

// RemoveBand deletes the band and, by cascade, its cards.
func (h *Handler) RemoveBand(ctx context.Context, id uuid.UUID) error {
	n, err := New(h.Pool).DeletePromoBand(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		return fmt.Errorf("featured: delete band: %w", err)
	}
	if n == 0 {
		return ErrNoBand
	}
	return nil
}

// GetRenderableBand returns the band this caller's AUDIENCE admits, or
// ErrNoBand.
//
// This is the only place the band's audience is read, and it is read
// with the same [ScopeVisibleSQL] every other featured surface uses —
// applied to `promo_bands.scope` rather than to a card's, because the
// band is the unit that carries an audience (migration 00053).
//
// Hand-built rather than sqlc for the reason [ListPlacements] is: the
// fragment is produced in Go. Unlike the two static readers pinned by
// TestScopeVisibleSQL_PinnedInStaticQueries, this one splices the helper
// for real, so there is no hand-copy to keep in step.
//
// `enabled` is part of the WHERE and not a field the caller filters on
// afterwards: a disabled band must not reach a reader at all, and a
// nil-returning read is a stronger guarantee than a boolean somebody
// downstream has to remember to check.
func GetRenderableBand(
	ctx context.Context,
	pool *pgxpool.Pool,
	caller visibility.Caller,
) (PromoBand, error) {
	sql := `SELECT b.id, b.title, b.blurb, b.cta_label, b.cta_url, b.enabled,
       b.after_page, b.scope, b.created_at, b.updated_at, b.created_by_user_ref
  FROM promo_bands b
 WHERE b.enabled
   AND ` + ScopeVisibleSQL("b", caller) + `
 ORDER BY b.after_page ASC, b.created_at ASC, b.id ASC
 LIMIT 1`

	var b PromoBand
	err := pool.QueryRow(ctx, sql).Scan(
		&b.ID, &b.Title, &b.Blurb, &b.CtaLabel, &b.CtaUrl, &b.Enabled,
		&b.AfterPage, &b.Scope, &b.CreatedAt, &b.UpdatedAt, &b.CreatedByUserRef,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PromoBand{}, ErrNoBand
	}
	if err != nil {
		return PromoBand{}, fmt.Errorf("featured: renderable band: %w", err)
	}
	return b, nil
}

// RenderableBand resolves the whole strip for one caller: the band, then
// the cards that caller may see, then the collapse decision.
//
// ⚠️ THE COLLAPSE IS DECIDED HERE, and returning ErrNoBand for an empty
// card list is the load-bearing line. A band whose every card filtered
// away for this reader is not "a band with zero items" — it is nothing,
// and it must not reach the client as a headline and a button floating
// over an empty row. Deciding it server-side means the rule lives beside
// the filter that produced the emptiness, rather than in a client-side
// length check that a second client (or a second surface) would have to
// re-implement and could omit.
//
// The title is required for the same reason: a strip with a button and
// no headline is an unfinished draft, and an operator who saved one
// should see nothing rather than see it published.
func RenderableBand(
	ctx context.Context,
	pool *pgxpool.Pool,
	q PlacementQuery,
) (*RenderedBand, error) {
	band, err := GetRenderableBand(ctx, pool, q.Caller)
	if errors.Is(err, ErrNoBand) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(band.Title) == "" {
		return nil, nil
	}
	id := uuid.UUID(band.ID.Bytes)
	q.BandID = &id
	items, err := ListPlacements(ctx, pool, q)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return &RenderedBand{Band: band, Items: items}, nil
}
