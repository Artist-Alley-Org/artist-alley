// Batched inbox endpoint per spec §10.4 + the 1.22.D design
// proposal §3.10. Phase 1.22.D-b-5.
//
// Amortises HTTP-Sig + TLS overhead across multiple envelopes
// in one request. HTTP-Sig signs the WHOLE batched body; per-
// envelope status reporting in the response so the sender can
// retry individual failures.

package inbox

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
)

// MaxBatchSize is the cap per spec §10.4.
const MaxBatchSize = 50

// MaxBatchBodyBytes is the cap per spec §10.4 (1MB total).
const MaxBatchBodyBytes = 1 << 20

// batchRequest is the on-wire shape for POST /federation/inbox/batch.
type batchRequest struct {
	Envelopes []json.RawMessage `json:"envelopes"`
}

// batchResult is one entry in the per-envelope response array.
type batchResult struct {
	ActivityURI string `json:"activity_uri"`
	Status      string `json:"status"` // accepted | replayed | rejected
	Reason      string `json:"reason"` // §12.1 reject reason; empty for accepted/replayed
}

// batchResponse is the wire shape per spec §10.4.
type batchResponse struct {
	Results []batchResult `json:"results"`
}

// PostInboxBatch is the chi handler for POST /federation/inbox/batch.
// Mounted alongside PostInbox in server.go.
func (h *Handler) PostInboxBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1. Drain + cap body.
	if r.Body == nil {
		http.Error(w, "missing body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	limited := io.LimitReader(r.Body, MaxBatchBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > MaxBatchBodyBytes {
		http.Error(w, "batch body exceeds 1MB", http.StatusRequestEntityTooLarge)
		return
	}

	// 2. Parse Signature header.
	sigParams, err := httpsig.ParseSignatureHeader(r.Header.Get("Signature"))
	if err != nil {
		writeBatchError(w, mapHTTPSigErrCode(err), err.Error())
		return
	}

	// 3. Resolve peer.
	peer, err := h.lookup.ByKeyID(ctx, sigParams.KeyID)
	if err != nil {
		if errors.Is(err, ErrPeerNotFound) {
			writeBatchError(w, http.StatusUnauthorized, "keyId does not resolve to any known peer")
			return
		}
		h.logErr(ctx, "inbox_batch.peer.lookup.error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !peer.Enabled || !peer.Connected {
		writeBatchError(w, http.StatusForbidden, "peer disabled or not connected")
		return
	}

	// 4. Verify HTTP-Sig covers the WHOLE batched body.
	resolver := func(keyID string) (ed25519.PublicKey, error) {
		if keyID != sigParams.KeyID || len(peer.InstancePublicKey) == 0 {
			return nil, httpsig.ErrUnknownKey
		}
		return peer.InstancePublicKey, nil
	}
	if _, err := httpsig.Verify(r, body, resolver, h.clock()); err != nil {
		writeBatchError(w, mapHTTPSigErrCode(err), err.Error())
		return
	}

	// 5. Per-peer rate limit. ONE token per batch (not per
	// envelope) — the §5.5 addition A budget is intentionally
	// expressed in REQUESTS not envelopes; batching is the
	// efficiency win.
	if ok, wait := h.limiter.Allow(peer.ID); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())))
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// 6. Parse the batch envelope.
	var batch batchRequest
	if err := json.Unmarshal(body, &batch); err != nil {
		http.Error(w, "malformed batch body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(batch.Envelopes) == 0 {
		writeBatchOK(w, batchResponse{Results: []batchResult{}})
		return
	}
	if len(batch.Envelopes) > MaxBatchSize {
		http.Error(w, fmt.Sprintf("batch size %d exceeds cap %d", len(batch.Envelopes), MaxBatchSize), http.StatusBadRequest)
		return
	}

	// 7. Per-envelope processing. Failures DON'T abort siblings.
	results := make([]batchResult, 0, len(batch.Envelopes))
	for _, raw := range batch.Envelopes {
		results = append(results, h.processBatchEnvelope(ctx, peer, sigParams.KeyID, raw))
	}

	writeBatchOK(w, batchResponse{Results: results})
}

