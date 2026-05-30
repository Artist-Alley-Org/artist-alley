package workflow_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/workflow"
)

// fixture isolates a workflow Service test to a single asset created in
// a rolled-back transaction's namespace — except we can't roll back
// (workflow.Service uses its own connection from the pool), so we
// cleanup explicitly.
type fixture struct {
	pool     *pgxpool.Pool
	svc      *workflow.Service
	assetID  uuid.UUID
	teamID   uuid.UUID
	caller   *auth.Identity
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := openPool(t, pwd)

	// Caller identity. We don't go through the auth resolver; instead
	// hand-build an Identity that has whichever caps each test wants
	// (set per-test before calling Transition).
	id := &auth.Identity{
		UserRef:  900000 + int64(time.Now().UnixNano()%100000),
		Username: "wf_test_user",
	}

	// Seed: team + asset (Photo type / asset_type = 1).
	teamID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO teams (id, slug, name) VALUES ($1, $2, $3)`,
		teamID, "wf_test_team_"+teamID.String()[:8], "WF Test Team",
	); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	// Initial state for asset:1 (Photo) — 'draft'.
	initState, err := workflow.NewService(pool, slog.New(slog.NewTextHandler(io.Discard, nil))).
		InitialStateID(ctx, workflow.AssetDomain(1))
	if err != nil {
		t.Fatalf("lookup initial state: %v", err)
	}

	// Need an owner user; reuse the seeded admin if one exists,
	// otherwise insert a throwaway. We don't go through the user
	// creation API to keep this hermetic.
	var ownerRef int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username) VALUES ($1) RETURNING ref`,
		"wf_test_owner_"+uuid.NewString()[:8],
	).Scan(&ownerRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	assetID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, title, asset_type, owner_user_ref, state_id, team_id)
		VALUES ($1, 'wf-test', 1, $2, $3, $4)
	`, assetID, ownerRef, pgtype.UUID{Bytes: initState, Valid: true},
		pgtype.UUID{Bytes: teamID, Valid: true},
	); err != nil {
		t.Fatalf("seed asset: %v", err)
	}

	t.Cleanup(func() {
		cleanCtx := context.Background()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM workflow_audit WHERE resource_id = $1`, assetID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM assets WHERE id = $1`, assetID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM teams WHERE id = $1`, teamID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM "user" WHERE ref = $1`, ownerRef)
		pool.Close()
	})

	return &fixture{
		pool:    pool,
		svc:     workflow.NewService(pool, slog.New(slog.NewTextHandler(io.Discard, nil))),
		assetID: assetID,
		teamID:  teamID,
		caller:  id,
	}
}

// stateID looks up a workflow_states row by (domain, code) and returns
// its UUID.
func (f *fixture) stateID(t *testing.T, domain, code string) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id FROM workflow_states WHERE domain=$1 AND code=$2`,
		domain, code,
	).Scan(&id); err != nil {
		t.Fatalf("lookup state %s/%s: %v", domain, code, err)
	}
	return uuid.UUID(id.Bytes)
}

// TestTransition_HappyPath_DraftToReview: an authenticated caller
// holding `assets.submit` globally can move a draft asset into
// pending_review. The transition writes an audit row and updates the
// asset's state_id.
func TestTransition_HappyPath_DraftToReview(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()

	fx.caller.Capabilities = []string{"assets.submit"}

	pending := fx.stateID(t, "asset:1", "pending_review")
	if err := fx.svc.Transition(ctx, workflow.KindAsset, fx.assetID, pending, fx.caller, "submitting for review"); err != nil {
		t.Fatalf("happy path: %v", err)
	}

	var cur pgtype.UUID
	if err := fx.pool.QueryRow(ctx, `SELECT state_id FROM assets WHERE id=$1`, fx.assetID).Scan(&cur); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if uuid.UUID(cur.Bytes) != pending {
		t.Errorf("state not updated: got %s want %s", uuid.UUID(cur.Bytes), pending)
	}
	var auditCount int
	if err := fx.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_audit WHERE resource_id=$1`, fx.assetID).Scan(&auditCount); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("expected 1 audit row, got %d", auditCount)
	}
}

// TestTransition_RejectsIllegalEdge: trying to skip from draft to
// archived (no row in workflow_transitions) returns
// ErrTransitionNotAllowed, leaves state untouched, writes no audit.
func TestTransition_RejectsIllegalEdge(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()

	fx.caller.Capabilities = []string{"assets.submit", "assets.archive"}
	archived := fx.stateID(t, "asset:1", "archived")

	err := fx.svc.Transition(ctx, workflow.KindAsset, fx.assetID, archived, fx.caller, "")
	if !errors.Is(err, workflow.ErrTransitionNotAllowed) {
		t.Errorf("expected ErrTransitionNotAllowed, got %v", err)
	}
	var auditCount int
	_ = fx.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_audit WHERE resource_id=$1`, fx.assetID).Scan(&auditCount)
	if auditCount != 0 {
		t.Errorf("audit should be empty after rejected transition, got %d rows", auditCount)
	}
}

