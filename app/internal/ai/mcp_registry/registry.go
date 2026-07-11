// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package mcpregistry owns the MCP server registration data — CRUD
// over mcp_server_registration + mcp_server_tool_grant, plus a cache
// layer that the dispatcher reads on every invoke.
//
// # Architecture
//
//   - Registry is the public face. Wraps sqlc Queries with cache-
//     warming + invalidation on every write. The dispatcher + admin
//     handler hold a *Registry; tests pass a registry built off a
//     pool fixture.
//
//   - Two cache domains via cache.Registry:
//       mcp.server.config   — per-server full registration row
//       mcp.server.tools    — per-server tool-grant list
//     Both invalidate cross-instance via the existing NOTIFY/LISTEN
//     pattern from 1.10.
//
//   - Sentinels: ErrServerNotFound (no such ID/name);
//     ErrDuplicateName (UNIQUE violation on insert).
//
// # No discovery
//
// Servers are operator-registered explicitly. The registry has no
// scan / mDNS / health-broadcast path — operator types the URL in
// the admin UI, AA queries the server's /tools endpoint, operator
// picks which tools to expose.

package mcpregistry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
)

// Cache domain names. The dispatcher + admin handler look these up
// via the registry's getters rather than touching cache directly.
const (
	CacheDomainServerConfig = "mcp.server.config"
	CacheDomainServerTools  = "mcp.server.tools"
)

// Common sentinels.
var (
	ErrServerNotFound = errors.New("mcpregistry: server not found")
	ErrDuplicateName  = errors.New("mcpregistry: server name already registered")
)

// Server is the operator-facing view of one registration. Maps 1:1
// from the sqlc row + strips the pgtype wrappers.
type Server struct {
	ID                   uuid.UUID
	Name                 string
	URL                  string
	Transport            string // 'http' | 'stdio'
	AuthKind             string // 'none' | 'bearer' | 'header' | 'mtls'
	AuthSecretRef        string // empty when AuthKind='none'
	AuthHeaderName       string // empty unless AuthKind='header'
	PrivacyClass         string // 'local' | 'cloud'
	Enabled              bool
	RateLimitPerSecond   int32
	RateLimitPerMinute   int32
	HealthCheckIntervalS int32
	LastHealthStatus     string // 'healthy' | 'degraded' | 'unreachable' | ""
	LastHealthError      string
	RegisteredByUserRef  *int64
}

// ToolGrant is one row from mcp_server_tool_grant.
type ToolGrant struct {
	ServerID             uuid.UUID
	ToolName             string
	AdditionalCapability string // empty = inherits server-level caps only
	CostEstimateMicros   int64  // 0 = free (local)
	Enabled              bool
}

// Registry is the public face of MCP server registration.
type Registry struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
	q      *Queries

	configs *cache.Cache[Server]
	tools   *cache.Cache[[]ToolGrant]
}

// NewRegistry wires the registry to its pool + cache. cacheReg may
// be nil (tests); when nil, every read hits the DB.
func NewRegistry(pool *pgxpool.Pool, cacheReg *cache.Registry, logger *slog.Logger) *Registry {
	r := &Registry{pool: pool, logger: logger, q: New(pool)}
	if cacheReg != nil {
		r.configs = cache.Register[Server](cacheReg, CacheDomainServerConfig, 200)
		r.tools = cache.Register[[]ToolGrant](cacheReg, CacheDomainServerTools, 200)
	}
	return r
}

// ListServers returns every registration (enabled + disabled). Used
// by the admin UI.
func (r *Registry) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := r.q.ListServers(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcpregistry.ListServers: %w", err)
	}
	out := make([]Server, 0, len(rows))
	for _, row := range rows {
		out = append(out, modelToServer(row))
	}
	return out, nil
}

// ListEnabledServers — hot path for the dispatcher + health-check
// pool. Backed by the partial index.
func (r *Registry) ListEnabledServers(ctx context.Context) ([]Server, error) {
	rows, err := r.q.ListEnabledServers(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcpregistry.ListEnabledServers: %w", err)
	}
	out := make([]Server, 0, len(rows))
	for _, row := range rows {
		out = append(out, modelToServer(row))
	}
	return out, nil
}

// GetServerByID — single-server fetch via cache.
func (r *Registry) GetServerByID(ctx context.Context, id uuid.UUID) (Server, error) {
	key := id.String()
	if r.configs != nil {
		if s, ok := r.configs.Get(key); ok {
			return s, nil
		}
	}
	row, err := r.q.GetServerByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Server{}, ErrServerNotFound
		}
		return Server{}, fmt.Errorf("mcpregistry.GetServerByID: %w", err)
	}
	s := modelToServer(row)
	if r.configs != nil {
		r.configs.Add(key, s)
	}
	return s, nil
}

// GetServerByName — operator-chosen name lookup (admin UI + audit-
// dashboard friendly).
func (r *Registry) GetServerByName(ctx context.Context, name string) (Server, error) {
	row, err := r.q.GetServerByName(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Server{}, ErrServerNotFound
		}
		return Server{}, fmt.Errorf("mcpregistry.GetServerByName: %w", err)
	}
	return modelToServer(row), nil
}

