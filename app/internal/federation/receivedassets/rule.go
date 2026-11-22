// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package receivedassets is the read side of #1574: who may see a
// received asset record, and who may receive its original file, on this
// instance. A received record is never a local asset (no assets row): it
// is shown only here, through /federation/received-assets and the
// "Received" account page, and never in browse, search or collections.
//
// # The two rules
//
// Both are evaluated in SQL on every request; nothing caches a positive
// answer and no URL outlives a check.
//
// Viewable (the list and the detail), all of:
//
//   - the caller is signed in (a user ref above 0): an anonymous or
//     public-mode visitor never sees a received record;
//   - the record is complete (every original verified and pinned);
//   - its tier is public. A team record is held but NOT shown: spec v1
//     §17.2 makes the origin's tier "the minimum tier" the receiver "never
//     lowers", the spec gives `team` no receiver meaning, and this
//     receiver has no team of the origin's to show it to. Showing it to
//     every user of a peer-wide grant would widen, that is lower, it;
//   - its sending peer is enabled and connected;
//   - a live inbound grant FROM THAT PEER (the record's own peer_id) for
//     the asset (object_kind asset, object_uri = the record's
//     origin_asset_uri), unexpired by this instance's clock, and either
//     peer-wide (no target user and no recipient) or bound to the caller
//     (§16.6 Authorising). A target that never bound to a local user is
//     not peer-wide, the same as inboundgrants.AllowsTx. Any scope will
//     do: view is the lowest.
//
// WithdrawnVisible (the withdrawal notice, #1575, spec v1 §17.3): the
// viewing rule with the record withdrawn instead of complete. A caller who
// could view the record while its grant lasts gets a notice instead of an
// unexplained 404; anyone else still gets 404, so the notice reveals
// nothing about the asset to a caller who could not see it.
//
// OriginalReadable (the original file): the same rule, with the SAME
// grant row also carrying the original-file permission (allow_original,
// §16.6: "for an original file, it carries aa:originalTransfer"). A
// view-only grant and an unrelated original grant never combine.
//
// The SQL fragments are the enforcement; [Viewable] and
// [OriginalReadable] are their Go twins over [Facts], held equal by
// TestRulesSQL_MatchGo.
package receivedassets

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Grant is one inbound grant row the rules read.
type Grant struct {
	State         string
	ExpiresAt     *time.Time
	Recipient     *int64 // recipient_user_ref
	Targeted      bool   // target_user_uri set: never peer-wide, even unbound
	AllowOriginal bool
}

// Facts is what the rules read about one record: its own columns, its
// peer, and the grants from that peer for its asset.
type Facts struct {
	State       string
	Sensitivity string
	PeerEnabled bool
	PeerStatus  string
	Grants      []Grant
}

// Viewable reports whether caller may see the record (Go twin of
// ViewableSQL).
func Viewable(f Facts, caller int64, now time.Time) bool {
	return allowed(f, caller, now, "complete", false)
}

// OriginalReadable reports whether caller may receive the record's
// original (Go twin of OriginalReadableSQL).
func OriginalReadable(f Facts, caller int64, now time.Time) bool {
	return allowed(f, caller, now, "complete", true)
}

// WithdrawnVisible reports whether caller may see the record's withdrawal
// notice (Go twin of WithdrawnVisibleSQL).
func WithdrawnVisible(f Facts, caller int64, now time.Time) bool {
	return allowed(f, caller, now, "withdrawn", false)
}

func allowed(f Facts, caller int64, now time.Time, state string, original bool) bool {
	if caller <= 0 || f.State != state || f.Sensitivity != "public" || !f.PeerEnabled || f.PeerStatus != "connected" {
		return false
	}
	for _, g := range f.Grants {
		if g.State == "live" && (g.ExpiresAt == nil || g.ExpiresAt.After(now)) &&
			((g.Recipient == nil && !g.Targeted) || (g.Recipient != nil && *g.Recipient == caller)) && (!original || g.AllowOriginal) {
			return true
		}
	}
	return false
}

// ViewableSQL is the viewing rule as a boolean SQL expression over the
// federation_received_assets alias r, for the caller user ref bound at
// callerArg (for example "$1").
func ViewableSQL(r, callerArg string) string { return ruleSQL(r, callerArg, "complete", false) }

// OriginalReadableSQL is the original-file rule, the same expression with
// the grant also carrying allow_original.
func OriginalReadableSQL(r, callerArg string) string { return ruleSQL(r, callerArg, "complete", true) }

// WithdrawnVisibleSQL is the withdrawal-notice rule: the viewing rule for a
// withdrawn record.
func WithdrawnVisibleSQL(r, callerArg string) string {
	return ruleSQL(r, callerArg, "withdrawn", false)
}

// ruleSQL is the rule for a record in state (a constant, never input).
func ruleSQL(r, callerArg, state string, original bool) string {
	orig := ""
	if original {
		orig = " AND g.allow_original"
	}
	return `(` + callerArg + `::bigint > 0 AND ` + r + `.state = '` + state + `' AND ` + r + `.sensitivity = 'public'
	  AND EXISTS (SELECT 1 FROM federation_peers rp
	               WHERE rp.id = ` + r + `.peer_id AND rp.enabled AND rp.status = 'connected')
	  AND EXISTS (SELECT 1 FROM federation_inbound_grants g
	               WHERE g.peer_id = ` + r + `.peer_id AND g.object_kind = 'asset' AND g.object_uri = ` + r + `.origin_asset_uri
	                 AND g.state = 'live' AND (g.expires_at IS NULL OR g.expires_at > now())
	                 AND ((g.recipient_user_ref IS NULL AND g.target_user_uri IS NULL) OR g.recipient_user_ref = ` + callerArg + `::bigint)` + orig + `))`
}

// Querier is the read surface LoadFacts needs.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ErrNoRecord: no received record has that id.
var ErrNoRecord = errors.New("receivedassets: no such record")

// LoadFacts reads what the rules need about one record.
func LoadFacts(ctx context.Context, q Querier, id uuid.UUID) (Facts, error) {
	var f Facts
	var peerID uuid.UUID
	var objectURI string
	err := q.QueryRow(ctx, `SELECT r.state, r.sensitivity, r.peer_id, r.origin_asset_uri, p.enabled, p.status
		  FROM federation_received_assets r JOIN federation_peers p ON p.id = r.peer_id
		 WHERE r.id = $1`, id).Scan(&f.State, &f.Sensitivity, &peerID, &objectURI, &f.PeerEnabled, &f.PeerStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return Facts{}, ErrNoRecord
	}
	if err != nil {
		return Facts{}, fmt.Errorf("receivedassets: load record: %w", err)
	}
	rows, err := q.Query(ctx, `SELECT state, expires_at, recipient_user_ref, target_user_uri IS NOT NULL, allow_original FROM federation_inbound_grants
		WHERE peer_id = $1 AND object_kind = 'asset' AND object_uri = $2`, peerID, objectURI)
	if err != nil {
		return Facts{}, fmt.Errorf("receivedassets: load grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.State, &g.ExpiresAt, &g.Recipient, &g.Targeted, &g.AllowOriginal); err != nil {
			return Facts{}, err
		}
		f.Grants = append(f.Grants, g)
	}
	return f, rows.Err()
}
