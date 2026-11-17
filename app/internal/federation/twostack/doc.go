// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package twostack is the two-instance federation gate (#72; reused by
// #1582). Its tests run two production servers (http.New), each on its
// OWN migrated database (AA_DB_NAME and AA_DB_NAME_B) and its OWN storage
// root, served over TLS, paired through the real handshake and driven
// through the real admin API. Nothing is shared between the instances
// except the process, the test certificate and the at-rest master key.
//
// The earlier in-package harnesses (internal/http handshake and actor
// signature tests) share one database between both instances, so they
// cannot prove anything about storage or deduplication across
// instances. This one can.
//
// With AA_DB_PASSWORD unset the tests skip, like every integration test.
// With AA_DB_PASSWORD set and AA_DB_NAME_B unset they FAIL: the gate
// scripts must create and migrate the second database.
package twostack
