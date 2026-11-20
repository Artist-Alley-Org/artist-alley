// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package assetingest is the receiver core of #1574: it turns one
// aa:AssetVersion announcement carrying an archival-1 aa:asset member
// (spec v1 §17.2) into a received asset record whose original files are
// held and verified here (§17.6, §8.5), or into a recorded refusal.
//
// # What a record is
//
// The sender's claims, kept as data and never trusted (the aa:asset
// member, its declared media types, file names, identifications, rights
// and description), plus what THIS instance established: which grant
// authorised it, and that every original file it lists arrived with the
// listed size and checksums. A complete record is held, not shown: no
// assets row is created and nothing reads the tables yet (materialising
// it as a visible asset, with its visibility rules, is a later slice).
//
// # Receive, step by step
//
//  1. Validate, without writing anything: the receiver's own peer row
//     is enabled, connected, and has both archival-1 and
//     original-transfer-1 in its stored intersection
//     (profile_not_negotiated otherwise; §16.2, §16.3). The envelope is
//     aa:AssetVersion, the one activity that requires aa:asset (§17.1);
//     federation.ValidateArchivalMembers checks the member strictly
//     (archival_invalid). Its actor and object are on the peer's own
//     origin and the object is /assets/<uuid> (foreign_origin).
//  2. Bind, in one short transaction serialised per (peer, asset):
//     the same activity finds its own record (a repeated delivery),
//     which must be the identical announcement (activity_conflict
//     otherwise; foreign_signer for another signer). A
//     live record for the asset with the same version id and file set is
//     a duplicate; with another file set, version_conflict (§17.2.1, fail
//     closed); with another version id, version_unsupported (version
//     lifecycle is #1575). A restricted or embargoed tier
//     (sensitivity_refused, §7.5.7.4), a file set over the size budget
//     (too_large) and no live inbound grant for (peer, signer, asset,
//     original) (no_inbound_grant, #1571) are recorded as rejected. Only
//     then is a pending record written with its file set.
//  3. Fetch every distinct original through casfetch, which asks both
//     authorities, streams through the fixity check (at most size + 1
//     bytes) and holds the bytes under an ingest pin. Preservation and
//     access entries are claims only (§8.5 serves the original class;
//     derivatives are #1597).
//  4. Complete, in ONE transaction: the record is locked and still
//     pending; the peer row (FOR SHARE) and the grant (AllowsTx, its row
//     held FOR SHARE) are checked again; every object row is locked and
//     taken off pending GC; the record pins each original and the ingest
//     pins go; the originals are marked verified and the record
//     complete, with the grant's id. The database refuses a commit that
//     leaves a complete record with an unpinned or unverified original,
//     or a pending or rejected one holding a pin (migration 00086).
//
// # Failure
//
// A final failure (fixity or length mismatch, an origin refusal, no
// grant, a revoke during the transfer, too large) marks the record
// rejected with that reason. A transient one (timeout, transport,
// a 5xx or 429 answer, a lost lease, storage) leaves it pending, due
// again after a capped backoff, up to Config.MaxAttempts, after which it
// is rejected with the last reason. Either way every ingest pin the
// attempt obtained is released, so nothing it fetched stays pinned and
// unpinned bytes go to GC. A crash between fetch and commit leaves
// ingest pins that casfetch.Reconcile releases. A rejected record never
// blocks a new announcement.
//
// # In production
//
// Steps 1 and 2 are Schedule, which the inbox dispatcher calls (through
// Worker.ScheduleAsset) for an aa:AssetVersion from a peer that
// negotiated both profiles; steps 3 and 4 are Process, which the Worker
// runs in the background for every pending record (worker.go). Receive
// is both in one call. No production peer reaches it yet: archival-1 and
// original-transfer-1 are not advertised (peer.KnownCapabilities), no
// sender emits aa:AssetVersion, and a complete record is not shown.
package assetingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// PinSubjectType is the pin a complete record holds each original under.
const PinSubjectType = "federation_received_asset"

// Record states (federation_received_assets.state).
const (
	StatePending  = "pending"
	StateComplete = "complete"
	StateRejected = "rejected"
)

// Reasons this package adds to casfetch's (which pass through unchanged).
const (
	ReasonUnsupportedActivity = "unsupported_activity"
	ReasonArchivalInvalid     = "archival_invalid"
	ReasonVersionConflict     = "version_conflict"
	ReasonVersionUnsupported  = "version_unsupported"
	// ReasonActivityConflict: an activity id already recorded arrived
	// with another actor, object, version, file set or claim. Nothing is
	// written; the record of the first delivery stands.
	ReasonActivityConflict = "activity_conflict"
)

// Error is a typed refusal or failure. Retryable means the record (if
// any) is still pending and a later delivery may complete it.
type Error struct {
	Reason    string
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	s := "assetingest: " + e.Reason
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// ReasonOf returns the typed reason of a Receive error, or "".
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// Retryable reports whether err left the record pending.
func Retryable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Retryable
}

