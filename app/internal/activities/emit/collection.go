package emit

import (
	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// CollectionRef is the typed reference an emit helper needs about
// a collection. Handlers build this from their existing collection
// row state.
type CollectionRef struct {
	ID          string // UUID stringified
	Name        string
	Description string
	OwnerRef    int64
	OwnerURI    string
}

// CreateCollection — Create(aa:Collection) per ADR 0043 §7.4.
// No notification — creating your own collection doesn't ping
// anyone. Subscription notifications land alongside aa:Subscribe
// in a future phase.
func CreateCollection(actor ActorContext, coll CollectionRef) Emission {
	actorRef := actor.UserRef
	objectURI := actor.ObjectURI(activities.ObjectKindCollection, coll.ID)
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityCreate,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     objectURI,
				Kind:    activities.ObjectKindCollection,
				LocalID: coll.ID,
			},
			Payload: map[string]any{
				"object_type": "aa:Collection",
				"name":        coll.Name,
				"description": coll.Description,
			},
		},
	}
}

// UpdateCollection — Update activity per AP §6.3 / §7.3. Full
// post-write collection state in the payload; consumers apply
// the same projection logic as CreateCollection.
func UpdateCollection(actor ActorContext, coll CollectionRef) Emission {
	actorRef := actor.UserRef
	objectURI := actor.ObjectURI(activities.ObjectKindCollection, coll.ID)
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityUpdate,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     objectURI,
				Kind:    activities.ObjectKindCollection,
				LocalID: coll.ID,
			},
			Payload: map[string]any{
				"object_type": "aa:Collection",
				"name":        coll.Name,
				"description": coll.Description,
			},
		},
	}
}

// DeleteCollection — Delete with Tombstone semantics per AP §6.4.
func DeleteCollection(actor ActorContext, collectionID, collectionName string) Emission {
	actorRef := actor.UserRef
	objectURI := actor.ObjectURI(activities.ObjectKindCollection, collectionID)
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityDelete,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     objectURI,
				Kind:    activities.ObjectKindCollection,
				LocalID: collectionID,
			},
			Payload: map[string]any{
				"object_type": "aa:Collection",
				"name":        collectionName, // last-known for audit
			},
		},
	}
}

// AddToCollection — Add activity per AP §6.6 / §7.8. `object` is
// the thing being added (a post or asset); `target` is the
// collection it's added to. The receiver MUST own the target
// collection (or have aa:Share access to it) for the activity
// to apply per AP §7.8.
func AddToCollection(
	actor ActorContext,
	addedKind activities.ActivityObjectKind,
	addedLocalID string,
	collectionID, collectionName string,
) Emission {
	actorRef := actor.UserRef
	addedURI := actor.ObjectURI(addedKind, addedLocalID)
	collURI := actor.ObjectURI(activities.ObjectKindCollection, collectionID)
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityAdd,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     addedURI,
				Kind:    addedKind,
				LocalID: addedLocalID,
			},
			Target: &activities.ObjectRef{
				URI:     collURI,
				Kind:    activities.ObjectKindCollection,
				LocalID: collectionID,
			},
			Payload: map[string]any{
				"target_name":     collectionName,
				"add_object_kind": string(addedKind),
			},
		},
	}
}

// RemoveFromCollection — Remove activity per AP §6.7 / §7.9.
// Mirror of AddToCollection.
func RemoveFromCollection(
	actor ActorContext,
	removedKind activities.ActivityObjectKind,
	removedLocalID string,
	collectionID, collectionName string,
) Emission {
	actorRef := actor.UserRef
	removedURI := actor.ObjectURI(removedKind, removedLocalID)
	collURI := actor.ObjectURI(activities.ObjectKindCollection, collectionID)
	return Emission{
		Activity: activities.Input{
			Type:         federation.ActivityRemove,
			ActivityURI:  actor.MintActivityURI(),
			ActorUserRef: &actorRef,
			ActorURI:     actor.URI(),
			Object: &activities.ObjectRef{
				URI:     removedURI,
				Kind:    removedKind,
				LocalID: removedLocalID,
			},
			Target: &activities.ObjectRef{
				URI:     collURI,
				Kind:    activities.ObjectKindCollection,
				LocalID: collectionID,
			},
			Payload: map[string]any{
				"target_name":        collectionName,
				"remove_object_kind": string(removedKind),
			},
		},
	}
}
