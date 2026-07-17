// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// HTTP admin surface for the directory subscriber — Phase
// 1.22.B-c. Five endpoints; all gated on system.admin.

package directory

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/identity"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const (
	capAdmin = "system.admin"
	// capRead gates the directory READ surfaces (#356). Subscribe,
	// unsubscribe, poll, and publish-listing stay on capAdmin.
	capRead = "federation.read"
)

// AdminHandler is the openapi-strict adapter.
type AdminHandler struct {
	registry *Registry
	client   *Client

	// Publish-side dependencies (1.22.B-c-bis). Wired post-
	// construction via SetPublishDeps; nil-safe so subscribe-only
	// installs (no AA_MASTER_KEY, no instance identity) still get
	// list/subscribe/unsubscribe/poll working.
	identity      *identity.Manager
	instanceURLFn func(ctx context.Context) string
}

// NewAdminHandler wires the admin surface.
func NewAdminHandler(r *Registry, c *Client) *AdminHandler {
	return &AdminHandler{registry: r, client: c}
}

// SetPublishDeps wires the identity manager + instance URL
// resolver needed for the publish flow. Boot calls this once.
func (h *AdminHandler) SetPublishDeps(id *identity.Manager, instanceURL func(ctx context.Context) string) {
	h.identity = id
	h.instanceURLFn = instanceURL
}

// ListFederationDirectories — GET /admin/federation/directories.
func (h *AdminHandler) ListFederationDirectories(
	ctx context.Context,
	_ openapi.ListFederationDirectoriesRequestObject,
) (openapi.ListFederationDirectoriesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListFederationDirectories401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capRead) && !id.Can(capAdmin) {
		return openapi.ListFederationDirectories403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: capRead + " capability required"},
		}, nil
	}
	dirs, err := h.registry.List(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.FederationDirectory, len(dirs))
	for i, d := range dirs {
		items[i] = directoryToAPI(d)
	}
	return openapi.ListFederationDirectories200JSONResponse(openapi.FederationDirectoryList{Items: items}), nil
}

// SubscribeFederationDirectory — POST /admin/federation/directories.
func (h *AdminHandler) SubscribeFederationDirectory(
	ctx context.Context,
	req openapi.SubscribeFederationDirectoryRequestObject,
) (openapi.SubscribeFederationDirectoryResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.SubscribeFederationDirectory401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.SubscribeFederationDirectory403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.SubscribeFederationDirectory400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "request body required"},
		}, nil
	}
	// Fetch /v1/operator first so we have a pubkey to pin.
	op, err := h.client.FetchOperator(ctx, req.Body.DirectoryUrl)
	if err != nil {
		return openapi.SubscribeFederationDirectory400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
		}, nil
	}
	notes := ""
	if req.Body.Notes != nil {
		notes = *req.Body.Notes
	}
	d, err := h.registry.Subscribe(ctx, SubscribeInput{
		URL:                 req.Body.DirectoryUrl,
		OperatorName:        op.Name,
		OperatorPublicKey:   op.PublicKeyPEM,
		OperatorFingerprint: op.Fingerprint,
		OperatorContact:     op.Contact,
		SubscribedByUserRef: id.UserRef,
		Notes:               notes,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidURL):
			return openapi.SubscribeFederationDirectory400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: err.Error()},
			}, nil
		case errors.Is(err, ErrAlreadySubscribed):
			return openapi.SubscribeFederationDirectory409JSONResponse{Error: err.Error()}, nil
		}
		return nil, err
	}
	return openapi.SubscribeFederationDirectory201JSONResponse(directoryToAPI(*d)), nil
}

// UnsubscribeFederationDirectory — DELETE /admin/federation/directories/{id}.
func (h *AdminHandler) UnsubscribeFederationDirectory(
	ctx context.Context,
	req openapi.UnsubscribeFederationDirectoryRequestObject,
) (openapi.UnsubscribeFederationDirectoryResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.UnsubscribeFederationDirectory401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.UnsubscribeFederationDirectory403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	if err := h.registry.Unsubscribe(ctx, uuid.UUID(req.Id)); err != nil {
		if errors.Is(err, ErrDirectoryNotFound) {
			return openapi.UnsubscribeFederationDirectory404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "directory not found"},
			}, nil
		}
		return nil, err
	}
	return openapi.UnsubscribeFederationDirectory204Response{}, nil
}