// processBatchEnvelope runs the per-envelope subset of the
// PostInbox pipeline (stages 7-12 — the host check, encryption
// gate, structural-sig check, INSERT, replay cache).
//
// Stages 1-6 (HTTP-level: rate limit, sig verify, body parse)
// already ran ONCE for the whole batch in PostInboxBatch above.
//
// Returns a typed batchResult — never errors out; the outer
// loop continues on any per-envelope failure per spec §10.4.
func (h *Handler) processBatchEnvelope(ctx context.Context, peer PeerInfo, keyID string, raw json.RawMessage) batchResult {
	// Peek activity_uri for the result envelope (even on
	// rejection we want to surface the id so the sender can
	// pair the result with its source).
	activityURI, err := peekActivityID([]byte(raw))
	if err != nil {
		return batchResult{
			ActivityURI: "",
			Status:      "rejected",
			Reason:      string(federation.InboxStatusInvalidContext),
		}
	}

	// Replay cache short-circuit.
	if h.replayCache.Seen(activityURI) {
		return batchResult{ActivityURI: activityURI, Status: "replayed"}
	}

	// Strict-parse envelope.
	env, err := federation.Unmarshal([]byte(raw))
	if err != nil {
		rej := mapEnvelopeErr(err).(rejection)
		h.auditReject(ctx, peer.ID, rej.status, activityURI, err.Error())
		return batchResult{ActivityURI: activityURI, Status: "rejected", Reason: string(rej.status)}
	}
	if env.Context != federation.ContextV1 {
		h.auditReject(ctx, peer.ID, federation.InboxStatusInvalidContext, activityURI, env.Context)
		return batchResult{ActivityURI: activityURI, Status: "rejected", Reason: string(federation.InboxStatusInvalidContext)}
	}
	if env.Encrypted != nil {
		h.auditReject(ctx, peer.ID, federation.InboxStatusEncryptionNotSupported, activityURI,
			"encrypted envelope received; X25519 decryption ships in 1.22.I")
		return batchResult{ActivityURI: activityURI, Status: "rejected", Reason: string(federation.InboxStatusEncryptionNotSupported)}
	}
	// Structural envelope-sig check (spec §5.6).
	if env.Signature == nil ||
		env.Signature.Type == "" ||
		env.Signature.PublicKey == "" ||
		env.Signature.Value == "" {
		h.auditReject(ctx, peer.ID, federation.InboxStatusEnvelopeSigMissing, activityURI,
			"envelope signature block absent or malformed")
		return batchResult{ActivityURI: activityURI, Status: "rejected", Reason: string(federation.InboxStatusEnvelopeSigMissing)}
	}
	if env.Signature.Type != federation.SignatureAlgEd25519 {
		h.auditReject(ctx, peer.ID, federation.InboxStatusUnsupportedAlgorithm, activityURI,
			"signature.type="+string(env.Signature.Type))
		return batchResult{ActivityURI: activityURI, Status: "rejected", Reason: string(federation.InboxStatusUnsupportedAlgorithm)}
	}

	// Object-ref extraction (host check via the singleton
	// PostInbox path; surfaces unknown_object for foreign-host
	// or bad-shape URLs).
	objectKindPtr, objectIDPtr, err := h.extractObjectRef(ctx, env)
	if err != nil {
		if errors.Is(err, ErrUnknownObject) {
			h.auditReject(ctx, peer.ID, federation.InboxStatusUnknownObject, activityURI,
				"object URL does not resolve to a local row")
			return batchResult{ActivityURI: activityURI, Status: "rejected", Reason: string(federation.InboxStatusUnknownObject)}
		}
		// Other extraction errors — surface as error reason but
		// don't fail the whole batch.
		return batchResult{ActivityURI: activityURI, Status: "rejected", Reason: string(federation.InboxStatusError)}
	}

	// INSERT (idempotent via UNIQUE on activity_uri).
	_, err = h.pool.InsertInbox(ctx, InsertInboxParams{
		ActivityUri:  activityURI,
		PeerID:       pgtype.UUID{Bytes: peer.ID, Valid: true},
		ActorUri:     env.Actor,
		ActivityType: string(env.Type),
		ObjectKind:   objectKindPtr,
		ObjectID:     objectIDPtr,
		EnvelopeJson: []byte(raw),
		HttpSigKey:   keyID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return batchResult{ActivityURI: activityURI, Status: "replayed"}
		}
		h.logErr(ctx, "inbox_batch.insert.error", err)
		return batchResult{ActivityURI: activityURI, Status: "rejected", Reason: string(federation.InboxStatusError)}
	}
	return batchResult{ActivityURI: activityURI, Status: "accepted"}
}

// mapHTTPSigErrCode returns the HTTP status code for a httpsig
// error — used by the batch handler's top-of-stack failures
// (the per-envelope path doesn't call this since envelope-
// level failures don't translate to HTTP codes).
func mapHTTPSigErrCode(err error) int {
	switch {
	case errors.Is(err, httpsig.ErrUnsignedRequest),
		errors.Is(err, httpsig.ErrUnknownKey),
		errors.Is(err, httpsig.ErrSigInvalid):
		return http.StatusUnauthorized
	case errors.Is(err, httpsig.ErrStaleRequest),
		errors.Is(err, httpsig.ErrUnsupportedAlgorithm),
		errors.Is(err, httpsig.ErrSigMalformed),
		errors.Is(err, httpsig.ErrMissingHeader),
		errors.Is(err, httpsig.ErrDigestMismatch):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func writeBatchOK(w http.ResponseWriter, resp batchResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func writeBatchError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Local helpers re-exported from the singleton path so the
// batch handler doesn't depend on import order.

// Ensure imports stay live in case the per-envelope path's
// rejected-status surface evolves.
var (
	_ = uuid.UUID{}
	_ = (*slog.Logger)(nil)
	_ = url.Parse
	_ = strings.TrimRight
)
