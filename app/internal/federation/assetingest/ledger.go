// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetingest

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation/casfetch"
)

// The transfer and fixity event ledger (#1574 slice 2, migration 00089):
// federation_transfer_events, one row per event, written in the SAME
// transaction as the record's state change. Each row snapshots the peer,
// the record and the file as they are when it is written, and has no
// foreign key, so it outlives the record and the peer.

// AgentName is the ledger's agent_name for the receiver core.
const AgentName = "artist-alley assetingest"

// Ledger event types and outcomes (federation_transfer_events CHECKs).
const (
	EventIngestion   = "ingestion"
	EventTransfer    = "transfer"
	EventFixityCheck = "fixity-check"

	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeWarning = "warning"
)

// ledgerFile is the file an event concerns.
type ledgerFile struct {
	pos       int
	role      string
	hash      string
	size      int64
	mediaType string
}

func fileOf(f file) *ledgerFile {
	return &ledgerFile{pos: f.pos, role: f.role, hash: f.hash, size: f.size, mediaType: f.mediaType}
}

// event is one ledger row to write.
type event struct {
	typ, outcome, reason string
	file                 *ledgerFile
	observed             *casfetch.Observation
	detail               string
}

// maxDetail bounds the stored detail (federation_transfer_events_detail_check).
const maxDetail = 2000

// boundDetail makes an error text storable in a text column: valid UTF-8
// (an invalid byte sequence, or a NUL, which PostgreSQL text refuses,
// becomes U+FFFD) and at most maxDetail bytes, cut at a rune boundary so
// the kept prefix is whole. A detail must never be the reason a state
// change and its event fail to commit.
func boundDetail(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(s, "\x00", "\uFFFD")
	if len(s) <= maxDetail {
		return s
	}
	n := maxDetail
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// writeEvents inserts evs for record id inside tx, taking the peer and
// record snapshots from their rows as they are in tx.
func (s *Service) writeEvents(ctx context.Context, tx pgx.Tx, id uuid.UUID, attempt int32, stateAfter string, evs ...event) error {
	var agentVersion *string
	if s.d.AgentVersion != "" {
		agentVersion = &s.d.AgentVersion
	}
	for _, e := range evs {
		var reason, detail *string
		if e.reason != "" {
			reason = &e.reason
		}
		if e.detail != "" {
			d := boundDetail(e.detail)
			detail = &d
		}
		var pos *int
		var role, hash, mediaType *string
		var size *int64
		if e.file != nil {
			pos, role, hash, size, mediaType = &e.file.pos, &e.file.role, &e.file.hash, &e.file.size, &e.file.mediaType
		}
		var basis, obsSHA, mAlg, mExp, mObs *string
		var obsSize *int64
		truncated := false
		if o := e.observed; o != nil {
			basis = &o.Basis
			if o.SHA256 != "" {
				obsSHA = &o.SHA256
			}
			if o.HasSize {
				obsSize = &o.Size
			}
			truncated = o.Truncated
			if o.MismatchAlgorithm != "" {
				mAlg, mExp, mObs = &o.MismatchAlgorithm, &o.MismatchExpected, &o.MismatchObserved
			}
		}
		tag, err := tx.Exec(ctx, `INSERT INTO federation_transfer_events
			(peer_id, peer_instance_url, peer_display_name, received_asset_id, activity_uri, origin_asset_uri,
			 version_uri, version_number, sensitivity,
			 file_position, file_role, object_hash, declared_media_type,
			 event_type, outcome, reason, expected_sha256, expected_size_bytes,
			 observed_basis, observed_sha256, observed_size_bytes, observed_truncated,
			 mismatch_algorithm, mismatch_expected, mismatch_observed,
			 attempt, agent_name, agent_version, record_state_after, detail)
			SELECT r.peer_id, p.instance_url, p.display_name, r.id, r.activity_uri, r.origin_asset_uri,
			       r.version_uri, r.version_number, r.sensitivity,
			       $2, $3, $4, $5, $6, $7, $8, $4, $9, $10, $11, $12, $13, $14, $15, $16,
			       $17, $18, $19, $20, $21
			  FROM federation_received_assets r JOIN federation_peers p ON p.id = r.peer_id
			 WHERE r.id = $1`,
			id, pos, role, hash, mediaType, e.typ, e.outcome, reason, size,
			basis, obsSHA, obsSize, truncated, mAlg, mExp, mObs,
			attempt, AgentName, agentVersion, stateAfter, detail)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("assetingest: ledger: the record or its peer is gone")
		}
	}
	return nil
}

// eventTypeOf names the ledger event a failure with reason is.
func eventTypeOf(reason string, fetching bool) string {
	switch {
	case reason == casfetch.ReasonFixityMismatch || reason == casfetch.ReasonLengthMismatch:
		return EventFixityCheck
	case fetching:
		return EventTransfer
	}
	return EventIngestion
}
