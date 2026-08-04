// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Stub per-verb handlers for the verbs that DON'T get a real
// implementation in 1.22.D-a-4 per the §5.5 Q1 lock-in.
//
// Each stub records the row as processed without a domain
// write + logs at debug level so an operator can grep for
// "skipping" while real handlers land per-domain.
//
// As real handlers land, replace `RegisterStubs` calls with
// the real `Like/CommentHandler`-style functions in the
// dispatcher boot.

package inbox

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// stubHandler returns a HandlerFn that just records the row as
// processed. Used for verbs where the wire-layer landing is
// enough until the per-domain follow-up lands.
func stubHandler(verb federation.ActivityType, logger *slog.Logger) HandlerFn {
	return func(ctx context.Context, env *federation.Envelope, peerID uuid.UUID, _ string, row FederationInbox) (DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
		if logger != nil {
			logger.LogAttrs(ctx, slog.LevelDebug, "inbox.dispatcher.stub_dispatch",
				slog.String("activity_type", string(verb)),
				slog.String("inbox_id", uuid.UUID(row.ID.Bytes).String()),
				slog.String("actor_uri", env.Actor),
			)
		}
		return OutcomeProcessed, "", uuid.UUID{}, nil
	}
}

// BuildRegistry returns the per-verb handler map. REAL handlers
// for Like + Create per §5.5 Q1; stubs for the rest. Boot
// passes the result to NewDispatcher; tests can also call this
// + then override individual entries before wiring the
// dispatcher.
//
// NOTE: the dispatcher applies aa:RevokeShare → aa:Unshare
// normalization BEFORE registry lookup, so we don't need an
// entry for aa:RevokeShare here.
func BuildRegistry(d *Dispatcher, logger *slog.Logger) map[federation.ActivityType]HandlerFn {
	return map[federation.ActivityType]HandlerFn{
		// REAL handlers (1.22.D-a-4 Q1).
		federation.ActivityLike:   LikeHandler(d),
		federation.ActivityCreate: CommentHandler(d),

		// Stubs — wire-layer landing only; per-domain
		// follow-up phases land the real handlers.
		federation.ActivityFollow:               stubHandler(federation.ActivityFollow, logger),
		federation.ActivityAccept:               stubHandler(federation.ActivityAccept, logger),
		federation.ActivityReject:               stubHandler(federation.ActivityReject, logger),
		federation.ActivityUndo:                 stubHandler(federation.ActivityUndo, logger),
		federation.ActivityAnnounce:             stubHandler(federation.ActivityAnnounce, logger),
		federation.ActivityBlock:                stubHandler(federation.ActivityBlock, logger),
		// #882 — when ActivityAdd stops being a stub and actually
		// pins an object into a local collection, it MUST apply the
		// same asset gate the HTTP path applies
		// (collections.Handler.mayCollectAsset): the ROW plane
		// (visibility.CanSee, EntityAsset) AND the CONTENT plane
		// (visibility.CanReadContent, ADR 0064), with "unreadable"
		// and "nonexistent" producing the SAME outcome. A remote
		// actor must not be able to collect what a local one cannot,
		// and the inbox is the easier surface to forget.
		federation.ActivityAdd:                  stubHandler(federation.ActivityAdd, logger),
		federation.ActivityRemove:               stubHandler(federation.ActivityRemove, logger),
		federation.ActivityAAShare:              stubHandler(federation.ActivityAAShare, logger),
		federation.ActivityAAUnshare:            stubHandler(federation.ActivityAAUnshare, logger),
		federation.ActivityAAApprove:            stubHandler(federation.ActivityAAApprove, logger),
		federation.ActivityAARequestChanges:     stubHandler(federation.ActivityAARequestChanges, logger),
		federation.ActivityAAMarkReviewed:       stubHandler(federation.ActivityAAMarkReviewed, logger),
		federation.ActivityAAAnnotation:         stubHandler(federation.ActivityAAAnnotation, logger),
		federation.ActivityAAWorkflowTransition: stubHandler(federation.ActivityAAWorkflowTransition, logger),
		federation.ActivityAAAssetVersion:       stubHandler(federation.ActivityAAAssetVersion, logger),
		federation.ActivityAASubscribe:          stubHandler(federation.ActivityAASubscribe, logger),
		federation.ActivityAAMention:            stubHandler(federation.ActivityAAMention, logger),
		// Delete + Update are object-kind dispatched in their
		// own per-domain phases; stubbed here.
		federation.ActivityDelete: stubHandler(federation.ActivityDelete, logger),
		federation.ActivityUpdate: stubHandler(federation.ActivityUpdate, logger),
	}
}
