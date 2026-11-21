// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Gate: authorization of the federation outbox and inbox admin routes,
// over B's production HTTP stack, with real rows.
//
// TestTwoStack_AdminFederationLists: the two lists answer only
// federation.read (the seeded Auditor role, #356 / #958) or system.admin.
// A's admin shares two assets with B, and B's admin shares two of B's
// assets with A, so B holds two delivered aa:Share rows in its inbox and
// two sent ones in its outbox. Then, on BOTH lists:
//   - no credentials: 401, and the body carries no row;
//   - an ordinary approved member's bearer token: 403, no row;
//   - B's admin: 200 with the rows, the peer / status / activity-type
//     filters applied, and a limit-1 cursor that pages through both rows
//     without repeating one;
//   - a member holding the seeded Auditor role: 200, the same rows.
//
// TestTwoStack_AdminFederationOutboxWrites: re-queueing a failed outbox
// row and cancelling a peer's queued rows stay on system.admin (#356
// keeps every write there). A refuses every delivery from B (503), so B's
// shares to A stay queued; one row is then marked failed (fixture: the
// real retry cap is hours of backoff away). Anonymous (401), a member
// and an Auditor (403) change nothing and leave no audit row; B's admin
// re-queues the failed row and cancels the queued ones, with one audit
// row each.
//
// The refusals reaching no store are pinned at the handler by the tests
// in app/internal/http/api_federation_admin_auth_test.go.

package twostack_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

