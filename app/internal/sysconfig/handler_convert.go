// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

// Type conversions between sysconfig Go types and the openapi-codegen
// generated wire structs, plus the 401/403 denial-response builders
// used by each handler method.
//
// Each {section}Denial(err) function returns the typed
// "Get{Section}Config..." response object for that section's GET
// path; each {section}UpdateDenial(err) does the same for the PATCH
// path. Splitting them keeps the call sites in handler.go terse.

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// Site
// ---------------------------------------------------------------------------

func siteToAPI(v Site) openapi.SiteConfig {
	out := openapi.SiteConfig{Name: v.Name}
	if v.BaseURL != "" {
		s := v.BaseURL
		out.BaseUrl = &s
	}
	return out
}

// apiToSite is the PATCH-input converter. It MERGES against the stored
// config rather than replacing it — the same COALESCE-PATCH shape as
// apiToSMTP above (#374). Without this, PATCH {base_url: "x"} wrote
// name: "" and silently un-named the site, because SiteConfig is one
// schema serving GET-response, PATCH-body, and PATCH-response at once,
// so an omitted `name` deserializes to "" and the schema's
// required:[name] can't tell omitted from empty.
//
// The two fields have different "omitted" markers, so they merge
// differently. Name is a non-pointer string, so an omitted field is
// indistinguishable from "" — and since a site cannot be nameless,
// empty means LEAVE UNCHANGED, never clear. BaseUrl is a genuine
// optional pointer, so its semantics are exact: nil (omitted) keeps the
// stored value; non-nil sets it, including to "" to deliberately clear
// it, which is valid because base_url is empty on a fresh install.
func apiToSite(v openapi.SiteConfig, before Site) Site {
	out := before
	if v.Name != "" {
		out.Name = v.Name
	}
	if v.BaseUrl != nil {
		out.BaseURL = *v.BaseUrl
	}
	return out
}

func siteConfigDenial(err error401or403) openapi.GetSiteConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.GetSiteConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.GetSiteConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

func siteConfigUpdateDenial(err error401or403) openapi.UpdateSiteConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.UpdateSiteConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.UpdateSiteConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// SMTP
// ---------------------------------------------------------------------------

// smtpToAPI is the GET-response converter — NEVER echoes the
// password back. `password_set` reports whether one is on file so
// the admin UI can render a "current secret on file" badge plus a
// "rotate" affordance.
func smtpToAPI(v SMTP) openapi.SMTPConfig {
	out := openapi.SMTPConfig{
		Host:        v.Host,
		Port:        v.Port,
		Encryption:  openapi.SMTPConfigEncryption(v.Encryption),
		FromAddress: v.FromAddr,
	}
	if v.Username != "" {
		u := v.Username
		out.Username = &u
	}
	set := v.Password != ""
	out.PasswordSet = &set
	return out
}

// apiToSMTP is the PATCH-input converter. The existing in-store
// password is merged in when the caller omits / sends empty — this
// is the standard write-only-secret pattern, so admins editing
// "host name" don't blank the password by accident.
func apiToSMTP(v openapi.SMTPConfig, current SMTP) (SMTP, error) {
	out := SMTP{
		Host:       v.Host,
		Port:       v.Port,
		Encryption: SMTPEncryption(v.Encryption),
		FromAddr:   v.FromAddress,
		Password:   current.Password,
	}
	if v.Username != nil {
		out.Username = *v.Username
	}
	if v.Password != nil && *v.Password != "" {
		out.Password = *v.Password
	}
	return out, nil
}

func smtpConfigDenial(err error401or403) openapi.GetSMTPConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.GetSMTPConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.GetSMTPConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

