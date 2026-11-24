// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Baseline descriptive and rights members of an aa:AssetVersion (#1577
// S1, spec v1 §17.2.5, §17.2.6): description.title and
// description.description from the asset's own columns, rights from the
// seeded rights_uri field and requiredStatement from the seeded
// required_statement field (migration 00097). No other descriptive member
// is sent: creator, subject, date, language, identifiers, accessionNumber
// and iiifManifest have no local source this slice maps, and credit or
// copyright text is not reinterpreted as any of them.
//
// # Peer emission policy
//
// A peer is not a local identity. It holds no capability, and it inherits
// nothing from the user who shared the asset, from an administrator or
// from any team: the share grants the asset to the peer, not anyone's
// field rights. So a member is emitted only from a source that every
// caller could read without any capability:
//
//   - title and description: the asset's columns, and only when NO field
//     definition that mirrors that column (whatever its code, status or
//     asset types) carries a read_capability. One gated binding hides the
//     column from the peer; it is never read past by going to the column.
//   - rights and requiredStatement: only from the row with the seeded id
//     AND the seeded code, while it is active, plain text, an asset field
//     that applies to this asset's type, mirrors no column and carries no
//     read_capability. No other field is ever used in its place.
//
// The asset-level checks (local, live, public or team, a live share
// granting this peer the original, the aa:Share delivered) are the
// existing ones, unchanged and evaluated as before: this policy only
// narrows what such an announcement says.
//
// Values are never trimmed, truncated, decoded or fetched. One the wire
// rules refuse (§17.2.5 rights allowlist, the §17.2.6 byte limits, control
// characters) is omitted, with a bounded reason code and never the value.
//
// # Snapshot and re-check
//
// The members, and the reason for each one omitted, are captured when the
// announcement is recorded (announce, inside the grant's or the seal's
// transaction, which holds the asset row), in ONE statement, so the values
// and every definition they were judged against come from one snapshot,
// and a title or description change, which updates that row, commits
// wholly before or after. They are kept in the announcement's local record
// as a versioned snapshot ("descriptive", v 1). Every delivery sends
// exactly those bytes: a later value edit, or a value set after an
// omission, never changes what this activity says. A record without the
// snapshot (one recorded before #1577) sends no descriptive member at all.
//
// Recorded bytes never override CURRENT privacy. Each delivery re-checks,
// in one statement, that every recorded member's source is still eligible
// under the policy above. If one is not (now gated, retired, removed,
// retyped, mirrored or no longer applicable), the whole activity is
// refused for good, through the outbox's normal refusal, and nothing of it
// is sent; no other member is substituted under the same activity id. The
// refusal names the member, never a value.
//
// The linearization point is that re-check statement: a restriction
// committed before it refuses the delivery; one committed after it is
// seen by the next delivery attempt, not this one, and nothing here can
// recall bytes a peer has already received (§17.8).
package assetsend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// The seeded definitions (migration 00097). Only these rows are mapped.
var (
	seedRightsURIID         = uuid.MustParse("56d71791-189d-429c-b7e4-e837baf91ddb")
	seedRequiredStatementID = uuid.MustParse("6c3e25be-83ed-4762-b636-a1e37cdbb74c")
)

const (
	seedRightsURICode         = "rights_uri"
	seedRequiredStatementCode = "required_statement"

	// descriptiveV is the version of the recorded snapshot. A record with
	// another version is refused rather than guessed at.
	descriptiveV = 1

	// requiredStatementLabel is the label every requiredStatement carries,
	// the same word the local IIIF manifest uses.
	requiredStatementLabel = "Attribution"
)

// The members a snapshot may hold.
const (
	memberTitle             = "title"
	memberDescription       = "description"
	memberRights            = "rights"
	memberRequiredStatement = "requiredStatement"
)

// Omission reasons: bounded codes, never values.
const (
	omitEmpty         = "empty"          // no value
	omitGated         = "gated"          // a read_capability applies
	omitUnmapped      = "unmapped"       // the seeded definition is absent (or another row holds its id or code)
	omitInactive      = "inactive"       // the seeded definition is deprecated or archived
	omitWrongType     = "wrong_type"     // not a plain-text asset field
	omitMirrored      = "mirrored"       // the seeded definition mirrors a column
	omitNotApplicable = "not_applicable" // the field does not apply to this asset's type
	omitInvalid       = "invalid"        // the wire rules refuse the value
	omitOverLimit     = "over_limit"     // longer than the wire limit; never truncated
)

// descriptiveSnapshot is the "descriptive" member of an announcement's
// local record. A pointer member that is nil was not captured; Omitted
// says why. Present members are exactly what every delivery sends.
type descriptiveSnapshot struct {
	V                 int               `json:"v"`
	Title             *string           `json:"title,omitempty"`
	Description       *string           `json:"description,omitempty"`
	Rights            *string           `json:"rights,omitempty"`
	RequiredStatement *string           `json:"required_statement,omitempty"`
	Omitted           map[string]string `json:"omitted,omitempty"`
}

