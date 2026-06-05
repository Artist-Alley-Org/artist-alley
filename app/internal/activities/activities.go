// Writer + typed input shapes for the activities ledger.
//
// See doc.go for the package invariant + the docs/spec/federation/
// v1.md cross-reference for the wire format.

package activities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// ActivityObjectKind discriminates the local-projection table the
// object_local_id refers to. Broader than federation.ObjectKind
// (which is restricted to SHARABLE kinds because federation_shares
// only holds those) — activities can be about ANY local object
// including comments, DMs, and other activities (Undo's target).
//
// Mirrored by the CHECK constraint in migration 00049 per ADR 0042.
type ActivityObjectKind string

const (
	ObjectKindPost       ActivityObjectKind = "post"
	ObjectKindComment    ActivityObjectKind = "comment"
	ObjectKindAsset      ActivityObjectKind = "asset"
	ObjectKindUser       ActivityObjectKind = "user"
	ObjectKindCollection ActivityObjectKind = "collection"
	ObjectKindWorkspace  ActivityObjectKind = "workspace"
	ObjectKindBrandKit   ActivityObjectKind = "brand_kit"
	ObjectKindMessage    ActivityObjectKind = "message"
	ObjectKindActivity   ActivityObjectKind = "activity" // for Undo's target activity
)

// KnownObjectKinds is the closed catalogue; matches the migration
// CHECK. New values land in both the const block + this map + the
// CHECK in one PR per ADR 0042.
var KnownObjectKinds = map[ActivityObjectKind]struct{}{
	ObjectKindPost:       {},
	ObjectKindComment:    {},
	ObjectKindAsset:      {},
	ObjectKindUser:       {},
	ObjectKindCollection: {},
	ObjectKindWorkspace:  {},
	ObjectKindBrandKit:   {},
	ObjectKindMessage:    {},
	ObjectKindActivity:   {},
}

// Valid reports whether k is in the closed catalogue.
func (k ActivityObjectKind) Valid() bool {
	_, ok := KnownObjectKinds[k]
	return ok
}

// SourceLocal is the source string for activities emitted by this
// instance. Peer-sourced activities use the peer's instance URL
// directly as the source value (e.g. "https://studio-b.example").
const SourceLocal = "local"

// Errors callers may distinguish on.
var (
	// ErrInvalidActivityType is returned when Input.Type is not in
	// the federation.ActivityType catalogue.
	ErrInvalidActivityType = errors.New("activities: activity type not in the closed catalogue")

	// ErrInvalidObjectKind is returned when Input.Object.Kind is
	// set but not in the local ActivityObjectKind catalogue.
	ErrInvalidObjectKind = errors.New("activities: object kind not in the closed catalogue")

	// ErrMissingActor is returned when neither ActorUserRef nor
	// ActorURI is supplied — local emits need both; peer emits
	// need at least the URI.
	ErrMissingActor = errors.New("activities: actor identity required (URI for peer, both for local)")
)

// ObjectRef is the typed reference to whatever the activity is
// about. URI is the cross-instance handle; Kind + LocalID are the
// shortcut for local-projection JOINs.
type ObjectRef struct {
	URI     string
	Kind    ActivityObjectKind
	LocalID string
}

// Input is the typed argument to RecordActivity. Per-activity-type
// emit helpers in the same package build these with the right
// defaults for each type; callers writing one-off activities
// construct directly.
type Input struct {
	// Type — required. One of federation.ActivityType.
	Type federation.ActivityType

	// ActivityURI — required. The cross-instance handle. Callers
	// build this from baseURL + a fresh UUID; emit helpers do it
	// automatically.
	ActivityURI string

	// Actor identity. ActorUserRef + ActorURI for local emits;
	// just ActorURI for peer-received activities (where the actor
	// is a remote user we don't have a local ref for).
	ActorUserRef *int64
	ActorURI     string

	// Object the activity is about. Optional only for activities
	// where AP itself omits the object (rare; e.g. some Tombstone
	// flows). Per-type emit helpers set this correctly.
	Object *ObjectRef

	// Target — used by Add/Remove activities (the collection the
	// object is added to / removed from). Nil for most types.
	Target *ObjectRef

	// Addressing per AP §6.1. Empty slices = "no recipient in
	// this category." bto/bcc are kept in the local ledger row
	// even though they're stripped before federation delivery —
	// the originating instance still answers DSAR queries.
	To, CC, BTo, BCC, Audience []string

	// Payload is the full AP envelope WITHOUT the signature field.
	// Marshaled to JSONB at insert. Emit helpers build this from
	// the typed Input + their per-type extras.
	Payload map[string]any

	// Source — defaults to SourceLocal. Set to a peer instance
	// URL when ingesting from a peer's inbox delivery (Phase
	// 1.22.D).
	Source string

	// Published — defaults to NOW. Set explicitly for peer-
	// received activities (use the envelope's `published` field).
	Published time.Time
}

