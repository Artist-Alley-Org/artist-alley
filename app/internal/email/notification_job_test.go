package email_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/email"
	"github.com/mscrnt/artist-alley/app/internal/jobs"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// seedUser inserts a minimal user row + returns its ref. Optional
// email; empty means "no email on file" (the no_email skip path).
func seedUser(t *testing.T, pool *pgxpool.Pool, email string) int64 {
	t.Helper()
	ctx := context.Background()
	username := "notif_email_test_" + uniqSuffix(t)
	var ref int64
	emailArg := any(nil)
	if email != "" {
		emailArg = email
	}
	err := pool.QueryRow(ctx, `
		INSERT INTO "user" (username, email, approved) VALUES ($1, $2, 1) RETURNING ref
	`, username, emailArg).Scan(&ref)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

func uniqSuffix(t *testing.T) string {
	t.Helper()
	// Username column is varchar(50); the test name + the "notif_
	// email_test_" prefix can overflow. Hash to keep it short +
	// deterministic per test.
	h := uint64(1469598103934665603)
	for _, r := range t.Name() {
		h ^= uint64(r)
		h *= 1099511628211
	}
	return strings.ToLower(strconvFormat(h, 16))
}

func strconvFormat(u uint64, base int) string {
	// Avoid pulling strconv just for one call.
	const digits = "0123456789abcdef"
	if u == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = digits[u%uint64(base)]
		u /= uint64(base)
	}
	return string(buf[i:])
}

func staticSite() email.SiteContextProvider {
	return func(_ context.Context) (email.SiteContext, error) {
		return email.SiteContext{Name: "Studio Alpha", URL: "https://art.example.com"}, nil
	}
}

func TestNotificationJob_SendsViaCapture(t *testing.T) {
	pool := openTestPool(t)
	ref := seedUser(t, pool, "recipient@example.com")

	cap := &email.Capture{}
	h := email.NewNotificationJobHandler(pool, cap, staticSite(), nil)

	payload, _ := json.Marshal(email.NotificationJobPayload{
		RecipientUserRef: ref,
		Verb:             "comment_on_my_post",
		TargetKind:       "post",
		TargetID:         "00000000-0000-0000-0000-000000000001",
		Payload:          map[string]any{"excerpt": "looks great"},
	})
	result, err := h.Handle(context.Background(), &jobs.Claim{
		Type: email.JobTypeNotificationEmail, Payload: payload,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(string(result), `"sent":true`) {
		t.Errorf("result missing sent:true: %s", result)
	}
	if cap.Len() != 1 {
		t.Fatalf("Capture.Len = %d, want 1", cap.Len())
	}
	msg, _ := cap.Last()
	if msg.To[0] != "recipient@example.com" {
		t.Errorf("To = %v, want [recipient@example.com]", msg.To)
	}
	if !strings.Contains(msg.Subject, "Studio Alpha") {
		t.Errorf("subject missing site name: %q", msg.Subject)
	}
	if !strings.Contains(msg.Subject, "comment_on_my_post") {
		t.Errorf("subject missing verb: %q", msg.Subject)
	}
	if !strings.Contains(msg.TextBody, "comment_on_my_post") {
		t.Errorf("text body missing verb: %q", msg.TextBody)
	}
}

func TestNotificationJob_SkipsRecipientWithNoEmail(t *testing.T) {
	pool := openTestPool(t)
	ref := seedUser(t, pool, "") // no email on file

	cap := &email.Capture{}
	h := email.NewNotificationJobHandler(pool, cap, staticSite(), nil)

	payload, _ := json.Marshal(email.NotificationJobPayload{
		RecipientUserRef: ref, Verb: "new_follower",
	})
	result, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(string(result), `"reason":"no_email"`) {
		t.Errorf("expected no_email reason, got %s", result)
	}
	if cap.Len() != 0 {
		t.Errorf("Capture.Len = %d, want 0 (no email → no send)", cap.Len())
	}
}

func TestNotificationJob_UnknownRecipientIsTerminal(t *testing.T) {
	pool := openTestPool(t)
	cap := &email.Capture{}
	h := email.NewNotificationJobHandler(pool, cap, staticSite(), nil)

	payload, _ := json.Marshal(email.NotificationJobPayload{
		RecipientUserRef: 99999999, Verb: "comment_on_my_post",
	})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if err == nil {
		t.Fatalf("expected error for unknown recipient")
	}
	if !jobs.IsTerminal(err) {
		t.Errorf("unknown-recipient error should be terminal: %v", err)
	}
}

func TestNotificationJob_BadPayloadIsTerminal(t *testing.T) {
	pool := openTestPool(t)
	h := email.NewNotificationJobHandler(pool, &email.Capture{}, staticSite(), nil)
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: []byte("not json")})
	if err == nil || !jobs.IsTerminal(err) {
		t.Errorf("bad payload should be terminal, got %v", err)
	}
}

func TestNotificationJob_NilSenderIsTerminal(t *testing.T) {
	pool := openTestPool(t)
	h := email.NewNotificationJobHandler(pool, nil, staticSite(), nil)
	payload, _ := json.Marshal(email.NotificationJobPayload{
		RecipientUserRef: 1, Verb: "x",
	})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if err == nil || !jobs.IsTerminal(err) {
		t.Errorf("nil sender should be terminal, got %v", err)
	}
}

func TestNotificationJob_TransientSenderErrorIsRetryable(t *testing.T) {
	pool := openTestPool(t)
	ref := seedUser(t, pool, "r@example.com")

	flaky := &flakySender{err: errors.New("connection refused")}
	h := email.NewNotificationJobHandler(pool, flaky, staticSite(), nil)
	payload, _ := json.Marshal(email.NotificationJobPayload{
		RecipientUserRef: ref, Verb: "x",
	})
	_, err := h.Handle(context.Background(), &jobs.Claim{Payload: payload})
	if err == nil {
		t.Fatalf("expected error from flaky sender")
	}
	if jobs.IsTerminal(err) {
		t.Errorf("transient sender error should NOT be terminal (job framework retries): %v", err)
	}
}

type flakySender struct{ err error }

func (f *flakySender) Send(_ context.Context, _ email.Message) error { return f.err }
