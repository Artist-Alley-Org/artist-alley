// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"context"
	"errors"
)

// EntityType identifies which searchable entity a Predicate gates.
// Small enum — every new addition needs a case in [Filter] +
// [Predicate.ToSQL].
type EntityType int

const (
	EntityAsset EntityType = iota
	EntityCollection
	EntityPost
)

// String returns the canonical name used in logs + admin panels.
func (e EntityType) String() string {
	switch e {
	case EntityAsset:
		return "asset"
	case EntityCollection:
		return "collection"
	case EntityPost:
		return "post"
	}
	return "unknown"
}

// AnonymousCaller is the sentinel user_ref for unauthenticated
// requests. Chosen to be zero so a nil *int64 doesn't accidentally
// masquerade as user 0. The actual DB never assigns ref=0.
const AnonymousCaller int64 = 0

// Caller is the parties whose visibility we're computing. Kept as a
// tiny struct rather than reusing auth.Identity so this package
// stays dependency-free of the auth package's larger surface.
type Caller struct {
	// UserRef is the authenticated caller's ref, or AnonymousCaller
	// for unauthed requests.
	UserRef int64

	// IsAnonymous is true when UserRef == AnonymousCaller. Kept as
	// an explicit boolean so callers don't ambiguate "user 0 who
	// doesn't exist" vs "anonymous".
	IsAnonymous bool
}

// NewCaller builds a Caller from an optional user ref. Nil ref =
// anonymous. Non-nil ref = authenticated.
func NewCaller(userRef *int64) Caller {
	if userRef == nil {
		return Caller{UserRef: AnonymousCaller, IsAnonymous: true}
	}
	return Caller{UserRef: *userRef, IsAnonymous: false}
}

// Predicate carries the caller's effective visibility set for one
// entity type and renders to a SQL WHERE-fragment. Constructed by
// [Filter]; consumed by query builders across the search subsystem.
type Predicate struct {
	entity EntityType
	caller Caller
	// includeSoftDeleted omits the soft-delete conjunct, and ONLY that
	// conjunct — see [IncludeSoftDeleted].
	includeSoftDeleted bool
	// postCaps is the caller's resolved post capabilities, read by the
	// EntityPost branch alone — see [WithPostCaps]. Zero value admits
	// nothing, which is the correct default for anonymous and the safe
	// default for a caller that forgets to pass it.
	postCaps PostCaps
}

// Option customises a Predicate at construction.
type Option func(*Predicate)

// IncludeSoftDeleted drops the predicate's `deleted_at IS NULL`
// conjunct, handing responsibility for the soft-delete dimension to the
// caller. Every other dimension — publication status, sensitivity,
// processing state, ownership, ACL grants — still applies.
//
// It exists for one escape hatch: the asset browse endpoint's
// superadmin-only `include_deleted` flag (#429). That flag means "also
// show me deleted rows", not "skip authorization", and this option is
// deliberately narrow so it cannot come to mean the latter.
//
// The narrowness is the security property. If the caller's own
// admin gate ever regresses, an attacker reaches a predicate that has
// lost only its soft-delete clause rather than one that was skipped
// entirely — an anonymous caller would still be held to
// status/sensitivity/processing. It also stays correct when the
// authenticated sensitivity rule lands (#210): a bypass-the-whole-
// predicate design would silently bypass that new rule too, which is a
// leak pre-installed today for a rule that does not exist yet.
func IncludeSoftDeleted() Option {
	return func(p *Predicate) { p.includeSoftDeleted = true }
}

// Filter constructs a [Predicate] for the given entity type + caller.
// Errors are reserved for future entity types that need DB lookups
// (team membership, follower graph, etc.); today all predicates
// are constant-shape per entity + caller so this never returns a
// non-nil error. The context param is here for the future.
func Filter(ctx context.Context, entityType EntityType, caller Caller, opts ...Option) (Predicate, error) {
	if entityType < EntityAsset || entityType > EntityPost {
		return Predicate{}, ErrUnknownEntityType
	}
	_ = ctx // reserved for future per-entity lookups
	p := Predicate{entity: entityType, caller: caller}
	for _, opt := range opts {
		opt(&p)
	}
	return p, nil
}

// Entity returns the entity type this Predicate gates. Exposed for
// callers that want to log or metric the effective type.
func (p Predicate) Entity() EntityType { return p.entity }

// Caller returns the caller this Predicate was built for. Exposed
// so downstream cache keys can hash the caller ref alongside the
// entity type + query.
func (p Predicate) Caller() Caller { return p.caller }

// ErrUnknownEntityType is returned by [Filter] for an out-of-range
// EntityType. Callers should treat this as an internal error
// (indicates programmer bug, not a runtime condition).
var ErrUnknownEntityType = errors.New("visibility: unknown entity type")
