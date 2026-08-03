// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// HTTP surface for operator-authored email templates (#795, ADR 0081
// §2 as amended).
//
//	GET    /email-templates                    — catalogue      (system.config.read)
//	PUT    /email-templates/{template}/{part}  — override one   (system.config.write)
//	DELETE /email-templates/{template}/{part}  — revert one     (system.config.write)
//
// Kept in its own file so non-HTTP consumers of the package (Render, the
// job handler) don't drag in the openapi import — the sitetext /
// featured convention.

package email

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// MaxOverrideLength caps a single override body. The longest shipped
// html body is a few kilobytes; the cap exists to stop a misdirected
// paste from wedging a row, not to express a real authoring limit.
const MaxOverrideLength = 65536

// HTTPHandler adapts the domain TemplateStore to the strict-server
// contract.
type HTTPHandler struct {
	store  *TemplateStore
	logger *slog.Logger
}

// NewHTTPHandler builds the adapter.
func NewHTTPHandler(store *TemplateStore, logger *slog.Logger) *HTTPHandler {
	return &HTTPHandler{store: store, logger: logger}
}

// ---------------------------------------------------------------------------
// GET /email-templates
// ---------------------------------------------------------------------------

// GetEmailTemplates returns every template with its shipped body, any
// override, and its documented field set. Requires system.config.read.
func (h *HTTPHandler) GetEmailTemplates(
	ctx context.Context,
	_ openapi.GetEmailTemplatesRequestObject,
) (openapi.GetEmailTemplatesResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetEmailTemplates401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapConfigRead) && !id.Can(CapConfigWrite) && !id.Can(CapSystemAdmin) {
		return openapi.GetEmailTemplates403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapConfigRead + " capability required"},
		}, nil
	}

	rows, err := h.store.ListRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("email: get templates: %w", err)
	}
	// Index overrides by (template, part) for the merge below.
	type ovr struct {
		body      string
		updatedAt *time.Time
	}
	byKey := make(map[string]map[string]ovr, len(rows))
	for _, r := range rows {
		m, ok := byKey[r.TemplateName]
		if !ok {
			m = make(map[string]ovr)
			byKey[r.TemplateName] = m
		}
		o := ovr{body: r.Body}
		if r.UpdatedAt.Valid {
			t := r.UpdatedAt.Time
			o.updatedAt = &t
		}
		m[r.Part] = o
	}

	var events []openapi.EmailTemplateEvent
	for _, name := range TemplateNames() {
		shipped, ok := ShippedParts(name)
		if !ok {
			continue
		}
		var parts []openapi.EmailTemplatePart
		// Stable order: subject, text, then html when present.
		for _, part := range []string{PartSubject, PartText, PartHTML} {
			src, has := shipped[part]
			if !has {
				continue
			}
			p := openapi.EmailTemplatePart{Part: part, Shipped: src}
			if o, ok := byKey[name][part]; ok {
				p.Overridden = true
				b := o.body
				p.Body = &b
				p.UpdatedAt = o.updatedAt
			}
			parts = append(parts, p)
		}
		events = append(events, openapi.EmailTemplateEvent{
			Name:        name,
			Description: describe(name),
			Parts:       parts,
			Fields:      wireFields(name),
		})
	}
	return openapi.GetEmailTemplates200JSONResponse{Templates: events}, nil
}

// describe returns the view-model's plain-word event summary, or a
// terse fallback if a template somehow has no declared view-model.
func describe(name string) string {
	if vm, ok := ViewModelFor(name); ok {
		return vm.Description
	}
	return "An email this instance can send."
}