// Grants is the receiver's inbound grant record (#1571). The production
// implementation is *inboundgrants.Store.
type Grants interface {
	Allows(ctx context.Context, q inboundgrants.Query) (uuid.UUID, error)
	AllowsTx(ctx context.Context, tx pgx.Tx, q inboundgrants.Query) (uuid.UUID, error)
}

var _ Grants = (*inboundgrants.Store)(nil)

// Fetcher receives one original file. The production implementation is
// *casfetch.Client, wired with casfetch.DatabaseGrants over the same
// inbound grant record.
type Fetcher interface {
	Fetch(ctx context.Context, req casfetch.Request) (*casfetch.Result, error)
}

var _ Fetcher = (*casfetch.Client)(nil)

// Auditor records federation.asset_ingest.* events.
type Auditor interface {
	FederationAssetIngest(ctx context.Context, eventType string, meta map[string]any)
}

// Config bounds one record.
type Config struct {
	// MaxTotalBytes caps the sum of a record's original file sizes
	// (too_large above it). casfetch caps each file on its own.
	MaxTotalBytes int64
	// MaxAttempts is how many fetch attempts a record gets before a
	// transient failure becomes its rejection.
	MaxAttempts int32
	// Lease is how long an attempt holds its record: while it runs, its
	// next_attempt_at is kept this far ahead (Worker heartbeat), so no
	// other worker starts it; a crashed attempt's record is due again
	// once the lease lapses.
	Lease time.Duration
	// RetryBase and RetryMax bound the wait before a transient failure is
	// retried: RetryBase doubled per attempt, at most RetryMax.
	RetryBase time.Duration
	RetryMax  time.Duration
}

// DefaultConfig returns production bounds.
func DefaultConfig() Config {
	return Config{MaxTotalBytes: 32 << 30, MaxAttempts: 5, Lease: 10 * time.Minute,
		RetryBase: 30 * time.Second, RetryMax: 30 * time.Minute}
}

// Deps wires the service.
type Deps struct {
	Pool    *pgxpool.Pool
	Storage *storage.Service
	Peers   casfetch.PeerLookup
	Grants  Grants
	Fetcher Fetcher
	Audit   Auditor
	Logger  *slog.Logger
	Config  Config
}

// Service receives asset records.
type Service struct {
	d   Deps
	cfg Config
	// Test hooks (export_test.go).
	afterFetch func()
	inComplete func(ctx context.Context, tx pgx.Tx) error
}

// New builds a service. Zero Config fields take DefaultConfig values.
func New(d Deps) *Service {
	cfg := d.Config
	def := DefaultConfig()
	if cfg.MaxTotalBytes <= 0 {
		cfg.MaxTotalBytes = def.MaxTotalBytes
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = def.MaxAttempts
	}
	if cfg.Lease <= 0 {
		cfg.Lease = def.Lease
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = def.RetryBase
	}
	if cfg.RetryMax < cfg.RetryBase {
		cfg.RetryMax = max(def.RetryMax, cfg.RetryBase)
	}
	return &Service{d: d, cfg: cfg}
}

// retryDelay is the wait before attempt attempts+1 of a transient
// failure: RetryBase doubled per attempt made, at most RetryMax.
func (s *Service) retryDelay(attempts int32) time.Duration {
	d := s.cfg.RetryBase
	for i := int32(1); i < attempts && d < s.cfg.RetryMax; i++ {
		d *= 2
	}
	return min(d, s.cfg.RetryMax)
}

// Announcement is one received aa:AssetVersion: the authenticated,
// member-validated envelope, the peer it came from, and the local
// identity the files are fetched as. The signer is the recipient the
// grant must cover: a local actor for a grant to that user, or the
// instance for a peer-wide grant (§8.5).
type Announcement struct {
	PeerID   uuid.UUID
	Envelope *federation.Envelope
	Signer   casfetch.Signer
}

// Outcome is what Receive established.
type Outcome struct {
	RecordID  uuid.UUID // uuid.Nil when refused before a record exists
	State     string
	Reason    string
	Duplicate bool // the activity, or the same version and file set, was already recorded
	GrantID   uuid.UUID
}

// file is one validated entry of the announced file set.
type file struct {
	pos       int
	role      string
	hash      string
	size      int64
	checksums []federation.ArchivalChecksum
	mediaType string
	filename  string
}

// announced is a validated announcement.
type announced struct {
	peer          casfetch.Peer
	env           *federation.Envelope
	asset         *federation.ArchivalAsset
	raw           json.RawMessage
	originAssetID uuid.UUID
	files         []file
	originals     []file // distinct originals, by hash
	total         int64
	digest        string
	signer        casfetch.Signer
}

