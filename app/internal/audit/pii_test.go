// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #425 — actor IPs are gated behind system.audit.pii.read.
//
// These assert on the API RESPONSE BODY, not on the UI and not on the
// mapper in isolation. The exposure being fixed was "the JSON contains
// an IP", so the test has to look at the JSON: a mapper-level unit test
// would pass even if a handler re-attached the field afterwards, and a
// UI test would pass while the data was still on the wire — which is
// exactly the mistake the demo made, where the column was merely not
// rendered.
//
// Skips without AA_DB_PASSWORD, same convention as the other
// integration suites.

package audit_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

const (
	capAuditRead    = "system.audit.read"
	capAuditPIIRead = "system.audit.pii.read"
	piiActorRef     = int64(4250001)
	piiTestIP       = "203.0.113.42" // TEST-NET-3, never routable
)

func piiPool(t *testing.T) *pgxpool.Pool {
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

// seedEventWithIP plants one audit row carrying an IP.
func seedEventWithIP(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO audit_events (id, event_type, actor_user_ref, ip, user_agent, metadata)
		VALUES ($1, 'login.succeeded', $2, $3::inet, $4, '{}'::jsonb)`,
		id, piiActorRef, piiTestIP, "pii-test-agent/1.0")
	if err != nil {
		t.Fatalf("seed audit event: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM audit_events WHERE id=$1`, id)
	})
	return id
}

type auditShim struct {
	*strictservershim.PanicShim
	h *audit.HTTPHandler
}

func (s auditShim) ListAdminAuditEvents(ctx context.Context, req openapi.ListAdminAuditEventsRequestObject) (openapi.ListAdminAuditEventsResponseObject, error) {
	return s.h.ListAdminAuditEvents(ctx, req)
}

func (s auditShim) ListAdminAuditEventTypes(ctx context.Context, req openapi.ListAdminAuditEventTypesRequestObject) (openapi.ListAdminAuditEventTypesResponseObject, error) {
	return s.h.ListAdminAuditEventTypes(ctx, req)
}

func piiRouter(t *testing.T, pool *pgxpool.Pool, caps ...string) chi.Router {
	t.Helper()
	h := audit.NewHTTPHandler(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := &auth.Identity{UserRef: 999, AuthMethod: "session", Capabilities: caps}
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
		})
	})
	openapi.HandlerFromMux(
		openapi.NewStrictHandler(auditShim{PanicShim: &strictservershim.PanicShim{}, h: h}, nil), router)
	return router
}

// rawEvent finds the seeded row in the response as a RAW map, so the
// assertion sees the JSON as a client does. Decoding into
// openapi.AuditEvent would erase the distinction between "field absent"
// and "field present and null", which is precisely what is under test.
func rawEvent(t *testing.T, router chi.Router, want uuid.UUID) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/audit?limit=500", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin/audit = %d, body=%s", rr.Code, rr.Body.String())
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
	t.Fatalf("seeded event %s not in the response (%d items) — the test cannot "+
		"assert redaction on a row it never received", want, len(body.Items))
	return nil
}

// TestAuditIP_WithoutPIICapability_IsAbsent is the fix.
func TestAuditIP_WithoutPIICapability_IsAbsent(t *testing.T) {
	pool := piiPool(t)
	id := seedEventWithIP(t, pool)

	ev := rawEvent(t, piiRouter(t, pool, capAuditRead), id)

	if v, present := ev["ip"]; present {
		t.Errorf("a caller with %s but NOT %s received ip=%v — actor IPs are personal "+
			"data and must not ride along with plain audit read", capAuditRead, capAuditPIIRead, v)
	}
	// The whole response must not contain the address anywhere, in case
	// a future field echoes it (metadata, a formatted summary).
	for k, v := range ev {
		if s, ok := v.(string); ok && s == piiTestIP {
			t.Errorf("the IP leaked through field %q despite the capability gate", k)
		}
	}
}

// TestAuditIP_WithPIICapability_IsReturned is the other half: the
// capability has to actually grant something, or the "fix" is just a
// removal.
func TestAuditIP_WithPIICapability_IsReturned(t *testing.T) {
	pool := piiPool(t)
	id := seedEventWithIP(t, pool)

	ev := rawEvent(t, piiRouter(t, pool, capAuditRead, capAuditPIIRead), id)

	got, _ := ev["ip"].(string)
	if got != piiTestIP {
		t.Errorf("ip = %q, want %q — a caller holding %s must still see it",
			got, piiTestIP, capAuditPIIRead)
	}
}

// TestAuditIP_SystemAdminIsReturned pins the wildcard. system.admin
// short-circuits Identity.Can, so an admin needs no explicit grant —
// worth asserting, because a redaction implemented as an allowlist
// membership test rather than a Can() call would lock admins out.
func TestAuditIP_SystemAdminIsReturned(t *testing.T) {
	pool := piiPool(t)
	id := seedEventWithIP(t, pool)

	ev := rawEvent(t, piiRouter(t, pool, "system.admin"), id)

	if got, _ := ev["ip"].(string); got != piiTestIP {
		t.Errorf("system.admin got ip=%q, want %q — the wildcard must satisfy the PII cap",
			got, piiTestIP)
	}
}

// TestAuditNonIPFields_UnchangedWithoutPII is acceptance 6: gating the
// IP must not quietly degrade the rest of the row for the auditor role
// this capability exists to serve.
func TestAuditNonIPFields_UnchangedWithoutPII(t *testing.T) {
	pool := piiPool(t)
	id := seedEventWithIP(t, pool)

	plain := rawEvent(t, piiRouter(t, pool, capAuditRead), id)
	full := rawEvent(t, piiRouter(t, pool, capAuditRead, capAuditPIIRead), id)

	for _, f := range []string{"id", "event_type", "occurred_at", "actor_user_ref", "user_agent", "metadata"} {
		a, _ := json.Marshal(plain[f])
		b, _ := json.Marshal(full[f])
		if string(a) != string(b) {
			t.Errorf("%s differs with and without the PII cap: %s vs %s — only ip may vary",
				f, a, b)
		}
	}
	// user_agent is deliberately NOT gated (#425 scoped it out, #426
	// revisits retention). Asserted so that scoping decision is
	// visible and a change to it is a deliberate edit here.
	if _, ok := plain["user_agent"]; !ok {
		t.Error("user_agent vanished for a non-PII caller; it is out of scope for #425 " +
			"and must keep flowing until a decision says otherwise")
	}
}
