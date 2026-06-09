-- artist-alley migration 00006 — extend LISTEN/NOTIFY to the
-- federation_outbox + federation_inbox tables so the full
-- delivery + dispatch pipeline runs event-driven, not ticker-
-- driven. Phase 1.22.D-b-6.
--
-- # Why
--
-- b-1 through b-5 shipped with ticker-only delivery worker +
-- inbox dispatcher (5s baseline each). Combined with the
-- LISTEN/NOTIFY-driven outbox dispatcher from 00005, end-to-
-- end p99 was ~8s (5s delivery tick + 5s inbox tick, in the
-- worst-case interleave). That's "feels like email circa
-- 2003" territory for cross-instance social activities.
--
-- Per the design proposal §3.1 reasoning that justified
-- LISTEN/NOTIFY on activities in the first place: the same
-- "user-facing latency matters" argument applies to outbox
-- + inbox. There's no principled distinction between
-- "activity needs to be dispatched fast" and "outbox row
-- needs to be delivered fast" and "inbox row needs to be
-- processed fast." All three are queue-shaped tables on the
-- path from a user clicking Like to another user seeing it.
--
-- This migration adds the trigger half of the extension. The
-- Go-side LISTEN goroutines + ticker-drop-to-30s land in
-- 1.22.D-b-6's Go diff alongside.
--
-- # Performance contract (locked in spec v1.md §3 by b-6)
--
-- - p50: sub-100ms end-to-end (commit → recipient-visible
--   side effect)
-- - p99: sub-1s in the happy path
-- - Tickers are CORRECTNESS BACKSTOP ONLY at 30s on each
--   layer (activities dispatcher, outbox delivery, inbox
--   dispatcher).

-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION federation_outbox_dispatch_notify()
RETURNS TRIGGER AS $$
BEGIN
    PERFORM pg_notify('federation_outbox_pending', NEW.id::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER federation_outbox_dispatch_notify_trg
    AFTER INSERT ON federation_outbox
    FOR EACH ROW
    EXECUTE FUNCTION federation_outbox_dispatch_notify();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION federation_inbox_dispatch_notify()
RETURNS TRIGGER AS $$
BEGIN
    PERFORM pg_notify('federation_inbox_pending', NEW.id::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER federation_inbox_dispatch_notify_trg
    AFTER INSERT ON federation_inbox
    FOR EACH ROW
    EXECUTE FUNCTION federation_inbox_dispatch_notify();

-- +goose Down

DROP TRIGGER IF EXISTS federation_outbox_dispatch_notify_trg ON federation_outbox;
DROP FUNCTION IF EXISTS federation_outbox_dispatch_notify();
DROP TRIGGER IF EXISTS federation_inbox_dispatch_notify_trg ON federation_inbox;
DROP FUNCTION IF EXISTS federation_inbox_dispatch_notify();
