// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package twostack is the two-instance federation gate (#72; reused by
// #1582). Its tests run two production servers (http.New), each on its
// OWN dedicated, separately migrated database (AA_DB_NAME_A and
// AA_DB_NAME_B, never the general suite database AA_DB_NAME) and its OWN
// storage root, served over TLS, paired through the real handshake and
// driven through the real admin API. Nothing is shared between the
// instances except the process, the test certificate and the at-rest
// master key, and nothing the gate creates reaches the database the rest
// of the suite uses.
//
// The earlier in-package harnesses (internal/http handshake and actor
// signature tests) share one database between both instances, so they
// cannot prove anything about storage or deduplication across
// instances. This one can.
//
// With AA_DB_PASSWORD unset the tests skip, like every integration test.
// With AA_DB_PASSWORD set they FAIL when either dedicated name is unset,
// when the two are equal, or when either equals AA_DB_NAME: the gate
// scripts (scripts/test.sh) create and migrate both databases.
package twostack
