package feedback

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ConfigProvider is the seam for reading the operator-facing knobs
// out of sysconfig. Injected so tests can pass a fixed config
// without a real store.
type ConfigProvider interface {
	Get(ctx context.Context) (Config, error)
}

// Config is the resolved-with-defaults feedback subsystem knobs.
// Mirrors sysconfig.FeedbackConfig shape so the boot-side adapter
// is trivial.
type Config struct {
	Enabled               bool
	MaxPerUserPerDay      int
	AggregationWindowDays int
}

// AggregationWindow returns the window duration (days as time.Duration).
func (c Config) AggregationWindow() time.Duration {
	return time.Duration(c.AggregationWindowDays) * 24 * time.Hour
}

// VisibilityChecker is the caller-provided seam for the B-2
// visibility filter. Returns true if `userRef` can see `assetID`.
// The Service refuses feedback on invisible hits — prevents an
// attacker from probing UUID existence via feedback submits.
type VisibilityChecker interface {
	CanSee(ctx context.Context, userRef int64, assetID uuid.UUID) (bool, error)
}

// Counter is the observability hook — nil-safe. Each Submit /
// Delete increments the corresponding search.Counter Result class.
type Counter interface {
	RecordFeedback(direction string) // "up" | "down"
	RecordFeedbackUndo()
	RecordFeedbackRateLimited()
	RecordFeedbackDisabled()
}

// Service is the caller-facing feedback API. Composed of the Store
// + the small seams the HTTP handler needs.
type Service struct {
	Store      *Store
	Cfg        ConfigProvider
	Visibility VisibilityChecker
	Counter    Counter
}

// NewService constructs a Service.
func NewService(store *Store, cfg ConfigProvider, vis VisibilityChecker, counter Counter) *Service {
	return &Service{Store: store, Cfg: cfg, Visibility: vis, Counter: counter}
}

// Submit records a thumbs up/down. Enforces:
//
//   - Feedback subsystem enabled (else ErrDisabled)
//   - Visibility floor (else ErrHitNotVisible)
//   - Per-user daily cap (else ErrRateLimited)
//
// Idempotent on the (user_ref, hit_asset_id, query_hash) unique
// constraint: a re-vote flips the direction rather than inserting
// a duplicate. Rate limit is checked BEFORE the upsert so a flip
// doesn't consume a token unnecessarily — the flip case is
// already-counted from the original vote.
func (s *Service) Submit(ctx context.Context, p SubmitParams) (SubmitResult, error) {
	cfg, err := s.Cfg.Get(ctx)
	if err != nil {
		return SubmitResult{}, err
	}
	if !cfg.Enabled {
		if s.Counter != nil {
			s.Counter.RecordFeedbackDisabled()
		}
		return SubmitResult{}, ErrDisabled
	}
	// Visibility gate.
	if s.Visibility != nil {
		ok, verr := s.Visibility.CanSee(ctx, p.UserRef, p.HitAssetID)
		if verr != nil {
			return SubmitResult{}, verr
		}
		if !ok {
			return SubmitResult{}, ErrHitNotVisible
		}
	}
	// Rate-limit gate — check first, upsert second. If the row
	// already exists (flip case), the previously-counted vote
	// stays in the count regardless of direction; a flip doesn't
	// double-count. The check here is a pre-flight; the flip case
	// still bumps the window's most-recent timestamp so the next
	// window boundary drifts, which is intentional (a user who
	// keeps flipping the same vote gets rate-limited by the flap).
	cutoff := time.Now().Add(-24 * time.Hour)
	count, err := s.Store.CountUserSince(ctx, p.UserRef, cutoff)
	if err != nil {
		return SubmitResult{}, err
	}
	if cfg.MaxPerUserPerDay > 0 && count >= int64(cfg.MaxPerUserPerDay) {
		if s.Counter != nil {
			s.Counter.RecordFeedbackRateLimited()
		}
		return SubmitResult{}, ErrRateLimited
	}
	res, err := s.Store.Upsert(ctx, p)
	if err != nil {
		return SubmitResult{}, err
	}
	if s.Counter != nil {
		s.Counter.RecordFeedback(string(res.Direction))
	}
	return res, nil
}

// DeleteOwn removes a feedback row IFF the caller owns it. Undo does
// NOT count toward the daily cap — the DELETE lowers the live count,
// implicitly refunding one token to the user.
func (s *Service) DeleteOwn(ctx context.Context, id uuid.UUID, userRef int64) error {
	cfg, err := s.Cfg.Get(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		if s.Counter != nil {
			s.Counter.RecordFeedbackDisabled()
		}
		return ErrDisabled
	}
	if err := s.Store.DeleteOwn(ctx, id, userRef); err != nil {
		return err
	}
	if s.Counter != nil {
		s.Counter.RecordFeedbackUndo()
	}
	return nil
}

// TopQueriesByDownvote surfaces the admin aggregation view.
func (s *Service) TopQueriesByDownvote(ctx context.Context, limit int32) ([]TopQueryRow, error) {
	cfg, err := s.Cfg.Get(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return []TopQueryRow{}, nil
	}
	return s.Store.TopQueriesByDownvote(ctx, cfg.AggregationWindow(), limit)
}

// UnderRankedHits surfaces the admin aggregation view.
func (s *Service) UnderRankedHits(ctx context.Context, minPosition int32, limit int32) ([]UnderRankedHitRow, error) {
	cfg, err := s.Cfg.Get(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return []UnderRankedHitRow{}, nil
	}
	return s.Store.UnderRankedHits(ctx, cfg.AggregationWindow(), minPosition, limit)
}

// ListForUser surfaces the abuse-review view.
func (s *Service) ListForUser(ctx context.Context, userRef int64, limit int32) ([]PerUserRow, error) {
	return s.Store.ListForUser(ctx, userRef, limit)
}

// ActiveVoters returns the DISTINCT user count in the aggregation
// window. Fed into the health gauge from the boot-side callback.
func (s *Service) ActiveVoters(ctx context.Context) (int64, error) {
	cfg, err := s.Cfg.Get(ctx)
	if err != nil {
		return 0, err
	}
	return s.Store.ActiveVoters(ctx, cfg.AggregationWindow())
}
