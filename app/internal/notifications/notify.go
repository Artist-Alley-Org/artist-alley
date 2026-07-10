// Permission/preference-aware notification writer (Phase 1.17.I2).
//
// Notify(...) is the single entry point every emitter calls. Three
// gates apply before a row lands:
//
//   1. Actor != recipient — no self-notifications.
//   2. No active block edge between actor and recipient (consults
//      social.HasBlockBetween via a local interface so this package
//      doesn't grow a cross-package import cycle).
//   3. Recipient's userprefs.NotificationChannels for the verb
//      includes "in_app" — explicit empty list MEANS "mute,"
//      omitted key falls back to userprefs.SystemDefaultChannels.
//
// Federation: every row carries origin_server_id (NULL = local),
// and the per-recipient unread-count cache invalidation rides
// the cache.Registry NOTIFY broadcast so federated peers drop the
// stale count the moment a write commits anywhere in the cluster.

package notifications

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// blockChecker is the social-graph slice we need — kept local so
// this package doesn't import the social package directly (the
// social handler in turn calls Notify, which would be a cycle).
// Concrete implementation is *social.Handler, injected at boot.
type blockChecker interface {
	HasBlockBetween(ctx context.Context, a, b int64) (bool, error)
}

// prefsResolver is the slice of the userprefs handler we need.
// ChannelsFor returns the channel list ("in_app", "email", ...) the
// recipient wants the verb delivered through (empty = mute).
// CadenceFor returns the email cadence for the verb ("immediate" |
// "hourly" | "daily" | "weekly"); Phase 1.55.Y. Both ride the same
// cached prefs load.
type prefsResolver interface {
	ChannelsFor(ctx context.Context, ref int64, verb string) ([]string, error)
	CadenceFor(ctx context.Context, ref int64, verb string) (string, error)
}

// jobsEnqueuer is the email-channel hook — when the recipient's
// pref includes "email," Notify enqueues a `notification.email` job
// the worker pool processes asynchronously. nil-safe — when no
// enqueuer is wired, the email channel is silently skipped (Phase
// I2-a ships the in_app path; I2-b lands the job handler).
type jobsEnqueuer interface {
	Enqueue(ctx context.Context, kind string, payload []byte) error
}

// cacheDomainUnreadCount holds the per-user "unread notification
// count" the bell on every page render reads. Per-recipient key
// (the user's ref stringified). cache.Registry broadcasts the
// invalidation across federated peers.
const cacheDomainUnreadCount = "notifications.unread_count"

// Writer is the package's central state — wraps the DB pool +
// cross-package deps + the per-recipient unread cache. Created
// once at boot; every emitter calls Notify on the shared instance.
type Writer struct {
	pool     *pgxpool.Pool
	logger   *slog.Logger
	blocks   blockChecker
	prefs    prefsResolver
	jobs     jobsEnqueuer
	registry *cache.Registry

	unread *cache.Cache[int64]
}

// NewWriter wires the writer to its dependencies. blocks + prefs
// can be nil — in tests we exercise the writer in isolation; in
// production both are always wired. registry nil-safe (degrades to
// no caching, same shape every other handler in this codebase
// uses).
func NewWriter(pool *pgxpool.Pool, logger *slog.Logger, blocks blockChecker, prefs prefsResolver, jobs jobsEnqueuer, registry *cache.Registry) *Writer {
	w := &Writer{
		pool:     pool,
		logger:   logger,
		blocks:   blocks,
		prefs:    prefs,
		jobs:     jobs,
		registry: registry,
	}
	if registry != nil {
		// 10k entries covers the active-users-in-last-30-days
		// population at typical install sizes. The cache value is
		// an int64; entries are ~24 bytes each, so the LRU sits
		// well under 1 MB even at full capacity.
		w.unread = cache.Register[int64](registry, cacheDomainUnreadCount, 10_000)
	}
	return w
}

