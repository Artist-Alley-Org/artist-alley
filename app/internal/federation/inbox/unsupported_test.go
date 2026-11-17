// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1569 fail-honestly gate: an activity type this release does not
// apply is recorded as rejected (unsupported_activity) with its
// envelope, never processed, never retried, and answered the same way
// on the singleton endpoint, the batch endpoint and every duplicate
// delivery. All DB-backed tests drive the production PostInbox /
// PostInboxBatch handlers and the production dispatcher.

package inbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
)

// --- helpers ----------------------------------------------------------

// envelopeOf returns a v1 envelope of the given type that passes every
// receive-time gate except, possibly, the applied-type gate. object is
// an activity URL on the peer (what Undo / Delete / aa:Unshare carry),
// which the object-ref check classifies as "not indexed", not unknown.
func (fx *fixture) envelopeOf(t federation.ActivityType, id string) []byte {
	if id == "" {
		id = fx.peerURL + "/activities/" + uuid.NewString()
	}
	env := map[string]any{
		"@context":  federation.ContextV1,
		"type":      string(t),
		"id":        id,
		"actor":     fx.peerURL + "/users/alice",
		"published": time.Now().UTC().Format(time.RFC3339),
		"object":    fx.peerURL + "/activities/" + uuid.NewString(),
		"signature": map[string]string{
			"type":      "Ed25519",
			"publicKey": fx.peerURL + "/users/alice#main-key",
			"value":     "AAAAAAAAAAAA",
		},
	}
	b, err := json.Marshal(env)
	if err != nil {
		fx.t.Fatal(err)
	}
	return fx.signEnvelope(b)
}

func activityIDOf(t *testing.T, envelope []byte) string {
	t.Helper()
	var head struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(envelope, &head); err != nil || head.ID == "" {
		t.Fatalf("envelope id: %v", err)
	}
	return head.ID
}

// newSignedBatchRequest builds a signed POST /federation/inbox/batch.
func (fx *fixture) newSignedBatchRequest(envelopes ...[]byte) *http.Request {
	raws := make([]json.RawMessage, len(envelopes))
	for i, e := range envelopes {
		raws[i] = e
	}
	body, err := json.Marshal(map[string]any{"envelopes": raws})
	if err != nil {
		fx.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost,
		"https://local.example/federation/inbox/batch", bytes.NewReader(body))
	if err != nil {
		fx.t.Fatal(err)
	}
	req.Header.Set("Host", "local.example")
	if err := httpsig.SignAndAttach(req, body, fx.peerKeyID, fx.peerPriv); err != nil {
		fx.t.Fatal(err)
	}
	return req
}

type batchResultT struct {
	ActivityURI string `json:"activity_uri"`
	Status      string `json:"status"`
	Reason      string `json:"reason"`
}

func (fx *fixture) postBatch(envelopes ...[]byte) []batchResultT {
	fx.t.Helper()
	rr := httptest.NewRecorder()
	fx.handler.PostInboxBatch(rr, fx.newSignedBatchRequest(envelopes...))
	if rr.Code != http.StatusOK {
		fx.t.Fatalf("batch: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Results []batchResultT `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		fx.t.Fatalf("batch body: %v (%s)", err, rr.Body.String())
	}
	if len(resp.Results) != len(envelopes) {
		fx.t.Fatalf("batch results: got %d want %d", len(resp.Results), len(envelopes))
	}
	return resp.Results
}

func (fx *fixture) postSingle(envelope []byte) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, fx.newSignedRequest(envelope))
	return rr
}

// storedRow reads the inbox row for an activity id. ok=false when none.
func (fx *fixture) storedRow(activityURI string) (inbox.FederationInbox, bool) {
	fx.t.Helper()
	row, err := fx.q.GetInboxByActivityURI(context.Background(), activityURI)
	if err != nil {
		return inbox.FederationInbox{}, false
	}
	return row, true
}

