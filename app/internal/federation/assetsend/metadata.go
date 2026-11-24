// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The sender's asset-metadata-1 snapshot (#1577, spec v1 §18.2, §18.3):
// capture and re-check of an asset's aa:metadata member.
//
// # Intentionally unwired
//
// Nothing calls these yet. No activity is recorded, nothing is queued and
// nothing is sent: this file is the source-privacy primitive a later
// metadata Update driver uses. It proves only that a member's sources are
// shareable with a peer under the peer emission policy of descriptive.go.
// It does not check a profile, a grant, the owner, the tier, a sealed
// version or an activity's identity and order; that driver must.
//
// # Capture
//
// CaptureMetadata reads the asset's sources inside the caller's
// transaction, which holds the asset row, through the same one statement
// and the same rules as an announcement's descriptive capture
// (captureDescriptive): the explicit, capability-free peer identity, the
// seeded definitions by id AND code, every mirror binding's gate, and the
// same value checks. The result is a complete snapshot of all four keys:
// a value that is absent, withheld or refused is null, with a bounded
// local reason that is never part of the wire member. Values are never
// trimmed, truncated, normalized or fetched.
//
// # The member budget
//
// An aa:metadata member is at most 32,768 bytes as serialized (§18.2), and
// the standard JSON encoding escapes <, > and & as six bytes each, so
// values valid one by one can exceed it together. The candidate is encoded
// with encoding/json, the same encoding a later envelope applies to it, and
// checked with federation.ParseAssetMetadata, the receiver's own rules. A
// candidate over the budget is not shortened: every claim it held is
// cleared to null at once, each with the local reason member_over_limit,
// and the four-null snapshot is captured instead. That is a conservative
// local choice for a snapshot that cannot be sent whole; it changes no
// wire rule. Any other failure of the final check is an error, never an
// empty or malformed capture.
//
// # Re-check
//
// Source.MetadataMember accepts only a recognized capture: one strict JSON
// value (nothing after it but whitespace, no repeated name at any depth),
// with exactly the lower-case keys v (1), snapshot and an optional object
// of reasons, whose snapshot, as recorded, is a valid complete member. It
// re-checks, in one statement, that the source of every non-null recorded
// claim is still shareable now. One that is not refuses the whole member
// (federation.ErrNotFederated, naming the claim, never a value); nothing
// else is substituted. A later value edit changes nothing: the recorded
// claims are sent as recorded, and a claim recorded as null stays null. A
// four-null capture names no source, so nothing is read for it; that says
// nothing about the asset, a grant, a profile or a tier, which the driver
// checks for a clear as for any other Update.
package assetsend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// metadataV is the version of a MetadataCapture. Another version is
// refused rather than guessed at.
const metadataV = 1

// omitMemberOverLimit: the snapshot as a whole was over the member budget,
// so this claim was cleared with every other one (file header).
const omitMemberOverLimit = "member_over_limit"

// MetadataCapture is an asset's captured aa:metadata snapshot, as a local
// record keeps it. Snapshot is the complete member; Omitted gives, for a
// null claim, the bounded local reason. Neither V nor Omitted is ever
// sent.
type MetadataCapture struct {
	V        int                              `json:"v"`
	Snapshot federation.AssetMetadataSnapshot `json:"snapshot"`
	Omitted  map[string]string                `json:"omitted,omitempty"`
}

// CaptureMetadata captures assetID's aa:metadata snapshot inside tx, which
// must hold the asset row. A missing asset is an error wrapping
// pgx.ErrNoRows; a read error is returned as it is. Neither is an empty
// capture.
func CaptureMetadata(ctx context.Context, tx pgx.Tx, assetID uuid.UUID) (*MetadataCapture, error) {
	d, err := captureDescriptive(ctx, tx, assetID)
	if err != nil {
		return nil, err
	}
	c := &MetadataCapture{V: metadataV, Omitted: maps.Clone(d.Omitted)}
	c.Snapshot = federation.AssetMetadataSnapshot{Title: d.Title, Description: d.Description, Rights: d.Rights}
	if d.RequiredStatement != nil {
		lv := requiredStatement(*d.RequiredStatement)
		c.Snapshot.RequiredStatement = &lv
	}
	raw, err := json.Marshal(c.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("assetsend: encode the metadata snapshot: %w", err)
	}
	if len(raw) > federation.AssetMetadataMaxMemberBytes {
		if c.Omitted == nil {
			c.Omitted = map[string]string{}
		}
		for member, held := range map[string]bool{
			memberTitle:             c.Snapshot.Title != nil,
			memberDescription:       c.Snapshot.Description != nil,
			memberRights:            c.Snapshot.Rights != nil,
			memberRequiredStatement: c.Snapshot.RequiredStatement != nil,
		} {
			if held {
				c.Omitted[member] = omitMemberOverLimit
			}
		}
		c.Snapshot = federation.AssetMetadataSnapshot{}
		if raw, err = json.Marshal(c.Snapshot); err != nil {
			return nil, fmt.Errorf("assetsend: encode the metadata snapshot: %w", err)
		}
	}
	if _, err := federation.ParseAssetMetadata(raw); err != nil {
		return nil, fmt.Errorf("assetsend: the captured metadata snapshot is not a valid member: %w", err)
	}
	if len(c.Omitted) == 0 {
		c.Omitted = nil
	}
	return c, nil
}

