-- artist-alley migration 00038 — users.approve capability.
--
-- Phase 1.17.B — lifecycle state machine.
--
-- Splits the "change user.approved" right out of users.write so a
-- future "User Approver" role can move accounts through pending →
-- active → disabled without inheriting the rest of users.write
-- (which also covers role grants, capability grants/revokes, and
-- profile-edit-any). Admin already has system.admin wildcard so it
-- continues to satisfy the new cap automatically; this is purely
-- additive plumbing.

-- +goose Up
-- +goose StatementBegin
INSERT INTO capabilities (code, description)
VALUES ('users.approve', 'Approve, suspend, or restore user accounts (lifecycle state machine)')
ON CONFLICT (code) DO NOTHING;

-- Grant to the Admin role (single-inheritance; Admin already has
-- system.admin wildcard, but explicit grant keeps the role's
-- capability page in sync with the action surface).
INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'users.approve' FROM roles WHERE name = 'admin'
ON CONFLICT DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM role_capabilities WHERE capability_code = 'users.approve';
DELETE FROM capabilities      WHERE code            = 'users.approve';
-- +goose StatementEnd
