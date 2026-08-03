// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package email_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/email"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// clearEmailTemplates wipes the override table so a test starts from the
// shipped baseline regardless of what ran before it.
func clearEmailTemplates(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM email_template`); err != nil {
		t.Fatalf("clear email_template: %v", err)
	}
}

func adminTestData() map[string]any {
	return map[string]any{
		"site_name":      "Studio Alpha",
		"site_url":       "https://art.example.com",
		"recipient_name": "Pat",
		"triggered_by":   "admin@example.com",
		"triggered_at":   "2026-08-02T08:00:00Z",
	}
}

// TestOverrides_SetAllDelete round-trips one override through the store
// with no cache wired (every read hits the DB).
func TestOverrides_SetAllDelete(t *testing.T) {
	pool := openTestPool(t)
	clearEmailTemplates(t, pool)
	ctx := context.Background()
	store := email.NewTemplateStore(pool, nil, discardLogger())

	if _, err := store.Set(ctx, email.TemplateAdminTest, email.PartSubject, "Hi {{.site_name}}", nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	all, err := store.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := all[email.TemplateAdminTest][email.PartSubject]; got != "Hi {{.site_name}}" {
		t.Fatalf("override not stored, got %q", got)
	}

	if err := store.Delete(ctx, email.TemplateAdminTest, email.PartSubject); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	all, err = store.All(ctx)
	if err != nil {
		t.Fatalf("All after delete: %v", err)
	}
	if _, ok := all[email.TemplateAdminTest]; ok {
		t.Fatalf("override survived delete: %#v", all)
	}

	// Deleting again is a reported miss, not a silent success.
	if err := store.Delete(ctx, email.TemplateAdminTest, email.PartSubject); err != email.ErrNotFound {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
}

// TestEndToEnd_CaptureReflectsOverride is acceptance #2: override
// admin_test's subject, render through the installed store, send via the
// capture sender, and assert the CAPTURED message carries the override —
// real proof, not a DB row read.
func TestEndToEnd_CaptureReflectsOverride(t *testing.T) {
	pool := openTestPool(t)
	clearEmailTemplates(t, pool)
	ctx := context.Background()
	store := email.NewTemplateStore(pool, nil, discardLogger())

	if _, err := store.Set(ctx, email.TemplateAdminTest, email.PartSubject,
		"OVERRIDDEN test for {{.site_name}}", nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	email.UseTemplateStore(store)
	t.Cleanup(func() { email.UseTemplateStore(nil) })

	msg, err := email.Render(ctx, email.TemplateAdminTest, []string{"ops@example.com"}, adminTestData())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	sender := &email.Capture{}
	if err := sender.Send(ctx, msg); err != nil {
		t.Fatalf("capture send: %v", err)
	}
	last, ok := sender.Last()
	if !ok {
		t.Fatal("nothing captured")
	}
	if !strings.HasPrefix(last.Subject, "OVERRIDDEN test for") {
		t.Errorf("captured subject did not reflect override: %q", last.Subject)
	}
	if !strings.Contains(last.Subject, "Studio Alpha") {
		t.Errorf("captured subject lost the interpolated field: %q", last.Subject)
	}
	// The text body was NOT overridden, so it still renders from the
	// shipped template — missing → shipped, per part.
	if !strings.Contains(last.TextBody, "Pat") {
		t.Errorf("shipped text body should still render, got %q", last.TextBody)
	}
}

// TestSend_FallsBackToShippedOnBrokenOverride is acceptance #4: an
// override that somehow got stored broken (inserted here directly,
// bypassing save-time validation) must NOT stop the mail — the shipped
// template renders instead.
func TestSend_FallsBackToShippedOnBrokenOverride(t *testing.T) {
	pool := openTestPool(t)
	clearEmailTemplates(t, pool)
	ctx := context.Background()

	// Insert a body that parses but errors at execute time, straight to
	// the DB — Set would (correctly) refuse it.
	if _, err := pool.Exec(ctx,
		`INSERT INTO email_template (template_name, part, body) VALUES ($1,$2,$3)`,
		email.TemplateAdminTest, email.PartSubject, "{{.site_name.nope}}"); err != nil {
		t.Fatalf("seed broken override: %v", err)
	}

	store := email.NewTemplateStore(pool, nil, discardLogger())
	email.UseTemplateStore(store)
	t.Cleanup(func() { email.UseTemplateStore(nil) })

	msg, err := email.Render(ctx, email.TemplateAdminTest, []string{"ops@example.com"}, adminTestData())
	if err != nil {
		t.Fatalf("Render must not fail on a broken override: %v", err)
	}
	sender := &email.Capture{}
	if err := sender.Send(ctx, msg); err != nil {
		t.Fatalf("capture send: %v", err)
	}
	last, _ := sender.Last()
	if last.Subject == "" {
		t.Fatal("subject empty — fallback did not render the shipped template")
	}
	if strings.Contains(last.Subject, "nope") {
		t.Errorf("subject leaked the broken override: %q", last.Subject)
	}
	// Shipped admin_test subject interpolates site_name.
	if !strings.Contains(last.Subject, "Studio Alpha") {
		t.Errorf("shipped subject should interpolate site_name, got %q", last.Subject)
	}
}

// TestRevert_RestoresShipped is acceptance #5.
func TestRevert_RestoresShipped(t *testing.T) {
	pool := openTestPool(t)
	clearEmailTemplates(t, pool)
	ctx := context.Background()
	store := email.NewTemplateStore(pool, nil, discardLogger())
	email.UseTemplateStore(store)
	t.Cleanup(func() { email.UseTemplateStore(nil) })

	if _, err := store.Set(ctx, email.TemplateAdminTest, email.PartSubject, "CUSTOM {{.site_name}}", nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	msg, err := email.Render(ctx, email.TemplateAdminTest, []string{"a@b.c"}, adminTestData())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(msg.Subject, "CUSTOM") {
		t.Fatalf("override not in effect: %q", msg.Subject)
	}

	if err := store.Delete(ctx, email.TemplateAdminTest, email.PartSubject); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	msg, err = email.Render(ctx, email.TemplateAdminTest, []string{"a@b.c"}, adminTestData())
	if err != nil {
		t.Fatalf("Render after revert: %v", err)
	}
	if strings.HasPrefix(msg.Subject, "CUSTOM") {
		t.Errorf("shipped subject did not return after revert: %q", msg.Subject)
	}
}

// TestCacheInvalidation_CrossInstance is acceptance #6: a write on one
// instance is visible to another WITHOUT a restart, via the pg_notify
// path — mirrors cache.TestNotifyRoundTrip for this domain.
func TestCacheInvalidation_CrossInstance(t *testing.T) {
	poolA := openTestPool(t)
	poolB := openTestPool(t)
	clearEmailTemplates(t, poolA)
	ctx := context.Background()
	logger := discardLogger()

	regA := cache.NewRegistry(poolA, logger)
	regB := cache.NewRegistry(poolB, logger)
	cacheA := email.NewCache(regA, logger)
	cacheB := email.NewCache(regB, logger)
	if err := regA.Start(ctx); err != nil {
		t.Fatalf("regA start: %v", err)
	}
	defer regA.Stop()
	if err := regB.Start(ctx); err != nil {
		t.Fatalf("regB start: %v", err)
	}
	defer regB.Stop()
	// Give both LISTENs a beat to subscribe.
	time.Sleep(100 * time.Millisecond)

	storeA := email.NewTemplateStore(poolA, cacheA, logger)
	storeB := email.NewTemplateStore(poolB, cacheB, logger)

	// Prime B's cache with the empty baseline.
	if all, err := storeB.All(ctx); err != nil {
		t.Fatalf("prime B: %v", err)
	} else if all[email.TemplateAdminTest] != nil {
		t.Fatalf("expected empty baseline on B")
	}

	// A writes an override — invalidates locally AND broadcasts NOTIFY.
	if _, err := storeA.Set(ctx, email.TemplateAdminTest, email.PartSubject, "NEW {{.site_name}}", nil); err != nil {
		t.Fatalf("A Set: %v", err)
	}

	// Wait for B to see it (its stale cache entry must be purged by the
	// NOTIFY, so the next All re-reads from the DB).
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		all, err := storeB.All(ctx)
		if err != nil {
			t.Fatalf("B All: %v", err)
		}
		got = all[email.TemplateAdminTest][email.PartSubject]
		if got == "NEW {{.site_name}}" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != "NEW {{.site_name}}" {
		t.Errorf("cross-instance invalidation failed; B still sees %q", got)
	}
}
