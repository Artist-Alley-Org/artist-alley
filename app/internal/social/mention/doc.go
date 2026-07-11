// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package mention parses @username mentions out of user-authored body
// text (post titles/descriptions, comment bodies) and resolves them to
// local user refs so the write handlers can fire mention_of_me
// notifications.
//
// Two stages, deliberately split:
//
//	ParseMentions(text) []Mention   — pure, no I/O. Extracts @username
//	    tokens, excluding matches inside code fences / inline code and
//	    inside markdown or bare links (Slack behaviour: a @user pasted
//	    in a code snippet is not a mention).
//
//	Resolver.ResolveLocal(ctx, []Mention) []int64 — maps local
//	    usernames to user refs via a 5-minute cache. Unknown usernames
//	    drop silently (no error). Federated mentions (InstanceHost != "")
//	    are ignored for v0.1.0.
//
// # Federation seam
//
// Mention carries an InstanceHost field that is always "" today. When
// federation lands (post-Phase-1.30), the parser learns the
// @user@peer.com grammar and populates InstanceHost, and the resolver
// grows a ResolveFederated sibling that resolves via WebFinger. The
// Mention struct + ParseMentions signature do NOT change — this is the
// cheap federation prep the roadmap calls for.
//
// # Non-goals (v0.1.0)
//
//   - No un-mention audit: editing a mention out of a post does not
//     retract an already-fired notification.
//   - No self-mention filtering here: the notifications.Writer already
//     gates actor==recipient, so the resolver keeps self-refs and the
//     Writer drops them.
//   - No cross-instance resolution.
package mention
