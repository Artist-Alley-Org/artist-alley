// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sysconfig

import (
	"context"
	"fmt"
	"log/slog"
)

// KeyMatureContent — system_config key for the install-wide mature
// content switch (#1115, epic #1114, ADR 0090).
//
// # The name, checked against the taxonomy rather than assumed
//
// The issue proposed `content.mature_allowed`. This table carries BOTH
// naming styles and they are not interchangeable: dotted scalar keys
// (`ai.enabled`, `jobs.type_concurrency.preview.gif`) are individual
// values written by code that reads them one at a time, while the
// TYPED sections this package exposes are single top-level keys holding
// a JSONB struct — `site`, `smtp`, `appearance`, `auth`, `browse_views`.
// This is the second kind: it has an admin surface, it will grow at
// least one more field (#1116's per-instance default), and every other
// member of that set is a bare noun. So `mature_content`, not
// `content.mature_allowed`.
//
// # ABSENT MEANS ALLOWED
//
// Same argument as [KeyBrowseViews], and it matters more here. An
// install that predates this feature has consented to nothing, and the
// only safe reading of "no row" is the behaviour it already has —
// which is an instance with no mature machinery at all, i.e. one where
// nothing is filtered. Storing nothing gives exactly that.
//
// The reading that seems safer — absent means DISALLOWED — is the one
// that breaks an upgrade: it would make every existing install refuse
// mature publication on the day it upgraded, for a flag none of their
// assets carry, and the operator would have to find and flip a setting
// to restore behaviour they never changed. Failing closed is right for
// a gate over CONTENT; this is a gate over a FEATURE.
//
// Note what the default does NOT do. Allowing mature content on an
// unconfigured install shows nothing to anybody: the viewer still has
// to be signed in and opted in, and the opt-in defaults OFF (ADR 0090
// §2). The permissive default here is permissive about the operator's
// intent, not about a reader's.
const KeyMatureContent = "mature_content"

// MatureContentConfig is the payload stored under [KeyMatureContent].
//
// `Disallowed` rather than `Allowed`, and the inversion is deliberate:
// the zero value of this struct has to mean the unconfigured default,
// and a `bool` zero-values to false. Naming the field for the
// PERMISSIVE direction would make the zero value "not allowed" and put
// the upgrade hazard above back — via the one path nobody tests, which
// is a decode of an empty or partial blob.
//
// This is the same contract `FeedFilters` documents for the user-side
// booleans: the zero value is the build's default, whatever the field
// happens to be called.
type MatureContentConfig struct {
	// Disallowed switches the whole feature off for this install.
	//
	// With it set: nobody qualifies for mature content except an
	// asset's owner and a system admin (ADR 0090 §2), and publishing a
	// mature flag is REFUSED rather than silently accepted — an
	// accepted-but-inert write is how a library fills up with flags
	// nothing enforces.
	//
	// It does not clear anything. Flags already set survive, so turning
	// the switch back on restores the library exactly. The switch
	// governs enforcement and publication, never storage.
	Disallowed bool `json:"disallowed,omitempty"`
}

// Allowed is the question every caller actually asks, so it is the one
// this type answers. Nobody outside this file should read `Disallowed`
// — the inversion exists for the zero value's sake and is not a thing
// call sites should have to remember.
func (c MatureContentConfig) Allowed() bool { return !c.Disallowed }

// GetMatureContent reads the install's mature-content switch. An absent
// key resolves to the zero value, i.e. ALLOWED — see the key's doc.
func (s *Store) GetMatureContent(ctx context.Context) (MatureContentConfig, error) {
	var cfg MatureContentConfig
	if err := s.getKey(ctx, KeyMatureContent, &cfg); err != nil {
		return MatureContentConfig{}, fmt.Errorf("sysconfig: get %s: %w", KeyMatureContent, err)
	}
	return cfg, nil
}

// SetMatureContent writes the switch.
//
// ⚠️ THE READ IS NOT CACHED, DELIBERATELY, and this is the one place to
// record why — because "add a cache" is the obvious next edit and it is
// the direction that leaks. Every caller of [Store.GetMatureContent]
// reads it inside a request that is already doing DB work, and a stale
// TRUE would keep serving mature content on an install whose operator
// has just switched it off. ADR 0013's rule (a mutation invalidates the
// domain that SERVES the value it changed) would apply the moment a
// cache appears, and the invalidation would have to be wired HERE.
// Until then there is nothing to go stale.
func (s *Store) SetMatureContent(ctx context.Context, cfg MatureContentConfig, log *slog.Logger) error {
	if err := s.setKey(ctx, KeyMatureContent, cfg); err != nil {
		return fmt.Errorf("sysconfig: set %s: %w", KeyMatureContent, err)
	}
	if log != nil {
		log.Info("sysconfig: mature content switch updated", "allowed", cfg.Allowed())
	}
	return nil
}
