// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

//go:build !race

package metadata_test

// raceEnabled is false in an uninstrumented build. See its twin in
// race_enabled_test.go for why the batch's latency budget consults it.
const raceEnabled = false