// seededField is one seeded definition as one statement saw it.
type seededField struct {
	present     bool // a row with the seeded id AND the seeded code exists
	status      string
	typ         string
	subjectKind string
	mirrored    bool
	gated       bool
	applies     bool
}

// eligible is the reason the field cannot be a source, or "".
func (f seededField) eligible() string {
	switch {
	case !f.present:
		return omitUnmapped
	case f.status != "active":
		return omitInactive
	case f.typ != "text" || f.subjectKind != "asset":
		return omitWrongType
	case f.mirrored:
		return omitMirrored
	case f.gated:
		return omitGated
	case !f.applies:
		return omitNotApplicable
	}
	return ""
}

// sources is what one statement saw of an asset's descriptive sources.
type sources struct {
	titleGated, descriptionGated bool
	rights, statement            seededField
	// The values; only read at capture.
	title, description          string
	rightsValue, statementValue *string
}

// shareable reports whether member's source is eligible now under the
// peer emission policy (file header). Every re-check of a recorded member
// (an announcement's and an aa:metadata capture's) asks this.
func (s sources) shareable(member string) bool {
	switch member {
	case memberTitle:
		return !s.titleGated
	case memberDescription:
		return !s.descriptionGated
	case memberRights:
		return s.rights.eligible() == ""
	case memberRequiredStatement:
		return s.statement.eligible() == ""
	}
	return false
}

// sourceSQL reads, in one statement, the asset's type and columns, every
// mirror binding's gate, and the two seeded definitions with this asset's
// values of them. $1 is the asset, $2 / $3 the seeded rights id / code,
// $4 / $5 the seeded statement id / code.
const sourceSQL = `
	WITH a AS (SELECT id, asset_type, title, description FROM assets WHERE id = $1),
	     d AS (SELECT fd.id, fd.code, fd.status, fd.type, fd.subject_kind,
	                  fd.mirrors_column IS NOT NULL AS mirrored,
	                  COALESCE(fd.read_capability, '') <> '' AS gated,
	                  (cardinality(fd.applies_to) = 0 OR (SELECT asset_type FROM a) = ANY(fd.applies_to)) AS applies
	             FROM field_definition fd
	            WHERE (fd.id = $2 AND fd.code = $3) OR (fd.id = $4 AND fd.code = $5))
	SELECT a.title, a.description,
	       EXISTS (SELECT 1 FROM field_definition m WHERE m.mirrors_column = 'title' AND COALESCE(m.read_capability, '') <> ''),
	       EXISTS (SELECT 1 FROM field_definition m WHERE m.mirrors_column = 'description' AND COALESCE(m.read_capability, '') <> ''),
	       r.id IS NOT NULL, COALESCE(r.status, ''), COALESCE(r.type, ''), COALESCE(r.subject_kind, ''),
	       COALESCE(r.mirrored, FALSE), COALESCE(r.gated, FALSE), COALESCE(r.applies, FALSE),
	       (SELECT v.value_text FROM asset_field_value v WHERE v.asset_id = a.id AND v.field_id = r.id),
	       s.id IS NOT NULL, COALESCE(s.status, ''), COALESCE(s.type, ''), COALESCE(s.subject_kind, ''),
	       COALESCE(s.mirrored, FALSE), COALESCE(s.gated, FALSE), COALESCE(s.applies, FALSE),
	       (SELECT v.value_text FROM asset_field_value v WHERE v.asset_id = a.id AND v.field_id = s.id)
	  FROM a
	  LEFT JOIN d r ON r.id = $2
	  LEFT JOIN d s ON s.id = $4`

func readSources(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, assetID uuid.UUID) (sources, error) {
	var s sources
	err := q.QueryRow(ctx, sourceSQL, assetID, seedRightsURIID, seedRightsURICode, seedRequiredStatementID, seedRequiredStatementCode).
		Scan(&s.title, &s.description, &s.titleGated, &s.descriptionGated,
			&s.rights.present, &s.rights.status, &s.rights.typ, &s.rights.subjectKind,
			&s.rights.mirrored, &s.rights.gated, &s.rights.applies, &s.rightsValue,
			&s.statement.present, &s.statement.status, &s.statement.typ, &s.statement.subjectKind,
			&s.statement.mirrored, &s.statement.gated, &s.statement.applies, &s.statementValue)
	return s, err
}