// InsertParams is the create payload.
type InsertParams struct {
	Name                 string
	URL                  string
	Transport            string
	AuthKind             string
	AuthSecretRef        string
	AuthHeaderName       string
	PrivacyClass         string
	Enabled              bool
	RateLimitPerSecond   int32
	RateLimitPerMinute   int32
	HealthCheckIntervalS int32
	RegisteredByUserRef  *int64
}

// Insert creates a new registration. ErrDuplicateName surfaces when
// the operator's chosen name collides with an existing row.
func (r *Registry) Insert(ctx context.Context, p InsertParams) (Server, error) {
	row, err := r.q.InsertServer(ctx, InsertServerParams{
		Name:                 p.Name,
		Url:                  p.URL,
		Transport:            p.Transport,
		AuthKind:             p.AuthKind,
		AuthSecretRef:        nullableNonEmpty(p.AuthSecretRef),
		AuthHeaderName:       nullableNonEmpty(p.AuthHeaderName),
		PrivacyClass:         p.PrivacyClass,
		Enabled:              p.Enabled,
		RateLimitPerSecond:   p.RateLimitPerSecond,
		RateLimitPerMinute:   p.RateLimitPerMinute,
		HealthCheckIntervalS: p.HealthCheckIntervalS,
		RegisteredByUserRef:  p.RegisteredByUserRef,
	})
	if err != nil {
		// 23505 = unique_violation. UNIQUE(name) catches the
		// duplicate-name case.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Server{}, ErrDuplicateName
		}
		return Server{}, fmt.Errorf("mcpregistry.Insert: %w", err)
	}
	s := modelToServer(row)
	r.invalidateConfig(ctx, s.ID)
	return s, nil
}

// UpdateParams uses pointer fields so the admin handler can express
// "update only this subset". nil keeps the existing value.
type UpdateParams struct {
	ID                   uuid.UUID
	URL                  *string
	Transport            *string
	AuthKind             *string
	AuthSecretRef        *string
	AuthHeaderName       *string
	PrivacyClass         *string
	Enabled              *bool
	RateLimitPerSecond   *int32
	RateLimitPerMinute   *int32
	HealthCheckIntervalS *int32
}

// Update mutates a registration.
func (r *Registry) Update(ctx context.Context, p UpdateParams) (Server, error) {
	row, err := r.q.UpdateServer(ctx, UpdateServerParams{
		ID:                   pgtype.UUID{Bytes: p.ID, Valid: true},
		Url:                  p.URL,
		Transport:            p.Transport,
		AuthKind:             p.AuthKind,
		AuthSecretRef:        p.AuthSecretRef,
		AuthHeaderName:       p.AuthHeaderName,
		PrivacyClass:         p.PrivacyClass,
		Enabled:              p.Enabled,
		RateLimitPerSecond:   p.RateLimitPerSecond,
		RateLimitPerMinute:   p.RateLimitPerMinute,
		HealthCheckIntervalS: p.HealthCheckIntervalS,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Server{}, ErrServerNotFound
		}
		return Server{}, fmt.Errorf("mcpregistry.Update: %w", err)
	}
	s := modelToServer(row)
	r.invalidateConfig(ctx, s.ID)
	return s, nil
}

// Delete removes a registration. ON DELETE CASCADE drops tool grants.
func (r *Registry) Delete(ctx context.Context, id uuid.UUID) error {
	if err := r.q.DeleteServer(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		return fmt.Errorf("mcpregistry.Delete: %w", err)
	}
	r.invalidateConfig(ctx, id)
	r.invalidateTools(ctx, id)
	return nil
}

// UpdateHealthStatus — called by the health-check goroutine after
// each poll. Invalidates the config cache so admin dashboards reflect
// the new status within one read.
func (r *Registry) UpdateHealthStatus(ctx context.Context, id uuid.UUID, status, errMsg string) error {
	err := r.q.UpdateHealthStatus(ctx, UpdateHealthStatusParams{
		ID:               pgtype.UUID{Bytes: id, Valid: true},
		LastHealthStatus: nullableNonEmpty(status),
		LastHealthError:  nullableNonEmpty(errMsg),
	})
	if err != nil {
		return fmt.Errorf("mcpregistry.UpdateHealthStatus: %w", err)
	}
	r.invalidateConfig(ctx, id)
	return nil
}

// ---------------------------------------------------------------------------
// Tool grants
// ---------------------------------------------------------------------------

