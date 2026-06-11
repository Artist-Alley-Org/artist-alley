// Package bootstrap runs at server startup, AFTER migrations
// + the at-rest crypto init but BEFORE the HTTP server. Its
// only job: ensure exactly one local admin exists on a fresh
// DB so operators can log in without going through the
// /setup interactive flow.
//
// # Two modes
//
//   - **Default (secure)**: generate a random 32-character
//     password, hash + persist, write the plaintext to
//     `/var/lib/artist-alley/bootstrap-admin.txt` (mode 0600
//     when possible), AND emit the plaintext to the boot log
//     so operators in a containerised deployment without a
//     writable volume can still recover it.
//   - **Demo (AA_BOOTSTRAP_DEFAULT_ADMIN=1)**: use the
//     documented literal `ArtistAlleyMogul`. Loud multi-line
//     WARN banner in the log marks the deployment as dev-mode.
//
// # Idempotent
//
// Bootstrap is skipped entirely when `CountSystemAdmins() > 0`
// — re-running a server that already has admins is a no-op.
// This makes the bootstrap safe across migrations + restarts.
//
// # Last-admin invariant
//
// The last-admin invariant (refuse to deactivate / demote /
// delete the last user holding system.admin) is enforced
// SEPARATELY in the user-management handlers + helpers. This
// package only handles first-boot creation.

package bootstrap

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation/userkeys"
)

// Config bundles the inputs Run needs. Boot wires these from
// the main config.Config.
type Config struct {
	// ScrambleKey is the legacy-compatible password-scramble
	// secret per auth/password.go. Required.
	ScrambleKey string

	// AdminPath is where the bootstrap-admin.txt file goes in
	// secure mode. Default `/var/lib/artist-alley` when empty.
	// Skipped on write-failure (logged + bootstrap continues
	// — the password is also in the boot log).
	AdminPath string

	// DefaultAdminEnabled mirrors AA_BOOTSTRAP_DEFAULT_ADMIN=1.
	// When true: use the documented literal password +
	// WARN banner. When false (default): random 32-char
	// password.
	DefaultAdminEnabled bool
}

// DefaultPassword is the documented dev-mode literal — keep
// in sync with seed/SEED_INSTRUCTIONS.md + any
// docker-compose.demo.yml that sets the env flag. Long enough
// to satisfy the min-length policy without operators typing
// it more than once.
const DefaultPassword = "ArtistAlleyMogul"

// DefaultUsername is the documented bootstrap admin username.
const DefaultUsername = "admin"

// DefaultEmail is the documented bootstrap admin email. Not a
// reachable address — the bootstrap admin is a local account;
// password-reset flows require operators to set a real email.
const DefaultEmail = "admin@localhost"

// adminRoleName mirrors setup.adminRoleName — the "Admin" role
// seeded by migration 00002.
const adminRoleName = "Admin"