func smtpConfigUpdateDenial(err error401or403) openapi.UpdateSMTPConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.UpdateSMTPConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.UpdateSMTPConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func authToAPI(v AuthConfig) openapi.AuthConfig {
	out := openapi.AuthConfig{}
	out.PasswordPolicy.MinLength = v.PasswordPolicy.MinLength
	out.PasswordPolicy.RequireUpper = v.PasswordPolicy.RequireUpper
	out.PasswordPolicy.RequireNumber = v.PasswordPolicy.RequireNumber
	out.PasswordPolicy.RequireSymbol = v.PasswordPolicy.RequireSymbol
	out.PasswordPolicy.DisallowCommon = v.PasswordPolicy.DisallowCommon
	out.PasswordPolicy.MaxAgeDays = v.PasswordPolicy.MaxAgeDays

	out.SsoProviders = make([]struct {
		Config      *map[string]interface{}            `json:"config,omitempty"`
		DisplayName string                             `json:"display_name"`
		Enabled     bool                               `json:"enabled"`
		Id          *string                            `json:"id,omitempty"`
		Kind        openapi.AuthConfigSsoProvidersKind `json:"kind"`
	}, 0, len(v.SSOProviders))
	for _, p := range v.SSOProviders {
		entry := struct {
			Config      *map[string]interface{}            `json:"config,omitempty"`
			DisplayName string                             `json:"display_name"`
			Enabled     bool                               `json:"enabled"`
			Id          *string                            `json:"id,omitempty"`
			Kind        openapi.AuthConfigSsoProvidersKind `json:"kind"`
		}{
			DisplayName: p.DisplayName,
			Enabled:     p.Enabled,
			Kind:        openapi.AuthConfigSsoProvidersKind(p.Kind),
		}
		if p.ID != "" {
			id := p.ID
			entry.Id = &id
		}
		if p.Config != nil {
			cfg := map[string]interface{}(p.Config)
			entry.Config = &cfg
		}
		out.SsoProviders = append(out.SsoProviders, entry)
	}
	return out
}

func apiToAuth(v openapi.AuthConfig) AuthConfig {
	out := AuthConfig{
		PasswordPolicy: PasswordPolicy{
			MinLength:      v.PasswordPolicy.MinLength,
			RequireUpper:   v.PasswordPolicy.RequireUpper,
			RequireNumber:  v.PasswordPolicy.RequireNumber,
			RequireSymbol:  v.PasswordPolicy.RequireSymbol,
			DisallowCommon: v.PasswordPolicy.DisallowCommon,
			MaxAgeDays:     v.PasswordPolicy.MaxAgeDays,
		},
	}
	out.SSOProviders = make([]SSOProvider, 0, len(v.SsoProviders))
	for _, p := range v.SsoProviders {
		entry := SSOProvider{
			Kind:        SSOProviderKind(p.Kind),
			Enabled:     p.Enabled,
			DisplayName: p.DisplayName,
		}
		if p.Id != nil && *p.Id != "" {
			entry.ID = *p.Id
		} else {
			// Server-side ID generation for fresh entries — the admin
			// UI sends `id: ""` (or omits it) on new providers.
			entry.ID = uuid.NewString()
		}
		if p.Config != nil {
			entry.Config = *p.Config
		}
		out.SSOProviders = append(out.SSOProviders, entry)
	}
	return out
}

func authConfigDenial(err error401or403) openapi.GetAuthConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.GetAuthConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.GetAuthConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

func authConfigUpdateDenial(err error401or403) openapi.UpdateAuthConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.UpdateAuthConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.UpdateAuthConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// AI
// ---------------------------------------------------------------------------

func aiToAPI(v AIConfig) openapi.AIConfig {
	out := openapi.AIConfig{}
	if v.DefaultProviderID != "" {
		s := v.DefaultProviderID
		out.DefaultProviderId = &s
	}
	out.Providers = make([]struct {
		ApiKey      *string                       `json:"api_key,omitempty"`
		BaseUrl     *string                       `json:"base_url,omitempty"`
		Config      *map[string]interface{}       `json:"config,omitempty"`
		DisplayName string                        `json:"display_name"`
		Enabled     bool                          `json:"enabled"`
		Id          *string                       `json:"id,omitempty"`
		Kind        openapi.AIConfigProvidersKind `json:"kind"`
		Model       *string                       `json:"model,omitempty"`
	}, 0, len(v.Providers))
	for _, p := range v.Providers {
		entry := struct {
			ApiKey      *string                       `json:"api_key,omitempty"`
			BaseUrl     *string                       `json:"base_url,omitempty"`
			Config      *map[string]interface{}       `json:"config,omitempty"`
			DisplayName string                        `json:"display_name"`
			Enabled     bool                          `json:"enabled"`
			Id          *string                       `json:"id,omitempty"`
			Kind        openapi.AIConfigProvidersKind `json:"kind"`
			Model       *string                       `json:"model,omitempty"`
		}{
			DisplayName: p.DisplayName,
			Enabled:     p.Enabled,
			Kind:        openapi.AIConfigProvidersKind(p.Kind),
		}
		if p.ID != "" {
			id := p.ID
			entry.Id = &id
		}
		if p.Model != "" {
			m := p.Model
			entry.Model = &m
		}
		if p.BaseURL != "" {
			b := p.BaseURL
			entry.BaseUrl = &b
		}
		if p.APIKey != "" {
			k := p.APIKey
			entry.ApiKey = &k
		}
		if p.Config != nil {
			cfg := map[string]interface{}(p.Config)
			entry.Config = &cfg
		}
		out.Providers = append(out.Providers, entry)
	}
	return out
}