func (fx *fixture) countRows(activityURI string) int {
	fx.t.Helper()
	var n int
	if err := fx.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM federation_inbox WHERE activity_uri = $1`, activityURI,
	).Scan(&n); err != nil {
		fx.t.Fatal(err)
	}
	return n
}

// assertStoredUnsupported checks the honest terminal state: rejected
// with unsupported_activity, never dispatched or processed, envelope
// kept (jsonb, so compared as JSON values rather than bytes).
func (fx *fixture) assertStoredUnsupported(activityURI string, sent []byte) {
	fx.t.Helper()
	row, ok := fx.storedRow(activityURI)
	if !ok {
		fx.t.Fatalf("no inbox row for %s", activityURI)
	}
	if row.Status != "rejected" {
		fx.t.Errorf("status: got %q want rejected", row.Status)
	}
	if row.RejectReason == nil || *row.RejectReason != string(federation.InboxStatusUnsupportedActivity) {
		fx.t.Errorf("reject_reason: got %v want unsupported_activity", row.RejectReason)
	}
	if row.ProcessedAt.Valid {
		fx.t.Error("processed_at is set; an unsupported activity must never be processed")
	}
	if row.DispatchAttempts != 0 {
		fx.t.Errorf("dispatch_attempts: got %d want 0 (never dispatched)", row.DispatchAttempts)
	}
	if row.LastError == "" {
		fx.t.Error("last_error empty; the operator-visible reason is missing")
	}
	if row.HttpSigKey != fx.peerKeyID || uuid.UUID(row.PeerID.Bytes) != fx.peerID {
		fx.t.Errorf("row not attributed to the authenticated peer: key=%q peer=%v", row.HttpSigKey, uuid.UUID(row.PeerID.Bytes))
	}
	var got, want any
	if err := json.Unmarshal(row.EnvelopeJson, &got); err != nil {
		fx.t.Fatalf("stored envelope: %v", err)
	}
	if err := json.Unmarshal(sent, &want); err != nil {
		fx.t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		fx.t.Errorf("stored envelope differs from the received one\n got: %s\nwant: %s", row.EnvelopeJson, sent)
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func (fx *fixture) newDispatcher(registry map[federation.ActivityType]inbox.HandlerFn) *inbox.Dispatcher {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := inbox.NewDispatcher(
		inbox.DispatcherConfig{Interval: time.Hour, BatchSize: 100, MaxAttempts: 5},
		fx.q,
		func(_ context.Context, id uuid.UUID) (inbox.PeerInfo, error) {
			return inbox.PeerInfo{ID: id, InstanceURL: fx.peerURL, Enabled: true, Connected: true}, nil
		},
		registry,
		logger,
	)
	if registry == nil {
		d.SetRegistry(inbox.BuildRegistry(d, logger))
	}
	return d
}

// unappliedCatalogueTypes is every §7 type, including the aa:RevokeShare
// alias, that this release does not apply for a peer with no negotiated
// profile. The share-lifecycle-1 types are among them (#1571): without
// the profile they are unsupported exactly like a type with no handler.
func unappliedCatalogueTypes() []federation.ActivityType {
	var out []federation.ActivityType
	for t := range federation.KnownActivityTypes {
		// aa:Provenance is profile-gated: without a negotiated
		// archival-1 it is profile_not_negotiated (§16.3, #1580),
		// refused before storage (members_test.go).
		if t == federation.ActivityAAProvenance {
			continue
		}
		if !inbox.Applies(t, nil) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// --- the applied set (no database) ------------------------------------

func TestAppliedSet_IsExactlyLikeCreateAndProfileGatedShares_AndRegistryMatches(t *testing.T) {
	want := []federation.ActivityType{federation.ActivityCreate, federation.ActivityLike,
		federation.ActivityAAShare, federation.ActivityAAUnshare}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if got := inbox.AppliedActivityTypes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("applied set: got %v want %v — update docs/spec/federation/v1.md D3 and §7 with any change", got, want)
	}
	d := inbox.NewDispatcher(inbox.DispatcherConfig{}, nil, nil, nil, nil)
	reg := inbox.BuildRegistry(d, nil)
	var keys []federation.ActivityType
	for k := range reg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("BuildRegistry keys %v != applied set %v (a stub or extra entry crept in)", keys, want)
	}
	// #1571: the share types are applied only under share-lifecycle-1.
	gated := inbox.ProfileGatedActivityTypes()
	if len(gated) != 2 || gated[federation.ActivityAAShare] != federation.ProfileShareLifecycle1 ||
		gated[federation.ActivityAAUnshare] != federation.ProfileShareLifecycle1 {
		t.Errorf("profile-gated set %v, want aa:Share and aa:Unshare under %s", gated, federation.ProfileShareLifecycle1)
	}
	lifecycle := []string{federation.ProfileShareLifecycle1}
	for _, v := range []federation.ActivityType{federation.ActivityAAShare, federation.ActivityAAUnshare, federation.ActivityAARevokeShare} {
		if inbox.Applies(v, nil) || inbox.Applies(v, []string{"archival-1", "nacl-box"}) {
			t.Errorf("%s applied without %s", v, federation.ProfileShareLifecycle1)
		}
		if !inbox.Applies(v, lifecycle) {
			t.Errorf("%s not applied with %s", v, federation.ProfileShareLifecycle1)
		}
	}
	for _, v := range []federation.ActivityType{
		federation.ActivityUndo, federation.ActivityDelete, federation.ActivityUpdate,
		federation.ActivityAAAssetVersion, federation.ActivityFollow, federation.ActivityAccept, federation.ActivityAdd,
	} {
		if inbox.Applies(v, lifecycle) {
			t.Errorf("%s reported as applied; no real handler exists", v)
		}
	}
	if inbox.Applies("aa:NotInTheCatalogue", lifecycle) || inbox.Applies("", lifecycle) {
		t.Error("an unknown or empty type must never be applied")
	}
}

// --- singleton endpoint -----------------------------------------------

func TestPostInbox_EveryUnappliedType_422_RecordedRejected_NeverProcessed(t *testing.T) {
	fx := newFixture(t)
	types := unappliedCatalogueTypes()
	if len(types) < 20 {
		t.Fatalf("expected the ~21 unapplied §7 types, got %d: %v", len(types), types)
	}
	for _, typ := range types {
		envelope := fx.envelopeOf(typ, "")
		id := activityIDOf(t, envelope)
		rr := fx.postSingle(envelope)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status got %d want 422; body=%s", typ, rr.Code, rr.Body.String())
			continue
		}
		assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnsupportedActivity)
		fx.assertStoredUnsupported(id, envelope)
		if e := fx.rejects.last(); e == nil || e.reason != federation.InboxStatusUnsupportedActivity || e.activityURI != id {
			t.Errorf("%s: audit hook got %+v want unsupported_activity for %s", typ, e, id)
		}
	}
}

func TestPostInbox_UnsupportedDuplicate_SameRejection_OneRow(t *testing.T) {
	fx := newFixture(t)
	envelope := fx.envelopeOf(federation.ActivityUndo, "")
	id := activityIDOf(t, envelope)
	for i := 0; i < 3; i++ { // first delivery + two retries
		rr := fx.postSingle(envelope)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("delivery %d: got %d want 422 (a retried unsupported activity must not get a 200 receipt); body=%s",
				i, rr.Code, rr.Body.String())
		}
		assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnsupportedActivity)
	}
	if n := fx.countRows(id); n != 1 {
		t.Errorf("rows for %s: got %d want 1", id, n)
	}
	fx.assertStoredUnsupported(id, envelope)
}

func TestPostInbox_SameIDAsUnsupported_NeverGetsSuccessReceipt(t *testing.T) {
	// A sender bug that reuses an unsupported activity's id for an
	// applied type is answered from the stored row, not with a 200.
	fx := newFixture(t)
	undo := fx.envelopeOf(federation.ActivityUndo, "")
	id := activityIDOf(t, undo)
	if rr := fx.postSingle(undo); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("undo: got %d want 422", rr.Code)
	}
	like := fx.envelopeOf(federation.ActivityLike, id)
	// Twice: both are answered from the stored row.
	for i := 0; i < 2; i++ {
		rr := fx.postSingle(like)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("like reusing an unsupported id, delivery %d: got %d want 422; body=%s", i, rr.Code, rr.Body.String())
		}
		assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnsupportedActivity)
		// The message names the stored activity's type, not this one's.
		var body struct{ Error string }
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		if !strings.Contains(body.Error, `"Undo"`) || strings.Contains(body.Error, `"Like"`) {
			t.Errorf("delivery %d message %q should name the stored Undo", i, body.Error)
		}
	}
	fx.assertStoredUnsupported(id, undo)
}

func TestPostInbox_NewlyIntroducedType_UnsupportedByDefault(t *testing.T) {
	// A type a later spec revision adds to the catalogue, with no
	// handler written for it, must be refused, not acknowledged.
	const future federation.ActivityType = "aa:FutureVerb"
	if _, exists := federation.KnownActivityTypes[future]; exists {
		t.Fatalf("%s already in the catalogue; pick another name", future)
	}
	federation.KnownActivityTypes[future] = struct{}{}
	t.Cleanup(func() { delete(federation.KnownActivityTypes, future) })

	fx := newFixture(t)
	envelope := fx.envelopeOf(future, "")
	id := activityIDOf(t, envelope)
	rr := fx.postSingle(envelope)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("newly introduced type: got %d want 422; body=%s", rr.Code, rr.Body.String())
	}
	assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnsupportedActivity)
	fx.assertStoredUnsupported(id, envelope)
}

func TestPostInbox_TypeOutsideCatalogue_StaysInvalidType_NoRow(t *testing.T) {
	// The compatibility rule's other half: a type outside §7 is a
	// parse failure (invalid_type), as before, and nothing is stored.
	fx := newFixture(t)
	envelope := fx.envelopeOf("aa:NotInTheCatalogue", "")
	id := activityIDOf(t, envelope)
	rr := fx.postSingle(envelope)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400; body=%s", rr.Code, rr.Body.String())
	}
	assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusInvalidType)
	if n := fx.countRows(id); n != 0 {
		t.Errorf("rows: got %d want 0", n)
	}
}

func TestPostInbox_UnsupportedType_AuthenticationGatesStillFirst(t *testing.T) {
	fx := newFixture(t)

	// Bad HTTP signature: tamper the body after signing.
	envelope := fx.envelopeOf(federation.ActivityAAShare, "")
	id := activityIDOf(t, envelope)
	req := fx.newSignedRequest(envelope)
	tampered := append([]byte(nil), envelope...)
	tampered[len(tampered)-2] = ' '
	req.Body = io.NopCloser(bytes.NewReader(tampered))
	rr := httptest.NewRecorder()
	fx.handler.PostInbox(rr, req)
	if rr.Code == http.StatusUnprocessableEntity || rr.Code < 400 {
		t.Fatalf("tampered unsupported envelope: got %d; the HTTP Signature gate must answer first", rr.Code)
	}
	if n := fx.countRows(id); n != 0 {
		t.Errorf("unauthenticated envelope stored: %d rows", n)
	}

	// Unknown peer keyId.
	envelope2 := fx.envelopeOf(federation.ActivityUndo, "")
	req2, _ := http.NewRequest(http.MethodPost, "https://local.example/federation/inbox", bytes.NewReader(envelope2))
	req2.Header.Set("Host", "local.example")
	if err := httpsig.SignAndAttach(req2, envelope2, "https://stranger.example/instance#main-key", fx.peerPriv); err != nil {
		t.Fatal(err)
	}
	rr2 := httptest.NewRecorder()
	fx.handler.PostInbox(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("unknown peer: got %d want 401", rr2.Code)
	}
	if n := fx.countRows(activityIDOf(t, envelope2)); n != 0 {
		t.Errorf("unknown-peer envelope stored: %d rows", n)
	}
}

func TestPostInbox_AppliedType_Unchanged_202ThenReplay200(t *testing.T) {
	fx := newFixture(t)
	envelope := fx.envelopeOf(federation.ActivityLike, "")
	if rr := fx.postSingle(envelope); rr.Code != http.StatusAccepted {
		t.Fatalf("like: got %d want 202", rr.Code)
	}
	if rr := fx.postSingle(envelope); rr.Code != http.StatusOK {
		t.Fatalf("like replay: got %d want 200", rr.Code)
	}
	row, ok := fx.storedRow(activityIDOf(t, envelope))
	if !ok || row.Status != "pending" {
		t.Fatalf("like row: ok=%v status=%q want pending", ok, row.Status)
	}
}

// --- batch endpoint ---------------------------------------------------

func TestPostInboxBatch_MatchesSingleton_IncludingDuplicates(t *testing.T) {
	const future federation.ActivityType = "aa:FutureVerb"
	federation.KnownActivityTypes[future] = struct{}{}
	t.Cleanup(func() { delete(federation.KnownActivityTypes, future) })

	fx := newFixture(t)
	like := fx.envelopeOf(federation.ActivityLike, "")
	undo := fx.envelopeOf(federation.ActivityUndo, "")
	share := fx.envelopeOf(federation.ActivityAAShare, "")
	newVerb := fx.envelopeOf(future, "")

	res := fx.postBatch(like, undo, share, newVerb)
	want := []batchResultT{
		{activityIDOf(t, like), "accepted", ""},
		{activityIDOf(t, undo), "rejected", string(federation.InboxStatusUnsupportedActivity)},
		{activityIDOf(t, share), "rejected", string(federation.InboxStatusUnsupportedActivity)},
		{activityIDOf(t, newVerb), "rejected", string(federation.InboxStatusUnsupportedActivity)},
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("first batch:\n got %+v\nwant %+v", res, want)
	}
	fx.assertStoredUnsupported(activityIDOf(t, undo), undo)
	fx.assertStoredUnsupported(activityIDOf(t, share), share)
	fx.assertStoredUnsupported(activityIDOf(t, newVerb), newVerb)

	// Duplicate delivery of the same batch: the applied one is
	// replayed, every unsupported one gets the same rejection.
	res2 := fx.postBatch(like, undo, share, newVerb)
	want[0].Status = "replayed"
	if !reflect.DeepEqual(res2, want) {
		t.Fatalf("duplicate batch:\n got %+v\nwant %+v", res2, want)
	}
	for _, e := range [][]byte{undo, share, newVerb} {
		if n := fx.countRows(activityIDOf(t, e)); n != 1 {
			t.Errorf("rows for %s: got %d want 1", activityIDOf(t, e), n)
		}
	}

	// Cross-endpoint duplicate: the singleton answers the batch's
	// stored row the same way.
	if rr := fx.postSingle(undo); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("singleton duplicate of a batch-recorded unsupported activity: got %d want 422", rr.Code)
	}
}

// --- dispatcher -------------------------------------------------------

func TestDispatcher_PendingUnsupportedRow_RejectedNotProcessed(t *testing.T) {
	// A row that was already pending when this release was installed
	// (or that an operator re-queued) has no handler: it is rejected,
	// terminal, not processed.
	fx := newFixture(t)
	for _, typ := range []federation.ActivityType{
		federation.ActivityUndo, federation.ActivityDelete, federation.ActivityAARevokeShare,
	} {
		envelope := fx.envelopeOf(typ, "")
		id := activityIDOf(t, envelope)
		row, err := fx.q.InsertInbox(context.Background(), inbox.InsertInboxParams{
			ActivityUri:  id,
			PeerID:       pgUUID(fx.peerID),
			ActorUri:     fx.peerURL + "/users/alice",
			ActivityType: string(typ),
			EnvelopeJson: envelope,
			HttpSigKey:   fx.peerKeyID,
		})
		if err != nil {
			t.Fatalf("seed pending %s: %v", typ, err)
		}
		d := fx.newDispatcher(nil)
		if got := d.DispatchRow(context.Background(), row); got != inbox.OutcomeRejected {
			t.Errorf("%s: outcome %v want OutcomeRejected", typ, got)
		}
		stored, _ := fx.storedRow(id)
		if stored.Status != "rejected" || stored.RejectReason == nil ||
			*stored.RejectReason != string(federation.InboxStatusUnsupportedActivity) {
			t.Errorf("%s: status=%q reason=%v want rejected/unsupported_activity", typ, stored.Status, stored.RejectReason)
		}
		if stored.ProcessedAt.Valid {
			t.Errorf("%s: processed_at set", typ)
		}
		if stored.LastError == "" {
			t.Errorf("%s: last_error empty", typ)
		}
		var got, want any
		_ = json.Unmarshal(stored.EnvelopeJson, &got)
		_ = json.Unmarshal(envelope, &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: envelope not preserved", typ)
		}
	}
}

func TestDispatcher_EmptyRegistry_RejectsEvenAppliedVerbs(t *testing.T) {
	// No "accept anything" default: a dispatcher wired without a
	// handler for a verb rejects it rather than recording success.
	fx := newFixture(t)
	envelope := fx.envelopeOf(federation.ActivityLike, "")
	id := activityIDOf(t, envelope)
	row, err := fx.q.InsertInbox(context.Background(), inbox.InsertInboxParams{
		ActivityUri: id, PeerID: pgUUID(fx.peerID), ActorUri: fx.peerURL + "/users/alice",
		ActivityType: string(federation.ActivityLike), EnvelopeJson: envelope, HttpSigKey: fx.peerKeyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := fx.newDispatcher(map[federation.ActivityType]inbox.HandlerFn{})
	if got := d.DispatchRow(context.Background(), row); got != inbox.OutcomeRejected {
		t.Fatalf("outcome %v want OutcomeRejected", got)
	}
	stored, _ := fx.storedRow(id)
	if stored.Status != "rejected" {
		t.Errorf("status %q want rejected", stored.Status)
	}
}

// --- a formerly unsupported type once its real handler lands ----------

func TestLandedHandler_FormerlyUnsupportedType_AcceptedAndDispatched(t *testing.T) {
	fx := newFixture(t)

	// Before: Undo is unsupported.
	before := fx.envelopeOf(federation.ActivityUndo, "")
	if rr := fx.postSingle(before); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("before landing: got %d want 422", rr.Code)
	}

	// Land a real Undo handler the only way production can: one
	// entry in the applied set.
	calls := 0
	inbox.RegisterAppliedForTest(t, federation.ActivityUndo, func(*inbox.Dispatcher) inbox.HandlerFn {
		return func(_ context.Context, env *federation.Envelope, _ uuid.UUID, _ string, _ inbox.FederationInbox) (inbox.DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
			calls++
			if env.Type != federation.ActivityUndo {
				t.Errorf("handler got %s", env.Type)
			}
			return inbox.OutcomeProcessed, "", uuid.UUID{}, nil
		}
	})

	after := fx.envelopeOf(federation.ActivityUndo, "")
	id := activityIDOf(t, after)
	if rr := fx.postSingle(after); rr.Code != http.StatusAccepted {
		t.Fatalf("after landing: got %d want 202; body=%s", rr.Code, rr.Body.String())
	}
	row, ok := fx.storedRow(id)
	if !ok || row.Status != "pending" {
		t.Fatalf("after landing: row ok=%v status=%q want pending", ok, row.Status)
	}
	d := fx.newDispatcher(nil) // production BuildRegistry, now including Undo
	if got := d.DispatchRow(context.Background(), row); got != inbox.OutcomeProcessed {
		t.Fatalf("dispatch: %v want OutcomeProcessed", got)
	}
	if calls != 1 {
		t.Errorf("real handler calls: got %d want 1", calls)
	}
	if stored, _ := fx.storedRow(id); stored.Status != "processed" {
		t.Errorf("status %q want processed", stored.Status)
	}
	// The earlier, pre-landing delivery stays as it was recorded.
	fx.assertStoredUnsupported(activityIDOf(t, before), before)
}

// --- no receipt without a stored row (#1569 correction) --------------

// An id first seen on an envelope that was refused before insert (or
// failed to store) has no stored row. A later delivery of that id must
// not get a receipt: with nothing stored it is a first delivery.

func TestPostInbox_SeenIDWithNoStoredRow_IsAFirstDelivery(t *testing.T) {
	fx := newFixture(t)

	// Id X is first seen on an envelope refused before insert
	// (invalid_type), then reused by a valid unsupported envelope.
	x := fx.peerURL + "/activities/" + uuid.NewString()
	if rr := fx.postSingle(fx.envelopeOf("aa:NotInTheCatalogue", x)); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid type: got %d want 400", rr.Code)
	}
	if n := fx.countRows(x); n != 0 {
		t.Fatalf("invalid-type envelope stored: %d rows", n)
	}
	undo := fx.envelopeOf(federation.ActivityUndo, x)
	rr := fx.postSingle(undo)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unsupported envelope on a seen id with no row: got %d want 422 (no receipt without a stored row); body=%s",
			rr.Code, rr.Body.String())
	}
	assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnsupportedActivity)
	fx.assertStoredUnsupported(x, undo)

	// Same for an applied type: id Y refused first, then a valid Like.
	y := fx.peerURL + "/activities/" + uuid.NewString()
	_ = fx.postSingle(fx.envelopeOf("aa:NotInTheCatalogue", y))
	if rr := fx.postSingle(fx.envelopeOf(federation.ActivityLike, y)); rr.Code != http.StatusAccepted {
		t.Fatalf("like on a seen id with no row: got %d want 202", rr.Code)
	}
	if row, ok := fx.storedRow(y); !ok || row.Status != "pending" {
		t.Fatalf("like row: ok=%v status=%q want pending", ok, row.Status)
	}
}

func TestPostInboxBatch_SeenIDWithNoStoredRow_IsAFirstDelivery(t *testing.T) {
	fx := newFixture(t)
	x := fx.peerURL + "/activities/" + uuid.NewString()
	y := fx.peerURL + "/activities/" + uuid.NewString()
	res := fx.postBatch(fx.envelopeOf("aa:NotInTheCatalogue", x), fx.envelopeOf("aa:NotInTheCatalogue", y))
	for i, r := range res {
		if r.Status != "rejected" || r.Reason != string(federation.InboxStatusInvalidType) {
			t.Fatalf("first batch result %d: %+v want rejected/invalid_type", i, r)
		}
	}
	undo := fx.envelopeOf(federation.ActivityUndo, x)
	res = fx.postBatch(undo, fx.envelopeOf(federation.ActivityLike, y))
	want := []batchResultT{
		{x, "rejected", string(federation.InboxStatusUnsupportedActivity)},
		{y, "accepted", ""},
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("second batch (seen ids, no rows):\n got %+v\nwant %+v", res, want)
	}
	fx.assertStoredUnsupported(x, undo)
	if row, ok := fx.storedRow(y); !ok || row.Status != "pending" {
		t.Fatalf("like row: ok=%v status=%q want pending", ok, row.Status)
	}
}

func TestInbox_CrossEndpoint_SeenIDWithNoStoredRow(t *testing.T) {
	fx := newFixture(t)

	// Single sees Z first (refused, nothing stored), batch reuses it.
	z := fx.peerURL + "/activities/" + uuid.NewString()
	_ = fx.postSingle(fx.envelopeOf("aa:NotInTheCatalogue", z))
	share := fx.envelopeOf(federation.ActivityAAShare, z)
	if res := fx.postBatch(share); res[0].Status != "rejected" || res[0].Reason != string(federation.InboxStatusUnsupportedActivity) {
		t.Fatalf("batch after single sighting: %+v want rejected/unsupported_activity", res[0])
	}
	fx.assertStoredUnsupported(z, share)

	// Batch sees W first (refused, nothing stored), single reuses it.
	w := fx.peerURL + "/activities/" + uuid.NewString()
	_ = fx.postBatch(fx.envelopeOf("aa:NotInTheCatalogue", w))
	if rr := fx.postSingle(fx.envelopeOf(federation.ActivityLike, w)); rr.Code != http.StatusAccepted {
		t.Fatalf("single after batch sighting: got %d want 202", rr.Code)
	}
	if n := fx.countRows(w); n != 1 {
		t.Errorf("rows for %s: got %d want 1", w, n)
	}
}

// --- storage failures are retryable, never final (#1569 correction) ---

// flakyDB wraps the real pool and fails selected inbox queries on
// demand, so the production handlers meet a storage failure exactly
// where a database outage would put it.
type flakyDB struct {
	inner              inbox.DBTX
	failRejectedInsert atomic.Bool
	failStoredLookup   atomic.Bool
}

var errInjected = errors.New("injected storage failure")

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func (f *flakyDB) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	return f.inner.Exec(ctx, sql, args...)
}

func (f *flakyDB) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	return f.inner.Query(ctx, sql, args...)
}

func (f *flakyDB) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	if f.failRejectedInsert.Load() && strings.Contains(sql, "name: InsertInboxRejected ") {
		return errRow{errInjected}
	}
	if f.failStoredLookup.Load() && strings.Contains(sql, "name: GetInboxByActivityURI ") {
		return errRow{errInjected}
	}
	return f.inner.QueryRow(ctx, sql, args...)
}

// flakyHandler is a production Handler for the fixture's peer whose
// queries go through a flakyDB. Its singleton and batch endpoints share
// it, like a real process.
func (fx *fixture) flakyHandler() (*inbox.Handler, *flakyDB) {
	db := &flakyDB{inner: fx.pool}
	h := inbox.NewHandler(inbox.HandlerDeps{
		Pool: inbox.New(db),
		Lookup: &staticPeerLookup{peerID: fx.peerID, peerURL: fx.peerURL, keyID: fx.peerKeyID,
			pubKey: fx.peerPub, enabled: true, connected: true},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		RejectAudit: fx.rejects.audit,
	})
	h.SetActorKeyResolver(fx.actorKeys)
	return h, db
}

func TestPostInbox_UnsupportedStoreFailure_5xx_ThenRetryRecorded(t *testing.T) {
	fx := newFixture(t)
	h, db := fx.flakyHandler()
	undo := fx.envelopeOf(federation.ActivityUndo, "")
	id := activityIDOf(t, undo)

	db.failRejectedInsert.Store(true)
	rr := httptest.NewRecorder()
	h.PostInbox(rr, fx.newSignedRequest(undo))
	if rr.Code < 500 {
		t.Fatalf("store failure: got %d want 5xx (retryable, neither a receipt nor a final rejection); body=%s", rr.Code, rr.Body.String())
	}
	if n := fx.countRows(id); n != 0 {
		t.Fatalf("rows after a failed store: %d want 0", n)
	}

	// The sender retries. The id has no stored row: it must be
	// processed, not acknowledged.
	db.failRejectedInsert.Store(false)
	rr = httptest.NewRecorder()
	h.PostInbox(rr, fx.newSignedRequest(undo))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("retry: got %d want 422; body=%s", rr.Code, rr.Body.String())
	}
	fx.assertStoredUnsupported(id, undo)
}

func (fx *fixture) postBatchTo(h *inbox.Handler, envelopes ...[]byte) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.PostInboxBatch(rr, fx.newSignedBatchRequest(envelopes...))
	return rr
}

func TestPostInboxBatch_StoreFailure_503_SiblingsReplayedOnRetry(t *testing.T) {
	fx := newFixture(t)
	h, db := fx.flakyHandler()
	likeA := fx.envelopeOf(federation.ActivityLike, "")
	undoB := fx.envelopeOf(federation.ActivityUndo, "")
	likeC := fx.envelopeOf(federation.ActivityLike, "")
	a, b, c := activityIDOf(t, likeA), activityIDOf(t, undoB), activityIDOf(t, likeC)

	db.failRejectedInsert.Store(true)
	rr := fx.postBatchTo(h, likeA, undoB, likeC)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("batch with a store failure: got %d want 503; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"results"`) {
		t.Errorf("503 body carries per-envelope results; the sender would treat them as final: %s", rr.Body.String())
	}
	if row, ok := fx.storedRow(a); !ok || row.Status != "pending" {
		t.Fatalf("sibling A before the failure: ok=%v status=%q want stored pending", ok, row.Status)
	}
	if n := fx.countRows(b); n != 0 {
		t.Errorf("B (failed store): %d rows want 0", n)
	}
	if n := fx.countRows(c); n != 0 {
		t.Errorf("C (after the failure, not processed): %d rows want 0", n)
	}

	// The sender retries the whole batch.
	db.failRejectedInsert.Store(false)
	rr = fx.postBatchTo(h, likeA, undoB, likeC)
	if rr.Code != http.StatusOK {
		t.Fatalf("retry: got %d want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Results []batchResultT `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := []batchResultT{
		{a, "replayed", ""},
		{b, "rejected", string(federation.InboxStatusUnsupportedActivity)},
		{c, "accepted", ""},
	}
	if !reflect.DeepEqual(resp.Results, want) {
		t.Fatalf("retry results:\n got %+v\nwant %+v", resp.Results, want)
	}
	fx.assertStoredUnsupported(b, undoB)
	for _, id := range []string{a, b, c} {
		if n := fx.countRows(id); n != 1 {
			t.Errorf("rows for %s: got %d want 1", id, n)
		}
	}

	// A duplicate whose stored row cannot be read is also a 503, not
	// a guess.
	db.failStoredLookup.Store(true)
	if rr := fx.postBatchTo(h, undoB); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("stored-row lookup failure: got %d want 503; body=%s", rr.Code, rr.Body.String())
	}
	db.failStoredLookup.Store(false)
}

