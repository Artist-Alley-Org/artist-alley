// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package inboundgrants is the receiver's own record of what paired
// peers granted this instance (#1571, spec v1 §7.5.1, §7.5.2, §16.6).
//
// # What a grant is
//
// An aa:Share applied under the share-lifecycle-1 profile. It names the
// origin's object, the grantor actor, an optional local recipient, a
// scope on the view < comment < annotate < remix ladder, an optional
// expiry and, with original-transfer-1, whether the original file is
// granted. A grant is an access right, not possession of bytes: ending
// it stops new access and recalls nothing already delivered.
//
// # Order without clocks
//
// Grants are ordered only by the sender's signed references:
// aa:supersedes on an aa:Share (the grant it replaces in the same tuple)
// and aa:revokes on an aa:Unshare (the grant it ends). Each applied
// activity is one transaction, serialised per peer:
//
//	Share(g, supersedes=p)
//	    g already stored                -> duplicate, no effect
//	    p stored, other tuple           -> fail closed: end p, store g ended (ambiguous_supersede)
//	    p stored and live               -> end p (superseded)
//	    p unseen                        -> tombstone p (superseded)
//	    g tombstoned                    -> store g ended (tombstoned_on_arrival)
//	    recipient name unknown          -> store g ended (recipient_unbound)
//	    expires_at not after now        -> store g ended (expired_on_arrival)
//	    another live grant in g's tuple -> fail closed: end it, store g ended (ambiguous_supersede)
//	    otherwise                       -> store g live
//	Unshare(revokes=g)
//	    g stored and live -> end g (revoked)
//	    g stored, ended   -> no effect
//	    g unseen          -> tombstone g (revoked)
//
// The tuple is (peer, grantor actor, object, recipient), the origin's
// own unique slot, so two grantors' live shares of one object coexist
// and either one authorises.
//
// # Authorisation
//
// Allows reads the database on every call; there is no positive cache.
// A grant authorises only while live, unexpired by THIS database's clock,
// on an enabled and connected peer, for the asking recipient (a
// user-specific grant only that user; an instance-signed request only a
// peer-wide grant), at or above the asked scope, and for the original
// class only with allow_original. The object is matched exactly, or,
// for a collection grant, through a membership the origin announced and
// #1574 recorded; with no membership recorded a container grant
// authorises no file. Overlapping grants are independent: ending one
// leaves access through any other live one. Identical bytes, or a local
// user's own asset with the same content, are never a right.
//
// AllowsTx is the same decision inside the caller's transaction, with
// the matched grant row held FOR SHARE until that transaction ends. An
// end (revoke, supersede) takes the row FOR UPDATE, so it either commits
// first and the check fails, or waits for the caller's commit.
package inboundgrants

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// End reasons (federation_inbound_grants.end_reason).
const (
	EndRevoked             = "revoked"
	EndSuperseded          = "superseded"
	EndAmbiguousSupersede  = "ambiguous_supersede"
	EndRecipientUnbound    = "recipient_unbound"
	EndTombstonedOnArrival = "tombstoned_on_arrival"
	EndExpiredOnArrival    = "expired_on_arrival"
)

// Outcome is what applying one activity did.
type Outcome string

const (
	OutcomeLive       Outcome = "live"       // the Share is a live grant
	OutcomeEnded      Outcome = "ended"      // the Share is stored ended (Result.Reason)
	OutcomeDuplicate  Outcome = "duplicate"  // already applied; nothing changed
	OutcomeRevoked    Outcome = "revoked"    // the Unshare ended a live grant
	OutcomeTombstoned Outcome = "tombstoned" // the Unshare named a grant not seen yet
	OutcomeNoEffect   Outcome = "no_effect"  // the Unshare named a grant already ended
)

// Result reports an apply.
type Result struct {
	Outcome Outcome
	Reason  string    // end reason for OutcomeEnded
	GrantID uuid.UUID // the Share's row, or the grant an Unshare touched
	// TupleMismatch: an Unshare named a stored grant of another tuple
	// than its own members; the grant was ended anyway (revoking is
	// always the safe direction) and this is recorded.
	TupleMismatch bool
}

// Share is an authenticated, member-validated aa:Share.
type Share struct {
	PeerID          uuid.UUID
	ActivityURI     string
	GrantorActorURI string
	ObjectKind      federation.ShareObjectKind
	ObjectURI       string
	TargetUserURI   string // "" = any user of this instance
	Scope           federation.ShareScope
	AllowOriginal   bool
	ExpiresAt       *time.Time
	Supersedes      string // "" on a tuple's first grant
}