// SetBlockChecker / SetPrefsResolver are post-construction setters
// so the boot wiring in http/server.go can hand-off the social +
// userprefs handlers AFTER all three are constructed — same
// pattern posts.Handler uses for follows.
func (w *Writer) SetBlockChecker(b blockChecker)   { w.blocks = b }
func (w *Writer) SetPrefsResolver(p prefsResolver) { w.prefs = p }

// Notify is the single write entry point. Returns nil on either a
// successful insert OR a skip (any of the three gates above) — the
// caller almost always wants "send if appropriate, otherwise stay
// silent" semantics, and bubbling skip-errors back up forces every
// emitter to repeat the same nil-checking dance.
//
// Real DB errors DO propagate — those are infra problems the
// emitter's request handler should surface as 500.
func (w *Writer) Notify(ctx context.Context, n Input) error {
	// Gate 1: actor != recipient. Common case is "comment on my own
	// post" — author writes a comment under their own post, we don't
	// notify them about it.
	if n.ActorUserRef != nil && *n.ActorUserRef == n.RecipientUserRef {
		return nil
	}
	// Gate 2: block edge. Skip when actor blocks recipient OR
	// recipient blocks actor. nil checker = skip the gate (test
	// path); production always wires it.
	if w.blocks != nil && n.ActorUserRef != nil {
		blocked, err := w.blocks.HasBlockBetween(ctx, *n.ActorUserRef, n.RecipientUserRef)
		if err != nil {
			return err
		}
		if blocked {
			return nil
		}
	}
	// Gate 3: channel pref. Empty channel list = recipient muted this
	// verb; skip both in_app and email. Defaults applied when nil.
	channels, err := w.resolveChannels(ctx, n.RecipientUserRef, n.Verb)
	if err != nil {
		return err
	}
	wantInApp := containsString(channels, "in_app")
	wantEmail := containsString(channels, "email")
	if !wantInApp && !wantEmail {
		return nil
	}

	// Marshal the payload once. JSONB column will reject malformed
	// bytes at insert; a payload that can't marshal is a programming
	// error in the emitter, not a runtime situation.
	payloadJSON := []byte("{}")
	if n.Payload != nil {
		b, err := json.Marshal(n.Payload)
		if err != nil {
			return err
		}
		payloadJSON = b
	}

	// The written notification's id — needed to anchor a digest_queue
	// row (Phase 1.55.Y). Invalid (zero) when the in-app row wasn't
	// written (in-app muted for this verb).
	var notifID pgtype.UUID
	if wantInApp {
		q := New(w.pool)
		row, err := q.InsertNotification(ctx, InsertNotificationParams{
			RecipientUserRef: n.RecipientUserRef,
			ActorUserRef:     n.ActorUserRef,
			Verb:             n.Verb,
			TargetKind:       stringPtrOrNil(n.TargetKind),
			TargetID:         stringPtrOrNil(n.TargetID),
			Payload:          payloadJSON,
		})
		if err != nil {
			return err
		}
		notifID = row.ID
		// Invalidate the recipient's unread count — federated peers
		// drop their copy via the NOTIFY broadcast in the same beat.
		w.invalidateUnread(ctx, n.RecipientUserRef)
		if w.logger != nil {
			w.logger.Info("notification written",
				slog.Int64("recipient", n.RecipientUserRef),
				slog.String("verb", n.Verb),
			)
		}
	}

	// Email channel (Phase 1.55.Y cadence fork). "email" being in the
	// channel list is the on/off gate; cadence refines *when* it fires:
	//   - immediate (default)  → enqueue notification.email now
	//   - hourly/daily/weekly  → insert a digest_queue row for the
	//                            coordinator to batch
	//   - (off = "email" simply not in the channel list; handled above)
	// A digest needs a notification row to reference; when in-app was
	// muted (no row) we fall back to immediate so the email isn't lost.
	if wantEmail && w.jobs != nil {
		cadence := w.resolveCadence(ctx, n.RecipientUserRef, n.Verb)
		if cadence != "immediate" && notifID.Valid {
			if err := New(w.pool).InsertDigestQueue(ctx, InsertDigestQueueParams{
				UserRef:        n.RecipientUserRef,
				Topic:          n.Verb,
				Cadence:        cadence,
				NotificationID: notifID,
			}); err != nil && w.logger != nil {
				w.logger.Warn("digest queue insert failed",
					slog.Int64("recipient", n.RecipientUserRef),
					slog.String("verb", n.Verb),
					slog.String("cadence", cadence),
					slog.String("err", err.Error()),
				)
			}
			return nil
		}

		jobPayload, err := json.Marshal(emailJobPayload{
			RecipientUserRef: n.RecipientUserRef,
			Verb:             n.Verb,
			TargetKind:       stringOrEmpty(n.TargetKind),
			TargetID:         stringOrEmpty(n.TargetID),
			Payload:          n.Payload,
		})
		if err != nil {
			return err
		}
		// Best-effort enqueue — a failed job queue should NOT block
		// the in-app notification that already landed.
		if err := w.jobs.Enqueue(ctx, "notification.email", jobPayload); err != nil && w.logger != nil {
			w.logger.Warn("notification email job enqueue failed",
				slog.Int64("recipient", n.RecipientUserRef),
				slog.String("verb", n.Verb),
				slog.String("err", err.Error()),
			)
		}
	}
	return nil
}