// recordedSnapshot is the snapshot of record, as recorded, when record is
// a recognized capture (file header). The canonicalizer is used only as
// the strict parser (one value, no repeated name at any depth); its output
// is discarded, so the snapshot's own bytes are what the member bound is
// measured on. The keys are compared exactly, never case-folded.
func recordedSnapshot(record json.RawMessage) (json.RawMessage, bool) {
	if _, err := federation.Canonicalize(record); err != nil {
		return nil, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(record, &top); err != nil {
		return nil, false
	}
	for k := range top {
		if k != "v" && k != "snapshot" && k != "omitted" {
			return nil, false
		}
	}
	var v int
	rawV, hasV := top["v"]
	snapshot, hasSnapshot := top["snapshot"]
	if !hasV || !hasSnapshot || json.Unmarshal(rawV, &v) != nil || v != metadataV {
		return nil, false
	}
	if raw, has := top["omitted"]; has {
		var reasons map[string]string
		if json.Unmarshal(raw, &reasons) != nil || reasons == nil {
			return nil, false
		}
	}
	return snapshot, true
}

// refusedMetadata is a refusal of an aa:metadata member, for good.
func refusedMetadata(format string, args ...any) error {
	return fmt.Errorf("%w: aa:metadata: %s", federation.ErrNotFederated, fmt.Sprintf(format, args...))
}

// MetadataMember is the aa:metadata member a recorded capture (record, the
// JSON of a MetadataCapture) sends for assetID now: exactly the recorded
// snapshot, all four keys, in the standard encoding. It is refused for
// good (federation.ErrNotFederated) when the record is not a valid capture
// of this version (file header) or, when a non-null claim needs its
// source re-checked, the asset is gone or that source is no longer
// shareable; a read error of that re-check is returned as it is. A
// four-null capture reads nothing. record is not modified.
func (s Source) MetadataMember(ctx context.Context, assetID uuid.UUID, record json.RawMessage) (json.RawMessage, error) {
	snapshot, ok := recordedSnapshot(record)
	if !ok {
		return nil, refusedMetadata("the recorded capture is not version %d", metadataV)
	}
	snap, err := federation.ParseAssetMetadata(snapshot)
	if err != nil {
		return nil, refusedMetadata("the recorded snapshot is not a valid member")
	}
	recorded := map[string]bool{
		memberTitle:             snap.Title != nil,
		memberDescription:       snap.Description != nil,
		memberRights:            snap.Rights != nil,
		memberRequiredStatement: snap.RequiredStatement != nil,
	}
	if recorded[memberTitle] || recorded[memberDescription] || recorded[memberRights] || recorded[memberRequiredStatement] {
		src, err := readSources(ctx, s.Pool, assetID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, refusedMetadata("the asset no longer exists")
			}
			return nil, fmt.Errorf("assetsend: re-check the metadata sources: %w", err)
		}
		for _, member := range []string{memberTitle, memberDescription, memberRights, memberRequiredStatement} {
			if recorded[member] && !src.shareable(member) {
				return nil, refusedMetadata("the recorded %s is no longer shareable with the peer", member)
			}
		}
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("assetsend: encode the metadata member: %w", err)
	}
	if _, err := federation.ParseAssetMetadata(raw); err != nil {
		return nil, refusedMetadata("the recorded snapshot is not a valid member")
	}
	return raw, nil
}
