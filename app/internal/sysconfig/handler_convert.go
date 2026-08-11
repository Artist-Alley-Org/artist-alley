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
	"strings"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// Optional-field helpers
// ---------------------------------------------------------------------------
//
// The generated wire structs use pointers for every optional field.
// opt* omits a zero value rather than sending it — "omitted, not
// blanked", ADR 0072's convention — while ptrBool always emits, because
// a false boolean is a real answer (`*_set: false` means "no secret on
// file", which the UI has to be able to tell from "field missing").
// deref* is the inbound direction: a missing field reads as the zero.

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optStrs(v []string) *[]string {
	if len(v) == 0 {
		return nil
	}
	return &v
}

func optInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func ptrBool(b bool) *bool { return &b }

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefStrs(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

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

// authToAPI is the GET/PATCH-response converter. It NEVER echoes an
// SSO provider's stored secrets — the OAuth client secret, the LDAP
// bind password, the SAML SP private key. Each reports a `*_set`
// boolean instead, exactly as aiToAPI does for api_key (#711) and
// smtpToAPI for the SMTP password.
//
// Why no capability unlocks them (#718): GET gates on
// `system.config.read` while setting one needs `system.auth.write`, so
// returning them made the narrower write cap protect nothing. A stored
// credential has no read-back workflow — you set it, you never need it
// back — so this is ADR 0072's field-level "omitted, not blanked"
// without 0072's read capability. `system.admin` included.
//
// The non-secret half of Config still comes back in full; that is what
// keeps editing a provider from being a retype-everything, and it is
// why the fix is a typed schema rather than dropping `config`.
func authToAPI(v AuthConfig) openapi.AuthConfig {
	out := openapi.AuthConfig{}
	out.PasswordPolicy.MinLength = v.PasswordPolicy.MinLength
	out.PasswordPolicy.RequireUpper = v.PasswordPolicy.RequireUpper
	out.PasswordPolicy.RequireNumber = v.PasswordPolicy.RequireNumber
	out.PasswordPolicy.RequireSymbol = v.PasswordPolicy.RequireSymbol
	out.PasswordPolicy.DisallowCommon = v.PasswordPolicy.DisallowCommon
	out.PasswordPolicy.MaxAgeDays = v.PasswordPolicy.MaxAgeDays

	// Sized-and-indexed rather than appended so the anonymous element
	// type oapi-codegen emits for sso_providers[] is spelled once —
	// see the same note on aiToAPI.
	out.SsoProviders = make([]struct {
		Config      *openapi.SSOProviderConfig         `json:"config,omitempty"`
		DisplayName string                             `json:"display_name"`
		Enabled     bool                               `json:"enabled"`
		Id          *string                            `json:"id,omitempty"`
		Kind        openapi.AuthConfigSsoProvidersKind `json:"kind"`
	}, len(v.SSOProviders))
	for i, p := range v.SSOProviders {
		entry := &out.SsoProviders[i]
		entry.DisplayName = p.DisplayName
		entry.Enabled = p.Enabled
		entry.Kind = openapi.AuthConfigSsoProvidersKind(p.Kind)
		if p.ID != "" {
			id := p.ID
			entry.Id = &id
		}
		entry.Config = ssoConfigToAPI(p.Config)
	}

	// #712. Both converters used to drop self_registration on the
	// floor: the admin auth page has had the three controls since
	// 1.19.C and posts them, but the read never returned them and the
	// write never stored them — so the checkbox reverted on every
	// reload and `auth.self_registration.enabled` could only ever be
	// set by writing system_config directly. That made /register
	// unreachable by construction, which is the other half of the bug.
	out.SelfRegistration = &struct {
		DefaultRole              *string `json:"default_role,omitempty"`
		Enabled                  *bool   `json:"enabled,omitempty"`
		RequireEmailVerification *bool   `json:"require_email_verification,omitempty"`
	}{
		DefaultRole:              &v.SelfRegistration.DefaultRole,
		Enabled:                  &v.SelfRegistration.Enabled,
		RequireEmailVerification: &v.SelfRegistration.RequireEmailVerification,
	}
	return out
}

// apiToAuth is the PATCH-input converter. Per-provider secrets are
// merged in from the stored config, matched on provider ID, whenever
// the caller omits or sends an empty value — the same write-only-secret
// merge apiToAI and apiToSMTP do.
//
// Without it, this endpoint's fix would be a data-loss bug rather than
// a security fix. The admin page round-trips the provider list through
// one PATCH; now that the read no longer returns the secrets, EVERY
// ordinary save arrives without them. If absent meant "clear it", the
// first display-name edit would destroy every stored credential —
// #708's shape, a font PATCH that cleared the logo.
//
// A provider arriving without an ID is new, so there is nothing to
// merge; it keeps whatever the body carried (possibly nothing).
func apiToAuth(v openapi.AuthConfig, current AuthConfig) AuthConfig {
	stored := make(map[string]SSOProviderConfig, len(current.SSOProviders))
	for _, p := range current.SSOProviders {
		if p.ID != "" {
			stored[p.ID] = p.Config
		}
	}

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
		entry.Config = apiToSSOConfig(p.Config, stored[entry.ID])
		out.SSOProviders = append(out.SSOProviders, entry)
	}

	// #712 — see authToAPI. Full-replace, like every other field on
	// this endpoint: an omitted block reads as the zero value, which
	// is "signups closed". Failing closed is the right direction for a
	// knob that opens an install to strangers.
	if sr := v.SelfRegistration; sr != nil {
		if sr.Enabled != nil {
			out.SelfRegistration.Enabled = *sr.Enabled
		}
		if sr.RequireEmailVerification != nil {
			out.SelfRegistration.RequireEmailVerification = *sr.RequireEmailVerification
		}
		if sr.DefaultRole != nil {
			out.SelfRegistration.DefaultRole = strings.TrimSpace(*sr.DefaultRole)
		}
	}
	return out
}