// --- the stored refusal outlives restarts and new handlers (#1569 c2) ---

// receiver is a fresh production Handler for the fixture's peer: no
// in-process state carried over, as after a restart. localBase wires
// the object-ref host check, so an object on another host is
// unknown_object for an applied type.
func (fx *fixture) receiver(localBase string) *inbox.Handler {
	h := inbox.NewHandler(inbox.HandlerDeps{
		Pool: fx.q,
		Lookup: &staticPeerLookup{peerID: fx.peerID, peerURL: fx.peerURL, keyID: fx.peerKeyID,
			pubKey: fx.peerPub, enabled: true, connected: true},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		LocalBaseURL: func(context.Context) string { return localBase },
		RejectAudit:  fx.rejects.audit,
	})
	h.SetActorKeyResolver(fx.actorKeys)
	return h
}

func (fx *fixture) postTo(h *inbox.Handler, envelope []byte) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.PostInbox(rr, fx.newSignedRequest(envelope))
	return rr
}

// landCountingUndo lands a real handler for Undo for the rest of t and
// returns its call counter.
func landCountingUndo(t *testing.T) *atomic.Int32 {
	var calls atomic.Int32
	inbox.RegisterAppliedForTest(t, federation.ActivityUndo, func(*inbox.Dispatcher) inbox.HandlerFn {
		return func(context.Context, *federation.Envelope, uuid.UUID, string, inbox.FederationInbox) (inbox.DispatchOutcome, federation.InboxStatus, uuid.UUID, error) {
			calls.Add(1)
			return inbox.OutcomeProcessed, "", uuid.UUID{}, nil
		}
	})
	return &calls
}

