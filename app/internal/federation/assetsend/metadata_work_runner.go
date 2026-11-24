// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assetsend

import (
	"context"
	"log/slog"
	"net/url"
	"time"
)

// The runner's pacing defaults (MetadataWorkRunner).
const (
	DefaultMetadataWorkBurst   = 8
	DefaultMetadataWorkIdle    = 5 * time.Second
	DefaultMetadataWorkYield   = 250 * time.Millisecond
	DefaultMetadataWorkBackoff = time.Second
	maxMetadataWorkBackoff     = time.Minute
)

// MetadataWorkRunner processes the metadata work ledger locally (#1577):
// a cancellation-aware loop around MetadataWork.RunOnce, started under
// the server's lifecycle. There is no LISTEN and no external worker: it
// polls.
//
//   - It tries at once on start, then runs bursts of at most Burst pages
//     of Page items, yields Yield after a full burst, and waits Idle once
//     the ledger is empty.
//   - The origin is read before every page. While it is empty or not an
//     absolute http(s) URL nothing is processed, so work stays pending
//     rather than producing malformed activities; it is retried after
//     Backoff.
//   - A failed page keeps nothing (RunOnce) and is retried after a backoff
//     that starts at Backoff and doubles to a minute, reset by a success.
//   - It logs constant events only, never an error's text: a database
//     error can quote a row, and a row can hold metadata.
//
// Every query and wait honours ctx; Run returns when ctx ends.
type MetadataWorkRunner struct {
	// Work is the processor; its BaseURL is replaced by Origin's for
	// each page, and a nil Username reads the owner in the transaction.
	Work MetadataWork
	// Origin returns this instance's base URL.
	Origin func(ctx context.Context) string
	Logger *slog.Logger

	// Zero values mean the defaults above (Page: DefaultMetadataWorkPage).
	Burst   int
	Page    int
	Idle    time.Duration
	Yield   time.Duration
	Backoff time.Duration
}

// Run processes work until ctx ends.
func (r *MetadataWorkRunner) Run(ctx context.Context) {
	burst := orInt(r.Burst, DefaultMetadataWorkBurst)
	idle := orDuration(r.Idle, DefaultMetadataWorkIdle)
	yield := orDuration(r.Yield, DefaultMetadataWorkYield)
	base := orDuration(r.Backoff, DefaultMetadataWorkBackoff)
	backoff := base
	originOK := true
	for {
		wait := yield
		for i := 0; i < burst; i++ {
			origin := ""
			if r.Origin != nil {
				origin = r.Origin(ctx)
			}
			if !validOrigin(origin) {
				if originOK {
					r.log(ctx, slog.LevelWarn, "metadata_work.origin_unavailable")
				}
				originOK = false
				wait = base
				break
			}
			if !originOK {
				r.log(ctx, slog.LevelInfo, "metadata_work.origin_available")
				originOK = true
			}
			m := r.Work
			m.BaseURL = origin
			worked, err := m.RunOnce(ctx, r.Page)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				r.log(ctx, slog.LevelWarn, "metadata_work.page_failed")
				wait = backoff
				backoff = min(backoff*2, maxMetadataWorkBackoff)
				break
			}
			backoff = base
			if !worked {
				wait = idle
				break
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (r *MetadataWorkRunner) log(ctx context.Context, level slog.Level, event string) {
	if r.Logger != nil {
		r.Logger.LogAttrs(ctx, level, event)
	}
}

// validOrigin reports whether origin is an absolute http(s) URL with a
// host, the only form an actor URI may be built on.
func validOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func orInt(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

func orDuration(v, d time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return d
}
