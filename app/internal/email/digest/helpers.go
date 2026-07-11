// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package digest

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/notifications"
)

// DueCadences returns the cadences whose digest is due at `now` (UTC).
// Always includes "hourly"; adds "daily" on the configured daily hour;
// adds "weekly" on the configured weekday+hour. Pure + testable.
func DueCadences(nowUTC time.Time, cfg Config) []string {
	out := []string{"hourly"}
	if nowUTC.Hour() == cfg.DailyHourUTC {
		out = append(out, "daily")
	}
	if nowUTC.Weekday() == cfg.WeeklyDay && nowUTC.Hour() == cfg.WeeklyHourUTC {
		out = append(out, "weekly")
	}
	return out
}

// coarsestCadence returns the least-frequent cadence present in a
// group (weekly > daily > hourly) for the digest's summary copy.
func coarsestCadence(group []notifications.ListPendingDigestRow) string {
	rank := map[string]int{"hourly": 0, "daily": 1, "weekly": 2}
	best := "hourly"
	for _, r := range group {
		if rank[r.Cadence] > rank[best] {
			best = r.Cadence
		}
	}
	return best
}

func cadenceLabel(cadence string) string {
	switch cadence {
	case "hourly":
		return "hourly"
	case "daily":
		return "daily"
	case "weekly":
		return "weekly"
	default:
		return "periodic"
	}
}

// headlineForVerb renders a human one-liner for a notification verb.
// Kept server-side + English (the digest email is not i18n'd in
// v0.1.0; per-locale email templating is a separate future arc).
func headlineForVerb(verb string) string {
	switch verb {
	case "mention_of_me":
		return "You were mentioned"
	case "comment_on_my_post":
		return "New comment on your post"
	case "reply_to_my_comment":
		return "Someone replied to your comment"
	case "like_on_my_post":
		return "Someone liked your post"
	case "new_follower":
		return "You have a new follower"
	case "post_from_followed_user":
		return "New post from someone you follow"
	case "direct_message_received":
		return "New direct message"
	case "broadcast_received":
		return "Announcement from the team"
	case "resource_request_received_to_approve":
		return "An asset access request is awaiting your approval"
	case "resource_request_approved":
		return "Your asset access request was approved"
	case "resource_request_denied":
		return "Your asset access request was denied"
	case "license_expiring_soon":
		return "Your license is expiring soon"
	case "license_expired":
		return "Your license has expired"
	default:
		return "New activity"
	}
}

// targetURL builds a deep link for the notification's target. Comments
// route to their containing post per the 1.55.X convention; unknown or
// absent targets fall back to the site root.
func targetURL(siteURL string, kind, id *string) string {
	base := strings.TrimRight(siteURL, "/")
	if id == nil || *id == "" || kind == nil {
		return base
	}
	switch *kind {
	case "post":
		return base + "/posts/" + *id
	case "user":
		return base + "/users/by-ref/" + *id
	default:
		return base + "/account/notifications"
	}
}

// payloadSummary pulls a short human summary from the notification
// payload when present (e.g. an excerpt), else "".
func payloadSummary(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	for _, k := range []string{"excerpt", "summary", "post_title"} {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

func humanWhen(ts pgtype.Timestamptz) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.UTC().Format("Jan 2, 15:04 UTC")
}

func displayName(r notifications.DigestRecipientEmailRow) string {
	if r.Fullname != nil && strings.TrimSpace(*r.Fullname) != "" {
		return strings.TrimSpace(*r.Fullname)
	}
	if r.Username != nil && strings.TrimSpace(*r.Username) != "" {
		return strings.TrimSpace(*r.Username)
	}
	return "there"
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
