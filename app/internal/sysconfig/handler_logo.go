// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Instance-logo HTTP surface (#517) — the operator-uploadable brand
// mark that replaces the shipped default icon in the app chrome, plus
// the recent-logo list that lets a previous mark be picked back up.
//
// Four endpoints, and the split between them is the security model:
//
//	POST   /admin/system/appearance/logo         — upload, gated on
//	                                               system.appearance.write
//	POST   /admin/system/appearance/logo/select  — re-activate, same gate
//	DELETE /admin/system/appearance/logo         — back to the shipped
//	                                               default, same gate
//	GET    /appearance/logo                      — public, unauthenticated
//
// The logo is NOT settable through PATCH /admin/system/appearance even
// though it lives in the same config object. That endpoint takes a
// whole AppearanceConfig from the caller; if the logo reference were
// part of that payload, an admin could point the public,
// unauthenticated GET at any object hash on the install — someone
// else's private asset included. The only ways to set a logo are to
// upload bytes this package has decoded and approved, or to re-select
// a hash that is already in the list because it went through that same
// gate earlier.
//
// The bytes themselves never touch system_config. They go through the
// content-addressed storage layer under an `appearance:logo` pin, so
// they inherit dedup, refcounting and GC, and the config row keeps
// holding nothing but short identifiers.
//
// RETENTION. Every hash in the history is pinned, and that is
// load-bearing rather than incidental: the storage layer marks an
// object GC-eligible the moment its last pin drops, so
// content-addressing alone would NOT keep a superseded logo readable.
// Pinning exactly the listed window makes "listed implies resolvable"
// an invariant the storage layer maintains for us. Eviction from the
// tail drops the pin and hands those bytes to the normal GC lifecycle
// (which has its own grace window) instead of deleting them outright.

package sysconfig

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// PinSubjectTypeAppearance / pinSubjectIDLogo form the storage pin
// that keeps the recent logos' bytes alive. The pin primary key is
// (hash, subject_type, subject_id), so one constant subject pins every
// listed hash as its own row — there is one logo list per install, so
// the subject needs no identifier of its own.
const (
	PinSubjectTypeAppearance = "appearance"
	pinSubjectIDLogo         = "logo"
)

// logoPin is the PinRef every logo retention decision uses.
func logoPin() storage.PinRef {
	return storage.PinRef{SubjectType: PinSubjectTypeAppearance, SubjectID: pinSubjectIDLogo}
}

// SetStorage wires the storage service post-construction, matching
// SetEmail / SetAuditRecorder. nil-safe: with no storage wired the
// write endpoints refuse with a 400 explaining the boot wire is
// missing, and the read endpoint reports "no logo" — a fixture that
// does not exercise logos is unaffected.
func (h *Handler) SetStorage(svc *storage.Service) { h.Storage = svc }

// UploadInstanceLogo implements POST /admin/system/appearance/logo.
func (h *Handler) UploadInstanceLogo(
	ctx context.Context,
	req openapi.UploadInstanceLogoRequestObject,
) (openapi.UploadInstanceLogoResponseObject, error) {
	id, denied := h.requireCap(ctx, CapAppearanceWrite)
	if denied != nil {
		return uploadLogoDenial(denied), nil
	}
	if h.Storage == nil {
		return logoBadRequest("storage backend is not configured on this server"), nil
	}
	if req.Body == nil {
		return logoBadRequest("missing request body"), nil
	}

	// Validate BEFORE anything is written. ValidateLogo buffers at
	// most MaxLogoBytes+1, so a hostile body cannot make us allocate
	// without bound, and nothing reaches the storage layer until the
	// bytes have been proven to be a decodable, allowlisted raster
	// image.
	buf, meta, err := ValidateLogo(req.Body)
	if err != nil {
		switch {
		case errors.Is(err, ErrLogoTooLarge),
			errors.Is(err, ErrLogoNotAnImage),
			errors.Is(err, ErrLogoDimensions):
			return logoBadRequest(err.Error()), nil
		}
		return nil, fmt.Errorf("sysconfig: validate logo: %w", err)
	}

	before, beforeErr := h.Store.GetAppearance(ctx)

	// The content type handed to storage is the DECODED one. The
	// client's declared type is not read on this path at all.
	res, err := h.Storage.UploadOriginal(ctx, bytes.NewReader(buf), meta.ContentType, logoPin())
	if err != nil {
		return nil, fmt.Errorf("sysconfig: store logo: %w", err)
	}
	meta.Hash = res.Hash

	evicted, err := h.Store.AddLogo(ctx, meta)
	if err != nil {
		return logoBadRequest(err.Error()), nil
	}

	// Release pins for entries that fell off the tail. Ordered AFTER
	// the config write on purpose: had the write failed, unpinning
	// first would have made bytes collectable that the install still
	// lists.
	for _, e := range evicted {
		h.releaseLogoPin(ctx, e.Hash)
	}

	return logoWriteResponse(ctx, h, id, before, beforeErr,
		func(cfg openapi.AppearanceConfig) openapi.UploadInstanceLogoResponseObject {
			return openapi.UploadInstanceLogo200JSONResponse(cfg)
		})
}

