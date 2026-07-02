package saved

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Row is the exported, plain-Go projection of one saved_search
// row. Distinct from the sqlc-generated SavedSearch so the HTTP +
// job layers don't drag pgtype into their imports.
type Row struct {
	ID                    uuid.UUID
	OwnerUserRef          int64
	Name                  string
	DSL                   string
	NotifyChannel         string
	NotifyIntervalMinutes int
	Enabled               bool
	LastResultHash        *string
	LastResultIDs         []uuid.UUID
	LastRunAt             *time.Time
	LastNotifiedAt        *time.Time
	OriginServerID        *uuid.UUID
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// CreateParams is the input to [Store.Create]. The owner is
// caller-supplied (from auth.Identity at the HTTP layer);
// name/dsl are user-supplied + validated. Intervals below the
// sysconfig floor are floored on write.
type CreateParams struct {
	OwnerUserRef          int64
	Name                  string
	DSL                   string
	NotifyChannel         string
	NotifyIntervalMinutes int
}

// UpdateParams is the input to [Store.Update]. Every field is a
// pointer so PATCH semantics work — nil = leave unchanged.
type UpdateParams struct {
	Name                  *string
	DSL                   *string
	NotifyChannel         *string
	NotifyIntervalMinutes *int
	Enabled               *bool
}

// NotifyChannel enum values. String-typed so JSON round-trips
// without a translation layer.
const (
	NotifyChannelEmail = "email"
	NotifyChannelNone  = "none"
)

// ValidNotifyChannel reports whether s is one of the two
// supported channel values.
func ValidNotifyChannel(s string) bool {
	return s == NotifyChannelEmail || s == NotifyChannelNone
}

// RunResult is what [Executor.Run] produces. The notifier
// computes delta + fires the email; the coordinator persists the
// fresh state.
type RunResult struct {
	// HitIDs is the sorted (ascending) set of asset UUIDs from
	// the current run. Sorted so hash computation is
	// deterministic.
	HitIDs []uuid.UUID

	// Hash is the sha256 hex of HitIDs joined by ",". Compared
	// to the row's LastResultHash to detect delta.
	Hash string

	// HitsMeta is a per-hit projection carrying title + summary +
	// permalink. Populated only when Executor is configured to
	// enrich; the notifier needs it for the email digest, the
	// coordinator's health-only path skips it to save work.
	HitsMeta []HitMeta
}

// HitMeta is one hit projected to the fields the digest email
// needs. Deliberately narrow — the digest never surfaces score,
// vector similarity, or provenance.
type HitMeta struct {
	ID    uuid.UUID
	Title string
	// Summary is the entity's short-form body, already truncated
	// by the Engine's hit projection.
	Summary string
}

// Delta describes the transition between the previous stored run
// and the current one.
type Delta struct {
	Added        []uuid.UUID
	Removed      []uuid.UUID
	Unchanged    int
	HashChanged  bool
}

// Sentinels — HTTP layer maps to appropriate status codes.
var (
	// ErrNotFound is returned by [Store.Get] / [Store.Update] /
	// [Store.Delete] when the target ID doesn't exist.
	ErrNotFound = errors.New("saved: not found")

	// ErrNameConflict is returned by [Store.Create] when the
	// (owner_user_ref, name) uniqueness constraint fires.
	ErrNameConflict = errors.New("saved: name already used by this owner")

	// ErrMaxPerUser is returned by [Store.Create] when the caller
	// already owns >= sysconfig.search.saved_search.max_per_user
	// ENABLED rows.
	ErrMaxPerUser = errors.New("saved: caller has reached the maximum saved-search count")

	// ErrIntervalTooSmall is returned when a caller supplies
	// notify_interval_minutes below the sysconfig floor.
	ErrIntervalTooSmall = errors.New("saved: notify_interval_minutes is below the sysconfig floor")

	// ErrInvalidNotifyChannel is returned for unrecognised
	// channel values.
	ErrInvalidNotifyChannel = errors.New("saved: unknown notify_channel")
)
