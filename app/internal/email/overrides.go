// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package email

// Operator-authored email template overrides — #795, ADR 0081 §2 (as
// amended 2026-07-31).
//
// An operator may replace any of a template's three faces (subject /
// text / html) with their own wording. Overrides are stored per row
// keyed (template_name, part), resolved at Render over the shipped
// registry, and cached wholesale with cross-instance pg_notify
// invalidation — the same posture site_text shipped in #857.
//
// Two rules, both from the ADR:
//
//   - MISSING → SHIPPED. No row for a (template, part) renders the
//     binary-embedded template, mirroring templateForVerb's own "no
//     per-verb template → notification_generic" fallback. So does an
//     override that somehow fails at send time (belt-and-braces:
//     ADR 0085's posture is that real mail still goes out).
//   - FAIL LOUD AT SAVE. A template referencing a field outside the
//     event's documented view-model (viewmodel.go) is refused at write
//     with the field named — never stored to fail silently at send.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io"
	"log/slog"
	"regexp"
	texttemplate "text/template"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// Capabilities. Reads take the config-read cap; writes the config-write
// cap — rewriting the mail an instance sends is the same kind of act as
// editing the site's SMTP config or its wording (#718, #794).
const (
	CapConfigRead  = "system.config.read"
	CapConfigWrite = "system.config.write"
	CapSystemAdmin = "system.admin"
)

// Parts — the three faces of a template. These are the legal `part`
// values on email_template and the keys of an [Overrides] sub-map.
const (
	PartSubject = "subject"
	PartText    = "text"
	PartHTML    = "html"
)

var validParts = map[string]bool{PartSubject: true, PartText: true, PartHTML: true}

// Errors. Mapped to HTTP status by the handler; the messages name the
// offending template / part / field so a caller hitting the endpoint
// directly (not just the admin UI) sees exactly what was wrong.
var (
	// ErrUnknownTemplate — no shipped template has this name.
	ErrUnknownTemplate = errors.New("email: unknown template")
	// ErrUnknownPart — part is not subject/text/html, or the named
	// template ships no such part.
	ErrUnknownPart = errors.New("email: unknown template part")
	// ErrTemplateParse — the override body is not a valid template.
	ErrTemplateParse = errors.New("email: template does not parse")
	// ErrUnknownField — the override references a field outside the
	// event's documented view-model. The wrapped message names it.
	ErrUnknownField = errors.New("email: template references a field that is not available for this event")
	// ErrNotFound — Delete found no override to remove.
	ErrNotFound = errors.New("email: override not found")
)

// Overrides is the resolved override set: template name → part → body.
// The shape the render path and the cache hold; small (at most a couple
// dozen entries), rebuilt wholesale on invalidation.
type Overrides map[string]map[string]string

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// CacheDomain is this package's cache-domain identifier, unique across
// the process-wide [cache.Registry]. Dotted to match convention.
const CacheDomain = "email.template_overrides"

// cacheKeyAll is the single key the whole override map lives under —
// one entry, rebuilt wholesale, exactly like site_text's.
const cacheKeyAll = "all"

// TemplateCache is the email-override domain's slice of the process
// cache.
type TemplateCache struct {
	m      *cache.Cache[Overrides]
	logger *slog.Logger
}

// NewCache registers the override map with the process registry. Size 2
// because the domain holds exactly one entry.
func NewCache(registry *cache.Registry, logger *slog.Logger) *TemplateCache {
	return &TemplateCache{
		m:      cache.Register[Overrides](registry, CacheDomain, 2),
		logger: logger,
	}
}