// SelectInstanceLogo implements
// POST /admin/system/appearance/logo/select — the recovery path.
func (h *Handler) SelectInstanceLogo(
	ctx context.Context,
	req openapi.SelectInstanceLogoRequestObject,
) (openapi.SelectInstanceLogoResponseObject, error) {
	id, denied := h.requireCap(ctx, CapAppearanceWrite)
	if denied != nil {
		return selectLogoDenial(denied), nil
	}
	if req.Body == nil || req.Body.Hash == "" {
		return openapi.SelectInstanceLogo400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: "missing logo hash"},
		}, nil
	}
	before, beforeErr := h.Store.GetAppearance(ctx)

	// Refuse an entry whose bytes are gone. Activating an unresolvable
	// logo would swap a working mark for a broken image across the
	// whole install, which is a strictly worse outcome than telling
	// the operator the file is missing.
	if entry := before.FindLogo(req.Body.Hash); entry != nil && !h.logoAvailable(ctx, entry.Hash) {
		return openapi.SelectInstanceLogo400JSONResponse{
			BadRequestJSONResponse: openapi.BadRequestJSONResponse{
				Error: "that logo's image data is no longer available on this server",
			},
		}, nil
	}

	if err := h.Store.SelectLogo(ctx, req.Body.Hash); err != nil {
		// The only expected failure is "not in the list", which is a
		// 404 rather than a 400: the caller named a resource that
		// isn't there.
		return openapi.SelectInstanceLogo404JSONResponse{
			NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: err.Error()},
		}, nil
	}

	return logoWriteResponse(ctx, h, id, before, beforeErr,
		func(cfg openapi.AppearanceConfig) openapi.SelectInstanceLogoResponseObject {
			return openapi.SelectInstanceLogo200JSONResponse(cfg)
		})
}

// DeleteInstanceLogo implements DELETE /admin/system/appearance/logo —
// revert to the shipped default mark.
//
// Idempotent: doing this on an install already showing the default is
// a success, because the caller's intent is already true.
//
// It deliberately does NOT unpin anything. The logo being deselected
// stays in the history and stays retrievable; discarding it here would
// destroy the recovery property the list exists for.
func (h *Handler) DeleteInstanceLogo(
	ctx context.Context,
	_ openapi.DeleteInstanceLogoRequestObject,
) (openapi.DeleteInstanceLogoResponseObject, error) {
	id, denied := h.requireCap(ctx, CapAppearanceWrite)
	if denied != nil {
		return deleteLogoDenial(denied), nil
	}
	before, beforeErr := h.Store.GetAppearance(ctx)
	if err := h.Store.SelectLogo(ctx, ""); err != nil {
		return nil, fmt.Errorf("sysconfig: revert to default logo: %w", err)
	}
	return logoWriteResponse(ctx, h, id, before, beforeErr,
		func(cfg openapi.AppearanceConfig) openapi.DeleteInstanceLogoResponseObject {
			return openapi.DeleteInstanceLogo200JSONResponse(cfg)
		})
}

