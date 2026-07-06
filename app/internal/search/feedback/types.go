package feedback

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Direction of a feedback vote. Enum-typed so the HTTP layer + DB
// CHECK constraint stay in sync.
type Direction string

const (
	DirectionUp   Direction = "up"
	DirectionDown Direction = "down"
)

// ValidDirection reports whether s is one of the two supported
// wire-form values.
func ValidDirection(s string) bool {
	switch Direction(s) {
	case DirectionUp, DirectionDown:
		return true
	}
	return false
}

// Row is the plain-Go projection of one search_feedback row.
type Row struct {
	ID          uuid.UUID
	QueryHash   string
	DSLQuery    string
	HitAssetID  uuid.UUID
	HitPosition int32
	Direction   Direction
	UserRef     int64
	IPHash      *string
	FeedbackAt  time.Time
}

// SubmitParams is the operator-facing input to Service.Submit.
type SubmitParams struct {
	UserRef     int64
	DSL         string
	HitAssetID  uuid.UUID
	HitPosition int32
	Direction   Direction
	IPHash      string
}

// SubmitResult carries the post-write state back to the caller so
// the HTTP handler can render the correct thumb state.
type SubmitResult struct {
	ID        uuid.UUID
	Direction Direction
	Flipped   bool // true if the ON CONFLICT UPDATE path fired
}

// TopQueryRow is a row in the "queries with most down-votes" admin
// aggregation.
type TopQueryRow struct {
	QueryHash    string
	DSLQuery     string
	TotalVotes   int64
	DownVotes    int64
	DownVotePct  float32
}

// UnderRankedHitRow is a row in the "under-ranked hits" admin
// aggregation — a hit that was thumbs-upped from a deep position,
// which suggests the ranker buried a good result.
type UnderRankedHitRow struct {
	HitAssetID uuid.UUID
	QueryHash  string
	DSLQuery   string
	AvgPos     float64
	UpVotes    int64
	AssetTitle string
}

// PerUserRow is a row in the abuse-review page's per-user log.
type PerUserRow struct {
	Row
	AssetTitle string
}

// Sentinels — HTTP layer maps to appropriate status codes.
var (
	// ErrRateLimited is returned by Submit when the user has already
	// hit sysconfig search.feedback.max_per_user_per_day within the
	// rolling 24h window. HTTP maps to 429.
	ErrRateLimited = errors.New("feedback: per-user daily cap exceeded")

	// ErrNotFound is returned by Get / DeleteOwn when the row doesn't
	// exist or the caller isn't the owner. HTTP maps to 404. Deliberate
	// enumeration-safe conflation — a non-owner probing IDs shouldn't
	// be able to distinguish "your row" from "someone else's row".
	ErrNotFound = errors.New("feedback: row not found")

	// ErrHitNotVisible fires when the visibility filter rejects the
	// hit_asset_id for the caller. HTTP maps to 403. Kept distinct
	// from ErrNotFound so the frontend can render a helpful "you
	// can't see this asset anymore" message when a share was revoked
	// mid-vote.
	ErrHitNotVisible = errors.New("feedback: hit not visible to caller")

	// ErrDisabled is returned everywhere when the sysconfig kill
	// switch is off. HTTP maps to 503.
	ErrDisabled = errors.New("feedback: subsystem disabled")
)
