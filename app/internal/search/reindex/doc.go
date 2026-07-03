// Package reindex implements Phase 1.16.B-5 — operator-triggered
// admin reindex of the search subsystem's tsvector + embedding
// columns.
//
// Two targets:
//
//   - tsvector: touch the entity's updated_at so its rebuild_*_
//     search_text trigger re-computes the weighted vector.
//     Useful after schema migration on large corpora where the
//     migration's inline backfill fell through the
//     10k-row-skip-to-admin-reindex path.
//   - embedding: enqueue one ai.embed job per asset in scope,
//     reusing the same idempotency-keyed enqueue as the upload
//     handler. The embed provider's dedup absorbs redundant fires
//     silently (pre-audit Q7), so reindex is safe to fire
//     thousands of enqueues.
//
// Scope forms (parsed from the operator-facing JSON body):
//
//	{"scope": "all"}
//	{"scope": "asset_type:<uuid>"}
//	{"scope": "collection:<uuid>"}
//	{"scope": "embedding_model:<provider>/<model>"}
//	{"scope": "federation_missing"}   // origin_server_id IS NOT
//	                                  //   NULL AND no embedding
//	                                  //   row — used by the boot
//	                                  //   backfill single-shot
//
// Non-concurrent per instance — the partial UNIQUE INDEX on
// search_reindex_run enforces one active run at a time; a second
// admin trigger races into 23505 → the handler maps to 409.
//
// Cancel-safe: the worker polls its own run row at every batch
// boundary; a non-null cancelled_at short-circuits the walk.
// Embed jobs already enqueued still run (documented cancel
// semantics in the admin UI).
//
// No new outbox events. No CGo. No new Postgres extensions.
package reindex
