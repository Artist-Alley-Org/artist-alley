// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package casfetch

// SetHeartbeatFault makes every lease renewal consult f first; a non-nil
// error is a failed renewal. Tests only.
func SetHeartbeatFault(c *Client, f func() error) { c.hbFault = f }

// SetBeforeReusePin runs f after reuse's final re-authorisation, just
// before its pin transaction. Tests only.
func SetBeforeReusePin(c *Client, f func()) { c.beforeReusePin = f }

// SetAfterFinalCheck runs f after the import's before-commit
// re-authorisation, just before its commit. Tests only.
func SetAfterFinalCheck(c *Client, f func()) { c.afterFinalCheck = f }