// assertRowUnchanged checks the stored refusal was not touched: same
// status, reason, message, attempts and updated_at, and not pending.
func (fx *fixture) assertRowUnchanged(id string, before inbox.FederationInbox) {
	fx.t.Helper()
	after, ok := fx.storedRow(id)
	if !ok {
		fx.t.Fatalf("row for %s vanished", id)
	}
	if after.Status != before.Status || *after.RejectReason != *before.RejectReason ||
		after.LastError != before.LastError || after.DispatchAttempts != before.DispatchAttempts ||
		!after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) || after.ProcessedAt.Valid {
		fx.t.Errorf("stored refusal changed:\n before %+v\n  after %+v", before, after)
	}
	if n := fx.countRows(id); n != 1 {
		fx.t.Errorf("rows for %s: got %d want 1", id, n)
	}
}

func TestPostInbox_StoredUnsupported_FreshReceiverAfterHandlerLands_SameAnswer(t *testing.T) {
	fx := newFixture(t)
	first := fx.receiver("https://local.example")

	// Recorded as unsupported. Its object is an activity on the peer,
	// which the host check would call unknown_object for an applied type.
	undo := fx.envelopeOf(federation.ActivityUndo, "")
	id := activityIDOf(t, undo)
	if rr := fx.postTo(first, undo); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("first delivery: got %d want 422; body=%s", rr.Code, rr.Body.String())
	}
	before, _ := fx.storedRow(id)

	// Undo's handler lands; the receiver restarts (fresh handler).
	calls := landCountingUndo(t)
	fresh := fx.receiver("https://local.example")

	// Control: a NEW Undo of the same shape now takes the applied
	// path and is refused by the object-ref check. This is the answer
	// the old id would get if the current handler set decided it.
	if rr := fx.postTo(fresh, fx.envelopeOf(federation.ActivityUndo, "")); rr.Code != http.StatusNotFound {
		t.Fatalf("control: new Undo after landing got %d want 404 unknown_object; body=%s", rr.Code, rr.Body.String())
	}

	// The SAME id: the stored refusal decides.
	for i := 0; i < 2; i++ {
		rr := fx.postTo(fresh, undo)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("redelivery %d on a fresh receiver: got %d want the original 422; body=%s", i, rr.Code, rr.Body.String())
		}
		assertReasonInBody(t, rr.Body.Bytes(), federation.InboxStatusUnsupportedActivity)
	}
	fx.assertRowUnchanged(id, before)
	fx.assertStoredUnsupported(id, undo)
	if n := calls.Load(); n != 0 {
		t.Errorf("landed Undo handler calls: got %d want 0", n)
	}

	// A stored ACCEPTED id keeps its idempotent receipt on a fresh
	// receiver too.
	like := fx.envelopeOf(federation.ActivityLike, "")
	_ = fx.postSingle(like) // stored pending via the fixture's handler
	if rr := fx.postTo(fx.receiver("https://local.example"), like); rr.Code != http.StatusOK {
		t.Errorf("stored accepted id on a fresh receiver: got %d want 200", rr.Code)
	}

	// A stored-row read failure is retryable, never success.
	h, db := fx.flakyHandler()
	db.failStoredLookup.Store(true)
	rr := httptest.NewRecorder()
	h.PostInbox(rr, fx.newSignedRequest(undo))
	if rr.Code < 500 {
		t.Errorf("stored-row read failure: got %d want 5xx", rr.Code)
	}
	fx.assertRowUnchanged(id, before)
}