// Writer is the package's central state — the pool (for non-
// transactional reads), the logger, and the per-actor outbox
// cache. Constructed once at boot; safe for concurrent use.
type Writer struct {
	Pool        *pgxpool.Pool
	Logger      *slog.Logger
	registry    *cache.Registry
	outboxCache *cache.Cache[CachedOutbox]
}

// CachedOutbox is the per-actor recent-feed projection held in
// the LRU. Refreshed on every successful local RecordActivity for
// the actor; invalidated across federated peers via cache.Registry
// NOTIFY.
//
// Holds just the most recent N activity URIs + types — enough to
// answer the admin "what's actor X been up to" snapshot without a
// DB hit. The full activity rows live in the DB; this is a hot-
// feed convenience.
type CachedOutbox struct {
	Entries []OutboxEntry
}

// OutboxEntry is one row of CachedOutbox.
type OutboxEntry struct {
	ActivityURI string
	Type        federation.ActivityType
	ObjectURI   string
	PublishedAt time.Time
}

const cacheDomainActorOutbox = "activities.actor_outbox"

// NewWriter wires the writer. registry can be nil (no caching;
// every read hits the DB). Recommended size is calibrated for the
// federation hot path — 5k actor-feeds at ~2KB each ≈ 10MB
// resident.
func NewWriter(pool *pgxpool.Pool, logger *slog.Logger, registry *cache.Registry) *Writer {
	w := &Writer{Pool: pool, Logger: logger, registry: registry}
	if registry != nil {
		w.outboxCache = cache.Register[CachedOutbox](registry, cacheDomainActorOutbox, 5_000)
	}
	return w
}

func actorOutboxKey(userRef int64) string { return strconv.FormatInt(userRef, 10) }

