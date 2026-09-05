// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

//go:build race

package metadata_test

// raceEnabled is true when the suite is built with -race.
//
// It gates ONE thing: whether the batch's wall-clock latency
// ACCEPTANCE is asserted. The acceptance is a PRODUCTION criterion —
// p95 <= 10 s at the 1,000-target ceiling — and the race detector
// instruments every memory access, so a duration measured under it is
// not a measurement of the thing the criterion is about.
//
// ⛔ The answer is NOT a looser threshold under -race. A second number
// standing in for the acceptance is how the acceptance quietly becomes
// the second number. Under -race the CORRECTNESS half of the ceiling
// test still runs in full — every target written, the search rebuild,
// the envelope, the guard contention — and the timing is REPORTED and
// NOT ASSERTED, with the reason stated in the log line.
//
// The acceptance itself is proven by the uninstrumented run, which is
// the only run that can prove it.
const raceEnabled = true
