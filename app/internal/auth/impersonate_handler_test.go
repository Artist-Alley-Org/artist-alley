package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// seedNonAdmin creates an approved user with no roles (Base role
// is fine if the bootstrap seeds it; we don't need to attach one).
// Returns the new user's ref.
func seedNonAdmin(t *testing.T, pool *pgxpool.Pool, label string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	q := auth.New(pool)
	username := "imp-target-" + label + "-" + randHex(4)
	pw := "irrelevant"
	user, err := q.CreateUser(ctx, auth.CreateUserParams{
		Username: &username, Password: &pw,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, user.Ref)
	})
	return user.Ref, username
}

// adminIdentity loads the Identity for an admin so we can drive
// the handler with the same caller shape the resolver would
// produce. Uses the Resolver against a freshly-issued session
// rather than hand-constructing the Identity to keep the
// closure-expanded capability set realistic.
func adminIdentity(t *testing.T, pool *pgxpool.Pool, adminRef int64) *auth.Identity {
	t.Helper()
	sm := auth.NewSessionManager(pool)
	token, info, err := sm.Issue(context.Background(), adminRef, nil)
	if err != nil {
		t.Fatalf("issue admin session: %v", err)
	}
	t.Cleanup(func() { _ = sm.Revoke(context.Background(), info.ID) })
	r := auth.NewResolver(pool, nil, sm, nil)
	req := httptest.NewRequest("POST", "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	rr := httptest.NewRecorder()
	var captured *auth.Identity
	h := r.ResolveIdentity(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		captured = auth.IdentityFromContext(req.Context())
	}))
	h.ServeHTTP(rr, req)
	if captured == nil {
		t.Fatalf("resolver returned nil identity (cookie/session mismatch)")
	}
	return captured
}

func handlerFor(t *testing.T, pool *pgxpool.Pool) *auth.Handler {
	t.Helper()
	sm := auth.NewSessionManager(pool)
	return auth.NewHandler(pool, nil, "test-scramble-key" /*scrambleKey*/, 0 /*sessionDays*/, sm, nil /*limiter*/, nil /*audit*/, nil /*cacheReg*/)
}

func ctxWithIdentity(id *auth.Identity, req *http.Request) context.Context {
	ctx := auth.WithIdentity(context.Background(), id)
	if req != nil {
		ctx = auth.WithRequest(ctx, req)
	}
	return ctx
}

func TestAdminImpersonateUser_HappyPath(t *testing.T) {
	pool := openTestPool(t)
	admin := seedAdmin(t, pool, "happy-admin")
	target, targetUsername := seedNonAdmin(t, pool, "happy")

	id := adminIdentity(t, pool, admin)
	if !id.Can(auth.CapImpersonate) {
		t.Fatalf("admin identity should hold auth.impersonate; caps=%v", id.Capabilities)
	}
	h := handlerFor(t, pool)
	resp, err := h.AdminImpersonateUser(ctxWithIdentity(id, nil),
		openapi.AdminImpersonateUserRequestObject{Ref: target})
	if err != nil {
		t.Fatalf("AdminImpersonateUser: %v", err)
	}
	rr := httptest.NewRecorder()
	if err := resp.VisitAdminImpersonateUserResponse(rr); err != nil {
		t.Fatalf("Visit: %v", err)
	}
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// Cookie set with the new token.
	var cookieToken string
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cookieToken = c.Value
		}
	}
	if cookieToken == "" {
		t.Errorf("response missing session cookie")
	}
	// Resolve the new session — it MUST point to the target user
	// and carry ImpersonatedBy = admin.
	info, err := auth.NewSessionManager(pool).Lookup(context.Background(), cookieToken)
	if err != nil {
		t.Fatalf("Lookup new session: %v", err)
	}
	if info.UserRef != target {
		t.Errorf("new session UserRef = %d, want %d (target)", info.UserRef, target)
	}
	if info.ImpersonatedBy == nil || *info.ImpersonatedBy != admin {
		t.Errorf("new session ImpersonatedBy = %v, want %d", info.ImpersonatedBy, admin)
	}
	if info.ExpiresAt == nil || time.Until(*info.ExpiresAt) > auth.ImpersonationDefaultLifetime+5*time.Minute {
		t.Errorf("expiry should default to ~%v from now; got %v", auth.ImpersonationDefaultLifetime, info.ExpiresAt)
	}
	_ = targetUsername
}