// ListToolGrants reads the per-server grant list — the dispatcher's
// hot path for tool-cap + cost-estimate lookup on every invoke.
func (r *Registry) ListToolGrants(ctx context.Context, serverID uuid.UUID) ([]ToolGrant, error) {
	key := serverID.String()
	if r.tools != nil {
		if grants, ok := r.tools.Get(key); ok {
			return grants, nil
		}
	}
	rows, err := r.q.ListToolGrants(ctx, pgtype.UUID{Bytes: serverID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("mcpregistry.ListToolGrants: %w", err)
	}
	out := make([]ToolGrant, 0, len(rows))
	for _, row := range rows {
		out = append(out, toolModelToGrant(row))
	}
	if r.tools != nil {
		r.tools.Add(key, out)
	}
	return out, nil
}

// GetToolGrant — single-grant lookup; bypasses the list cache (the
// per-tool read is rare; the dispatcher iterates the whole list once
// per invoke).
func (r *Registry) GetToolGrant(ctx context.Context, serverID uuid.UUID, toolName string) (ToolGrant, error) {
	row, err := r.q.GetToolGrant(ctx, GetToolGrantParams{
		ServerID: pgtype.UUID{Bytes: serverID, Valid: true},
		ToolName: toolName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ToolGrant{}, ErrServerNotFound
		}
		return ToolGrant{}, fmt.Errorf("mcpregistry.GetToolGrant: %w", err)
	}
	return toolModelToGrant(row), nil
}

// UpsertToolGrantInput — explicit shape the admin handler builds.
// Named *Input not *Params to avoid colliding with the sqlc-generated
// UpsertToolGrantParams which has a pgtype.UUID server_id.
type UpsertToolGrantInput struct {
	ServerID             uuid.UUID
	ToolName             string
	AdditionalCapability string // empty = inherits server caps only
	CostEstimateMicros   int64
	Enabled              bool
}

// UpsertToolGrant adds or replaces one tool grant.
func (r *Registry) UpsertToolGrant(ctx context.Context, p UpsertToolGrantInput) (ToolGrant, error) {
	row, err := r.q.UpsertToolGrant(ctx, UpsertToolGrantParams{
		ServerID:             pgtype.UUID{Bytes: p.ServerID, Valid: true},
		ToolName:             p.ToolName,
		AdditionalCapability: nullableNonEmpty(p.AdditionalCapability),
		CostEstimateMicros:   p.CostEstimateMicros,
		Enabled:              p.Enabled,
	})
	if err != nil {
		return ToolGrant{}, fmt.Errorf("mcpregistry.UpsertToolGrant: %w", err)
	}
	r.invalidateTools(ctx, p.ServerID)
	return toolModelToGrant(row), nil
}

// DeleteToolGrant removes one tool grant.
func (r *Registry) DeleteToolGrant(ctx context.Context, serverID uuid.UUID, toolName string) error {
	err := r.q.DeleteToolGrant(ctx, DeleteToolGrantParams{
		ServerID: pgtype.UUID{Bytes: serverID, Valid: true},
		ToolName: toolName,
	})
	if err != nil {
		return fmt.Errorf("mcpregistry.DeleteToolGrant: %w", err)
	}
	r.invalidateTools(ctx, serverID)
	return nil
}

// ---------------------------------------------------------------------------
// Cache invalidation
// ---------------------------------------------------------------------------

func (r *Registry) invalidateConfig(ctx context.Context, id uuid.UUID) {
	if r.configs == nil {
		return
	}
	r.configs.Invalidate(ctx, id.String())
}

func (r *Registry) invalidateTools(ctx context.Context, serverID uuid.UUID) {
	if r.tools == nil {
		return
	}
	r.tools.Invalidate(ctx, serverID.String())
}

// ---------------------------------------------------------------------------
// Row → domain converters
// ---------------------------------------------------------------------------

func modelToServer(m McpServerRegistration) Server {
	out := Server{
		ID:                   uuid.UUID(m.ID.Bytes),
		Name:                 m.Name,
		URL:                  m.Url,
		Transport:            m.Transport,
		AuthKind:             m.AuthKind,
		PrivacyClass:         m.PrivacyClass,
		Enabled:              m.Enabled,
		RateLimitPerSecond:   m.RateLimitPerSecond,
		RateLimitPerMinute:   m.RateLimitPerMinute,
		HealthCheckIntervalS: m.HealthCheckIntervalS,
		RegisteredByUserRef:  m.RegisteredByUserRef,
	}
	if m.AuthSecretRef != nil {
		out.AuthSecretRef = *m.AuthSecretRef
	}
	if m.AuthHeaderName != nil {
		out.AuthHeaderName = *m.AuthHeaderName
	}
	if m.LastHealthStatus != nil {
		out.LastHealthStatus = *m.LastHealthStatus
	}
	if m.LastHealthError != nil {
		out.LastHealthError = *m.LastHealthError
	}
	return out
}

func toolModelToGrant(m McpServerToolGrant) ToolGrant {
	out := ToolGrant{
		ServerID:           uuid.UUID(m.ServerID.Bytes),
		ToolName:           m.ToolName,
		CostEstimateMicros: m.CostEstimateMicros,
		Enabled:            m.Enabled,
	}
	if m.AdditionalCapability != nil {
		out.AdditionalCapability = *m.AdditionalCapability
	}
	return out
}

// nullableNonEmpty returns *string=nil when the input is empty so
// the COALESCE / sqlc.narg path treats empty-string and nil-input
// identically.
func nullableNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
