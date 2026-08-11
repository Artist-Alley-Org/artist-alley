// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package acls holds the rules shared by every per-resource ACL surface
// (ADR 0010 Layer 6): post_acls, collection_acls and asset_type_acls.
//
// Right now that is principal validation. `principal_id` is a single
// TEXT column serving three different kinds of reference, so "is this
// value meaningful?" is a question every ACL write has to ask and none
// of them could answer from the column type alone.
package acls

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
)

// Principal types. The DB CHECK constraint on all three *_acls tables
// admits exactly these.
const (
	PrincipalUser = "user"
	PrincipalRole = "role"
	PrincipalTeam = "team"
)

// ErrPrincipalInert reports a grant that would be written but could
// never be matched, because the surface's read rule does not consult
// that principal type. Distinguished from a malformed reference so
// callers can phrase the two differently.
// Its text carries no package prefix because these errors are returned
// to API callers verbatim as the 400 body.
var ErrPrincipalInert = errors.New("principal type confers no access on this surface")

// ValidatePrincipalRef checks that principalID is a well-formed
// reference OF THE KIND principalType implies:
//
//   - user       → a users.ref, i.e. a positive BIGINT in decimal
//   - role, team → the row's UUID
//
// principal_id is TEXT because one column has to hold all three, and
// TEXT accepts anything — so nothing downstream ever discovered that a
// value was not a reference at all. Every read rule compares this
// column against a ref cast to text (`$n::BIGINT::TEXT`) or against a
// set of UUIDs cast to text; a value of the wrong shape simply never
// matches. The write succeeds, the grant does nothing, and the only
// symptom is an access denial nobody can explain.
//
// So the shape is checked at the boundary, where it can still be
// reported to the caller (#916).
func ValidatePrincipalRef(principalType, principalID string) error {
	if principalID == "" {
		return errors.New("principal_id is required")
	}
	switch principalType {
	case PrincipalUser:
		ref, err := strconv.ParseInt(principalID, 10, 64)
		if err != nil {
			return fmt.Errorf(
				"principal_id must be a numeric user ref for principal_type=user, not %q "+
					"(a username is not a reference; look the user up first)", principalID)
		}
		if ref <= 0 {
			return fmt.Errorf("principal_id must be a positive user ref, got %d", ref)
		}
		return nil
	case PrincipalRole, PrincipalTeam:
		if _, err := uuid.Parse(principalID); err != nil {
			return fmt.Errorf(
				"principal_id must be a UUID for principal_type=%s, not %q",
				principalType, principalID)
		}
		return nil
	default:
		return fmt.Errorf("principal_type must be user|role|team, got %q", principalType)
	}
}

// ValidateContentPrincipal is ValidatePrincipalRef plus the rule that
// content ACLs — post_acls and collection_acls — honour `user` and
// nothing else.
//
// This is not a policy choice made here; it is what the read rules
// actually do. Both gate on `principal_type = 'user'` before they look
// at principal_id at all (visibility.PostLiveGrantSQL,
// visibility.collectionGrantSQL), because role and team scoping on
// content is ADR 0010 Layer 5 and unimplemented. A role or team grant
// on a post is therefore inert in exactly the way a username is inert:
// accepted, stored, and matched by nothing.
//
// asset_type_acls is the surface where all three DO work — its access
// queries resolve role and team membership properly — so it uses
// ValidatePrincipalRef directly and admits the full set.
func ValidateContentPrincipal(principalType, principalID string) error {
	switch principalType {
	case PrincipalRole, PrincipalTeam:
		return fmt.Errorf(
			"%w: principal_type=%s is not honoured on posts or collections yet "+
				"(ADR 0010 Layer 5), so the grant would be stored and match nothing. "+
				"Grant to individual users instead",
			ErrPrincipalInert, principalType)
	}
	return ValidatePrincipalRef(principalType, principalID)
}