func TestPostInboxBatch_StoredUnsupported_FreshReceiverAfterHandlerLands_SameAnswer(t *testing.T) {
	fx := newFixture(t)
	first := fx.receiver("https://local.example")

	undo := fx.envelopeOf(federation.ActivityUndo, "")
	// The Like targets a local post, so the host check accepts it and
	// it is stored.
	var likeMap map[string]any
	_ = json.Unmarshal(fx.envelopeOf(federation.ActivityLike, ""), &likeMap)
	likeMap["object"] = "https://local.example/posts/" + uuid.NewString()
	like, _ := json.Marshal(likeMap)
	like = fx.signEnvelope(like) // re-sign after the edit (#1526)
	id, likeID := activityIDOf(t, undo), activityIDOf(t, like)
	rr := fx.postBatchTo(first, undo, like)
	var firstResp struct {
		Results []batchResultT `json:"results"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &firstResp)
	if rr.Code != http.StatusOK || !reflect.DeepEqual(firstResp.Results, []batchResultT{
		{id, "rejected", string(federation.InboxStatusUnsupportedActivity)},
		{likeID, "accepted", ""},
	}) {
		t.Fatalf("first batch: %d %s", rr.Code, rr.Body.String())
	}
	before, _ := fx.storedRow(id)

	calls := landCountingUndo(t)
	fresh := fx.receiver("https://local.example")

	// Control, as in the singleton test.
	ctl := fx.envelopeOf(federation.ActivityUndo, "")
	rr = fx.postBatchTo(fresh, ctl)
	var ctlResp struct {
		Results []batchResultT `json:"results"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &ctlResp)
	if len(ctlResp.Results) != 1 || ctlResp.Results[0].Reason != string(federation.InboxStatusUnknownObject) {
		t.Fatalf("control: new Undo after landing got %s want rejected/unknown_object", rr.Body.String())
	}

	rr = fx.postBatchTo(fresh, undo, like)
	if rr.Code != http.StatusOK {
		t.Fatalf("redelivery batch: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Results []batchResultT `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := []batchResultT{
		{id, "rejected", string(federation.InboxStatusUnsupportedActivity)},
		{likeID, "replayed", ""},
	}
	if !reflect.DeepEqual(resp.Results, want) {
		t.Fatalf("redelivery on a fresh receiver:\n got %+v\nwant %+v", resp.Results, want)
	}
	fx.assertRowUnchanged(id, before)
	if n := calls.Load(); n != 0 {
		t.Errorf("landed Undo handler calls: got %d want 0", n)
	}

	// A stored-row read failure is a whole-batch 503.
	h, db := fx.flakyHandler()
	db.failStoredLookup.Store(true)
	if rr := fx.postBatchTo(h, undo); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("stored-row read failure: got %d want 503", rr.Code)
	}
	fx.assertRowUnchanged(id, before)
}