// Unshare is an authenticated, member-validated aa:Unshare or
// aa:RevokeShare.
type Unshare struct {
	PeerID        uuid.UUID
	ActivityURI   string
	ActorURI      string
	ObjectKind    federation.ShareObjectKind
	ObjectURI     string
	TargetUserURI string
	Revokes       string
}

// ClassOriginal is the original-file class (§7.5.7).
const ClassOriginal = "original"

// Query asks whether a peer's grant covers a use.
type Query struct {
	PeerID uuid.UUID
	// ActorURI is the local actor acting (a /users/<name> URI on this
	// instance), or "" for the instance itself, which only a peer-wide
	// grant covers.
	ActorURI  string
	ObjectURI string
	// Class is ClassOriginal for the original file; "" for any other use.
	Class string
	// MinScope is the least scope the use needs; "" means view.
	MinScope federation.ShareScope
}

// ErrNoGrant is every refusal of Allows: no live grant covers the use.
var ErrNoGrant = errors.New("inboundgrants: no live inbound grant covers this use")

// ErrInvalid is an activity the store refuses to record: it does not
// name this instance, or its recipient is not a local actor URI. The
// inbox validates members first, so reaching this is a defect upstream.
var ErrInvalid = errors.New("inboundgrants: invalid grant activity")

// Store is the inbound grant record.
type Store struct {
	pool         *pgxpool.Pool
	localBaseURL func(ctx context.Context) string
	audit        *audit.Recorder
	logger       *slog.Logger
}

// New wires the store. localBaseURL is this instance's base URL (the
// origin target_user URIs must be on); rec and logger may be nil.
func New(pool *pgxpool.Pool, localBaseURL func(ctx context.Context) string, rec *audit.Recorder, logger *slog.Logger) *Store {
	return &Store{pool: pool, localBaseURL: localBaseURL, audit: rec, logger: logger}
}

func (s *Store) localOrigin(ctx context.Context) string {
	if s.localBaseURL == nil {
		return ""
	}
	return federation.OriginOf(s.localBaseURL(ctx))
}

