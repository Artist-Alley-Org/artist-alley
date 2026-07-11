// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package aiedit

import (
	"context"

	"github.com/mscrnt/artist-alley/app/internal/ai"
	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// Context plumbing — caller identity + source asset sensitivity
// ride the context so providers can forward them to the
// mcpdispatch gate without widening every request type. Lives in
// the aiedit package (not the provider sub-package) so the job
// handler in aiedit/jobs.go can plant them without importing the
// provider (which would create a cycle: provider already imports
// aiedit for the interface + sentinels).

type callerCtxKeyT struct{}
type sensitivityCtxKeyT struct{}

var (
	callerCtxKey      callerCtxKeyT
	sensitivityCtxKey sensitivityCtxKeyT
)

// WithCaller attaches the calling identity to ctx. The
// comfyuimcp provider (and any future ImageEditProvider) forwards
// it to mcpdispatch.InvokeOpts.Caller for the capability gate.
func WithCaller(ctx context.Context, id *auth.Identity) context.Context {
	return context.WithValue(ctx, callerCtxKey, id)
}

// WithSensitivity attaches the source asset's sensitivity tier so
// the dispatcher's privacy gate can clamp restricted/embargo bytes
// to local-only MCP servers.
func WithSensitivity(ctx context.Context, tier ai.SensitivityTier) context.Context {
	return context.WithValue(ctx, sensitivityCtxKey, tier)
}

// CallerFromContext is the provider-side accessor.
func CallerFromContext(ctx context.Context) *auth.Identity {
	v := ctx.Value(callerCtxKey)
	id, _ := v.(*auth.Identity)
	return id
}

// SensitivityFromContext is the provider-side accessor.
func SensitivityFromContext(ctx context.Context) ai.SensitivityTier {
	v := ctx.Value(sensitivityCtxKey)
	tier, _ := v.(ai.SensitivityTier)
	return tier
}
