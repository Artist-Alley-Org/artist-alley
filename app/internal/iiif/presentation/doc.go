// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package presentation implements IIIF Presentation API 3.0 for
// artist-alley. Phase 1.54.B.
//
// Two entity types render as manifests:
//
//   - Assets → single-canvas manifests (or would-be multi-canvas
//     for multi-page PDFs, but per-page tile routing wasn't in
//     1.54.A per pre-audit Q3; PDFs currently render as
//     single-canvas with the first-page thumbnail + a documented
//     "page N of M" note in the metadata block until follow-up
//     ships the per-page URL grammar).
//   - Collections → Collection manifests listing member assets in
//     the canonical (sort_order ASC, added_at ASC) order the
//     ListCollectionResourcesPage query returns.
//
// Every manifest passes through:
//
//  1. The ROW-PLANE predicate (ADR 0063) — every query in loader.go
//     splices visibility.Predicate.ToSQL, so a caller reaches a
//     manifest only for a row they could also reach through
//     GET /assets/{id} or GET /collections/{id}. This was ABSENT
//     until #661: the loaders read by id with `deleted_at IS NULL`
//     and nothing else (LoadCollection not even that), so a draft
//     asset served a full anonymous manifest and any signed-in user
//     could read anyone's private collection manifest. Earlier
//     versions of this comment described the state accurately and
//     the state was wrong; see epic #665.
//  2. The IIIF CONTENT-PLANE gate (ADR 0064) — restricted/team
//     assets return 404, embargoed assets return a stub. This is NOT
//     a second copy of the rule in (1), and it is not redundant with
//     it either: the AUTHENTICATED EntityAsset predicate is
//     soft-delete only, so the row plane admits a signed-in caller to
//     a restricted asset's existence and the content plane still
//     decides what its manifest may contain. The row plane decides
//     whether a caller may know an asset EXISTS; the content plane
//     decides what its manifest may CONTAIN. Two planes, two rules,
//     by design — see #432, which settled that this is not
//     redundancy.
//  2. The federated-URI resolver — assets with a non-nil
//     origin_server_id surface their canvas as the remote peer's
//     actor URI per ADR 0043. Local instance never proxies remote
//     bytes; the IIIF client fetches directly.
//  3. The metadata block builder — reads custom-field values,
//     filters by field-definition visibility, honours anonymous
//     hide flags.
//
// Embargoed assets render a "stub manifest" — label + provider +
// requiredStatement only. No canvas details, no metadata leak.
// Per ADR 0020 embargo semantics.
//
// No new outbox events. No CGo. Presentation manifests are JSON
// assembly against local reads + a peer directory lookup for
// federated canvases.
package presentation