// Run executes the bootstrap check + creates the admin if
// needed. Idempotent — safe to call on every startup.
//
// Returns nil on success (including skipped + no-op). Returns
// an error only when the bootstrap was BLOCKED by an
// unexpected DB error (a misconfigured ScrambleKey, missing
// "Admin" role, etc.) — startup should fail loud in those
// cases.
// Run executes the first-boot bootstrap. The audit recorder is
// optional — pass nil to skip the federation.user.key_generated
// audit row (boot paths that don't have one wired up still get a
// keypair, just no audit event). Other audit surfaces in this
// package remain slog-only.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config, logger *slog.Logger, recorder *audit.Recorder) error {
	q := auth.New(pool)
	count, err := q.CountSystemAdmins(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: count admins: %w", err)
	}
	if count > 0 {
		// Already bootstrapped on a prior boot OR setup-flow-
		// completed by an operator. No-op.
		if logger != nil {
			logger.LogAttrs(ctx, slog.LevelDebug, "bootstrap.skipped",
				slog.Int64("existing_admins", count),
			)
		}
		return nil
	}

	password, mode := pickPassword(cfg)
	hash, err := auth.HashPassword(password, cfg.ScrambleKey)
	if err != nil {
		return fmt.Errorf("bootstrap: hash password: %w", err)
	}

	// Single tx for user + role assignment. Matches the
	// setup-flow's transactional invariant (either both land
	// or neither does).
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("bootstrap: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := auth.New(tx)

	// Re-check inside the tx so two parallel boots can't both
	// race to create admin. Postgres serialisable enough at the
	// "user" table layer; the redundant check is belt-and-braces.
	if c2, err := qtx.CountSystemAdmins(ctx); err != nil {
		return fmt.Errorf("bootstrap: recheck: %w", err)
	} else if c2 > 0 {
		// Lost the race — another process bootstrapped. Roll
		// back + treat as success.
		return nil
	}

	username := DefaultUsername
	email := DefaultEmail
	usergroup := int64(3) // legacy "Super Admin" group seeded by migration 00002
	fullname := "Bootstrap Admin"

	// Recover when the `admin` user already exists but has no admin
	// role — common after a test run that DELETEs `user_roles` for
	// cleanup. The previous behaviour FATAL-exited here, which put
	// the container in a restart loop. Re-assigning the Admin role
	// to the existing user IS the recovery action the operator would
	// have taken manually, so just do it.
	var adminRef int64
	existing, err := qtx.FindUserByUsername(ctx, &username)
	switch {
	case err == nil:
		adminRef = existing.Ref
	case errors.Is(err, pgx.ErrNoRows):
		userRow, err := qtx.CreateUser(ctx, auth.CreateUserParams{
			Username:  &username,
			Password:  &hash,
			Fullname:  &fullname,
			Email:     &email,
			Usergroup: &usergroup,
		})
		if err != nil {
			return fmt.Errorf("bootstrap: create user: %w", err)
		}
		adminRef = userRow.Ref
	default:
		return fmt.Errorf("bootstrap: lookup admin user: %w", err)
	}

	role, err := qtx.FindRoleByName(ctx, adminRoleName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("bootstrap: %q role not found (was migration 00002 applied?)", adminRoleName)
		}
		return fmt.Errorf("bootstrap: lookup admin role: %w", err)
	}
	if err := qtx.SetUserGlobalRole(ctx, auth.SetUserGlobalRoleParams{
		UserRef: adminRef,
		RoleID:  role.ID,
		// AssignedByUserRef nil — bootstrap has no actor.
	}); err != nil {
		return fmt.Errorf("bootstrap: assign admin role: %w", err)
	}

	// Federation keypair (Phase 1.22.I-b). Idempotent: backfills
	// the existing-admin branch and mints for the freshly-created
	// branch. Lives in the same tx so a user with no keypair never
	// commits — federation encryption (I-e/I-f) assumes every
	// user has exactly one current key, so half-creation is worse
	// than no creation.
	ukq := userkeys.New(tx)
	alreadyHadKey, err := userkeys.EnsureCurrentForUser(ctx, ukq, adminRef)
	if err != nil {
		return fmt.Errorf("bootstrap: ensure federation user key: %w", err)
	}
	if !alreadyHadKey && recorder != nil {
		// Bootstrap is server-initiated; no human actor. Pass the
		// same tx so the audit row commits atomically with the
		// key insert (write-ahead-audit invariant per shares 1.22.C).
		recorder.FederationUserKeyGenerated(ctx, audit.New(tx), adminRef, nil, 1, userkeys.Algorithm)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("bootstrap: commit: %w", err)
	}

	announce(ctx, logger, mode, password, cfg.AdminPath)
	return nil
}

type passwordMode int

const (
	modeRandom passwordMode = iota
	modeDefaultDemo
)

