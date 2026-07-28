// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #573 — session IPs on the admin view are gated behind users.pii.read,
// additive to the users.read that admits a caller to the list at all
// (ADR 0072). Same data class, same bar, same mechanism as audit's
// actor IPs (#425).
//
// This is the direct analogue of audit/pii_test.go and is written the
// same way on purpose: real DB, real router, assertions on the RAW
// response JSON. A mapper-level unit test cannot catch the regression
// being pinned here, because the bug shape is a HANDLER that passes
// `includeIP: true` unconditionally — the mapper is innocent either
// way. And decoding into openapi.SessionRow would erase the difference
// between "field absent" and "field present and null", which is
// precisely the distinction under test.
//
// Skips without AA_DB_PASSWORD, same convention as the other
// integration suites.

package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

const (
	capUsersRead    = "users.read"
	capUsersPIIRead = "users.pii.read"
	sessPIIUserRef  = int64(5730001)
	sessPIITestIP   = "203.0.113.73" // TEST-NET-3, never routable
	sessPIIAgent    = "sessions-pii-test/1.0"
)

func sessPIIPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + env("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedSessionWithIP plants one live session row carrying an address,
// owned by a user that exists (the FK is real).
func seedSessionWithIP(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "user" (ref, username, approved)
		VALUES ($1, 'pii_sessions_probe', 1)
		ON CONFLICT (ref) DO NOTHING`, sessPIIUserRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO sessions (id, user_ref, token_hash, ip, user_agent, expires_at)
		VALUES ($1, $2, $3, $4::inet, $5, now() + interval '1 day')`,
		id, sessPIIUserRef, []byte("pii-test-token-hash"), sessPIITestIP, sessPIIAgent); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM sessions WHERE id=$1`, id)
		_, _ = pool.Exec(bg, `DELETE FROM "user" WHERE ref=$1`, sessPIIUserRef)
	})
	return id
}

type sessionsShim struct {
	*strictservershim.PanicShim
	h *auth.Handler
}

func (s sessionsShim) ListAdminUserSessions(ctx context.Context, req openapi.ListAdminUserSessionsRequestObject) (openapi.ListAdminUserSessionsResponseObject, error) {
	return s.h.ListAdminUserSessions(ctx, req)
}

func sessPIIRouter(t *testing.T, pool *pgxpool.Pool, caps ...string) chi.Router {
	t.Helper()
	h := &auth.Handler{Pool: pool}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: 999, AuthMethod: "session", Capabilities: caps}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(
		openapi.NewStrictHandler(sessionsShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router
}

// rawSession pulls the seeded row out of the response as a RAW map, so
// the assertion sees exactly what a client sees.
func rawSession(t *testing.T, router chi.Router, want uuid.UUID) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	url := "/admin/users/" + strconv.FormatInt(sessPIIUserRef, 10) + "/sessions"
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, body=%s", url, rr.Code, rr.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, it := range body.Items {
		if s, _ := it["id"].(string); s == want.String() {
			return it
		}
	}
	t.Fatalf("seeded session %s not in the response (%d items) — the test cannot "+
		"assert redaction on a row it never received", want, len(body.Items))
	return nil
}

// TestSessionIP_WithoutPIICapability_IsAbsent is the fix. Fails on
// pre-#573 code, where ListAdminUserSessions passed includeIP=true for
// every users.read holder.
func TestSessionIP_WithoutPIICapability_IsAbsent(t *testing.T) {
	pool := sessPIIPool(t)
	id := seedSessionWithIP(t, pool)

	row := rawSession(t, sessPIIRouter(t, pool, capUsersRead), id)

	if v, present := row["ip"]; present {
		t.Errorf("a caller with %s but NOT %s received ip=%v — session IPs are the same "+
			"data class as audit actor IPs and must not ride along with plain user read",
			capUsersRead, capUsersPIIRead, v)
	}
	for k, v := range row {
		if s, ok := v.(string); ok && s == sessPIITestIP {
			t.Errorf("the IP leaked through field %q despite the capability gate", k)
		}
	}
	// Withholding the field must not gut the surface: a support role
	// still needs to label the device it is about to revoke.
	if got, _ := row["user_agent"].(string); got != sessPIIAgent {
		t.Errorf("user_agent = %q, want %q — the row must stay usable without the PII cap", got, sessPIIAgent)
	}
}

// The other half: the capability has to actually grant something, or
// the "fix" is just a removal and the admin view loses a real
// incident-response tool.
func TestSessionIP_WithPIICapability_IsReturned(t *testing.T) {
	pool := sessPIIPool(t)
	id := seedSessionWithIP(t, pool)

	row := rawSession(t, sessPIIRouter(t, pool, capUsersRead, capUsersPIIRead), id)

	if got, _ := row["ip"].(string); got != sessPIITestIP {
		t.Errorf("ip = %q, want %q — a caller holding %s must still see it",
			got, sessPIITestIP, capUsersPIIRead)
	}
}

// system.admin short-circuits Identity.Can, so an admin needs no
// explicit grant. Worth asserting: a gate implemented as an allowlist
// membership test rather than a Can() call would lock admins out.
func TestSessionIP_SystemAdminIsReturned(t *testing.T) {
	pool := sessPIIPool(t)
	id := seedSessionWithIP(t, pool)

	row := rawSession(t, sessPIIRouter(t, pool, "system.admin"), id)

	if got, _ := row["ip"].(string); got != sessPIITestIP {
		t.Errorf("system.admin got ip=%q, want %q — the wildcard must satisfy the PII cap",
			got, sessPIITestIP)
	}
}

// The capability must exist in the capabilities table, or an operator
// cannot grant it and the gate is a permanent denial. Pins the
// migration against the constant the handler checks.
func TestUsersPIIReadCapabilityIsDefined(t *testing.T) {
	pool := sessPIIPool(t)
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM capabilities WHERE code = $1`, capUsersPIIRead).Scan(&n); err != nil {
		t.Fatalf("query capabilities: %v", err)
	}
	if n != 1 {
		t.Errorf("capability %q defined %d times, want 1 — see migrations/00018", capUsersPIIRead, n)
	}
}
