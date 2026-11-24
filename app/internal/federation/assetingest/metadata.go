// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The receiver's storage of asset-metadata-1 snapshots (#1577, spec v1
// §18.4), kept with the Update that carried them.
//
// # What this is, and is not
//
// The domain half of a metadata Update: ApplyFlagUpdate (update.go)
// admits an Update carrying aa:metadata and keeps its flags revision and
// its snapshot together, in one transaction; CurrentMetadata derives the
// asset's current snapshot from what is kept. The inbox admits
// aa:metadata (federation.ValidateMembers) and routes the Update here
// through Worker.UpdateAssetFlags; the profile is not advertised, so no
// production peer reaches it. The received-asset detail API
// (receivedassets) shows the current snapshot under its own rules; no
// page reads one yet. Its caller must already have
// verified the envelope's signature by its actor (the inbox does that
// before any handler); nothing here fetches or checks a key.
//
// # Admission (in addition to an Update's own rules)
//
//   - federation.ValidateAssetMetadataMembers with the peer's stored
//     profiles, then federation.ParseAssetMetadata, before anything is
//     written: a malformed member is metadata_invalid, a missing profile
//     profile_not_negotiated, and nothing is kept (no flags either);
//   - the envelope's declared signature has the actor's #main-key and a
//     64-byte Ed25519 value (invalid_request otherwise); it is kept as the
//     FIRST accepted origin signature, and a re-signed duplicate never
//     replaces it. It is kept apart from the recipient identity the grant
//     names (the local fetch signer), which is this instance's, not the
//     origin's;
//   - under the activity's, the asset's and the peer's grant locks, the
//     peer row is read again FOR SHARE: still enabled, connected, on the
//     same origin, with all three profiles;
//   - the duplicate check also compares the snapshot (as jsonb, so member
//     order and whitespace do not matter) and the published time to the
//     nanosecond; metadata added to, or missing from, an id already kept
//     is activity_conflict.
//
// # The current snapshot
//
// Among the snapshots kept for (peer, asset), the greatest by published
// (seconds, then nanoseconds), then activity URI compared byte by byte,
// whose revision names a version held here, with the same file set, in
// a verified state (complete or superseded). A snapshot that names a
// version not held yet, pending, rejected or withdrawn is kept but not
// current; once that version completes, it is. No tombstone may be kept
// for the asset. Nothing is stored about which snapshot is current, and
// the files a record serves are never chosen by a snapshot.
package assetingest

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
)

// metadataUpdate is a validated aa:metadata member and the provenance
// kept with it.
type metadataUpdate struct {
	snapshot  []byte // the validated snapshot, marshalled with all four keys
	seconds   int64
	nanos     int32
	published string // RFC 3339, UTC, to the nanosecond
	// The origin's signature as declared on the envelope, which the caller
	// verified.
	originKeyID     string
	originAlgorithm string
	originValue     string
	// This instance's identity the grant names (the local fetch signer).
	recipientKeyID string
}

// validateMetadata checks an Update's aa:metadata member and its
// provenance, writing nothing. It returns nil, nil when the Update
// carries no aa:metadata.
func (s *Service) validateMetadata(an *announced) (*metadataUpdate, error) {
	raw, ok := an.env.Extra[federation.MemberAssetMetadata]
	if !ok {
		return nil, nil
	}
	mc := federation.MemberContext{Negotiated: func(token string) bool { return slices.Contains(an.peer.Capabilities, token) }}
	if err := federation.ValidateAssetMetadataMembers(an.env, mc); err != nil {
		return nil, &Error{Reason: metadataReason(err), Err: err}
	}
	snap, err := federation.ParseAssetMetadata(raw)
	if err != nil {
		return nil, &Error{Reason: metadataReason(err), Err: err}
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return nil, &Error{Reason: ReasonMetadataInvalid, Err: errors.New("the snapshot cannot be kept")}
	}
	return withProvenance(an, b, "a metadata Update")
}