func TestAdminImpersonateUser_NonAdmin_Forbidden(t *testing.T) {
	pool := openTestPool(t)
	nonAdmin, _ := seedNonAdmin(t, pool, "non-admin-caller")
	target, _ := seedNonAdmin(t, pool, "non-admin-target")

	id := adminIdentity(t, pool, nonAdmin) // resolver loads caps for a non-admin
	if id.Can(auth.CapImpersonate) {
		t.Fatalf("non-admin should NOT hold auth.impersonate; caps=%v", id.Capabilities)
	}
	h := handlerFor(t, pool)
	resp, err := h.AdminImpersonateUser(ctxWithIdentity(id, nil),
		openapi.AdminImpersonateUserRequestObject{Ref: target})
	if err != nil {
		t.Fatalf("AdminImpersonateUser: %v", err)
	}
	if _, ok := resp.(openapi.AdminImpersonateUser403JSONResponse); !ok {
		t.Errorf("expected 403, got %T", resp)
	}
}

func TestAdminImpersonateUser_SelfImpersonation_Refused(t *testing.T) {
	pool := openTestPool(t)
	admin := seedAdmin(t, pool, "self-imp")
	id := adminIdentity(t, pool, admin)
	h := handlerFor(t, pool)
	resp, err := h.AdminImpersonateUser(ctxWithIdentity(id, nil),
		openapi.AdminImpersonateUserRequestObject{Ref: admin})
	if err != nil {
		t.Fatalf("AdminImpersonateUser: %v", err)
	}
	if _, ok := resp.(openapi.AdminImpersonateUser400JSONResponse); !ok {
		t.Errorf("self-impersonate should 400, got %T", resp)
	}
}

func TestAdminImpersonateUser_TargetIsAdmin_Refused(t *testing.T) {
	pool := openTestPool(t)
	a := seedAdmin(t, pool, "actor")
	b := seedAdmin(t, pool, "target-admin")
	id := adminIdentity(t, pool, a)
	h := handlerFor(t, pool)
	resp, err := h.AdminImpersonateUser(ctxWithIdentity(id, nil),
		openapi.AdminImpersonateUserRequestObject{Ref: b})
	if err != nil {
		t.Fatalf("AdminImpersonateUser: %v", err)
	}
	if _, ok := resp.(openapi.AdminImpersonateUser403JSONResponse); !ok {
		t.Errorf("impersonating an admin should 403, got %T", resp)
	}
}

func TestAdminImpersonateUser_UnknownTarget_404(t *testing.T) {
	pool := openTestPool(t)
	admin := seedAdmin(t, pool, "unknown-tgt")
	id := adminIdentity(t, pool, admin)
	h := handlerFor(t, pool)
	resp, err := h.AdminImpersonateUser(ctxWithIdentity(id, nil),
		openapi.AdminImpersonateUserRequestObject{Ref: 999_999_999})
	if err != nil {
		t.Fatalf("AdminImpersonateUser: %v", err)
	}
	if _, ok := resp.(openapi.AdminImpersonateUser404JSONResponse); !ok {
		t.Errorf("unknown target should 404, got %T", resp)
	}
}

