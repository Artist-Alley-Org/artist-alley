// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package userprefs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func unsubTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") + " port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") + " dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedUnsubUser(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var ref int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		"unsub"+itoaU(time.Now().UnixNano()), "Unsub Test",
	).Scan(&ref); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_preferences WHERE user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

func hasEmail(chs []string) bool {
	for _, c := range chs {
		if c == ChannelEmail {
			return true
		}
	}
	return false
}

func TestUnsubscribeEmail_SingleTopic_TurnsOff(t *testing.T) {
	pool := unsubTestPool(t)
	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref := seedUnsubUser(t, pool)
	ctx := context.Background()

	// Opt the user into email for a topic first.
	if err := h.savePreferences(ctx, ref, Preferences{
		NotificationChannels: NotificationChannels{EventMentionOfMe: {ChannelInApp, ChannelEmail}},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Unsubscribe that topic.
	if err := h.UnsubscribeEmail(ctx, ref, EventMentionOfMe); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	chs, err := h.ChannelsFor(ctx, ref, EventMentionOfMe)
	if err != nil {
		t.Fatalf("channels: %v", err)
	}
	if hasEmail(chs) {
		t.Fatalf("email should be off after unsubscribe, got %v", chs)
	}
	// in_app survives.
	found := false
	for _, c := range chs {
		if c == ChannelInApp {
			found = true
		}
	}
	if !found {
		t.Fatalf("in_app should survive unsubscribe, got %v", chs)
	}
}

func TestUnsubscribeEmail_AllTopics_TurnsOffEverywhere(t *testing.T) {
	pool := unsubTestPool(t)
	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref := seedUnsubUser(t, pool)
	ctx := context.Background()

	// Email on for two topics (DM defaults to email too).
	if err := h.savePreferences(ctx, ref, Preferences{
		NotificationChannels: NotificationChannels{
			EventMentionOfMe:           {ChannelInApp, ChannelEmail},
			EventDirectMessageReceived: {ChannelInApp, ChannelEmail},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := h.UnsubscribeEmail(ctx, ref, "__all__"); err != nil {
		t.Fatalf("unsubscribe all: %v", err)
	}
	for _, ev := range KnownEventTypes {
		chs, err := h.ChannelsFor(ctx, ref, ev)
		if err != nil {
			t.Fatalf("channels %s: %v", ev, err)
		}
		if hasEmail(chs) {
			t.Fatalf("email should be off for every topic after __all__, %s still has it: %v", ev, chs)
		}
	}
}

func itoaU(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// An unrelated write must not reset a preference it does not mention.
// Unsubscribe and the preferences PATCH now share one marshal-and-upsert
// (savePreferences); when they were two copies, a column added to the
// row was persisted by the one that had been updated and quietly
// defaulted by the other. The one-click unsubscribe link is exactly the
// write a user takes without thinking about their feed settings.
func TestUnsubscribeEmail_PreservesFeedFilters(t *testing.T) {
	pool := unsubTestPool(t)
	h := NewHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref := seedUnsubUser(t, pool)
	ctx := context.Background()

	if err := h.savePreferences(ctx, ref, Preferences{
		NotificationChannels: NotificationChannels{EventMentionOfMe: {ChannelInApp, ChannelEmail}},
		FeedFilters:          FeedFilters{ShowRestricted: true},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := h.UnsubscribeEmail(ctx, ref, EventMentionOfMe); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	show, err := h.ShowRestrictedFeedMembers(ctx, ref)
	if err != nil {
		t.Fatalf("read filter: %v", err)
	}
	if !show {
		t.Fatal("unsubscribing from an email topic reset the user's feed setting to the default")
	}
}