// wireFields projects a template's view-model onto the API shape.
func wireFields(name string) openapi.EmailTemplateFields {
	vm, ok := ViewModelFor(name)
	if !ok {
		return openapi.EmailTemplateFields{Scalars: []openapi.EmailTemplateField{}, Collections: []openapi.EmailTemplateCollection{}}
	}
	scalars := make([]openapi.EmailTemplateField, 0, len(vm.Scalars))
	for _, f := range vm.Scalars {
		scalars = append(scalars, wireField(f))
	}
	collections := make([]openapi.EmailTemplateCollection, 0, len(vm.Collections))
	for _, c := range vm.Collections {
		rowFields := make([]openapi.EmailTemplateField, 0, len(c.Fields))
		for _, f := range c.Fields {
			rowFields = append(rowFields, wireField(f))
		}
		collections = append(collections, openapi.EmailTemplateCollection{
			Name:        c.Name,
			Description: c.Description,
			Fields:      rowFields,
		})
	}
	return openapi.EmailTemplateFields{Scalars: scalars, Collections: collections}
}

func wireField(f Field) openapi.EmailTemplateField {
	return openapi.EmailTemplateField{Name: f.Name, Kind: string(f.Kind), Description: f.Description}
}

// ---------------------------------------------------------------------------
// PUT /email-templates/{template}/{part}
// ---------------------------------------------------------------------------

func (h *HTTPHandler) SetEmailTemplate(
	ctx context.Context,
	req openapi.SetEmailTemplateRequestObject,
) (openapi.SetEmailTemplateResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.SetEmailTemplate401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapConfigWrite) && !id.Can(CapSystemAdmin) {
		return openapi.SetEmailTemplate403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapConfigWrite + " capability required"},
		}, nil
	}
	if req.Body == nil {
		return openapi.SetEmailTemplate400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing body"},
		}, nil
	}
	name := strings.TrimSpace(req.Template)
	part := strings.TrimSpace(req.Part)
	if req.Body.Body == "" {
		return openapi.SetEmailTemplate400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "body is required"},
		}, nil
	}
	if len(req.Body.Body) > MaxOverrideLength {
		return openapi.SetEmailTemplate400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: fmt.Sprintf("body exceeds %d characters", MaxOverrideLength),
			},
		}, nil
	}

	userRef := id.UserRef
	row, err := h.store.Set(ctx, name, part, req.Body.Body, &userRef)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownTemplate), errors.Is(err, ErrUnknownPart):
			// A path segment names nothing — 404, not a body error.
			return openapi.SetEmailTemplate404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: err.Error()},
			}, nil
		case errors.Is(err, ErrUnknownField), errors.Is(err, ErrTemplateParse):
			// The body is well-formed JSON but a broken/over-reaching
			// template — 422 naming the field (fail-loud).
			return openapi.SetEmailTemplate422JSONResponse{
				UnprocessableEntityJSONResponse: openapi.UnprocessableEntityJSONResponse{Error: err.Error()},
			}, nil
		default:
			return nil, fmt.Errorf("email: set template: %w", err)
		}
	}

	out := openapi.EmailTemplateOverride{
		Template: row.TemplateName,
		Part:     row.Part,
		Body:     row.Body,
	}
	if row.UpdatedAt.Valid {
		t := row.UpdatedAt.Time
		out.UpdatedAt = &t
	}
	return openapi.SetEmailTemplate200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// DELETE /email-templates/{template}/{part}
// ---------------------------------------------------------------------------

func (h *HTTPHandler) DeleteEmailTemplate(
	ctx context.Context,
	req openapi.DeleteEmailTemplateRequestObject,
) (openapi.DeleteEmailTemplateResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.DeleteEmailTemplate401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	if !id.Can(CapConfigWrite) && !id.Can(CapSystemAdmin) {
		return openapi.DeleteEmailTemplate403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: CapConfigWrite + " capability required"},
		}, nil
	}
	name := strings.TrimSpace(req.Template)
	part := strings.TrimSpace(req.Part)
	if err := h.store.Delete(ctx, name, part); err != nil {
		if errors.Is(err, ErrNotFound) {
			return openapi.DeleteEmailTemplate404JSONResponse{
				NotFoundJSONResponse: openapi.NotFoundJSONResponse{
					Error: fmt.Sprintf("no override for %q / %q", name, part),
				},
			}, nil
		}
		return nil, fmt.Errorf("email: delete template: %w", err)
	}
	return openapi.DeleteEmailTemplate204Response{}, nil
}