// withProvenance is snapshot with the provenance kept beside it: the
// envelope's signed published to the nanosecond, its declared origin
// signature (the actor's #main-key, a 64-byte Ed25519 value; the caller
// verified it), and the local recipient identity. what names the activity
// in a refusal.
func withProvenance(an *announced, snapshot []byte, what string) (*metadataUpdate, error) {
	sig := an.env.Signature
	if sig == nil || sig.Type != federation.SignatureAlgEd25519 || sig.PublicKey != an.env.Actor+"#main-key" {
		return nil, &Error{Reason: casfetch.ReasonInvalidRequest, Err: errors.New(what + " needs its actor's Ed25519 signature")}
	}
	if v, err := base64.RawURLEncoding.DecodeString(sig.Value); err != nil || len(v) != ed25519.SignatureSize {
		return nil, &Error{Reason: casfetch.ReasonInvalidRequest, Err: errors.New("the signature value is not 64 base64url bytes")}
	}
	p := an.env.Published
	if p.IsZero() {
		return nil, &Error{Reason: casfetch.ReasonInvalidRequest, Err: errors.New(what + " needs its published time")}
	}
	return &metadataUpdate{
		snapshot: snapshot, seconds: p.Unix(), nanos: int32(p.Nanosecond()), published: p.UTC().Format(time.RFC3339Nano),
		originKeyID: sig.PublicKey, originAlgorithm: string(sig.Type), originValue: sig.Value,
		recipientKeyID: an.signer.KeyID,
	}, nil
}

// initialCapture is an aa:AssetVersion's initial snapshot (spec v1 §18.4),
// prepared from the original delivery before the binding transaction: the
// snapshot and its provenance, or why none can be kept. It is used only if
// that transaction writes a new revision for the activity; a delivery of an
// activity already kept (and Process, which rebuilds the envelope without
// its signature or published) never captures one.
type initialCapture struct {
	meta *metadataUpdate
	err  error
}

// initialCandidate prepares the initial snapshot of an announcement whose
// peer negotiated asset-metadata-1 with archival-1 and original-transfer-1,
// or returns nil (a legacy announcement, unchanged).
//
// The snapshot is derived from the aa:asset member archival-1 already
// validated: title and description from description, rights,
// requiredStatement; an absent or empty claim is null, and nothing else is
// copied. It is not an aa:metadata member, so §18.2's member bounds (the
// raw 32,768 bytes, the 2,048-byte rights URI) do not apply: a claim
// archival-1 accepted is kept as accepted, never dropped or trimmed.
func initialCandidate(an *announced) *initialCapture {
	for _, token := range []string{federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1} {
		if !slices.Contains(an.peer.Capabilities, token) {
			return nil
		}
	}
	var snap federation.AssetMetadataSnapshot
	if d := an.asset.Description; d != nil {
		if d.Title != "" {
			snap.Title = &d.Title
		}
		if d.Description != "" {
			snap.Description = &d.Description
		}
	}
	if an.asset.Rights != "" {
		snap.Rights = &an.asset.Rights
	}
	snap.RequiredStatement = an.asset.RequiredStatement
	b, err := json.Marshal(snap) // a copy: later changes to the member cannot reach it
	if err != nil {
		return &initialCapture{err: &Error{Reason: ReasonArchivalInvalid, Err: errors.New("the baseline cannot be kept")}}
	}
	m, err := withProvenance(an, b, "an announcement under asset-metadata-1")
	return &initialCapture{meta: m, err: err}
}

// keepRevision keeps an announcement's revision (recordRevision) and,
// when init is set, its initial snapshot on the revision this INSERT
// wrote. bind clears init for an activity whose revision is already kept
// (a historical delivery, never given a snapshot), so with init set an
// INSERT that wrote no row is refused: the admission and its record roll
// back rather than be kept without their snapshot.
func keepRevision(ctx context.Context, tx pgx.Tx, an *announced, init *initialCapture) error {
	inserted, err := recordRevision(ctx, tx, an)
	if err != nil || init == nil {
		return err
	}
	if !inserted {
		return &Error{Reason: ReasonActivityConflict, Err: errors.New("the revision for the activity was not written by this delivery")}
	}
	return keepInitial(ctx, tx, an, init)
}

