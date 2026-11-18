// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package http

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Database capacity a running server needs (#1605).
//
// Long-lived connections:
//
//   - Three are held FROM the pool for as long as the server runs, one
//     per federation LISTEN loop started by Server.Run: the inbox
//     dispatcher (federation_inbox_pending), the outbox dispatcher
//     (federation_dispatch_pending) and the outbox delivery worker
//     (federation_outbox_pending). Each acquires a pool connection and
//     releases it only when the server's context ends.
//   - One more is the cache registry's listener (cache_invalidate). It
//     is acquired and then hijacked, so it leaves the pool: it does not
//     count against AA_DB_MAX_CONNS, but it is a fourth connection to
//     PostgreSQL, so the server opens up to AA_DB_MAX_CONNS + 1.
//
// Requests, jobs and every other worker share what the pool has left.
// With the driver's own default (max(4, CPUs)) a four-CPU host would
// keep one slot for all of them and stall, which is what PR #1603's CI
// hit. So the pool must leave at least minDBQuerySlots free after the
// listeners; anything smaller is refused at startup, before a listener
// is acquired or a request served, instead of hanging later.
const (
	poolHeldListeners = 3
	minDBQuerySlots   = 5
	// MinDBMaxConns is the smallest AA_DB_MAX_CONNS a server starts
	// with. The production default (config.Load) is 20.
	MinDBMaxConns = poolHeldListeners + minDBQuerySlots
)

// checkDBCapacity refuses a pool too small to serve alongside the
// server's long-lived listeners. It reads the pool's actual limit, not
// the configuration that asked for it.
func checkDBCapacity(pool *pgxpool.Pool) error {
	if pool == nil {
		return fmt.Errorf("database pool: none")
	}
	if got := pool.Config().MaxConns; got < MinDBMaxConns {
		return fmt.Errorf("database pool: max %d connections, need at least %d: set AA_DB_MAX_CONNS to %d or more (default 20). "+
			"%d pool connections are held by federation LISTEN loops while the server runs, and at least %d must stay free "+
			"for requests and jobs; the cache listener uses one more connection outside the pool",
			got, MinDBMaxConns, MinDBMaxConns, poolHeldListeners, minDBQuerySlots)
	}
	return nil
}