// GetPublicInstanceLogo implements GET /appearance/logo — the
// unauthenticated read the browser makes for every page's chrome, and
// the thumbnail source for the admin picker.
//
// 404 is the ordinary state of a default install: it means "no
// operator logo, draw the shipped one".
func (h *Handler) GetPublicInstanceLogo(
	ctx context.Context,
	req openapi.GetPublicInstanceLogoRequestObject,
) (openapi.GetPublicInstanceLogoResponseObject, error) {
	cfg, err := h.Store.GetAppearance(ctx)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: get appearance: %w", err)
	}
	if h.Storage == nil {
		return logoNotFound(), nil
	}

	// Resolve which entry to serve. A caller-supplied `v` is looked up
	// in the history and NOWHERE else, so this route can only ever
	// serve one of the (at most five) images the operator uploaded
	// through the validated path. An unlisted hash is indistinguishable
	// from a nonexistent one.
	var entry *LogoConfig
	if req.Params.V != nil && *req.Params.V != "" {
		entry = cfg.FindLogo(*req.Params.V)
	} else {
		entry = cfg.ActiveLogoEntry()
	}
	if entry == nil {
		return logoNotFound(), nil
	}

	// Re-check the stored content type at serve time. It is written
	// only by the validated path above, but this header is the whole
	// reason that path is careful — a value that somehow got in by
	// another route must not reach the browser.
	if _, ok := allowedLogoMIME[entry.ContentType]; !ok {
		h.logger().LogAttrs(ctx, slog.LevelError, "sysconfig.logo.bad_content_type",
			slog.String("content_type", entry.ContentType),
			slog.String("hash", entry.Hash),
		)
		return logoNotFound(), nil
	}

	body, info, err := h.Storage.Download(ctx, entry.Hash, storage.VariantOriginal)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// The config points at bytes the backend no longer has.
			// Degrade to the shipped default rather than 500 — chrome
			// on every page must not be able to break the page. The
			// admin picker surfaces the same condition as an explicit
			// "no longer available" via the `available` flag.
			h.logger().LogAttrs(ctx, slog.LevelWarn, "sysconfig.logo.missing_object",
				slog.String("hash", entry.Hash),
			)
			return logoNotFound(), nil
		}
		return nil, fmt.Errorf("sysconfig: download logo: %w", err)
	}
	return logoImageResponse{
		Body:        body,
		Size:        info.Size,
		ContentType: entry.ContentType,
		Hash:        entry.Hash,
	}, nil
}

// --- serve path -----------------------------------------------------------

// logoImageResponse streams a logo with the headers the generated
// octet-stream response type cannot express: a per-request
// Content-Type, and the hardening this endpoint needs because it
// serves an operator-supplied file from our own origin.
//
// The generated GetPublicInstanceLogo200ApplicationoctetStreamResponse
// would send `application/octet-stream` and nothing else. That is both
// wrong (the browser must render it as an image) and unsafe, so this
// implements the response interface directly — the same escape hatch
// the auth package uses for Set-Cookie responses.
type logoImageResponse struct {
	Body        io.Reader
	Size        int64
	ContentType string
	Hash        string
}

func (r logoImageResponse) VisitGetPublicInstanceLogoResponse(w http.ResponseWriter) error {
	h := w.Header()
	// Derived from decoding the bytes at upload — never client input.
	h.Set("Content-Type", r.ContentType)
	// Defence in depth against a polyglot file: even if some future
	// validation gap let through a file a sniffing browser would read
	// as HTML, nosniff forces it to be treated as the image type we
	// declared.
	h.Set("X-Content-Type-Options", "nosniff")
	// Applies when the URL is navigated to directly rather than loaded
	// as an <img>. `default-src 'none'` plus `sandbox` means the
	// response cannot fetch, script, frame or navigate even if the
	// browser decides to treat it as a document.
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox; base-uri 'none'; form-action 'none'")
	// The URL carries the content hash as ?v=, so a given URL's bytes
	// genuinely never change and can be cached hard. Changing the logo
	// changes the URL, which is the invalidation.
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+r.Hash+`"`)
	if r.Size > 0 {
		h.Set("Content-Length", strconv.FormatInt(r.Size, 10))
	}
	w.WriteHeader(http.StatusOK)

	if closer, ok := r.Body.(io.ReadCloser); ok {
		defer closer.Close()
	}
	_, err := io.Copy(w, r.Body)
	return err
}

// --- helpers --------------------------------------------------------------

// logoWriteResponse re-reads the config, records the audit event, and
// hands the admin-shaped payload to the caller's response constructor.
// The three write endpoints differ only in their response type, so this
// keeps the post-write sequence in exactly one place.
//
// A free function rather than a method because Go has no generic
// methods; the handler is passed explicitly instead.
func logoWriteResponse[T any](
	ctx context.Context,
	h *Handler,
	id *auth.Identity,
	before AppearanceConfig,
	beforeErr error,
	mk func(openapi.AppearanceConfig) T,
) (T, error) {
	var zero T
	after, err := h.Store.GetAppearance(ctx)
	if err != nil {
		return zero, fmt.Errorf("sysconfig: reread appearance: %w", err)
	}
	h.recordLogoChange(ctx, id, before, beforeErr, after)
	return mk(h.appearanceAdminAPI(ctx, after)), nil
}

