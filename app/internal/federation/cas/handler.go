// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package cas serves an asset's original file to a paired peer (#72,
// spec v1 §8.5, §7.5.7), and, on its own version-bound route, one access
// copy of a sealed version (#1597, spec v1 §8.5.1; access.go).
//
//	GET  /federation/assets/{asset_id}/files/original/{sha256}?size={bytes}
//	HEAD (same path)
//	GET  /federation/assets/{asset_id}/versions/{n}/files/access/{sha256}?size={bytes}
//	HEAD (same path)
//
// What follows describes the original-file route; access.go describes the
// access-copy route, which shares the signature, the per-peer limit and the
// whole-object serving below.
//
// Mounted at the root, outside /api/v1, like /federation/inbox and the
// actor document: it is authenticated by HTTP signature, not by a
// session. It never reuses the local /assets/{id}/variants or
// /storage/objects routes (#723).
//
// # The decision, per request
//
//  1. The request is signed (draft-cavage, hs2019) over
//     (request-target), host, date and digest, so the signature binds
//     asset, class, hash and size; Date is within the replay window.
//     The signer is either the peer's instance key or an actor key of
//     that peer (<peer>/users/<name>#main-key, resolved and pinned as
//     in #1526). The peer must be known, enabled and connected.
//     Failure: 401 unauthenticated.
//  2. The per-peer rate limit. Failure: 429.
//  3. The byte class: only `original`. `access` and `preview` are
//     refused here with class_not_available, also when an access copy
//     may be fetched: that is only through the version-bound route
//     (§8.5.1, access.go). Anything else is a bad request.
//  4. original-transfer-1 in the stored intersection: 403
//     profile_not_negotiated.
//  5. shares.CanPeerFetchOriginal (#1573), read from this instance's
//     database: peer state and profile again, actor on the peer's
//     origin, the asset and its stored original's hash and size, this
//     instance being the origin, public or team tier, and a live share
//     to this peer (and actor, for a user-scoped share) carrying
//     allow_original whose grantor owns the asset now. An
//     instance-signed request can only use a share naming no recipient.
//
// Every asset-level refusal (unknown asset, wrong hash or size, tier,
// no share, wrong user, expired, revoked, grantor not owner, not
// granted) answers the same 403 not_available, so the endpoint cannot
// be used to learn whether an asset exists. The precise reason is in
// the federation.cas.refused audit row.
//
// HEAD runs the same decision and answers with headers only.
//
// The grant is checked when the request starts. A revoke during a
// transfer stops later requests; bytes already sent are not recalled.
package cas

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/federation/httpsig"
	"github.com/mscrnt/artist-alley/app/internal/federation/inbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/peer"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// RoutePattern is the chi pattern the handler is mounted at.
const RoutePattern = "/federation/assets/{asset_id}/files/{class}/{sha256}"

// FilePath returns the request path for one original file. Clients
// build their URL from the paired peer's registered instance URL plus
// this path, never from anything an envelope carries.
func FilePath(assetID uuid.UUID, class, sha256Hex string) string {
	return "/federation/assets/" + assetID.String() + "/files/" + class + "/" + sha256Hex
}

// Byte classes (spec §7.5.7).
const (
	ClassOriginal = "original"
	ClassAccess   = "access"
	ClassPreview  = "preview"
)

// External reasons (spec §12.4).
const (
	ReasonUnauthenticated      = "unauthenticated"
	ReasonBadRequest           = "bad_request"
	ReasonRateLimited          = "rate_limited"
	ReasonClassNotAvailable    = "class_not_available"
	ReasonProfileNotNegotiated = "profile_not_negotiated"
	ReasonPeerDisabled         = "peer_disabled"
	ReasonPeerNotConnected     = "peer_not_connected"
	ReasonActorNotOnPeer       = "actor_not_on_peer"
	ReasonNotAvailable         = "not_available"
	ReasonUnavailable          = "temporarily_unavailable"
)

// Policy is the origin's byte decisions (shares.Registry): the #1573
// original-file decision and the #1597 access-copy decision. Each route
// asks only its own.
type Policy interface {
	CanPeerFetchOriginal(ctx context.Context, req shares.OriginalFetchRequest) (shares.OriginalFetchDecision, error)
	CanPeerFetchAccessCopy(ctx context.Context, req shares.AccessCopyFetchRequest) (shares.AccessCopyFetchDecision, error)
}

