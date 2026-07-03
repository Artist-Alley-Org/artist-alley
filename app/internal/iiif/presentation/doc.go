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
//  1. The shared visibility gate — the presentation subsystem
//     ADDS its own sensitivity check ON TOP OF visibility.Filter
//     because Phase 1.54.A's anonymous visibility for assets is
//     soft-delete only (pre-audit Q5 finding). Restricted +
//     embargoed assets are gated at the IIIF layer regardless of
//     what the shared filter allows.
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
