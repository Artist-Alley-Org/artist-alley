// HTTP inbox endpoint per the 1.22.D design proposal §2.
// Phase 1.22.D-a-3.
//
// This file implements the synchronous portion of the inbox
// pipeline (stages 1-11). Stages 12-13 (dispatch + handler
// invocation) run in a background goroutine — that ships in
// 1.22.D-a-4.
//
// # Pipeline summary
//
// Each stage either short-circuits the response with a typed
// rejection OR advances to the next. Rejections AFTER stage 4
// (peer resolved) record an `federation_inbox` row with
// status='rejected' so admins can see what bounced + why.
// Rejections BEFORE stage 4 return without a DB write — we
// can't attribute them to a peer.
//
//   1. Body drain + size cap (§5.5 Q6 — 2MB, 413 on overflow)
//   2. Parse Signature header → keyId
//   3. Resolve peer by keyId URL
//   4. Confirm peer enabled + connected
//   5. Verify HTTP-Signature (httpsig.Verify) — checks Date,
//      Digest, allowlist algo, signed-header set
//   6. Per-peer rate-limit token consume (§5.5 addition A)
//   7. Cheap replay-cache check (short-circuits dups before
//      envelope parse)
//   8. Parse envelope (federation.Unmarshal) — strict-parse,
//      catches unknown fields + invalid types
//   9. Reject encrypted envelopes (§5.5 addition 1 — 1.22.I
//      will flip this to decrypt-and-continue)
//  10. Structural-validate envelope `signature` field
//      (§5.5 addition C — full crypto verify lands in 1.22.I)
//  11. INSERT federation_inbox row (UNIQUE on activity_uri →
//      authoritative dedup if cache missed)
//  12. Return 202 Accepted

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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
)

// MaxBodyBytes is the inbox payload cap per the 1.22.D design
// proposal §5.5 Q6. Envelopes carry metadata + signatures + at
// most ~100KB of inline content; asset bytes go through CAS.
// 2MB is generous headroom.
const MaxBodyBytes = 2 << 20

// PeerLookup is the contract the handler needs to resolve a
// peer from a httpsig keyId URL. Boot wires this to
// peer.Registry's ByInstanceURL lookup.
type PeerLookup interface {
	// ByKeyID returns the peer that owns the given httpsig
	// keyId URL. The lookup is typically by URL-prefix match
	// against federation_peers.instance_url.
	ByKeyID(ctx context.Context, keyID string) (PeerInfo, error)
}

// PeerInfo is the subset of peer.Peer the inbox handler needs.
// Mirrors shares.PeerInfo to keep cross-package coupling local.
type PeerInfo struct {
	ID                uuid.UUID
	InstanceURL       string
	InstancePublicKey ed25519.PublicKey
	Enabled           bool
	Connected         bool
}

// ErrPeerNotFound is what PeerLookup.ByKeyID returns when no
// peer matches the supplied keyId URL.
var ErrPeerNotFound = errors.New("inbox: peer not found for keyId")

// Handler is the chi-mountable handler for POST /federation/inbox.
// All dependencies are injected so tests can wire stubs.
type Handler struct {
	pool         *Queries // sqlc binding — see queries.sql.go
	lookup       PeerLookup
	limiter      *PeerRateLimiter
	replayCache  *ReplayCache
	logger       *slog.Logger
	clock        func() time.Time
	rejectAudit  func(ctx context.Context, peerID uuid.UUID, reason federation.InboxStatus, activityURI, msg string)
}

// HandlerDeps bundles the constructor inputs. Required for the
// non-trivial set + lets tests skip the rejectAudit hook cleanly.
type HandlerDeps struct {
	Pool         *Queries
	Lookup       PeerLookup
	Limiter      *PeerRateLimiter
	ReplayCache  *ReplayCache
	Logger       *slog.Logger
	// Clock override for tests. Defaults to time.Now if nil.
	Clock        func() time.Time
	// Audit hook. Called whenever the pipeline rejects an
	// envelope post-peer-resolution. Production wires
	// audit.Recorder.ActivityRejected. nil-safe (skipped).
	RejectAudit  func(ctx context.Context, peerID uuid.UUID, reason federation.InboxStatus, activityURI, msg string)
}