// ByteSource opens stored bytes (storage.Service).
type ByteSource interface {
	Download(ctx context.Context, hash, variant string) (io.ReadCloser, *storage.ObjectInfo, error)
	Stat(ctx context.Context, hash, variant string) (*storage.ObjectInfo, error)
}

// Auditor records federation.cas.* events (audit.Recorder).
type Auditor interface {
	FederationCAS(ctx context.Context, eventType string, meta map[string]any)
}

// Deps wires the handler.
type Deps struct {
	Lookup    inbox.PeerLookup       // instance keyId -> peer
	ActorKeys inbox.ActorKeyResolver // actor key, fetched and pinned (#1526)
	Policy    Policy
	Bytes     ByteSource
	Limiter   *inbox.PeerRateLimiter
	Audit     Auditor
	Clock     func() time.Time
	Logger    *slog.Logger
}

// Handler serves original files to paired peers.
type Handler struct{ d Deps }

// NewHandler builds the handler. A nil Limiter gets the inbox defaults.
func NewHandler(d Deps) *Handler {
	if d.Limiter == nil {
		d.Limiter = inbox.NewPeerRateLimiter(100, 100)
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	return &Handler{d: d}
}

// Mount registers GET and HEAD for the original-file route and, as its own
// handler, the version-bound access-copy route (access.go). The route
// decides the mode; nothing in a request does.
func (h *Handler) Mount(r chi.Router) {
	r.Get(RoutePattern, h.ServeHTTP)
	r.Head(RoutePattern, h.ServeHTTP)
	r.Get(AccessRoutePattern, h.serveAccess)
	r.Head(AccessRoutePattern, h.serveAccess)
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxRequestBody caps the (expected empty) body the signature digest is
// computed over.
const maxRequestBody = 4 << 10

// signer is who signed the request.
type signer struct {
	peer     inbox.PeerInfo
	keyID    string
	actorURI string // "" for the instance key
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta := map[string]any{"method": r.Method}

	// Shape first: nothing below runs on a malformed request.
	assetID, err := uuid.Parse(chi.URLParam(r, "asset_id"))
	class := chi.URLParam(r, "class")
	hash := chi.URLParam(r, "sha256")
	size, sizeErr := parseSize(r.URL.Query())
	if err != nil || !hashPattern.MatchString(hash) || sizeErr != nil {
		h.refuse(ctx, w, http.StatusBadRequest, ReasonBadRequest, "bad_request", meta)
		return
	}
	meta["asset_id"], meta["class"], meta["hash"], meta["size"] = assetID.String(), class, hash, size

	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		if err != nil || len(b) > maxRequestBody {
			h.refuse(ctx, w, http.StatusBadRequest, ReasonBadRequest, "body_too_large", meta)
			return
		}
		body = b
	}

	// 1. Signature and peer.
	s, err := h.authenticate(ctx, r, body)
	if err != nil {
		if errors.Is(err, errUnauthenticated) {
			meta["detail"] = err.Error()
			h.refuse(ctx, w, http.StatusUnauthorized, ReasonUnauthenticated, "unauthenticated", meta)
			return
		}
		h.logErr(ctx, "federation.cas.auth.error", err)
		h.refuse(ctx, w, http.StatusServiceUnavailable, ReasonUnavailable, "lookup_error", meta)
		return
	}
	meta["peer_id"], meta["signer"] = s.peer.ID.String(), s.keyID

	// 2. Rate.
	if ok, retry := h.d.Limiter.Allow(s.peer.ID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		h.refuse(ctx, w, http.StatusTooManyRequests, ReasonRateLimited, "rate_limited", meta)
		return
	}

	// 3. Class.
	switch class {
	case ClassOriginal:
	case ClassAccess, ClassPreview:
		h.refuse(ctx, w, http.StatusForbidden, ReasonClassNotAvailable, "class_not_available", meta)
		return
	default:
		h.refuse(ctx, w, http.StatusBadRequest, ReasonBadRequest, "unknown_class", meta)
		return
	}

	// 4. Profile, from the stored intersection.
	if !hasCapability(s.peer.Capabilities, string(peer.CapOriginalTransfer1)) {
		h.refuse(ctx, w, http.StatusForbidden, ReasonProfileNotNegotiated, "profile_not_negotiated", meta)
		return
	}

	// 5. The #1573 decision, from this instance's database.
	dec, err := h.d.Policy.CanPeerFetchOriginal(ctx, shares.OriginalFetchRequest{
		PeerID:   s.peer.ID,
		ActorURL: s.actorURI,
		AssetID:  assetID,
		SHA256:   hash,
		Size:     size,
	})
	if err != nil {
		h.logErr(ctx, "federation.cas.policy.error", err)
		h.refuse(ctx, w, http.StatusServiceUnavailable, ReasonUnavailable, "policy_error", meta)
		return
	}
	if !dec.Allowed {
		status, reason := externalRefusal(dec.Reason)
		h.refuse(ctx, w, status, reason, string(dec.Reason), meta)
		return
	}
	meta["share_id"] = dec.Share.ID.String()

	h.serveObject(ctx, w, r, "original file not served", hash, size, false, meta)
}

// preflightBytes bounds serveObject's read before the status is committed.
const preflightBytes = 32 << 10

// serveObject sends the whole stored object hash (its canonical
// VariantOriginal key) after a decision allowed it: the stored length must
// equal size, GET streams the bytes and HEAD sends the headers only, with no
// Range. A failure before headers are written is a retryable 503
// temporarily_unavailable (internal bytes_unavailable) without the object's
// length or digest. With preflight, a GET of a non-empty object first reads
// up to preflightBytes, so a reader that fails or ends early is still
// answered 503; those bytes are then sent and the rest streamed. Once the 200
// is written the status cannot change: a short copy, including one that ends
// in a bare EOF, is recorded in the served audit with the bytes actually sent
// and a copy_error. Shared by the original route (no preflight) and the
// access-copy route (preflight); message is the route's fixed error text.
func (h *Handler) serveObject(ctx context.Context, w http.ResponseWriter, r *http.Request, message, hash string, size int64, preflight bool, meta map[string]any) {
	unavailable := func() {
		w.Header().Del("Content-Length")
		w.Header().Del("Repr-Digest")
		h.refuseWith(ctx, w, message, http.StatusServiceUnavailable, ReasonUnavailable, "bytes_unavailable", meta)
	}
	info, err := h.d.Bytes.Stat(ctx, hash, storage.VariantOriginal)
	if err != nil || info == nil || info.Size != size {
		// The row says the bytes exist; the backend disagrees. An
		// operator problem, not the peer's: retryable, audited.
		unavailable()
		return
	}
	digest, _ := hex.DecodeString(hash)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Repr-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest)+":")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		h.audit(ctx, audit.EventFederationCASServed, meta)
		return
	}
	rc, _, err := h.d.Bytes.Download(ctx, hash, storage.VariantOriginal)
	if err != nil {
		unavailable()
		return
	}
	defer rc.Close()
	var head []byte
	if preflight && size > 0 {
		head = make([]byte, min(size, preflightBytes))
		if _, err := io.ReadFull(rc, head); err != nil {
			meta["read_error"] = err.Error()
			unavailable()
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	var sent int64
	var copyErr error
	if len(head) > 0 {
		n, err := w.Write(head)
		sent, copyErr = int64(n), err
	}
	if copyErr == nil {
		n, err := io.Copy(w, io.LimitReader(rc, size-sent))
		sent, copyErr = sent+n, err
	}
	meta["bytes_sent"] = sent
	switch {
	case copyErr != nil:
		meta["copy_error"] = copyErr.Error()
	case sent < size:
		meta["copy_error"] = fmt.Sprintf("short copy: %d of %d bytes", sent, size)
	}
	h.audit(ctx, audit.EventFederationCASServed, meta)
}

// externalRefusal maps a #1573 reason to what the peer is told. Only
// peer-level reasons, which say nothing about any asset, are passed on.
func externalRefusal(r shares.RejectReason) (int, string) {
	switch r {
	case shares.RejectReasonPeerDisabled:
		return http.StatusForbidden, ReasonPeerDisabled
	case shares.RejectReasonPeerNotConnected:
		return http.StatusForbidden, ReasonPeerNotConnected
	case shares.RejectReasonProfileNotNegotiated:
		return http.StatusForbidden, ReasonProfileNotNegotiated
	case shares.RejectReasonActorNotOnPeer:
		return http.StatusForbidden, ReasonActorNotOnPeer
	case shares.RejectReasonUnknownPeer:
		return http.StatusUnauthorized, ReasonUnauthenticated
	}
	return http.StatusForbidden, ReasonNotAvailable
}

var errUnauthenticated = errors.New("cas: request is not signed by an enabled, connected peer or one of its actors")

// authenticate resolves and verifies the signer. An actor keyId
// (<origin>/users/<name>#main-key) is verified against that actor's
// pinned key on the peer at <origin>; any other keyId is the peer's
// instance key.
func (h *Handler) authenticate(ctx context.Context, r *http.Request, body []byte) (signer, error) {
	params, err := httpsig.ParseSignatureHeader(r.Header.Get("Signature"))
	if err != nil {
		return signer{}, errors.Join(errUnauthenticated, err)
	}
	actorURI, origin, isActor := actorKey(params.KeyID)
	if !isActor {
		p, err := inbox.AuthenticatePeerRequest(ctx, r, body, h.d.Lookup, h.d.Clock())
		if err != nil {
			if errors.Is(err, inbox.ErrPeerUnauthenticated) {
				return signer{}, errors.Join(errUnauthenticated, err)
			}
			return signer{}, err
		}
		return signer{peer: p, keyID: params.KeyID}, nil
	}
	if h.d.ActorKeys == nil {
		return signer{}, errors.Join(errUnauthenticated, errors.New("actor signatures not configured"))
	}
	p, err := h.d.Lookup.ByKeyID(ctx, origin+"/federation/instance#main-key")
	if err != nil {
		if errors.Is(err, inbox.ErrPeerNotFound) {
			return signer{}, errors.Join(errUnauthenticated, err)
		}
		return signer{}, err
	}
	if !p.Enabled || !p.Connected {
		return signer{}, errors.Join(errUnauthenticated, errors.New("peer disabled or not connected"))
	}
	if !inbox.ActorBoundToPeer(p.InstanceURL, actorURI) {
		return signer{}, errors.Join(errUnauthenticated, errors.New("actor is not on the peer's origin"))
	}
	pub, err := h.d.ActorKeys.SigningKey(ctx, p, actorURI)
	if err != nil {
		if errors.Is(err, inbox.ErrActorUnknown) {
			return signer{}, errors.Join(errUnauthenticated, err)
		}
		return signer{}, err
	}
	resolve := func(keyID string) (ed25519.PublicKey, error) {
		if keyID != params.KeyID {
			return nil, httpsig.ErrUnknownKey
		}
		return pub, nil
	}
	if _, err := httpsig.Verify(r, body, resolve, h.d.Clock()); err != nil {
		return signer{}, errors.Join(errUnauthenticated, err)
	}
	return signer{peer: p, keyID: params.KeyID, actorURI: actorURI}, nil
}

// actorKey reports whether keyID is an actor key
// "<scheme>://<host>/users/<segment>#main-key", returning the actor URI
// and its origin.
func actorKey(keyID string) (actorURI, origin string, ok bool) {
	u, err := url.Parse(keyID)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Fragment != "main-key" || u.RawQuery != "" || u.User != nil {
		return "", "", false
	}
	seg, found := strings.CutPrefix(u.Path, "/users/")
	if !found || seg == "" || strings.Contains(seg, "/") {
		return "", "", false
	}
	origin = u.Scheme + "://" + u.Host
	return origin + "/users/" + seg, origin, true
}

// parseSize requires exactly one non-negative integer `size` and no
// other query parameter.
func parseSize(q url.Values) (int64, error) {
	if len(q) != 1 || len(q["size"]) != 1 {
		return 0, errors.New("size is required and must be the only query parameter")
	}
	raw := q.Get("size")
	if raw == "" || (len(raw) > 1 && raw[0] == '0') {
		return 0, errors.New("size must be a canonical non-negative integer")
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("size must be a non-negative integer")
	}
	return n, nil
}

func hasCapability(caps []string, token string) bool {
	for _, c := range caps {
		if c == token {
			return true
		}
	}
	return false
}

func (h *Handler) refuse(ctx context.Context, w http.ResponseWriter, status int, reason, internal string, meta map[string]any) {
	h.refuseWith(ctx, w, "original file not served", status, reason, internal, meta)
}

// refuseWith audits the precise internal reason and answers the external
// one as {"error", "reason"}; message is the route's fixed error text.
func (h *Handler) refuseWith(ctx context.Context, w http.ResponseWriter, message string, status int, reason, internal string, meta map[string]any) {
	meta["status"] = status
	meta["reason"] = internal
	h.audit(ctx, audit.EventFederationCASRefused, meta)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "reason": reason})
}

func (h *Handler) audit(ctx context.Context, event string, meta map[string]any) {
	if h.d.Audit != nil {
		h.d.Audit.FederationCAS(context.WithoutCancel(ctx), event, meta)
	}
}

func (h *Handler) logErr(ctx context.Context, msg string, err error) {
	if h.d.Logger != nil {
		h.d.Logger.LogAttrs(ctx, slog.LevelError, msg, slog.String("err", err.Error()))
	}
}
