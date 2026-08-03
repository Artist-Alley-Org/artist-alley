// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package digest batches non-immediate notification emails (Phase
// 1.55.Y). The Writer routes hourly/daily/weekly notifications into
// digest_queue; this coordinator ticks hourly, aggregates each user's
// due rows into one email, sends it, and marks the rows consumed.
//
// One coordinator handles all three cadences, gated by the clock
// (mirroring the single hour-ticking softdelete gc coordinator): every
// tick processes `hourly` rows; on the sysconfig daily hour it also
// processes `daily`; on the sysconfig weekly day+hour it also processes
// `weekly`. Cleaner than three job types with three schedules.
package digest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/email"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/notifications"
)

// JobTypeCoordinator is the hourly wake-up job.
const JobTypeCoordinator jobs.JobType = "email.digest.coordinator"

// Config holds the sysconfig-driven timing knobs. Zero values are
// sensible defaults (8 AM UTC daily; Monday 8 AM UTC weekly).
type Config struct {
	DailyHourUTC  int          // 0-23
	WeeklyDay     time.Weekday // time.Monday..Sunday
	WeeklyHourUTC int          // 0-23
}

// SiteContext is the small per-instance projection the templates +
// unsubscribe links need.
type SiteContext struct {
	Name string
	URL  string
}

// Coordinator is the digest batch engine. All deps are injected so the
// batch logic is unit-testable against a real DB + capture sender.
type Coordinator struct {
	Pool        *pgxpool.Pool
	Sender      email.Sender
	Jobs        *jobs.Service // nil-safe (tests skip re-enqueue)
	Logger      *slog.Logger
	ScrambleKey string
	// CfgFn resolves the timing knobs per tick so runtime sysconfig
	// changes take effect without a restart. nil = built-in defaults.
	CfgFn  func(ctx context.Context) Config
	SiteFn func(ctx context.Context) SiteContext
	// Now is injected for deterministic tests; defaults to time.Now.
	Now func() time.Time
}

func (c *Coordinator) cfg(ctx context.Context) Config {
	if c.CfgFn != nil {
		return c.CfgFn(ctx)
	}
	return Config{DailyHourUTC: 8, WeeklyDay: time.Monday, WeeklyHourUTC: 8}
}

// Type implements jobs.Handler.
func (c *Coordinator) Type() jobs.JobType { return JobTypeCoordinator }

// Handle runs one batch for the cadences due at the current time, then
// self-re-enqueues at the top of the next hour.
func (c *Coordinator) Handle(ctx context.Context, _ *jobs.Claim) (json.RawMessage, error) {
	now := c.now()
	sent, err := c.RunOnce(ctx, now)
	if err != nil {
		return nil, err
	}
	c.reEnqueue(ctx, now)
	b, _ := json.Marshal(map[string]any{"emails_sent": sent, "ran_at": now.UTC()})
	return b, nil
}

// RunOnce processes every due cadence for `now` and returns the number
// of digest emails sent. Exported + clock-injected so tests drive it
// directly without the jobs framework.
func (c *Coordinator) RunOnce(ctx context.Context, now time.Time) (int, error) {
	cadences := DueCadences(now.UTC(), c.cfg(ctx))
	if len(cadences) == 0 {
		return 0, nil
	}
	q := notifications.New(c.Pool)
	rows, err := q.ListPendingDigest(ctx, cadences)
	if err != nil {
		return 0, fmt.Errorf("digest: list pending: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	site := SiteContext{}
	if c.SiteFn != nil {
		site = c.SiteFn(ctx)
	}

	// Rows arrive ordered by user_ref; group into contiguous runs.
	sent := 0
	i := 0
	for i < len(rows) {
		j := i
		for j < len(rows) && rows[j].UserRef == rows[i].UserRef {
			j++
		}
		group := rows[i:j]
		i = j

		ids := make([]pgtype.UUID, 0, len(group))
		for _, r := range group {
			ids = append(ids, r.ID)
		}

		if err := c.sendUserDigest(ctx, group, site); err != nil {
			// Log + skip this user; do NOT mark their rows sent so the
			// next tick retries. Other users still get their digests.
			if c.Logger != nil {
				c.Logger.LogAttrs(ctx, slog.LevelWarn, "digest.user_send_failed",
					slog.Int64("user_ref", group[0].UserRef),
					slog.String("err", err.Error()))
			}
			continue
		}
		if err := q.MarkDigestSent(ctx, ids); err != nil {
			if c.Logger != nil {
				c.Logger.LogAttrs(ctx, slog.LevelWarn, "digest.mark_sent_failed",
					slog.Int64("user_ref", group[0].UserRef),
					slog.String("err", err.Error()))
			}
			continue
		}
		sent++
	}
	return sent, nil
}

// sendUserDigest renders + sends one aggregated email for a user's
// group of due rows. A recipient with no email on file is a no-op
// success (the caller marks their rows sent so the queue doesn't grow
// unbounded).
func (c *Coordinator) sendUserDigest(ctx context.Context, group []notifications.ListPendingDigestRow, site SiteContext) error {
	if c.Sender == nil {
		return fmt.Errorf("digest: no sender wired")
	}
	q := notifications.New(c.Pool)
	rcpt, err := q.DigestRecipientEmail(ctx, group[0].UserRef)
	if err != nil {
		return fmt.Errorf("digest: recipient lookup: %w", err)
	}
	emailAddr := deref(rcpt.Email)
	if strings.TrimSpace(emailAddr) == "" {
		// No email — nothing to send. Success so the rows get consumed.
		return nil
	}

	items := make([]map[string]any, 0, len(group))
	for _, r := range group {
		items = append(items, map[string]any{
			"headline": headlineForVerb(r.Verb),
			"url":      targetURL(site.URL, r.TargetKind, r.TargetID),
			"when":     humanWhen(r.CreatedAt),
			"summary":  payloadSummary(r.Payload),
		})
	}

	// One-click unsubscribe token targets the digest as a whole
	// (pseudo-topic "__all__" → the endpoint drops email from every
	// topic). Cadence label = the coarsest cadence present, for copy.
	token := email.SignUnsubscribe(c.ScrambleKey, group[0].UserRef, "__all__", c.now())
	unsubURL := email.UnsubscribeURL(site.URL, token)

	data := map[string]any{
		"recipient_name":  displayName(rcpt),
		"site_name":       site.Name,
		"site_url":        site.URL,
		"cadence_label":   cadenceLabel(coarsestCadence(group)),
		"count":           len(group),
		"items":           items,
		"unsubscribe_url": unsubURL,
	}

	msg, err := email.Render(ctx, email.TemplateNotificationDigest, []string{emailAddr}, data)
	if err != nil {
		return fmt.Errorf("digest: render: %w", err)
	}
	msg.Headers = email.UnsubscribeHeaders(site.URL, token)
	if err := c.Sender.Send(ctx, msg); err != nil {
		return fmt.Errorf("digest: send: %w", err)
	}
	return nil
}

func (c *Coordinator) reEnqueue(ctx context.Context, now time.Time) {
	if c.Jobs == nil {
		return
	}
	next := now.UTC().Truncate(time.Hour).Add(time.Hour)
	if _, err := c.Jobs.Enqueue(ctx, JobTypeCoordinator, struct{}{}, jobs.EnqueueOpts{
		ScheduledFor:   &next,
		IdempotencyKey: "email.digest.coordinator." + next.Format(time.RFC3339),
	}); err != nil && c.Logger != nil {
		c.Logger.LogAttrs(ctx, slog.LevelWarn, "digest.reenqueue_failed",
			slog.String("err", err.Error()))
	}
}

func (c *Coordinator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
