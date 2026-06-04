-- artist-alley migration 00041 — system.audit.read capability seed
-- (Phase 1.17.K).
--
-- The admin audit viewer (GET /admin/audit) is read-only and a
-- separate concern from the existing system.config / system.auth
-- caps — a future "auditor" or "compliance" role might want read
-- access to the audit log without being able to mutate anything.
-- Splitting the cap up front keeps that door open. system.admin
-- holders bypass via Identity.Can's wildcard.

-- +goose Up

INSERT INTO capabilities (code, description) VALUES
    ('system.audit.read', 'Read the system-wide audit event log via the admin viewer')
ON CONFLICT (code) DO NOTHING;

-- +goose Down

DELETE FROM capabilities WHERE code = 'system.audit.read';
