// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The access-copy route (#1597, spec v1 §8.5.1, §7.5.7 "Access copies",
// §12.4, §16.7; proposed ADR 0103).
//
//	GET  /federation/assets/{asset_id}/versions/{n}/files/access/{sha256}?size={bytes}
//	HEAD (same path)
//
// It is registered as its own handler (Handler.Mount), so the route, never
// anything a request carries, decides that this is an access-copy request.
// It shares the original route's signature check (authenticate: instance or
// pinned actor key, enabled and connected peer), its per-peer rate limiter
// and its whole-object serving (serveObject). The original route and its
// answers are unchanged.
//
// # The decision, per request
//
//  1. An empty body, read at most one byte (any byte, declared or not, is
//     400 bad_request); then the signature, over that empty body and the
//     request target (path and query), so it binds asset, version, class,
//     hash and size. Failure: 401 unauthenticated (503 when the lookup
//     itself fails).
//  2. The per-peer rate limit. Failure: 429.
//  3. The shape, checked strictly: a canonical asset UUID; the class
//     segment exactly `access`; a canonical positive decimal version that
//     fits int64; a lowercase 64-hex SHA-256; and a query that is exactly
//     one canonical non-negative decimal `size` that fits int64. Anything
//     else is 400 bad_request. These checks belong to this route only; the
//     original route keeps its own parsing.
//  4. shares.Registry.CanPeerFetchAccessCopy, read from this instance's
//     database with the authenticated peer and actor and the route's
//     asset, version, hash and size: the peer's state and all four
//     profiles, the actor, a live local asset, the sealed version's own
//     verified access copy, this origin's announcement of it to the peer,
//     the tier, and a live share carrying allow_access_copies. Peer-level
//     refusals keep their §12.4 answers (profile_not_negotiated,
//     peer_disabled, peer_not_connected, actor_not_on_peer); every refusal
//     that concerns the asset, its version, its file or a grant is the same
//     403 not_available, with the precise reason only in the
//     federation.cas.refused audit row. A failed decision is 503.
//  5. The bytes: the authorised copy's own stored object (its canonical
//     key), never the asset's original, served whole by serveObject with
//     its read preflight: for a GET of a non-empty copy, the first bytes
//     are read before the 200 is committed, so a store that cannot read
//     them answers a retryable 503 temporarily_unavailable without the
//     copy's length or digest.
//
// No production peer can reach step 4 yet: derived-transfer-1 is not
// advertised (peer.CapDerivedTransfer1), so no stored intersection holds
// it, and the decision answers profile_not_negotiated.

package cas

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
)

// AccessRoutePattern is the chi pattern of the access-copy route.
const AccessRoutePattern = "/federation/assets/{asset_id}/versions/{n}/files/{class}/{sha256}"

// AccessFilePath returns the request path for one access copy of version n.
// Clients build their URL from the paired peer's registered instance URL
// plus this path, never from anything an envelope carries.
func AccessFilePath(assetID uuid.UUID, version int64, sha256Hex string) string {
	return "/federation/assets/" + assetID.String() + "/versions/" + strconv.FormatInt(version, 10) +
		"/files/" + ClassAccess + "/" + sha256Hex
}

// accessNotServed is the access-copy route's fixed error text.
const accessNotServed = "access copy not served"

var (
	versionPattern     = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	accessQueryPattern = regexp.MustCompile(`^size=(0|[1-9][0-9]{0,18})$`)
)

// accessShape is one well-formed access-copy request.
type accessShape struct {
	assetID uuid.UUID
	version int64
	hash    string
	size    int64
}

// parseAccessShape checks the route's path values and raw query strictly:
// only their canonical spelling is accepted, so one copy has exactly one
// request target.
func parseAccessShape(r *http.Request) (accessShape, error) {
	var s accessShape
	rawAsset := chi.URLParam(r, "asset_id")
	id, err := uuid.Parse(rawAsset)
	if err != nil || id.String() != rawAsset {
		return s, errors.New("asset id is not a canonical UUID")
	}
	if chi.URLParam(r, "class") != ClassAccess {
		return s, errors.New("class is not access")
	}
	rawVersion := chi.URLParam(r, "n")
	if !versionPattern.MatchString(rawVersion) {
		return s, errors.New("version is not a canonical positive integer")
	}
	version, err := strconv.ParseInt(rawVersion, 10, 64)
	if err != nil {
		return s, errors.New("version does not fit int64")
	}
	hash := chi.URLParam(r, "sha256")
	if !hashPattern.MatchString(hash) {
		return s, errors.New("hash is not lowercase 64-hex")
	}
	m := accessQueryPattern.FindStringSubmatch(r.URL.RawQuery)
	if m == nil {
		return s, errors.New("query is not exactly one canonical size")
	}
	size, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return s, errors.New("size does not fit int64")
	}
	return accessShape{assetID: id, version: version, hash: hash, size: size}, nil
}

// serveAccess serves one access copy (the package comment of access.go).
func (h *Handler) serveAccess(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta := map[string]any{"method": r.Method, "class": ClassAccess}
	refuse := func(status int, reason, internal string) {
		h.refuseWith(ctx, w, accessNotServed, status, reason, internal, meta)
	}

	// The body must be empty (§8.5: the signature's digest is of the empty
	// body), also when its length is not declared. At most one byte is
	// read: any byte, or a body that cannot be read, is a bad request.
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(b) != 0 {
			refuse(http.StatusBadRequest, ReasonBadRequest, "body_not_empty")
			return
		}
	}

	// 1. Signature and peer, over the empty body.
	s, err := h.authenticate(ctx, r, []byte{})
	if err != nil {
		if errors.Is(err, errUnauthenticated) {
			meta["detail"] = err.Error()
			refuse(http.StatusUnauthorized, ReasonUnauthenticated, "unauthenticated")
			return
		}
		h.logErr(ctx, "federation.cas.auth.error", err)
		refuse(http.StatusServiceUnavailable, ReasonUnavailable, "lookup_error")
		return
	}
	meta["peer_id"], meta["signer"] = s.peer.ID.String(), s.keyID

	// 2. Rate, shared with the original route.
	if ok, retry := h.d.Limiter.Allow(s.peer.ID); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		refuse(http.StatusTooManyRequests, ReasonRateLimited, "rate_limited")
		return
	}

	// 3. Shape.
	shape, err := parseAccessShape(r)
	if err != nil {
		meta["detail"] = err.Error()
		refuse(http.StatusBadRequest, ReasonBadRequest, "bad_request")
		return
	}
	meta["asset_id"], meta["version"], meta["hash"], meta["size"] = shape.assetID.String(), shape.version, shape.hash, shape.size

	// 4. The #1597 decision, from this instance's database.
	dec, err := h.d.Policy.CanPeerFetchAccessCopy(ctx, shares.AccessCopyFetchRequest{
		PeerID:   s.peer.ID,
		ActorURL: s.actorURI,
		AssetID:  shape.assetID,
		Version:  shape.version,
		SHA256:   shape.hash,
		Size:     shape.size,
	})
	if err != nil {
		h.logErr(ctx, "federation.cas.access_policy.error", err)
		refuse(http.StatusServiceUnavailable, ReasonUnavailable, "policy_error")
		return
	}
	if !dec.Allowed {
		status, reason := externalRefusal(dec.Reason)
		refuse(status, reason, string(dec.Reason))
		return
	}
	meta["share_id"] = dec.Share.ID.String()

	// 5. The authorised copy's own object.
	h.serveObject(ctx, w, r, accessNotServed, shape.hash, shape.size, true, meta)
}
