// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

//go:build race

package metadata_test

// raceEnabled is true when the suite is built with -race.
//
// It exists for ONE assertion: the batch's latency budget. The race
// detector instruments every memory access and inflates wall-clock
// several-fold, so a duration measured under it is not the duration the
// contract is about. CI runs the whole suite with -race, so a budget
// enforced at its production value there is a gate that fails for a
// reason unrelated to the code — and a red X that is never anybody's
// fault trains people to ignore red.
//
// The measurement is REPORTED either way. Only the threshold moves.
const raceEnabled = true