// TestTransition_MissingCapability: the (draft → pending_review)
// transition needs `assets.submit`. Without it, the call returns
// ErrInsufficientCapability and the asset stays in draft.
func TestTransition_MissingCapability(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()

	// Caller has no caps. Transition should fail at the cap gate.
	fx.caller.Capabilities = nil

	pending := fx.stateID(t, "asset:1", "pending_review")
	err := fx.svc.Transition(ctx, workflow.KindAsset, fx.assetID, pending, fx.caller, "")
	if !errors.Is(err, workflow.ErrInsufficientCapability) {
		t.Errorf("expected ErrInsufficientCapability, got %v", err)
	}
}

// TestTransition_TeamScopedCapability: pending_review → published
// requires `assets.review` AND requires_team_scope=TRUE. A caller
// holding the cap globally passes; a caller holding it only for a
// different team is rejected.
func TestTransition_TeamScopedCapability(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()

	// Walk the asset into pending_review first (uses global submit).
	fx.caller.Capabilities = []string{"assets.submit"}
	pending := fx.stateID(t, "asset:1", "pending_review")
	if err := fx.svc.Transition(ctx, workflow.KindAsset, fx.assetID, pending, fx.caller, ""); err != nil {
		t.Fatalf("setup transition: %v", err)
	}

	// Now try to publish. The asset's team_id = fx.teamID. Caller has
	// assets.review only scoped to a different team → must reject.
	otherTeam := uuid.New()
	hackIdentity(fx.caller, "assets.review", otherTeam)

	published := fx.stateID(t, "asset:1", "published")
	err := fx.svc.Transition(ctx, workflow.KindAsset, fx.assetID, published, fx.caller, "")
	if !errors.Is(err, workflow.ErrInsufficientCapability) {
		t.Errorf("expected ErrInsufficientCapability for wrong team scope, got %v", err)
	}

	// And try again with the cap scoped to the correct team → must pass.
	hackIdentity(fx.caller, "assets.review", fx.teamID)
	if err := fx.svc.Transition(ctx, workflow.KindAsset, fx.assetID, published, fx.caller, "approved"); err != nil {
		t.Errorf("expected success when scope matches, got %v", err)
	}
}

// TestTransition_ResourceNotFound: the asset ID doesn't exist.
func TestTransition_ResourceNotFound(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()

	fx.caller.Capabilities = []string{"assets.submit"}
	pending := fx.stateID(t, "asset:1", "pending_review")
	err := fx.svc.Transition(ctx, workflow.KindAsset, uuid.New(), pending, fx.caller, "")
	if !errors.Is(err, workflow.ErrResourceNotFound) {
		t.Errorf("expected ErrResourceNotFound, got %v", err)
	}
}

// hackIdentity sticks a team-scoped capability onto the Identity. We
// can't go through the resolver because that requires a real DB-side
// grant; for tests we poke the unexported scopedCaps directly via
// auth.SetIdentityScopedCap which we add in test-only helper.
func hackIdentity(id *auth.Identity, code string, team uuid.UUID) {
	auth.SetIdentityScopedCapForTest(id, code, team)
}

// ---------------------------------------------------------------------------
// Pool helper
// ---------------------------------------------------------------------------

func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// silence unused-import false positives if a sub-test gets commented out.
var _ = pgx.ErrNoRows