// record is the stored state Receive works from.
type record struct {
	id       uuid.UUID
	state    string
	reason   string
	attempts int32
	grantID  uuid.UUID
}

// Receive schedules one announcement and runs its first attempt at once
// (see the package header): Schedule, then Process with the delivery's
// own signer. It is idempotent per activity: a repeated delivery returns
// the stored outcome, or resumes a pending record. The production path
// is Schedule from the inbox and Process from the Worker; Receive is
// that path in one call.
func (s *Service) Receive(ctx context.Context, a Announcement) (Outcome, error) {
	out, err := s.Schedule(ctx, a)
	if err != nil || out.State != StatePending {
		return out, err
	}
	res, err := s.process(ctx, out.RecordID, func(keyID, actorURI string) (casfetch.Signer, error) {
		if keyID != a.Signer.KeyID || actorURI != a.Signer.ActorURI {
			return casfetch.Signer{}, errOtherSigner
		}
		return a.Signer, nil
	})
	res.Duplicate = res.Duplicate || out.Duplicate
	return res, err
}

// Schedule validates one announcement and binds it to its record (steps
// 1 and 2 of the package header), without any network transfer. A
// pending outcome means an import is scheduled: the record is the
// durable task, which Process (the Worker) runs. A complete record is
// returned as it is, never fetched again; a refusal is recorded on a
// rejected record where step 2 says so, or returned with no record.
func (s *Service) Schedule(ctx context.Context, a Announcement) (Outcome, error) {
	an, err := s.validate(ctx, a)
	if err != nil {
		meta := map[string]any{"peer_id": a.PeerID.String(), "reason": ReasonOf(err), "error": err.Error()}
		if a.Envelope != nil {
			meta["activity_uri"] = a.Envelope.ID
			meta["origin_asset_uri"] = a.Envelope.Object
		}
		s.audit(ctx, audit.EventFederationAssetIngestRejected, meta)
		return Outcome{State: StateRejected, Reason: ReasonOf(err)}, err
	}
	rec, dup, err := s.bind(ctx, an)
	if err != nil {
		if !Retryable(err) {
			s.auditRecord(ctx, audit.EventFederationAssetIngestRejected, an.ref(), uuid.Nil, ReasonOf(err), map[string]any{"error": err.Error()})
			return Outcome{State: StateRejected, Reason: ReasonOf(err)}, err
		}
		return Outcome{State: StatePending, Reason: ReasonOf(err)}, err
	}
	switch rec.state {
	case StateComplete:
		return Outcome{RecordID: rec.id, State: rec.state, Duplicate: dup, GrantID: rec.grantID}, nil
	case StateRejected:
		if !dup {
			s.auditRecord(ctx, audit.EventFederationAssetIngestRejected, an.ref(), rec.id, rec.reason, nil)
		}
		return Outcome{RecordID: rec.id, State: rec.state, Reason: rec.reason, Duplicate: dup},
			&Error{Reason: rec.reason}
	}
	return Outcome{RecordID: rec.id, State: StatePending, Duplicate: dup}, nil
}

// --- validate ---------------------------------------------------------------

func (s *Service) validate(ctx context.Context, a Announcement) (*announced, error) {
	env := a.Envelope
	if env == nil || a.PeerID == uuid.Nil {
		return nil, &Error{Reason: casfetch.ReasonInvalidRequest, Err: errors.New("no envelope or peer")}
	}
	p, err := s.d.Peers.ByID(ctx, a.PeerID)
	if err != nil {
		return nil, &Error{Reason: casfetch.ReasonPeerUnavailable, Err: err}
	}
	if !p.Enabled || !p.Connected {
		return nil, &Error{Reason: casfetch.ReasonPeerUnavailable}
	}
	if !profilesInEffect(p.Capabilities) {
		return nil, &Error{Reason: casfetch.ReasonProfile,
			Err: fmt.Errorf("%w: archival-1 and original-transfer-1 are not both negotiated", federation.ErrProfileNotNegotiated)}
	}
	if env.Type != federation.ActivityAAAssetVersion {
		return nil, &Error{Reason: ReasonUnsupportedActivity, Err: fmt.Errorf("%s is not received as an asset record", env.Type)}
	}
	if err := federation.ValidateArchivalMembers(env, true); err != nil {
		reason := ReasonArchivalInvalid
		if errors.Is(err, federation.ErrProfileNotNegotiated) {
			reason = casfetch.ReasonProfile
		}
		return nil, &Error{Reason: reason, Err: err}
	}
	raw := env.Extra[federation.MemberArchivalAsset]
	asset, err := federation.ParseArchivalAsset(raw)
	if err != nil {
		return nil, &Error{Reason: ReasonArchivalInvalid, Err: err}
	}
	peerOrigin := federation.OriginOf(p.InstanceURL)
	if peerOrigin == "" || federation.OriginOf(env.Actor) != peerOrigin {
		return nil, &Error{Reason: casfetch.ReasonForeignOrigin, Err: errors.New("the actor is not on the peer's origin")}
	}
	assetID, err := originAssetID(env.Object, peerOrigin)
	if err != nil {
		return nil, &Error{Reason: casfetch.ReasonForeignOrigin, Err: err}
	}
	if a.Signer.KeyID == "" {
		return nil, &Error{Reason: casfetch.ReasonInvalidRequest, Err: errors.New("no signer")}
	}
	an := &announced{peer: p, env: env, asset: asset, raw: raw, originAssetID: assetID, signer: a.Signer}
	seen := map[string]bool{}
	for i, f := range asset.Files {
		hash := strings.TrimPrefix(f.ContentURI, "cas://")
		fl := file{pos: i, role: f.Role, hash: hash, size: *f.Size, checksums: f.Checksums,
			mediaType: f.MediaType, filename: f.OriginalFilename}
		an.files = append(an.files, fl)
		if f.Role == "original" && !seen[hash] {
			seen[hash] = true
			an.originals = append(an.originals, fl)
			an.total += fl.size
		}
	}
	sort.Slice(an.originals, func(i, j int) bool { return an.originals[i].hash < an.originals[j].hash })
	an.digest = fileSetDigest(an.files)
	return an, nil
}

