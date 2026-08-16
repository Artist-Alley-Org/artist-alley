-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- name: FollowTag :exec
-- Bookmark a tag into the caller's browse rail (#1123).
--
-- ON CONFLICT DO NOTHING makes follow IDEMPOTENT, for 00041's reason:
-- a double-tapped button, a retried request and a genuine re-follow are
-- one request with one outcome, and the alternative — letting the PK
-- raise 23505 and mapping it to a 409 — makes the client's correctness
-- depend on it knowing a state the server already knows.
--
-- THERE IS NOTHING TO PROBE FIRST, unlike FollowTeam. A tag has no row
-- to be alive or deleted; following one nobody has used yet is legal
-- and inert. See 00050 for why an existence check would be actively
-- wrong here rather than merely unnecessary: the corpus spans posts the
-- caller cannot read, so the probe would be an enumeration oracle.
INSERT INTO tag_follows (user_ref, tag)
VALUES ($1, $2)
ON CONFLICT (user_ref, tag) DO NOTHING;

-- name: UnfollowTag :execrows
-- Drop the caller's bookmark. :execrows rather than :exec so the
-- handler can log the no-op case; the response is 204 either way,
-- because unfollowing something you do not follow has already achieved
-- what the caller asked for.
DELETE FROM tag_follows
WHERE user_ref = $1 AND tag = $2;

-- name: ListFollowedTags :many
-- The caller's followed tags, for the browse rail's `#` chips (#1123).
--
-- Ordered by WHEN THEY WERE FOLLOWED, newest first, and then by the tag
-- for a stable tiebreak. Deliberately not alphabetical, which is what
-- ListFollowedTeams does: a team chip carries an avatar and a name the
-- reader recognises by shape, so alphabetical order is findable, while
-- a row of `#` chips is uniform and the only thing distinguishing a
-- reader's newest interest from their oldest is recency. The reader's
-- own `tag_order` preference overrides this on the client either way —
-- this is the order it is a partial override OF.
--
-- No liveness filter, because there is no liveness: a tag with no posts
-- left simply matches nothing, and the chip stays until the reader
-- removes it. That is the follow being theirs rather than the corpus's.
SELECT tag, created_at
FROM tag_follows
WHERE user_ref = $1
ORDER BY created_at DESC, tag ASC;
