// Dispatch helpers — the "gold-standard" handler shape that
// can't forget anything.
//
// Pattern:
//
//	em := emit.Like(actorCtx, postRef)
//	err := h.activities.WithEmission(ctx, em, func(tx pgx.Tx) error {
//	    return New(tx).LikeTarget(ctx, params)
//	})
//
// WithEmission begins a transaction, runs the caller's domain
// write inside it, records the activity in the SAME transaction
// (ADR 0044's invariant: either both commit or both roll back),
// commits, then fires notifications AFTER successful commit
// (notification failures are logged-not-propagated; a transient
// notifications-table problem must NEVER block the social action
// it accompanies).
//
// Two variants:
//   - WithEmission: Emission known up front. Most handlers.
//   - WithEmissionFn: Emission computed inside the tx (needed
//     when the activity's input depends on the domain write's
//     result — e.g. CreatePost mints a UUID at insert time, so
//     the activity URI + object URI aren't known until after).

package activities

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// Notifier is the cross-package post-commit notification hook —
// the boot wiring constructs an adapter from
// *notifications.Writer that satisfies this interface. Defined
// here (not imported from notifications) because the emit
// subpackage already returns the typed NotificationInput payloads;
// the dispatch helpers below just need a way to fire them.
type Notifier interface {
	Notify(ctx context.Context, recipient int64, actor *int64, verb, targetKind, targetID string, payload map[string]any) error
}

// UsernameResolver is the cross-package username-by-ref lookup
// federation emit helpers need to build actor URIs for arbitrary
// users (post authors, comment-parent authors, DM recipients,
// followees, etc.). Implemented by *users.Handler — which has the
// existing UserPublic byRef cache so resolves are typically
// memory-bound.
//
// Defined here so the dispatch layer doesn't import users
// directly; the boot wiring constructs an adapter. Empty-string
// return on miss is the contract; callers treat that as "skip
// federated addressing for this user" rather than failing.
//
// Per docs/spec/federation/v1.md §8.4 the username is immutable
// from the federation perspective so cached values stay correct
// for the actor's lifetime — no invalidation plumbing required.
type UsernameResolver interface {
	ResolveUsername(ctx context.Context, userRef int64) string
}

// EmissionInput is the shape handlers pass to WithEmission. It
// matches emit.Emission exactly — emit helpers' Emission is
// trivially convertible. Defined here as a separate type so the
// activities package's dispatch helpers don't depend on the
// emit subpackage (cycle avoidance).
type EmissionInput struct {
	Activity      Input
	Notifications []NotificationInput
}

// NotificationInput mirrors emit.NotificationFanout. Same
// cycle-avoidance reason.
type NotificationInput struct {
	Recipient  int64
	Verb       string
	TargetKind string
	TargetID   string
	Payload    map[string]any
}

// SetNotifier wires the cross-package notifications writer. Boot
// constructs an adapter from *notifications.Writer (which has
// Notify(ctx, recipient, actor, verb, kind, id, payload)) and
// passes it here. Nil-safe — when not wired, notifications fire
// as logged no-ops.
func (w *Writer) SetNotifier(n Notifier) { w.notifier = n }

// SetUsernameResolver wires the cross-package username lookup
// per the UsernameResolver doc. Nil-safe — when not wired,
// ResolveUsername returns empty string and emit helpers fall back
// to local-only addressing.
func (w *Writer) SetUsernameResolver(r UsernameResolver) { w.userResolver = r }

// ResolveUsername resolves a user_ref to their username via the
// wired UsernameResolver (typically users.Handler with its
// existing cache). Empty string when no resolver is wired or the
// user isn't found.
func (w *Writer) ResolveUsername(ctx context.Context, userRef int64) string {
	if w.userResolver == nil {
		return ""
	}
	return w.userResolver.ResolveUsername(ctx, userRef)
}

// WithEmission is the gold-standard handler-side helper. Caller
// supplies an Emission (activity + notifications) and a closure
// that does the domain write inside the supplied tx.
//
// Sequence:
//  1. Begin transaction.
//  2. Run fn(tx) — the caller's domain write.
//  3. Record the activity in the SAME tx.
//  4. Commit.
//  5. Fire notifications AFTER commit (best-effort; errors
//     logged, not propagated).
//
// Returns:
//   - nil on full success (commit + notifications fired).
//   - fn's error if the domain write failed — tx rolled back,
//     activity NOT recorded, notifications NOT fired.
//   - the RecordActivity error if the activity insert failed —
//     tx rolled back.
//   - the Commit error if commit failed — notifications NOT
//     fired.
//
// Notifications fire after commit so they can't block the
// social action. They also can't fail it.
func (w *Writer) WithEmission(ctx context.Context, em EmissionInput, fn func(tx pgx.Tx) error) error {
	return w.withEmissionImpl(ctx, func(tx pgx.Tx) (EmissionInput, error) {
		if err := fn(tx); err != nil {
			return EmissionInput{}, err
		}
		return em, nil
	})
}

// WithEmissionFn is the variant for handlers whose activity
// input depends on the domain write's result — e.g. CreatePost
// mints a UUID at INSERT time, so the activity URI + object URI
// aren't computable until after the insert returns.
//
// Same sequence as WithEmission, but the closure both does the
// domain write AND returns the Emission to record.
func (w *Writer) WithEmissionFn(ctx context.Context, fn func(tx pgx.Tx) (EmissionInput, error)) error {
	return w.withEmissionImpl(ctx, fn)
}

// withEmissionImpl is the shared body for both public helpers.
func (w *Writer) withEmissionImpl(ctx context.Context, fn func(tx pgx.Tx) (EmissionInput, error)) error {
	if w.Pool == nil {
		return errors.New("activities: Writer not wired to a pool")
	}
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("activities: begin tx: %w", err)
	}
	// Defer rollback unconditionally — pgx.Tx.Rollback on an
	// already-committed tx is a no-op (returns ErrTxClosed which
	// we ignore).
	defer func() { _ = tx.Rollback(ctx) }()

	em, err := fn(tx)
	if err != nil {
		return err
	}
	if _, err := w.RecordActivity(ctx, tx, em.Activity); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("activities: commit: %w", err)
	}

	// Post-commit side effects. Errors logged, not propagated —
	// the social action already succeeded; we won't fail it on a
	// notification dispatch issue.
	w.fireNotifications(ctx, em.Activity.ActorUserRef, em.Notifications)
	return nil
}

// fireNotifications dispatches every queued notification.
// Best-effort: each Notify failure is logged independently so
// one bad recipient doesn't shadow others.
func (w *Writer) fireNotifications(ctx context.Context, actor *int64, notifs []NotificationInput) {
	if w.notifier == nil || len(notifs) == 0 {
		return
	}
	for _, n := range notifs {
		if err := w.notifier.Notify(ctx, n.Recipient, actor, n.Verb, n.TargetKind, n.TargetID, n.Payload); err != nil && w.Logger != nil {
			w.Logger.LogAttrs(ctx, slog.LevelWarn, "activities.notification.dispatch_error",
				slog.Int64("recipient", n.Recipient),
				slog.String("verb", n.Verb),
				slog.String("err", err.Error()),
			)
		}
	}
}