func profilesInEffect(caps []string) bool {
	return slices.Contains(caps, federation.ProfileArchival1) && slices.Contains(caps, string(peer.CapOriginalTransfer1))
}

// originAssetID returns the asset id of an object URI on peerOrigin of
// the form <origin>/assets/<uuid>, with no query or fragment.
func originAssetID(objectURI, peerOrigin string) (uuid.UUID, error) {
	u, err := url.Parse(objectURI)
	if err != nil || federation.OriginOf(objectURI) != peerOrigin || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return uuid.Nil, errors.New("the asset is not on the peer's own origin")
	}
	rest, ok := strings.CutPrefix(u.Path, "/assets/")
	if !ok {
		return uuid.Nil, errors.New("the object is not an asset URI")
	}
	id, err := uuid.Parse(rest)
	if err != nil || id.String() != rest {
		return uuid.Nil, errors.New("the object is not an asset URI")
	}
	return id, nil
}

// fileSetDigest identifies a version's bytes: each entry's role, hash,
// size and checksums, order-independent. Declared media types and file
// names are claims about the bytes, not the bytes, so they are not part
// of it (§17.2.1: changed bytes make a new version).
func fileSetDigest(files []file) string {
	type entry struct {
		Role      string                        `json:"role"`
		Hash      string                        `json:"hash"`
		Size      int64                         `json:"size"`
		Checksums []federation.ArchivalChecksum `json:"checksums"`
	}
	entries := make([]entry, 0, len(files))
	for _, f := range files {
		cs := slices.Clone(f.checksums)
		sort.Slice(cs, func(i, j int) bool { return cs[i].Algorithm < cs[j].Algorithm })
		entries = append(entries, entry{Role: f.role, Hash: f.hash, Size: f.size, Checksums: cs})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Role != entries[j].Role {
			return entries[i].Role < entries[j].Role
		}
		return entries[i].Hash < entries[j].Hash
	})
	b, _ := json.Marshal(entries)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- bind -------------------------------------------------------------------

const recordCols = `id, state, COALESCE(reason, ''), attempts, COALESCE(authorizing_grant_id, '00000000-0000-0000-0000-000000000000'::uuid)`

func scanRecord(row pgx.Row, extra ...any) (record, error) {
	var r record
	err := row.Scan(append([]any{&r.id, &r.state, &r.reason, &r.attempts, &r.grantID}, extra...)...)
	return r, err
}

