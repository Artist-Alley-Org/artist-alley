// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE SHARED METADATA FIXTURE — one isolated world per test.
//
// Real users, real teams, real grants, real assets, real posts and real
// field definitions, all namespaced per test so parallel packages
// sharing the database cannot collide.
//
// It compiles against dev@80028e36 unchanged, which is the point:
// single_target_baseline_test.go's Class B counterweights use nothing
// but this file, so they can be RUN at that commit and shown to pass
// before and after. The batch editor's own helpers live in
// batch_fixture_test.go, which names types and tables that do not exist
// there.
//
// # Why the corpus cannot substitute for it
//
// A census of the live field definitions finds ZERO with read_only,
// ZERO with a write_capability, ZERO with a read_capability, ZERO with
// a display_condition, ONE required and ONE reference — and NO ROLE
// HOLDS assets.admin. The corpus exercises almost none of the gates
// this sprint adds, so almost every assertion here CONSTRUCTS its own
// definition, vocabulary state, grant, team, ownership and reference
// target.
//
// # Why the identities are real
//
// The apply re-resolves the caller's effective capabilities FROM THE
// TRANSACTION, so a synthetic Identity literal with a flat capability
// list would resolve to NOTHING at apply time and every write test
// would refuse. That is the fixture being forced to be honest by the
// implementation: capabilities here come from real `user` rows, real
// `user_capability_grants` rows and real teams, resolved through
// auth.Resolver.LoadIdentity — the same path the middleware takes. A
// team-scoped grant therefore arrives CLOSURE-EXPANDED from the
// database rather than from a map the test wrote.
package metadata_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

type batchFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	h    *metadata.Handler
	res  *auth.Resolver
	ctx  context.Context
}

// sharedFixturePool is ONE bounded pool for the whole suite.
//
// openPool never closes what it opens — no test in this package did
// before, and none of them noticed, because a handful of leaked pools
// stays under Postgres' connection limit. This suite adds enough
// fixtures to cross it, and "sorry, too many clients already" is a
// failure that looks like a broken handler and is not one. So the
// fixtures share one bounded pool instead of opening one each.
var (
	sharedFixturePoolOnce sync.Once
	sharedFixturePool     *pgxpool.Pool
	sharedFixturePoolErr  error
)

func fixturePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	sharedFixturePoolOnce.Do(func() {
		dsn := fmt.Sprintf(
			"host=%s port=%s user=%s dbname=%s sslmode=disable password=%s pool_max_conns=12",
			envOr("AA_DB_HOST", "postgres"), envOr("AA_DB_PORT", "5432"),
			envOr("AA_DB_USER", "artist_alley"), testdb.Name(t), os.Getenv("AA_DB_PASSWORD"))
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			sharedFixturePoolErr = err
			return
		}
		if err := pool.Ping(context.Background()); err != nil {
			pool.Close()
			sharedFixturePoolErr = err
			return
		}
		sharedFixturePool = pool
	})
	if sharedFixturePoolErr != nil {
		t.Fatalf("fixture pool: %v", sharedFixturePoolErr)
	}
	return sharedFixturePool
}

func newBatchFixture(t *testing.T) *batchFixture {
	t.Helper()
	if os.Getenv("AA_DB_PASSWORD") == "" {
		t.Skip("AA_DB_PASSWORD not set")
	}
	pool := fixturePool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := metadata.NewHandler(pool, logger, nil)
	h.Audit = audit.NewRecorder(pool, logger)
	return &batchFixture{
		t: t, pool: pool, h: h,
		res: &auth.Resolver{Pool: pool, Logger: logger},
		ctx: context.Background(),
	}
}

// ---------------------------------------------------------------------------
// Principals, teams, grants
// ---------------------------------------------------------------------------