// Invalidate drops the cached map locally and broadcasts a pg_notify so
// peer instances drop theirs too — what makes an operator's save
// visible on the next send WITHOUT a restart.
func (c *TemplateCache) Invalidate(ctx context.Context) {
	if err := c.m.Invalidate(ctx, cacheKeyAll); err != nil && c.logger != nil {
		c.logger.LogAttrs(ctx, slog.LevelWarn,
			"email.template_cache.invalidate_error",
			slog.String("err", err.Error()),
		)
	}
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// TemplateStore is the domain surface for reading + writing overrides.
// The cache may be nil (tests that skip a registry), in which case
// every resolve hits the DB.
type TemplateStore struct {
	pool   *pgxpool.Pool
	cache  *TemplateCache
	logger *slog.Logger
}

// NewTemplateStore builds the store.
func NewTemplateStore(pool *pgxpool.Pool, c *TemplateCache, logger *slog.Logger) *TemplateStore {
	return &TemplateStore{pool: pool, cache: c, logger: logger}
}

// All returns the whole override map, template → part → body. Cached
// under a single key and rebuilt wholesale on a miss. Never nil.
func (s *TemplateStore) All(ctx context.Context) (Overrides, error) {
	if s.cache != nil {
		if hit, ok := s.cache.m.Get(cacheKeyAll); ok {
			return hit, nil
		}
	}
	rows, err := New(s.pool).ListEmailTemplateOverrides(ctx)
	if err != nil {
		return nil, fmt.Errorf("email: list overrides: %w", err)
	}
	out := make(Overrides, 4)
	for _, r := range rows {
		byPart, ok := out[r.TemplateName]
		if !ok {
			byPart = make(map[string]string)
			out[r.TemplateName] = byPart
		}
		byPart[r.Part] = r.Body
	}
	if s.cache != nil {
		s.cache.m.Add(cacheKeyAll, out)
	}
	return out, nil
}

// ListRows returns every override with its metadata (updated_at, author)
// for the admin read. Straight off the DB — the admin surface is
// infrequent and wants the freshest metadata, and the cached body map
// carries no timestamps.
func (s *TemplateStore) ListRows(ctx context.Context) ([]EmailTemplate, error) {
	rows, err := New(s.pool).ListEmailTemplateOverrides(ctx)
	if err != nil {
		return nil, fmt.Errorf("email: list override rows: %w", err)
	}
	return rows, nil
}

// overridesFor returns the part → body map for one template, from the
// cache. Errors resolve to "no overrides" so a cache/DB hiccup degrades
// to shipped mail rather than no mail — the render path must never fail
// for want of an override.
func (s *TemplateStore) overridesFor(ctx context.Context, name string) map[string]string {
	all, err := s.All(ctx)
	if err != nil {
		if s.logger != nil {
			s.logger.LogAttrs(ctx, slog.LevelWarn,
				"email.template_overrides.resolve_error",
				slog.String("template", name),
				slog.String("err", err.Error()),
			)
		}
		return nil
	}
	return all[name]
}

// Set validates then upserts one override, then invalidates the cache.
//
// The validation is here, not only in the admin UI, because the UI is
// not a boundary: anyone holding system.config.write can call the
// endpoint directly, and ADR 0081 §2's fail-loud rule has to hold for
// them too.
func (s *TemplateStore) Set(ctx context.Context, name, part, body string, userRef *int64) (EmailTemplate, error) {
	if err := ValidateOverride(name, part, body); err != nil {
		return EmailTemplate{}, err
	}
	row, err := New(s.pool).UpsertEmailTemplateOverride(ctx, UpsertEmailTemplateOverrideParams{
		TemplateName:     name,
		Part:             part,
		Body:             body,
		UpdatedByUserRef: userRef,
	})
	if err != nil {
		return EmailTemplate{}, fmt.Errorf("email: upsert override %q/%q: %w", name, part, err)
	}
	s.invalidate(ctx)
	return row, nil
}

// Delete removes one override, reverting that face to what ships.
func (s *TemplateStore) Delete(ctx context.Context, name, part string) error {
	n, err := New(s.pool).DeleteEmailTemplateOverride(ctx, DeleteEmailTemplateOverrideParams{
		TemplateName: name,
		Part:         part,
	})
	if err != nil {
		return fmt.Errorf("email: delete override %q/%q: %w", name, part, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	s.invalidate(ctx)
	return nil
}

// invalidate is the one place a write announces itself. Private so no
// caller can write without invalidating.
func (s *TemplateStore) invalidate(ctx context.Context) {
	if s.cache == nil {
		return
	}
	s.cache.Invalidate(ctx)
}

// ---------------------------------------------------------------------------
// Package-global resolver, installed at boot
// ---------------------------------------------------------------------------

// templateStore is the override source Render consults. Nil until boot
// wires one (see api.go) — and nil is the safe default: Render falls
// back to the shipped registry, so tests that never install a store
// render exactly the shipped mail.
var templateStore *TemplateStore

// UseTemplateStore installs the process-wide override source Render
// resolves against. Called once at boot; tests set + reset it.
func UseTemplateStore(s *TemplateStore) { templateStore = s }

// ---------------------------------------------------------------------------
// Validation (save-time fail-loud) + parse helpers
// ---------------------------------------------------------------------------

// missingKeyRE pulls the field name out of Go's execute-time
// missing-key error so the 422 can name it plainly.
var missingKeyRE = regexp.MustCompile(`map has no entry for key "([^"]*)"`)

// ValidateOverride enforces ADR 0081 §2's fail-loud rule at save time:
//
//  1. the template name + part must exist (a template ships all three
//     of its parts; you cannot override an html face a template does
//     not have);
//  2. the body must PARSE as a Go template in the right engine
//     (text/template for subject+text, html/template for html);
//  3. the body must EXECUTE against the event's sample context with
//     Option("missingkey=error") — the one setting that turns a
//     reference to an undocumented field from a silent empty string
//     into a hard error, which we surface as a 422 naming the field.
func ValidateOverride(name, part, body string) error {
	tpl, ok := registry[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownTemplate, name)
	}
	if !validParts[part] {
		return fmt.Errorf("%w: %q (want subject, text, or html)", ErrUnknownPart, part)
	}
	if part == PartHTML && tpl.html == nil {
		return fmt.Errorf("%w: template %q ships no html part", ErrUnknownPart, name)
	}
	sample := SampleContext(name)
	if part == PartHTML {
		t, err := htmltemplate.New(name + "." + part).Option("missingkey=error").Parse(body)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrTemplateParse, err)
		}
		if err := t.Execute(io.Discard, sample); err != nil {
			return classifyExecError(err)
		}
		return nil
	}
	t, err := texttemplate.New(name + "." + part).Option("missingkey=error").Parse(body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTemplateParse, err)
	}
	if err := t.Execute(io.Discard, sample); err != nil {
		return classifyExecError(err)
	}
	return nil
}

