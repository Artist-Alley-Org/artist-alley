// Adapter that bridges federation/remote's Queries to the
// inbox dispatcher's RemoteActorUpserter contract.
// Phase 1.22.D-a-4-dispatch.
//
// Keeps the import edge one-directional: federation/inbox
// depends on a small interface; federation/remote provides the
// implementation via this adapter.

package remote

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Upserter implements inbox.RemoteActorUpserter via the sqlc-
// generated Queries. Construct with NewUpserter(pool).
type Upserter struct {
	q *Queries
}

func NewUpserter(pool *pgxpool.Pool) *Upserter {
	return &Upserter{q: New(pool)}
}

// Upsert satisfies the inbox.RemoteActorUpserter contract.
func (u *Upserter) Upsert(ctx context.Context, actorURI string, peerID uuid.UUID, displayName, avatarURL string) error {
	_, err := u.q.UpsertRemoteActor(ctx, UpsertRemoteActorParams{
		ActorUri:    actorURI,
		PeerID:      pgtype.UUID{Bytes: peerID, Valid: true},
		DisplayName: displayName,
		AvatarUrl:   avatarURL,
	})
	return err
}
