// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package coverfocal holds the write-side rule for a cover crop's focal
// point, shared by every entity that stores one.
//
// # Why it is a package and not a helper in one handler
//
// #1207 gave collections two focal pairs and wrote one validator for
// both, with a comment saying why: "three copies of a range check is
// how one of them ends up admitting 1.5". #1210 adds the third pair, on
// posts, and that comment becomes the argument for lifting the rule out
// rather than copying it a third time.
//
// The refusals are returned as a MESSAGE rather than as an HTTP
// response, because each endpoint's 400 is its own generated type. The
// rule is shared; the envelope is not.
package coverfocal

// Validate refuses the three shapes a focal pair must never reach the
// database in. It returns the message for a 400, or "" when the pair is
// acceptable, which includes when it is absent entirely: "leave alone"
// is always valid.
//
// Each refusal is a state the column CHECK would otherwise reject as a
// constraint error, surfacing as a 500 rather than as a 400 the caller
// can act on:
//
//   - One coordinate without the other. Half a point is not a weaker
//     positioning, it is an unanswerable one, and the only way to
//     complete it is to invent an axis the author did not choose.
//   - Either coordinate alongside the clear flag. That is the
//     exclusivity rule every clear verb in this API carries, refused
//     rather than resolved because the server has no basis for
//     preferring one.
//   - Out of 0..1. A fraction outside the picture is a client bug, and
//     rejecting it here is what stops it becoming an object-position of
//     -240% on a surface nobody is looking at.
//
// The names are passed in so the message says which pair the caller got
// wrong; a post and a collection's featured slot produce different
// text from the same rule.
func Validate(xName, yName, clearName string, x, y *float64, clear bool) string {
	if (x == nil) != (y == nil) {
		return xName + " and " + yName + " must be sent together"
	}
	if clear && x != nil {
		return "send either the " + xName + "/" + yName + " pair or " + clearName + ", not both"
	}
	if x != nil && (*x < 0 || *x > 1 || *y < 0 || *y > 1) {
		return xName + " and " + yName + " must be fractions between 0 and 1"
	}
	return ""
}
