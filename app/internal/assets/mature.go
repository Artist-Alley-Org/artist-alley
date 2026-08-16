// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import (
	"context"
	"errors"
)

// errMatureNotAllowed is the refusal the two write paths share.
//
// One sentence, one place: an operator who has switched the feature off
// and an uploader who ticked a box are having the same conversation on
// two endpoints, and two hand-written messages is how they stop saying
// the same thing.
var errMatureNotAllowed = errors.New(
	"this instance does not allow mature content; the mature flag cannot be set")

// matureWriteAllowed decides whether a write may SET the mature flag
// (#1115, ADR 0090 §5).
//
// # Refused loudly, never accepted-and-ignored
//
// With the instance switch off, a request that asks for `mature: true`
// gets a 400. The alternative — store it and stop enforcing it, or drop
// it silently — is how a library fills up with flags nothing enforces
// and nobody can see: the uploader has no way to learn their label did
// not take, and the operator who switches the feature back on inherits
// a corpus whose labels they cannot trust.
//
// # Clearing is ALWAYS allowed
//
// `mature: false` is accepted on every instance, including one that
// disallows the feature, and the asymmetry is deliberate twice over:
//
//   - an operator who has just switched the feature off must still be
//     able to UNMARK what was marked while it was on — refusing that
//     would strand the flags where they are, which is the opposite of
//     what switching it off was for;
//   - every client that sends the whole object sends `mature: false` on
//     every ordinary save. Refusing a write that asserts nothing would
//     break the edit form for assets nobody has ever labelled.
//
// # Nil-safe, and it fails OPEN
//
// A nil SysConfig is the test/boot-order case, and it resolves to
// ALLOWED — matching [sysconfig.KeyMatureContent]'s own default, where
// an unconfigured install permits the feature. That is the right
// direction here because this gate governs a LABEL, not access: failing
// closed on a missing config would make an install refuse writes it has
// never been told to refuse. Nothing is disclosed either way — the
// viewing rules live in visibility.MatureItemVisible and consult the
// same switch independently.
//
// A config READ ERROR is different from a missing config and is
// propagated: "we could not ask" must not silently become "yes".
func (h *Handler) matureWriteAllowed(ctx context.Context, want bool) (bool, error) {
	if !want {
		return true, nil
	}
	if h.SysConfig == nil {
		return true, nil
	}
	cfg, err := h.SysConfig.GetMatureContent(ctx)
	if err != nil {
		return false, err
	}
	return cfg.Allowed(), nil
}