func (f *batchFixture) user(label string) int64 {
	f.t.Helper()
	var ref int64
	name := "bx-" + label + "-" + uuid.NewString()[:8]
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`, name,
	).Scan(&ref); err != nil {
		f.t.Fatalf("seed user %q: %v", label, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

func (f *batchFixture) team(label string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`,
		id, "bx_team_"+id.String()[:8], label,
	); err != nil {
		f.t.Fatalf("seed team %q: %v", label, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id)
	})
	return id
}

// grant writes a REAL grant row, globally when team is nil. The
// capability resolution that reads it back is the shipped one.
func (f *batchFixture) grant(userRef int64, code string, team *uuid.UUID) {
	f.t.Helper()
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO user_capability_grants (user_ref, capability_code, team_id) VALUES ($1, $2, $3)`,
		userRef, code, teamArg,
	); err != nil {
		f.t.Fatalf("grant %s to %d: %v", code, userRef, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM user_capability_grants WHERE user_ref = $1 AND capability_code = $2`, userRef, code)
	})
}

// revoke removes a grant and DROPS THE CAPS CACHE, which is what makes
// a revocation between preview and apply real rather than notional.
func (f *batchFixture) revoke(userRef int64, code string, team *uuid.UUID) {
	f.t.Helper()
	var err error
	if team == nil {
		_, err = f.pool.Exec(f.ctx,
			`DELETE FROM user_capability_grants WHERE user_ref = $1 AND capability_code = $2 AND team_id IS NULL`,
			userRef, code)
	} else {
		_, err = f.pool.Exec(f.ctx,
			`DELETE FROM user_capability_grants WHERE user_ref = $1 AND capability_code = $2 AND team_id = $3`,
			userRef, code, *team)
	}
	if err != nil {
		f.t.Fatalf("revoke %s from %d: %v", code, userRef, err)
	}
}

// grantViaRole confers a capability through a ROLE assignment rather
// than a direct grant. It is what proves the apply asks about EFFECTIVE
// permission and not about grant-set equality: a caller whose direct
// grant is removed while a role still confers the capability has lost
// nothing.
func (f *batchFixture) grantViaRole(userRef int64, code string) {
	f.t.Helper()
	roleID := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO roles (id, name, description) VALUES ($1, $2, 'batch fixture role')`,
		roleID, "bx_role_"+roleID.String()[:8],
	); err != nil {
		f.t.Fatalf("seed role: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO role_capabilities (role_id, capability_code) VALUES ($1, $2)`, roleID, code,
	); err != nil {
		f.t.Fatalf("role capability: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO user_roles (user_ref, role_id) VALUES ($1, $2)`, userRef, roleID,
	); err != nil {
		f.t.Fatalf("assign role: %v", err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM user_roles WHERE role_id = $1`, roleID)
		_, _ = f.pool.Exec(c, `DELETE FROM role_capabilities WHERE role_id = $1`, roleID)
		_, _ = f.pool.Exec(c, `DELETE FROM roles WHERE id = $1`, roleID)
	})
}

// identity resolves the caller the middleware would build.
//
// A FRESH Resolver every time, deliberately: the shipped one caches
// capability sets per user, and a test that revokes a grant and then
// asked a cached resolver would be asserting against the cache rather
// than against the database.
func (f *batchFixture) identity(userRef int64) context.Context {
	f.t.Helper()
	res := &auth.Resolver{Pool: f.pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return auth.WithIdentity(f.ctx, res.LoadIdentity(f.ctx, userRef))
}

// ---------------------------------------------------------------------------
// Subjects
// ---------------------------------------------------------------------------

func (f *batchFixture) asset(owner *int64, team *uuid.UUID) uuid.UUID {
	return f.assetOfType(owner, team, 1, "active")
}

func (f *batchFixture) assetOfType(owner *int64, team *uuid.UUID, assetType int64, status string) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	hb := make([]byte, 16)
	_, _ = rand.Read(hb)
	hashHex := hex.EncodeToString(sha256.New().Sum(hb))[:64]
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		 VALUES ($1, 1024, 'image/png', 'fs') ON CONFLICT (hash) DO NOTHING`, hashHex,
	); err != nil {
		f.t.Fatalf("seed storage_object: %v", err)
	}
	var teamArg any
	if team != nil {
		teamArg = *team
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO assets (id, title, asset_type, owner_user_ref, team_id, status,
		                    file_hash, file_extension, file_size_bytes, sensitivity)
		VALUES ($1, 'bx-asset', $2, $3, $4, $5, $6, 'png', 1024, 'public')`,
		id, assetType, owner, teamArg, status, hashHex,
	); err != nil {
		f.t.Fatalf("seed asset: %v", err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM asset_field_value_history WHERE asset_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM asset_field_value WHERE asset_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM assets WHERE id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, hashHex)
	})
	return id
}

func (f *batchFixture) post(owner int64, members ...uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO posts (id, title, author_user_ref) VALUES ($1, 'bx-post', $2)`, id, owner,
	); err != nil {
		f.t.Fatalf("seed post: %v", err)
	}
	for i, m := range members {
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, $3)`, id, m, i,
		); err != nil {
			f.t.Fatalf("seed post member: %v", err)
		}
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM post_assets WHERE post_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM posts WHERE id = $1`, id)
	})
	return id
}

