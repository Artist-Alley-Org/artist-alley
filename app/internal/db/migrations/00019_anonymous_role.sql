-- artist-alley migration 00019 — seed the Anonymous role.
--
-- See ADR 0010 Layer 7b.
--
-- The Anonymous role is the synthetic principal the auth middleware
-- will inject when a request arrives with no session and no API token
-- (gated on the system.anonymous_browse_enabled flag — wired in
-- Phase 1.13.G). It carries no capabilities by default; turning on
-- anonymous browse is "grant 'posts.read.public' (or whichever caps
-- you want to expose) to the Anonymous role" plus flipping the flag.
--
-- The role row is NOT given to any user — its sole consumer is the
-- middleware. Listing roles via /admin/roles will show it; the admin
-- UI should mark it special so nobody assigns it.

-- +goose Up

INSERT INTO roles (name, description)
VALUES ('Anonymous', 'Synthetic role for unauthenticated requests; caps gate which public surfaces anonymous users may read')
ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description;

-- +goose Down

DELETE FROM roles WHERE name = 'Anonymous';
