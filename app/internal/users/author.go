// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Renderable author identity, for callers OUTSIDE this package (#557).
//
// A feed card draws an author header — a face, a name, and somewhere to
// click. Twenty cards on a page means twenty identities, and there is no
// batch user endpoint outside admin, so `posts` used to have exactly two
// options: an N+1 of `GET /users/{ref}` from the browser, or its own
// second copy of the resolution rules. This file is the third option:
// one expression of "who is this, as far as THIS caller is concerned",
// obtained by both callers rather than restated by either (#665).
//
// The two rules it owns, both from ADR 0070 §3:
//
//  1. An owner who set `hide_from_anonymous` (ADR 0024's opt-out) is not
//     disclosed to an anonymous caller AT ALL. Their profile already
//     404s for that caller; a name and face riding in on someone else's
//     post would be that decision defeated by a JOIN.
//
//  2. Real name is authenticated-only — and that includes the
//     display_name FALLBACK. An anonymous caller's ladder is
//     display_name → username, skipping `fullname`. Getting this wrong
//     leaks nothing to a user who set a display name and leaks the real
//     name of every user who did not, which is the majority.

package users

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ResolveDisplayName is THE display-string rule, for every surface that
// renders a user (#665).
//
// Precedence, best information first:
//
//  1. profile.display_name  — what the user chose to be called
//  2. user.fullname         — AUTHENTICATED CALLERS ONLY (ADR 0070 §3)
//  3. user.username         — the handle, always present in practice
//  4. "user {ref}"          — a row with neither; never rendered blank
//
// Rung 2 is the one that carries a rule rather than a preference, and
// the `anonymous` parameter exists solely to skip it. It is a parameter
// and not a package-level flag because the SAME row is rendered
// differently to two callers in the same process, often in the same
// request (a mixed feed read by an anonymous browser).
//
// Empty `profileDisplayName` and nil pointers are all normal: the
// profile row is a LEFT JOIN, and `user.fullname` is nullable.
func ResolveDisplayName(
	profileDisplayName string,
	fullname *string,
	username *string,
	ref int64,
	anonymous bool,
) string {
	if profileDisplayName != "" {
		return profileDisplayName
	}
	if !anonymous && fullname != nil && *fullname != "" {
		return *fullname
	}
	if username != nil && *username != "" {
		return *username
	}
	return fmt.Sprintf("user %d", ref)
}

// LookupAuthors resolves a SET of user refs into the renderable author
// shape, in ONE query, for one caller.
//
// This is the whole reason the author travels on the post payload. The
// caller hands over every distinct author ref on a page and gets back a
// map; a 20-post feed by 14 authors is one round trip, not 14 and
// certainly not 20. Adding posts to the page cannot add queries.
//
// WITHHOLDING IS AN OMISSION FROM THE MAP, and that is deliberate. A ref
// whose owner opted out of anonymous exposure is simply absent for an
// anonymous caller — there is no "hidden" sentinel to be mistaken for a
// present identity, and no placeholder label. "Someone who opted out
// posted this" still discloses that they posted; the card renders with
// no author header instead. A caller that ranges the map therefore
// cannot accidentally render a withheld user, and a caller that indexes
// it gets the zero value plus `ok == false`.
//
// WHITELIST BY CONSTRUCTION. The SELECT names five columns and the
// struct is built field by field from them. Nothing here trims a fuller
// object down, so a column added to `user` or `user_profiles` later —
// an email, a moderation note, a password reset token — is withheld
// because it was never fetched, not because someone remembered to strip
// it. Same discipline as PostMember's restricted placeholder (#883).
//
// An empty `refs` is not an error and issues no query.
func LookupAuthors(
	ctx context.Context,
	pool *pgxpool.Pool,
	refs []int64,
	anonymous bool,
) (map[int64]openapi.PostAuthor, error) {
	out := make(map[int64]openapi.PostAuthor, len(refs))
	if pool == nil || len(refs) == 0 {
		return out, nil
	}

	// The projection mirrors users/queries.sql's GetUserPublicBy* JOIN —
	// same tables, same LEFT JOIN, same COALESCE on the profile columns —
	// narrowed to the five values an author header can use. `fullname`
	// is fetched because ResolveDisplayName's rung 2 needs it for
	// authenticated callers; it never reaches the response object.
	rows, err := pool.Query(ctx, `
		SELECT u.ref,
		       u.username,
		       u.fullname,
		       COALESCE(p.display_name, '')           AS display_name,
		       p.avatar_url,
		       COALESCE(p.hide_from_anonymous, false) AS hide_from_anonymous
		FROM "user" u
		LEFT JOIN user_profiles p ON p.user_ref = u.ref
		WHERE u.ref = ANY($1::BIGINT[])`, refs)
	if err != nil {
		return nil, fmt.Errorf("users: lookup authors: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			ref         int64
			username    *string
			fullname    *string
			displayName string
			avatarURL   *string
			hidden      bool
		)
		if err := rows.Scan(&ref, &username, &fullname, &displayName, &avatarURL, &hidden); err != nil {
			return nil, fmt.Errorf("users: lookup authors scan: %w", err)
		}
		// The opt-out (ADR 0024 / ADR 0070 §3). Not a redacted entry —
		// no entry.
		if anonymous && hidden {
			continue
		}
		a := openapi.PostAuthor{
			Ref:         ref,
			DisplayName: ResolveDisplayName(displayName, fullname, username, ref, anonymous),
			AvatarUrl:   avatarURL,
		}
		if username != nil {
			a.Username = *username
		}
		out[ref] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("users: lookup authors rows: %w", err)
	}
	return out, nil
}