type adminListPage struct {
	Items []struct {
		ID           string `json:"id"`
		PeerID       string `json:"peer_id"`
		Status       string `json:"status"`
		ActivityType string `json:"activity_type"` // inbox rows only
		ActivityID   string `json:"activity_id"`   // outbox rows only
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func TestTwoStack_AdminFederationLists(t *testing.T) {
	c := newCluster(t)
	c.runWorkers(c.a)
	c.runWorkers(c.b)
	g := &casGate{cluster: c, peerAonB: c.peerID(c.b, c.a), peerBonA: c.peerID(c.a, c.b)}

	// A -> B: two shares, landing in B's inbox.
	for range 2 {
		g.grantWith(g.newOriginal("public"), nil)
	}
	// B -> A: two shares of B's own assets, leaving through B's outbox.
	for range 2 {
		shareFromB(g)
	}
	deadline := time.Now().Add(60 * time.Second)
	for g.countOnB(`SELECT count(*) FROM federation_inbox WHERE peer_id = $1 AND activity_type = 'aa:Share' AND status = 'processed'`, g.peerAonB) < 2 ||
		g.countOnB(`SELECT count(*) FROM federation_outbox o JOIN activities a ON a.id = o.activity_id
			WHERE o.peer_id = $1 AND a.activity_type = 'aa:Share' AND o.status = 'sent'`, g.peerAonB) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("B never held two processed inbox and two sent outbox aa:Share rows; %s", c.peerRows())
		}
		time.Sleep(100 * time.Millisecond)
	}

	member := g.memberOnB()
	auditor := auditorOnB(g)
	peerQ := "peer_id=" + g.peerAonB.String() + "&activity_type=" + url.QueryEscape("aa:Share")
	for _, l := range []struct{ path, done string }{
		{"/admin/federation/inbox", "processed"},
		{"/admin/federation/outbox", "sent"},
	} {
		t.Run(strings.TrimPrefix(l.path, "/admin/federation/"), func(t *testing.T) {
			filtered := l.path + "?" + peerQ + "&status=" + l.done

			// The admin's filtered list: both rows, every filter held.
			st, raw := g.getOnB(c.b.token, filtered)
			var p adminListPage
			if err := json.Unmarshal(raw, &p); st != 200 || err != nil {
				t.Fatalf("admin: %d %s", st, raw)
			}
			var allIDs []string
			for _, it := range p.Items {
				allIDs = append(allIDs, it.ID)
				if it.PeerID != g.peerAonB.String() || it.Status != l.done {
					t.Errorf("filtered row %+v: want peer %s, status %s", it, g.peerAonB, l.done)
				}
				if l.path == "/admin/federation/inbox" && it.ActivityType != "aa:Share" {
					t.Errorf("filtered row %+v: want activity_type aa:Share", it)
				}
			}
			if len(allIDs) != 2 {
				t.Fatalf("admin sees %d filtered rows, want 2: %s", len(allIDs), raw)
			}

			// The Auditor reads the same filtered rows.
			{
				st, raw := g.getOnB(auditor, filtered)
				var q adminListPage
				if err := json.Unmarshal(raw, &q); st != 200 || err != nil {
					t.Fatalf("auditor: %d %s", st, raw)
				}
				var ids []string
				for _, it := range q.Items {
					ids = append(ids, it.ID)
				}
				sort.Strings(ids)
				want := append([]string(nil), allIDs...)
				sort.Strings(want)
				if !equalStrings(ids, want) {
					t.Errorf("auditor sees %v, admin %v", ids, want)
				}
			}

			// Refusals: the right status, and no row in the body.
			for _, who := range []struct {
				name, tok string
				want      int
			}{{"anonymous", "", 401}, {"member", member, 403}} {
				for _, path := range []string{l.path, filtered, l.path + "?limit=1"} {
					st, raw := g.getOnB(who.tok, path)
					if st != who.want {
						t.Errorf("%s GET %s: %d %s, want %d", who.name, path, st, raw, who.want)
					}
					for _, id := range allIDs {
						if strings.Contains(string(raw), id) {
							t.Errorf("%s GET %s: the body carries row %s", who.name, path, id)
						}
					}
					var q adminListPage
					if json.Unmarshal(raw, &q) == nil && len(q.Items) > 0 {
						t.Errorf("%s GET %s: %d items in the body", who.name, path, len(q.Items))
					}
				}
			}

			// Another peer's filter, or another status, matches none of them.
			for _, path := range []string{
				l.path + "?peer_id=" + uuid.NewString(),
				l.path + "?" + peerQ + "&status=failed",
				l.path + "?peer_id=" + g.peerAonB.String() + "&activity_type=Like",
			} {
				st, raw := g.getOnB(c.b.token, path)
				var q adminListPage
				if err := json.Unmarshal(raw, &q); st != 200 || err != nil {
					t.Fatalf("admin GET %s: %d %s", path, st, raw)
				}
				for _, it := range q.Items {
					for _, id := range allIDs {
						if it.ID == id {
							t.Errorf("admin GET %s returned row %s", path, id)
						}
					}
				}
			}

			// A limit-1 cursor pages through both rows, once each.
			var paged []string
			path := filtered + "&limit=1"
			for page := 0; ; page++ {
				if page > 3 {
					t.Fatalf("cursor did not end: %v", paged)
				}
				st, raw := g.getOnB(c.b.token, path)
				var q adminListPage
				if err := json.Unmarshal(raw, &q); st != 200 || err != nil {
					t.Fatalf("admin GET %s: %d %s", path, st, raw)
				}
				if len(q.Items) > 1 {
					t.Fatalf("limit=1 returned %d rows", len(q.Items))
				}
				for _, it := range q.Items {
					paged = append(paged, it.ID)
				}
				if q.NextCursor == nil || len(q.Items) == 0 {
					break
				}
				path = filtered + "&limit=1&cursor=" + url.QueryEscape(*q.NextCursor)
			}
			sort.Strings(paged)
			sort.Strings(allIDs)
			if !equalStrings(paged, allIDs) {
				t.Errorf("paged %v, want each of %v once", paged, allIDs)
			}
		})
	}
}

