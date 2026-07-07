package sysconfig

import (
	"context"
	"fmt"
)

// KeySoftDelete — system_config key for the soft-delete recovery-
// window knobs (Phase 1.55.C-1). Read/written via the Store's
// GetSoftDelete / SetSoftDelete methods.
//
// The GC coordinator (soft_delete.gc job) re-reads these knobs each
// tick, so operator changes at runtime take effect on the next
// nightly pass without a restart.
const KeySoftDelete = "soft_delete"

// SoftDeleteConfig captures the per-entity retention windows +
// coordinator cadence knob.
//
// Retention days is the delta between soft-delete and hard-delete-
// by-gc. A row soft-deleted today with a 30-day retention is
// eligible for gc-hard-delete 30 days from now (rounded down to the
// coordinator's next tick).
//
// Users get a longer default (90 days) than the other three
// entities (30 days) because user removal is the highest-blast-
// radius delete an operator can commit — cascades touch every
// asset/collection/post that user owns, plus federation actor state,
// plus session revocation. The longer window buys recovery time
// proportional to the consequence.
type SoftDeleteConfig struct {
	// AssetRetentionDays — soft-deleted assets past this age are
	// hard-deleted by gc. Range: 1..365. Default: 30.
	AssetRetentionDays int `json:"asset_retention_days"`

	// PostRetentionDays — soft-deleted posts past this age are
	// hard-deleted by gc. Range: 1..365. Default: 30.
	PostRetentionDays int `json:"post_retention_days"`

	// CollectionRetentionDays — soft-deleted collections past this
	// age are hard-deleted by gc. Range: 1..365. Default: 30.
	CollectionRetentionDays int `json:"collection_retention_days"`

	// UserRetentionDays — users in UserStateArchived past this age
	// are hard-deleted by gc. The state-machine's archived-at
	// timestamp anchors the window, not deleted_at (user has no
	// deleted_at column — the archived state IS the soft-delete).
	// Range: 1..365. Default: 90.
	UserRetentionDays int `json:"user_retention_days"`

	// GCHourUTC — hour (0-23) at which the gc coordinator wakes
	// each day. Default: 0 (midnight UTC). Operators pick their
	// low-traffic window here so the batch DELETE doesn't collide
	// with peak read traffic.
	GCHourUTC int `json:"gc_hour_utc"`
}

// SoftDeleteConfigDefaults returns the config with all defaults
// applied. Used by the Store when the key hasn't been written yet.
func SoftDeleteConfigDefaults() SoftDeleteConfig {
	return SoftDeleteConfig{
		AssetRetentionDays:      30,
		PostRetentionDays:       30,
		CollectionRetentionDays: 30,
		UserRetentionDays:       90,
		GCHourUTC:               0,
	}
}

// GetSoftDelete returns the soft-delete config or, if unset, the
// defaults. Errors only on transport / decode failure.
func (s *Store) GetSoftDelete(ctx context.Context) (SoftDeleteConfig, error) {
	out := SoftDeleteConfigDefaults()
	if err := s.getKey(ctx, KeySoftDelete, &out); err != nil {
		return SoftDeleteConfig{}, err
	}
	// Backfill any zero-value field that predates a schema addition.
	// Callers shouldn't have to reason about "did this operator save
	// the config before I added GCHourUTC?" — always yield sane
	// values.
	if out.AssetRetentionDays == 0 {
		out.AssetRetentionDays = 30
	}
	if out.PostRetentionDays == 0 {
		out.PostRetentionDays = 30
	}
	if out.CollectionRetentionDays == 0 {
		out.CollectionRetentionDays = 30
	}
	if out.UserRetentionDays == 0 {
		out.UserRetentionDays = 90
	}
	return out, nil
}

// SetSoftDelete validates and writes the soft-delete config.
func (s *Store) SetSoftDelete(ctx context.Context, v SoftDeleteConfig) error {
	if err := validateRetention("asset_retention_days", v.AssetRetentionDays); err != nil {
		return err
	}
	if err := validateRetention("post_retention_days", v.PostRetentionDays); err != nil {
		return err
	}
	if err := validateRetention("collection_retention_days", v.CollectionRetentionDays); err != nil {
		return err
	}
	if err := validateRetention("user_retention_days", v.UserRetentionDays); err != nil {
		return err
	}
	if v.GCHourUTC < 0 || v.GCHourUTC > 23 {
		return fmt.Errorf("sysconfig: soft_delete.gc_hour_utc: out of range (0-23)")
	}
	return s.setKey(ctx, KeySoftDelete, v)
}

func validateRetention(field string, v int) error {
	if v < 1 || v > 365 {
		return fmt.Errorf("sysconfig: soft_delete.%s: out of range (1-365)", field)
	}
	return nil
}