// RecordActivity inserts an activity in the supplied transaction.
// MUST be called inside the same pgx.Tx as the caller's domain
// write — see ADR 0044's invariant. Returns the persisted Record,
// including the canonical ID + created_at.
//
// Idempotent: a second call with the same ActivityURI returns the
// pre-existing row unchanged (the underlying INSERT uses ON CONFLICT
// DO NOTHING per the queries.sql contract).
//
// After a successful insert, the per-actor outbox cache is
// invalidated. The invalidation rides cache.Registry NOTIFY so
// federated peers' in-process caches stay coherent.
//
// nil-tx is rejected — callers MUST supply a real transaction.
// We do not silently start one because that would defeat the
// "domain write + activity write commit together" invariant.
func (w *Writer) RecordActivity(ctx context.Context, tx pgx.Tx, in Input) (*Record, error) {
	if tx == nil {
		return nil, errors.New("activities: RecordActivity requires a non-nil transaction (ADR 0044 invariant)")
	}
	if err := validateInput(&in); err != nil {
		return nil, err
	}
	if in.Source == "" {
		in.Source = SourceLocal
	}
	if in.Published.IsZero() {
		in.Published = time.Now().UTC()
	}

	// Marshal payload + addressing slices to JSONB-compatible
	// bytes. Empty addressing fields become `[]` not `null` so
	// query consumers don't have to NULL-check.
	payloadJSON, err := json.Marshal(in.Payload)
	if err != nil {
		return nil, fmt.Errorf("activities: marshal payload: %w", err)
	}
	if len(in.Payload) == 0 {
		payloadJSON = []byte("{}")
	}
	toJSON, _ := json.Marshal(stringSliceOrEmpty(in.To))
	ccJSON, _ := json.Marshal(stringSliceOrEmpty(in.CC))
	btoJSON, _ := json.Marshal(stringSliceOrEmpty(in.BTo))
	bccJSON, _ := json.Marshal(stringSliceOrEmpty(in.BCC))
	audJSON, _ := json.Marshal(stringSliceOrEmpty(in.Audience))

	var objectURI, objectLocalID *string
	var objectKind *string
	if in.Object != nil {
		if in.Object.URI != "" {
			s := in.Object.URI
			objectURI = &s
		}
		if in.Object.LocalID != "" {
			s := in.Object.LocalID
			objectLocalID = &s
		}
		if in.Object.Kind != "" {
			s := string(in.Object.Kind)
			objectKind = &s
		}
	}
	var targetURI *string
	if in.Target != nil && in.Target.URI != "" {
		s := in.Target.URI
		targetURI = &s
	}

	q := New(tx)
	row, err := q.InsertActivity(ctx, InsertActivityParams{
		ActivityUri:    in.ActivityURI,
		ActivityType:   string(in.Type),
		ActorUri:       in.ActorURI,
		ActorUserRef:   in.ActorUserRef,
		ObjectUri:      objectURI,
		ObjectKind:     objectKind,
		ObjectLocalID:  objectLocalID,
		TargetUri:      targetURI,
		ToUris:         toJSON,
		CcUris:         ccJSON,
		BtoUris:        btoJSON,
		BccUris:        bccJSON,
		AudienceUris:   audJSON,
		Payload:        payloadJSON,
		Source:         in.Source,
		PublishedAt:    pgtype.Timestamptz{Time: in.Published, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("activities: insert: %w", err)
	}

	// The InsertActivity RETURNING shape (InsertActivityRow) has
	// the same field set as the Activity table type but is a
	// distinct Go type from sqlc's POV. Adapt in place — cheap
	// since field copies are bytewise primitives.
	out := rowToRecord(Activity{
		ID:              row.ID,
		ActivityUri:     row.ActivityUri,
		ActivityType:    row.ActivityType,
		ActorUri:        row.ActorUri,
		ActorUserRef:    row.ActorUserRef,
		ObjectUri:       row.ObjectUri,
		ObjectKind:      row.ObjectKind,
		ObjectLocalID:   row.ObjectLocalID,
		TargetUri:       row.TargetUri,
		ToUris:          row.ToUris,
		CcUris:          row.CcUris,
		BtoUris:         row.BtoUris,
		BccUris:         row.BccUris,
		AudienceUris:    row.AudienceUris,
		Payload:         row.Payload,
		SignatureValue:  row.SignatureValue,
		SignaturePubkey: row.SignaturePubkey,
		Source:          row.Source,
		PublishedAt:     row.PublishedAt,
		CreatedAt:       row.CreatedAt,
	})

	// Cache invalidation: only invalidate when this was a local
	// insert with an actor_user_ref. Peer-source rows don't show
	// in the local actor's outbox (different actor URI namespace).
	if in.Source == SourceLocal && in.ActorUserRef != nil && w.outboxCache != nil {
		if err := w.outboxCache.Invalidate(ctx, actorOutboxKey(*in.ActorUserRef)); err != nil && w.Logger != nil {
			w.Logger.Warn("activities.cache.invalidate.error",
				"actor_user_ref", *in.ActorUserRef,
				"err", err.Error(),
			)
		}
	}
	return out, nil
}

// MintActivityURI generates a fresh cross-instance activity handle
// per docs/spec/federation/v1.md §8.1. Emit helpers call this so
// callers don't have to remember the URL shape.
func MintActivityURI(baseURL string) string {
	return baseURL + "/activities/" + uuid.New().String()
}

// Record is the in-memory representation of one ledger row.
// Public so cross-package consumers (federation outbox dispatcher
// in 1.22.D, admin audit UI in 1.22.A-bis-3) can hold + pass it
// without going through the raw sqlc row type (which is named
// Activity per the table — we use Record here to avoid the name
// collision and to make the "ledger row" meaning explicit).
type Record struct {
	ID              uuid.UUID
	ActivityURI     string
	Type            federation.ActivityType
	ActorURI        string
	ActorUserRef    *int64
	ObjectURI       string
	ObjectKind      ActivityObjectKind
	ObjectLocalID   string
	TargetURI       string
	To, CC, BTo, BCC, Audience []string
	Payload         map[string]json.RawMessage
	SignatureValue  string
	SignaturePubkey string
	Source          string
	PublishedAt     time.Time
	CreatedAt       time.Time
}

// --- helpers --------------------------------------------------------------

func validateInput(in *Input) error {
	if !in.Type.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidActivityType, in.Type)
	}
	if in.ActivityURI == "" {
		return errors.New("activities: ActivityURI required (use MintActivityURI to generate)")
	}
	if in.ActorURI == "" {
		return ErrMissingActor
	}
	if in.Object != nil && in.Object.Kind != "" && !in.Object.Kind.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidObjectKind, in.Object.Kind)
	}
	return nil
}

func stringSliceOrEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// rowToRecord adapts the sqlc-generated Activity row type into
// the package's public Record type. Centralises the field-name +
// JSONB-decode plumbing in one place so callers don't have to do
// it.
func rowToRecord(r Activity) *Record {
	out := &Record{
		ID:              uuid.UUID(r.ID.Bytes),
		ActivityURI:     r.ActivityUri,
		Type:            federation.ActivityType(r.ActivityType),
		ActorURI:        r.ActorUri,
		ActorUserRef:    r.ActorUserRef,
		ObjectURI:       derefStr(r.ObjectUri),
		ObjectKind:      ActivityObjectKind(derefStr(r.ObjectKind)),
		ObjectLocalID:   derefStr(r.ObjectLocalID),
		TargetURI:       derefStr(r.TargetUri),
		SignatureValue:  derefStr(r.SignatureValue),
		SignaturePubkey: derefStr(r.SignaturePubkey),
		Source:          r.Source,
		PublishedAt:     r.PublishedAt.Time,
		CreatedAt:       r.CreatedAt.Time,
	}
	_ = json.Unmarshal(r.ToUris, &out.To)
	_ = json.Unmarshal(r.CcUris, &out.CC)
	_ = json.Unmarshal(r.BtoUris, &out.BTo)
	_ = json.Unmarshal(r.BccUris, &out.BCC)
	_ = json.Unmarshal(r.AudienceUris, &out.Audience)
	_ = json.Unmarshal(r.Payload, &out.Payload)
	return out
}