func TestTwoStack_AdminFederationOutboxWrites(t *testing.T) {
	c := newCluster(t)
	// A refuses every delivery (503 on its inbox), so B's rows to A stay
	// queued. The instance's front hook covers only the CAS paths, so A's
	// served handler is wrapped instead, and restored when the test ends.
	served := c.a.handler.Load()
	refusing := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/federation/inbox") {
			http.Error(w, "gate: deliveries refused", http.StatusServiceUnavailable)
			return
		}
		(*served).ServeHTTP(w, r)
	}))
	c.a.handler.Store(&refusing)
	t.Cleanup(func() { c.a.handler.Store(served) })
	c.runWorkers(c.b)
	g := &casGate{cluster: c, peerAonB: c.peerID(c.b, c.a), peerBonA: c.peerID(c.a, c.b)}
	ctx := context.Background()
	for range 3 {
		shareFromB(g)
	}
	// Every row was tried and refused once, so all three are real queued
	// deliveries. (A delivery result never overwrites a cancel: every
	// worker UPDATE is guarded by status = 'queued'.)
	deadline := time.Now().Add(60 * time.Second)
	for g.countOnB(`SELECT count(*) FROM federation_outbox WHERE peer_id = $1 AND status = 'queued' AND attempts >= 1`, g.peerAonB) < 3 {
		if time.Now().After(deadline) {
			var rows string
			_ = c.b.pool.QueryRow(ctx, `SELECT coalesce(string_agg(status || ' attempts=' || attempts || ' next=' || next_attempt_at::text
				|| ' err=' || coalesce(last_error, ''), '; '), 'none') FROM federation_outbox WHERE peer_id = $1`, g.peerAonB).Scan(&rows)
			t.Fatalf("B never held 3 refused queued rows for A (rows: %s); %s", rows, c.peerRows())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Fixture: one row reaches status failed.
	var failedID uuid.UUID
	if err := c.b.pool.QueryRow(ctx, `UPDATE federation_outbox SET status = 'failed', last_error = 'gate: forced failure'
		WHERE id = (SELECT id FROM federation_outbox WHERE peer_id = $1 AND status = 'queued' ORDER BY created_at LIMIT 1)
		RETURNING id`, g.peerAonB).Scan(&failedID); err != nil {
		t.Fatal(err)
	}
	// snapshot is what a refused write could change: every row's status,
	// and the attempts and last error of each row that is not queued (the
	// delivery worker keeps retrying the queued ones meanwhile).
	snapshot := func() string {
		t.Helper()
		var out string
		if err := c.b.pool.QueryRow(ctx, `SELECT coalesce(string_agg(id::text || ' ' || status || CASE WHEN status <> 'queued'
				THEN ' ' || attempts || ' ' || coalesce(last_error, '') ELSE '' END, '; ' ORDER BY id), '')
			FROM federation_outbox WHERE peer_id = $1`, g.peerAonB).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	audits := func(event string) int {
		t.Helper()
		return g.countOnB(`SELECT count(*) FROM audit_events WHERE event_type = $1 AND metadata->>'peer_id' = $2`,
			event, g.peerAonB.String())
	}

	member := g.memberOnB()
	auditor := auditorOnB(g)
	requeue := "/admin/federation/outbox/" + failedID.String() + "/requeue"
	cancel := "/admin/federation/peers/" + g.peerAonB.String() + "/cancel-pending"

	t.Run("refused callers change nothing", func(t *testing.T) {
		before := snapshot()
		for _, who := range []struct {
			name, tok string
			want      int
		}{{"anonymous", "", 401}, {"member", member, 403}, {"auditor", auditor, 403}} {
			for _, path := range []string{requeue, cancel} {
				if st, raw := postOnB(g, who.tok, path); st != who.want {
					t.Errorf("%s POST %s: %d %s, want %d", who.name, path, st, raw, who.want)
				}
			}
		}
		if after := snapshot(); after != before {
			t.Errorf("outbox rows changed under refused callers:\n before %s\n after  %s", before, after)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_outbox WHERE id = $1 AND status = 'failed'`, failedID); n != 1 {
			t.Errorf("the failed row is no longer failed")
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_outbox WHERE peer_id = $1 AND status = 'cancelled'`, g.peerAonB); n != 0 {
			t.Errorf("%d rows cancelled by refused callers", n)
		}
		if n, m := audits("federation.outbox.requeued"), audits("federation.peer.cascade_cancelled"); n != 0 || m != 0 {
			t.Errorf("refused callers left audit rows: %d requeued, %d cascade_cancelled", n, m)
		}
	})

	t.Run("the admin re-queues the failed row", func(t *testing.T) {
		var out struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Attempts int    `json:"attempts"`
		}
		if st := c.api(c.b, http.MethodPost, requeue, nil, &out); st != 200 || out.ID != failedID.String() || out.Status != "queued" || out.Attempts != 0 {
			t.Fatalf("admin requeue: %d %+v", st, out)
		}
		if n := audits("federation.outbox.requeued"); n != 1 {
			t.Errorf("%d requeued audit rows, want 1", n)
		}
		// Idempotency guard: the row is no longer failed.
		if st := c.api(c.b, http.MethodPost, requeue, nil, nil); st != 409 {
			t.Errorf("second requeue: %d, want 409", st)
		}
		if st := c.api(c.b, http.MethodPost, "/admin/federation/outbox/"+uuid.NewString()+"/requeue", nil, nil); st != 404 {
			t.Errorf("requeue of an unknown row: %d, want 404", st)
		}
	})

	t.Run("the admin cancels the queued rows", func(t *testing.T) {
		queued := g.countOnB(`SELECT count(*) FROM federation_outbox WHERE peer_id = $1 AND status = 'queued'`, g.peerAonB)
		var out struct {
			PeerID         string `json:"peer_id"`
			CancelledCount int    `json:"cancelled_count"`
		}
		if st := c.api(c.b, http.MethodPost, cancel, nil, &out); st != 200 || out.PeerID != g.peerAonB.String() || out.CancelledCount != queued || queued != 3 {
			t.Fatalf("admin cancel: %d %+v, %d queued before", st, out, queued)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_outbox WHERE peer_id = $1 AND status = 'cancelled'`, g.peerAonB); n != 3 {
			t.Errorf("%d rows cancelled, want 3", n)
		}
		if n := g.countOnB(`SELECT count(*) FROM federation_outbox WHERE peer_id = $1 AND status = 'queued'`, g.peerAonB); n != 0 {
			t.Errorf("%d rows still queued", n)
		}
		if n := audits("federation.peer.cascade_cancelled"); n != 1 {
			t.Errorf("%d cascade_cancelled audit rows, want 1", n)
		}
	})
}

// shareFromB creates an asset on B, with its stored original, and shares
// it with A through B's admin API.
func shareFromB(g *casGate) {
	g.t.Helper()
	ctx := context.Background()
	body := make([]byte, 4<<10)
	_, _ = rand.Read(body)
	if _, err := g.storageOf(g.b).UploadOriginal(ctx, bytes.NewReader(body), "image/png",
		storage.PinRef{SubjectType: "gate", SubjectID: uuid.NewString()}); err != nil {
		g.t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	asset := uuid.New()
	if _, err := g.b.pool.Exec(ctx,
		`INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity)
		 VALUES ($1, 'Gate asset on B', 1, $2, $3, $4, 'public')`,
		asset, g.b.admin, hex.EncodeToString(sum[:]), len(body)); err != nil {
		g.t.Fatal(err)
	}
	share := map[string]any{"object_kind": "asset", "object_id": asset, "peer_id": g.peerAonB, "scope": "view"}
	if st := g.api(g.b, http.MethodPost, "/admin/federation/shares", share, nil); st != 201 {
		g.t.Fatalf("B grant: %d", st)
	}
}

// auditorOnB is a member of B given the seeded Auditor role before its
// first request, so no cached capability set predates the grant.
func auditorOnB(g *casGate) string {
	g.t.Helper()
	tok := g.memberOnB()
	if tag, err := g.b.pool.Exec(context.Background(), `INSERT INTO user_roles (user_ref, role_id)
		SELECT t.user_ref, r.id FROM api_tokens t, roles r WHERE t.token_hash = $1 AND r.name = 'Auditor'`,
		auth.HashAPIToken(tok)); err != nil || tag.RowsAffected() != 1 {
		g.t.Fatalf("grant the Auditor role: %v (%d rows)", err, tag.RowsAffected())
	}
	return tok
}

// postOnB POSTs B's /api/v1 path with no body, with the bearer token, or
// anonymously when tok is empty.
func postOnB(g *casGate, tok, path string) (int, []byte) {
	g.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, g.b.url+"/api/v1"+path, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Transport: g.tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		g.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}