// bind returns the record this announcement is decided by, writing a
// pending or rejected one when none exists. dup is true when the record
// was already there.
func (s *Service) bind(ctx context.Context, an *announced) (record, bool, error) {
	var rec record
	var dup bool
	err := pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		// The activity first, then the asset: every caller takes them in
		// this order, so two announcements cannot deadlock. The activity
		// lock serialises deliveries of one activity id whatever object
		// they name; the asset lock serialises announcements of one asset.
		for _, key := range []string{"activity " + an.peer.ID.String() + " " + an.env.ID,
			"asset " + an.peer.ID.String() + " " + an.env.Object} {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1574))`, key); err != nil {
				return err
			}
		}
		var same bool
		var storedSigner string
		r, err := scanRecord(tx.QueryRow(ctx, `SELECT `+recordCols+`,
			    (actor_uri = $3 AND origin_asset_uri = $4 AND version_uri = $5 AND version_number = $6
			     AND file_set_sha256 = $7 AND sensitivity = $8 AND record = $9::jsonb),
			    signer_key_id
			  FROM federation_received_assets
			 WHERE peer_id = $1 AND activity_uri = $2`,
			an.peer.ID, an.env.ID, an.env.Actor, an.env.Object, an.asset.Version.ID, an.asset.Version.Number,
			an.digest, an.asset.Sensitivity, []byte(an.raw)),
			&same, &storedSigner)
		if err == nil {
			// A redelivery of an activity is the same announcement: same
			// actor, object, version, file set, tier and every aa:asset
			// claim (jsonb equality, so member order and whitespace do
			// not matter), fetched as the same signer. Anything else
			// under that id is refused, and the stored record and its
			// pins are left as they are.
			if !same {
				return &Error{Reason: ReasonActivityConflict, Err: errors.New("the activity id was recorded with another payload")}
			}
			if storedSigner != an.signer.KeyID {
				return &Error{Reason: casfetch.ReasonForeignSigner, Err: errors.New("the activity was recorded for another signer")}
			}
			rec, dup = r, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var liveVersion, liveDigest string
		r, err = scanRecord(tx.QueryRow(ctx, `SELECT `+recordCols+` FROM federation_received_assets
			WHERE peer_id = $1 AND origin_asset_uri = $2 AND state <> 'rejected'`, an.peer.ID, an.env.Object))
		switch {
		case err == nil:
			if err := tx.QueryRow(ctx, `SELECT version_uri, file_set_sha256 FROM federation_received_assets WHERE id = $1`,
				r.id).Scan(&liveVersion, &liveDigest); err != nil {
				return err
			}
			if liveVersion == an.asset.Version.ID && liveDigest == an.digest {
				rec, dup = r, true
				return nil
			}
			reason := ReasonVersionUnsupported
			if liveVersion == an.asset.Version.ID {
				reason = ReasonVersionConflict
			}
			rec, err = s.insert(ctx, tx, an, StateRejected, reason)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		switch {
		case an.asset.Sensitivity != "public" && an.asset.Sensitivity != "team":
			rec, err = s.insert(ctx, tx, an, StateRejected, casfetch.ReasonSensitivity)
			return err
		case an.total > s.cfg.MaxTotalBytes:
			rec, err = s.insert(ctx, tx, an, StateRejected, casfetch.ReasonTooLarge)
			return err
		}
		if _, err := s.d.Grants.AllowsTx(ctx, tx, s.grantQuery(an)); err != nil {
			if !errors.Is(err, inboundgrants.ErrNoGrant) {
				return err
			}
			rec, err = s.insert(ctx, tx, an, StateRejected, casfetch.ReasonNoInboundGrant)
			return err
		}
		rec, err = s.insert(ctx, tx, an, StatePending, "")
		return err
	})
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return record{}, false, e
		}
		return record{}, false, &Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: err}
	}
	return rec, dup, nil
}

func (s *Service) grantQuery(an *announced) inboundgrants.Query {
	return inboundgrants.Query{PeerID: an.peer.ID, ActorURI: an.signer.ActorURI, ObjectURI: an.env.Object,
		Class: inboundgrants.ClassOriginal}
}

// insert writes the record and its file set.
func (s *Service) insert(ctx context.Context, tx pgx.Tx, an *announced, state, reason string) (record, error) {
	var reasonArg, actorArg *string
	if reason != "" {
		reasonArg = &reason
	}
	if an.signer.ActorURI != "" {
		actorArg = &an.signer.ActorURI
	}
	r, err := scanRecord(tx.QueryRow(ctx, `INSERT INTO federation_received_assets
		(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
		 file_set_sha256, record, sensitivity, signer_key_id, signer_actor_uri, state, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING `+recordCols,
		an.peer.ID, an.env.ID, an.env.Actor, an.env.Object, an.originAssetID, an.asset.Version.ID,
		an.asset.Version.Number, an.digest, []byte(an.raw), an.asset.Sensitivity, an.signer.KeyID, actorArg,
		state, reasonArg))
	if err != nil {
		return record{}, err
	}
	for _, f := range an.files {
		cs, err := json.Marshal(f.checksums)
		if err != nil {
			return record{}, err
		}
		fstate := "pending"
		if f.role != "original" {
			fstate = "not_transferred"
		}
		var name *string
		if f.filename != "" {
			name = &f.filename
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type,
			 declared_original_filename, state)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			r.id, f.pos, f.role, f.hash, f.size, cs, f.mediaType, name, fstate); err != nil {
			return record{}, err
		}
	}
	return r, nil
}

// --- process ----------------------------------------------------------------

// heldPin is an ingest pin an attempt obtained.
type heldPin struct {
	pin  storage.PinRef
	hash string
	size int64
}

var errNotPending = errors.New("assetingest: the record is no longer pending")

// errOtherSigner: Receive's delivery is not the signer the record was
// bound for; the record is left to its own signer.
var errOtherSigner = errors.New("assetingest: not the record's signer")

// SignerResolver returns the local identity a record's files are fetched
// as, from the key id and actor URI it was bound with ("" for the
// instance). An error rejects the record (foreign_signer): nothing is
// fetched as anyone else.
type SignerResolver func(keyID, actorURI string) (casfetch.Signer, error)

// stored is a pending record as bound, the source of every later attempt.
type stored struct {
	ref                 recordRef
	actorURI, digest    string
	record              []byte
	signerKey, signerAs string
}

// announcement rebuilds the announcement the record was bound from.
func (st stored) announcement(signer casfetch.Signer) Announcement {
	return Announcement{PeerID: st.ref.peerID, Signer: signer, Envelope: &federation.Envelope{
		Context: federation.ContextV1, Type: federation.ActivityAAAssetVersion, ID: st.ref.activityURI,
		Actor: st.actorURI, Object: st.ref.originAssetURI,
		Extra: map[string]json.RawMessage{federation.MemberArchivalAsset: st.record},
	}}
}

// Process runs one attempt for a pending record (step 3 and 4 of the
// package header): the Worker's entry. The attempt starts from the
// record as bound, never from a later delivery, and checks everything
// again first: the peer row and its profiles, the actor and object
// origins, the aa:asset member, the signer, and the live inbound grant
// (no_inbound_grant, nothing asked of the origin). A record that is no
// longer pending is returned as it is.
func (s *Service) Process(ctx context.Context, id uuid.UUID, signers SignerResolver) (Outcome, error) {
	return s.process(ctx, id, signers)
}

func (s *Service) process(ctx context.Context, id uuid.UUID, signers SignerResolver) (Outcome, error) {
	var st stored
	err := s.d.Pool.QueryRow(ctx, `SELECT peer_id, activity_uri, actor_uri, origin_asset_uri, version_uri,
		    file_set_sha256, record, signer_key_id, COALESCE(signer_actor_uri, '')
		  FROM federation_received_assets WHERE id = $1 AND state = 'pending'`, id).
		Scan(&st.ref.peerID, &st.ref.activityURI, &st.actorURI, &st.ref.originAssetURI, &st.ref.versionURI,
			&st.digest, &st.record, &st.signerKey, &st.signerAs)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.current(ctx, id)
	}
	if err != nil {
		return Outcome{RecordID: id, State: StatePending}, &Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: err}
	}
	signer, serr := signers(st.signerKey, st.signerAs)
	if errors.Is(serr, errOtherSigner) {
		return s.current(ctx, id)
	}
	// The attempt holds the record for a lease, so no worker starts it
	// meanwhile; the Worker heartbeat keeps the lease while it runs.
	var attempts int32
	err = s.d.Pool.QueryRow(ctx, `UPDATE federation_received_assets
		SET attempts = attempts + 1, next_attempt_at = now() + make_interval(secs => $2), updated_at = now()
		WHERE id = $1 AND state = 'pending' RETURNING attempts`, id, s.cfg.Lease.Seconds()).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.current(ctx, id)
	}
	if err != nil {
		return Outcome{RecordID: id, State: StatePending}, &Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: err}
	}
	if serr != nil {
		return s.fail(ctx, st.ref, id, attempts, casfetch.ReasonForeignSigner, false, serr)
	}
	an, err := s.validate(ctx, st.announcement(signer))
	if err != nil {
		reason := ReasonOf(err)
		return s.fail(ctx, st.ref, id, attempts, reason, reason == casfetch.ReasonPeerUnavailable, err)
	}
	if an.digest != st.digest {
		return s.fail(ctx, st.ref, id, attempts, ReasonArchivalInvalid, false, errors.New("the stored file set does not match its digest"))
	}
	if _, err := s.d.Grants.Allows(ctx, s.grantQuery(an)); err != nil {
		if errors.Is(err, inboundgrants.ErrNoGrant) {
			return s.fail(ctx, st.ref, id, attempts, casfetch.ReasonNoInboundGrant, false, err)
		}
		return s.fail(ctx, st.ref, id, attempts, casfetch.ReasonStorage, true, err)
	}
	return s.transfer(ctx, an, id, attempts)
}

// transfer fetches every original of a started attempt and completes the
// record.
func (s *Service) transfer(ctx context.Context, an *announced, id uuid.UUID, attempts int32) (Outcome, error) {
	var held []heldPin
	release := func() {
		for _, h := range held {
			if err := s.d.Storage.RemovePin(context.WithoutCancel(ctx), h.pin, h.hash); err != nil && s.d.Logger != nil {
				s.d.Logger.WarnContext(ctx, "assetingest: release ingest pin", "hash", h.hash, "error", err)
			}
		}
	}
	for _, f := range an.originals {
		res, err := s.d.Fetcher.Fetch(ctx, casfetch.Request{
			PeerID: an.peer.ID, OriginAssetURI: an.env.Object, AssetID: an.originAssetID,
			SHA256: f.hash, Size: f.size, ContentType: f.mediaType, Checksums: extraChecksums(f.checksums),
			AnnouncedSensitivity: an.asset.Sensitivity, Signer: an.signer,
		})
		if err != nil {
			release()
			return s.fail(ctx, an.ref(), id, attempts, casfetch.ReasonOf(err), retryableFetch(err), err)
		}
		held = append(held, heldPin{pin: res.Pin, hash: res.Hash, size: res.Size})
	}
	if s.afterFetch != nil {
		s.afterFetch()
	}
	grantID, err := s.complete(ctx, an, id, held)
	release() // the ingest pins: removed in the commit, or released here
	if err != nil {
		var e *Error
		switch {
		case errors.Is(err, errNotPending):
			return s.current(ctx, id)
		case errors.As(err, &e):
			return s.fail(ctx, an.ref(), id, attempts, e.Reason, e.Retryable, err)
		}
		return s.fail(ctx, an.ref(), id, attempts, casfetch.ReasonStorage, true, err)
	}
	s.auditRecord(ctx, audit.EventFederationAssetIngestCompleted, an.ref(), id, "", map[string]any{
		"grant_id": grantID.String(), "files": len(an.originals)})
	return Outcome{RecordID: id, State: StateComplete, GrantID: grantID}, nil
}

func extraChecksums(cs []federation.ArchivalChecksum) []federation.ArchivalChecksum {
	var out []federation.ArchivalChecksum
	for _, c := range cs {
		if c.Algorithm != "sha256" {
			out = append(out, c)
		}
	}
	return out
}

// retryableFetch is true for a casfetch failure a later attempt may not
// repeat: the transfer did not finish, rather than being refused or
// failing verification.
func retryableFetch(err error) bool {
	var e *casfetch.Error
	if !errors.As(err, &e) {
		return true
	}
	switch e.Reason {
	case casfetch.ReasonTimeout, casfetch.ReasonTransport, casfetch.ReasonLeaseLost,
		casfetch.ReasonStorage, casfetch.ReasonPeerUnavailable:
		return true
	case casfetch.ReasonHTTPStatus:
		return e.Status >= 500 || e.Status == 429
	}
	return false
}

// complete records the held originals under the record in one
// transaction, after checking the record, the peer and the grant again.
func (s *Service) complete(ctx context.Context, an *announced, id uuid.UUID, held []heldPin) (uuid.UUID, error) {
	var grantID uuid.UUID
	err := pgx.BeginFunc(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM federation_received_assets WHERE id = $1 FOR UPDATE`, id).Scan(&state); err != nil {
			return err
		}
		if state != StatePending {
			return errNotPending
		}
		// The receiver's peer record, as it is now (§8.5: re-read as the
		// bytes are recorded).
		var instanceURL, status string
		var enabled bool
		var caps []string
		if err := tx.QueryRow(ctx, `SELECT instance_url, enabled, status,
			    ARRAY(SELECT jsonb_array_elements_text(capabilities))
			  FROM federation_peers WHERE id = $1 FOR SHARE`, an.peer.ID).Scan(&instanceURL, &enabled, &status, &caps); err != nil {
			return &Error{Reason: casfetch.ReasonRevokedDuringFetch, Err: fmt.Errorf("peer: %w", err)}
		}
		if !enabled || status != "connected" || !profilesInEffect(caps) ||
			federation.OriginOf(instanceURL) != federation.OriginOf(an.peer.InstanceURL) {
			return &Error{Reason: casfetch.ReasonRevokedDuringFetch, Err: errors.New("the peer was disabled, disconnected, downgraded or moved")}
		}
		g, err := s.d.Grants.AllowsTx(ctx, tx, s.grantQuery(an))
		if err != nil {
			if errors.Is(err, inboundgrants.ErrNoGrant) {
				return &Error{Reason: casfetch.ReasonRevokedDuringFetch, Err: err}
			}
			return err
		}
		grantID = g
		pin := storage.PinRef{SubjectType: PinSubjectType, SubjectID: id.String()}
		q := storage.New(tx)
		byHash := map[string]heldPin{}
		for _, h := range held {
			byHash[h.hash] = h
		}
		for _, f := range an.originals { // hash order: concurrent commits lock objects alike
			h, ok := byHash[f.hash]
			if !ok || h.size != f.size {
				return &Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: fmt.Errorf("original %s was not held", f.hash)}
			}
			// Lock the object, which must still exist at this size, and
			// take it off any pending GC, before changing its pins.
			tag, err := tx.Exec(ctx, `UPDATE storage_objects SET gc_eligible_at = NULL
				WHERE hash = $1 AND size_bytes = $2`, f.hash, f.size)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return &Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: fmt.Errorf("original %s is no longer stored", f.hash)}
			}
			if err := q.AddPin(ctx, storage.AddPinParams{ObjectHash: f.hash, PinSubjectType: pin.SubjectType, PinSubjectID: pin.SubjectID}); err != nil {
				return err
			}
			if err := q.RemovePin(ctx, storage.RemovePinParams{ObjectHash: f.hash, PinSubjectType: h.pin.SubjectType, PinSubjectID: h.pin.SubjectID}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_received_asset_files
			SET state = 'verified', verified_at = now()
			WHERE received_asset_id = $1 AND role = 'original'`, id); err != nil {
			return err
		}
		if s.inComplete != nil {
			if err := s.inComplete(ctx, tx); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE federation_received_assets
			SET state = 'complete', reason = NULL, completed_at = now(), authorizing_grant_id = $2, updated_at = now()
			WHERE id = $1`, id, grantID)
		return err
	})
	return grantID, err
}

