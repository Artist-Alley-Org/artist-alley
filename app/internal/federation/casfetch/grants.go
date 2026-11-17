// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package casfetch

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
)

// DatabaseGrants is the production InboundGrants: the receiver's
// inbound grant record (#1571), read on every call. A fetch of the
// original class needs a live grant with allow_original; the signer's
// actor (or, for an instance-signed fetch, only a peer-wide grant) is
// the recipient asking.
type DatabaseGrants struct{ Store *inboundgrants.Store }

var _ InboundGrants = DatabaseGrants{}

func grantQuery(q GrantQuery) inboundgrants.Query {
	return inboundgrants.Query{PeerID: q.PeerID, ActorURI: q.ActorURI, ObjectURI: q.OriginAssetURI, Class: q.Class}
}

// Allows refuses with ErrNoInboundGrant unless a live grant covers the
// fetch. A database error is a refusal too.
func (g DatabaseGrants) Allows(ctx context.Context, q GrantQuery) error {
	if _, err := g.Store.Allows(ctx, grantQuery(q)); err != nil {
		return fmt.Errorf("%w: %w", ErrNoInboundGrant, err)
	}
	return nil
}

// AllowsTx is Allows inside tx, holding the matched grant FOR SHARE
// until tx ends.
func (g DatabaseGrants) AllowsTx(ctx context.Context, tx pgx.Tx, q GrantQuery) error {
	if _, err := g.Store.AllowsTx(ctx, tx, grantQuery(q)); err != nil {
		return fmt.Errorf("%w: %w", ErrNoInboundGrant, err)
	}
	return nil
}