func apiToAI(v openapi.AIConfig) AIConfig {
	out := AIConfig{}
	if v.DefaultProviderId != nil {
		out.DefaultProviderID = *v.DefaultProviderId
	}
	out.Providers = make([]AIProvider, 0, len(v.Providers))
	for _, p := range v.Providers {
		entry := AIProvider{
			Kind:        AIProviderKind(p.Kind),
			Enabled:     p.Enabled,
			DisplayName: p.DisplayName,
		}
		if p.Id != nil && *p.Id != "" {
			entry.ID = *p.Id
		} else {
			entry.ID = uuid.NewString()
		}
		if p.Model != nil {
			entry.Model = *p.Model
		}
		if p.BaseUrl != nil {
			entry.BaseURL = *p.BaseUrl
		}
		if p.ApiKey != nil {
			entry.APIKey = *p.ApiKey
		}
		if p.Config != nil {
			entry.Config = *p.Config
		}
		out.Providers = append(out.Providers, entry)
	}
	return out
}

func aiConfigDenial(err error401or403) openapi.GetAIConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.GetAIConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.GetAIConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

func aiConfigUpdateDenial(err error401or403) openapi.UpdateAIConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.UpdateAIConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.UpdateAIConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// AI image-edit (Phase 1.14.E-1)
// ---------------------------------------------------------------------------

func aiEditToAPI(v AIEditConfig) openapi.AIEditConfig {
	out := openapi.AIEditConfig{}
	if v.ImageEditServer != "" {
		s := v.ImageEditServer
		out.ImageEditServer = &s
	}
	return out
}

func apiToAIEdit(v openapi.AIEditConfig) AIEditConfig {
	out := AIEditConfig{}
	if v.ImageEditServer != nil {
		out.ImageEditServer = *v.ImageEditServer
	}
	return out
}

func aiEditConfigDenial(err error401or403) openapi.GetAIEditConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.GetAIEditConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.GetAIEditConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

func aiEditConfigUpdateDenial(err error401or403) openapi.UpdateAIEditConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.UpdateAIEditConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.UpdateAIEditConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Appearance
// ---------------------------------------------------------------------------

func appearanceToAPI(v AppearanceConfig) openapi.AppearanceConfig {
	out := openapi.AppearanceConfig{}
	if v.BrandFont != "" {
		s := v.BrandFont
		out.BrandFont = &s
	}
	if v.DisplayFont != "" {
		s := v.DisplayFont
		out.DisplayFont = &s
	}
	if v.BodyFont != "" {
		s := v.BodyFont
		out.BodyFont = &s
	}
	if v.MonoFont != "" {
		s := v.MonoFont
		out.MonoFont = &s
	}
	return out
}

func apiToAppearance(v openapi.AppearanceConfig) AppearanceConfig {
	out := AppearanceConfig{}
	if v.BrandFont != nil {
		out.BrandFont = *v.BrandFont
	}
	if v.DisplayFont != nil {
		out.DisplayFont = *v.DisplayFont
	}
	if v.BodyFont != nil {
		out.BodyFont = *v.BodyFont
	}
	if v.MonoFont != nil {
		out.MonoFont = *v.MonoFont
	}
	return out
}

func appearanceConfigDenial(err error401or403) openapi.GetAppearanceConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.GetAppearanceConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.GetAppearanceConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

func appearanceConfigUpdateDenial(err error401or403) openapi.UpdateAppearanceConfigResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.UpdateAppearanceConfig401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.UpdateAppearanceConfig403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

// fmt usage marker so the import sticks even if we later remove the
// only fmt call (currently in handler.go).
var _ = fmt.Sprintf