func TestAdminImpersonateUser_ChainRefused(t *testing.T) {
	pool := openTestPool(t)
	admin := seedAdmin(t, pool, "chain-admin")
	target, _ := seedNonAdmin(t, pool, "chain-target")
	other, _ := seedNonAdmin(t, pool, "chain-other")

	// First impersonation → ok.
	id := adminIdentity(t, pool, admin)
	h := handlerFor(t, pool)
	if _, err := h.AdminImpersonateUser(ctxWithIdentity(id, nil),
		openapi.AdminImpersonateUserRequestObject{Ref: target}); err != nil {
		t.Fatalf("first impersonate: %v", err)
	}
	// Synthesize an "already-impersonating" identity (would
	// normally arrive via the resolver after a follow-up request
	// carries the impersonation cookie). Set ImpersonatedBy on a
	// fresh Identity to emulate that.
	impersonating := &auth.Identity{
		UserRef:        target,
		ImpersonatedBy: &admin,
		Capabilities:   []string{auth.CapImpersonate}, // even if cap held, chain is refused
	}
	resp, err := h.AdminImpersonateUser(ctxWithIdentity(impersonating, nil),
		openapi.AdminImpersonateUserRequestObject{Ref: other})
	if err != nil {
		t.Fatalf("chain attempt: %v", err)
	}
	if _, ok := resp.(openapi.AdminImpersonateUser400JSONResponse); !ok {
		t.Errorf("chain attempt should 400, got %T", resp)
	}
}

func TestEndImpersonation_RotatesToAdminSession(t *testing.T) {
	pool := openTestPool(t)
	admin := seedAdmin(t, pool, "end-admin")
	target, _ := seedNonAdmin(t, pool, "end-target")

	// Start impersonation via the handler path so the cookie
	// state matches production.
	id := adminIdentity(t, pool, admin)
	h := handlerFor(t, pool)
	startResp, err := h.AdminImpersonateUser(ctxWithIdentity(id, nil),
		openapi.AdminImpersonateUserRequestObject{Ref: target})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	startRR := httptest.NewRecorder()
	_ = startResp.VisitAdminImpersonateUserResponse(startRR)
	var impToken string
	for _, c := range startRR.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			impToken = c.Value
		}
	}

	// Resolve the impersonation identity by looking up the
	// session row directly.
	impInfo, err := auth.NewSessionManager(pool).Lookup(context.Background(), impToken)
	if err != nil {
		t.Fatalf("lookup imp session: %v", err)
	}
	impID := &auth.Identity{
		UserRef:        impInfo.UserRef,
		SessionID:      &impInfo.ID,
		ImpersonatedBy: impInfo.ImpersonatedBy,
	}

	// End impersonation.
	endResp, err := h.EndImpersonation(ctxWithIdentity(impID, nil),
		openapi.EndImpersonationRequestObject{})
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	endRR := httptest.NewRecorder()
	if err := endResp.VisitEndImpersonationResponse(endRR); err != nil {
		t.Fatalf("end visit: %v", err)
	}
	if endRR.Code != 200 {
		t.Fatalf("end status = %d, body=%s", endRR.Code, endRR.Body.String())
	}

	// The new cookie token resolves to a session bound to the
	// original admin (NOT the target) with ImpersonatedBy nil.
	var newToken string
	for _, c := range endRR.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			newToken = c.Value
		}
	}
	newInfo, err := auth.NewSessionManager(pool).Lookup(context.Background(), newToken)
	if err != nil {
		t.Fatalf("lookup new admin session: %v", err)
	}
	if newInfo.UserRef != admin {
		t.Errorf("post-end UserRef = %d, want %d (admin)", newInfo.UserRef, admin)
	}
	if newInfo.ImpersonatedBy != nil {
		t.Errorf("post-end session should NOT be impersonating; got %v", newInfo.ImpersonatedBy)
	}

	// The old impersonation session is revoked.
	if _, err := auth.NewSessionManager(pool).Lookup(context.Background(), impToken); err == nil {
		t.Errorf("old impersonation session should be revoked but Lookup returned no error")
	}
}

func TestEndImpersonation_NotImpersonating_400(t *testing.T) {
	pool := openTestPool(t)
	admin := seedAdmin(t, pool, "end-nop")
	id := adminIdentity(t, pool, admin)
	h := handlerFor(t, pool)
	resp, err := h.EndImpersonation(ctxWithIdentity(id, nil),
		openapi.EndImpersonationRequestObject{})
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if _, ok := resp.(openapi.EndImpersonation400JSONResponse); !ok {
		t.Errorf("expected 400, got %T", resp)
	}
}