// lockPeer serialises every grant apply of one peer for the rest of tx:
// a Share and the Unshare that names it, or two Shares of one tuple,
// never interleave their reads and writes.
func lockPeer(ctx context.Context, tx pgx.Tx, peerID uuid.UUID) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('federation_inbound_grants:' || $1::text, 0))`, peerID)
	return err
}

type storedGrant struct {
	ID            uuid.UUID
	Grantor       string
	ObjectURI     string
	TargetUserURI *string
	State         string
}

func (g storedGrant) sameTuple(grantor, objectURI, target string) bool {
	t := ""
	if g.TargetUserURI != nil {
		t = *g.TargetUserURI
	}
	return g.Grantor == grantor && g.ObjectURI == objectURI && t == target
}

func getGrant(ctx context.Context, tx pgx.Tx, peerID uuid.UUID, activityURI string) (storedGrant, bool, error) {
	var g storedGrant
	err := tx.QueryRow(ctx, `SELECT id, grantor_actor_uri, object_uri, target_user_uri, state
		FROM federation_inbound_grants WHERE peer_id = $1 AND grant_activity_uri = $2 FOR UPDATE`,
		peerID, activityURI).Scan(&g.ID, &g.Grantor, &g.ObjectURI, &g.TargetUserURI, &g.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedGrant{}, false, nil
	}
	return g, err == nil, err
}

func endGrant(ctx context.Context, tx pgx.Tx, id uuid.UUID, reason, byActivity string) error {
	_, err := tx.Exec(ctx, `UPDATE federation_inbound_grants
		SET state = 'ended', end_reason = $2, ended_by_activity_uri = $3, ended_at = now()
		WHERE id = $1 AND state = 'live'`, id, reason, byActivity)
	return err
}

func tombstone(ctx context.Context, tx pgx.Tx, peerID uuid.UUID, grantURI, kind, byActivity string) error {
	_, err := tx.Exec(ctx, `INSERT INTO federation_inbound_tombstones (peer_id, grant_activity_uri, kind, by_activity_uri)
		VALUES ($1, $2, $3, $4) ON CONFLICT (peer_id, grant_activity_uri) DO NOTHING`, peerID, grantURI, kind, byActivity)
	return err
}

// ApplyShare records an aa:Share (see the package comment for the
// state machine). One transaction; safe to retry.
func (s *Store) ApplyShare(ctx context.Context, sh Share) (Result, error) {
	if !sh.Scope.Valid() || !sh.ObjectKind.Valid() || sh.ActivityURI == "" || sh.ObjectURI == "" || sh.GrantorActorURI == "" {
		return Result{}, fmt.Errorf("%w: incomplete aa:Share", ErrInvalid)
	}
	var recipientName string
	if sh.TargetUserURI != "" {
		name, ok := federation.LocalUsername(sh.TargetUserURI, s.localOrigin(ctx))
		if !ok {
			return Result{}, fmt.Errorf("%w: target_user is not a /users/<name> actor on this instance", ErrInvalid)
		}
		recipientName = name
	}
	var res Result
	var ended []endedGrant
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		ended = ended[:0]
		if err := lockPeer(ctx, tx, sh.PeerID); err != nil {
			return err
		}
		if g, ok, err := getGrant(ctx, tx, sh.PeerID, sh.ActivityURI); err != nil {
			return err
		} else if ok {
			res = Result{Outcome: OutcomeDuplicate, GrantID: g.ID}
			return nil
		}

		reason := ""
		var endedBy string
		// The replaced grant, as the origin names it.
		if sh.Supersedes != "" {
			p, ok, err := getGrant(ctx, tx, sh.PeerID, sh.Supersedes)
			if err != nil {
				return err
			}
			switch {
			case !ok:
				if err := tombstone(ctx, tx, sh.PeerID, sh.Supersedes, EndSuperseded, sh.ActivityURI); err != nil {
					return err
				}
			case !p.sameTuple(sh.GrantorActorURI, sh.ObjectURI, sh.TargetUserURI):
				// The origin's chain names a grant of another tuple:
				// inconsistent, so neither grants anything.
				if p.State == "live" {
					if err := endGrant(ctx, tx, p.ID, EndAmbiguousSupersede, sh.ActivityURI); err != nil {
						return err
					}
					ended = append(ended, endedGrant{p.ID, EndAmbiguousSupersede})
				}
				reason, endedBy = EndAmbiguousSupersede, sh.ActivityURI
			case p.State == "live":
				if err := endGrant(ctx, tx, p.ID, EndSuperseded, sh.ActivityURI); err != nil {
					return err
				}
				ended = append(ended, endedGrant{p.ID, EndSuperseded})
			}
		}

		if reason == "" {
			var by string
			err := tx.QueryRow(ctx, `SELECT by_activity_uri FROM federation_inbound_tombstones
				WHERE peer_id = $1 AND grant_activity_uri = $2`, sh.PeerID, sh.ActivityURI).Scan(&by)
			switch {
			case err == nil:
				reason, endedBy = EndTombstonedOnArrival, by
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
		}

		var recipientRef *int64
		if reason == "" && recipientName != "" {
			var ref int64
			err := tx.QueryRow(ctx, `SELECT ref FROM "user" WHERE username = $1`, recipientName).Scan(&ref)
			switch {
			case err == nil:
				recipientRef = &ref
			case errors.Is(err, pgx.ErrNoRows):
				reason = EndRecipientUnbound
			default:
				return err
			}
		}

		if reason == "" && sh.ExpiresAt != nil {
			var expired bool
			if err := tx.QueryRow(ctx, `SELECT $1::timestamptz <= now()`, *sh.ExpiresAt).Scan(&expired); err != nil {
				return err
			}
			if expired {
				reason = EndExpiredOnArrival
			}
		}

		if reason == "" {
			// Another live grant in this tuple that the Share does not
			// name: the origin's state is inconsistent. Fail closed.
			var other uuid.UUID
			err := tx.QueryRow(ctx, `SELECT id FROM federation_inbound_grants
				WHERE peer_id = $1 AND grantor_actor_uri = $2 AND object_uri = $3
				  AND COALESCE(target_user_uri, '') = $4 AND state = 'live'
				FOR UPDATE`, sh.PeerID, sh.GrantorActorURI, sh.ObjectURI, sh.TargetUserURI).Scan(&other)
			switch {
			case err == nil:
				if err := endGrant(ctx, tx, other, EndAmbiguousSupersede, sh.ActivityURI); err != nil {
					return err
				}
				ended = append(ended, endedGrant{other, EndAmbiguousSupersede})
				reason, endedBy = EndAmbiguousSupersede, sh.ActivityURI
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
		}

		var target, supersedes *string
		if sh.TargetUserURI != "" {
			target = &sh.TargetUserURI
		}
		if sh.Supersedes != "" {
			supersedes = &sh.Supersedes
		}
		state := "live"
		var reasonPtr, endedByPtr *string
		if reason != "" {
			state = "ended"
			reasonPtr = &reason
			if endedBy != "" {
				endedByPtr = &endedBy
			}
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO federation_inbound_grants
			(peer_id, grant_activity_uri, grantor_actor_uri, object_kind, object_uri, target_user_uri,
			 recipient_user_ref, scope, allow_original, expires_at, supersedes_activity_uri,
			 state, end_reason, ended_by_activity_uri, ended_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			        CASE WHEN $12 = 'ended' THEN now() END)
			RETURNING id`,
			sh.PeerID, sh.ActivityURI, sh.GrantorActorURI, string(sh.ObjectKind), sh.ObjectURI, target,
			recipientRef, string(sh.Scope), sh.AllowOriginal, sh.ExpiresAt, supersedes,
			state, reasonPtr, endedByPtr).Scan(&id); err != nil {
			return err
		}
		if reason == "" {
			res = Result{Outcome: OutcomeLive, GrantID: id}
		} else {
			res = Result{Outcome: OutcomeEnded, Reason: reason, GrantID: id}
		}
		s.writeAudit(ctx, tx, audit.EventFederationInboundGrantApplied, map[string]any{
			"peer_id":            sh.PeerID.String(),
			"grant_id":           id.String(),
			"grant_activity_uri": sh.ActivityURI,
			"object_kind":        string(sh.ObjectKind),
			"object_uri":         sh.ObjectURI,
			"scope":              string(sh.Scope),
			"allow_original":     sh.AllowOriginal,
			"outcome":            string(res.Outcome),
			"reason":             reason,
		})
		for _, e := range ended {
			s.writeAudit(ctx, tx, audit.EventFederationInboundGrantEnded, map[string]any{
				"peer_id": sh.PeerID.String(), "grant_id": e.id.String(), "reason": e.reason,
				"by_activity_uri": sh.ActivityURI,
			})
		}
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("inboundgrants: apply aa:Share: %w", err)
	}
	return res, nil
}