// ssoConfigToAPI is the outbound half of the #718 fix. The three
// secret fields stay nil — omitted, never blanked — and each reports a
// `*_set` boolean so the admin UI can render "configured" plus a
// rotate affordance without ever holding the value.
func ssoConfigToAPI(c SSOProviderConfig) *openapi.SSOProviderConfig {
	return &openapi.SSOProviderConfig{
		// OAuth 2.0 / OIDC. ClientSecret deliberately absent.
		ClientId:        optStr(c.ClientID),
		ClientSecretSet: ptrBool(c.ClientSecret != ""),
		RedirectUri:     optStr(c.RedirectURI),
		Scopes:          optStrs(c.Scopes),

		// LDAP. BindPassword deliberately absent; BindDN is an
		// identifier and comes back.
		ServerUrl:        optStr(c.ServerURL),
		StartTls:         ptrBool(c.StartTLS),
		BaseDn:           optStr(c.BaseDN),
		BindDn:           optStr(c.BindDN),
		BindPasswordSet:  ptrBool(c.BindPassword != ""),
		UserSearchFilter: optStr(c.UserSearchFilter),

		// SAML. SPPrivateKey deliberately absent; the IdP certificate
		// is public material and comes back.
		IdpMetadataUrl:  optStr(c.IDPMetadataURL),
		IdpEntityId:     optStr(c.IDPEntityID),
		IdpCertificate:  optStr(c.IDPCertificate),
		SpEntityId:      optStr(c.SPEntityID),
		SpAcsUrl:        optStr(c.SPACSURL),
		SpPrivateKeySet: ptrBool(c.SPPrivateKey != ""),
	}
}