// resolveCadence returns the recipient's email cadence for the verb,
// defaulting to immediate when no resolver is wired (tests) or on a
// lookup error (fail-open to send-now rather than silently dropping).
func (w *Writer) resolveCadence(ctx context.Context, ref int64, verb string) string {
	if w.prefs == nil {
		return "immediate"
	}
	c, err := w.prefs.CadenceFor(ctx, ref, verb)
	if err != nil || c == "" {
		return "immediate"
	}
	return c
}

// resolveChannels asks the prefs resolver for the channel list,
// falling back to a conservative default (in_app only) when no
// resolver is wired (tests).
func (w *Writer) resolveChannels(ctx context.Context, ref int64, verb string) ([]string, error) {
	if w.prefs == nil {
		return []string{"in_app"}, nil
	}
	return w.prefs.ChannelsFor(ctx, ref, verb)
}

// invalidateUnread drops the recipient's cached count + NOTIFYs
// federated peers. Failure is logged-and-keep-going; the cache
// stays stale until next manual read which will re-populate.
func (w *Writer) invalidateUnread(ctx context.Context, ref int64) {
	if w.unread == nil {
		return
	}
	if err := w.unread.Invalidate(ctx, unreadKey(ref)); err != nil && w.logger != nil {
		w.logger.Warn("notifications.cache.invalidate.error",
			slog.Int64("recipient", ref),
			slog.String("err", err.Error()),
		)
	}
}

func unreadKey(ref int64) string { return strconv.FormatInt(ref, 10) }

// Input is the typed Notify argument. Pointer fields for the
// nullable bits so emitters can stay terse on the common path
// (just RecipientUserRef + Verb).
type Input struct {
	RecipientUserRef int64
	ActorUserRef     *int64
	Verb             string
	TargetKind       string         // empty → NULL
	TargetID         string         // empty → NULL
	Payload          map[string]any // nil → {}
}

// emailJobPayload is what the Phase I2-b email worker picks off
// the queue. Frozen now so the job-handler PR doesn't churn this
// shape.
type emailJobPayload struct {
	RecipientUserRef int64          `json:"recipient_user_ref"`
	Verb             string         `json:"verb"`
	TargetKind       string         `json:"target_kind,omitempty"`
	TargetID         string         `json:"target_id,omitempty"`
	Payload          map[string]any `json:"payload,omitempty"`
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func stringOrEmpty(s string) string { return s }

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}
