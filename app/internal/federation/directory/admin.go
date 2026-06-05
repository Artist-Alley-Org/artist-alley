// HTTP admin surface for the directory subscriber — Phase
// 1.22.B-c. Five endpoints; all gated on system.admin.

package directory

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const capAdmin = "system.admin"

// AdminHandler is the openapi-strict adapter.
type AdminHandler struct {
	registry *Registry
	client   *Client
}

// NewAdminHandler wires the admin surface.
func NewAdminHandler(r *Registry, c *Client) *AdminHandler {
	return &AdminHandler{registry: r, client: c}
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
	if !id.Can(capAdmin) {
		return openapi.ListFederationDirectories403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
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
	if !id.Can(capAdmin) {
		return openapi.ListFederationDirectoryEntries403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "system.admin capability required"},
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
	return out
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
