// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

//go:build !race

package metadata_test

// raceEnabled is false in an uninstrumented build, which is the only
// build that can prove the batch's production latency acceptance. See
// its twin in race_enabled_test.go.
const raceEnabled = false