// keptAnnouncementRevision reports whether an aa:AssetVersion revision is
// kept for an's activity. When it carries an initial snapshot, the
// delivery must be the same activity: actor, object, version, file set,
// every aa:asset claim and the published time to the nanosecond
// (activity_conflict otherwise), for the same local recipient
// (foreign_signer). A revision kept without one is left as it is.
func keptAnnouncementRevision(ctx context.Context, tx pgx.Tx, an *announced) (bool, error) {
	var same, hasSnapshot bool
	var recipient string
	p := an.env.Published
	err := tx.QueryRow(ctx, `SELECT (r.actor_uri = $3 AND r.origin_asset_uri = $4 AND r.version_uri = $5
		        AND r.file_set_sha256 = $6 AND r.record = $7::jsonb
		        AND COALESCE(m.published_seconds = $8 AND m.published_nanos = $9, TRUE)),
		        m.revision_id IS NOT NULL, COALESCE(m.recipient_signer_key_id, '')
		  FROM federation_received_asset_revisions r
		  LEFT JOIN federation_received_asset_metadata m ON m.revision_id = r.id
		 WHERE r.peer_id = $1 AND r.activity_uri = $2 AND r.activity_type = 'aa:AssetVersion'`,
		an.peer.ID, an.env.ID, an.env.Actor, an.env.Object, an.asset.Version.ID, an.digest, []byte(an.raw),
		p.Unix(), int32(p.Nanosecond())).Scan(&same, &hasSnapshot, &recipient)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	case !hasSnapshot:
		return true, nil
	case !same:
		return true, &Error{Reason: ReasonActivityConflict, Err: errors.New("the activity id was recorded with another payload")}
	case recipient != an.signer.KeyID:
		return true, &Error{Reason: casfetch.ReasonForeignSigner, Err: errors.New("the activity was recorded for another signer")}
	}
	return true, nil
}