func pickPassword(cfg Config) (string, passwordMode) {
	if cfg.DefaultAdminEnabled {
		return DefaultPassword, modeDefaultDemo
	}
	return randomPassword(), modeRandom
}

// randomPassword generates a 32-char URL-safe password. ~190
// bits of entropy — well above any reasonable brute-force
// budget for an offline hash + above the 8-char policy minimum
// by enough that operators can paste it into a vault without
// rotating immediately.
func randomPassword() string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	// 'i','l','I','O','0','1' excluded — operators reading from
	// a terminal log shouldn't have to disambiguate confusing
	// glyphs.
	out := make([]byte, 32)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			// crypto/rand should never fail in practice; if it
			// does we'd rather crash than emit a weak password.
			panic(fmt.Sprintf("bootstrap: rand: %v", err))
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out)
}

func announce(ctx context.Context, logger *slog.Logger, mode passwordMode, password, adminPath string) {
	if logger == nil {
		return
	}
	switch mode {
	case modeDefaultDemo:
		// Loud multi-line WARN banner so operators see it on
		// any container-log surface. Three INFO lines because
		// some log aggregators truncate at one line.
		logger.LogAttrs(ctx, slog.LevelWarn,
			"================================================================")
		logger.LogAttrs(ctx, slog.LevelWarn,
			"AA_BOOTSTRAP_DEFAULT_ADMIN=1 — DEVELOPMENT MODE — default admin "+
				"created with the PUBLISHED default password")
		logger.LogAttrs(ctx, slog.LevelWarn,
			"  username: "+DefaultUsername+
				"   email: "+DefaultEmail+
				"   password: "+DefaultPassword)
		logger.LogAttrs(ctx, slog.LevelWarn,
			"PRODUCTION DEPLOYMENTS MUST NOT SET THIS FLAG. Rotate the "+
				"password immediately if this is anything but a demo box.")
		logger.LogAttrs(ctx, slog.LevelWarn,
			"================================================================")
		return
	}
	// modeRandom — secure default.
	target := writeBootstrapFile(adminPath, password, logger)
	logger.LogAttrs(ctx, slog.LevelWarn,
		"================================================================")
	logger.LogAttrs(ctx, slog.LevelWarn,
		"FIRST-BOOT bootstrap: default admin created with a random password")
	logger.LogAttrs(ctx, slog.LevelWarn,
		"  username: "+DefaultUsername+
			"   email: "+DefaultEmail+
			"   password: "+password)
	if target != "" {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"  Password also written to "+target+" (chmod 0600)")
	}
	logger.LogAttrs(ctx, slog.LevelWarn,
		"Rotate this credential after first login; it is the LAST place "+
			"the plaintext exists on this host.")
	logger.LogAttrs(ctx, slog.LevelWarn,
		"================================================================")
}

// writeBootstrapFile attempts to write the plaintext password to
// <AdminPath>/bootstrap-admin.txt at mode 0600. Returns the path
// on success or "" on failure (boot continues — the password is
// also in the log).
func writeBootstrapFile(adminPath, password string, logger *slog.Logger) string {
	dir := adminPath
	if dir == "" {
		dir = "/var/lib/artist-alley"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"bootstrap.admin_file.mkdir.failed",
			slog.String("path", dir),
			slog.String("err", err.Error()))
		return ""
	}
	target := filepath.Join(dir, "bootstrap-admin.txt")
	body := strings.Join([]string{
		"# artist-alley first-boot admin credentials",
		"# Generated by the bootstrap package; rotate after first login.",
		"# DELETE THIS FILE after recording the password in your secrets store.",
		"",
		"username: " + DefaultUsername,
		"email:    " + DefaultEmail,
		"password: " + password,
		"",
	}, "\n")
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		logger.LogAttrs(context.Background(), slog.LevelWarn,
			"bootstrap.admin_file.write.failed",
			slog.String("path", target),
			slog.String("err", err.Error()))
		return ""
	}
	return target
}