type endedGrant struct {
	id     uuid.UUID
	reason string
}

// ApplyUnshare records an aa:Unshare / aa:RevokeShare naming the grant
// it ends (aa:revokes). One transaction; safe to retry.
func (s *Store) ApplyUnshare(ctx context.Context, u Unshare) (Result, error) {
	if u.ActivityURI == "" || u.Revokes == "" {
		return Result{}, fmt.Errorf("%w: incomplete aa:Unshare", ErrInvalid)
	}
	var res Result
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockPeer(ctx, tx, u.PeerID); err != nil {
			return err
		}
		g, ok, err := getGrant(ctx, tx, u.PeerID, u.Revokes)
		if err != nil {
			return err
		}
		if !ok {
			if err := tombstone(ctx, tx, u.PeerID, u.Revokes, EndRevoked, u.ActivityURI); err != nil {
				return err
			}
			res = Result{Outcome: OutcomeTombstoned}
			return nil
		}
		// The members name the tuple the sender believes it revokes. A
		// stored grant of another tuple is still ended (revoking is the
		// safe direction); the mismatch is recorded.
		target := ""
		if g.TargetUserURI != nil {
			target = *g.TargetUserURI
		}
		mismatch := g.ObjectURI != u.ObjectURI || target != u.TargetUserURI
		if g.State != "live" {
			res = Result{Outcome: OutcomeNoEffect, GrantID: g.ID, TupleMismatch: mismatch}
			return nil
		}
		if err := endGrant(ctx, tx, g.ID, EndRevoked, u.ActivityURI); err != nil {
			return err
		}
		res = Result{Outcome: OutcomeRevoked, GrantID: g.ID, TupleMismatch: mismatch}
		s.writeAudit(ctx, tx, audit.EventFederationInboundGrantEnded, map[string]any{
			"peer_id": u.PeerID.String(), "grant_id": g.ID.String(), "reason": EndRevoked,
			"by_activity_uri": u.ActivityURI, "tuple_mismatch": mismatch,
		})
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("inboundgrants: apply aa:Unshare: %w", err)
	}
	return res, nil
}

func (s *Store) writeAudit(ctx context.Context, tx pgx.Tx, event string, meta map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.WriteInTx(ctx, audit.New(tx), event, nil, nil, meta)
}

// queryer is a pool or a transaction.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Allows returns the id of a live grant covering q, or ErrNoGrant. It
// reads the database every time.
func (s *Store) Allows(ctx context.Context, q Query) (uuid.UUID, error) {
	return s.allows(ctx, s.pool, q, false)
}

