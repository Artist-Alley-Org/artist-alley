// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #966 — POST /assets answered a bad `asset_type` with a 500 carrying
// the database's own constraint name.
//
// # What actually leaked
//
// NewStrictHandler's default response-error handler is
// `http.Error(w, err.Error(), 500)`. CreateAsset wrapped the INSERT
// failure as `fmt.Errorf("assets: insert: %w", err)`, so pgx's full
// message reached the wire:
//
//	assets: insert: ERROR: insert or update on table "assets" violates
//	foreign key constraint "assets_asset_type_fkey" (SQLSTATE 23503)
//
// One malformed request bought a table name, a column name, the shape
// of a relation and a SQLSTATE. Every deliberate refusal in this
// codebase was tightened for the opposite reason (#922, #941, #946):
// they were made indistinguishable so an endpoint could not be used to
// learn about state it should not disclose. This disclosed more by
// accident than any of them do on purpose.
//
// # Why the assertions are about the BODY
//
// The status code is the least interesting half. A test that asserted
// only `!= 500` would pass against a 400 that still pasted the pg
// message into `error`. So every case here asserts:
//
//  1. the body NAMES THE FIELD the caller sent, and
//  2. no SQL identifier appears anywhere in it — checked by scanning
//     for a list of schema-shaped substrings rather than by comparing
//     against one expected string, so a future message that reintroduces
//     the leak fails here even if it is worded differently.
//
// # Why every FK, not just asset_type
//
// `asset_type` is simply the column the stale seed script hit first.
// Three of the four foreign keys on `assets` take a client value on this
// path and all three leaked identically. Fixing one is the exact gap
// #941/#946 cost weeks over. `team_id` is the fourth and is EXCLUDED on
// purpose — it answers 404 "team not found" so the endpoint cannot be
// used to probe for teams (#953) — and that exclusion is asserted here
// too, so "we forgot it" and "we decided it" stay distinguishable.
//
// Skips without AA_DB_PASSWORD.
package assets_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// sqlShapedFragments are substrings that must never appear in a response
// body. They are the vocabulary of the SCHEMA, not of the API: a table
// name, the constraint-name suffix, the error text Postgres emits, and
// the SQLSTATE machinery around it.
//
// The API's own field names (`asset_type`, `state_id`, `file_hash`) are
// deliberately absent from this list — naming the field the caller sent
// is the requirement, not the leak. What must not escape is where that
// field landed.
var sqlShapedFragments = []string{
	"_fkey",
	"sqlstate",
	"foreign key",
	"constraint",
	"insert or update on table",
	"violates",
	"relation",
	"pgerror",
	"23503",
	`table "assets"`,
	"assets_",
}

// assertNoSQLLeak fails with the offending fragment named, so a
// regression reports WHAT leaked rather than "body mismatch".
func assertNoSQLLeak(t *testing.T, label, body string) {
	t.Helper()
	low := strings.ToLower(body)
	for _, frag := range sqlShapedFragments {
		if strings.Contains(low, frag) {
			t.Errorf("%s: response body leaks SQL detail %q\n  body: %s", label, frag, body)
		}
	}
}

// fkCreate drives the real POST /assets with one deliberately bad
// foreign-key value and returns the response.
func fkCreate(f *maFixture, userRef int64, mutate func(*openapi.AssetCreate)) openapi.CreateAssetResponseObject {
	f.t.Helper()
	body := &openapi.AssetCreate{Title: "fk-probe", AssetType: 1}
	mutate(body)
	resp, err := f.h.CreateAsset(f.identity(userRef), openapi.CreateAssetRequestObject{Body: body})
	if err != nil {
		// A non-nil error here IS the bug: it is what the strict
		// handler turns into the leaking 500. Report it as such rather
		// than as a test-harness failure.
		f.t.Fatalf("CreateAsset returned a raw error, which the strict handler renders as a 500 body verbatim: %v", err)
	}
	return resp
}

// TestCreateAsset_BadForeignKeys_Answer400WithoutSQLDetail is the core
// of #966: each client-supplied FK column, named in the body, with no
// schema vocabulary anywhere in it.
func TestCreateAsset_BadForeignKeys_Answer400WithoutSQLDetail(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("fk-owner")

	// A UUID that exists nowhere. Generated rather than hardcoded so the
	// test cannot start passing because someone seeded the literal.
	missing := openapi_types.UUID(uuid.New())

	cases := []struct {
		name string
		// field is the request field the message must name.
		field  string
		mutate func(*openapi.AssetCreate)
	}{
		{
			// The reported case: the type id the stale seed script
			// produced by sending `resource_type` and leaving
			// `asset_type` at its zero value.
			name:   "asset_type that does not exist",
			field:  "asset_type",
			mutate: func(b *openapi.AssetCreate) { b.AssetType = 999999 },
		},
		{
			name:   "asset_type zero (the omitted-field shape)",
			field:  "asset_type",
			mutate: func(b *openapi.AssetCreate) { b.AssetType = 0 },
		},
		{
			name:   "state_id that does not exist",
			field:  "state_id",
			mutate: func(b *openapi.AssetCreate) { b.StateId = &missing },
		},
		{
			name:  "file_hash with no stored object",
			field: "file_hash",
			mutate: func(b *openapi.AssetCreate) {
				// Well-formed (ValidateHash passes) and unknown, which
				// is the only way to reach the FK: a malformed hash is
				// refused earlier with its own 400.
				h := strings.Repeat("ab", 32)
				b.FileHash = &h
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := fkCreate(f, owner, tc.mutate)

			bad, ok := resp.(openapi.CreateAsset400JSONResponse)
			if !ok {
				t.Fatalf("CreateAsset returned %T, want 400 — a bad %s is caller error, not a server fault",
					resp, tc.field)
			}
			body := bad.Error
			if !strings.Contains(body, tc.field) {
				t.Errorf("400 body does not name the offending field %q: %s", tc.field, body)
			}
			assertNoSQLLeak(t, "POST /assets "+tc.name, body)
		})
	}
}

// TestCreateAsset_BadTeamStaysA404 pins the deliberate exclusion. team_id
// is the one FK on this row whose existence is itself sensitive, so it
// keeps the refusal that makes an unassignable team and a nonexistent
// one identical (#953). If a later change "makes the FK handling
// consistent" by folding team_id into the 400 map, POST /assets becomes
// a team-existence oracle and this fails.
func TestCreateAsset_BadTeamStaysA404(t *testing.T) {
	f := newMAFixture(t)
	owner := f.user("fk-team-owner")
	missing := openapi_types.UUID(uuid.New())

	resp := fkCreate(f, owner, func(b *openapi.AssetCreate) { b.TeamId = &missing })

	nf, ok := resp.(openapi.CreateAsset404JSONResponse)
	if !ok {
		t.Fatalf("CreateAsset returned %T, want 404 — a team the caller cannot assign to "+
			"and one that does not exist must be indistinguishable", resp)
	}
	if nf.Error != "team not found" {
		t.Errorf("team refusal body = %q, want %q", nf.Error, "team not found")
	}
	assertNoSQLLeak(t, "POST /assets bad team_id", nf.Error)
}