// NewHandler wires the handler.
func NewHandler(deps HandlerDeps) *Handler {
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	limiter := deps.Limiter
	if limiter == nil {
		limiter = NewPeerRateLimiter(100, 100)
	}
	cache := deps.ReplayCache
	if cache == nil {
		cache = NewReplayCache(16_384, 30*time.Second)
	}
	return &Handler{
		pool:        deps.Pool,
		lookup:      deps.Lookup,
		limiter:     limiter,
		replayCache: cache,
		logger:      deps.Logger,
		clock:       clock,
		rejectAudit: deps.RejectAudit,
	}
}

// rejection is the typed result of a pipeline stage that failed.
// status is the §12.1 reject reason; message is the operator-
// facing explanation.
type rejection struct {
	status     federation.InboxStatus
	httpStatus int
	message    string
}

func (r rejection) Error() string { return r.message }

// reject builds a typed pipeline rejection.
func reject(s federation.InboxStatus, http int, msg string) error {
	return rejection{status: s, httpStatus: http, message: msg}
}

// PostInbox is the HTTP entry point.
//
// `responseSink` lets tests inspect the response.WriteHeader call
// without spinning a full httptest server. Production callers pass
// the chi handler's ResponseWriter directly.
func (h *Handler) PostInbox(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Stage 1: body drain with size cap.
	body, err := h.drainCappedBody(r)
	if err != nil {
		writeRejection(w, r, h.logger, reject(federation.InboxStatusInvalidContext, http.StatusRequestEntityTooLarge,
			"envelope larger than 2MB; use CAS URIs for content"))
		return
	}

	// Stage 2: parse Signature header for the keyId.
	sigParams, err := httpsig.ParseSignatureHeader(r.Header.Get("Signature"))
	if err != nil {
		writeRejection(w, r, h.logger, mapHTTPSigErr(err))
		return
	}

	// Stage 3: resolve peer.
	peer, err := h.lookup.ByKeyID(ctx, sigParams.KeyID)
	if err != nil {
		if errors.Is(err, ErrPeerNotFound) {
			writeRejection(w, r, h.logger, reject(federation.InboxStatusUnknownPeer, http.StatusUnauthorized,
				"keyId does not resolve to any known peer"))
			return
		}
		h.logErr(ctx, "inbox.peer.lookup.error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Stage 4: peer enabled + connected.
	if !peer.Enabled {
		h.auditReject(ctx, peer.ID, federation.InboxStatusPeerDisabled, "", "peer disabled")
		writeRejection(w, r, h.logger, reject(federation.InboxStatusPeerDisabled, http.StatusForbidden,
			"peer disabled"))
		return
	}
	if !peer.Connected {
		h.auditReject(ctx, peer.ID, federation.InboxStatusPeerDisabled, "", "peer not connected (status != 'connected')")
		writeRejection(w, r, h.logger, reject(federation.InboxStatusPeerDisabled, http.StatusForbidden,
			"peer not in 'connected' status"))
		return
	}

	// Stage 5: HTTP-Signature verify.
	resolver := func(keyID string) (ed25519.PublicKey, error) {
		if keyID != sigParams.KeyID {
			return nil, httpsig.ErrUnknownKey
		}
		if len(peer.InstancePublicKey) == 0 {
			return nil, httpsig.ErrUnknownKey
		}
		return peer.InstancePublicKey, nil
	}
	if _, err := httpsig.Verify(r, body, resolver, h.clock()); err != nil {
		rej := mapHTTPSigErr(err)
		h.auditReject(ctx, peer.ID, rej.(rejection).status, "", err.Error())
		writeRejection(w, r, h.logger, rej)
		return
	}

	// Stage 6: per-peer rate limit.
	if ok, wait := h.limiter.Allow(peer.ID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
		writeRejection(w, r, h.logger, reject(federation.InboxStatusError, http.StatusTooManyRequests,
			fmt.Sprintf("rate limit exceeded; retry after %v", wait)))
		return
	}

	// Stage 7: cheap activity-URI peek for replay-cache hit.
	// We extract just the `id` field with a tiny parse pass —
	// full envelope unmarshal happens at stage 8.
	activityURI, err := peekActivityID(body)
	if err != nil {
		writeRejection(w, r, h.logger, reject(federation.InboxStatusInvalidContext, http.StatusBadRequest,
			"envelope JSON missing or invalid 'id'"))
		return
	}
	if h.replayCache.Seen(activityURI) {
		// Same envelope already in flight or recently seen.
		// Return 200 OK no-op per the design (idempotent
		// receipt) — sender's retry behaves correctly.
		w.WriteHeader(http.StatusOK)
		return
	}

	// Stage 8: parse envelope (strict).
	env, err := federation.Unmarshal(body)
	if err != nil {
		rej := mapEnvelopeErr(err)
		h.auditReject(ctx, peer.ID, rej.(rejection).status, activityURI, err.Error())
		writeRejection(w, r, h.logger, rej)
		return
	}
	// Spec §3.1: @context must equal the protocol-v1 string.
	if env.Context != federation.ContextV1 {
		h.auditReject(ctx, peer.ID, federation.InboxStatusInvalidContext, activityURI, env.Context)
		writeRejection(w, r, h.logger, reject(federation.InboxStatusInvalidContext, http.StatusBadRequest,
			"unexpected @context"))
		return
	}

	// Stage 9: encrypted envelopes not supported until 1.22.I
	// per §5.5 addition 1. Distinct from encryption_required
	// (that one fires when the SENDER violated a MUST-encrypt
	// rule; this one fires when the RECEIVER can't decrypt).
	//
	// TODO(1.22.I): replace this rejection with a real NaCl-box
	// decrypt path using the per-actor X25519 private key. The
	// receiver's response shape switches from 422 to 202 once
	// decryption lands.
	if env.Encrypted != nil {
		h.auditReject(ctx, peer.ID, federation.InboxStatusEncryptionNotSupported, activityURI,
			"encrypted envelope received; X25519 decryption ships in 1.22.I")
		writeRejection(w, r, h.logger, reject(federation.InboxStatusEncryptionNotSupported, http.StatusUnprocessableEntity,
			"encrypted federation not supported until Phase 1.22.I"))
		return
	}

	// Stage 10: structural-validate envelope signature per spec
	// §5.6 + §5.5 addition C. We don't crypto-verify it yet
	// (per-actor pubkey distribution lands in 1.22.I); the
	// structural check keeps the wire format honest so 1.22.I
	// can flip on real verify without rejecting traffic that
	// was previously accepted.
	//
	// envelope_sig_missing is the spec §12.1 reason for any
	// structural failure (absent OR malformed). sig_invalid is
	// reserved for the future crypto-verify failure path —
	// keeping them distinct so operators can tell "spec-
	// noncompliant peer" from "key compromised / drifted."
	//
	// TODO(1.22.I): add real ed25519.Verify against the per-
	// actor pubkey resolved from env.Signature.PublicKey. On
	// failure, return reject(federation.InboxStatusSigInvalid,
	// http.StatusUnauthorized, "envelope signature failed crypto verify").
	if env.Signature == nil ||
		env.Signature.Type == "" ||
		env.Signature.PublicKey == "" ||
		env.Signature.Value == "" {
		h.auditReject(ctx, peer.ID, federation.InboxStatusEnvelopeSigMissing, activityURI,
			"envelope signature block absent or malformed")
		writeRejection(w, r, h.logger, reject(federation.InboxStatusEnvelopeSigMissing, http.StatusUnauthorized,
			"envelope signature block absent or malformed"))
		return
	}
	if env.Signature.Type != federation.SignatureAlgEd25519 {
		h.auditReject(ctx, peer.ID, federation.InboxStatusUnsupportedAlgorithm, activityURI,
			"signature.type="+string(env.Signature.Type))
		writeRejection(w, r, h.logger, reject(federation.InboxStatusUnsupportedAlgorithm, http.StatusBadRequest,
			"signature algorithm not in allowlist"))
		return
	}

	// Stage 11: INSERT. UNIQUE constraint on activity_uri is
	// the authoritative dedup; cache miss + duplicate envelope
	// surfaces here as a constraint violation → return 200 OK
	// no-op per the idempotent-receipt invariant.
	objectKindPtr, objectIDPtr := extractObjectRef(env)
	row, err := h.pool.InsertInbox(ctx, InsertInboxParams{
		ActivityUri:   activityURI,
		PeerID:        pgtype.UUID{Bytes: peer.ID, Valid: true},
		ActorUri:      env.Actor,
		ActivityType:  string(env.Type),
		ObjectKind:    objectKindPtr,
		ObjectID:      objectIDPtr,
		EnvelopeJson:  body,
		HttpSigKey:    sigParams.KeyID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Replay caught by the DB after cache miss. Idempotent
			// receipt → 200 OK no-op.
			w.WriteHeader(http.StatusOK)
			return
		}
		h.logErr(ctx, "inbox.insert.error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.logger != nil {
		h.logger.LogAttrs(ctx, slog.LevelInfo, "inbox.received",
			slog.String("activity_uri", activityURI),
			slog.String("activity_type", string(env.Type)),
			slog.String("peer_id", peer.ID.String()),
			slog.String("inbox_id", row.ID.String()),
		)
	}

	// Stage 12: 202 Accepted. Dispatch happens async in the
	// 1.22.D-a-4 worker.
	w.WriteHeader(http.StatusAccepted)
}

// --- helpers ----------------------------------------------------------

// drainCappedBody reads up to MaxBodyBytes + 1 from r.Body. If
// the body is larger, returns an error so the caller can reply
// 413. The +1 is the standard trick for "anything-larger-than-N
// is too big" without consuming the entire stream.
func (h *Handler) drainCappedBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	limited := io.LimitReader(r.Body, MaxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > MaxBodyBytes {
		return nil, fmt.Errorf("body exceeds %d bytes", MaxBodyBytes)
	}
	return body, nil
}

// peekActivityID extracts the envelope's `id` field via a small
// parse pass. Lets the replay-cache short-circuit before the full
// strict-unmarshal of stage 8.
func peekActivityID(body []byte) (string, error) {
	var head struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return "", err
	}
	if head.ID == "" {
		return "", fmt.Errorf("missing id field")
	}
	// Sanity: id must be a syntactically-valid URL per spec §3.
	if _, err := url.Parse(head.ID); err != nil {
		return "", fmt.Errorf("id not a URL: %w", err)
	}
	return head.ID, nil
}

// extractObjectRef pulls (kind, uuid) from the envelope's Object
// when it's a known kind. Stored on the inbox row for admin
// filtering; nil + nil when the activity doesn't target an object
// we can classify (Follow, Block, aa:Subscribe etc.).
func extractObjectRef(env *federation.Envelope) (*string, pgtype.UUID) {
	if env.Object == "" {
		return nil, pgtype.UUID{}
	}
	// The envelope's Object is a URL. We parse host + path to
	// detect "<host>/<kind>/<uuid>" shapes per spec §8.2.
	parsed, err := url.Parse(env.Object)
	if err != nil {
		return nil, pgtype.UUID{}
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 {
		return nil, pgtype.UUID{}
	}
	kind := parts[0]
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return nil, pgtype.UUID{}
	}
	// Singular kinds in URLs (per spec §8.2: /posts/, /assets/
	// etc.) translate to our singular catalogue values.
	switch kind {
	case "posts":
		k := "post"
		return &k, pgtype.UUID{Bytes: id, Valid: true}
	case "assets":
		k := "asset"
		return &k, pgtype.UUID{Bytes: id, Valid: true}
	case "collections":
		k := "collection"
		return &k, pgtype.UUID{Bytes: id, Valid: true}
	case "workspaces":
		k := "workspace"
		return &k, pgtype.UUID{Bytes: id, Valid: true}
	case "brand_kits":
		k := "brand_kit"
		return &k, pgtype.UUID{Bytes: id, Valid: true}
	case "users":
		k := "user"
		return &k, pgtype.UUID{Bytes: id, Valid: true}
	}
	return nil, pgtype.UUID{}
}

// mapHTTPSigErr translates a httpsig package error into the
// pipeline's typed rejection.
func mapHTTPSigErr(err error) error {
	switch {
	case errors.Is(err, httpsig.ErrUnsignedRequest):
		return reject(federation.InboxStatusUnsigned, http.StatusUnauthorized, err.Error())
	case errors.Is(err, httpsig.ErrUnsupportedAlgorithm):
		return reject(federation.InboxStatusUnsupportedAlgorithm, http.StatusBadRequest, err.Error())
	case errors.Is(err, httpsig.ErrSigMalformed):
		return reject(federation.InboxStatusSigMalformed, http.StatusBadRequest, err.Error())
	case errors.Is(err, httpsig.ErrMissingHeader):
		return reject(federation.InboxStatusSigMalformed, http.StatusBadRequest, err.Error())
	case errors.Is(err, httpsig.ErrUnknownKey):
		return reject(federation.InboxStatusUnknownKey, http.StatusUnauthorized, err.Error())
	case errors.Is(err, httpsig.ErrDigestMismatch):
		return reject(federation.InboxStatusSigInvalid, http.StatusBadRequest, err.Error())
	case errors.Is(err, httpsig.ErrStaleRequest):
		return reject(federation.InboxStatusStaleRequest, http.StatusBadRequest, err.Error())
	case errors.Is(err, httpsig.ErrSigInvalid):
		return reject(federation.InboxStatusSigInvalid, http.StatusUnauthorized, err.Error())
	}
	return reject(federation.InboxStatusError, http.StatusInternalServerError, err.Error())
}

// mapEnvelopeErr translates a federation.Envelope parse error
// into the pipeline's typed rejection.
func mapEnvelopeErr(err error) error {
	switch {
	case errors.Is(err, federation.ErrInvalidContext):
		return reject(federation.InboxStatusInvalidContext, http.StatusBadRequest, err.Error())
	case errors.Is(err, federation.ErrUnknownField):
		return reject(federation.InboxStatusUnknownField, http.StatusBadRequest, err.Error())
	case errors.Is(err, federation.ErrMissingField):
		return reject(federation.InboxStatusSigMalformed, http.StatusBadRequest, err.Error())
	case errors.Is(err, federation.ErrInvalidType):
		return reject(federation.InboxStatusInvalidType, http.StatusBadRequest, err.Error())
	case errors.Is(err, federation.ErrInvalidPublished):
		return reject(federation.InboxStatusInvalidPublished, http.StatusBadRequest, err.Error())
	case errors.Is(err, federation.ErrUnsigned):
		// envelope.Unmarshal's ErrUnsigned is the envelope-layer
		// "no signature block" — route to envelope_sig_missing
		// per spec §5.6, NOT to unsigned (which is the request-
		// level HTTP-Sig header missing).
		return reject(federation.InboxStatusEnvelopeSigMissing, http.StatusUnauthorized, err.Error())
	case errors.Is(err, federation.ErrUnsupportedAlg):
		return reject(federation.InboxStatusUnsupportedAlgorithm, http.StatusBadRequest, err.Error())
	}
	return reject(federation.InboxStatusError, http.StatusBadRequest, err.Error())
}

// writeRejection serialises a pipeline rejection to the HTTP
// response. Body shape: {"error":"...","reason":"<§12.1>"} so
// peers can machine-read the failure.
func writeRejection(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	rej, ok := err.(rejection)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rej.httpStatus)
	body, _ := json.Marshal(map[string]string{
		"error":  rej.message,
		"reason": string(rej.status),
	})
	_, _ = w.Write(body)
	if logger != nil {
		logger.LogAttrs(r.Context(), slog.LevelWarn, "inbox.rejected",
			slog.String("reason", string(rej.status)),
			slog.Int("status", rej.httpStatus),
			slog.String("message", rej.message),
		)
	}
}

// auditReject calls the audit hook when wired. nil-safe.
func (h *Handler) auditReject(ctx context.Context, peerID uuid.UUID, reason federation.InboxStatus, activityURI, msg string) {
	if h.rejectAudit == nil {
		return
	}
	h.rejectAudit(ctx, peerID, reason, activityURI, msg)
}

func (h *Handler) logErr(ctx context.Context, msg string, err error) {
	if h.logger != nil {
		h.logger.LogAttrs(ctx, slog.LevelError, msg, slog.String("err", err.Error()))
	}
}

// isUniqueViolation detects a Postgres 23505 unique-constraint
// violation. The activity_uri UNIQUE index surfaces replays here.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