// keepInitial keeps a new revision's initial snapshot, inside the binding
// transaction, after the caller re-read the peer under the locks.
func keepInitial(ctx context.Context, tx pgx.Tx, an *announced, c *initialCapture) error {
	if c.err != nil {
		return c.err
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM federation_received_asset_revisions WHERE peer_id = $1 AND activity_uri = $2`,
		an.peer.ID, an.env.ID).Scan(&id); err != nil {
		return err
	}
	return insertMetadata(ctx, tx, id, c.meta)
}

// metadataReason maps a pure-validator error to this package's reason.
// A profile member's own failures are metadata_invalid; a missing
// profile, or an invalid companion aa:asset, keep their existing
// reasons; anything else about the envelope is invalid_request.
func metadataReason(err error) string {
	switch {
	case errors.Is(err, federation.ErrMetadataInvalid):
		return ReasonMetadataInvalid
	case errors.Is(err, federation.ErrProfileNotNegotiated):
		return casfetch.ReasonProfile
	case errors.Is(err, federation.ErrArchivalInvalid):
		return ReasonArchivalInvalid
	default:
		return casfetch.ReasonInvalidRequest
	}
}

// recheckMetadataPeer reads the peer row again, FOR SHARE, inside the
// admitting transaction: a metadata Update is kept only while the peer is
// enabled, connected, on the same origin and still negotiates every
// profile it needs.
func recheckMetadataPeer(ctx context.Context, tx pgx.Tx, an *announced) error {
	var instanceURL, status string
	var enabled bool
	var caps []string
	if err := tx.QueryRow(ctx, `SELECT instance_url, enabled, status,
		    ARRAY(SELECT jsonb_array_elements_text(capabilities))
		  FROM federation_peers WHERE id = $1 FOR SHARE`, an.peer.ID).Scan(&instanceURL, &enabled, &status, &caps); err != nil {
		return &Error{Reason: casfetch.ReasonPeerUnavailable, Err: fmt.Errorf("peer: %w", err)}
	}
	if !enabled || status != "connected" || federation.OriginOf(instanceURL) != federation.OriginOf(an.peer.InstanceURL) {
		return &Error{Reason: casfetch.ReasonPeerUnavailable, Err: errors.New("the peer was disabled, disconnected or moved")}
	}
	for _, token := range []string{federation.ProfileAssetMetadata1, federation.ProfileArchival1, federation.ProfileOriginalTransfer1} {
		if !slices.Contains(caps, token) {
			return &Error{Reason: casfetch.ReasonProfile, Err: fmt.Errorf("%w: %s is no longer negotiated", federation.ErrProfileNotNegotiated, token)}
		}
	}
	return nil
}

// insertMetadata keeps m with the revision it belongs to, inside the
// admitting transaction. It never skips a row already there: the caller
// established, under the activity's lock, that none is.
func insertMetadata(ctx context.Context, tx pgx.Tx, revisionID uuid.UUID, m *metadataUpdate) error {
	_, err := tx.Exec(ctx, `INSERT INTO federation_received_asset_metadata
		(revision_id, profile_token, snapshot, published_seconds, published_nanos, published_text,
		 origin_signer_key_id, origin_signature_algorithm, origin_signature_value, recipient_signer_key_id)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10)`,
		revisionID, federation.ProfileAssetMetadata1, m.snapshot, m.seconds, m.nanos, m.published,
		m.originKeyID, m.originAlgorithm, m.originValue, m.recipientKeyID)
	return err
}

// MetadataRevision is a kept snapshot and its provenance.
type MetadataRevision struct {
	RevisionID  uuid.UUID
	PeerID      uuid.UUID
	AssetURI    string
	ActivityURI string
	ActorURI    string
	Snapshot    federation.AssetMetadataSnapshot
	// Published is the activity's signed published, to the nanosecond
	// (PublishedSeconds and PublishedNanos as kept).
	Published        time.Time
	PublishedSeconds int64
	PublishedNanos   int32
	VersionURI       string
	FileSetSHA256    string
	// The first accepted origin signature, as declared on the envelope.
	OriginSignerKeyID        string
	OriginSignatureAlgorithm string
	OriginSignatureValue     string
	// This instance's identity the grant named when the snapshot was kept.
	RecipientSignerKeyID string
	ReceivedAt           time.Time
}

// CurrentMetadata returns the asset's current snapshot from peerID (the
// file header), or nil when none is current. A snapshot of four nulls is
// returned as one: it clears every claim, which is not the same as no
// snapshot.
//
// It is a storage primitive, not a permission: it says nothing about who
// may read the snapshot, and it returns kept history whatever the peer's
// or the grant's state now. A caller that shows it applies the record's,
// the peer's and the grant's rules first, as the received-asset detail
// does (receivedassets, #1577).
func (s *Service) CurrentMetadata(ctx context.Context, peerID uuid.UUID, assetURI string) (*MetadataRevision, error) {
	var m MetadataRevision
	var snap []byte
	err := s.d.Pool.QueryRow(ctx, `SELECT r.id, r.peer_id, r.origin_asset_uri, r.activity_uri, r.actor_uri,
		    m.snapshot, m.published_seconds, m.published_nanos, r.version_uri, r.file_set_sha256,
		    m.origin_signer_key_id, m.origin_signature_algorithm, m.origin_signature_value,
		    m.recipient_signer_key_id, m.received_at
		  FROM federation_received_asset_metadata m
		  JOIN federation_received_asset_revisions r ON r.id = m.revision_id
		 WHERE r.peer_id = $1 AND r.origin_asset_uri = $2
		   AND EXISTS (SELECT 1 FROM federation_received_assets a
		                WHERE a.peer_id = r.peer_id AND a.origin_asset_uri = r.origin_asset_uri
		                  AND a.version_uri = r.version_uri AND a.file_set_sha256 = r.file_set_sha256
		                  AND a.state IN ('`+StateComplete+`', '`+StateSuperseded+`'))
		   AND NOT EXISTS (SELECT 1 FROM federation_received_tombstones t
		                    WHERE t.peer_id = r.peer_id AND t.origin_asset_uri = r.origin_asset_uri)
		 ORDER BY m.published_seconds DESC, m.published_nanos DESC, r.activity_uri COLLATE "C" DESC
		 LIMIT 1`, peerID, assetURI).Scan(&m.RevisionID, &m.PeerID, &m.AssetURI, &m.ActivityURI, &m.ActorURI,
		&snap, &m.PublishedSeconds, &m.PublishedNanos, &m.VersionURI, &m.FileSetSHA256,
		&m.OriginSignerKeyID, &m.OriginSignatureAlgorithm, &m.OriginSignatureValue, &m.RecipientSignerKeyID, &m.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(snap, &m.Snapshot); err != nil {
		return nil, fmt.Errorf("assetingest: a kept snapshot: %w", err)
	}
	m.Published = time.Unix(m.PublishedSeconds, int64(m.PublishedNanos)).UTC()
	return &m, nil
}
