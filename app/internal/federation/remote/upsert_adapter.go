// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Adapter that bridges federation/remote's Queries + the
// I-c Handler to the inbox dispatcher's RemoteActorUpserter
// contract.
//
// Phase 1.22.D-a-4-dispatch (display-info upsert) +
// Phase 1.22.I-c-3   (encryption-key upsert + change-detected audit).
//
// federation/inbox owns the RemoteActorUpserter contract;
// federation/remote imports federation/inbox to satisfy it via
// the EncryptionKeyInline type. Implementor-imports-interface
// is the conventional Go direction; the previous "small
// primitive-only interface" shape didn't need the import, but
// the encryption-key parameter type pulls the import in.

package remote

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

// Upserter implements inbox.RemoteActorUpserter via the sqlc-
// generated Queries (display info) + the I-c Handler
// (encryption-key cache + change-detected audit).
//
// Construct via NewUpserter. Both the Handler and the
// audit.Recorder are optional — when nil, the upserter still
// does the display-info write but skips encryption-key
// processing (keeps boot-without-cache-or-audit configurations
// working for tests + degraded modes).
type Upserter struct {
	q        *Queries
	handler  *Handler
	recorder *audit.Recorder
	logger   *slog.Logger
}

// NewUpserter wires the package's contributions. The Handler +
// Recorder + logger are all optional (any may be nil); the
// display-info upsert path works regardless.
func NewUpserter(pool *pgxpool.Pool, h *Handler, rec *audit.Recorder, logger *slog.Logger) *Upserter {
	return &Upserter{q: New(pool), handler: h, recorder: rec, logger: logger}
}

// Upsert satisfies the inbox.RemoteActorUpserter contract.
//
// Display-info upsert ALWAYS runs (this is the legacy
// 1.22.D-a-4 behaviour). When encKey != nil — i.e. the inbound
// envelope carried an aa:encryptionPublicKey block — also writes
// the key block via Handler.SetEncryptionKey, and on a real
// change (first-time advertisement or rotation) emits
// federation.remote_actor.key_updated.
//
// Failures on the encryption-key path log + swallow — the
// dispatch decision shouldn't hinge on a key-cache write. The
// display-info path's failures still propagate so a missing
// row gets surfaced.
func (u *Upserter) Upsert(
	ctx context.Context,
	actorURI string,
	peerID uuid.UUID,
	displayName, avatarURL string,
	encKey *inbox.EncryptionKeyInline,
) error {
	if _, err := u.q.UpsertRemoteActor(ctx, UpsertRemoteActorParams{
		ActorUri:    actorURI,
		PeerID:      pgtype.UUID{Bytes: peerID, Valid: true},
		DisplayName: displayName,
		AvatarUrl:   avatarURL,
	}); err != nil {
		return err
	}

	if encKey == nil || u.handler == nil {
		return nil
	}

	changed, prevVersion, err := u.handler.SetEncryptionKey(ctx, actorURI, encKey.PublicKey, encKey.Version)
	if err != nil {
		// Most likely cause: ErrNoActor (the display upsert
		// above should have created the row, so this is rare —
		// would mean a concurrent DELETE landed between the
		// two queries). Log + swallow so the activity dispatch
		// still succeeds.
		if u.logger != nil {
			u.logger.LogAttrs(ctx, slog.LevelWarn, "inbox.remote_actor.encryption_key_set.error",
				slog.String("actor", actorURI),
				slog.String("err", err.Error()),
			)
		}
		return nil
	}

	if changed && u.recorder != nil {
		u.recorder.FederationRemoteActorKeyUpdated(ctx, actorURI, peerID.String(), prevVersion, encKey.Version)
	}
	return nil
}

// Compile-time check: a future drift in the inbox.RemoteActorUpserter
// signature will fail at build instead of slipping past test
// coverage.
var _ inbox.RemoteActorUpserter = (*Upserter)(nil)
