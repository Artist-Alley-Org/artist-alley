// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"fmt"
)

// KeyAIEdit — system_config key for the AI image-edit settings
// (Phase 1.14.E-1). Read/written via the Store's GetAIEdit /
// SetAIEdit methods.
//
// Lives separately from KeyAI even though both are "AI-related" —
// KeyAI describes inference providers (OpenAI / Claude / Ollama keys
// + model + base URL), KeyAIEdit picks which operator-registered
// MCP server handles image-edit ops. Different audiences, different
// lifecycles.
const KeyAIEdit = "aiedit"

// AIEditConfig is the full image-edit settings payload stored under
// KeyAIEdit. E-1 ships one field; E-2 adds the per-op overrides
// (different MCP servers for img2img vs inpaint vs variations) when
// the typed surface expands.
type AIEditConfig struct {
	// ImageEditServer is the name of an operator-registered MCP
	// server (see /admin/ai/mcp-clients) the aiedit dispatcher
	// routes image-edit calls to. Must match a row in
	// mcp_server_registration.name exactly (case-sensitive).
	//
	// Empty value disables the feature — the HTTP handler returns
	// 409 / ErrServerNotConfigured and the viewer hides the
	// Generate variation button.
	ImageEditServer string `json:"image_edit_server"`
}

// GetAIEdit returns the image-edit config or, if unset, an empty
// AIEditConfig. Empty config means the feature is disabled.
func (s *Store) GetAIEdit(ctx context.Context) (AIEditConfig, error) {
	var out AIEditConfig
	if err := s.getKey(ctx, KeyAIEdit, &out); err != nil {
		return AIEditConfig{}, err
	}
	return out, nil
}

// SetAIEdit validates and writes the image-edit config.
//
// Validation is minimal in E-1 (just length-limits to keep the
// JSON column reasonable); E-2 will cross-check the server name
// against the live mcp_server_registration table to catch typos
// at write time. The runtime path already returns a clean
// ErrServerNotConfigured when an empty / stale name reaches the
// dispatcher, so the immediate consequence of a typo is "the
// feature stops working" rather than a silent bad state.
func (s *Store) SetAIEdit(ctx context.Context, v AIEditConfig) error {
	if len(v.ImageEditServer) > 200 {
		return fmt.Errorf("sysconfig: aiedit.image_edit_server: too long (max 200)")
	}
	return s.setKey(ctx, KeyAIEdit, v)
}
