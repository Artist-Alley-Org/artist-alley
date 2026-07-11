// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"fmt"
)

// Phase 1.18.A-2 follow-up A — typed accessors for the
// upload.* sysconfig keys seeded by migration 00015. These cover
// the dedup posture (scope + behavior) that the upload handler
// reads on every CreateAsset call.

const (
	KeyUploadDedupScope    = "upload.dedup_scope"
	KeyUploadDedupBehavior = "upload.dedup_behavior"
)

// DedupScope selects the population the upload handler checks
// for an existing asset with the same content hash.
//
//   - DedupScopePerUser (default): only the uploading user's
//     own assets. Backed by the partial unique index from
//     migration 00016.
//   - DedupScopePerTeam: every asset owned by the uploading
//     user's owning team. Application-level check; no DB
//     constraint (team membership is mutable).
//   - DedupScopeGlobal: every visible asset across the
//     instance. Application-level check; visibility filter
//     prevents leaking restricted assets.
//   - DedupScopeOff: skip the lookup entirely.
type DedupScope string

const (
	DedupScopePerUser DedupScope = "per_user"
	DedupScopePerTeam DedupScope = "per_team"
	DedupScopeGlobal  DedupScope = "global"
	DedupScopeOff     DedupScope = "off"
)

// ValidDedupScope reports whether s is one of the four supported values.
func ValidDedupScope(s DedupScope) bool {
	switch s {
	case DedupScopePerUser, DedupScopePerTeam, DedupScopeGlobal, DedupScopeOff:
		return true
	}
	return false
}

// DedupBehavior controls what the upload handler does when the
// scope query finds an existing asset.
//
//   - DedupBehaviorWarn (default): create the new asset row but
//     also return a `duplicate_warning` field pointing at the
//     existing asset id; UI surfaces a dialog.
//   - DedupBehaviorBlock: refuse the upload with 409; existing
//     asset id in the response so UI can navigate.
//   - DedupBehaviorAllow: dedup lookup is skipped entirely;
//     upload always succeeds without warnings.
type DedupBehavior string

const (
	DedupBehaviorWarn  DedupBehavior = "warn"
	DedupBehaviorBlock DedupBehavior = "block"
	DedupBehaviorAllow DedupBehavior = "allow"
)

// ValidDedupBehavior reports whether b is one of the three
// supported values.
func ValidDedupBehavior(b DedupBehavior) bool {
	switch b {
	case DedupBehaviorWarn, DedupBehaviorBlock, DedupBehaviorAllow:
		return true
	}
	return false
}

// UploadConfig is the typed projection of the upload.* sysconfig
// keys. Both fields default to the conservative-but-visible pair
// (per_user + warn) when their underlying keys are absent —
// matches the operator-safe behaviour migration 00015 seeded.
type UploadConfig struct {
	DedupScope    DedupScope
	DedupBehavior DedupBehavior
}

// GetUpload returns the current upload-config projection. Missing
// keys fall back to the per_user / warn defaults; invalid persisted
// values fall back to defaults + are NOT corrected on the read path
// (admin write path validates on write, so an invalid persisted
// value indicates manual DB tampering or a pre-migration row).
func (s *Store) GetUpload(ctx context.Context) (UploadConfig, error) {
	out := UploadConfig{
		DedupScope:    DedupScopePerUser,
		DedupBehavior: DedupBehaviorWarn,
	}
	var scope string
	if err := s.getKey(ctx, KeyUploadDedupScope, &scope); err == nil {
		if ValidDedupScope(DedupScope(scope)) {
			out.DedupScope = DedupScope(scope)
		}
	}
	var behavior string
	if err := s.getKey(ctx, KeyUploadDedupBehavior, &behavior); err == nil {
		if ValidDedupBehavior(DedupBehavior(behavior)) {
			out.DedupBehavior = DedupBehavior(behavior)
		}
	}
	return out, nil
}

// SetUpload validates + persists the upload config. Both fields
// are required-on-write; partial-update at this level would risk
// silently leaving the other field at a stale value.
func (s *Store) SetUpload(ctx context.Context, v UploadConfig) error {
	if !ValidDedupScope(v.DedupScope) {
		return fmt.Errorf("sysconfig: invalid upload.dedup_scope %q", v.DedupScope)
	}
	if !ValidDedupBehavior(v.DedupBehavior) {
		return fmt.Errorf("sysconfig: invalid upload.dedup_behavior %q", v.DedupBehavior)
	}
	if err := s.setKey(ctx, KeyUploadDedupScope, string(v.DedupScope)); err != nil {
		return err
	}
	return s.setKey(ctx, KeyUploadDedupBehavior, string(v.DedupBehavior))
}