// captureDescriptive builds the snapshot of assetID's descriptive members
// inside tx, which holds the asset row (announce). Any error fails the
// recording; nothing is recorded from a partial read.
func captureDescriptive(ctx context.Context, tx pgx.Tx, assetID uuid.UUID) (*descriptiveSnapshot, error) {
	src, err := readSources(ctx, tx, assetID)
	if err != nil {
		return nil, fmt.Errorf("assetsend: read the descriptive sources: %w", err)
	}
	snap := &descriptiveSnapshot{V: descriptiveV, Omitted: map[string]string{}}
	omit := func(member, reason string) { snap.Omitted[member] = reason }

	text := func(member, value string, gated bool, max int, check func(string) error) *string {
		switch {
		case gated:
			omit(member, omitGated)
		case value == "":
			omit(member, omitEmpty)
		case len(value) > max:
			omit(member, omitOverLimit)
		case check(value) != nil:
			omit(member, omitInvalid)
		default:
			v := value
			return &v
		}
		return nil
	}
	snap.Title = text(memberTitle, src.title, src.titleGated, federation.ArchivalMaxTitleBytes, func(v string) error {
		return federation.ValidateArchivalDescription(federation.ArchivalDescription{Title: v})
	})
	snap.Description = text(memberDescription, src.description, src.descriptionGated, federation.ArchivalMaxLongText, func(v string) error {
		return federation.ValidateArchivalDescription(federation.ArchivalDescription{Description: v})
	})

	field := func(member string, f seededField, value *string, max int, check func(string) error) *string {
		if reason := f.eligible(); reason != "" {
			omit(member, reason)
			return nil
		}
		v := ""
		if value != nil {
			v = *value
		}
		return text(member, v, false, max, check)
	}
	snap.Rights = field(memberRights, src.rights, src.rightsValue, 2048, func(v string) error {
		if !federation.ArchivalRightsURIValid(v) {
			return fmt.Errorf("not an allowed rights URI")
		}
		return nil
	})
	snap.RequiredStatement = field(memberRequiredStatement, src.statement, src.statementValue, federation.ArchivalMaxLongText,
		func(v string) error { return federation.ValidateArchivalRequiredStatement(requiredStatement(v)) })
	if len(snap.Omitted) == 0 {
		snap.Omitted = nil
	}
	return snap, nil
}

// requiredStatement is the wire shape of a recorded statement.
func requiredStatement(v string) federation.ArchivalLabelValue {
	return federation.ArchivalLabelValue{
		Label: map[string][]string{"en": {requiredStatementLabel}},
		Value: map[string][]string{"none": {v}},
	}
}

// descriptiveMembers are the members one delivery adds to aa:asset.
type descriptiveMembers struct {
	description       *federation.ArchivalDescription
	rights            string
	requiredStatement *federation.ArchivalLabelValue
}

// put adds the members to an aa:asset member map.
func (d *descriptiveMembers) put(m map[string]any) {
	if d == nil {
		return
	}
	if d.description != nil {
		m["description"] = d.description
	}
	if d.rights != "" {
		m["rights"] = d.rights
	}
	if d.requiredStatement != nil {
		m["requiredStatement"] = d.requiredStatement
	}
}

// materializeDescriptive turns an announcement's recorded snapshot into
// the members this delivery sends, after re-checking, in one statement,
// that every recorded member's source is still eligible (file header). raw
// is the record's "descriptive" member; nil (a record from before #1577)
// sends none. A member no longer eligible refuses the whole activity.
func (s Source) materializeDescriptive(ctx context.Context, assetID uuid.UUID, raw json.RawMessage) (*descriptiveMembers, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var snap descriptiveSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil || snap.V != descriptiveV {
		return nil, refused("the recorded descriptive snapshot is not version %d", descriptiveV)
	}
	if snap.Title == nil && snap.Description == nil && snap.Rights == nil && snap.RequiredStatement == nil {
		return nil, nil
	}
	src, err := readSources(ctx, s.Pool, assetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refused("the asset no longer exists")
		}
		return nil, fmt.Errorf("assetsend: re-check the descriptive sources: %w", err)
	}
	still := func(member string, ok bool) error {
		if !ok {
			return refused("the recorded %s is no longer shareable with the peer", member)
		}
		return nil
	}
	checks := []struct {
		member   string
		recorded bool
		ok       bool
	}{
		{memberTitle, snap.Title != nil, src.shareable(memberTitle)},
		{memberDescription, snap.Description != nil, src.shareable(memberDescription)},
		{memberRights, snap.Rights != nil, src.shareable(memberRights)},
		{memberRequiredStatement, snap.RequiredStatement != nil, src.shareable(memberRequiredStatement)},
	}
	for _, c := range checks {
		if c.recorded {
			if err := still(c.member, c.ok); err != nil {
				return nil, err
			}
		}
	}
	out := &descriptiveMembers{}
	if snap.Title != nil || snap.Description != nil {
		d := federation.ArchivalDescription{}
		if snap.Title != nil {
			d.Title = *snap.Title
		}
		if snap.Description != nil {
			d.Description = *snap.Description
		}
		out.description = &d
	}
	if snap.Rights != nil {
		out.rights = *snap.Rights
	}
	if snap.RequiredStatement != nil {
		lv := requiredStatement(*snap.RequiredStatement)
		out.requiredStatement = &lv
	}
	return out, nil
}
