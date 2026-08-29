// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package saved implements Phase 1.16.B-4 — saved searches +
// email-on-match. Layered over the B-1/B-2/B-3 search subsystem
// plus the Phase 1.19.A email substrate.
//
// Three responsibilities:
//
//  1. CRUD on the saved_search table (Store): create + list-for-
//     owner + get + patch + delete + record-run. Rate limits are
//     enforced at Create (max-per-user) and at update-time
//     (min-interval floor). Owner-gates enforced at the HTTP
//     layer, not this package — Store is oblivious to caller
//     identity so tests can drive it directly.
//
//     ⭐ WHAT A ROW STORES IS THE WHOLE QUERY (#1368). `dsl` is one
//     canonical string carrying the caller's expression as a single
//     parenthesised operand conjuncted with their facet selection, and
//     there is deliberately no second filter column: two authoritative
//     forms of one query would need a merge rule and a precedence
//     answer. The create endpoint takes the selection as the ordinary
//     `dimension:value` tokens every other surface uses and composes the
//     string via search.ComposeDSL. Before that, the expression was the
//     only thing that travelled and every saved search replayed WIDER
//     than the page it was saved from.
//
//  2. Execute + Delta: parses the stored DSL string, compiles
//     via the shared dsl package, resolves any similar_to:<uuid>
//     anchor via the vector.Fetcher (B-3), runs the query
//     through search.Engine.Run WITH VISIBILITY GATED FOR THE
//     OWNER AT EXECUTION TIME (not save time), then hashes the
//     sorted asset-ID set. A hash mismatch against
//     last_result_hash means "something changed"; the notifier
//     computes the exact Added set-diff against last_result_ids
//     for the digest email.
//
//  3. Coordinator + Notifier: a scheduled saved_search.notify_
//     coordinator job walks the due rows, enqueues per-row
//     saved_search.notify_run children with idempotency keys,
//     and self-re-enqueues via jobs.EnqueueOpts.ScheduledFor.
//     Each notify_run reads its row, executes, computes delta,
//     writes the fresh snapshot back to the row, and — on delta
//     + notify_channel='email' — enqueues a notification.email
//     job with the saved_search_digest template payload for the
//     existing Phase 1.19.A NotificationJobHandler to render +
//     deliver.
//
// Load-bearing invariants (highest-severity failure modes):
//
//   - Visibility at execution time, not save time. A user who
//     saves a search + later loses access to some assets stops
//     seeing those assets in the digest silently. Test:
//     TestExecute_OwnerLostAccess_HitsDropped.
//   - Delta = hash of sorted asset-ID set. Deterministic;
//     independent of vector-score noise or hit ordering within a
//     tie. Cross-run hash equality is the definition of
//     "no change".
//   - Never federates. Per pre-audit Q5, federation is opt-in via
//     activities.Emit; saved_search never emits. origin_server_id
//     column present for schema-shape parity only.
//   - Cache bypass. Execution calls search.Engine.Run directly,
//     not search.Service.Execute — the query-result cache is
//     skipped so notifications trigger on real-time state, not
//     stale cached results. Interactive user cache hits are
//     untouched.
//
// Federation soak still applies: no code in this package writes
// to app/internal/federation/.
package saved
