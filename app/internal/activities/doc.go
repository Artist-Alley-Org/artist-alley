// Package activities is the CQRS-lite activities ledger — the
// canonical record of every federated social action on this
// instance.
//
// # The invariant (ADR 0044)
//
// Every state-mutating social handler MUST emit an activity in
// the same database transaction as its domain write. The activity
// row is the source of truth for federation; the domain tables
// (posts, comments, likes, user_follows, user_blocks,
// direct_messages, notifications) are kept in sync synchronously
// as optimized read projections.
//
// If you wrote a social-state mutation that does NOT call
// RecordActivity in the same transaction, you wrote a bug. The
// code-review checklist enforces this; future federation work
// assumes it.
//
// # Shape
//
// Activity rows are AP-shape per docs/spec/federation/v1.md §3.
// activity_uri is the cross-instance handle; activity_type is one
// of the closed catalogue in federation.ActivityType. Idempotent
// inserts: ON CONFLICT (activity_uri) DO NOTHING means re-emitting
// the same activity (job retry, replay tool, peer redelivery) is
// a no-op.
//
// # Two sources, one ledger
//
//   - source = "local"      — emitted by handlers on this instance.
//                             Federation outbox (Phase 1.22.D)
//                             reads these to publish to peers.
//   - source = "https://..." — received from a federated peer.
//                             Inbox dispatch (Phase 1.22.D) writes
//                             these as it admits inbound activities.
//
// # Caching
//
// Per-actor outbox feed is cached behind activities.actor_outbox
// (cache.Registry NOTIFY broadcast on every emit so federated
// peers' replicas stay in sync). Per-object timeline is cached
// per (object_kind, object_local_id) for the admin audit drill-
// down. Cold misses fall through to the indexed query.
package activities
