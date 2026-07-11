// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package visibility is the shared per-entity visibility gate.
// Extracted in Phase 1.16.B-2 to close a load-bearing divergence
// carried forward from 1.16.B-1: search + facets + suggestions
// need the exact same visibility semantics as the existing list
// handlers, but no shared helper existed — the search Engine
// replicated each entity's visibility WHERE clause inline, and any
// future rule change would have needed edits in every replica.
//
// This package is the single source of truth. Callers (list
// handlers, search Engine, facet aggregators, suggestion queries,
// saved-search notifier in a later sub-phase) request a
// [Predicate] for the entity type they're about to query; the
// Predicate renders to a SQL WHERE-fragment that composes with any
// query builder.
//
// Design decisions in a nutshell:
//
//   - EntityType is a small enum (Asset, Collection, Post). Growing
//     the set is a matter of adding a case in [Filter].
//   - Each Predicate carries the CALLER's effective visibility set
//     (own / public / ACL-granted). Nil caller (anonymous) is
//     modelled explicitly as [AnonymousCaller].
//   - ToSQL(alias) returns (fragment, args). fragment always begins
//     with " AND (…)" so callers can concatenate into any existing
//     WHERE clause without pre-processing. args are $-indexed by
//     the CALLER passing them through pgx.Query starting at the
//     next unused index — callers supply an ArgOffset so the
//     first placeholder in fragment lines up with the caller's
//     next-available parameter number.
//
// Scope in B-2 (documented divergences from the brief):
//
//   - This package is the load-bearing floor for the SEARCH
//     subsystem in B-2 (search.Engine, facet aggregators,
//     suggestion queries all consume it). Retrofitting the
//     existing list handlers is documented as a follow-up in the
//     PR body — the immediate goal was closing the search
//     visibility-leak vector, not disturbing tested list surfaces.
//   - Federation-remote entities are gated identically to local
//     entities: origin_server_id is a pass-through column, not a
//     visibility signal. A federated-restricted asset the local
//     user cannot see falls under the same [Predicate] filter as
//     a local-restricted one.
package visibility