// classifyExecError turns an execute-time error into ErrUnknownField
// naming the field when it is a missing-key error, and passes anything
// else through as a parse-class error (a genuinely broken template that
// parsed but blows up on a real value, e.g. calling a nonexistent
// function on a field).
func classifyExecError(err error) error {
	if m := missingKeyRE.FindStringSubmatch(err.Error()); m != nil {
		return fmt.Errorf("%w: %q", ErrUnknownField, m[1])
	}
	return fmt.Errorf("%w: %v", ErrTemplateParse, err)
}

// ---------------------------------------------------------------------------
// Render helpers (send-time resolution + fallback)
// ---------------------------------------------------------------------------

// renderTextPart renders one text face, preferring the operator
// override and falling back to the shipped template if the override is
// absent, unparseable, or errors at execute time. Send-time uses the
// DEFAULT missingkey behaviour (empty string, not error): a live send
// must not fail because one field went missing — that is what save-time
// validation is for.
func renderTextPart(name, part, override string, shipped *texttemplate.Template, data map[string]any, logger *slog.Logger) (string, error) {
	if override != "" {
		t, err := texttemplate.New(name + "." + part).Parse(override)
		if err == nil {
			var buf bytes.Buffer
			if err := t.Execute(&buf, data); err == nil {
				return buf.String(), nil
			} else {
				logOverrideFallback(logger, name, part, err)
			}
		} else {
			logOverrideFallback(logger, name, part, err)
		}
	}
	var buf bytes.Buffer
	if err := shipped.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderHTMLPart is renderTextPart's html/template twin — the override,
// when used, is parsed with html/template so interpolations stay
// contextually escaped (the injection defense the ADR keeps).
func renderHTMLPart(name, override string, shipped *htmltemplate.Template, data map[string]any, logger *slog.Logger) (string, error) {
	if override != "" {
		t, err := htmltemplate.New(name + "." + PartHTML).Parse(override)
		if err == nil {
			var buf bytes.Buffer
			if err := t.Execute(&buf, data); err == nil {
				return buf.String(), nil
			} else {
				logOverrideFallback(logger, name, PartHTML, err)
			}
		} else {
			logOverrideFallback(logger, name, PartHTML, err)
		}
	}
	var buf bytes.Buffer
	if err := shipped.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func logOverrideFallback(logger *slog.Logger, name, part string, err error) {
	if logger == nil {
		return
	}
	logger.LogAttrs(context.Background(), slog.LevelWarn,
		"email.template_override.fell_back_to_shipped",
		slog.String("template", name),
		slog.String("part", part),
		slog.String("err", err.Error()),
	)
}