// PollFederationDirectory — POST /admin/federation/directories/{id}/poll.
func (h *AdminHandler) PollFederationDirectory(
	ctx context.Context,
	req openapi.PollFederationDirectoryRequestObject,
) (openapi.PollFederationDirectoryResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.PollFederationDirectory401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.PollFederationDirectory403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	d, err := h.registry.ByID(ctx, uuid.UUID(req.Id))
	if err != nil {
		if errors.Is(err, ErrDirectoryNotFound) {
			return openapi.PollFederationDirectory404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "directory not found"},
			}, nil
		}
		return nil, err
	}
	// Best-effort poll. Errors land on the directory row's notes
	// (the engine already recorded them); we still return 200
	// with the post-poll directory so the UI can refresh.
	_ = h.client.Poll(ctx, h.registry, d)
	updated, err := h.registry.ByID(ctx, uuid.UUID(req.Id))
	if err != nil {
		return nil, err
	}
	entries, err := h.registry.ListEntries(ctx, uuid.UUID(req.Id), 0)
	if err != nil {
		return nil, err
	}
	return openapi.PollFederationDirectory200JSONResponse(openapi.FederationDirectoryPollResult{
		Directory:  directoryToAPI(*updated),
		EntryCount: len(entries),
	}), nil
}

// ListFederationDirectoryEntries — GET /admin/federation/directories/{id}/entries.
func (h *AdminHandler) ListFederationDirectoryEntries(
	ctx context.Context,
	req openapi.ListFederationDirectoryEntriesRequestObject,
) (openapi.ListFederationDirectoryEntriesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.ListFederationDirectoryEntries401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capRead) && !id.Can(capAdmin) {
		return openapi.ListFederationDirectoryEntries403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: capRead + " capability required"},
		}, nil
	}
	if _, err := h.registry.ByID(ctx, uuid.UUID(req.Id)); err != nil {
		if errors.Is(err, ErrDirectoryNotFound) {
			return openapi.ListFederationDirectoryEntries404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "directory not found"},
			}, nil
		}
		return nil, err
	}
	limit := int32(200)
	if req.Params.Limit != nil {
		limit = int32(*req.Params.Limit)
	}
	entries, err := h.registry.ListEntries(ctx, uuid.UUID(req.Id), limit)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.FederationDirectoryEntry, len(entries))
	for i, e := range entries {
		items[i] = entryToAPI(e)
	}
	return openapi.ListFederationDirectoryEntries200JSONResponse(openapi.FederationDirectoryEntryList{
		Items: items,
	}), nil
}

// --- publish admin endpoints (Phase 1.22.B-c-bis) -----------------------

// RequestFederationDirectoryPublishChallenge — POST /admin/federation/
// directories/{id}/publish/challenge.
func (h *AdminHandler) RequestFederationDirectoryPublishChallenge(
	ctx context.Context,
	req openapi.RequestFederationDirectoryPublishChallengeRequestObject,
) (openapi.RequestFederationDirectoryPublishChallengeResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.RequestFederationDirectoryPublishChallenge401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.RequestFederationDirectoryPublishChallenge403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	if h.instanceURLFn == nil {
		return openapi.RequestFederationDirectoryPublishChallenge503JSONResponse{
			Error: "instance base URL not configured (set System → Site → Base URL)",
		}, nil
	}
	instanceURL := h.instanceURLFn(ctx)
	if instanceURL == "" {
		return openapi.RequestFederationDirectoryPublishChallenge503JSONResponse{
			Error: "instance base URL not configured (set System → Site → Base URL)",
		}, nil
	}
	d, err := h.registry.ByID(ctx, uuid.UUID(req.Id))
	if err != nil {
		if errors.Is(err, ErrDirectoryNotFound) {
			return openapi.RequestFederationDirectoryPublishChallenge404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "directory not found"},
			}, nil
		}
		return nil, err
	}
	updated, err := h.client.RequestChallenge(ctx, h.registry, d, instanceURL)
	if err != nil {
		return openapi.RequestFederationDirectoryPublishChallenge503JSONResponse{
			Error: err.Error(),
		}, nil
	}
	return openapi.RequestFederationDirectoryPublishChallenge200JSONResponse(directoryToAPI(*updated)), nil
}