// fail records a failed attempt: pending (retryable, attempts left),
// due again after retryDelay, or rejected. An attempt whose own context
// ended (the process is stopping) decides nothing: the record stays
// pending and is due again at once, for the next process. The attempt's
// ingest pins are already released.
func (s *Service) fail(ctx context.Context, ref recordRef, id uuid.UUID, attempts int32, reason string, retryable bool, cause error) (Outcome, error) {
	if reason == "" {
		reason = casfetch.ReasonStorage
	}
	stopping := ctx.Err() != nil
	state := StateRejected
	if stopping || (retryable && attempts < s.cfg.MaxAttempts) {
		state = StatePending
	}
	delay := s.retryDelay(attempts)
	if stopping {
		delay = 0
	}
	tag, err := s.d.Pool.Exec(context.WithoutCancel(ctx), `UPDATE federation_received_assets
		SET state = $2, reason = $3, next_attempt_at = now() + make_interval(secs => $4), updated_at = now()
		WHERE id = $1 AND state = 'pending'`, id, state, reason, delay.Seconds())
	if err != nil {
		return Outcome{RecordID: id, State: StatePending, Reason: reason},
			&Error{Reason: reason, Retryable: true, Err: errors.Join(cause, err)}
	}
	if tag.RowsAffected() == 0 {
		return s.current(ctx, id)
	}
	event := audit.EventFederationAssetIngestRejected
	if state == StatePending {
		event = audit.EventFederationAssetIngestDeferred
	}
	s.auditRecord(ctx, event, ref, id, reason, map[string]any{"attempts": attempts, "error": cause.Error()})
	return Outcome{RecordID: id, State: state, Reason: reason}, &Error{Reason: reason, Retryable: state == StatePending, Err: cause}
}