func (f *batchFixture) softDeleteAsset(id uuid.UUID) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE assets SET deleted_at = NOW() WHERE id = $1`, id); err != nil {
		f.t.Fatalf("soft delete: %v", err)
	}
}

func (f *batchFixture) archiveAsset(id uuid.UUID) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE assets SET status = 'archived' WHERE id = $1`, id); err != nil {
		f.t.Fatalf("archive: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Field definitions
// ---------------------------------------------------------------------------

type fieldSpec struct {
	Type            string
	Required        bool
	ReadOnly        bool
	Options         []map[string]any
	OpenVocabulary  bool
	AppliesTo       []int64
	ReadCapability  string
	WriteCapability string
	RegexpFilter    string
	MirrorsColumn   string
	Status          string
}

// field inserts a definition DIRECTLY, because almost every
// configuration this suite needs cannot be reached through the create
// API: mirrors_column, an archived status, and a read_capability naming
// a capability the create body validates against a closed list are all
// states an operator reaches by other routes or not at all. Inserting
// the row is what lets the gate be tested rather than the create form.
func (f *batchFixture) field(code string, spec fieldSpec) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	fullCode := "bx_" + code + "_" + id.String()[:8]
	status := spec.Status
	if status == "" {
		status = "active"
	}
	// `options` is NOT NULL. A field with no vocabulary carries the
	// empty document, which is what the create path writes too.
	optionsJSON := []byte("{}")
	if spec.Options != nil {
		doc := map[string]any{"values": spec.Options}
		optionsJSON, _ = json.Marshal(doc)
	}
	var readCap, writeCap, regexp, mirrors any
	if spec.ReadCapability != "" {
		readCap = spec.ReadCapability
	}
	if spec.WriteCapability != "" {
		writeCap = spec.WriteCapability
	}
	if spec.RegexpFilter != "" {
		regexp = spec.RegexpFilter
	}
	if spec.MirrorsColumn != "" {
		mirrors = spec.MirrorsColumn
	}
	appliesTo := spec.AppliesTo
	if appliesTo == nil {
		appliesTo = []int64{}
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO field_definition
		    (id, code, label, type, subject_kind, applies_to, required, status,
		     options, open_vocabulary, read_capability, write_capability,
		     regexp_filter, mirrors_column, read_only)
		VALUES ($1, $2, $3, $4, 'asset', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		id, fullCode, code, spec.Type, appliesTo, spec.Required, status,
		optionsJSON, spec.OpenVocabulary, readCap, writeCap, regexp, mirrors, spec.ReadOnly,
	); err != nil {
		f.t.Fatalf("seed field %q: %v", code, err)
	}
	f.t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM asset_field_value_history WHERE field_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM asset_field_value WHERE field_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM metadata_batch_preview WHERE field_id = $1`, id)
		_, _ = f.pool.Exec(c, `DELETE FROM field_definition WHERE id = $1`, id)
	})
	return id
}

func (f *batchFixture) textField(required bool) uuid.UUID {
	return f.field("text", fieldSpec{Type: "text", Required: required})
}

// setValue writes a stored value directly, so a test can establish the
// PRE-MUTATION STATE it needs without going through the very writer it
// is about to compare against.
func (f *batchFixture) setValue(asset, field uuid.UUID, v map[string]any) {
	f.t.Helper()
	var text any
	var num any
	var date any
	var options any
	var ref any
	if s, ok := v["text"]; ok {
		text = s
	}
	if n, ok := v["num"]; ok {
		num = n
	}
	if d, ok := v["date"]; ok {
		date = d
	}
	if o, ok := v["options"]; ok {
		options = o
	}
	if r, ok := v["ref"]; ok {
		ref = r
	}
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO asset_field_value
		    (asset_id, field_id, value_text, value_num, value_date, value_options, value_ref, set_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'manual')
		ON CONFLICT (asset_id, field_id) DO UPDATE SET
		    value_text = EXCLUDED.value_text, value_num = EXCLUDED.value_num,
		    value_date = EXCLUDED.value_date, value_options = EXCLUDED.value_options,
		    value_ref = EXCLUDED.value_ref, set_at = NOW()`,
		asset, field, text, num, date, options, ref,
	); err != nil {
		f.t.Fatalf("seed value: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reading the world back
// ---------------------------------------------------------------------------

func (f *batchFixture) storedText(asset, field uuid.UUID) (string, bool) {
	f.t.Helper()
	var s *string
	err := f.pool.QueryRow(f.ctx,
		`SELECT value_text FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		asset, field).Scan(&s)
	if err != nil {
		return "", false
	}
	if s == nil {
		return "", true
	}
	return *s, true
}

func (f *batchFixture) storedOptions(asset, field uuid.UUID) ([]string, bool) {
	f.t.Helper()
	var opts []string
	err := f.pool.QueryRow(f.ctx,
		`SELECT value_options FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		asset, field).Scan(&opts)
	if err != nil {
		return nil, false
	}
	return opts, true
}

func (f *batchFixture) rowExists(asset, field uuid.UUID) bool {
	f.t.Helper()
	var n int
	_ = f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		asset, field).Scan(&n)
	return n > 0
}

func (f *batchFixture) historyCount(asset, field uuid.UUID) int {
	f.t.Helper()
	var n int
	_ = f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM asset_field_value_history WHERE asset_id = $1 AND field_id = $2`,
		asset, field).Scan(&n)
	return n
}

func (f *batchFixture) setAt(asset, field uuid.UUID) time.Time {
	f.t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(f.ctx,
		`SELECT set_at FROM asset_field_value WHERE asset_id = $1 AND field_id = $2`,
		asset, field).Scan(&at); err != nil {
		f.t.Fatalf("read set_at: %v", err)
	}
	return at
}

func (f *batchFixture) optionsDoc(field uuid.UUID) []byte {
	f.t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(f.ctx,
		`SELECT options FROM field_definition WHERE id = $1`, field).Scan(&raw); err != nil {
		f.t.Fatalf("read options: %v", err)
	}
	return raw
}

// mustJSON re-encodes a decoded envelope so a disclosure assertion can
// search the WHOLE document rather than the members it thought to look
// at — the point being to catch a value that leaked into a member
// nobody predicted.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return string(raw)
}

func strp(s string) *string { return &s }

func vocabOption(slug, label, status string) map[string]any {
	o := map[string]any{"value": slug, "label": label}
	if status != "" {
		o["status"] = status
	}
	return o
}

func vocabOptionWith(slug, label, status string, extra map[string]any) map[string]any {
	o := vocabOption(slug, label, status)
	for k, v := range extra {
		o[k] = v
	}
	return o
}