// RegisterFederationDirectoryPublishListing — POST /admin/federation/
// directories/{id}/publish/register.
func (h *AdminHandler) RegisterFederationDirectoryPublishListing(
	ctx context.Context,
	req openapi.RegisterFederationDirectoryPublishListingRequestObject,
) (openapi.RegisterFederationDirectoryPublishListingResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.RegisterFederationDirectoryPublishListing401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(capAdmin) {
		return openapi.RegisterFederationDirectoryPublishListing403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.RegisterFederationDirectoryPublishListing400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "request body required"},
		}, nil
	}
	if h.identity == nil || h.instanceURLFn == nil {
		return openapi.RegisterFederationDirectoryPublishListing400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "publish flow not configured (set System → Site → Base URL + ensure AA_MASTER_KEY is set)"},
		}, nil
	}
	instanceURL := h.instanceURLFn(ctx)
	if instanceURL == "" {
		return openapi.RegisterFederationDirectoryPublishListing400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "instance base URL not configured"},
		}, nil
	}
	dir, err := h.registry.ByID(ctx, uuid.UUID(req.Id))
	if err != nil {
		if errors.Is(err, ErrDirectoryNotFound) {
			return openapi.RegisterFederationDirectoryPublishListing404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "directory not found"},
			}, nil
		}
		return nil, err
	}

	// Persist the operator-chosen metadata FIRST so it sticks even
	// if the register POST fails (operator's typing isn't lost on
	// retry).
	meta := PublishMetadata{DisplayName: req.Body.DisplayName}
	if req.Body.Region != nil {
		meta.Region = *req.Body.Region
	}
	if req.Body.Description != nil {
		meta.Description = *req.Body.Description
	}
	if req.Body.Tags != nil {
		meta.Tags = *req.Body.Tags
	}
	if _, err := h.registry.SetPublishMetadata(ctx, dir.ID, meta); err != nil {
		return nil, err
	}
	dir, err = h.registry.ByID(ctx, dir.ID)
	if err != nil {
		return nil, err
	}

	updated, regErr := h.client.RegisterListing(ctx, h.registry, dir, h.identity, instanceURL, meta)
	if regErr != nil {
		// Best-effort response — `updated` carries the row state
		// even on failure (Client.RegisterListing flips to failed
		// + populates publish_last_error before returning). The UI
		// reads publish_last_error to show the user.
		if updated == nil {
			return openapi.RegisterFederationDirectoryPublishListing400JSONResponse{
				BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: regErr.Error()},
			}, nil
		}
		return openapi.RegisterFederationDirectoryPublishListing200JSONResponse(directoryToAPI(*updated)), nil
	}
	// Successful registration — defensive type-checked log so
	// future drift in the federation typed catalogue surfaces.
	_ = federation.PublishStatusListed
	return openapi.RegisterFederationDirectoryPublishListing200JSONResponse(directoryToAPI(*updated)), nil
}

// --- adapters -----------------------------------------------------------

func directoryToAPI(d Directory) openapi.FederationDirectory {
	out := openapi.FederationDirectory{
		Id:                  d.ID,
		DirectoryUrl:        d.URL,
		OperatorName:        d.OperatorName,
		OperatorPublicKey:   d.OperatorPublicKey,
		OperatorFingerprint: d.OperatorFingerprint,
		Enabled:             d.Enabled,
		SubscribedByUserRef: d.SubscribedByUserRef,
		LastPollStatus:      openapi.FederationDirectoryLastPollStatus(d.LastPollStatus),
		LastPollError:       d.LastPollError,
		PollIntervalSeconds: int(d.PollIntervalSeconds),
		Notes:               d.Notes,
		PublishStatus:       ptrFederationDirectoryPublishStatus(d.PublishStatus),
		PublishPendingToken: strPtr(d.PublishPendingToken),
		PublishRecordName:   strPtr(d.PublishRecordName),
		PublishRecordValue:  strPtr(d.PublishRecordValue),
		PublishListingId:    strPtr(d.PublishListingID),
		PublishLastError:    strPtr(d.PublishLastError),
		PublishDisplayName:  strPtr(d.PublishDisplayName),
		PublishRegion:       strPtr(d.PublishRegion),
		PublishDescription:  strPtr(d.PublishDescription),
		PublishTags:         &d.PublishTags,
	}
	if d.OperatorContact != "" {
		c := d.OperatorContact
		out.OperatorContact = &c
	}
	if d.SubscribedAt.Valid {
		out.SubscribedAt = d.SubscribedAt.Time
	}
	if d.LastPolledAt.Valid {
		t := d.LastPolledAt.Time
		out.LastPolledAt = &t
	}
	if d.PublishTokenExpiresAt.Valid {
		t := d.PublishTokenExpiresAt.Time
		out.PublishTokenExpiresAt = &t
	}
	if d.PublishLastAttemptAt.Valid {
		t := d.PublishLastAttemptAt.Time
		out.PublishLastAttemptAt = &t
	}
	return out
}

// strPtr returns nil for empty strings, a pointer otherwise.
// Lets the JSON omit empty fields when the publish state isn't
// engaged for a directory.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrFederationDirectoryPublishStatus(s federation.PublishStatus) *openapi.FederationDirectoryPublishStatus {
	v := openapi.FederationDirectoryPublishStatus(s)
	return &v
}

func entryToAPI(e Entry) openapi.FederationDirectoryEntry {
	out := openapi.FederationDirectoryEntry{
		Id:                e.ID,
		DirectoryId:       e.DirectoryID,
		InstanceUrl:       e.InstanceURL,
		DisplayName:       e.DisplayName,
		InstancePublicKey: e.InstancePublicKey,
		Fingerprint:       e.Fingerprint,
		VerifiedVia:       e.VerifiedVia,
	}
	if e.Region != "" {
		r := e.Region
		out.Region = &r
	}
	if e.Description != "" {
		d := e.Description
		out.Description = &d
	}
	if len(e.Tags) > 0 {
		out.Tags = &e.Tags
	}
	if e.ListingID != "" {
		l := e.ListingID
		out.ListingId = &l
	}
	if e.VerifiedAt.Valid {
		out.VerifiedAt = e.VerifiedAt.Time
	}
	if e.CachedAt.Valid {
		out.CachedAt = e.CachedAt.Time
	}
	return out
}