// current returns the stored outcome of a record another attempt
// decided.
func (s *Service) current(ctx context.Context, id uuid.UUID) (Outcome, error) {
	r, err := scanRecord(s.d.Pool.QueryRow(ctx, `SELECT `+recordCols+` FROM federation_received_assets WHERE id = $1`, id))
	if err != nil {
		return Outcome{RecordID: id}, &Error{Reason: casfetch.ReasonStorage, Retryable: true, Err: err}
	}
	out := Outcome{RecordID: r.id, State: r.state, Reason: r.reason, Duplicate: true, GrantID: r.grantID}
	switch r.state {
	case StateComplete:
		return out, nil
	case StateRejected:
		return out, &Error{Reason: r.reason}
	}
	return out, &Error{Reason: r.reason, Retryable: true}
}

// recordRef identifies a record in its audit events.
type recordRef struct {
	peerID                                  uuid.UUID
	activityURI, originAssetURI, versionURI string
}

func (an *announced) ref() recordRef {
	return recordRef{peerID: an.peer.ID, activityURI: an.env.ID, originAssetURI: an.env.Object, versionURI: an.asset.Version.ID}
}

func (s *Service) auditRecord(ctx context.Context, event string, ref recordRef, id uuid.UUID, reason string, extra map[string]any) {
	meta := map[string]any{
		"peer_id": ref.peerID.String(), "record_id": id.String(), "activity_uri": ref.activityURI,
		"origin_asset_uri": ref.originAssetURI, "version_uri": ref.versionURI,
	}
	if reason != "" {
		meta["reason"] = reason
	}
	for k, v := range extra {
		meta[k] = v
	}
	s.audit(ctx, event, meta)
}

func (s *Service) audit(ctx context.Context, event string, meta map[string]any) {
	if s.d.Audit != nil {
		s.d.Audit.FederationAssetIngest(context.WithoutCancel(ctx), event, meta)
	}
	if s.d.Logger != nil {
		s.d.Logger.InfoContext(ctx, event, "reason", meta["reason"], "record_id", meta["record_id"])
	}
}