// apiToSSOConfig is the inbound half. Non-secret fields are a straight
// replace, matching the rest of this endpoint. The three secrets start
// from `stored` and are only overwritten when the caller actually sent
// one — an omitted or empty value means "keep", not "clear".
//
// An entirely absent `config` block therefore still keeps the stored
// secrets. That is not leniency: the UI cannot send them back (it never
// receives them), so "absent" carries no intent about them either way,
// and the only safe reading is "unchanged".
func apiToSSOConfig(in *openapi.SSOProviderConfig, stored SSOProviderConfig) SSOProviderConfig {
	out := SSOProviderConfig{
		ClientSecret: stored.ClientSecret,
		BindPassword: stored.BindPassword,
		SPPrivateKey: stored.SPPrivateKey,
	}
	if in == nil {
		return out
	}

	out.ClientID = derefStr(in.ClientId)
	out.RedirectURI = derefStr(in.RedirectUri)
	out.Scopes = derefStrs(in.Scopes)

	out.ServerURL = derefStr(in.ServerUrl)
	out.StartTLS = derefBool(in.StartTls)
	out.BaseDN = derefStr(in.BaseDn)
	out.BindDN = derefStr(in.BindDn)
	out.UserSearchFilter = derefStr(in.UserSearchFilter)

	out.IDPMetadataURL = derefStr(in.IdpMetadataUrl)
	out.IDPEntityID = derefStr(in.IdpEntityId)
	out.IDPCertificate = derefStr(in.IdpCertificate)
	out.SPEntityID = derefStr(in.SpEntityId)
	out.SPACSURL = derefStr(in.SpAcsUrl)

	// `*_set` on input is readOnly and ignored — only a real value
	// moves a secret.
	if in.ClientSecret != nil && *in.ClientSecret != "" {
		out.ClientSecret = *in.ClientSecret
	}
	if in.BindPassword != nil && *in.BindPassword != "" {
		out.BindPassword = *in.BindPassword
	}
	if in.SpPrivateKey != nil && *in.SpPrivateKey != "" {
		out.SPPrivateKey = *in.SpPrivateKey
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

// aiToAPI is the GET/PATCH-response converter — NEVER echoes a
// provider's stored API key back. `api_key_set` reports whether one
// is on file so the admin UI can render "key on file" plus a rotate
// affordance, exactly as smtpToAPI does for the SMTP password.
//
// Why no capability unlocks the key (#711): GET gates on
// `system.config.read` while setting a key needs `system.ai.write`,
// so returning the key made the narrower write cap protect nothing.
// ADR 0072 grants raw IPs to a dedicated `<area>.pii.read` because
// an IP has a legitimate read-back workflow (abuse investigation);
// a stored credential has none — you set it, you never need it
// returned — so this takes 0072's field-level "omitted, not blanked"
// mechanism without its capability. `system.admin` included.
func aiToAPI(v AIConfig) openapi.AIConfig {
	out := openapi.AIConfig{}
	if v.DefaultProviderID != "" {
		s := v.DefaultProviderID
		out.DefaultProviderId = &s
	}
	// The element type is the anonymous struct oapi-codegen emits for
	// AIConfig.providers[]. Sized-and-indexed rather than appended so
	// that shape is spelled once — a codegen field addition otherwise
	// has to be mirrored in two literals.
	out.Providers = make([]struct {
		ApiKey      *string                       `json:"api_key,omitempty"`
		ApiKeySet   *bool                         `json:"api_key_set,omitempty"`
		BaseUrl     *string                       `json:"base_url,omitempty"`
		Config      *openapi.AIProviderConfig     `json:"config,omitempty"`
		DisplayName string                        `json:"display_name"`
		Enabled     bool                          `json:"enabled"`
		Id          *string                       `json:"id,omitempty"`
		Kind        openapi.AIConfigProvidersKind `json:"kind"`
		Model       *string                       `json:"model,omitempty"`
	}, len(v.Providers))
	for i, p := range v.Providers {
		entry := &out.Providers[i]
		entry.DisplayName = p.DisplayName
		entry.Enabled = p.Enabled
		entry.Kind = openapi.AIConfigProvidersKind(p.Kind)
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
		// entry.ApiKey stays nil — omitted, never blanked.
		set := p.APIKey != ""
		entry.ApiKeySet = &set
		entry.Config = aiConfigToAPI(p.Config)
	}
	return out
}

// aiConfigToAPI has no redaction to do, and that is the point (#718).
// `config` used to be a free-form map returned verbatim to every
// `system.config.read` holder — the same defect as the SSO one, sitting
// beside the api_key that #711 had already fixed. Typing it closed
// means there is no field here that can hold a credential, so the whole
// block is safe to return.
func aiConfigToAPI(c AIProviderConfig) *openapi.AIProviderConfig {
	out := &openapi.AIProviderConfig{
		MaxOutputTokens:       optInt(c.MaxOutputTokens),
		SystemPrompt:          optStr(c.SystemPrompt),
		RequestTimeoutSeconds: optInt(c.RequestTimeoutSeconds),
		RateLimitRpm:          optInt(c.RateLimitRPM),
	}
	// Copied, not aliased — the caller owns the stored struct.
	if c.Temperature != nil {
		t := *c.Temperature
		out.Temperature = &t
	}
	if c.TopP != nil {
		p := *c.TopP
		out.TopP = &p
	}
	return out
}

// apiToAIConfig is a straight replace: nothing in this block is a
// secret, so there is nothing to merge.
func apiToAIConfig(in *openapi.AIProviderConfig) AIProviderConfig {
	if in == nil {
		return AIProviderConfig{}
	}
	out := AIProviderConfig{
		MaxOutputTokens:       derefInt(in.MaxOutputTokens),
		SystemPrompt:          derefStr(in.SystemPrompt),
		RequestTimeoutSeconds: derefInt(in.RequestTimeoutSeconds),
		RateLimitRPM:          derefInt(in.RateLimitRpm),
	}
	if in.Temperature != nil {
		t := *in.Temperature
		out.Temperature = &t
	}
	if in.TopP != nil {
		p := *in.TopP
		out.TopP = &p
	}
	return out
}

// apiToAI is the PATCH-input converter. Keys are merged in from the
// stored config, matched on provider ID, whenever the caller omits or
// sends an empty `api_key` — the same write-only-secret merge
// apiToSMTP does. Without it, saving an unrelated field (the whole
// provider list round-trips through one PATCH) would wipe every key,
// which is the #708 shape: a font PATCH that cleared the logo.
//
// A provider arriving without an ID is new, so there is nothing to
// merge; it keeps whatever key the body carried (possibly none).
func apiToAI(v openapi.AIConfig, current AIConfig) AIConfig {
	stored := make(map[string]string, len(current.Providers))
	for _, p := range current.Providers {
		if p.ID != "" {
			stored[p.ID] = p.APIKey
		}
	}

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
			entry.APIKey = stored[entry.ID]
		} else {
			entry.ID = uuid.NewString()
		}
		if p.Model != nil {
			entry.Model = *p.Model
		}
		if p.BaseUrl != nil {
			entry.BaseURL = *p.BaseUrl
		}
		if p.ApiKey != nil && *p.ApiKey != "" {
			entry.APIKey = *p.ApiKey
		}
		entry.Config = apiToAIConfig(p.Config)
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
	// The active logo, as a ready-to-render URL. Absent when the
	// install is on the shipped default — absent means "draw the
	// bundled mark", the same contract an empty font slot has.
	//
	// logo_history is deliberately NOT populated here: this converter
	// feeds the public boot payload too, and that path must neither
	// publish the operator's older marks nor pay for an availability
	// probe per entry. The admin surface adds it via
	// appearanceAdminAPI.
	if logo := v.ActiveLogoEntry(); logo != nil {
		u := logoURL(logo.Hash)
		out.LogoUrl = &u
		w, hgt := logo.Width, logo.Height
		out.LogoWidth = &w
		out.LogoHeight = &hgt
	}
	return out
}

// apiToAppearance reads the CLIENT-WRITABLE fields only.
//
// Every logo field is read-only on this path by design. The logo is a
// reference to stored bytes, and letting a caller name that reference
// directly would aim the public, unauthenticated logo route at any
// object on the install. Logos are set only by the upload and select
// endpoints, which resolve to bytes this package validated itself.
//
// Because of that, callers of this function MUST carry the logo
// forward from the existing config — see UpdateAppearanceConfig. A
// PATCH is a whole-object replace, so a font change that dropped the
// logo fields on the floor would silently reset the operator's brand.
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

func publicModeDenial(err error401or403) openapi.GetPublicModeResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.GetPublicMode401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.GetPublicMode403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

func publicModeUpdateDenial(err error401or403) openapi.UpdatePublicModeResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.UpdatePublicMode401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.UpdatePublicMode403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Browse views (#709)
// ---------------------------------------------------------------------------

// browseViewsToAPI projects the stored config onto the wire shape,
// always through Resolved() so every response carries a non-empty set
// in canonical order — including the unconfigured case, where the
// stored value is nil and the answer is all five.
func browseViewsToAPI(cfg BrowseViewsConfig) openapi.BrowseViewsConfig {
	modes := cfg.Resolved()
	out := openapi.BrowseViewsConfig{
		Enabled: make([]openapi.BrowseViewsConfigEnabled, 0, len(modes)),
	}
	for _, m := range modes {
		out.Enabled = append(out.Enabled, openapi.BrowseViewsConfigEnabled(m))
	}
	return out
}

// browseViewsFromAPI converts the request body WITHOUT repairing it.
//
// Deliberately no filtering here: an unknown mode has to survive as far
// as SetBrowseViews so that validator can refuse the write. Dropping it
// on the way in would turn a typo into a silently smaller enabled set,
// and a body of nothing but typos into an empty one that then fails
// open to all five — accepted, inert, and disagreeing with what the
// operator saved.
func browseViewsFromAPI(in openapi.BrowseViewsConfig) BrowseViewsConfig {
	out := BrowseViewsConfig{Enabled: make([]BrowseViewMode, 0, len(in.Enabled))}
	for _, m := range in.Enabled {
		out.Enabled = append(out.Enabled, BrowseViewMode(m))
	}
	return out
}

func browseViewsDenial(err error401or403) openapi.GetBrowseViewsResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.GetBrowseViews401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.GetBrowseViews403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}

func browseViewsUpdateDenial(err error401or403) openapi.UpdateBrowseViewsResponseObject {
	switch e := err.(type) {
	case errUnauthenticated:
		return openapi.UpdateBrowseViews401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}
	case errForbidden:
		return openapi.UpdateBrowseViews403JSONResponse{
			ForbiddenJSONResponse: openapi.ForbiddenJSONResponse{Error: "missing capability: " + e.Cap},
		}
	}
	return nil
}