// appearanceAdminAPI is the admin-surface projection: the public
// fields plus the recent-logo list with a live availability probe per
// entry.
//
// The probe is a Stat, not a read — it costs no byte transfer, it runs
// at most MaxLogoHistory times, and it only ever runs on admin
// requests. It is what lets the picker say "this one is gone" instead
// of rendering a broken thumbnail, which is the whole point of a list
// whose stated purpose is recovering a lost image.
func (h *Handler) appearanceAdminAPI(ctx context.Context, cfg AppearanceConfig) openapi.AppearanceConfig {
	out := appearanceToAPI(cfg)
	if len(cfg.LogoHistory) == 0 {
		return out
	}
	items := make([]openapi.InstanceLogo, 0, len(cfg.LogoHistory))
	for _, l := range cfg.LogoHistory {
		items = append(items, openapi.InstanceLogo{
			Hash:        l.Hash,
			Url:         logoURL(l.Hash),
			ContentType: l.ContentType,
			Width:       l.Width,
			Height:      l.Height,
			SizeBytes:   l.SizeBytes,
			Available:   h.logoAvailable(ctx, l.Hash),
			Active:      l.Hash == cfg.ActiveLogo,
		})
	}
	out.LogoHistory = &items
	return out
}

// logoAvailable reports whether a logo's bytes can still be read.
//
// Conservative on error: anything other than a clean Stat counts as
// unavailable, because the cost of a false "available" is the broken
// thumbnail this flag exists to prevent, while the cost of a false
// "unavailable" is one entry the operator has to re-upload.
func (h *Handler) logoAvailable(ctx context.Context, hash string) bool {
	if h.Storage == nil {
		return false
	}
	_, err := h.Storage.Stat(ctx, hash, storage.VariantOriginal)
	return err == nil
}

// releaseLogoPin drops the appearance pin from a hash that has been
// evicted from the recent list. Best-effort: a failure here leaks an
// unreferenced object, which the storage sweep reports, and is not a
// reason to fail the operator's request after the config has already
// been updated.
func (h *Handler) releaseLogoPin(ctx context.Context, hash string) {
	if h.Storage == nil || hash == "" {
		return
	}
	if err := h.Storage.RemovePin(ctx, logoPin(), hash); err != nil {
		h.logger().LogAttrs(ctx, slog.LevelWarn, "sysconfig.logo.unpin_failed",
			slog.String("hash", hash),
			slog.String("err", err.Error()),
		)
	}
}

// recordLogoChange emits the same audit event a font change does — the
// logo is part of the appearance config, and an operator auditing "who
// changed how this install looks" wants one timeline, not two.
func (h *Handler) recordLogoChange(
	ctx context.Context,
	id *auth.Identity,
	before AppearanceConfig,
	beforeErr error,
	after AppearanceConfig,
) {
	if h.Audit == nil {
		return
	}
	var beforeArg any = &before
	if beforeErr != nil {
		beforeArg = (*AppearanceConfig)(nil)
	}
	actor := &id.UserRef
	h.Audit.RecordChange(ctx, auth.RequestFromContext(ctx),
		audit.EventAdminAppearanceConfigUpdated,
		nil, actor,
		beforeArg, &after, nil)
}

// logger returns the handler's logger or a discard logger, so the
// serve path never nil-panics in a fixture that skipped the wire.
func (h *Handler) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// logoURL builds the fetch URL for a logo hash. Relative to the API
// base, which is the same string in dev (Vite proxy) and prod (the Go
// binary serving the embedded frontend).
func logoURL(hash string) string {
	return "/api/v1/appearance/logo?v=" + hash
}

func logoBadRequest(msg string) openapi.UploadInstanceLogo400JSONResponse {
	return openapi.UploadInstanceLogo400JSONResponse{
		BadRequestJSONResponse: openapi.BadRequestJSONResponse{Error: msg},
	}
}

func logoNotFound() openapi.GetPublicInstanceLogo404JSONResponse {
	return openapi.GetPublicInstanceLogo404JSONResponse{
		NotFoundJSONResponse: openapi.NotFoundJSONResponse{Error: "no instance logo is set"},
	}
}

func uploadLogoDenial(err error401or403) openapi.UploadInstanceLogoResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.UploadInstanceLogo401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.UploadInstanceLogo403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

func selectLogoDenial(err error401or403) openapi.SelectInstanceLogoResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.SelectInstanceLogo401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.SelectInstanceLogo403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

func deleteLogoDenial(err error401or403) openapi.DeleteInstanceLogoResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.DeleteInstanceLogo401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.DeleteInstanceLogo403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}