// AllowsTx is Allows inside tx, holding the matched grant row FOR SHARE
// until tx ends, so a concurrent end either committed first (and this
// refuses) or waits for tx.
func (s *Store) AllowsTx(ctx context.Context, tx pgx.Tx, q Query) (uuid.UUID, error) {
	return s.allows(ctx, tx, q, true)
}

func (s *Store) allows(ctx context.Context, db queryer, q Query, lock bool) (uuid.UUID, error) {
	minScope := q.MinScope
	if minScope == "" {
		minScope = federation.ShareScopeView
	}
	if !minScope.Valid() || q.ObjectURI == "" || q.PeerID == (uuid.UUID{}) || (q.Class != "" && q.Class != ClassOriginal) {
		return uuid.UUID{}, fmt.Errorf("%w: incomplete query", ErrNoGrant)
	}
	// The asking recipient: the local user an actor URI names, or none
	// (the instance), which only a peer-wide grant covers. An actor that
	// does not resolve is no recipient at all: refused.
	var userRef *int64
	if q.ActorURI != "" {
		name, ok := federation.LocalUsername(q.ActorURI, s.localOrigin(ctx))
		if !ok {
			return uuid.UUID{}, fmt.Errorf("%w: %q is not a local actor", ErrNoGrant, q.ActorURI)
		}
		var ref int64
		if err := db.QueryRow(ctx, `SELECT ref FROM "user" WHERE username = $1`, name).Scan(&ref); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return uuid.UUID{}, fmt.Errorf("%w: no local user %q", ErrNoGrant, name)
			}
			return uuid.UUID{}, err
		}
		userRef = &ref
	}
	// Two steps, so expiry is judged by a clock read AFTER any wait for
	// the grant rows (#1571 review): the candidates that match every rule
	// except expiry (locked FOR SHARE in AllowsTx; that statement is where
	// a concurrent end makes it wait), then the database's current time,
	// clock_timestamp(), read once that statement has returned. Neither
	// now() (the transaction's START time) nor clock_timestamp() inside
	// the locking statement would do: both can be evaluated before the
	// wait. Candidates are bounded and ordered longest-lived first, so a
	// live grant, if any covers the use, is among them.
	sql := `SELECT g.id, g.expires_at FROM federation_inbound_grants g
		JOIN federation_peers p ON p.id = g.peer_id
		WHERE g.peer_id = $1 AND g.state = 'live'
		  AND p.enabled AND p.status = 'connected'
		  AND (g.recipient_user_ref IS NULL AND g.target_user_uri IS NULL
		       OR g.recipient_user_ref = $2)
		  AND (g.object_uri = $3
		       OR (g.object_kind = 'collection' AND EXISTS (
		             SELECT 1 FROM federation_inbound_memberships m
		              WHERE m.peer_id = g.peer_id AND m.container_uri = g.object_uri AND m.member_uri = $3)))
		  AND (NOT $4::boolean OR g.allow_original)
		  AND array_position(ARRAY['view','comment','annotate','remix'], g.scope)
		      >= array_position(ARRAY['view','comment','annotate','remix'], $5::text)
		ORDER BY g.expires_at DESC NULLS FIRST, g.id
		LIMIT ` + strconv.Itoa(maxCandidates)
	if lock {
		sql += ` FOR SHARE OF g`
	}
	rows, err := db.Query(ctx, sql, q.PeerID, userRef, q.ObjectURI, q.Class == ClassOriginal, string(minScope))
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("inboundgrants: allows: %w", err)
	}
	type candidate struct {
		id      uuid.UUID
		expires pgtype.Timestamptz
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.expires); err != nil {
			rows.Close()
			return uuid.UUID{}, fmt.Errorf("inboundgrants: allows: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return uuid.UUID{}, fmt.Errorf("inboundgrants: allows: %w", err)
	}
	if len(cands) == 0 {
		return uuid.UUID{}, ErrNoGrant
	}
	var now time.Time
	if err := db.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return uuid.UUID{}, fmt.Errorf("inboundgrants: allows: clock: %w", err)
	}
	for _, c := range cands {
		if !c.expires.Valid || c.expires.Time.After(now) {
			return c.id, nil
		}
	}
	return uuid.UUID{}, fmt.Errorf("%w: every covering grant has expired", ErrNoGrant)
}

// maxCandidates bounds the grants one decision reads (and, in AllowsTx,
// locks). At most one live grant exists per tuple (peer, grantor,
// object, recipient), so this many covers every realistic overlap; they
// are read longest-lived first.
const maxCandidates = 16
